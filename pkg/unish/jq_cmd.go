package unish

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

type jqCLIOpts struct {
	rawOutput   bool
	joinOutput  bool
	nulOutput   bool
	compact     bool
	slurp       bool
	nullInput   bool
	exitStatus  bool
	rawInput    bool
	sortKeys    bool
	colorOutput bool
	unbuffered  bool
	indent      string
	fromFile    string
	hasFromFile bool
	filter      string
	files       []string
	namedVars   *jqObj
	posArgs     []jqVal
	showHelp    bool
	showVersion bool
}

var errJqPipeClosed = errors.New("jq: pipe closed")

func cmdJq(ctx context.Context, hc interp.HandlerContext, args []string) error {
	opts, code, err := parseJqCLI(ctx, hc, args[1:])
	if err != nil {
		if isPipeClosed(err) {
			return nil
		}
		fmt.Fprintf(hc.Stderr, "jq: error: %v\n", err)
		return exitError{code}
	}
	if opts.showHelp {
		_, _ = io.WriteString(hc.Stdout, jqHelpText)
		return nil
	}
	if opts.showVersion {
		_, _ = io.WriteString(hc.Stdout, "jq-1.7-unish\n")
		return nil
	}

	ast, err := jqParse(opts.filter)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "%v\n", err)
		return exitError{3}
	}

	rootEnv := newJqEnv(jqPreludeEnv())
	if opts.namedVars != nil && len(opts.namedVars.entries) > 0 {
		rootEnv.vars = make(map[string]jqVal, len(opts.namedVars.entries))
		for _, ent := range opts.namedVars.entries {
			rootEnv.vars[ent.key] = ent.val
		}
	}

	argsObj := jqMakeObj([]jqEntry{
		{key: "positional", val: jqMakeArr(opts.posArgs)},
		{key: "named", val: jqVal{kind: jqObject, obj: opts.namedVars}},
	})
	envObj := buildJqEnvObj(hc)

	outBuf := bufio.NewWriterSize(hc.Stdout, 32*1024)
	var fmtBuf bytes.Buffer
	fmtOpts := jqFmtOpts{
		compact:  opts.compact,
		sortKeys: opts.sortKeys,
		indent:   opts.indent,
		color:    opts.colorOutput,
	}

	hasOutput := false
	lastTruthy := false

	emitOutput := func(val jqVal) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		hasOutput = true
		lastTruthy = val.isTruthy()

		fmtBuf.Reset()
		if opts.rawOutput && val.kind == jqString {
			if opts.nulOutput && strings.IndexByte(val.str, 0) >= 0 {
				return jqErrStr("Cannot dump a string containing NUL with --raw-output0 option")
			}
			fmtBuf.WriteString(val.str)
		} else {
			jqWriteFormatted(&fmtBuf, val, fmtOpts, 0)
		}
		if opts.nulOutput {
			fmtBuf.WriteByte(0)
		} else if !opts.joinOutput {
			fmtBuf.WriteByte('\n')
		}

		if _, werr := outBuf.Write(fmtBuf.Bytes()); werr != nil {
			if isPipeClosed(werr) {
				return errJqPipeClosed
			}
			return werr
		}
		if opts.unbuffered {
			if ferr := outBuf.Flush(); ferr != nil {
				if isPipeClosed(ferr) {
					return errJqPipeClosed
				}
				return ferr
			}
		}
		return nil
	}

	stream, err := newJqInputStream(ctx, hc, opts.files, opts.rawInput)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "jq: error: %v\n", err)
		return exitError{2}
	}
	defer stream.close()

	evalSt := &jqEvalState{
		ctx:       ctx,
		stderr:    hc.Stderr,
		envObj:    envObj,
		argsObj:   argsObj,
		nextInput: stream.next,
	}

	runFilterOn := func(inVal jqVal) error {
		err := evalSt.eval(ast, inVal, rootEnv, emitOutput)
		if err != nil {
			return err
		}
		if ferr := outBuf.Flush(); ferr != nil {
			if isPipeClosed(ferr) {
				return errJqPipeClosed
			}
			return ferr
		}
		return nil
	}

	var runErr error
	switch {
	case opts.nullInput:
		runErr = runFilterOn(jqMakeNull())

	case opts.slurp && opts.rawInput:
		rawStr, rerr := stream.readAllRaw()
		if rerr != nil {
			fmt.Fprintf(hc.Stderr, "jq: error: %v\n", rerr)
			return exitError{2}
		}
		runErr = runFilterOn(jqMakeStr(rawStr))

	case opts.slurp:
		var all []jqVal
		for {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			iv, nerr := stream.next()
			if nerr == io.EOF {
				break
			}
			if nerr != nil {
				fmt.Fprintf(hc.Stderr, "jq: %v\n", nerr)
				return exitError{4}
			}
			all = append(all, iv)
		}
		runErr = runFilterOn(jqMakeArr(all))

	default:
		for {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			iv, nerr := stream.next()
			if nerr == io.EOF {
				break
			}
			if nerr != nil {
				_ = outBuf.Flush()
				fmt.Fprintf(hc.Stderr, "jq: %v\n", nerr)
				return exitError{4}
			}
			if err := runFilterOn(iv); err != nil {
				runErr = err
				break
			}
		}
	}

	if flushErr := outBuf.Flush(); flushErr != nil && !isPipeClosed(flushErr) && runErr == nil {
		runErr = flushErr
	}

	if runErr != nil {
		if errors.Is(runErr, errJqPipeClosed) || isPipeClosed(runErr) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var haltErr *jqHaltError
		if errors.As(runErr, &haltErr) {
			if haltErr.err.kind != jqNull {
				if haltErr.err.kind == jqString {
					_, _ = io.WriteString(hc.Stderr, haltErr.err.str)
				} else {
					_, _ = fmt.Fprintln(hc.Stderr, jqFormatCompactStr(haltErr.err))
				}
			}
			if haltErr.code == 0 {
				return nil
			}
			return exitError{haltErr.code}
		}
		var rtErr *jqRuntimeError
		if errors.As(runErr, &rtErr) {
			fmt.Fprintf(hc.Stderr, "jq: error: %s\n", rtErr.Error())
			return exitError{5}
		}
		var brkErr *jqBreakError
		if errors.As(runErr, &brkErr) {
			fmt.Fprintf(hc.Stderr, "jq: error: %s\n", brkErr.Error())
			return exitError{3}
		}
		fmt.Fprintf(hc.Stderr, "jq: error: %v\n", runErr)
		return exitError{5}
	}

	if opts.exitStatus {
		if !hasOutput {
			return exitError{4}
		}
		if !lastTruthy {
			return exitError{1}
		}
	}
	return nil
}

