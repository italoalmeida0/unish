package unish

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

type awkKind uint8

const (
	awkUninit awkKind = iota
	awkNum
	awkStr
	awkStrNum
)

type awkVal struct {
	kind awkKind
	num  float64
	str  string
}

func awkNumVal(n float64) awkVal {
	if n == 0 {
		n = 0 // normalize -0.0 to +0.0
	}
	return awkVal{kind: awkNum, num: n}
}

func awkBoolVal(b bool) awkVal {
	if b {
		return awkVal{kind: awkNum, num: 1}
	}
	return awkVal{kind: awkNum, num: 0}
}

func awkStrVal(s string) awkVal {
	return awkVal{kind: awkStr, str: s}
}

// awkStrNumVal creates a strnum value if the entire string (trimmed of ASCII
// whitespace) is a valid decimal/float number, or a plain string otherwise.
func awkStrNumVal(s string) awkVal {
	if n, ok := parseAwkFullNum(s); ok {
		return awkVal{kind: awkStrNum, num: n, str: s}
	}
	return awkVal{kind: awkStr, str: s}
}

func isAwkSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// parseAwkFullNum checks whether s is a valid AWK numeric string (entire string
// after trimming ASCII whitespace, excluding NaN/Inf).
func parseAwkFullNum(s string) (float64, bool) {
	i := 0
	for i < len(s) && isAwkSpace(s[i]) {
		i++
	}
	j := len(s)
	for j > i && isAwkSpace(s[j-1]) {
		j--
	}
	if i >= j {
		return 0, false
	}
	sub := s[i:j]
	// Fast path for common unsigned/signed integers up to 15 digits
	if len(sub) <= 15 {
		p0 := 0
		neg := false
		if sub[0] == '-' {
			neg = true
			p0 = 1
		} else if sub[0] == '+' {
			p0 = 1
		}
		if p0 < len(sub) {
			var u uint64
			allDigits := true
			for k := p0; k < len(sub); k++ {
				c := sub[k]
				if c < '0' || c > '9' {
					allDigits = false
					break
				}
				u = u*10 + uint64(c-'0')
			}
			if allDigits {
				f := float64(u)
				if neg && f != 0 {
					f = -f
				}
				return f, true
			}
		}
	}
	p := 0
	if sub[p] == '+' || sub[p] == '-' {
		p++
	}
	if p >= len(sub) {
		return 0, false
	}
	digits := 0
	for p < len(sub) && sub[p] >= '0' && sub[p] <= '9' {
		p++
		digits++
	}
	if p < len(sub) && sub[p] == '.' {
		p++
		for p < len(sub) && sub[p] >= '0' && sub[p] <= '9' {
			p++
			digits++
		}
	}
	if digits == 0 {
		return 0, false
	}
	if p < len(sub) && (sub[p] == 'e' || sub[p] == 'E') {
		p++
		if p < len(sub) && (sub[p] == '+' || sub[p] == '-') {
			p++
		}
		expDigits := 0
		for p < len(sub) && sub[p] >= '0' && sub[p] <= '9' {
			p++
			expDigits++
		}
		if expDigits == 0 {
			return 0, false
		}
	}
	if p != len(sub) {
		return 0, false
	}
	f, err := strconv.ParseFloat(sub, 64)
	if err != nil && !strings.Contains(err.Error(), "value out of range") {
		return 0, false
	}
	if f == 0 {
		f = 0
	}
	return f, true
}

// parseAwkPrefixNum converts an AWK string to float64 by scanning its leading
// numeric prefix (returning 0 if none).
func parseAwkPrefixNum(s string) float64 {
	i := 0
	for i < len(s) && isAwkSpace(s[i]) {
		i++
	}
	if i >= len(s) {
		return 0
	}
	start := i
	if s[i] == '+' || s[i] == '-' {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return 0
	}
	numEnd := i
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		expDigits := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			expDigits++
		}
		if expDigits > 0 {
			numEnd = i
		}
	}
	f, _ := strconv.ParseFloat(s[start:numEnd], 64)
	if f == 0 {
		f = 0
	}
	return f
}

type awkSlotKind uint8

const (
	slotUntyped awkSlotKind = iota
	slotScalar
	slotArray
)

type awkSlot struct {
	kind   awkSlotKind
	val    awkVal
	arr    map[string]awkVal
	origin *awkSlot // for untyped function parameter promoted to array
}

func (s *awkSlot) ensureArray(name string) (map[string]awkVal, error) {
	if s.kind == slotScalar {
		return nil, fmt.Errorf("attempt to use scalar %q as an array", name)
	}
	if s.kind == slotUntyped {
		s.kind = slotArray
		s.arr = make(map[string]awkVal)
		if s.origin != nil && s.origin.kind == slotUntyped {
			s.origin.kind = slotArray
			s.origin.arr = s.arr
		}
	}
	return s.arr, nil
}

func (s *awkSlot) setScalar(name string, v awkVal) error {
	if s.kind == slotArray {
		return fmt.Errorf("attempt to use array %q in a scalar context", name)
	}
	s.kind = slotScalar
	s.origin = nil
	s.val = v
	return nil
}

type awkCtrlKind uint8

const (
	ctrlNone awkCtrlKind = iota
	ctrlBreak
	ctrlContinue
	ctrlNext
	ctrlNextfile
	ctrlReturn
	ctrlExit
	ctrlPipeClosed
	ctrlError
)

type awkOutStream struct {
	bw     *bufio.Writer
	closer io.Closer // nil for stdout/stderr
}

type awkRecordReader struct {
	br     *bufio.Reader
	closer io.Closer
	eof    bool
}

type awkOutPipe struct {
	bw   *bufio.Writer
	pw   *io.PipeWriter
	done chan int
}

type awkInPipe struct {
	rr     *awkRecordReader
	pr     *io.PipeReader
	cancel context.CancelFunc
	done   chan int
}

type awkSyncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (sw *awkSyncWriter) Write(p []byte) (int, error) {
	if sw == nil || sw.w == nil {
		return len(p), nil
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

type awkVM struct {
	ctx  context.Context
	hc   interp.HandlerContext
	prog *awkProgram

	// Buffered stdout & managed file/pipe streams
	stdoutRaw io.Writer
	stderrRaw io.Writer
	stdout    *bufio.Writer
	outFiles  map[string]*awkOutStream
	inFiles   map[string]*awkRecordReader
	outPipes  map[string]*awkOutPipe
	inPipes   map[string]*awkInPipe

	// Built-in scalar variables (fast direct fields)
	fs       string
	fsRe     *regexp.Regexp
	fsReSrc  string
	rs       string
	rsRe     *regexp.Regexp
	rsReSrc  string
	ofs      string
	ors      string
	subsep   string
	ofmt     string
	convfmt  string
	nr       int64
	fnr      int64
	filename string
	rstart   int64
	rlength  int64
	argc     int
	rt       string

	// Current record & lazy fields
	line         string
	lineIsStrNum bool
	fields       []awkVal
	fieldsSplit  bool

	// Current input reader (for bare `getline`)
	curReader      *awkRecordReader
	hadFileOperand bool

	// Variables & arrays (ENVIRON and ARGV live in vars too)
	vars map[string]*awkSlot

	// Range pattern state per rule
	rangeActive []bool

	// Bounded dynamic regex cache
	reCache map[string]*regexp.Regexp

	// PRNG
	rng      *rand.Rand
	lastSeed int64

	// Control flow & exit state
	ctrl        awkCtrlKind
	retVal      awkVal
	exitCode    int
	hasExitCode bool
	inEnd       bool
	err         error
	tick        uint32
	callDepth   int
}

func newAwkVM(ctx context.Context, hc interp.HandlerContext, prog *awkProgram, argv0 string, fileArgs []string) *awkVM {
	seed := int64(1)
	stdoutSync := &awkSyncWriter{w: hc.Stdout}
	stderrSync := &awkSyncWriter{w: hc.Stderr}
	vm := &awkVM{
		ctx:         ctx,
		hc:          hc,
		prog:        prog,
		stdoutRaw:   stdoutSync,
		stderrRaw:   stderrSync,
		stdout:      bufio.NewWriterSize(stdoutSync, 64*1024),
		outFiles:    make(map[string]*awkOutStream),
		inFiles:     make(map[string]*awkRecordReader),
		outPipes:    make(map[string]*awkOutPipe),
		inPipes:     make(map[string]*awkInPipe),
		fs:          " ",
		rs:          "\n",
		ofs:         " ",
		ors:         "\n",
		subsep:      "\x1c",
		ofmt:        "%.6g",
		convfmt:     "%.6g",
		argc:        len(fileArgs) + 1,
		fields:      make([]awkVal, 0, 16),
		vars:        make(map[string]*awkSlot, 32),
		rangeActive: make([]bool, len(prog.rules)),
		reCache:     make(map[string]*regexp.Regexp, 32),
		rng:         rand.New(rand.NewSource(seed)),
		lastSeed:    seed,
	}

	// Populate ARGV[0..ARGC-1]
	argvMap := make(map[string]awkVal, len(fileArgs)+1)
	argvMap["0"] = awkStrVal(argv0)
	for i, a := range fileArgs {
		argvMap[strconv.Itoa(i+1)] = awkStrNumVal(a)
		if eq := strings.IndexByte(a, '='); !(eq > 0 && isValidAwkIdent(a[:eq])) && a != "" {
			vm.hadFileOperand = true
		}
	}
	vm.vars["ARGV"] = &awkSlot{kind: slotArray, arr: argvMap}

	// Populate ENVIRON from explicit/OS env overlaid with shell hc.Env
	envMap := make(map[string]awkVal, 64)
	if hc.Env != nil {
		if ee, ok := hc.Env.(explicitEnviron); ok {
			for _, kv := range ee.pairs {
				if eq := strings.IndexByte(kv, '='); eq >= 0 {
					envMap[kv[:eq]] = awkStrNumVal(kv[eq+1:])
				}
			}
		} else if envFallbackAllowed(hc, "") {
			for _, kv := range os.Environ() {
				if eq := strings.IndexByte(kv, '='); eq >= 0 {
					envMap[kv[:eq]] = awkStrNumVal(kv[eq+1:])
				}
			}
		}
		hc.Env.Each(func(name string, vr expand.Variable) bool {
			if vr.IsSet() && vr.Kind == expand.String {
				envMap[name] = awkStrNumVal(vr.Str)
			}
			return true
		})
	} else {
		for _, kv := range os.Environ() {
			if eq := strings.IndexByte(kv, '='); eq >= 0 {
				envMap[kv[:eq]] = awkStrNumVal(kv[eq+1:])
			}
		}
	}
	vm.vars["ENVIRON"] = &awkSlot{kind: slotArray, arr: envMap}

	return vm
}

func (vm *awkVM) closeAllFiles() error {
	var firstErr error
	if err := vm.stdout.Flush(); err != nil && !isPipeClosed(err) && firstErr == nil {
		firstErr = err
	}
	for name, out := range vm.outFiles {
		if err := out.bw.Flush(); err != nil && !isPipeClosed(err) && firstErr == nil {
			firstErr = err
		}
		if out.closer != nil {
			if err := out.closer.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		delete(vm.outFiles, name)
	}
	for name, in := range vm.inFiles {
		if in.closer != nil {
			_ = in.closer.Close()
		}
		delete(vm.inFiles, name)
	}
	for cmd, p := range vm.outPipes {
		_ = p.bw.Flush()
		_ = p.pw.Close()
		<-p.done
		delete(vm.outPipes, cmd)
	}
	for cmd, ip := range vm.inPipes {
		_ = ip.pr.Close()
		if vm.ctx.Err() != nil {
			ip.cancel()
		}
		<-ip.done
		ip.cancel()
		delete(vm.inPipes, cmd)
	}
	return firstErr
}

func runAwkShellCmd(ctx context.Context, hc interp.HandlerContext, cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
	prog, err := syntax.NewParser().Parse(strings.NewReader(cmd), "")
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "sh: %v\n", err)
		}
		return 2
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	if hc.Env == nil {
		hc.Env = expand.ListEnviron()
	}
	execHandlers := []func(interp.ExecHandlerFunc) interp.ExecHandlerFunc{trackExec, extraHandler}
	if ee, ok := hc.Env.(explicitEnviron); ok {
		execHandlers = []func(interp.ExecHandlerFunc) interp.ExecHandlerFunc{wrapExplicitEnvMiddleware(ee.cleared), trackExec, extraHandler}
	}
	r, err := interp.New(
		interp.Dir(hc.Dir),
		interp.Env(expand.ListEnviron(shellExecEnv(hc)...)),
		interp.StdIO(stdin, stdout, stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ReadDirHandler2(shellReadDirHandler()),
		interp.StatHandler(shellStatHandler()),
		interp.AccessHandler(shellAccessHandler()),
		interp.ExecHandlers(execHandlers...),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		return 1
	}
	runErr := r.Run(ctx, prog)
	if runErr == nil {
		return 0
	}
	var st interp.ExitStatus
	if errors.As(runErr, &st) {
		return int(st)
	}
	var ee exitError
	if errors.As(runErr, &ee) {
		return ee.code
	}
	if errors.Is(runErr, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded {
		return 124
	}
	if errors.Is(runErr, context.Canceled) || ctx.Err() == context.Canceled {
		return 130
	}
	return 1
}

func (vm *awkVM) failf(format string, args ...any) {
	if vm.ctrl == ctrlNone {
		vm.ctrl = ctrlError
		vm.err = fmt.Errorf(format, args...)
	}
}

func (vm *awkVM) checkInterrupt() bool {
	vm.tick++
	if vm.tick&0xff == 0 {
		if err := vm.ctx.Err(); err != nil {
			vm.ctrl = ctrlError
			vm.err = err
			return true
		}
	}
	return vm.ctrl != ctrlNone
}

func (vm *awkVM) cachedRegex(pat string) (*regexp.Regexp, error) {
	if re, ok := vm.reCache[pat]; ok {
		return re, nil
	}
	re, err := compileAwkRegex(pat)
	if err != nil {
		return nil, err
	}
	if len(vm.reCache) >= 512 {
		clear(vm.reCache)
	}
	vm.reCache[pat] = re
	return re, nil
}

// ---------- Value Conversions ----------

func (vm *awkVM) toNum(v awkVal) float64 {
	switch v.kind {
	case awkUninit:
		return 0
	case awkNum, awkStrNum:
		return v.num
	case awkStr:
		return parseAwkPrefixNum(v.str)
	default:
		return 0
	}
}

func (vm *awkVM) toStr(v awkVal) string {
	return vm.formatVal(v, vm.convfmt)
}

func (vm *awkVM) toPrintStr(v awkVal) string {
	return vm.formatVal(v, vm.ofmt)
}

func (vm *awkVM) formatVal(v awkVal, numFmt string) string {
	switch v.kind {
	case awkUninit:
		return ""
	case awkStr, awkStrNum:
		return v.str
	case awkNum:
		n := v.num
		if math.IsNaN(n) {
			return "nan"
		}
		if math.IsInf(n, 1) {
			return "+inf"
		}
		if math.IsInf(n, -1) {
			return "-inf"
		}
		// POSIX/gawk exact integer rule: exact integers fitting in int64 always format as decimal integer!
		if n >= -9223372036854775808.0 && n < 9223372036854775808.0 && math.Trunc(n) == n {
			return strconv.FormatInt(int64(n), 10)
		}
		if numFmt == "%.6g" {
			return strconv.FormatFloat(n, 'g', 6, 64)
		}
		out, err := awkSprintf(numFmt, []awkVal{v})
		if err != nil {
			return strconv.FormatFloat(n, 'g', 6, 64)
		}
		return out
	default:
		return ""
	}
}

func isAwkTrue(v awkVal) bool {
	switch v.kind {
	case awkUninit:
		return false
	case awkNum, awkStrNum:
		return v.num != 0
	case awkStr:
		return v.str != ""
	default:
		return false
	}
}

func isCmpNum(v awkVal) bool {
	return v.kind == awkNum || v.kind == awkStrNum || v.kind == awkUninit
}

func (vm *awkVM) compareVals(a, b awkVal) int {
	if isCmpNum(a) && isCmpNum(b) {
		na, nb := a.num, b.num
		if na < nb {
			return -1
		}
		if na > nb {
			return 1
		}
		return 0
	}
	sa := vm.toStr(a)
	sb := vm.toStr(b)
	if sa < sb {
		return -1
	}
	if sa > sb {
		return 1
	}
	return 0
}

// ---------- Records & Fields ----------

func (vm *awkVM) setRecord(rec string, fromInput bool) {
	vm.line = rec
	vm.lineIsStrNum = fromInput
	vm.fieldsSplit = false
}

func (vm *awkVM) getField0() awkVal {
	if vm.lineIsStrNum {
		return awkStrNumVal(vm.line)
	}
	return awkStrVal(vm.line)
}

func (vm *awkVM) ensureFields() {
	if vm.fieldsSplit {
		return
	}
	vm.fieldsSplit = true
	vm.fields = vm.splitInto(vm.fields[:0], vm.line, vm.fs, nil, vm.rs == "")
}

// splitInto splits s into dst using fsStr (or explicit fsRegex if non-nil).
// paragraphMode is true ONLY when splitting $0 while RS == "".
func (vm *awkVM) splitInto(dst []awkVal, s string, fsStr string, fsRegex *regexp.Regexp, paragraphMode bool) []awkVal {
	if s == "" {
		return dst
	}
	if fsRegex != nil {
		return appendRegexSplit(dst, s, fsRegex)
	}
	if fsStr == " " {
		// Trim leading/trailing whitespace and split on runs of [ \t\n]+
		i := 0
		n := len(s)
		for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
			i++
		}
		for n > i && (s[n-1] == ' ' || s[n-1] == '\t' || s[n-1] == '\n') {
			n--
		}
		for i < n {
			start := i
			for i < n && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' {
				i++
			}
			dst = append(dst, awkStrNumVal(s[start:i]))
			for i < n && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n') {
				i++
			}
		}
		return dst
	}
	if fsStr == "" {
		// Split every UTF-8 rune
		for i := 0; i < len(s); {
			_, w := utf8.DecodeRuneInString(s[i:])
			dst = append(dst, awkStrNumVal(s[i:i+w]))
			i += w
		}
		return dst
	}
	if len(fsStr) == 1 {
		sep := fsStr[0]
		if !paragraphMode || sep == '\n' {
			start := 0
			for {
				idx := strings.IndexByte(s[start:], sep)
				if idx < 0 {
					dst = append(dst, awkStrNumVal(s[start:]))
					break
				}
				dst = append(dst, awkStrNumVal(s[start:start+idx]))
				start += idx + 1
			}
			return dst
		}
		// Paragraph mode (RS=="") with single-char FS: both sep and '\n' separate fields.
		start := 0
		for i := 0; i < len(s); i++ {
			if s[i] == sep || s[i] == '\n' {
				dst = append(dst, awkStrNumVal(s[start:i]))
				start = i + 1
			}
		}
		dst = append(dst, awkStrNumVal(s[start:]))
		return dst
	}

	// Multi-char FS is a regular expression.
	var re *regexp.Regexp
	if fsStr == vm.fs {
		if vm.fsRe == nil || vm.fsReSrc != vm.fs {
			compiled, err := compileAwkRegex(vm.fs)
			if err != nil {
				vm.failf("invalid FS regex %q: %v", vm.fs, err)
				return dst
			}
			vm.fsRe = compiled
			vm.fsReSrc = vm.fs
		}
		re = vm.fsRe
	} else {
		var err error
		re, err = vm.cachedRegex(fsStr)
		if err != nil {
			vm.failf("invalid split regex %q: %v", fsStr, err)
			return dst
		}
	}
	return appendRegexSplit(dst, s, re)
}

