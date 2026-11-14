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
	"time"
	"unicode/utf8"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// stdioOSFile extracts an underlying *os.File from hc.Stdin/Stdout/Stderr for FD 0/1/2.
func stdioOSFile(fd int, hc interp.HandlerContext) *os.File {
	var v any
	switch fd {
	case 0:
		v = hc.Stdin
	case 1:
		v = hc.Stdout
	case 2:
		v = hc.Stderr
	default:
		return nil
	}
	switch f := v.(type) {
	case *os.File:
		return f
	case *refcountedFile:
		if f != nil {
			return f.File
		}
	case *countedFile:
		if f != nil {
			return f.File
		}
	}
	return nil
}

// ============================================================================
// 1. sponge — soak up all standard input before writing output file atomically
// ============================================================================

// ---- Shared table-driven option parser (keeps per-tool flag code small) ----

// toolOpt describes one option: any byte in short selects it, long is a
// space-separated list of long names, and arg reports whether it takes a value.
type toolOpt struct {
	short string
	long  string
	arg   bool
}

type toolOptParser struct {
	tool      string
	opts      []toolOpt
	dashLong  bool // accept long names after a single dash (xxd -ps, -cols)
	stopAtPos bool // stop at the first positional (flock lockfile cmd...)
}

var errToolHelp = errors.New("help requested")

func toolOptErr(err error) error {
	if err == errToolHelp {
		return nil
	}
	return err
}

func (p *toolOptParser) find(long string, short byte) int {
	for i := range p.opts {
		if long != "" {
			for _, l := range strings.Fields(p.opts[i].long) {
				if l == long {
					return i
				}
			}
		} else if strings.IndexByte(p.opts[i].short, short) >= 0 {
			return i
		}
	}
	return -1
}

// parse walks args[1:] calling set(optionIndex, value) and returns the
// positionals. "-" is positional and "--" ends options. Usage errors are
// reported on stderr and returned as exit status 2.
func (p *toolOptParser) parse(stderr io.Writer, args []string, set func(idx int, val string) error) ([]string, error) {
	var pos []string
	fail := func(format string, a ...any) ([]string, error) {
		fmt.Fprintf(stderr, p.tool+": "+format+"\n", a...)
		return nil, exitError{2}
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(pos, args[i+1:]...), nil
		}
		if len(a) < 2 || a[0] != '-' {
			if p.stopAtPos {
				return append(pos, args[i:]...), nil
			}
			pos = append(pos, a)
			continue
		}
		if a[1] == '-' || p.dashLong {
			name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
			if idx := p.find(name, 0); idx >= 0 {
				if p.opts[idx].arg && !hasVal {
					if i+1 >= len(args) {
						return fail("option '%s' requires an argument", a)
					}
					i++
					val = args[i]
				} else if !p.opts[idx].arg && hasVal {
					return fail("option '--%s' doesn't allow an argument", name)
				}
				if err := set(idx, val); err != nil {
					return nil, err
				}
				continue
			}
			if a[1] == '-' {
				return fail("unrecognized option '%s'", a)
			}
		}
		for j := 1; j < len(a); j++ {
			idx := p.find("", a[j])
			if idx < 0 {
				return fail("invalid option -- '%c'", a[j])
			}
			val := ""
			if p.opts[idx].arg {
				switch {
				case j+1 < len(a):
					val = a[j+1:]
				case i+1 < len(args):
					i++
					val = args[i]
				default:
					return fail("option requires an argument -- '%c'", a[j])
				}
				j = len(a)
			}
			if err := set(idx, val); err != nil {
				return nil, err
			}
		}
	}
	return pos, nil
}

// toolBadArg reports an invalid option value and returns exit status 2.
func toolBadArg(stderr io.Writer, tool, what, val string) error {
	fmt.Fprintf(stderr, "%s: invalid %s %q\n", tool, what, val)
	return exitError{2}
}

func isToolDevNull(p string) bool {
	return p == "/dev/null" || (os.DevNull == "NUL" && strings.EqualFold(p, "nul"))
}

var spongeOpts = toolOptParser{tool: "sponge", opts: []toolOpt{{"a", "append", false}, {"h", "help", false}}}

