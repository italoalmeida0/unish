package unish

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	extraCommands = append(extraCommands,
		extraCmd{"sed", cmdSed},
		extraCmd{"timeout", cmdTimeout},
	)
}

type sedAddr struct {
	kind int
	line int
	re   *regexp.Regexp
	step int
	zero bool

	// addr2 relative forms: +N (N lines after the range starts) and
	// ~N (up to the next line whose number is a multiple of N).
	plus  int
	tilde int

	// lazy "previous regular expression" support: pat == "" means the
	// address reuses the last regex used (GNU semantics)
	pat      string
	reNocase bool
	reExt    bool
}

type sedCmd struct {
	a1, a2  *sedAddr
	bang    bool
	name    byte
	arg     string
	sub     *sedSub
	label   string
	rangeID int
	endPC   int // for '{': PC immediately after matching '}'
}

type sedSub struct {
	re *regexp.Regexp
	// rePat == "" means "use the previous regular expression" (GNU).
	rePat   string
	reExt   bool
	ngroups int
	repl    string
	plain   bool
	global  bool
	nth     int
	nocase  bool
	print   bool
	write   string
}

func extractSedFlags(args []string) (cleanArgs []string, inplaceOn bool, inplaceSuffix string) {
	var flagArgs, posArgs []string
	skipNext := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if skipNext {
			skipNext = false
			flagArgs = append(flagArgs, a)
			continue
		}
		if a == "--" {
			posArgs = append(posArgs, args[i+1:]...)
			break
		}
		if a == "-i" || a == "--in-place" {
			inplaceOn = true
			inplaceSuffix = ""
			continue
		}
		if strings.HasPrefix(a, "--in-place=") {
			inplaceOn = true
			inplaceSuffix = strings.TrimPrefix(a, "--in-place=")
			continue
		}
		if a == "-e" || a == "--expression" || a == "-f" || a == "--file" {
			skipNext = true
			flagArgs = append(flagArgs, a)
			continue
		}
		if strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file=") {
			flagArgs = append(flagArgs, a)
			continue
		}
		if len(a) >= 2 && a[0] == '-' && a[1] != '-' {
			var boolCluster []string
			handled := false
			for j := 1; j < len(a); j++ {
				ch := a[j]
				switch ch {
				case 'n', 'E', 'r':
					boolCluster = append(boolCluster, "-"+string(ch))
				case 'e', 'f':
					flagArgs = append(flagArgs, boolCluster...)
					flagArgs = append(flagArgs, "-"+string(ch))
					if j+1 < len(a) {
						flagArgs = append(flagArgs, a[j+1:])
					} else {
						skipNext = true
					}
					handled = true
				case 'i':
					flagArgs = append(flagArgs, boolCluster...)
					inplaceOn = true
					inplaceSuffix = a[j+1:]
					handled = true
				default:
					j = len(a)
				}
				if handled {
					break
				}
			}
			if handled {
				continue
			}
			if len(boolCluster) == len(a)-1 {
				flagArgs = append(flagArgs, boolCluster...)
				continue
			}
			flagArgs = append(flagArgs, a)
			continue
		}
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			flagArgs = append(flagArgs, a)
			continue
		}
		posArgs = append(posArgs, a)
	}
	cleanArgs = append(cleanArgs, flagArgs...)
	if len(posArgs) > 0 {
		cleanArgs = append(cleanArgs, "--")
		cleanArgs = append(cleanArgs, posArgs...)
	}
	return cleanArgs, inplaceOn, inplaceSuffix
}

func cmdSed(ctx context.Context, hc interp.HandlerContext, args []string) error {
	cleanArgs, inplaceOn, inplace := extractSedFlags(args[1:])
	fs := newFlagSet("sed", hc.Stderr)
	quiet := fs.Bool("n", false, "")
	fs.BoolVar(quiet, "quiet", false, "")
	fs.BoolVar(quiet, "silent", false, "")
	extended := fs.Bool("E", false, "")
	fs.BoolVar(extended, "r", false, "")
	fs.BoolVar(extended, "regexp-extended", false, "")
	var exprs stringList
	fs.Var(&exprs, "e", "")
	fs.Var(&exprs, "expression", "")
	var files stringList
	fs.Var(&files, "f", "")
	fs.Var(&files, "file", "")
	if err := fs.Parse(cleanArgs); err != nil {
		return err
	}
	var src strings.Builder
	for _, e := range exprs {
		src.WriteString(e)
		src.WriteString("\n")
	}
	for _, f := range files {
		data, err := fsReadInputFile(ctx, hc.Dir, "sed", f)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "sed:", err)
			return exitError{1}
		}
		src.Write(data)
		src.WriteString("\n")
	}
	rest := fs.Args()
	var inputs []string
	progParts := []string{src.String()}
	if len(exprs) > 0 || len(files) > 0 {
		inputs = rest
	} else if len(rest) > 0 {
		progParts = append(progParts, rest[0]+"\n")
		inputs = rest[1:]
	}
	prog, err := parseSed(strings.Join(progParts, "\n"), *extended)
	if err != nil {
		// GNU wording: "sed: -e expression #1, char 1: unknown command: `['".
		fmt.Fprintf(hc.Stderr, "sed: -e expression #1, char 1: %v\n", dropSedPrefix(err))
		return exitError{1}
	}
	if inplaceOn {
		return sedInplace(ctx, hc, prog, *quiet, *extended, inputs, inplace)
	}
	if ok, ferr := sedTryFastStream(ctx, hc, prog, *quiet, inputs); ok {
		return ferr
	}
	r := newSedRunner(ctx, hc, prog, *quiet, *extended, hc.Stdout)
	stream := &sedStream{ctx: ctx, hc: hc, inputs: inputs}
	r.runStream(prog, stream)
	stream.closeCur()
	for _, f := range r.wfiles {
		f.Close()
	}
	if r.writeErr != nil {
		if isPipeClosed(r.writeErr) {
			return r.writeErr
		}
		fmt.Fprintln(hc.Stderr, "sed:", r.writeErr)
		return exitError{1}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if stream.openErr && r.err == nil && r.exitCode == 0 {
		return exitError{1}
	}
	return sedFinish(hc, r)
}

// sedExitCode parses the optional N argument of qN/QN.
func sedExitCode(arg string) int {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return 0
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 0 || n > 255 {
		return 1
	}
	return n
}

// sedFinish converts runner state into GNU-compatible errors/exit codes.
func sedFinish(hc interp.HandlerContext, r *sedRunner) error {
	if r.err != nil {
		fmt.Fprintf(hc.Stderr, "sed: -e expression #1, char 0: %v\n", r.err)
		return exitError{1}
	}
	if r.exitCode != 0 {
		return exitError{r.exitCode}
	}
	return nil
}