func appendRegexSplit(dst []awkVal, s string, re *regexp.Regexp) []awkVal {
	pos := 0
	for pos <= len(s) {
		loc := re.FindStringIndex(s[pos:])
		if loc == nil {
			dst = append(dst, awkStrNumVal(s[pos:]))
			break
		}
		mStart := pos + loc[0]
		mEnd := pos + loc[1]
		if mStart == mEnd {
			// Zero-width match: advance by one rune so we never loop infinitely.
			if pos >= len(s) {
				break
			}
			_, w := utf8.DecodeRuneInString(s[pos:])
			dst = append(dst, awkStrNumVal(s[pos:pos+w]))
			pos += w
			if pos == len(s) {
				break
			}
			continue
		}
		dst = append(dst, awkStrNumVal(s[pos:mStart]))
		pos = mEnd
	}
	return dst
}

func (vm *awkVM) getField(idx int) awkVal {
	if idx < 0 {
		vm.failf("attempt to access negative field $%d", idx)
		return awkVal{}
	}
	if idx == 0 {
		return vm.getField0()
	}
	vm.ensureFields()
	if idx > len(vm.fields) {
		return awkStrVal("")
	}
	return vm.fields[idx-1]
}

func (vm *awkVM) setField(idx int, val awkVal) {
	if idx < 0 {
		vm.failf("attempt to assign negative field $%d", idx)
		return
	}
	if idx == 0 {
		vm.line = vm.toStr(val)
		vm.lineIsStrNum = (val.kind == awkStrNum)
		vm.fieldsSplit = false
		return
	}
	vm.ensureFields()
	if idx > len(vm.fields) {
		for len(vm.fields) < idx {
			vm.fields = append(vm.fields, awkStrVal(""))
		}
	}
	vm.fields[idx-1] = val
	vm.rebuildLine()
}

func (vm *awkVM) setNF(n int) {
	if n < 0 {
		vm.failf("NF set to negative value %d", n)
		return
	}
	vm.ensureFields()
	if n == 0 {
		vm.fields = vm.fields[:0]
		vm.line = ""
		vm.lineIsStrNum = false
		return
	}
	if n < len(vm.fields) {
		vm.fields = vm.fields[:n]
	} else {
		for len(vm.fields) < n {
			vm.fields = append(vm.fields, awkStrVal(""))
		}
	}
	vm.rebuildLine()
}

func (vm *awkVM) rebuildLine() {
	if len(vm.fields) == 0 {
		vm.line = ""
		vm.lineIsStrNum = false
		return
	}
	var sb strings.Builder
	for i, f := range vm.fields {
		if i > 0 {
			sb.WriteString(vm.ofs)
		}
		sb.WriteString(vm.toStr(f))
	}
	vm.line = sb.String()
	vm.lineIsStrNum = false
}

// ---------- Record Stream Reading ----------

func newAwkRecordReader(r io.Reader, closer io.Closer) *awkRecordReader {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 64*1024)
	}
	return &awkRecordReader{br: br, closer: closer}
}