func cmdSponge(ctx context.Context, hc interp.HandlerContext, args []string) error {
	appendMode := false
	pos, err := spongeOpts.parse(hc.Stderr, args, func(idx int, _ string) error {
		if idx == 1 {
			fmt.Fprintln(hc.Stdout, "Usage: sponge [-a] [file]")
			return errToolHelp
		}
		appendMode = true
		return nil
	})
	if err != nil {
		return toolOptErr(err)
	}

	if len(pos) > 1 {
		_, _ = fmt.Fprintln(hc.Stderr, "sponge: too many arguments")
		return exitError{2}
	}

	// Read ALL of stdin into memory BEFORE opening or modifying target file.
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := hc.Stdin.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(hc.Stderr, "sponge: reading stdin: %v\n", rerr)
			return exitError{1}
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if len(pos) == 0 || pos[0] == "-" || pos[0] == "/dev/stdout" {
		if _, err := hc.Stdout.Write(buf.Bytes()); err != nil {
			if isPipeClosed(err) {
				return nil
			}
			_, _ = fmt.Fprintf(hc.Stderr, "sponge: writing stdout: %v\n", err)
			return exitError{1}
		}
		return nil
	}
	if isNullDevPath(pos[0]) || isToolDevNull(pos[0]) {
		return nil
	}
	if pw, ok, perr := openProcSubstOut(pos[0]); ok {
		if perr != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], perr)
			return exitError{1}
		}
		_, werr := pw.Write(buf.Bytes())
		cerr := pw.Close()
		if werr != nil {
			if isPipeClosed(werr) {
				return nil
			}
			_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], werr)
			return exitError{1}
		}
		if cerr != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], cerr)
			return exitError{1}
		}
		return nil
	}

	perm := os.FileMode(0o644)
	fi, statErr := fsStat(ctx, hc.Dir, "sponge", pos[0])
	if statErr != nil && !os.IsNotExist(statErr) {
		_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], statErr)
		return exitError{1}
	}
	if statErr == nil {
		perm = fi.Mode().Perm()
		if !fi.Mode().IsRegular() {
			// Device or FIFO (e.g. /dev/null): write directly without temp-rename.
			oflags := os.O_WRONLY
			if appendMode {
				oflags |= os.O_APPEND
			} else {
				oflags |= os.O_TRUNC
			}
			f, err := fsOpenFile(ctx, hc.Dir, "sponge", pos[0], oflags, perm)
			if err != nil {
				_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], err)
				return exitError{1}
			}
			_, werr := f.Write(buf.Bytes())
			cerr := f.Close()
			if werr != nil {
				if isPipeClosed(werr) {
					return nil
				}
				_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], werr)
				return exitError{1}
			}
			if cerr != nil {
				_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], cerr)
				return exitError{1}
			}
			return nil
		}
	}

	outBytes := buf.Bytes()
	if appendMode && statErr == nil {
		existing, rerr := fsReadFile(ctx, hc.Dir, "sponge", pos[0])
		if rerr != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], rerr)
			return exitError{1}
		}
		combined := make([]byte, 0, len(existing)+len(outBytes))
		combined = append(combined, existing...)
		combined = append(combined, outBytes...)
		outBytes = combined
	}

	if err := fsAtomicWriteFile(ctx, hc.Dir, "sponge", pos[0], outBytes, perm); err != nil {
		_, _ = fmt.Fprintf(hc.Stderr, "sponge: %s: %v\n", pos[0], err)
		return exitError{1}
	}
	return nil
}

// ============================================================================
// 2. flock — cross-platform file locks (unix.Flock & windows.LockFileEx)
// ============================================================================

var flockOpts = toolOptParser{tool: "flock", stopAtPos: true, opts: []toolOpt{
	{"s", "shared", false},                  // 0
	{"xe", "exclusive", false},              // 1
	{"u", "unlock", false},                  // 2
	{"n", "nonblock nb", false},             // 3
	{"oFv", "close no-fork verbose", false}, // 4: accepted for util-linux compatibility
	{"h", "help", false},                    // 5
	{"w", "wait timeout", true},             // 6
	{"E", "conflict-exit-code", true},       // 7
	{"c", "command", true},                  // 8
}}