// --- CLI Flag & Argument Parser ---

// jqLongFlagShort maps boolean long options onto their short-flag letter.
func jqLongFlagShort(name string) byte {
	const tbl = "help:h version:V raw-output:r join-output:j nul-output:0 raw-output0:0 compact-output:c slurp:s null-input:n exit-status:e raw-input:R sort-keys:S color-output:C monochrome-output:M seq:a ascii-output:a stream:a unbuffered:U tab:T "
	if i := strings.Index(tbl, " "+name+":"); i >= 0 {
		return tbl[i+len(name)+2]
	}
	if strings.HasPrefix(tbl, name+":") {
		return tbl[len(name)+1]
	}
	return 0
}

func parseJqCLI(ctx context.Context, hc interp.HandlerContext, argv []string) (*jqCLIOpts, int, error) {
	opts := &jqCLIOpts{
		indent:    "  ",
		namedVars: newJqObj(4),
	}

	var positionals []string
	endFlags := false
	argsMode := 0 // 0=files, 1=--args (strings), 2=--jsonargs (JSON)

	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if argsMode != 0 {
			// Everything after --args or --jsonargs goes into $ARGS.positional
			// EXCEPT if filter hasn't been seen yet? Wait: in jq, filter comes BEFORE --args,
			// unless -f was used! Wait: in jq `jq -n '$ARGS.positional' --args a b`, filter was already seen.
			// What if someone writes `jq --args '$ARGS.positional' a b`? No, in jq `--args` says
			// "remaining arguments are positional string arguments" (after filter if not yet given? Actually in jq, if filter wasn't given yet, 1st positional before or after `--` is filter; before `--args`, filter is given).
			if !opts.hasFromFile && opts.filter == "" && len(positionals) == 0 {
				positionals = append(positionals, a)
				continue
			}
			if argsMode == 1 {
				opts.posArgs = append(opts.posArgs, jqMakeStr(a))
			} else {
				jv, err := jqParseSingleJSON(a)
				if err != nil {
					return nil, 2, fmt.Errorf("invalid JSON argument %q: %v", a, err)
				}
				opts.posArgs = append(opts.posArgs, jv)
			}
			continue
		}

		if endFlags {
			positionals = append(positionals, a)
			continue
		}

		if a == "--" {
			endFlags = true
			continue
		}
		if a == "-" || !strings.HasPrefix(a, "-") ||
			(len(a) >= 2 && a[0] == '-' && ((a[1] >= '1' && a[1] <= '9') || a[1] == '.')) {
			positionals = append(positionals, a)
			continue
		}

		// Long flags
		isLong := strings.HasPrefix(a, "--")
		if isLong {
			name := a[2:]
			eqVal := ""
			hasEq := false
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				eqVal = name[eq+1:]
				hasEq = true
				name = name[:eq]
			}
			nextOne := func(flagName string) (string, error) {
				if hasEq {
					return eqVal, nil
				}
				if i+1 >= len(argv) {
					return "", fmt.Errorf("--%s requires an argument", flagName)
				}
				i++
				return argv[i], nil
			}
			nextTwo := func(flagName string) (string, string, error) {
				if hasEq || i+2 >= len(argv) {
					return "", "", fmt.Errorf("--%s requires two arguments: name and value", flagName)
				}
				k, v := argv[i+1], argv[i+2]
				i += 2
				return k, v, nil
			}

			shortEq := jqLongFlagShort(name)
			if shortEq != 0 {
				if hasEq {
					return nil, 2, fmt.Errorf("--%s takes no argument", name)
				}
				a = "-" + string(shortEq)
			}
			switch name {
			case "indent":
				ns, err := nextOne("indent")
				if err != nil {
					return nil, 2, err
				}
				n, err := strconv.Atoi(ns)
				if err != nil || n < 0 || n > 7 {
					return nil, 2, fmt.Errorf("indent count must be an integer between 0 and 7, got %q", ns)
				}
				opts.indent = strings.Repeat(" ", n)
			case "from-file":
				fp, err := nextOne("from-file")
				if err != nil {
					return nil, 2, err
				}
				opts.fromFile = fp
				opts.hasFromFile = true
			case "library-path":
				if _, err := nextOne("library-path"); err != nil {
					return nil, 2, err
				}
			case "arg":
				k, v, err := nextTwo("arg")
				if err != nil {
					return nil, 2, err
				}
				opts.namedVars.set(k, jqMakeStr(v))
			case "argjson":
				k, rawJ, err := nextTwo("argjson")
				if err != nil {
					return nil, 2, err
				}
				jv, err := jqParseSingleJSON(rawJ)
				if err != nil {
					return nil, 2, fmt.Errorf("--argjson %s: invalid JSON text: %v", k, err)
				}
				opts.namedVars.set(k, jv)
			case "slurpfile", "argfile":
				k, fpath, err := nextTwo(name)
				if err != nil {
					return nil, 2, err
				}
				arr, err := readSlurpFile(ctx, hc, fpath)
				if err != nil {
					return nil, 2, err
				}
				if name == "argfile" && len(arr.arr) == 1 {
					opts.namedVars.set(k, arr.arr[0])
				} else {
					opts.namedVars.set(k, arr)
				}
			case "rawfile":
				k, fpath, err := nextTwo("rawfile")
				if err != nil {
					return nil, 2, err
				}
				var data []byte
				if !isNullDevPath(fpath) {
					data, err = fsReadInputFile(ctx, hc.Dir, "jq", fpath)
					if err != nil {
						return nil, 2, fmt.Errorf("could not open %s: %v", fpath, err)
					}
				}
				opts.namedVars.set(k, jqMakeStr(string(data)))
			case "args":
				argsMode = 1
			case "jsonargs":
				argsMode = 2
			default:
				if shortEq == 0 {
					return nil, 2, fmt.Errorf("unknown option --%s", name)
				}
			}
			if shortEq == 0 {
				continue
			}
		}

		// Short flags (may be clustered, e.g. -rc, -rn, -sr, -fprog.jq)
		shorts := a[1:]
		for j := 0; j < len(shorts); j++ {
			ch := shorts[j]
			switch ch {
			case 'h':
				opts.showHelp = true
				return opts, 0, nil
			case 'V':
				opts.showVersion = true
				return opts, 0, nil
			case 'r':
				opts.rawOutput = true
			case 'j':
				opts.rawOutput = true
				opts.joinOutput = true
			case '0':
				opts.rawOutput = true
				opts.nulOutput = true
			case 'c':
				opts.compact = true
			case 's':
				opts.slurp = true
			case 'n':
				opts.nullInput = true
			case 'e':
				opts.exitStatus = true
			case 'R':
				opts.rawInput = true
			case 'S':
				opts.sortKeys = true
			case 'C':
				opts.colorOutput = true
			case 'M':
				opts.colorOutput = false
			case 'a':
				// -a (--ascii-output, --seq, --stream) tolerated
			case 'U':
				if !isLong {
					return nil, 2, fmt.Errorf("unknown option -%c", ch)
				}
				opts.unbuffered = true
			case 'T':
				if !isLong {
					return nil, 2, fmt.Errorf("unknown option -%c", ch)
				}
				opts.indent = "\t"
			case 'f':
				if j+1 < len(shorts) {
					opts.fromFile = shorts[j+1:]
					opts.hasFromFile = true
					j = len(shorts)
				} else {
					if i+1 >= len(argv) {
						return nil, 2, fmt.Errorf("-f requires a filename argument")
					}
					i++
					opts.fromFile = argv[i]
					opts.hasFromFile = true
				}
			case 'L':
				if j+1 < len(shorts) {
					j = len(shorts)
				} else {
					if i+1 >= len(argv) {
						return nil, 2, fmt.Errorf("-L requires a directory argument")
					}
					i++
				}
			default:
				return nil, 2, fmt.Errorf("unknown option -%c", ch)
			}
		}
	}

	if opts.hasFromFile {
		var data []byte
		if !isNullDevPath(opts.fromFile) {
			var err error
			data, err = fsReadInputFile(ctx, hc.Dir, "jq", opts.fromFile)
			if err != nil {
				return nil, 2, fmt.Errorf("could not open filter file %s: %v", opts.fromFile, err)
			}
		}
		// Strip optional UTF-8 BOM on filter file
		src := string(data)
		src = strings.TrimPrefix(src, "\xef\xbb\xbf")
		opts.filter = src
		opts.files = positionals
	} else if len(positionals) == 0 {
		opts.filter = "."
	} else {
		opts.filter = positionals[0]
		opts.files = positionals[1:]
	}

	if argsMode == 0 && len(opts.files) > 0 {
		posVals := make([]jqVal, len(opts.files))
		for k, f := range opts.files {
			posVals[k] = jqMakeStr(f)
		}
		opts.posArgs = posVals
	}

	return opts, 0, nil
}