func parseSed(src string, extended bool) ([]*sedCmd, error) {
	// Normalize CRLF to LF for script parsing while keeping raw command bytes intact.
	s := strings.ReplaceAll(src, "\r\n", "\n")
	var prog []*sedCmd
	var blockStack []int
	rangeSeq := 0

	for len(s) > 0 {
		// Skip command separators and whitespace.
		for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\r' || s[0] == '\n' || s[0] == ';') {
			s = s[1:]
		}
		if len(s) == 0 {
			break
		}
		if s[0] == '#' {
			if nl := strings.IndexByte(s, '\n'); nl >= 0 {
				s = s[nl+1:]
			} else {
				s = ""
			}
			continue
		}
		if s[0] == '}' {
			if len(blockStack) == 0 {
				return nil, fmt.Errorf("unexpected `}'")
			}
			openIdx := blockStack[len(blockStack)-1]
			blockStack = blockStack[:len(blockStack)-1]
			prog = append(prog, &sedCmd{name: '}'})
			prog[openIdx].endPC = len(prog)
			s = s[1:]
			continue
		}

		var a1, a2 *sedAddr
		var err error
		s, a1, err = parseAddr(s, extended)
		if err != nil {
			return nil, err
		}
		s = strings.TrimLeft(s, " \t")
		if a1 != nil && strings.HasPrefix(s, ",") {
			s = strings.TrimLeft(s[1:], " \t")
			if len(s) > 1 && (s[0] == '+' || s[0] == '~') && s[1] >= '0' && s[1] <= '9' {
				i := 1
				for i < len(s) && s[i] >= '0' && s[i] <= '9' {
					i++
				}
				n, _ := strconv.Atoi(s[1:i])
				if s[0] == '+' {
					a2 = &sedAddr{kind: 6, plus: n}
				} else {
					a2 = &sedAddr{kind: 7, tilde: n}
				}
				s = s[i:]
			} else {
				s, a2, err = parseAddr(s, extended)
				if err != nil {
					return nil, err
				}
			}
		}
		s = strings.TrimLeft(s, " \t")
		bang := false
		if strings.HasPrefix(s, "!") {
			bang = true
			s = strings.TrimLeft(s[1:], " \t")
		}
		if len(s) == 0 || s[0] == '\n' || s[0] == ';' || s[0] == '}' {
			return nil, fmt.Errorf("missing command")
		}

		name := s[0]
		s = s[1:]
		rangeSeq++
		c := &sedCmd{a1: a1, a2: a2, bang: bang, name: name, rangeID: rangeSeq}

		switch name {
		case '{':
			blockStack = append(blockStack, len(prog))
			prog = append(prog, c)
		case 's':
			var sub *sedSub
			sub, s, err = parseSubStream(s, extended)
			if err != nil {
				return nil, err
			}
			c.sub = sub
			prog = append(prog, c)
		case 'y':
			if len(s) == 0 {
				return nil, fmt.Errorf("y: missing delimiter")
			}
			d := s[0]
			fromSet, rest2, ok := takeUntil(s[1:], d)
			if !ok {
				return nil, fmt.Errorf("y: need 2 sets")
			}
			toSet, rest3, ok := takeUntil(rest2, d)
			if !ok {
				return nil, fmt.Errorf("y: need 2 sets")
			}
			c.arg = fromSet + "\x00" + toSet
			s = rest3
			prog = append(prog, c)
		case 'a', 'i', 'c':
			s = strings.TrimLeft(s, " \t")
			if strings.HasPrefix(s, "\\\n") {
				s = s[2:]
				var textBuf strings.Builder
				for {
					nl := strings.IndexByte(s, '\n')
					var linePart string
					if nl >= 0 {
						linePart = s[:nl]
						s = s[nl+1:]
					} else {
						linePart = s
						s = ""
					}
					if hasTrailingSingleBackslash(linePart) {
						textBuf.WriteString(unescapeSedText(linePart[:len(linePart)-1]))
						textBuf.WriteByte('\n')
						if len(s) == 0 {
							break
						}
						continue
					}
					textBuf.WriteString(unescapeSedText(linePart))
					break
				}
				c.arg = textBuf.String()
			} else {
				if strings.HasPrefix(s, "\\") {
					s = s[1:]
				}
				end := len(s)
				for k := 0; k < len(s); k++ {
					if s[k] == '\\' && k+1 < len(s) {
						k++
						continue
					}
					if s[k] == '\n' || (len(blockStack) > 0 && s[k] == '}') {
						end = k
						break
					}
				}
				c.arg = unescapeSedText(s[:end])
				s = s[end:]
				if len(s) > 0 && s[0] == '\n' {
					s = s[1:]
				}
			}
			prog = append(prog, c)
		case 'r', 'w':
			s = strings.TrimLeft(s, " \t")
			end := len(s)
			for k := 0; k < len(s); k++ {
				if s[k] == '\n' || s[k] == ';' || s[k] == '}' {
					end = k
					break
				}
			}
			c.arg = strings.TrimSpace(s[:end])
			s = s[end:]
			prog = append(prog, c)
		case ':', 'b', 't', 'T':
			s = strings.TrimLeft(s, " \t")
			end := len(s)
			for k := 0; k < len(s); k++ {
				if s[k] == '\n' || s[k] == ';' || s[k] == '}' || s[k] == '#' || s[k] == ' ' || s[k] == '\t' {
					end = k
					break
				}
			}
			c.label = s[:end]
			s = strings.TrimLeft(s[end:], " \t")
			prog = append(prog, c)
		case 'q', 'Q':
			s = strings.TrimLeft(s, " \t")
			end := 0
			for end < len(s) && s[end] >= '0' && s[end] <= '9' {
				end++
			}
			c.arg = s[:end]
			s = s[end:]
			prog = append(prog, c)
		case 'd', 'D', 'p', 'P', 'n', 'N', 'h', 'H', 'g', 'G', 'x', 'l', '=':
			prog = append(prog, c)
		default:
			return nil, fmt.Errorf("unknown command: `%c'", name)
		}
	}
	if len(blockStack) > 0 {
		return nil, fmt.Errorf("unmatched `{'")
	}
	return prog, nil
}

func hasTrailingSingleBackslash(s string) bool {
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

func parseAddr(s string, extended bool) (string, *sedAddr, error) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return s, nil, nil
	}
	if s[0] == '$' {
		return s[1:], &sedAddr{kind: 2}, nil
	}
	if s[0] == '/' || (s[0] == '\\' && len(s) > 1) {
		var d byte = '/'
		rest := s[1:]
		if s[0] == '\\' {
			d = s[1]
			rest = s[2:]
		}
		end := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == '\\' {
				i++
				continue
			}
			if rest[i] == d {
				end = i
				break
			}
		}
		if end < 0 {
			return s, nil, fmt.Errorf("unterminated address regex")
		}
		body := rest[:end]
		var re *regexp.Regexp
		if body != "" {
			var err error
			re, err = compileSedRe(body, false, extended)
			if err != nil {
				return s, nil, err
			}
		}
		rest2, a := parseStep(rest[end+1:], &sedAddr{kind: 3, re: re, pat: body, reExt: extended})
		return rest2, a, nil
	}
	if s[0] >= '0' && s[0] <= '9' {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		n, _ := strconv.Atoi(s[:i])
		a := &sedAddr{kind: 1, line: n}
		if n == 0 {
			a.zero = true
		}
		rest, a := parseStep(s[i:], a)
		return rest, a, nil
	}
	return s, nil, nil
}