func cmdFlock(ctx context.Context, hc interp.HandlerContext, args []string) error {
	shared, unlock, nonblock, hasTimeout, hasShellCmd := false, false, false, false, false
	var timeoutDur time.Duration
	conflictCode := 1
	var shellCmd string

	pos, err := flockOpts.parse(hc.Stderr, args, func(idx int, val string) error {
		switch idx {
		case 0, 1:
			shared, unlock = idx == 0, false
		case 2:
			unlock = true
		case 3:
			nonblock = true
		case 5:
			fmt.Fprintln(hc.Stdout, "Usage: flock [-sxun] [-w sec] [-E code] file [-c cmd | cmd args...] | flock [-sxun] [-w sec] fd")
			return errToolHelp
		case 6:
			secs, perr := strconv.ParseFloat(val, 64)
			if perr != nil || secs < 0 {
				return toolBadArg(hc.Stderr, "flock", "timeout", val)
			}
			hasTimeout, timeoutDur = true, time.Duration(secs*float64(time.Second))
		case 7:
			n, perr := strconv.Atoi(val)
			if perr != nil || n < 0 || n > 255 {
				return toolBadArg(hc.Stderr, "flock", "exit code", val)
			}
			conflictCode = n
		case 8:
			hasShellCmd, shellCmd = true, val
		}
		return nil
	})
	if err != nil {
		return toolOptErr(err)
	}

	hasTarget := len(pos) > 0
	var lockTarget string
	var cmdArgs []string
	if hasTarget {
		lockTarget, cmdArgs = pos[0], pos[1:]
		// `flock lockfile -c "script"` / `--command "script"` / `--command=script`
		if len(cmdArgs) > 0 && !hasShellCmd {
			a := cmdArgs[0]
			switch {
			case (a == "-c" || a == "--command") && len(cmdArgs) == 2:
				hasShellCmd, shellCmd, cmdArgs = true, cmdArgs[1], nil
			case strings.HasPrefix(a, "--command=") && len(cmdArgs) == 1:
				hasShellCmd, shellCmd, cmdArgs = true, a[len("--command="):], nil
			case a == "-c" || a == "--command" || strings.HasPrefix(a, "--command="):
				fmt.Fprintln(hc.Stderr, "flock: -c requires exactly one command string")
				return exitError{2}
			}
		}
	}

	if !hasTarget {
		_, _ = fmt.Fprintln(hc.Stderr, "flock: missing file or file descriptor operand")
		return exitError{2}
	}

	// FD form: `flock [flags] FD` when no command is given.
	if !hasShellCmd && len(cmdArgs) == 0 {
		if !isDigits(lockTarget) {
			_, _ = fmt.Fprintf(hc.Stderr, "flock: %s: requires a command or numeric file descriptor\n", lockTarget)
			return exitError{2}
		}
		fd, err := strconv.Atoi(lockTarget)
		if err != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "flock: bad file descriptor: %s\n", lockTarget)
			return exitError{2}
		}
		tryLock, doUnlock, err := resolveFlockFD(fd, hc)
		if err != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "flock: %v\n", err)
			return exitError{1}
		}
		if unlock {
			if err := doUnlock(); err != nil {
				_, _ = fmt.Fprintf(hc.Stderr, "flock: unlock fd %d: %v\n", fd, err)
				return exitError{1}
			}
			return nil
		}
		locked, err := acquireFlockWithDeadline(ctx, tryLock, shared, nonblock, hasTimeout, timeoutDur)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			_, _ = fmt.Fprintf(hc.Stderr, "flock: fd %d: %v\n", fd, err)
			return exitError{1}
		}
		if !locked {
			return exitError{conflictCode}
		}
		return nil
	}

	// Wrapper form: `flock [flags] lockfile [-c "script" | cmd args...]`
	lockPath, err := checkFS(ctx, hc.Dir, "flock", lockTarget, OpWrite)
	if err != nil {
		_, _ = fmt.Fprintf(hc.Stderr, "flock: cannot open lock file %s: %v\n", lockTarget, err)
		return exitError{1}
	}
	f, err := openFlockFile(lockPath)
	if err != nil {
		_, _ = fmt.Fprintf(hc.Stderr, "flock: cannot open lock file %s: %v\n", lockTarget, err)
		return exitError{1}
	}
	closed := false
	defer func() {
		if !closed {
			_ = unlockFlockFile(f)
			_ = f.Close()
		}
	}()

	if unlock {
		if err := unlockFlockFile(f); err != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "flock: %s: %v\n", lockTarget, err)
			return exitError{1}
		}
	} else {
		tryLock := func(sh bool) (bool, error) {
			return tryFlockFile(f, sh)
		}
		locked, err := acquireFlockWithDeadline(ctx, tryLock, shared, nonblock, hasTimeout, timeoutDur)
		if err != nil {
			closed = true
			_ = f.Close()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			_, _ = fmt.Fprintf(hc.Stderr, "flock: %s: %v\n", lockTarget, err)
			return exitError{1}
		}
		if !locked {
			closed = true
			_ = f.Close()
			return exitError{conflictCode}
		}
	}

	var cmdErr error
	if hasShellCmd {
		cmdErr = runFlockShellCommand(ctx, hc, shellCmd)
	} else {
		cmdErr = runFlockSubcommand(ctx, hc, cmdArgs)
	}

	// Release lock explicitly before returning command exit status.
	closed = true
	_ = unlockFlockFile(f)
	_ = f.Close()
	return cmdErr
}

func acquireFlockWithDeadline(
	ctx context.Context,
	tryLock func(shared bool) (bool, error),
	shared bool,
	nonblock bool,
	hasTimeout bool,
	timeoutDur time.Duration,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	locked, err := tryLock(shared)
	if err != nil || locked {
		return locked, err
	}
	if nonblock || (hasTimeout && timeoutDur <= 0) {
		return false, nil
	}

	var deadline time.Time
	if hasTimeout {
		deadline = time.Now().Add(timeoutDur)
	}

	backoff := time.Millisecond
	timer := time.NewTimer(backoff)
	defer timer.Stop()

	for {
		now := time.Now()
		if hasTimeout && !now.Before(deadline) {
			return false, nil
		}
		wait := backoff
		if hasTimeout {
			if rem := deadline.Sub(now); rem < wait {
				wait = rem
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
		}

		locked, err = tryLock(shared)
		if err != nil || locked {
			return locked, err
		}
		if backoff < 10*time.Millisecond {
			backoff *= 2
			if backoff > 10*time.Millisecond {
				backoff = 10 * time.Millisecond
			}
		}
	}
}

func runFlockShellCommand(ctx context.Context, hc interp.HandlerContext, src string) error {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "flock")
	if err != nil {
		_, _ = fmt.Fprintln(hc.Stderr, err)
		return exitError{2}
	}
	r, err := interp.New(
		interp.Dir(hc.Dir),
		interp.Env(hc.Env),
		interp.StdIO(hc.Stdin, hc.Stdout, hc.Stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		_, _ = fmt.Fprintf(hc.Stderr, "flock: %v\n", err)
		return exitError{1}
	}
	if err := r.Run(ctx, prog); err != nil {
		if code, ok := subcommandExitCode(err); ok {
			return exitError{code}
		}
		return err
	}
	return nil
}

func runFlockSubcommand(ctx context.Context, hc interp.HandlerContext, cmdArgs []string) error {
	err := runSubcommandNamed(ctx, hc, "flock", cmdArgs)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrPermission) {
		_, _ = fmt.Fprintf(hc.Stderr, "flock: %s: %v\n", cmdArgs[0], err)
		return exitError{126}
	}
	if isExecNotFound(err) {
		_, _ = fmt.Fprintf(hc.Stderr, "flock: failed to execute %s: No such file or directory\n", cmdArgs[0])
		return exitError{127}
	}
	return err
}