func (vm *awkVM) readRecord(rr *awkRecordReader) (string, bool, error) {
	if rr == nil || rr.eof {
		return "", false, nil
	}
	// If upstream has nothing buffered in memory and stdout has pending output,
	// flush stdout before potentially blocking on Read (prevents pipeline stalls!).
	if rr.br.Buffered() == 0 && vm.stdout.Buffered() > 0 {
		if err := vm.stdout.Flush(); err != nil {
			if isPipeClosed(err) {
				vm.ctrl = ctrlPipeClosed
				return "", false, nil
			}
			return "", false, err
		}
	}

	rs := vm.rs
	// Fast path 1: single-byte RS (including default "\n") with zero-copy ReadSlice
	if len(rs) == 1 {
		delim := rs[0]
		slice, err := rr.br.ReadSlice(delim)
		if err == nil {
			vm.rt = rs
			return string(slice[:len(slice)-1]), true, nil
		}
		if err == bufio.ErrBufferFull {
			var buf bytes.Buffer
			buf.Write(slice)
			for err == bufio.ErrBufferFull {
				slice, err = rr.br.ReadSlice(delim)
				buf.Write(slice)
			}
			if err != nil && err != io.EOF {
				return "", false, err
			}
			if err == io.EOF {
				rr.eof = true
				vm.rt = ""
				return buf.String(), true, nil
			}
			b := buf.Bytes()
			vm.rt = rs
			return string(b[:len(b)-1]), true, nil
		}
		if err == io.EOF {
			rr.eof = true
			vm.rt = ""
			if len(slice) == 0 {
				return "", false, nil
			}
			return string(slice), true, nil
		}
		return "", false, err
	}

	// Fast path 2: RS == "" (paragraph mode: records separated by 1+ blank lines)
	if rs == "" {
		// Skip leading '\n's
		for {
			b, err := rr.br.ReadByte()
			if err != nil {
				if err == io.EOF {
					rr.eof = true
					vm.rt = ""
					return "", false, nil
				}
				return "", false, err
			}
			if b != '\n' {
				_ = rr.br.UnreadByte()
				break
			}
		}
		var buf bytes.Buffer
		var rtBuf bytes.Buffer
		for {
			frag, err := rr.br.ReadBytes('\n')
			if err != nil {
				if err == io.EOF {
					rr.eof = true
					buf.Write(frag)
					rec := buf.String()
					rec = strings.TrimSuffix(rec, "\n")
					vm.rt = ""
					return rec, true, nil
				}
				return "", false, err
			}
			// Check if next byte is also '\n' (or sequence of '\n's -> paragraph end!)
			next, perr := rr.br.Peek(1)
			if perr == nil && next[0] == '\n' {
				// Current frag ended with '\n', followed by >=1 '\n': strip trailing '\n'
				buf.Write(frag[:len(frag)-1])
				rtBuf.WriteByte('\n')
				for {
					nb, nerr := rr.br.ReadByte()
					if nerr != nil {
						if nerr == io.EOF {
							rr.eof = true
						}
						break
					}
					if nb != '\n' {
						_ = rr.br.UnreadByte()
						break
					}
					rtBuf.WriteByte('\n')
				}
				vm.rt = rtBuf.String()
				return buf.String(), true, nil
			}
			buf.Write(frag)
		}
	}

	// General path 3: multi-character / regex RS (gawk feature!)
	if vm.rsRe == nil || vm.rsReSrc != rs {
		re, err := compileAwkRegex(rs)
		if err != nil {
			return "", false, fmt.Errorf("invalid RS regex %q: %v", rs, err)
		}
		vm.rsRe = re
		vm.rsReSrc = rs
	}
	// Read remaining stream into buffer when regex RS is active (or incremental chunk)
	all, err := io.ReadAll(rr.br)
	if err != nil {
		return "", false, err
	}
	if len(all) == 0 {
		rr.eof = true
		vm.rt = ""
		return "", false, nil
	}
	s := string(all)
	loc := vm.rsRe.FindStringIndex(s)
	if loc == nil || (loc[0] == 0 && loc[1] == 0) {
		rr.eof = true
		vm.rt = ""
		return s, true, nil
	}
	vm.rt = s[loc[0]:loc[1]]
	rem := s[loc[1]:]
	rr.br.Reset(strings.NewReader(rem))
	if len(rem) == 0 {
		rr.eof = true
	}
	return s[:loc[0]], true, nil
}

// ---------- Variable & LValue Access ----------

func (vm *awkVM) getSlot(name string) *awkSlot {
	if s, ok := vm.vars[name]; ok {
		return s
	}
	s := &awkSlot{kind: slotUntyped}
	vm.vars[name] = s
	return s
}

func (vm *awkVM) getBuiltinVar(id awkBuiltinVarID) awkVal {
	switch id {
	case awkVarFS:
		return awkStrVal(vm.fs)
	case awkVarRS:
		return awkStrVal(vm.rs)
	case awkVarOFS:
		return awkStrVal(vm.ofs)
	case awkVarORS:
		return awkStrVal(vm.ors)
	case awkVarSUBSEP:
		return awkStrVal(vm.subsep)
	case awkVarOFMT:
		return awkStrVal(vm.ofmt)
	case awkVarCONVFMT:
		return awkStrVal(vm.convfmt)
	case awkVarNR:
		return awkNumVal(float64(vm.nr))
	case awkVarFNR:
		return awkNumVal(float64(vm.fnr))
	case awkVarNF:
		vm.ensureFields()
		return awkNumVal(float64(len(vm.fields)))
	case awkVarFILENAME:
		return awkStrVal(vm.filename)
	case awkVarRSTART:
		return awkNumVal(float64(vm.rstart))
	case awkVarRLENGTH:
		return awkNumVal(float64(vm.rlength))
	case awkVarARGC:
		return awkNumVal(float64(vm.argc))
	case awkVarRT:
		return awkStrVal(vm.rt)
	default:
		return awkVal{}
	}
}

func (vm *awkVM) setBuiltinVar(id awkBuiltinVarID, v awkVal) {
	switch id {
	case awkVarFS:
		// POSIX AWK: changing FS does NOT retroactively re-split an already-read
		// current record; materialize current fields with the old FS first!
		if !vm.fieldsSplit && (vm.nr > 0 || vm.line != "") {
			vm.ensureFields()
		}
		vm.fs = vm.toStr(v)
		vm.fsRe = nil
	case awkVarRS:
		vm.rs = vm.toStr(v)
		vm.rsRe = nil
	case awkVarOFS:
		vm.ofs = vm.toStr(v)
	case awkVarORS:
		vm.ors = vm.toStr(v)
	case awkVarSUBSEP:
		vm.subsep = vm.toStr(v)
	case awkVarOFMT:
		vm.ofmt = vm.toStr(v)
	case awkVarCONVFMT:
		vm.convfmt = vm.toStr(v)
	case awkVarNR:
		vm.nr = int64(vm.toNum(v))
	case awkVarFNR:
		vm.fnr = int64(vm.toNum(v))
	case awkVarNF:
		vm.setNF(int(vm.toNum(v)))
	case awkVarFILENAME:
		vm.filename = vm.toStr(v)
	case awkVarRSTART:
		vm.rstart = int64(vm.toNum(v))
	case awkVarRLENGTH:
		vm.rlength = int64(vm.toNum(v))
	case awkVarARGC:
		vm.argc = int(vm.toNum(v))
	case awkVarRT:
		vm.rt = vm.toStr(v)
	}
}

func (vm *awkVM) evalSubscript(indices []awkExpr) string {
	if len(indices) == 1 {
		v := vm.evalExpr(indices[0])
		return vm.toStr(v)
	}
	var sb strings.Builder
	for i, idxExpr := range indices {
		if i > 0 {
			sb.WriteString(vm.subsep)
		}
		v := vm.evalExpr(idxExpr)
		if vm.ctrl != ctrlNone {
			return ""
		}
		sb.WriteString(vm.toStr(v))
	}
	return sb.String()
}

func (vm *awkVM) setVarByName(name string, v awkVal) {
	if id := resolveAwkBuiltinVar(name); id != awkVarNone {
		vm.setBuiltinVar(id, v)
		return
	}
	slot := vm.getSlot(name)
	if err := slot.setScalar(name, v); err != nil {
		vm.failf("%v", err)
	}
}