func parseStep(s string, a *sedAddr) (string, *sedAddr) {
	if strings.HasPrefix(s, "~") {
		i := 1
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if n, err := strconv.Atoi(s[1:i]); err == nil {
			a.step = n
		}
		return s[i:], a
	}
	return s, a
}

func parseSubStream(s string, extended bool) (*sedSub, string, error) {
	if s == "" || s[0] == '\n' {
		return nil, s, fmt.Errorf("unterminated `s' command")
	}
	d := s[0]
	rest := s[1:]
	pat, rest2, ok := takeUntil(rest, d)
	if !ok {
		return nil, s, fmt.Errorf("unterminated `s' command")
	}
	repl, rest3, ok := takeUntil(rest2, d)
	if !ok {
		return nil, s, fmt.Errorf("unterminated `s' command")
	}
	sub := &sedSub{rePat: pat, reExt: extended}
	if pat != "" {
		re, err := compileSedRe(pat, false, extended)
		if err != nil {
			return nil, s, err
		}
		sub.re = re
		sub.ngroups = re.NumSubexp()
	}
	sub.repl = repl
	sub.plain = isPlainRepl(repl)
	i := 0
	for i < len(rest3) {
		f := rest3[i]
		if f == '\n' || f == ';' || f == '}' || f == '#' {
			break
		}
		switch f {
		case ' ', '\t':
			i++
		case 'g':
			sub.global = true
			i++
		case 'p':
			sub.print = true
			i++
		case 'i', 'I':
			sub.nocase = true
			if pat != "" {
				re2, err := compileSedRe(pat, true, extended)
				if err != nil {
					return nil, s, err
				}
				sub.re = re2
			}
			i++
		case 'm', 'M', 'e':
			i++
		case 'w':
			i++
			for i < len(rest3) && (rest3[i] == ' ' || rest3[i] == '\t') {
				i++
			}
			end := i
			for end < len(rest3) && rest3[end] != '\n' && rest3[end] != ';' && rest3[end] != '}' {
				end++
			}
			sub.write = strings.TrimSpace(rest3[i:end])
			i = end
		default:
			if f >= '0' && f <= '9' {
				sub.nth = sub.nth*10 + int(f-'0')
				i++
			} else {
				return nil, s, fmt.Errorf("unknown option to `s'")
			}
		}
	}
	if pat != "" {
		if err := checkSubRefs(repl, sub.ngroups); err != nil {
			return nil, s, err
		}
	}
	return sub, rest3[i:], nil
}

func takeUntil(s string, d byte) (string, string, bool) {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			sb.WriteByte(s[i])
			sb.WriteByte(s[i+1])
			i++
			continue
		}
		if s[i] == d {
			return sb.String(), s[i+1:], true
		}
		sb.WriteByte(s[i])
	}
	return "", s, false
}

func splitDelim(s string, d byte, n int) []string {
	var parts []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			cur += s[i : i+2]
			i++
			continue
		}
		if s[i] == d && len(parts) < n-1 {
			parts = append(parts, cur)
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	parts = append(parts, cur)
	return parts
}

func compileSedRe(pat string, nocase, extended bool) (*regexp.Regexp, error) {
	if !extended {
		pat = breToGo(pat)
	}
	if nocase {
		pat = "(?i)" + pat
	}
	return regexp.Compile(pat)
}

func unescapeSedText(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case '\\':
				sb.WriteByte('\\')
			default:
				sb.WriteByte(s[i])
			}
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

type sedStream struct {
	ctx       context.Context
	hc        interp.HandlerContext
	inputs    []string // empty means read singleR / hc.Stdin
	singleR   io.Reader
	usedR     bool
	inIdx     int
	curFile   io.Closer
	lr        *lineReader
	openErr   bool
	hasPeek   bool
	peekLine  string
	peekHasNL bool
	eof       bool
}

func (s *sedStream) closeCur() {
	if s.curFile != nil {
		s.curFile.Close()
		s.curFile = nil
	}
	s.lr = nil
}

func (s *sedStream) buffered() int {
	if s.hasPeek {
		return 1
	}
	if s.lr != nil && s.lr.br != nil {
		return s.lr.br.Buffered()
	}
	return 0
}

func (s *sedStream) nextRaw() (string, bool, bool) {
	if s.eof {
		return "", false, false
	}
	for {
		if s.ctx != nil && s.ctx.Err() != nil {
			s.eof = true
			return "", false, false
		}
		if s.lr != nil {
			if s.lr.Scan() {
				return s.lr.Text(), s.lr.EndedWithNewline(), true
			}
			s.closeCur()
		}
		if len(s.inputs) == 0 {
			if s.usedR {
				s.eof = true
				return "", false, false
			}
			s.usedR = true
			rd := s.singleR
			if rd == nil {
				rd = s.hc.Stdin
			}
			if rd == nil {
				s.eof = true
				return "", false, false
			}
			s.lr = newLineReader(rd)
			continue
		}
		if s.inIdx >= len(s.inputs) {
			s.eof = true
			return "", false, false
		}
		in := s.inputs[s.inIdx]
		s.inIdx++
		if in == "-" || in == "/dev/stdin" {
			rd := s.hc.Stdin
			if rd == nil {
				continue
			}
			s.lr = newLineReader(rd)
			continue
		}
		f, err := fsOpenInput(s.ctx, s.hc.Dir, "sed", in)
		if err != nil {
			fmt.Fprintln(s.hc.Stderr, "sed:", err)
			s.openErr = true
			continue
		}
		s.curFile = f
		s.lr = newLineReader(f)
	}
}

func (s *sedStream) readLine() (string, bool, bool) {
	if s.hasPeek {
		line, nl := s.peekLine, s.peekHasNL
		s.hasPeek = false
		s.peekLine = ""
		return line, nl, true
	}
	return s.nextRaw()
}

func (s *sedStream) isEOF() bool {
	if s.hasPeek {
		return false
	}
	if s.eof {
		return true
	}
	line, nl, ok := s.nextRaw()
	if !ok {
		return true
	}
	s.hasPeek = true
	s.peekLine = line
	s.peekHasNL = nl
	return false
}