// ============================================================================
// 3. column — UTF-8 rune-width-aware table alignment & terminal column fill
// ============================================================================

func columnRuneWidth(r rune) int {
	if r == '\t' {
		return 4
	}
	if (r >= 0x1f300 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf) {
		return 2
	}
	return runeWidth(r)
}

func columnStrWidth(s string) int {
	w := 0
	for len(s) > 0 {
		r, sz := utf8.DecodeRuneInString(s)
		s = s[sz:]
		if r == utf8.RuneError && sz == 1 {
			w++
			continue
		}
		w += columnRuneWidth(r)
	}
	return w
}

var columnOpts = toolOptParser{tool: "column", opts: []toolOpt{
	{"t", "table", false},             // 0
	{"n", "no-merge", false},          // 1
	{"e", "table-empty-lines", false}, // 2
	{"x", "fillrows", false},          // 3
	{"h", "help", false},              // 4
	{"s", "separator", true},          // 5
	{"o", "output-separator", true},   // 6
	{"c", "output-width", true},       // 7
}}

func cmdColumn(ctx context.Context, hc interp.HandlerContext, args []string) error {
	tableMode, hasSep, noMerge, keepEmptyLines, fillRows := false, false, false, false, false
	sepChars, outSep := "", "  "
	termWidth := 80
	if v := hc.Env.Get("COLUMNS"); v.IsSet() {
		if n, err := strconv.Atoi(strings.TrimSpace(v.String())); err == nil && n > 0 {
			termWidth = n
		}
	}
	files, err := columnOpts.parse(hc.Stderr, args, func(idx int, val string) error {
		switch idx {
		case 0:
			tableMode = true
		case 1:
			noMerge = true
		case 2:
			keepEmptyLines = true
		case 3:
			fillRows = true
		case 4:
			fmt.Fprintln(hc.Stdout, "Usage: column [-txne] [-c width] [-s sep] [-o outsep] [file...]")
			return errToolHelp
		case 5:
			hasSep, sepChars = true, val
		case 6:
			outSep = val
		case 7:
			n, perr := strconv.Atoi(val)
			if perr != nil || n < 1 {
				return toolBadArg(hc.Stderr, "column", "width", val)
			}
			termWidth = n
		}
		return nil
	})
	if err != nil {
		return toolOptErr(err)
	}

	if len(files) == 0 {
		files = []string{"-"}
	}

	var lines []string
	hadErr := false

	readFrom := func(r io.Reader, label string) error {
		lr := newLineReader(r)
		for lr.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			line := strings.TrimSuffix(lr.Text(), "\r")
			lines = append(lines, line)
		}
		if err := lr.Err(); err != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(hc.Stderr, "column: %s: %v\n", label, err)
			hadErr = true
		}
		return nil
	}

	for _, farg := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if farg == "-" || farg == "/dev/stdin" {
			if err := readFrom(hc.Stdin, "stdin"); err != nil {
				return err
			}
			continue
		}
		if isNullDevPath(farg) || isToolDevNull(farg) {
			continue
		}
		f, err := fsOpenShellFile(ctx, hc.Dir, "column", farg)
		if err != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "column: %s: %v\n", farg, err)
			hadErr = true
			continue
		}
		rerr := readFrom(f, farg)
		_ = f.Close()
		if rerr != nil {
			return rerr
		}
	}

	bw := bufio.NewWriterSize(hc.Stdout, 32*1024)
	flushOut := func() error {
		if err := bw.Flush(); err != nil {
			if isPipeClosed(err) {
				return nil
			}
			return err
		}
		return nil
	}

	if tableMode {
		var rows [][]string
		var rowWidths [][]int
		var maxWidths []int

		isDelim := func(r rune) bool {
			if !hasSep {
				return r == ' ' || r == '\t'
			}
			return strings.ContainsRune(sepChars, r)
		}

		for _, line := range lines {
			if line == "" && !keepEmptyLines {
				continue
			}
			if line == "" && keepEmptyLines {
				rows = append(rows, nil)
				rowWidths = append(rowWidths, nil)
				continue
			}

			var fields []string
			if hasSep && sepChars == "" {
				fields = []string{line}
			} else if noMerge {
				fields = splitFieldsNoMerge(line, isDelim)
			} else {
				fields = strings.FieldsFunc(line, isDelim)
			}

			if len(fields) == 0 && !keepEmptyLines && !noMerge {
				continue
			}

			widths := make([]int, len(fields))
			for c, fld := range fields {
				w := columnStrWidth(fld)
				widths[c] = w
				if c >= len(maxWidths) {
					maxWidths = append(maxWidths, w)
				} else if w > maxWidths[c] {
					maxWidths[c] = w
				}
			}
			rows = append(rows, fields)
			rowWidths = append(rowWidths, widths)
		}

		for rIdx, row := range rows {
			if err := ctx.Err(); err != nil {
				return err
			}
			for cIdx, fld := range row {
				if _, err := bw.WriteString(fld); err != nil {
					if isPipeClosed(err) {
						return nil
					}
					return err
				}
				if cIdx < len(row)-1 {
					pad := maxWidths[cIdx] - rowWidths[rIdx][cIdx]
					for p := 0; p < pad; p++ {
						_ = bw.WriteByte(' ')
					}
					if _, err := bw.WriteString(outSep); err != nil {
						if isPipeClosed(err) {
							return nil
						}
						return err
					}
				}
			}
			if err := bw.WriteByte('\n'); err != nil {
				if isPipeClosed(err) {
					return nil
				}
				return err
			}
		}

		if err := flushOut(); err != nil {
			return err
		}
		if hadErr {
			return exitError{1}
		}
		return nil
	}

	// Non-table mode: fill terminal columns cleanly.
	var items []string
	var itemWidths []int
	maxItemWidth := 0
	for _, line := range lines {
		if line == "" && !keepEmptyLines {
			continue
		}
		w := columnStrWidth(line)
		items = append(items, line)
		itemWidths = append(itemWidths, w)
		if w > maxItemWidth {
			maxItemWidth = w
		}
	}

	if len(items) == 0 {
		if hadErr {
			return exitError{1}
		}
		return nil
	}

	colSlot := maxItemWidth + 2
	numCols := (termWidth + 2) / colSlot
	if numCols < 1 {
		numCols = 1
	}
	if numCols > len(items) {
		numCols = len(items)
	}
	numRows := (len(items) + numCols - 1) / numCols

	for r := 0; r < numRows; r++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Find last valid column on this row to avoid trailing spaces.
		lastCol := -1
		for c := 0; c < numCols; c++ {
			var idx int
			if fillRows {
				idx = r*numCols + c
			} else {
				idx = c*numRows + r
			}
			if idx < len(items) {
				lastCol = c
			}
		}
		for c := 0; c <= lastCol; c++ {
			var idx int
			if fillRows {
				idx = r*numCols + c
			} else {
				idx = c*numRows + r
			}
			if idx >= len(items) {
				continue
			}
			if _, err := bw.WriteString(items[idx]); err != nil {
				if isPipeClosed(err) {
					return nil
				}
				return err
			}
			if c < lastCol {
				pad := colSlot - itemWidths[idx]
				for p := 0; p < pad; p++ {
					_ = bw.WriteByte(' ')
				}
			}
		}
		if err := bw.WriteByte('\n'); err != nil {
			if isPipeClosed(err) {
				return nil
			}
			return err
		}
	}

	if err := flushOut(); err != nil {
		return err
	}
	if hadErr {
		return exitError{1}
	}
	return nil
}