func (vm *awkVM) assignLValue(target awkExpr, op awkTokKind, rhsExpr awkExpr) awkVal {
	switch t := target.(type) {
	case *awkIdentExpr:
		// Check if local function parameter shadows a built-in name
		if localSlot, isBound := vm.vars[t.name]; isBound && t.builtinVar != awkVarNone {
			_ = localSlot
		}
		if t.builtinVar != awkVarNone && vm.vars[t.name] == nil {
			if op == awkTokAssign {
				val := vm.evalExpr(rhsExpr)
				if vm.ctrl != ctrlNone {
					return awkVal{}
				}
				vm.setBuiltinVar(t.builtinVar, val)
				return val
			}
			cur := vm.getBuiltinVar(t.builtinVar)
			rhs := vm.evalExpr(rhsExpr)
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			res := vm.applyCompoundOp(op, cur, rhs)
			vm.setBuiltinVar(t.builtinVar, res)
			return res
		}
		slot := vm.getSlot(t.name)
		if slot.kind == slotArray {
			vm.failf("attempt to use array %q in a scalar context", t.name)
			return awkVal{}
		}
		if op == awkTokAssign {
			val := vm.evalExpr(rhsExpr)
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			_ = slot.setScalar(t.name, val)
			return val
		}
		cur := slot.val
		rhs := vm.evalExpr(rhsExpr)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		res := vm.applyCompoundOp(op, cur, rhs)
		_ = slot.setScalar(t.name, res)
		return res

	case *awkFieldExpr:
		var idx int
		if t.constIdx >= 0 {
			idx = t.constIdx
		} else {
			iv := vm.evalExpr(t.index)
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			idx = int(vm.toNum(iv))
		}
		if op == awkTokAssign {
			val := vm.evalExpr(rhsExpr)
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			vm.setField(idx, val)
			return val
		}
		cur := vm.getField(idx)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		rhs := vm.evalExpr(rhsExpr)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		res := vm.applyCompoundOp(op, cur, rhs)
		vm.setField(idx, res)
		return res

	case *awkIndexExpr:
		key := vm.evalSubscript(t.indices)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		slot := vm.getSlot(t.arrName)
		arr, err := slot.ensureArray(t.arrName)
		if err != nil {
			vm.failf("%v", err)
			return awkVal{}
		}
		if op == awkTokAssign {
			val := vm.evalExpr(rhsExpr)
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			// Re-fetch arr in case rhsExpr performed `delete arr`!
			arr, err = slot.ensureArray(t.arrName)
			if err != nil {
				vm.failf("%v", err)
				return awkVal{}
			}
			arr[key] = val
			return val
		}
		cur := arr[key]
		rhs := vm.evalExpr(rhsExpr)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		res := vm.applyCompoundOp(op, cur, rhs)
		arr, err = slot.ensureArray(t.arrName)
		if err != nil {
			vm.failf("%v", err)
			return awkVal{}
		}
		arr[key] = res
		return res
	}
	vm.failf("invalid assignment target")
	return awkVal{}
}

func (vm *awkVM) applyCompoundOp(op awkTokKind, a, b awkVal) awkVal {
	na := vm.toNum(a)
	nb := vm.toNum(b)
	switch op {
	case awkTokAddAssign:
		return awkNumVal(na + nb)
	case awkTokSubAssign:
		return awkNumVal(na - nb)
	case awkTokMulAssign:
		return awkNumVal(na * nb)
	case awkTokDivAssign:
		if nb == 0 {
			vm.failf("division by zero")
			return awkVal{}
		}
		return awkNumVal(na / nb)
	case awkTokModAssign:
		if nb == 0 {
			vm.failf("division by zero in %%=")
			return awkVal{}
		}
		return awkNumVal(math.Mod(na, nb))
	case awkTokPowAssign:
		return awkNumVal(math.Pow(na, nb))
	default:
		return b
	}
}

func (vm *awkVM) evalIncDec(e *awkIncDecExpr) awkVal {
	delta := 1.0
	if e.op == awkTokDec {
		delta = -1.0
	}
	switch t := e.target.(type) {
	case *awkIdentExpr:
		if t.builtinVar != awkVarNone && vm.vars[t.name] == nil {
			oldNum := vm.toNum(vm.getBuiltinVar(t.builtinVar))
			newVal := awkNumVal(oldNum + delta)
			vm.setBuiltinVar(t.builtinVar, newVal)
			if e.pre {
				return newVal
			}
			return awkNumVal(oldNum)
		}
		slot := vm.getSlot(t.name)
		if slot.kind == slotArray {
			vm.failf("attempt to use array %q in a scalar context", t.name)
			return awkVal{}
		}
		oldNum := vm.toNum(slot.val)
		newVal := awkNumVal(oldNum + delta)
		_ = slot.setScalar(t.name, newVal)
		if e.pre {
			return newVal
		}
		return awkNumVal(oldNum)

	case *awkFieldExpr:
		var idx int
		if t.constIdx >= 0 {
			idx = t.constIdx
		} else {
			iv := vm.evalExpr(t.index)
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			idx = int(vm.toNum(iv))
		}
		oldNum := vm.toNum(vm.getField(idx))
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		newVal := awkNumVal(oldNum + delta)
		vm.setField(idx, newVal)
		if e.pre {
			return newVal
		}
		return awkNumVal(oldNum)

	case *awkIndexExpr:
		key := vm.evalSubscript(t.indices)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		slot := vm.getSlot(t.arrName)
		arr, err := slot.ensureArray(t.arrName)
		if err != nil {
			vm.failf("%v", err)
			return awkVal{}
		}
		oldNum := vm.toNum(arr[key])
		newVal := awkNumVal(oldNum + delta)
		arr[key] = newVal
		if e.pre {
			return newVal
		}
		return awkNumVal(oldNum)
	}
	return awkVal{}
}

// ---------- Expression Evaluator ----------

func (vm *awkVM) evalExpr(expr awkExpr) awkVal {
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	switch e := expr.(type) {
	case *awkNumExpr:
		return awkNumVal(e.val)

	case *awkStrExpr:
		return awkStrVal(e.val)

	case *awkRegexExpr:
		// Standalone `/re/` evaluates `$0 ~ /re/`
		return awkBoolVal(e.re.MatchString(vm.line))

	case *awkIdentExpr:
		if e.builtinVar != awkVarNone && vm.vars[e.name] == nil {
			return vm.getBuiltinVar(e.builtinVar)
		}
		slot := vm.getSlot(e.name)
		if slot.kind == slotArray {
			vm.failf("attempt to use array %q in a scalar context", e.name)
			return awkVal{}
		}
		return slot.val

	case *awkFieldExpr:
		if e.constIdx >= 0 {
			return vm.getField(e.constIdx)
		}
		iv := vm.evalExpr(e.index)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		return vm.getField(int(vm.toNum(iv)))

	case *awkIndexExpr:
		key := vm.evalSubscript(e.indices)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		slot := vm.getSlot(e.arrName)
		arr, err := slot.ensureArray(e.arrName)
		if err != nil {
			vm.failf("%v", err)
			return awkVal{}
		}
		if v, ok := arr[key]; ok {
			return v
		}
		// POSIX AWK: reading arr[key] creates an uninitialized element!
		arr[key] = awkVal{}
		return awkVal{}

	case *awkUnaryExpr:
		v := vm.evalExpr(e.expr)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		switch e.op {
		case awkTokPlus:
			return awkNumVal(vm.toNum(v))
		case awkTokMinus:
			return awkNumVal(-vm.toNum(v))
		case awkTokBang:
			return awkBoolVal(!isAwkTrue(v))
		}

	case *awkIncDecExpr:
		return vm.evalIncDec(e)

	case *awkAssignExpr:
		return vm.assignLValue(e.target, e.op, e.val)

	case *awkConcatExpr:
		var sb strings.Builder
		for _, part := range e.parts {
			pv := vm.evalExpr(part)
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			sb.WriteString(vm.toStr(pv))
		}
		return awkStrVal(sb.String())

	case *awkTernaryExpr:
		cv := vm.evalExpr(e.cond)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		if isAwkTrue(cv) {
			return vm.evalExpr(e.thenExpr)
		}
		return vm.evalExpr(e.elseExpr)

	case *awkInExpr:
		key := vm.evalSubscript(e.indices)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		slot := vm.getSlot(e.arrName)
		arr, err := slot.ensureArray(e.arrName)
		if err != nil {
			vm.failf("%v", err)
			return awkVal{}
		}
		_, ok := arr[key]
		return awkBoolVal(ok)

	case *awkBinaryExpr:
		return vm.evalBinary(e)

	case *awkCallExpr:
		if e.builtinID != awkFnNone {
			return vm.evalBuiltinCall(e)
		}
		return vm.evalUserCall(e)

	case *awkGetlineExpr:
		return vm.evalGetline(e)
	}
	return awkVal{}
}

func (vm *awkVM) evalRegexArg(expr awkExpr) (*regexp.Regexp, bool) {
	if reNode, ok := expr.(*awkRegexExpr); ok {
		return reNode.re, true
	}
	v := vm.evalExpr(expr)
	if vm.ctrl != ctrlNone {
		return nil, false
	}
	re, err := vm.cachedRegex(vm.toStr(v))
	if err != nil {
		vm.failf("invalid regexp: %v", err)
		return nil, false
	}
	return re, true
}