type sedRunner struct {
	ctx        context.Context
	hc         interp.HandlerContext
	quiet      bool
	extended   bool
	pat        string
	hold       string
	quit       bool
	lineNo     int
	curHasNL   bool
	labels     map[string]int
	wfiles     map[string]*os.File
	ranges     map[int]bool
	rangeSeen  map[int]bool
	rangeStart map[int]int
	subbed     bool
	out        io.Writer
	bw         *bufio.Writer
	writeErr   error
	pending    []pendingOut
	stream     *sedStream

	// GNU parity state
	err        error // runtime script error (e.g. no previous regexp)
	exitCode   int   // from qN/QN
	rangeEnd   bool  // the current command's range closed on this line
	lastRe     string
	lastReRe   *regexp.Regexp
	lastReCase bool
	lastReExt  bool
}

func newSedRunner(ctx context.Context, hc interp.HandlerContext, prog []*sedCmd, quiet, extended bool, out io.Writer) *sedRunner {
	r := &sedRunner{
		ctx:        ctx,
		hc:         hc,
		quiet:      quiet,
		extended:   extended,
		labels:     map[string]int{},
		wfiles:     map[string]*os.File{},
		ranges:     map[int]bool{},
		rangeSeen:  map[int]bool{},
		rangeStart: map[int]int{},
		out:        out,
	}
	for i, c := range prog {
		if c.name == ':' {
			r.labels[c.label] = i
		}
	}
	return r
}

// sedReFor resolves a command's regular expression, implementing GNU's
// "previous regular expression": an empty pattern reuses the last one.
// The regex becomes the new "last used" on success.
func sedReFor(r *sedRunner, pre *regexp.Regexp, pat string, nocase, ext bool) (*regexp.Regexp, bool) {
	if pat == "" {
		if r.lastRe == "" {
			r.err = fmt.Errorf("no previous regular expression")
			return nil, false
		}
		return r.lastReRe, true
	}
	re := pre
	if re == nil || nocase {
		var err error
		re, err = compileSedRe(pat, nocase, ext)
		if err != nil {
			r.err = err
			return nil, false
		}
	}
	r.lastRe, r.lastReRe, r.lastReCase, r.lastReExt = pat, re, nocase, ext
	return re, true
}

// checkSubRefs validates backreferences in a s/// replacement against
// the pattern's group count (GNU: "invalid reference \1 on `s' RHS").
func checkSubRefs(repl string, ngroups int) error {
	for i := 0; i+1 < len(repl); i++ {
		if repl[i] != '\\' {
			continue
		}
		n := repl[i+1]
		if n >= '1' && n <= '9' && int(n-'0') > ngroups {
			return fmt.Errorf("invalid reference \\%c on `s' command's RHS", n)
		}
		i++
	}
	return nil
}

func (r *sedRunner) needTrailingNL() bool {
	if r.curHasNL {
		return true
	}
	return r.stream != nil && !r.stream.isEOF()
}

func (r *sedRunner) maybeFlush() {
	if r.bw == nil || r.writeErr != nil {
		return
	}
	if r.bw.Buffered() >= 4096 || (r.stream != nil && r.stream.buffered() == 0 && r.bw.Buffered() > 0) {
		if err := r.bw.Flush(); err != nil {
			r.writeErr = err
			r.quit = true
		}
	}
}

func (r *sedRunner) runStream(prog []*sedCmd, stream *sedStream) {
	r.stream = stream
	if r.out == nil {
		r.out = r.hc.Stdout
	}
	ownBuf := false
	if bw, ok := r.out.(*bufio.Writer); ok {
		r.bw = bw
	} else if _, isBuilder := r.out.(*strings.Builder); !isBuilder {
		r.bw = bufio.NewWriterSize(r.out, 64*1024)
		r.out = r.bw
		ownBuf = true
	}
	if r.ranges == nil {
		r.ranges = map[int]bool{}
	}
	restart := false
	for !r.quit && r.writeErr == nil {
		if r.ctx != nil && r.ctx.Err() != nil {
			break
		}
		if !restart {
			line, hasNL, ok := stream.readLine()
			if !ok {
				break
			}
			r.lineNo++
			r.pat = line
			r.curHasNL = hasNL
		}
		restart = false
		r.subbed = false
		next := r.runCycle(prog)
		r.maybeFlush()
		if next == cycleRestart {
			restart = true
		}
	}
	if ownBuf && r.bw != nil {
		if err := r.bw.Flush(); err != nil && r.writeErr == nil {
			r.writeErr = err
		}
	}
}

const (
	cycleNext = iota
	cycleRestart
)

func (r *sedRunner) runCycle(prog []*sedCmd) int {
	steps := 0
	for pc := 0; pc < len(prog); {
		if r.quit || r.writeErr != nil {
			return cycleNext
		}
		steps++
		if steps&0xff == 0 && r.ctx != nil && r.ctx.Err() != nil {
			r.quit = true
			return cycleNext
		}
		c := prog[pc]
		pc++
		if c.name == '}' {
			continue
		}
		if !r.addrMatch(c) {
			if c.name == '{' {
				pc = c.endPC
			}
			continue
		}
		if c.name == '{' {
			continue
		}
		res := r.exec(c)
		switch res {
		case jumpToEnd:
			r.flushPending()
			return cycleNext
		case jumpRestart:
			return cycleRestart
		case jumpQuit:
			return cycleNext
		}
		if res >= 0 {
			pc = res
		}
	}
	if !r.quiet {
		r.writeLine(r.pat, r.needTrailingNL())
	}
	r.flushPending()
	return cycleNext
}

const (
	jumpToEnd   = -2
	jumpRestart = -3
	jumpQuit    = -5
)

func (r *sedRunner) addrMatch(c *sedCmd) bool {
	m := r.matchOnce(c.a1, c.a2, c.rangeID)
	if c.bang {
		return !m
	}
	return m
}

func (r *sedRunner) matchOnce(a1, a2 *sedAddr, id int) bool {
	r.rangeEnd = false
	if a1 == nil {
		r.rangeEnd = true
		return true
	}
	if a2 == nil {
		m := matchAddr(r, a1)
		r.rangeEnd = m
		return m
	}
	rangeEnds := func() bool {
		switch a2.kind {
		case 6: // addr1,+N
			return r.lineNo >= r.rangeStart[id]+a2.plus
		case 7: // addr1,~N
			return a2.tilde > 0 && r.lineNo%a2.tilde == 0
		default:
			return matchAddr(r, a2)
		}
	}
	if r.ranges[id] {
		if rangeEnds() {
			delete(r.ranges, id)
			r.rangeEnd = true
		}
		return true
	}
	if a1.zero && r.lineNo == 1 && !r.rangeSeen[id] {
		r.rangeSeen[id] = true
		r.ranges[id] = true
		r.rangeStart[id] = r.lineNo
		if rangeEnds() {
			delete(r.ranges, id)
			r.rangeEnd = true
		}
		return true
	}
	if matchAddr(r, a1) {
		r.rangeStart[id] = r.lineNo
		if rangeEnds() {
			r.rangeEnd = true
		} else {
			r.ranges[id] = true
		}
		return true
	}
	return false
}