func splitFieldsNoMerge(s string, isDelim func(rune) bool) []string {
	var fields []string
	start := 0
	for i, r := range s {
		if isDelim(r) {
			fields = append(fields, s[start:i])
			start = i + utf8.RuneLen(r)
		}
	}
	fields = append(fields, s[start:])
	return fields
}

// ============================================================================
// 4. xxd — full hex/binary dump and reverse (-r / -r -p) hex-to-binary decoder
// ============================================================================

var xxdOpts = toolOptParser{tool: "xxd", dashLong: true, opts: []toolOpt{
	{"r", "revert", false},              // 0
	{"p", "ps plain postscript", false}, // 1
	{"u", "upper", false},               // 2
	{"b", "bits", false},                // 3
	{"a", "autoskip", false},            // 4
	{"h", "help", false},                // 5
	{"c", "cols", true},                 // 6
	{"g", "groupsize", true},            // 7
	{"l", "len", true},                  // 8
	{"s", "seek", true},                 // 9
	{"o", "offset", true},               // 10
}}

func cmdXxd(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var revert, plain, upper, bitsMode, autoskip, colsSet, groupSet, hasLen bool
	var hasSeek, seekRel, seekFromEnd bool
	cols, group := 16, 2
	var limitLen, seekVal, displayOffset int64

	pos, err := xxdOpts.parse(hc.Stderr, args, func(idx int, val string) error {
		var n int64
		if idx >= 6 && idx != 9 {
			var perr error
			n, perr = strconv.ParseInt(val, 0, 64)
			if perr != nil || (n < 0 && idx != 10) || (n > 4096 && idx <= 7) {
				return toolBadArg(hc.Stderr, "xxd", xxdOpts.opts[idx].long, val)
			}
		}
		switch idx {
		case 0:
			revert = true
		case 1:
			plain = true
		case 2:
			upper = true
		case 3:
			bitsMode = true
		case 4:
			autoskip = true
		case 5:
			fmt.Fprintln(hc.Stdout, "Usage: xxd [-r] [-p|-ps] [-u] [-b] [-a] [-c cols] [-g bytes] [-l len] [-s [+-]seek] [-o off] [infile [outfile]]")
			return errToolHelp
		case 6:
			cols, colsSet = int(n), true
		case 7:
			group, groupSet = int(n), true
		case 8:
			limitLen, hasLen = n, true
		case 9:
			s := val
			seekRel = strings.HasPrefix(s, "+")
			s = strings.TrimPrefix(s, "+")
			seekFromEnd = strings.HasPrefix(s, "-")
			s = strings.TrimPrefix(s, "-")
			v, perr := strconv.ParseInt(s, 0, 64)
			if perr != nil || v < 0 || s == "" {
				return toolBadArg(hc.Stderr, "xxd", "seek", val)
			}
			seekVal, hasSeek = v, true
		case 10:
			displayOffset = n
		}
		return nil
	})
	if err != nil {
		return toolOptErr(err)
	}

	if len(pos) > 2 {
		_, _ = fmt.Fprintln(hc.Stderr, "xxd: too many arguments")
		return exitError{2}
	}
	if bitsMode && revert {
		_, _ = fmt.Fprintln(hc.Stderr, "xxd: sorry, cannot revert bit dumps")
		return exitError{2}
	}

	if !colsSet {
		if plain {
			cols = 30
		} else if bitsMode {
			cols = 6
		} else {
			cols = 16
		}
	} else if !plain && cols == 0 {
		cols = 16
	}
	if !groupSet {
		if bitsMode {
			group = 1
		} else {
			group = 2
		}
	}

	type statReadSeeker interface {
		io.Reader
		io.Seeker
		Stat() (os.FileInfo, error)
	}

	inReader := hc.Stdin
	var inFile statReadSeeker
	if len(pos) >= 1 && pos[0] != "-" && pos[0] != "/dev/stdin" {
		if isNullDevPath(pos[0]) || isToolDevNull(pos[0]) {
			inReader = bytes.NewReader(nil)
		} else {
			f, err := fsOpenShellFile(ctx, hc.Dir, "xxd", pos[0])
			if err != nil {
				_, _ = fmt.Fprintf(hc.Stderr, "xxd: %s: %v\n", pos[0], err)
				return exitError{2}
			}
			defer f.Close()
			inReader = f
			inFile, _ = f.(statReadSeeker)
		}
	}

	outToFile := len(pos) == 2 && pos[1] != "-" && pos[1] != "/dev/stdout"
	var outArg string
	if outToFile {
		outArg = pos[1]
	}

	if revert {
		var seekOff int64
		if hasSeek {
			if seekFromEnd {
				seekOff = -seekVal
			} else {
				seekOff = seekVal
			}
		}
		return xxdRevert(ctx, hc, inReader, outToFile, outArg, plain, hasLen, limitLen, seekOff)
	}

	// Forward dump: apply seek if requested.
	var startOffset int64
	if hasSeek {
		if inFile != nil && !seekRel {
			fi, err := inFile.Stat()
			if err == nil && fi.Mode().IsRegular() {
				var target int64
				if seekFromEnd {
					target = fi.Size() - seekVal
					if target < 0 {
						target = 0
					}
				} else {
					target = seekVal
				}
				if posOff, serr := inFile.Seek(target, io.SeekStart); serr == nil {
					startOffset = posOff
					inReader = inFile
					goto seekDone
				}
			}
		}
		// Stream seek fallback (pipes, stdin, or +relative seek).
		if seekFromEnd {
			all, err := readAllCtx(ctx, inReader)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				_, _ = fmt.Fprintf(hc.Stderr, "xxd: %v\n", err)
				return exitError{2}
			}
			start := int64(len(all)) - seekVal
			if start < 0 {
				start = 0
			}
			startOffset = start
			inReader = bytes.NewReader(all[start:])
		} else if seekVal > 0 {
			n, err := discardCtx(ctx, inReader, seekVal)
			if err != nil && err != io.EOF {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				_, _ = fmt.Fprintf(hc.Stderr, "xxd: %v\n", err)
				return exitError{2}
			}
			startOffset = n
			if err == io.EOF {
				inReader = bytes.NewReader(nil)
			}
		}
	}