func readSlurpFile(ctx context.Context, hc interp.HandlerContext, fpath string) (jqVal, error) {
	if isNullDevPath(fpath) {
		return jqMakeArr(nil), nil
	}
	f, err := fsOpenInput(ctx, hc.Dir, "jq", fpath)
	if err != nil {
		return jqMakeNull(), fmt.Errorf("could not open %s: %v", fpath, err)
	}
	defer f.Close()
	dec := newJqJSONDecoder(f)
	var items []jqVal
	for {
		v, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return jqMakeNull(), fmt.Errorf("%s: %v", fpath, err)
		}
		items = append(items, v)
	}
	return jqMakeArr(items), nil
}

func buildJqEnvObj(hc interp.HandlerContext) jqVal {
	obj := newJqObj(32)
	if hc.Env != nil {
		if ee, ok := hc.Env.(explicitEnviron); ok {
			for _, kv := range ee.pairs {
				if eq := strings.IndexByte(kv, '='); eq >= 0 {
					obj.set(kv[:eq], jqMakeStr(kv[eq+1:]))
				}
			}
		} else if envFallbackAllowed(hc, "") {
			for _, kv := range os.Environ() {
				if eq := strings.IndexByte(kv, '='); eq >= 0 {
					obj.set(kv[:eq], jqMakeStr(kv[eq+1:]))
				}
			}
		}
		hc.Env.Each(func(name string, vr expand.Variable) bool {
			if vr.IsSet() && vr.Exported {
				obj.set(name, jqMakeStr(vr.String()))
			}
			return true
		})
	}
	return jqVal{kind: jqObject, obj: obj}
}