func sameSedAddr(a, b *sedAddr) bool {
	return a.kind == b.kind && a.line == b.line
}

func matchAddr(r *sedRunner, a *sedAddr) bool {
	switch a.kind {
	case 1:
		if a.step > 0 {
			base := a.line
			if base == 0 {
				base = a.step
			}
			return r.lineNo >= base && (r.lineNo-base)%a.step == 0
		}
		return r.lineNo == a.line
	case 2:
		return r.stream != nil && r.stream.isEOF()
	case 3:
		re, ok := sedReFor(r, a.re, a.pat, a.reNocase, a.reExt)
		if !ok {
			return false
		}
		return re.MatchString(r.pat)
	}
	return true
}

func (r *sedRunner) exec(c *sedCmd) int {
	switch c.name {
	case ':':
		return -1
	case 'b':
		if c.label == "" {
			return jumpToEnd
		}
		if dest, ok := r.labels[c.label]; ok {
			return dest
		}
		return -1
	case 't', 'T':
		hit := r.subbed
		r.subbed = false
		if (c.name == 't') == hit {
			if c.label == "" {
				return jumpToEnd
			}
			if dest, ok := r.labels[c.label]; ok {
				return dest
			}
		}
		return -1
	case 's':
		r.doSub(c.sub)
		if r.err != nil {
			// Runtime script error: end the cycle without auto-print.
			return jumpQuit
		}
	case 'y':
		parts := strings.SplitN(c.arg, "\x00", 2)
		r.pat = trString(r.pat, parts[0], parts[1])
	case 'd':
		return jumpToEnd
	case 'D':
		if i := strings.IndexByte(r.pat, '\n'); i >= 0 {
			r.pat = r.pat[i+1:]
			return jumpRestart
		}
		return jumpToEnd
	case 'p':
		r.writeLine(r.pat, true)
		r.maybeFlush()
	case 'P':
		if i := strings.IndexByte(r.pat, '\n'); i >= 0 {
			r.writeLine(r.pat[:i], true)
		} else {
			r.writeLine(r.pat, true)
		}
		r.maybeFlush()
	case 'n':
		if !r.quiet {
			r.writeLine(r.pat, r.needTrailingNL())
		}
		r.flushPending()
		r.maybeFlush()
		if r.writeErr != nil {
			return jumpQuit
		}
		line, hasNL, ok := r.stream.readLine()
		if ok {
			r.lineNo++
			r.pat = line
			r.curHasNL = hasNL
			r.subbed = false
			return -1
		}
		r.pat = ""
		r.quit = true
		return jumpQuit
	case 'N':
		line, hasNL, ok := r.stream.readLine()
		if ok {
			r.pat += "\n" + line
			r.curHasNL = hasNL
			r.lineNo++
			break
		}
		// GNU: with no next input, N stops processing; the pattern
		// space is printed (unless -n) and sed exits.
		if !r.quiet {
			r.writeLine(r.pat, r.needTrailingNL())
		}
		r.flushPending()
		r.quit = true
		return jumpQuit
	case 'q':
		if !r.quiet {
			r.writeLine(r.pat, r.needTrailingNL())
		}
		r.flushPending()
		r.exitCode = sedExitCode(c.arg)
		r.quit = true
		return jumpQuit
	case 'Q':
		r.flushPending()
		r.exitCode = sedExitCode(c.arg)
		r.quit = true
		return jumpQuit
	case 'h':
		r.hold = r.pat
	case 'H':
		r.hold += "\n" + r.pat
	case 'g':
		r.pat = r.hold
	case 'G':
		r.pat += "\n" + r.hold
	case 'x':
		r.pat, r.hold = r.hold, r.pat
	case 'a':
		r.pending = append(r.pending, pendingOut{text: c.arg, after: true})
	case 'i':
		r.writeLine(c.arg, true)
		r.maybeFlush()
	case 'c':
		// GNU emits the text once, when the range ends; inside an open
		// range the pattern space is just deleted.
		if c.a2 != nil && !r.rangeEnd {
			return jumpToEnd
		}
		r.writeLine(c.arg, true)
		r.maybeFlush()
		return jumpToEnd
	case 'r':
		if data, err := fsReadFile(r.ctx, r.hc.Dir, "sed", c.arg); err == nil {
			r.pending = append(r.pending, pendingOut{data: string(data), after: true})
		}
	case 'w':
		if f := r.wfile(c.arg); f != nil {
			fmt.Fprintln(f, r.pat)
		}
	case 'l':
		r.writeLine(sedList(r.pat)+"$", true)
		r.maybeFlush()
		if r.quiet {
			return jumpToEnd
		}
	case '=':
		r.writeLine(strconv.Itoa(r.lineNo), true)
		r.maybeFlush()
	}
	return -1
}

type pendingOut struct {
	text  string
	data  string
	after bool
}

func (r *sedRunner) wfile(name string) *os.File {
	name = strings.TrimSpace(name)
	if f, ok := r.wfiles[name]; ok {
		return f
	}
	f, err := fsOpenFile(r.ctx, r.hc.Dir, "sed", name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		fmt.Fprintln(r.hc.Stderr, "sed:", err)
		if r.exitCode == 0 {
			r.exitCode = 1
		}
		r.quit = true
		return nil
	}
	r.wfiles[name] = f
	return f
}