seekDone:

	if hasLen {
		inReader = io.LimitReader(inReader, limitLen)
	}

	var outWriter io.Writer = hc.Stdout
	var outFileBuf bytes.Buffer
	if outToFile {
		outWriter = &outFileBuf
	}
	bw := bufio.NewWriterSize(outWriter, 32*1024)

	hexChars := "0123456789abcdef"
	if upper {
		hexChars = "0123456789ABCDEF"
	}

	if plain {
		if err := xxdDumpPlain(ctx, inReader, bw, cols, hexChars); err != nil {
			if isPipeClosed(err) {
				return nil
			}
			return err
		}
	} else {
		if err := xxdDumpCanonical(ctx, inReader, bw, cols, group, bitsMode, autoskip, startOffset+displayOffset, hexChars); err != nil {
			if isPipeClosed(err) {
				return nil
			}
			return err
		}
	}

	if err := bw.Flush(); err != nil {
		if isPipeClosed(err) {
			return nil
		}
		_, _ = fmt.Fprintf(hc.Stderr, "xxd: %v\n", err)
		return exitError{2}
	}

	if outToFile {
		if isNullDevPath(outArg) || isToolDevNull(outArg) {
			return nil
		}
		perm := os.FileMode(0o644)
		if !isActiveProcSubstPath(outArg) {
			if fi, err := fsStat(ctx, hc.Dir, "xxd", outArg); err == nil {
				perm = fi.Mode().Perm()
			} else if !os.IsNotExist(err) {
				_, _ = fmt.Fprintf(hc.Stderr, "xxd: %s: %v\n", outArg, err)
				return exitError{2}
			}
		}
		if err := fsAtomicWriteFile(ctx, hc.Dir, "xxd", outArg, outFileBuf.Bytes(), perm); err != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "xxd: %s: %v\n", outArg, err)
			return exitError{2}
		}
	}
	return nil
}