func (vm *awkVM) evalBinary(e *awkBinaryExpr) awkVal {
	// Short-circuit logical operators
	if e.op == awkTokAnd {
		lv := vm.evalExpr(e.left)
		if vm.ctrl != ctrlNone || !isAwkTrue(lv) {
			return awkBoolVal(false)
		}
		rv := vm.evalExpr(e.right)
		return awkBoolVal(isAwkTrue(rv))
	}
	if e.op == awkTokOr {
		lv := vm.evalExpr(e.left)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		if isAwkTrue(lv) {
			return awkBoolVal(true)
		}
		rv := vm.evalExpr(e.right)
		return awkBoolVal(isAwkTrue(rv))
	}

	// Regex match `~` and `!~`
	if e.op == awkTokMatch || e.op == awkTokNotMatch {
		lv := vm.evalExpr(e.left)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		s := vm.toStr(lv)
		re, ok := vm.evalRegexArg(e.right)
		if !ok {
			return awkVal{}
		}
		matched := re.MatchString(s)
		if e.op == awkTokNotMatch {
			matched = !matched
		}
		return awkBoolVal(matched)
	}

	lv := vm.evalExpr(e.left)
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	rv := vm.evalExpr(e.right)
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}

	switch e.op {
	case awkTokPlus:
		return awkNumVal(vm.toNum(lv) + vm.toNum(rv))
	case awkTokMinus:
		return awkNumVal(vm.toNum(lv) - vm.toNum(rv))
	case awkTokStar:
		return awkNumVal(vm.toNum(lv) * vm.toNum(rv))
	case awkTokSlash:
		den := vm.toNum(rv)
		if den == 0 {
			vm.failf("division by zero")
			return awkVal{}
		}
		return awkNumVal(vm.toNum(lv) / den)
	case awkTokPercent:
		den := vm.toNum(rv)
		if den == 0 {
			vm.failf("division by zero in %%")
			return awkVal{}
		}
		return awkNumVal(math.Mod(vm.toNum(lv), den))
	case awkTokCaret:
		return awkNumVal(math.Pow(vm.toNum(lv), vm.toNum(rv)))
	case awkTokEQ:
		return awkBoolVal(vm.compareVals(lv, rv) == 0)
	case awkTokNE:
		return awkBoolVal(vm.compareVals(lv, rv) != 0)
	case awkTokLT:
		return awkBoolVal(vm.compareVals(lv, rv) < 0)
	case awkTokLE:
		return awkBoolVal(vm.compareVals(lv, rv) <= 0)
	case awkTokGT:
		return awkBoolVal(vm.compareVals(lv, rv) > 0)
	case awkTokGE:
		return awkBoolVal(vm.compareVals(lv, rv) >= 0)
	}
	return awkVal{}
}

func (vm *awkVM) evalUserCall(e *awkCallExpr) awkVal {
	fn, ok := vm.prog.funcs[e.name]
	if !ok {
		vm.failf("function %q is not defined", e.name)
		return awkVal{}
	}
	if vm.callDepth >= 1000 {
		vm.failf("function call recursion depth limit exceeded in %q", e.name)
		return awkVal{}
	}

	// Evaluate call arguments BEFORE binding parameter names in vm.vars!
	newSlots := make([]*awkSlot, len(fn.params))
	for i := range fn.params {
		if i < len(e.args) {
			argExpr := e.args[i]
			if idExpr, isIdent := argExpr.(*awkIdentExpr); isIdent && (idExpr.builtinVar == awkVarNone || vm.vars[idExpr.name] != nil) {
				callerSlot := vm.getSlot(idExpr.name)
				switch callerSlot.kind {
				case slotArray:
					newSlots[i] = callerSlot // pass array by reference
				case slotScalar:
					newSlots[i] = &awkSlot{kind: slotScalar, val: callerSlot.val}
				case slotUntyped:
					newSlots[i] = &awkSlot{kind: slotUntyped, origin: callerSlot}
				}
			} else {
				v := vm.evalExpr(argExpr)
				if vm.ctrl != ctrlNone {
					return awkVal{}
				}
				newSlots[i] = &awkSlot{kind: slotScalar, val: v}
			}
		} else {
			// Omitted parameter -> local variable (scalar or array)!
			newSlots[i] = &awkSlot{kind: slotUntyped}
		}
	}
	// Evaluate any excess arguments for side effects (POSIX AWK requirement)
	for i := len(fn.params); i < len(e.args); i++ {
		_ = vm.evalExpr(e.args[i])
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
	}

	// Save outer bindings and install parameter bindings
	type savedBinding struct {
		existed bool
		slot    *awkSlot
	}
	saved := make([]savedBinding, len(fn.params))
	for i, pName := range fn.params {
		old, existed := vm.vars[pName]
		saved[i] = savedBinding{existed: existed, slot: old}
		vm.vars[pName] = newSlots[i]
	}

	vm.callDepth++
	vm.execBlock(fn.body)
	vm.callDepth--

	// Restore outer bindings
	for i, pName := range fn.params {
		if saved[i].existed {
			vm.vars[pName] = saved[i].slot
		} else {
			delete(vm.vars, pName)
		}
	}

	if vm.ctrl == ctrlReturn {
		res := vm.retVal
		vm.retVal = awkVal{}
		vm.ctrl = ctrlNone
		return res
	}
	return awkVal{}
}

func (vm *awkVM) evalGetline(e *awkGetlineExpr) awkVal {
	if vm.checkInterrupt() {
		return awkVal{}
	}
	var rr *awkRecordReader
	externalSource := e.file != nil || e.pipeCmd != nil
	if e.pipeCmd != nil {
		cv := vm.evalExpr(e.pipeCmd)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		cmdStr := vm.toStr(cv)
		rr = vm.getInPipe(cmdStr)
	} else if e.file != nil {
		fv := vm.evalExpr(e.file)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		fname := vm.toStr(fv)
		var err error
		rr, err = vm.getInFile(fname)
		if err != nil {
			// POSIX AWK: getline < file returns -1 on open/read error (not fatal!)
			return awkNumVal(-1)
		}
	} else {
		rr = vm.curReader
		if rr == nil {
			// Inside BEGIN before files open: bare getline reads from stdin if no file args
			rr = vm.ensureImplicitStdin()
		}
	}

	rec, ok, err := vm.readRecord(rr)
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	if err != nil {
		return awkNumVal(-1)
	}
	if !ok {
		return awkNumVal(0)
	}

	if !externalSource {
		vm.nr++
		vm.fnr++
	}
	if e.target == nil {
		vm.setRecord(rec, true)
	} else {
		vm.assignValueDirect(e.target, awkStrNumVal(rec))
	}
	return awkNumVal(1)
}

func (vm *awkVM) assignValueDirect(target awkExpr, val awkVal) {
	switch t := target.(type) {
	case *awkIdentExpr:
		if t.builtinVar != awkVarNone && vm.vars[t.name] == nil {
			vm.setBuiltinVar(t.builtinVar, val)
			return
		}
		slot := vm.getSlot(t.name)
		if err := slot.setScalar(t.name, val); err != nil {
			vm.failf("%v", err)
		}
	case *awkFieldExpr:
		idx := t.constIdx
		if idx < 0 {
			iv := vm.evalExpr(t.index)
			if vm.ctrl != ctrlNone {
				return
			}
			idx = int(vm.toNum(iv))
		}
		vm.setField(idx, val)
	case *awkIndexExpr:
		key := vm.evalSubscript(t.indices)
		if vm.ctrl != ctrlNone {
			return
		}
		slot := vm.getSlot(t.arrName)
		arr, err := slot.ensureArray(t.arrName)
		if err != nil {
			vm.failf("%v", err)
			return
		}
		arr[key] = val
	}
}

func (vm *awkVM) ensureImplicitStdin() *awkRecordReader {
	if rr, ok := vm.inFiles["-"]; ok {
		return rr
	}
	stdin := vm.hc.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	rr := newAwkRecordReader(stdin, nil)
	vm.inFiles["-"] = rr
	return rr
}