func (r *sedRunner) doSub(sub *sedSub) {
	re, ok := sedReFor(r, sub.re, sub.rePat, sub.nocase, sub.reExt)
	if !ok {
		r.quit = true
		return
	}
	if err := checkSubRefs(sub.repl, re.NumSubexp()); err != nil {
		r.err = err
		r.quit = true
		return
	}
	if sub.plain && sub.nth == 0 && !sub.nocase && sub.rePat != "" && sedLiteralPat(sub.rePat) {
		// Fast path: literal pattern and literal replacement need no
		// regexp engine at all. No match means no substitution (and no
		// s///p/t side effects), like the engine path below.
		if !strings.Contains(r.pat, sub.rePat) {
			return
		}
		if sub.global {
			r.pat = strings.ReplaceAll(r.pat, sub.rePat, sub.repl)
		} else {
			r.pat = strings.Replace(r.pat, sub.rePat, sub.repl, 1)
		}
		r.afterSub(sub)
		return
	}
	if sub.plain && sub.nth == 0 {
		if sub.global {
			r.pat = re.ReplaceAllString(r.pat, sub.repl)
		} else {
			loc := re.FindStringIndex(r.pat)
			if loc == nil {
				return
			}
			r.pat = r.pat[:loc[0]] + sub.repl + r.pat[loc[1]:]
		}
		r.afterSub(sub)
		return
	}
	matches := re.FindAllStringSubmatchIndex(r.pat, -1)
	if len(matches) == 0 {
		return
	}
	if sub.nth > 0 {
		if sub.nth > len(matches) {
			return
		}
		// GNU sed: a numeric flag with 'g' replaces every match from the
		// nth onward; without 'g' it replaces only the nth match.
		if sub.global {
			var sb strings.Builder
			sb.Grow(len(r.pat) + 16)
			last := 0
			for _, m := range matches[sub.nth-1:] {
				sb.WriteString(r.pat[last:m[0]])
				sb.WriteString(expandSubMatch(r.pat, sub, m))
				last = m[1]
			}
			sb.WriteString(r.pat[last:])
			r.pat = sb.String()
			r.afterSub(sub)
			return
		}
		m := matches[sub.nth-1]
		r.pat = r.pat[:m[0]] + expandSubMatch(r.pat, sub, m) + r.pat[m[1]:]
		r.afterSub(sub)
		return
	}
	if sub.global {
		var sb strings.Builder
		sb.Grow(len(r.pat) + 16)
		last := 0
		for _, m := range matches {
			sb.WriteString(r.pat[last:m[0]])
			sb.WriteString(expandSubMatch(r.pat, sub, m))
			last = m[1]
		}
		sb.WriteString(r.pat[last:])
		r.pat = sb.String()
	} else {
		m := matches[0]
		r.pat = r.pat[:m[0]] + expandSubMatch(r.pat, sub, m) + r.pat[m[1]:]
	}
	r.afterSub(sub)
}

func (r *sedRunner) afterSub(sub *sedSub) {
	r.subbed = true
	if sub.print {
		r.writeLine(r.pat, true)
	}
	if sub.write != "" {
		if f := r.wfile(sub.write); f != nil {
			fmt.Fprintln(f, r.pat)
		}
	}
}

func isPlainRepl(repl string) bool {
	for i := 0; i < len(repl); i++ {
		if repl[i] == '\\' || repl[i] == '&' {
			return false
		}
	}
	return true
}

// sedLiteralPat reports whether a BRE/ERE pattern is a plain literal
// string (no metacharacters, escapes, or newlines).
func sedLiteralPat(pat string) bool {
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\', '.', '[', ']', '*', '^', '$', '(', ')', '{', '}', '+', '?', '|', '\n':
			return false
		}
	}
	return pat != ""
}

type sedFastOp struct {
	pat    []byte
	repl   []byte
	global bool
}