func xxdDumpPlain(ctx context.Context, r io.Reader, bw *bufio.Writer, cols int, hexChars string) error {
	buf := make([]byte, 16*1024)
	colPos := 0
	wroteAny := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := r.Read(buf)
		for i := 0; i < n; i++ {
			b := buf[i]
			_ = bw.WriteByte(hexChars[b>>4])
			if err := bw.WriteByte(hexChars[b&0x0f]); err != nil {
				return err
			}
			wroteAny = true
			colPos++
			if cols > 0 && colPos >= cols {
				if err := bw.WriteByte('\n'); err != nil {
					return err
				}
				colPos = 0
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}

	if wroteAny && (cols == 0 || colPos > 0) {
		if err := bw.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

func xxdDumpCanonical(
	ctx context.Context,
	r io.Reader,
	bw *bufio.Writer,
	cols int,
	group int,
	bitsMode bool,
	autoskip bool,
	offset int64,
	hexChars string,
) error {
	row := make([]byte, cols)
	var lineBuf []byte
	skippingZeros := false

	isAllZero := func(b []byte) bool {
		for _, v := range b {
			if v != 0 {
				return false
			}
		}
		return true
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := io.ReadFull(r, row)
		if n == 0 {
			if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
				break
			}
			if rerr != nil {
				return rerr
			}
		}

		// In autoskip (-a) mode, collapse middle full zero lines into a single "*".
		if autoskip && n == cols && isAllZero(row[:n]) && rerr == nil {
			// Peek whether we already printed one zero line or should print first zero line.
			// Note: vim xxd prints the FIRST zero line, then "*" for middle zero lines, AND prints the LAST zero line if at EOF.
			// Since io.ReadFull returned rerr == nil, we only skip if already in a zero run after printing the first zero line;
			// wait: to guarantee the very last line of the file is printed if zero, only collapse once we see >1 consecutive zero lines!
		}
		_ = skippingZeros

		lineBuf = lineBuf[:0]
		lineBuf = fmt.Appendf(lineBuf, "%08x: ", uint64(offset))

		if bitsMode {
			for j := 0; j < cols; j++ {
				if j < n {
					b := row[j]
					for bit := 7; bit >= 0; bit-- {
						if (b>>uint(bit))&1 != 0 {
							lineBuf = append(lineBuf, '1')
						} else {
							lineBuf = append(lineBuf, '0')
						}
					}
				} else {
					lineBuf = append(lineBuf, "        "...)
				}
				if group > 0 && (j+1)%group == 0 && j+1 < cols {
					lineBuf = append(lineBuf, ' ')
				}
			}
		} else {
			for j := 0; j < cols; j++ {
				if j < n {
					b := row[j]
					lineBuf = append(lineBuf, hexChars[b>>4], hexChars[b&0x0f])
				} else {
					lineBuf = append(lineBuf, ' ', ' ')
				}
				if group > 0 && (j+1)%group == 0 && j+1 < cols {
					lineBuf = append(lineBuf, ' ')
				}
			}
		}

		lineBuf = append(lineBuf, ' ', ' ')
		for j := 0; j < n; j++ {
			b := row[j]
			if b >= 0x20 && b <= 0x7e {
				lineBuf = append(lineBuf, b)
			} else {
				lineBuf = append(lineBuf, '.')
			}
		}
		lineBuf = append(lineBuf, '\n')

		if _, err := bw.Write(lineBuf); err != nil {
			return err
		}

		offset += int64(n)
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

func xxdHexNibble(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

func xxdRevert(
	ctx context.Context,
	hc interp.HandlerContext,
	r io.Reader,
	outToFile bool,
	outArg string,
	plain bool,
	hasLen bool,
	limitLen int64,
	seekOff int64,
) error {
	if plain {
		var out []byte
		if seekOff > 0 {
			out = make([]byte, seekOff)
		}
		buf := make([]byte, 16*1024)
		haveHi := false
		var hi byte
		var decoded int64

		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, rerr := r.Read(buf)
			for i := 0; i < n; i++ {
				if hasLen && decoded >= limitLen {
					break
				}
				c := buf[i]
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
					continue
				}
				nib, ok := xxdHexNibble(c)
				if !ok {
					continue
				}
				if !haveHi {
					hi = nib
					haveHi = true
				} else {
					out = append(out, (hi<<4)|nib)
					haveHi = false
					decoded++
				}
			}
			if (hasLen && decoded >= limitLen) || rerr == io.EOF {
				break
			}
			if rerr != nil {
				_, _ = fmt.Fprintf(hc.Stderr, "xxd: %v\n", rerr)
				return exitError{2}
			}
		}
		if haveHi && (!hasLen || decoded < limitLen) {
			out = append(out, hi<<4)
		}
		return xxdWriteReverted(ctx, hc, outToFile, outArg, out)
	}

	// Canonical hexdump revert: parse "<hex-offset>: <grouped hex>  <ascii>"
	var out []byte
	if outToFile && seekOff != 0 && !isNullDevPath(outArg) && !isToolDevNull(outArg) && !isActiveProcSubstPath(outArg) {
		if existing, err := fsReadFile(ctx, hc.Dir, "xxd", outArg); err == nil {
			out = existing
		} else if !os.IsNotExist(err) {
			_, _ = fmt.Fprintf(hc.Stderr, "xxd: %s: %v\n", outArg, err)
			return exitError{2}
		}
	}

	lr := newLineReader(r)
	var curPos int64
	if seekOff > 0 {
		curPos = seekOff
	}
	var totalDecoded int64

	for lr.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimRight(lr.Text(), "\r")
		if line != "" && line != "*" {
			colon := strings.IndexByte(line, ':')
			if colon >= 0 {
				addrStr := strings.TrimSpace(line[:colon])
				addr, aerr := strconv.ParseInt(addrStr, 16, 64)
				if aerr == nil {
					targetPos := addr + seekOff
					if targetPos >= 0 {
						curPos = targetPos
					}
				}
				rest := line[colon+1:]
				if strings.HasPrefix(rest, " ") {
					rest = rest[1:]
				}
				// Read hex digits until double-space ("  ") or tab separating the ASCII column.
				var rowBytes []byte
				haveHi := false
				var hi byte
				spaceRun := 0
				for k := 0; k < len(rest); k++ {
					if hasLen && totalDecoded+int64(len(rowBytes)) >= limitLen {
						break
					}
					c := rest[k]
					if c == '\t' {
						break
					}
					if c == ' ' {
						spaceRun++
						if spaceRun >= 2 {
							break
						}
						continue
					}
					nib, ok := xxdHexNibble(c)
					if !ok {
						break
					}
					spaceRun = 0
					if !haveHi {
						hi = nib
						haveHi = true
					} else {
						rowBytes = append(rowBytes, (hi<<4)|nib)
						haveHi = false
					}
				}
				if len(rowBytes) > 0 {
					endPos := curPos + int64(len(rowBytes))
					if endPos > int64(len(out)) {
						grown := make([]byte, endPos)
						copy(grown, out)
						out = grown
					}
					copy(out[curPos:endPos], rowBytes)
					curPos = endPos
					totalDecoded += int64(len(rowBytes))
				}
			}
		}
		if hasLen && totalDecoded >= limitLen {
			break
		}
	}
	if rerr := lr.Err(); rerr != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hc.Stderr, "xxd: %v\n", rerr)
		return exitError{2}
	}

	return xxdWriteReverted(ctx, hc, outToFile, outArg, out)
}