func (vm *awkVM) getInFile(name string) (*awkRecordReader, error) {
	if rr, ok := vm.inFiles[name]; ok {
		return rr, nil
	}
	if name == "-" || name == "/dev/stdin" {
		stdin := vm.hc.Stdin
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		rr := newAwkRecordReader(stdin, nil)
		vm.inFiles[name] = rr
		return rr, nil
	}
	if isNullDevPath(name) {
		rr := newAwkRecordReader(strings.NewReader(""), nil)
		vm.inFiles[name] = rr
		return rr, nil
	}
	f, err := fsOpenInput(vm.ctx, vm.hc.Dir, "awk", name)
	if err != nil {
		return nil, err
	}
	rr := newAwkRecordReader(f, f)
	vm.inFiles[name] = rr
	return rr, nil
}

func (vm *awkVM) getOutStream(name string, appendMode bool) (*bufio.Writer, io.Writer, error) {
	if name == "/dev/stdout" || name == "-" {
		return vm.stdout, nil, nil
	}
	if name == "/dev/stderr" {
		return nil, vm.stderrRaw, nil
	}
	if out, ok := vm.outFiles[name]; ok {
		return out.bw, nil, nil
	}
	flags := os.O_WRONLY | os.O_CREATE
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := fsOpenOutput(vm.ctx, vm.hc.Dir, "awk", name, flags, 0o666)
	if err != nil {
		return nil, nil, err
	}
	bw := bufio.NewWriterSize(f, 32*1024)
	vm.outFiles[name] = &awkOutStream{bw: bw, closer: f}
	return bw, nil, nil
}

func (vm *awkVM) getInPipe(cmd string) *awkRecordReader {
	if ip, ok := vm.inPipes[cmd]; ok {
		return ip.rr
	}
	_ = vm.stdout.Flush()
	pctx, cancel := context.WithCancel(vm.ctx)
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		code := runAwkShellCmd(pctx, vm.hc, cmd, nil, pw, vm.stderrRaw)
		_ = pw.Close()
		done <- code
	}()
	rr := newAwkRecordReader(pr, pr)
	vm.inPipes[cmd] = &awkInPipe{rr: rr, pr: pr, cancel: cancel, done: done}
	return rr
}

func (vm *awkVM) getOutPipe(cmd string) *bufio.Writer {
	if op, ok := vm.outPipes[cmd]; ok {
		return op.bw
	}
	_ = vm.stdout.Flush()
	pr, pw := io.Pipe()
	done := make(chan int, 1)
	go func() {
		code := runAwkShellCmd(vm.ctx, vm.hc, cmd, pr, vm.stdoutRaw, vm.stderrRaw)
		_ = pr.Close()
		done <- code
	}()
	bw := bufio.NewWriterSize(pw, 32*1024)
	vm.outPipes[cmd] = &awkOutPipe{bw: bw, pw: pw, done: done}
	return bw
}

// ---------- Statement Execution ----------

func (vm *awkVM) execBlock(blk *awkBlockStmt) {
	if blk == nil {
		return
	}
	for _, st := range blk.stmts {
		if vm.ctrl != ctrlNone {
			return
		}
		vm.execStmt(st)
	}
}

func (vm *awkVM) execStmt(stmt awkStmt) {
	if stmt == nil || vm.checkInterrupt() {
		return
	}
	switch s := stmt.(type) {
	case *awkBlockStmt:
		vm.execBlock(s)

	case *awkExprStmt:
		_ = vm.evalExpr(s.expr)

	case *awkIfStmt:
		cv := vm.evalExpr(s.cond)
		if vm.ctrl != ctrlNone {
			return
		}
		if isAwkTrue(cv) {
			vm.execStmt(s.thenStmt)
		} else if s.elseStmt != nil {
			vm.execStmt(s.elseStmt)
		}

	case *awkWhileStmt:
		for {
			if vm.checkInterrupt() {
				return
			}
			cv := vm.evalExpr(s.cond)
			if vm.ctrl != ctrlNone || !isAwkTrue(cv) {
				return
			}
			vm.execStmt(s.body)
			if vm.ctrl == ctrlBreak {
				vm.ctrl = ctrlNone
				return
			}
			if vm.ctrl == ctrlContinue {
				vm.ctrl = ctrlNone
				continue
			}
			if vm.ctrl != ctrlNone {
				return
			}
		}

	case *awkDoWhileStmt:
		for {
			if vm.checkInterrupt() {
				return
			}
			vm.execStmt(s.body)
			if vm.ctrl == ctrlBreak {
				vm.ctrl = ctrlNone
				return
			}
			if vm.ctrl == ctrlContinue {
				vm.ctrl = ctrlNone
			} else if vm.ctrl != ctrlNone {
				return
			}
			cv := vm.evalExpr(s.cond)
			if vm.ctrl != ctrlNone || !isAwkTrue(cv) {
				return
			}
		}

	case *awkForStmt:
		if s.init != nil {
			vm.execStmt(s.init)
			if vm.ctrl != ctrlNone {
				return
			}
		}
		for {
			if vm.checkInterrupt() {
				return
			}
			if s.cond != nil {
				cv := vm.evalExpr(s.cond)
				if vm.ctrl != ctrlNone || !isAwkTrue(cv) {
					return
				}
			}
			vm.execStmt(s.body)
			if vm.ctrl == ctrlBreak {
				vm.ctrl = ctrlNone
				return
			}
			if vm.ctrl == ctrlContinue {
				vm.ctrl = ctrlNone
			} else if vm.ctrl != ctrlNone {
				return
			}
			if s.post != nil {
				vm.execStmt(s.post)
				if vm.ctrl != ctrlNone {
					return
				}
			}
		}

	case *awkForInStmt:
		slot := vm.getSlot(s.arrName)
		arr, err := slot.ensureArray(s.arrName)
		if err != nil {
			vm.failf("%v", err)
			return
		}
		for k := range arr {
			if vm.checkInterrupt() {
				return
			}
			vm.setVarByName(s.varName, awkStrVal(k))
			if vm.ctrl != ctrlNone {
				return
			}
			vm.execStmt(s.body)
			if vm.ctrl == ctrlBreak {
				vm.ctrl = ctrlNone
				return
			}
			if vm.ctrl == ctrlContinue {
				vm.ctrl = ctrlNone
				continue
			}
			if vm.ctrl != ctrlNone {
				return
			}
		}

	case *awkBreakStmt:
		vm.ctrl = ctrlBreak

	case *awkContinueStmt:
		vm.ctrl = ctrlContinue

	case *awkNextStmt:
		vm.ctrl = ctrlNext

	case *awkNextfileStmt:
		vm.ctrl = ctrlNextfile

	case *awkExitStmt:
		if s.code != nil {
			cv := vm.evalExpr(s.code)
			if vm.ctrl != ctrlNone {
				return
			}
			vm.exitCode = int(vm.toNum(cv))
			vm.hasExitCode = true
		}
		vm.ctrl = ctrlExit

	case *awkReturnStmt:
		if s.val != nil {
			rv := vm.evalExpr(s.val)
			if vm.ctrl != ctrlNone {
				return
			}
			vm.retVal = rv
		} else {
			vm.retVal = awkVal{}
		}
		vm.ctrl = ctrlReturn

	case *awkDeleteStmt:
		slot := vm.getSlot(s.arrName)
		arr, err := slot.ensureArray(s.arrName)
		if err != nil {
			vm.failf("%v", err)
			return
		}
		if len(s.indices) == 0 {
			clear(arr)
			return
		}
		key := vm.evalSubscript(s.indices)
		if vm.ctrl != ctrlNone {
			return
		}
		delete(arr, key)

	case *awkPrintStmt:
		vm.execPrint(s)
	}
}