// sedTryFastStream executes unconditional literal substitution chains
// (`s/lit/repl/[g]`) directly over 64 KB []byte chunks using SIMD bytes.Index
// with zero regexp execution and zero per-line string allocations.
func sedTryFastStream(ctx context.Context, hc interp.HandlerContext, prog []*sedCmd, quiet bool, inputs []string) (bool, error) {
	if quiet || len(prog) == 0 || hc.Stdout == nil {
		return false, nil
	}
	ops := make([]sedFastOp, 0, len(prog))
	for _, c := range prog {
		if c.a1 != nil || c.a2 != nil || c.bang || c.name != 's' || c.sub == nil {
			return false, nil
		}
		sub := c.sub
		if sub.rePat == "" || !sedLiteralPat(sub.rePat) || sub.nocase || sub.print || sub.write != "" || sub.nth > 1 {
			return false, nil
		}
		var replStr string
		if sub.plain {
			replStr = sub.repl
		} else if !strings.ContainsRune(sub.repl, '\\') {
			// Only '&' metacharacters: since rePat is a pure literal, every
			// match equals rePat verbatim.
			replStr = strings.ReplaceAll(sub.repl, "&", sub.rePat)
		} else {
			return false, nil
		}
		if len(prog) > 1 && strings.ContainsRune(replStr, '\n') {
			return false, nil
		}
		ops = append(ops, sedFastOp{
			pat:    []byte(sub.rePat),
			repl:   []byte(replStr),
			global: sub.global,
		})
	}

	bw := getBufWriter64K(hc.Stdout)
	defer func() {
		_ = bw.Flush()
		putBufWriter64K(bw)
	}()

	handleWriteErr := func(werr error) (bool, error) {
		if isPipeClosed(werr) {
			return true, werr
		}
		fmt.Fprintln(hc.Stderr, "sed:", werr)
		return true, exitError{1}
	}

	openErr := false
	needNL := false

	// Mode 1: single unconditional literal substitution over 64 KB chunks.
	if len(ops) == 1 {
		op := ops[0]
		patB, replB, global := op.pat, op.repl, op.global
		sameLen := len(patB) == len(replB)

		bp := bufPool64K.Get().(*[]byte)
		buf := (*bp)[:64*1024]
		defer bufPool64K.Put(bp)

		var carry []byte
		processReader := func(rd io.Reader) error {
			if rd == nil {
				return nil
			}
			carry = carry[:0]
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				n, rerr := rd.Read(buf)
				var data []byte
				hadCarry := len(carry) > 0
				if hadCarry {
					if n > 0 {
						carry = append(carry, buf[:n]...)
					}
					data = carry
				} else {
					data = buf[:n]
				}
				if len(data) == 0 {
					if rerr != nil && rerr != io.EOF {
						return rerr
					}
					break
				}
				var tail []byte
				if data[len(data)-1] != '\n' {
					if j := bytes.LastIndexByte(data, '\n'); j >= 0 {
						tail = data[j+1:]
						data = data[:j+1]
					} else {
						tail = data
						data = nil
					}
				}
				if len(data) > 0 {
					if needNL {
						if werr := bw.WriteByte('\n'); werr != nil {
							return werr
						}
						needNL = false
					}
					if sameLen {
						pos := 0
						for pos < len(data) {
							idx := bytes.Index(data[pos:], patB)
							if idx < 0 {
								break
							}
							m := pos + idx
							copy(data[m:], replB)
							after := m + len(patB)
							if global {
								pos = after
							} else {
								nl := bytes.IndexByte(data[after:], '\n')
								if nl < 0 {
									break
								}
								pos = after + nl + 1
							}
						}
						if bw.Buffered() > 0 {
							if werr := bw.Flush(); werr != nil {
								return werr
							}
						}
						if _, werr := hc.Stdout.Write(data); werr != nil {
							return werr
						}
					} else {
						written := 0
						pos := 0
						for pos < len(data) {
							idx := bytes.Index(data[pos:], patB)
							if idx < 0 {
								break
							}
							m := pos + idx
							if m > written {
								if _, werr := bw.Write(data[written:m]); werr != nil {
									return werr
								}
							}
							if _, werr := bw.Write(replB); werr != nil {
								return werr
							}
							after := m + len(patB)
							written = after
							if global {
								pos = after
							} else {
								nl := bytes.IndexByte(data[after:], '\n')
								if nl < 0 {
									break
								}
								pos = after + nl + 1
							}
						}
						if written < len(data) {
							if _, werr := bw.Write(data[written:]); werr != nil {
								return werr
							}
						}
						if bw.Buffered() > 0 {
							if werr := bw.Flush(); werr != nil {
								return werr
							}
						}
					}
				}
				if len(tail) > 0 {
					if hadCarry {
						if len(tail) != len(carry) || &tail[0] != &carry[0] {
							nt := copy(carry[:cap(carry)], tail)
							carry = carry[:nt]
						}
					} else {
						carry = append(carry[:0], tail...)
					}
				} else {
					carry = carry[:0]
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					return rerr
				}
			}
			// Final unterminated line of this reader, if any.
			if len(carry) > 0 {
				if needNL {
					if werr := bw.WriteByte('\n'); werr != nil {
						return werr
					}
				}
				if sameLen {
					pos := 0
					for pos < len(carry) {
						idx := bytes.Index(carry[pos:], patB)
						if idx < 0 {
							break
						}
						m := pos + idx
						copy(carry[m:], replB)
						if !global {
							break
						}
						pos = m + len(patB)
					}
					if _, werr := bw.Write(carry); werr != nil {
						return werr
					}
				} else {
					written := 0
					pos := 0
					for pos < len(carry) {
						idx := bytes.Index(carry[pos:], patB)
						if idx < 0 {
							break
						}
						m := pos + idx
						if m > written {
							if _, werr := bw.Write(carry[written:m]); werr != nil {
								return werr
							}
						}
						if _, werr := bw.Write(replB); werr != nil {
							return werr
						}
						after := m + len(patB)
						written = after
						if !global {
							break
						}
						pos = after
					}
					if written < len(carry) {
						if _, werr := bw.Write(carry[written:]); werr != nil {
							return werr
						}
					}
				}
				if bw.Buffered() > 0 {
					if werr := bw.Flush(); werr != nil {
						return werr
					}
				}
				needNL = true
			}
			return nil
		}

		if len(inputs) == 0 {
			if err := processReader(hc.Stdin); err != nil {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				return handleWriteErr(err)
			}
		} else {
			for _, in := range inputs {
				if in == "-" || in == "/dev/stdin" {
					if err := processReader(hc.Stdin); err != nil {
						if ctx.Err() != nil {
							return true, ctx.Err()
						}
						return handleWriteErr(err)
					}
					continue
				}
				f, err := fsOpenInput(ctx, hc.Dir, "sed", in)
				if err != nil {
					fmt.Fprintln(hc.Stderr, "sed:", err)
					openErr = true
					continue
				}
				perr := processReader(f)
				f.Close()
				if perr != nil {
					if ctx.Err() != nil {
						return true, ctx.Err()
					}
					return handleWriteErr(perr)
				}
			}
		}
		if werr := bw.Flush(); werr != nil {
			return handleWriteErr(werr)
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
		if openErr {
			return true, exitError{1}
		}
		return true, nil
	}

	// Mode 2: multi-command unconditional literal substitution chain using
	// two reusable scratch byte buffers (zero heap allocations per line).
	var scrA, scrB []byte
	applyOps := func(line []byte) []byte {
		cur := line
		inA := false
		for _, op := range ops {
			idx := bytes.Index(cur, op.pat)
			if idx < 0 {
				continue
			}
			var dst []byte
			if inA {
				dst = scrB[:0]
			} else {
				dst = scrA[:0]
			}
			dst = append(dst, cur[:idx]...)
			dst = append(dst, op.repl...)
			pos := idx + len(op.pat)
			if op.global {
				for pos < len(cur) {
					j := bytes.Index(cur[pos:], op.pat)
					if j < 0 {
						break
					}
					dst = append(dst, cur[pos:pos+j]...)
					dst = append(dst, op.repl...)
					pos += j + len(op.pat)
				}
			}
			dst = append(dst, cur[pos:]...)
			if inA {
				scrB = dst
				inA = false
			} else {
				scrA = dst
				inA = true
			}
			cur = dst
		}
		return cur
	}

	processMulti := func(rd io.Reader) error {
		if rd == nil {
			return nil
		}
		var writeErr error
		err := streamLinesChunked(ctx, rd, func(line []byte, terminated bool) bool {
			if ctx.Err() != nil {
				return false
			}
			if needNL {
				if werr := bw.WriteByte('\n'); werr != nil {
					writeErr = werr
					return false
				}
				needNL = false
			}
			out := applyOps(line)
			if _, werr := bw.Write(out); werr != nil {
				writeErr = werr
				return false
			}
			if terminated {
				if werr := bw.WriteByte('\n'); werr != nil {
					writeErr = werr
					return false
				}
			} else {
				needNL = true
			}
			if bw.Available() < 4096 {
				if werr := bw.Flush(); werr != nil {
					writeErr = werr
					return false
				}
			}
			return true
		}, func() error {
			if bw.Buffered() > 0 {
				return bw.Flush()
			}
			return nil
		})
		if writeErr != nil {
			return writeErr
		}
		return err
	}

	if len(inputs) == 0 {
		if err := processMulti(hc.Stdin); err != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			return handleWriteErr(err)
		}
	} else {
		for _, in := range inputs {
			if in == "-" || in == "/dev/stdin" {
				if err := processMulti(hc.Stdin); err != nil {
					if ctx.Err() != nil {
						return true, ctx.Err()
					}
					return handleWriteErr(err)
				}
				continue
			}
			f, err := fsOpenInput(ctx, hc.Dir, "sed", in)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "sed:", err)
				openErr = true
				continue
			}
			perr := processMulti(f)
			f.Close()
			if perr != nil {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				return handleWriteErr(perr)
			}
		}
	}
	if werr := bw.Flush(); werr != nil {
		return handleWriteErr(werr)
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if openErr {
		return true, exitError{1}
	}
	return true, nil
}