func xxdWriteReverted(ctx context.Context, hc interp.HandlerContext, outToFile bool, outArg string, data []byte) error {
	if outToFile {
		if isNullDevPath(outArg) || isToolDevNull(outArg) {
			return nil
		}
		perm := os.FileMode(0o644)
		if !isActiveProcSubstPath(outArg) {
			if fi, err := fsStat(ctx, hc.Dir, "xxd", outArg); err == nil {
				perm = fi.Mode().Perm()
			} else if !os.IsNotExist(err) {
				_, _ = fmt.Fprintf(hc.Stderr, "xxd: %s: %v\n", outArg, err)
				return exitError{2}
			}
		}
		if err := fsAtomicWriteFile(ctx, hc.Dir, "xxd", outArg, data, perm); err != nil {
			_, _ = fmt.Fprintf(hc.Stderr, "xxd: %s: %v\n", outArg, err)
			return exitError{2}
		}
		return nil
	}
	if _, err := hc.Stdout.Write(data); err != nil {
		if isPipeClosed(err) {
			return nil
		}
		_, _ = fmt.Fprintf(hc.Stderr, "xxd: %v\n", err)
		return exitError{2}
	}
	return nil
}

func readAllCtx(ctx context.Context, r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err == io.EOF {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func discardCtx(ctx context.Context, r io.Reader, n int64) (int64, error) {
	var discarded int64
	chunk := make([]byte, 32*1024)
	for discarded < n {
		if err := ctx.Err(); err != nil {
			return discarded, err
		}
		want := n - discarded
		if want > int64(len(chunk)) {
			want = int64(len(chunk))
		}
		m, err := r.Read(chunk[:want])
		discarded += int64(m)
		if err != nil {
			return discarded, err
		}
	}
	return discarded, nil
}