// --- Multi-File / Stdin Input Stream ---

type jqInputStream struct {
	ctx      context.Context
	hc       interp.HandlerContext
	files    []string
	fileIdx  int
	rawInput bool

	curCloser io.Closer
	curJSON   *jqJSONDecoder
	curRaw    *bufio.Reader
	started   bool
}

func newJqInputStream(ctx context.Context, hc interp.HandlerContext, files []string, rawInput bool) (*jqInputStream, error) {
	for _, fp := range files {
		if fp == "-" || fp == "/dev/stdin" || isNullDevPath(fp) {
			continue
		}
		hostPath := resolveHostPath(hc.Dir, fp)
		if st := winProcSubstLookup(hostPath); st != nil {
			continue
		}
		fi, err := fsStat(ctx, hc.Dir, "jq", fp)
		if err != nil {
			return nil, fmt.Errorf("Could not open file %s: %v", fp, err)
		}
		if fi.IsDir() {
			return nil, fmt.Errorf("Could not open file %s: is a directory", fp)
		}
	}
	return &jqInputStream{
		ctx:      ctx,
		hc:       hc,
		files:    files,
		rawInput: rawInput,
	}, nil
}

func (s *jqInputStream) closeCurrent() {
	if s.curCloser != nil {
		_ = s.curCloser.Close()
		s.curCloser = nil
	}
	s.curJSON = nil
	s.curRaw = nil
}