func (vm *awkVM) execPrint(s *awkPrintStmt) {
	var text string
	if s.isPrintf {
		fmtVal := vm.evalExpr(s.args[0])
		if vm.ctrl != ctrlNone {
			return
		}
		fmtArgs := make([]awkVal, 0, len(s.args)-1)
		for _, a := range s.args[1:] {
			av := vm.evalExpr(a)
			if vm.ctrl != ctrlNone {
				return
			}
			fmtArgs = append(fmtArgs, av)
		}
		out, err := awkSprintf(vm.toStr(fmtVal), fmtArgs)
		if err != nil {
			vm.failf("printf: %v", err)
			return
		}
		text = out
	} else if len(s.args) == 0 {
		text = vm.line + vm.ors
	} else if len(s.args) == 1 {
		v := vm.evalExpr(s.args[0])
		if vm.ctrl != ctrlNone {
			return
		}
		text = vm.toPrintStr(v) + vm.ors
	} else {
		var sb strings.Builder
		for i, a := range s.args {
			if i > 0 {
				sb.WriteString(vm.ofs)
			}
			v := vm.evalExpr(a)
			if vm.ctrl != ctrlNone {
				return
			}
			sb.WriteString(vm.toPrintStr(v))
		}
		sb.WriteString(vm.ors)
		text = sb.String()
	}

	bw := vm.stdout
	var rawW io.Writer
	if s.redirOp != awkRedirNone {
		tv := vm.evalExpr(s.redirTarget)
		if vm.ctrl != ctrlNone {
			return
		}
		targetName := vm.toStr(tv)
		if s.redirOp == awkRedirPipe {
			bw = vm.getOutPipe(targetName)
		} else {
			var err error
			bw, rawW, err = vm.getOutStream(targetName, s.redirOp == awkRedirAppend)
			if err != nil {
				vm.failf("cannot open %q for output: %v", targetName, err)
				return
			}
		}
	}

	var werr error
	if bw != nil {
		_, werr = bw.WriteString(text)
	} else if rawW != nil {
		_, werr = io.WriteString(rawW, text)
	}
	if werr != nil {
		if isPipeClosed(werr) {
			if s.redirOp != awkRedirPipe {
				vm.ctrl = ctrlPipeClosed
			}
			return
		}
		vm.failf("write error: %v", werr)
	}
}

// ---------- Top-Level Program Driver ----------

func (vm *awkVM) run() error {
	defer func() {
		_ = vm.closeAllFiles()
	}()

	// 1. Execute BEGIN blocks
	for _, b := range vm.prog.begins {
		vm.execBlock(b)
		if vm.ctrl != ctrlNone {
			break
		}
	}
	if vm.ctrl == ctrlPipeClosed {
		return nil
	}
	if vm.ctrl == ctrlError {
		return vm.err
	}

	exitedBeforeEnd := vm.ctrl == ctrlExit

	// 2. Execute main input loop (only if not exited AND program has rules or END blocks!)
	if !exitedBeforeEnd && (len(vm.prog.rules) > 0 || len(vm.prog.ends) > 0) {
		if err := vm.runInputLoop(); err != nil {
			if isPipeClosed(err) || vm.ctrl == ctrlPipeClosed {
				return nil
			}
			return err
		}
		if vm.ctrl == ctrlPipeClosed {
			return nil
		}
		if vm.ctrl == ctrlError {
			return vm.err
		}
	}

	// 3. Execute END blocks (POSIX AWK: `exit` in BEGIN or main rule STILL runs END!)
	vm.ctrl = ctrlNone
	vm.inEnd = true
	for _, e := range vm.prog.ends {
		vm.execBlock(e)
		if vm.ctrl != ctrlNone {
			break
		}
	}
	if vm.ctrl == ctrlPipeClosed {
		return nil
	}
	if vm.ctrl == ctrlError {
		return vm.err
	}

	if err := vm.closeAllFiles(); err != nil {
		return err
	}
	if vm.exitCode != 0 {
		return exitError{code: vm.exitCode}
	}
	return nil
}

func (vm *awkVM) runInputLoop() error {
	argvSlot := vm.vars["ARGV"]
	var argvMap map[string]awkVal
	if argvSlot != nil && argvSlot.kind == slotArray {
		argvMap = argvSlot.arr
	}

	consumedAnyFile := false
	for i := 1; i < vm.argc; i++ {
		if err := vm.ctx.Err(); err != nil {
			return err
		}
		argVal, ok := argvMap[strconv.Itoa(i)]
		if !ok {
			continue
		}
		arg := vm.toStr(argVal)
		if arg == "" {
			// POSIX AWK: setting ARGV[i] = "" skips that argument!
			continue
		}
		// Check inline var=val assignment (`awk '{...}' x=1 file1 x=2 file2`)
		if eq := strings.IndexByte(arg, '='); eq > 0 && isValidAwkIdent(arg[:eq]) {
			valStr := awkUnescapeC(arg[eq+1:])
			vm.setVarByName(arg[:eq], awkStrNumVal(valStr))
			if vm.ctrl == ctrlError {
				return vm.err
			}
			continue
		}

		consumedAnyFile = true
		if err := vm.processInputSource(arg); err != nil {
			return err
		}
		if vm.ctrl == ctrlExit || vm.ctrl == ctrlPipeClosed || vm.ctrl == ctrlError {
			return vm.err
		}
	}

	if !consumedAnyFile && !vm.hadFileOperand {
		if err := vm.processInputSource("-"); err != nil {
			return err
		}
	}
	return vm.err
}

func (vm *awkVM) processInputSource(arg string) error {
	var rr *awkRecordReader
	if arg == "-" || arg == "/dev/stdin" {
		stdin := vm.hc.Stdin
		if stdin == nil {
			stdin = strings.NewReader("")
		}
		rr = newAwkRecordReader(stdin, nil)
		if arg == "-" {
			vm.filename = "-"
		} else {
			vm.filename = arg
		}
	} else if isNullDevPath(arg) {
		rr = newAwkRecordReader(strings.NewReader(""), nil)
		vm.filename = arg
	} else {
		f, err := fsOpenInput(vm.ctx, vm.hc.Dir, "awk", arg)
		if err != nil {
			fmt.Fprintf(vm.hc.Stderr, "awk: can't open file %s: %v\n", arg, err)
			return exitError{code: 2}
		}
		defer f.Close()
		rr = newAwkRecordReader(f, nil)
		vm.filename = arg
	}

	vm.fnr = 0
	vm.curReader = rr
	defer func() { vm.curReader = nil }()

	for {
		if vm.checkInterrupt() {
			return vm.err
		}
		rec, ok, rerr := vm.readRecord(rr)
		if vm.ctrl == ctrlPipeClosed {
			return nil
		}
		if rerr != nil {
			if isPipeClosed(rerr) {
				return nil
			}
			fmt.Fprintf(vm.hc.Stderr, "awk: error reading %s: %v\n", arg, rerr)
			return exitError{code: 2}
		}
		if !ok {
			break
		}

		vm.nr++
		vm.fnr++
		vm.setRecord(rec, true)

		for ri, rule := range vm.prog.rules {
			matched := false
			if rule.pat1 == nil {
				matched = true
			} else if rule.pat2 == nil {
				pv := vm.evalExpr(rule.pat1)
				if vm.ctrl != ctrlNone {
					break
				}
				matched = isAwkTrue(pv)
			} else {
				// Range pattern: pat1, pat2
				if !vm.rangeActive[ri] {
					p1 := vm.evalExpr(rule.pat1)
					if vm.ctrl != ctrlNone {
						break
					}
					if isAwkTrue(p1) {
						matched = true
						p2 := vm.evalExpr(rule.pat2)
						if vm.ctrl != ctrlNone {
							break
						}
						if !isAwkTrue(p2) {
							vm.rangeActive[ri] = true
						}
					}
				} else {
					matched = true
					p2 := vm.evalExpr(rule.pat2)
					if vm.ctrl != ctrlNone {
						break
					}
					if isAwkTrue(p2) {
						vm.rangeActive[ri] = false
					}
				}
			}

			if !matched {
				continue
			}

			if rule.action == nil {
				// Default action: `{ print $0 }`
				if _, werr := vm.stdout.WriteString(vm.line); werr != nil {
					if isPipeClosed(werr) {
						vm.ctrl = ctrlPipeClosed
						return nil
					}
					return werr
				}
				if _, werr := vm.stdout.WriteString(vm.ors); werr != nil {
					if isPipeClosed(werr) {
						vm.ctrl = ctrlPipeClosed
						return nil
					}
					return werr
				}
			} else {
				vm.execBlock(rule.action)
			}

			if vm.ctrl == ctrlNext {
				vm.ctrl = ctrlNone
				break
			}
			if vm.ctrl == ctrlNextfile {
				vm.ctrl = ctrlNone
				return nil
			}
			if vm.ctrl != ctrlNone {
				return vm.err
			}
		}
	}
	return nil
}
