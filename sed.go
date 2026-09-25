package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

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
	blk     []*sedCmd
	label   string
	rangeID int
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

var sedRangeSeq int

func cmdSed(ctx context.Context, hc interp.HandlerContext, args []string) error {
	_ = ctx
	inplace := ""
	inplaceOn := false
	var cleanArgs []string
	for _, a := range args[1:] {
		if len(a) > 2 && a[0] == '-' && a[1] == 'i' && a[2] != '=' {
			inplaceOn = true
			inplace = a[2:]
			continue
		}
		if a == "-i" {
			inplaceOn = true
			continue
		}
		cleanArgs = append(cleanArgs, a)
	}
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
		data, err := os.ReadFile(resolvePath(hc, f))
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
	inplaceFlag := inplaceOn || hasIFlag(args)
	if inplaceFlag {
		return sedInplace(hc, prog, *quiet, *extended, inputs, inplace)
	}
	r := &sedRunner{hc: hc, quiet: *quiet, extended: *extended,
		labels: map[string]int{}, wfiles: map[string]*os.File{},
		ranges: map[int]bool{}, rangeSeen: map[int]bool{}, rangeStart: map[int]int{}}
	for i, c := range prog {
		if c.name == ':' {
			r.labels[c.label] = i
		}
	}
	if len(inputs) == 0 {
		r.runStream(prog, hc.Stdin)
	} else {
		for _, in := range inputs {
			if in == "-" || in == "/dev/stdin" {
				r.runStream(prog, hc.Stdin)
				continue
			}
			f, err := os.Open(resolvePath(hc, in))
			if err != nil {
				fmt.Fprintln(hc.Stderr, "sed:", err)
				return exitError{1}
			}
			r.runStream(prog, f)
			f.Close()
			if r.quit {
				break
			}
		}
	}
	for _, f := range r.wfiles {
		f.Close()
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

func hasIFlag(args []string) bool {
	for _, a := range args[1:] {
		if len(a) >= 2 && a[0] == '-' && a[1] == 'i' {
			return true
		}
	}
	return false
}

func splitSedArgs(args []string) []string {
	var out []string
	for _, a := range args {
		if len(a) > 2 && a[0] == '-' && a[1] != '-' {
			switch a[1] {
			case 'i':
				out = append(out, "-i", a[2:])
				continue
			case 'e', 'f':
				out = append(out, "-"+string(a[1]), a[2:])
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

func isSedProgram(a, full string) bool {
	if _, err := os.Stat(full); err != nil {
		return true
	}
	for _, c := range a {
		if strings.ContainsRune("spdqyngGNPHhlawrbt:{}=!#", c) {
			return true
		}
	}
	return false
}

func parseSed(src string, extended bool) ([]*sedCmd, error) {
	var lines []string
	var cur strings.Builder
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimRight(raw, "\r")
		if cur.Len() > 0 {
			cur.WriteString("\n")
		}
		cur.WriteString(line)
		if strings.HasSuffix(line, "\\") && !strings.HasSuffix(line, "\\\\") {
			cur.WriteString("\n")
			continue
		}
		lines = append(lines, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	var prog []*sedCmd
	for _, line := range lines {
		for _, chunk := range splitCommands(line) {
			cmds, err := parseSedLine(chunk, extended)
			if err != nil {
				return nil, err
			}
			prog = append(prog, cmds...)
		}
	}
	return prog, nil
}

func splitCommands(line string) []string {
	var parts []string
	start := 0
	i := 0
	for i < len(line) {
		ch := line[i]
		if ch == 'a' || ch == 'i' || ch == 'c' {
			j := i + 1
			for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
				j++
			}
			if j < len(line) && line[j] == '\\' {
				break
			}
		}
		if ch == 's' || ch == 'y' {
			if i+1 < len(line) {
				d := line[i+1]
				j := i + 2
				count := 0
				need := 2
				for j < len(line) && count < need {
					if line[j] == '\\' {
						j += 2
						continue
					}
					if line[j] == d {
						count++
					}
					j++
				}
				i = j
				continue
			}
		}
		if ch == '{' {
			depth := 1
			j := i + 1
			for j < len(line) && depth > 0 {
				if line[j] == '{' {
					depth++
				} else if line[j] == '}' {
					depth--
				}
				j++
			}
			i = j
			continue
		}
		if ch == ';' {
			parts = append(parts, line[start:i])
			start = i + 1
		}
		i++
	}
	parts = append(parts, line[start:])
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseSedLine(line string, extended bool) ([]*sedCmd, error) {
	s := strings.TrimLeft(line, " \t")
	if s == "" || strings.HasPrefix(s, "#") {
		return nil, nil
	}
	var a1, a2 *sedAddr
	var err error
	s, a1, err = parseAddr(s, extended)
	if err != nil {
		return nil, err
	}
	if a1 != nil && strings.HasPrefix(s, ",") {
		s = s[1:]
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
	if s == "" {
		return nil, fmt.Errorf("missing command")
	}
	if s[0] == '{' {
		inner := strings.TrimSpace(s[1:])
		var blk []*sedCmd
		if strings.HasSuffix(inner, "}") {
			inner = strings.TrimSpace(inner[:len(inner)-1])
			for _, part := range splitSedBlock(inner) {
				sub, err := parseSedLine(part, extended)
				if err != nil {
					return nil, err
				}
				blk = append(blk, sub...)
			}
		}
		sedRangeSeq++
		return []*sedCmd{{a1: a1, a2: a2, bang: bang, name: '{', blk: blk, rangeID: sedRangeSeq}}, nil
	}
	name := s[0]
	rest := ""
	if len(s) > 1 {
		rest = s[1:]
	}
	sedRangeSeq++
	c := &sedCmd{a1: a1, a2: a2, bang: bang, name: name, rangeID: sedRangeSeq}
	switch name {
	case 's':
		sub, err := parseSub(rest, extended)
		if err != nil {
			return nil, err
		}
		c.sub = sub
	case 'y':
		if len(rest) < 3 {
			return nil, fmt.Errorf("y: missing delimiter")
		}
		d := rest[0]
		parts := splitDelim(rest[1:], d, 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("y: need 2 sets")
		}
		c.arg = parts[0] + "\x00" + parts[1]
	case 'a', 'i', 'c':
		t := strings.TrimLeft(rest, " \t")
		t = strings.TrimPrefix(t, "\\")
		c.arg = unescapeSedText(t)
	case 'r', 'w':
		c.arg = strings.TrimSpace(rest)
	case ':', 'b', 't', 'T':
		c.label = strings.TrimSpace(rest)
		if (name == 'b' || name == 't' || name == 'T') && c.label != "" {
			c.label = strings.Fields(c.label)[0]
		}
	case 'q', 'Q':
		c.arg = strings.TrimSpace(rest)
	case 'd', 'D', 'p', 'P', 'n', 'N', 'h', 'H', 'g', 'G', 'x', 'l', '=', '}':
	default:
		return nil, fmt.Errorf("unknown command: `%c'", name)
	}
	return []*sedCmd{c}, nil
}

func splitSedBlock(s string) []string {
	var parts []string
	depth := 0
	start := 0
	var delim byte
	inCmd := byte(0)
	seenDelim := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if delim != 0 {
			if ch == delim {
				delim = 0
			} else if ch == '\\' {
				i++
			}
			continue
		}
		if inCmd != 0 && !seenDelim {
			delim = ch
			seenDelim = true
			inCmd = 0
			continue
		}
		if ch == ';' && depth == 0 && delim == 0 {
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		} else if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
		} else if (ch == 's' || ch == 'y') && delim == 0 {
			inCmd = ch
			seenDelim = false
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		parts = append(parts, t)
	}
	return parts
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

func parseSub(s string, extended bool) (*sedSub, error) {
	if s == "" {
		return nil, fmt.Errorf("unterminated `s' command")
	}
	d := s[0]
	rest := s[1:]
	pat, rest2, ok := takeUntil(rest, d)
	if !ok {
		return nil, fmt.Errorf("unterminated `s' command")
	}
	repl, rest3, ok := takeUntil(rest2, d)
	if !ok {
		return nil, fmt.Errorf("unterminated `s' command")
	}
	sub := &sedSub{rePat: pat, reExt: extended}
	if pat != "" {
		re, err := compileSedRe(pat, false, extended)
		if err != nil {
			return nil, err
		}
		sub.re = re
		sub.ngroups = re.NumSubexp()
	}
	sub.repl = repl
	sub.plain = isPlainRepl(repl)
	flags := strings.TrimSpace(rest3)
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		switch f {
		case 'g':
			sub.global = true
		case 'p':
			sub.print = true
		case 'i', 'I':
			sub.nocase = true
			if pat != "" {
				re2, err := compileSedRe(pat, true, extended)
				if err != nil {
					return nil, err
				}
				sub.re = re2
			}
		case 'w':
			sub.write = strings.TrimSpace(flags[i+1:])
			i = len(flags)
		default:
			if f >= '0' && f <= '9' {
				sub.nth = sub.nth*10 + int(f-'0')
			}
		}
	}
	if pat != "" {
		if err := checkSubRefs(repl, sub.ngroups); err != nil {
			return nil, err
		}
	}
	return sub, nil
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
	s = strings.ReplaceAll(s, "\n", "\n")
	s = strings.ReplaceAll(s, "\\t", "\t")
	s = strings.ReplaceAll(s, "\\\\", "\\")
	return s
}

type sedRunner struct {
	hc         interp.HandlerContext
	quiet      bool
	extended   bool
	pat        string
	hold       string
	quit       bool
	lineNo     int
	lastLine   bool
	labels     map[string]int
	wfiles     map[string]*os.File
	ranges     map[int]bool
	rangeSeen  map[int]bool
	rangeStart map[int]int
	subbed     bool
	out        io.Writer
	pending    []pendingOut
	trailNL    bool

	// GNU parity state
	err        error // runtime script error (e.g. no previous regexp)
	exitCode   int   // from qN/QN
	rangeEnd   bool  // the current command's range closed on this line
	lastRe     string
	lastReRe   *regexp.Regexp
	lastReCase bool
	lastReExt  bool
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

func (r *sedRunner) runStream(prog []*sedCmd, rd io.Reader) {
	data, _ := io.ReadAll(rd)
	trailingNL := len(data) > 0 && data[len(data)-1] == '\n'
	var lines []string
	if len(data) > 0 {
		text := string(data)
		if trailingNL {
			text = text[:len(text)-1]
		}
		lines = strings.Split(text, "\n")
	}
	if r.out == nil {
		r.out = r.hc.Stdout
	}
	ownBuf := false
	if _, ok := r.out.(*bufio.Writer); !ok {
		r.out = bufio.NewWriterSize(r.out, 64*1024)
		ownBuf = true
	}
	r.trailNL = trailingNL
	if r.ranges == nil {
		r.ranges = map[int]bool{}
	}
	i := 0
	for i < len(lines) {
		if r.quit {
			break
		}
		r.lineNo++
		r.lastLine = i == len(lines)-1
		r.pat = lines[i]
		r.subbed = false
		next := r.runCycle(prog, 0, &i, lines)
		if next == cycleNext {
			i++
		} else if next == cycleRestart {
			lines[i] = r.pat
		} else if next == cycleReadNext {
			i++
		}
	}
	if ownBuf {
		if bw, ok := r.out.(*bufio.Writer); ok {
			bw.Flush()
		}
	}
}

const (
	cycleNext = iota
	cycleRestart
	cycleReadNext
)

func (r *sedRunner) runCycle(prog []*sedCmd, pc int, idx *int, lines []string) int {
	for pc < len(prog) {
		if r.quit {
			return cycleNext
		}
		c := prog[pc]
		pc++
		if !r.addrMatch(c) {
			continue
		}
		res := r.exec(c, prog, pc, idx, lines)
		switch res {
		case jumpToEnd:
			return cycleNext
		case jumpRestart:
			return cycleRestart
		case jumpReadNext:
			return cycleReadNext
		case jumpQuit:
			return cycleNext
		}
		if res >= 0 {
			pc = res
		}
	}
	if !r.quiet {
		r.writeLine(r.pat, !(r.lastLine && !r.trailNL))
	}
	r.flushPending()
	return cycleNext
}

// runBlock runs a {…} block as part of the enclosing cycle. Unlike
// runCycle it never auto-prints; a cycle-ending command (d, D, q, Q, b
// without label, …) ends the whole cycle and is propagated to the
// caller so the pattern space is not printed by mistake.
func (r *sedRunner) runBlock(prog []*sedCmd, idx *int, lines []string) int {
	for pc := 0; pc < len(prog); {
		if r.quit {
			return jumpQuit
		}
		c := prog[pc]
		pc++
		if !r.addrMatch(c) {
			continue
		}
		res := r.exec(c, prog, pc, idx, lines)
		switch res {
		case jumpToEnd, jumpRestart, jumpQuit, jumpReadNext:
			return res
		}
		if res >= 0 {
			pc = res
		}
	}
	return -1
}

const (
	jumpToEnd    = -2
	jumpRestart  = -3
	jumpReadNext = -4
	jumpQuit     = -5
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
		return r.lastLine
	case 3:
		re, ok := sedReFor(r, a.re, a.pat, a.reNocase, a.reExt)
		if !ok {
			return false
		}
		return re.MatchString(r.pat)
	}
	return true
}

func (r *sedRunner) exec(c *sedCmd, prog []*sedCmd, pc int, idx *int, lines []string) int {
	switch c.name {
	case '{':
		return r.runBlock(c.blk, idx, lines)
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
	case 'P':
		if i := strings.IndexByte(r.pat, '\n'); i >= 0 {
			r.writeLine(r.pat[:i], true)
		} else {
			r.writeLine(r.pat, true)
		}
	case 'n':
		if !r.quiet {
			r.writeLine(r.pat, true)
		}
		if *idx+1 < len(lines) {
			*idx++
			r.lineNo++
			r.lastLine = *idx == len(lines)-1
			r.pat = lines[*idx]
			r.subbed = false
			return -1
		}
		r.pat = ""
		return jumpToEnd
	case 'N':
		if *idx+1 < len(lines) {
			r.pat += "\n" + lines[*idx+1]
			*idx++
			r.lineNo++
			r.lastLine = *idx == len(lines)-1
			break
		}
		// GNU: with no next input, N stops processing; the pattern
		// space is printed (unless -n) and sed exits.
		if !r.quiet {
			r.writeLine(r.pat, !(r.lastLine && !r.trailNL))
		}
		r.flushPending()
		r.quit = true
		return jumpQuit
	case 'q':
		if !r.quiet {
			r.writeLine(r.pat, true)
		}
		r.exitCode = sedExitCode(c.arg)
		r.quit = true
		return jumpQuit
	case 'Q':
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
		fmt.Fprintln(r.out, c.arg)
	case 'c':
		// GNU emits the text once, when the range ends; inside an open
		// range the pattern space is just deleted.
		if c.a2 != nil && !r.rangeEnd {
			return jumpToEnd
		}
		fmt.Fprintln(r.out, c.arg)
		return jumpToEnd
	case 'r':
		if data, err := os.ReadFile(resolvePath(r.hc, c.arg)); err == nil {
			r.pending = append(r.pending, pendingOut{data: string(data), after: true})
		}
	case 'w':
		if f := r.wfile(c.arg); f != nil {
			fmt.Fprintln(f, r.pat)
		}
	case 'l':
		fmt.Fprintln(r.out, sedList(r.pat)+"$")
		if r.quiet {
			return jumpToEnd
		}
	case '=':
		fmt.Fprintln(r.out, r.lineNo)
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
	f, err := os.Create(resolvePath(r.hc, name))
	if err != nil {
		fmt.Fprintln(r.hc.Stderr, "sed:", err)
		return nil
	}
	r.wfiles[name] = f
	return f
}

func resolvePath(hc interp.HandlerContext, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if hc.Dir == "" {
		return p
	}
	return filepath.Join(hc.Dir, p)
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
				if m[0] == m[1] {
					continue
				}
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
			if m[0] == m[1] {
				continue
			}
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
// string (no metacharacters and no escapes).
func sedLiteralPat(pat string) bool {
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\', '.', '[', ']', '*', '^', '$', '(', ')', '{', '}', '+', '?', '|':
			return false
		}
	}
	return pat != ""
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
	for i := 0; i < len(repl); i++ {
		c := repl[i]
		if c == '\\' && i+1 < len(repl) {
			i++
			n := repl[i]
			switch n {
			case 'n':
				sb.WriteString("\n")
			case 't':
				sb.WriteString("\\t")
			case '\\':
				sb.WriteByte('\\')
			case '&':
				sb.WriteString("&")
			case 'U':
				upper, lower = true, false
			case 'L':
				lower, upper = true, false
			case 'E':
				upper, lower = false, false
			default:
				if n >= '0' && n <= '9' {
					s := group(int(n - '0'))
					if upper {
						s = strings.ToUpper(s)
					} else if lower {
						s = strings.ToLower(s)
					}
					sb.WriteString(s)
				} else {
					sb.WriteByte(n)
				}
			}
			continue
		}
		if c == '&' {
			s := match
			if upper {
				s = strings.ToUpper(s)
			} else if lower {
				s = strings.ToLower(s)
			}
			sb.WriteString(s)
			continue
		}
		s := string(c)
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
	for i, r := range fr {
		c := toR[len(toR)-1]
		if i < len(toR) {
			c = toR[i]
		}
		if r < 256 {
			table[r] = c
		}
	}
	var sb strings.Builder
	for _, r := range s {
		if r < 256 {
			sb.WriteRune(table[r])
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
	io.WriteString(r.out, s)
	if nl {
		io.WriteString(r.out, "\n")
	}
}

func (r *sedRunner) flushPending() {
	for _, p := range r.pending {
		if p.data != "" {
			fmt.Fprint(r.out, p.data)
			if !strings.HasSuffix(p.data, "\n") {
				fmt.Fprintln(r.out)
			}
		} else {
			fmt.Fprintln(r.out, p.text)
		}
	}
	r.pending = nil
}

func sedInplace(hc interp.HandlerContext, prog []*sedCmd, quiet, extended bool, inputs []string, suffix string) error {
	if len(inputs) == 0 {
		fmt.Fprintln(hc.Stderr, "sed: no input files")
		return exitError{1}
	}
	code := 0
	for _, in := range inputs {
		full := resolvePath(hc, in)
		data, err := os.ReadFile(full)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "sed:", err)
			code = 1
			continue
		}
		var out strings.Builder
		r := &sedRunner{hc: hc, quiet: quiet, extended: extended,
			labels: map[string]int{}, wfiles: map[string]*os.File{},
			ranges: map[int]bool{}, rangeSeen: map[int]bool{}, rangeStart: map[int]int{}, out: &out}
		for i, c := range prog {
			if c.name == ':' {
				r.labels[c.label] = i
			}
		}
		lines := strings.Split(string(data), "\n")
		hasNL := false
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
			hasNL = true
		}
		i := 0
		for i < len(lines) {
			if r.quit {
				break
			}
			r.lineNo++
			r.lastLine = i == len(lines)-1
			r.pat = lines[i]
			r.subbed = false
			next := r.runCycle(prog, 0, &i, lines)
			if next == cycleNext || next == cycleReadNext {
				i++
			} else if next == cycleRestart {
				lines[i] = r.pat
			}
		}
		for _, f := range r.wfiles {
			f.Close()
		}
		if err := sedFinish(hc, r); err != nil {
			if ee, ok := err.(exitError); ok && code == 0 {
				code = ee.code
			}
		}
		if suffix != "" {
			os.WriteFile(full+suffix, data, 0o644)
		}
		content := out.String()
		if hasNL && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			fmt.Fprintln(hc.Stderr, "sed:", err)
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