func (s *jqInputStream) close() {
	s.closeCurrent()
}

func (s *jqInputStream) advanceSource() (bool, error) {
	s.closeCurrent()
	if len(s.files) == 0 {
		if s.started {
			return false, nil
		}
		s.started = true
		r := s.hc.Stdin
		if r == nil {
			r = strings.NewReader("")
		}
		if s.rawInput {
			s.curRaw = bufio.NewReaderSize(r, 32*1024)
		} else {
			s.curJSON = newJqJSONDecoder(r)
		}
		return true, nil
	}
	if s.fileIdx >= len(s.files) {
		return false, nil
	}
	fp := s.files[s.fileIdx]
	s.fileIdx++
	s.started = true
	var r io.Reader
	if isNullDevPath(fp) {
		r = strings.NewReader("")
	} else if fp == "-" || fp == "/dev/stdin" {
		r = s.hc.Stdin
		if r == nil {
			r = strings.NewReader("")
		}
	} else {
		f, err := fsOpenInput(s.ctx, s.hc.Dir, "jq", fp)
		if err != nil {
			return false, fmt.Errorf("Could not open file %s: %v", fp, err)
		}
		s.curCloser = f
		r = f
	}
	if s.rawInput {
		s.curRaw = bufio.NewReaderSize(r, 32*1024)
	} else {
		s.curJSON = newJqJSONDecoder(r)
	}
	return true, nil
}

func (s *jqInputStream) next() (jqVal, error) {
	for {
		if s.curJSON == nil && s.curRaw == nil {
			ok, err := s.advanceSource()
			if err != nil {
				return jqMakeNull(), err
			}
			if !ok {
				return jqMakeNull(), io.EOF
			}
		}
		if s.rawInput {
			line, err := s.curRaw.ReadString('\n')
			if len(line) > 0 {
				line = strings.TrimSuffix(line, "\n")
				return jqMakeStr(line), nil
			}
			if err == io.EOF {
				s.closeCurrent()
				continue
			}
			if err != nil {
				return jqMakeNull(), err
			}
		} else {
			val, err := s.curJSON.Next()
			if err == io.EOF {
				s.closeCurrent()
				continue
			}
			if err != nil {
				return jqMakeNull(), err
			}
			return val, nil
		}
	}
}

func (s *jqInputStream) readAllRaw() (string, error) {
	var sb strings.Builder
	for {
		ok, err := s.advanceSource()
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		data, err := io.ReadAll(s.curRaw)
		s.closeCurrent()
		if err != nil {
			return "", err
		}
		sb.Write(data)
	}
	return sb.String(), nil
}

const jqHelpText = `jq - command-line JSON processor (unish pure-Go engine)
Usage: jq [OPTIONS...] [FILTER] [FILES...]

Options:
  -c, --compact-output       compact instead of pretty-printed output
  -n, --null-input           use 'null' as the single input value
  -e, --exit-status          set exit status code based on the output
  -s, --slurp                read (slurp) all inputs into an array; apply filter to it
  -r, --raw-output           output strings without escapes and quotes
  -j, --join-output          implies -r and output without newline after each item
  -0, --nul-output           implies -r and output NUL after each item
  -R, --raw-input            read each line as string instead of JSON
  -S, --sort-keys            sort keys of objects on output
  -C, --color-output         colorize JSON
  -M, --monochrome-output    monochrome (don't colorize JSON)
      --tab                  use tabs for indentation
      --indent N             use N spaces for indentation (0-7)
      --unbuffered           flush output stream after each output
  -f, --from-file FILE       load filter from FILE
      --arg NAME VALUE       set variable $NAME to string VALUE
      --argjson NAME JSON    set variable $NAME to JSON value
      --slurpfile NAME FILE  set variable $NAME to array of JSON values in FILE
      --rawfile NAME FILE    set variable $NAME to string contents of FILE
      --args                 remaining arguments are string positional arguments
      --jsonargs             remaining arguments are JSON positional arguments
  -h, --help                 display this help and exit
  -V, --version              output version information and exit
`