func expandSubMatch(orig string, sub *sedSub, m []int) string {
	match := orig[m[0]:m[1]]
	group := func(k int) string {
		if k*2+1 < len(m) && m[k*2] >= 0 {
			return orig[m[k*2]:m[k*2+1]]
		}
		return ""
	}
	var sb strings.Builder
	upper, lower := false, false
	repl := sub.repl
	for i := 0; i < len(repl); {
		if repl[i] == '\\' && i+1 < len(repl) {
			i++
			r, sz := utf8.DecodeRuneInString(repl[i:])
			if r == utf8.RuneError && sz == 1 {
				sb.WriteByte(repl[i])
				i++
				continue
			}
			i += sz
			switch r {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case '\\':
				sb.WriteByte('\\')
			case '&':
				sb.WriteByte('&')
			case 'U':
				upper, lower = true, false
			case 'L':
				lower, upper = true, false
			case 'E':
				upper, lower = false, false
			default:
				if r >= '0' && r <= '9' {
					s := group(int(r - '0'))
					if upper {
						s = strings.ToUpper(s)
					} else if lower {
						s = strings.ToLower(s)
					}
					sb.WriteString(s)
				} else {
					s := string(r)
					if upper {
						s = strings.ToUpper(s)
					} else if lower {
						s = strings.ToLower(s)
					}
					sb.WriteString(s)
				}
			}
			continue
		}
		if repl[i] == '&' {
			i++
			s := match
			if upper {
				s = strings.ToUpper(s)
			} else if lower {
				s = strings.ToLower(s)
			}
			sb.WriteString(s)
			continue
		}
		r, sz := utf8.DecodeRuneInString(repl[i:])
		if r == utf8.RuneError && sz == 1 {
			sb.WriteByte(repl[i])
			i++
			continue
		}
		i += sz
		s := string(r)
		if upper {
			s = strings.ToUpper(s)
		} else if lower {
			s = strings.ToLower(s)
		}
		sb.WriteString(s)
	}
	return sb.String()
}

func trString(s, from, to string) string {
	fr := []rune(unescapeSedRepl(from))
	toR := []rune(unescapeSedRepl(to))
	if len(toR) == 0 {
		return s
	}
	var table [256]rune
	for i := range table {
		table[i] = rune(i)
	}
	var hiMap map[rune]rune
	for i, r := range fr {
		c := toR[len(toR)-1]
		if i < len(toR) {
			c = toR[i]
		}
		if r >= 0 && r < 256 {
			table[r] = c
		} else {
			if hiMap == nil {
				hiMap = make(map[rune]rune)
			}
			hiMap[r] = c
		}
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && sz == 1 {
			sb.WriteByte(s[i])
			i++
			continue
		}
		i += sz
		if r >= 0 && r < 256 {
			sb.WriteRune(table[r])
		} else if hiMap != nil {
			if mapped, ok := hiMap[r]; ok {
				sb.WriteRune(mapped)
			} else {
				sb.WriteRune(r)
			}
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func unescapeSedRepl(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('	')
			default:
				sb.WriteByte(s[i])
			}
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

func sedList(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '\t':
			sb.WriteString("\\\\t")
		case '\n':
			sb.WriteString("\\\\n")
		default:
			if r < 32 || r == 127 {
				fmt.Fprintf(&sb, "\\\\%03o", r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	return sb.String()
}

// writeLine writes a line to the output without fmt overhead (the hot
// path of every sed script).
func (r *sedRunner) writeLine(s string, nl bool) {
	if r.writeErr != nil {
		return
	}
	if _, err := io.WriteString(r.out, s); err != nil {
		r.writeErr = err
		r.quit = true
		return
	}
	if nl {
		if _, err := io.WriteString(r.out, "\n"); err != nil {
			r.writeErr = err
			r.quit = true
		}
	}
}

func (r *sedRunner) flushPending() {
	for _, p := range r.pending {
		if r.writeErr != nil {
			break
		}
		if p.data != "" {
			r.writeLine(p.data, !strings.HasSuffix(p.data, "\n"))
		} else {
			r.writeLine(p.text, true)
		}
	}
	r.pending = nil
}

func sedInplace(ctx context.Context, hc interp.HandlerContext, prog []*sedCmd, quiet, extended bool, inputs []string, suffix string) error {
	if len(inputs) == 0 {
		fmt.Fprintln(hc.Stderr, "sed: no input files")
		return exitError{1}
	}
	code := 0
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		lfi, err := fsLstat(ctx, hc.Dir, "sed", in)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "sed:", err)
			code = 1
			continue
		}
		inFile, err := fsOpen(ctx, hc.Dir, "sed", in)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "sed:", err)
			code = 1
			continue
		}
		perm := os.FileMode(0o644)
		if fi, serr := inFile.Stat(); serr == nil {
			perm = fi.Mode().Perm()
		} else {
			perm = lfi.Mode().Perm()
		}
		data, err := io.ReadAll(inFile)
		inFile.Close()
		if err != nil {
			fmt.Fprintln(hc.Stderr, "sed:", err)
			code = 1
			continue
		}
		var out strings.Builder
		r := newSedRunner(ctx, hc, prog, quiet, extended, &out)
		stream := &sedStream{ctx: ctx, hc: hc, singleR: bytes.NewReader(data)}
		r.runStream(prog, stream)
		stream.closeCur()
		for _, f := range r.wfiles {
			f.Close()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Never overwrite or truncate the target file on runtime script error!
		if r.err != nil {
			_ = sedFinish(hc, r)
			code = 1
			continue
		}
		if r.exitCode != 0 && code == 0 {
			code = r.exitCode
		}
		if _, err := checkFS(ctx, hc.Dir, "sed", in, OpWrite); err != nil {
			fmt.Fprintln(hc.Stderr, "sed:", err)
			code = 1
			continue
		}
		if suffix != "" {
			if err := fsWriteFile(ctx, hc.Dir, "sed", in+suffix, data, perm); err != nil {
				fmt.Fprintln(hc.Stderr, "sed:", err)
				code = 1
				continue
			}
		}
		outBytes := []byte(out.String())
		targetDir := filepath.Dir(in)
		tf, err := fsCreateTemp(ctx, hc.Dir, "sed", targetDir, ".sed-*")
		if err != nil {
			fmt.Fprintln(hc.Stderr, "sed:", err)
			code = 1
			continue
		}
		tmpPath := tf.Name()
		_, werr := tf.Write(outBytes)
		cerr := tf.Close()
		if werr != nil || cerr != nil {
			_ = fsRemove(ctx, hc.Dir, "sed", tmpPath)
			if werr != nil {
				fmt.Fprintln(hc.Stderr, "sed:", werr)
			} else {
				fmt.Fprintln(hc.Stderr, "sed:", cerr)
			}
			code = 1
			continue
		}
		_ = fsChmod(ctx, hc.Dir, "sed", tmpPath, perm)
		if rerr := fsRename(ctx, hc.Dir, "sed", tmpPath, in); rerr != nil {
			_ = fsRemove(ctx, hc.Dir, "sed", tmpPath)
			if lfi.Mode()&os.ModeSymlink == 0 && !errors.Is(rerr, os.ErrPermission) {
				if ferr := fsWriteFile(ctx, hc.Dir, "sed", in, outBytes, perm); ferr == nil {
					continue
				}
			}
			fmt.Fprintln(hc.Stderr, "sed:", rerr)
			code = 1
		}
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

// dropSedPrefix strips our internal "unknown command " prefix so the call
// site can emit the full GNU diagnostic.
func dropSedPrefix(err error) string {
	msg := err.Error()
	if strings.HasPrefix(msg, "unknown command ") {
		return msg[len("unknown command "):]
	}
	return msg
}
