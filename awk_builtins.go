package main

import (
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (vm *awkVM) evalBuiltinCall(e *awkCallExpr) awkVal {
	switch e.builtinID {
	case awkFnLength:
		return vm.builtinLength(e.args)
	case awkFnSubstr:
		return vm.builtinSubstr(e.args)
	case awkFnIndex:
		return vm.builtinIndex(e.args)
	case awkFnSplit:
		return vm.builtinSplit(e.args)
	case awkFnSub:
		return vm.builtinSub(e.args, false)
	case awkFnGsub:
		return vm.builtinSub(e.args, true)
	case awkFnGensub:
		return vm.builtinGensub(e.args)
	case awkFnMatch:
		return vm.builtinMatch(e.args)
	case awkFnTolower:
		if len(e.args) != 1 {
			vm.failf("tolower() requires 1 argument")
			return awkVal{}
		}
		v := vm.evalExpr(e.args[0])
		return awkStrVal(strings.ToLower(vm.toStr(v)))
	case awkFnToupper:
		if len(e.args) != 1 {
			vm.failf("toupper() requires 1 argument")
			return awkVal{}
		}
		v := vm.evalExpr(e.args[0])
		return awkStrVal(strings.ToUpper(vm.toStr(v)))
	case awkFnSprintf:
		return vm.builtinSprintf(e.args)
	case awkFnInt:
		if len(e.args) != 1 {
			vm.failf("int() requires 1 argument")
			return awkVal{}
		}
		v := vm.evalExpr(e.args[0])
		return awkNumVal(math.Trunc(vm.toNum(v)))
	case awkFnSqrt:
		if len(e.args) != 1 {
			vm.failf("sqrt() requires 1 argument")
			return awkVal{}
		}
		v := vm.evalExpr(e.args[0])
		return awkNumVal(math.Sqrt(vm.toNum(v)))
	case awkFnSin:
		if len(e.args) != 1 {
			vm.failf("sin() requires 1 argument")
			return awkVal{}
		}
		v := vm.evalExpr(e.args[0])
		return awkNumVal(math.Sin(vm.toNum(v)))
	case awkFnCos:
		if len(e.args) != 1 {
			vm.failf("cos() requires 1 argument")
			return awkVal{}
		}
		v := vm.evalExpr(e.args[0])
		return awkNumVal(math.Cos(vm.toNum(v)))
	case awkFnAtan2:
		if len(e.args) != 2 {
			vm.failf("atan2() requires 2 arguments")
			return awkVal{}
		}
		y := vm.evalExpr(e.args[0])
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		x := vm.evalExpr(e.args[1])
		return awkNumVal(math.Atan2(vm.toNum(y), vm.toNum(x)))
	case awkFnExp:
		if len(e.args) != 1 {
			vm.failf("exp() requires 1 argument")
			return awkVal{}
		}
		v := vm.evalExpr(e.args[0])
		return awkNumVal(math.Exp(vm.toNum(v)))
	case awkFnLog:
		if len(e.args) != 1 {
			vm.failf("log() requires 1 argument")
			return awkVal{}
		}
		v := vm.evalExpr(e.args[0])
		return awkNumVal(math.Log(vm.toNum(v)))
	case awkFnRand:
		if len(e.args) != 0 {
			vm.failf("rand() takes 0 arguments")
			return awkVal{}
		}
		return awkNumVal(vm.rng.Float64())
	case awkFnSrand:
		if len(e.args) > 1 {
			vm.failf("srand() takes at most 1 argument")
			return awkVal{}
		}
		prev := vm.lastSeed
		var newSeed int64
		if len(e.args) == 0 {
			newSeed = time.Now().Unix()
		} else {
			sv := vm.evalExpr(e.args[0])
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			newSeed = int64(vm.toNum(sv))
		}
		vm.lastSeed = newSeed
		vm.rng = rand.New(rand.NewSource(newSeed))
		return awkNumVal(float64(prev))
	case awkFnClose:
		return vm.builtinClose(e.args)
	case awkFnFflush:
		return vm.builtinFflush(e.args)
	case awkFnSystem:
		return vm.builtinSystem(e.args)
	case awkFnSystime:
		if len(e.args) != 0 {
			vm.failf("systime() takes 0 arguments")
			return awkVal{}
		}
		return awkNumVal(float64(time.Now().Unix()))
	case awkFnStrftime:
		return vm.builtinStrftime(e.args)
	default:
		vm.failf("unknown built-in function %q", e.name)
		return awkVal{}
	}
}

func awkRuneLen(s string) int {
	// Fast path: check if pure ASCII
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return utf8.RuneCountInString(s)
		}
	}
	return len(s)
}

func isAwkASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func (vm *awkVM) builtinLength(args []awkExpr) awkVal {
	if len(args) == 0 {
		return awkNumVal(float64(awkRuneLen(vm.line)))
	}
	if len(args) > 1 {
		vm.failf("length() takes at most 1 argument")
		return awkVal{}
	}
	if id, ok := args[0].(*awkIdentExpr); ok {
		if slot, exists := vm.vars[id.name]; exists && slot.kind == slotArray {
			return awkNumVal(float64(len(slot.arr)))
		}
	}
	v := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	return awkNumVal(float64(awkRuneLen(vm.toStr(v))))
}

func (vm *awkVM) builtinSubstr(args []awkExpr) awkVal {
	if len(args) < 2 || len(args) > 3 {
		vm.failf("substr() requires 2 or 3 arguments")
		return awkVal{}
	}
	sv := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	pv := vm.evalExpr(args[1])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	s := vm.toStr(sv)
	pos := int(math.Trunc(vm.toNum(pv)))
	if pos < 1 {
		pos = 1
	}
	hasLen := len(args) == 3
	maxLen := 0
	if hasLen {
		lv := vm.evalExpr(args[2])
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		maxLen = int(math.Trunc(vm.toNum(lv)))
		if maxLen <= 0 {
			return awkStrVal("")
		}
	}

	if isAwkASCII(s) {
		start := pos - 1
		if start >= len(s) {
			return awkStrVal("")
		}
		if !hasLen || start+maxLen >= len(s) {
			return awkStrVal(s[start:])
		}
		return awkStrVal(s[start : start+maxLen])
	}

	// UTF-8 rune-indexed substring without allocating []rune
	runeIdx := 0
	startByte := len(s)
	endByte := len(s)
	targetStart := pos - 1
	targetEnd := -1
	if hasLen {
		targetEnd = targetStart + maxLen
	}
	for bytePos := 0; bytePos < len(s); {
		if runeIdx == targetStart {
			startByte = bytePos
			if !hasLen {
				return awkStrVal(s[startByte:])
			}
		}
		if hasLen && runeIdx == targetEnd {
			endByte = bytePos
			break
		}
		_, w := utf8.DecodeRuneInString(s[bytePos:])
		bytePos += w
		runeIdx++
	}
	if startByte >= len(s) {
		return awkStrVal("")
	}
	return awkStrVal(s[startByte:endByte])
}

func (vm *awkVM) builtinIndex(args []awkExpr) awkVal {
	if len(args) != 2 {
		vm.failf("index() requires 2 arguments")
		return awkVal{}
	}
	sv := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	tv := vm.evalExpr(args[1])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	s := vm.toStr(sv)
	sub := vm.toStr(tv)
	if sub == "" {
		// In POSIX AWK / gawk: index(s, "") returns 1 if s != ""? Actually in gawk index("", "") == 1 and index("abc", "") == 1!
		// Wait: let's check POSIX: strings.Index returns 0 when sub == "", 0 + 1 = 1!
		return awkNumVal(1)
	}
	idx := strings.Index(s, sub)
	if idx < 0 {
		return awkNumVal(0)
	}
	return awkNumVal(float64(awkRuneLen(s[:idx]) + 1))
}

func (vm *awkVM) builtinSplit(args []awkExpr) awkVal {
	if len(args) < 2 || len(args) > 3 {
		vm.failf("split() requires 2 or 3 arguments")
		return awkVal{}
	}
	arrIdent, ok := args[1].(*awkIdentExpr)
	if !ok {
		vm.failf("split() second argument must be an array name")
		return awkVal{}
	}
	sv := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	s := vm.toStr(sv)

	fsStr := vm.fs
	var fsRegex *regexp.Regexp
	if len(args) == 3 {
		if reNode, isRe := args[2].(*awkRegexExpr); isRe {
			fsRegex = reNode.re
		} else {
			fv := vm.evalExpr(args[2])
			if vm.ctrl != ctrlNone {
				return awkVal{}
			}
			fsStr = vm.toStr(fv)
		}
	}

	slot := vm.getSlot(arrIdent.name)
	arr, err := slot.ensureArray(arrIdent.name)
	if err != nil {
		vm.failf("%v", err)
		return awkVal{}
	}
	clear(arr)

	parts := vm.splitInto(nil, s, fsStr, fsRegex, false)
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	for i, p := range parts {
		arr[strconv.Itoa(i+1)] = p
	}
	return awkNumVal(float64(len(parts)))
}

func (vm *awkVM) builtinSub(args []awkExpr, global bool) awkVal {
	if len(args) < 2 || len(args) > 3 {
		vm.failf("sub/gsub() requires 2 or 3 arguments")
		return awkVal{}
	}
	re, ok := vm.evalRegexArg(args[0])
	if !ok {
		return awkVal{}
	}
	rv := vm.evalExpr(args[1])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	repl := vm.toStr(rv)

	var target awkExpr
	var orig string
	if len(args) == 3 {
		target = args[2]
		if !isAwkLValue(target) {
			vm.failf("sub/gsub() third argument must be an lvalue")
			return awkVal{}
		}
		tv := vm.evalExpr(target)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		orig = vm.toStr(tv)
	} else {
		orig = vm.line
	}

	if !global {
		loc := re.FindStringIndex(orig)
		if loc == nil {
			return awkNumVal(0)
		}
		var sb strings.Builder
		sb.Grow(len(orig) + len(repl))
		sb.WriteString(orig[:loc[0]])
		writeSubReplacement(&sb, repl, orig[loc[0]:loc[1]])
		sb.WriteString(orig[loc[1]:])
		newStr := sb.String()
		if target == nil {
			vm.setField(0, awkStrVal(newStr))
		} else {
			vm.assignValueDirect(target, awkStrVal(newStr))
		}
		return awkNumVal(1)
	}

	locs := re.FindAllStringIndex(orig, -1)
	if len(locs) == 0 {
		return awkNumVal(0)
	}
	var sb strings.Builder
	sb.Grow(len(orig) + len(locs)*len(repl))
	prev := 0
	for _, loc := range locs {
		sb.WriteString(orig[prev:loc[0]])
		writeSubReplacement(&sb, repl, orig[loc[0]:loc[1]])
		prev = loc[1]
	}
	sb.WriteString(orig[prev:])
	newStr := sb.String()
	if target == nil {
		vm.setField(0, awkStrVal(newStr))
	} else {
		vm.assignValueDirect(target, awkStrVal(newStr))
	}
	return awkNumVal(float64(len(locs)))
}

// writeSubReplacement expands POSIX AWK sub/gsub replacement escapes:
// `&` -> matched substring, `\&` -> `&`, `\\` -> `\`.
func writeSubReplacement(sb *strings.Builder, repl string, matched string) {
	for i := 0; i < len(repl); i++ {
		ch := repl[i]
		if ch == '&' {
			sb.WriteString(matched)
			continue
		}
		if ch == '\\' && i+1 < len(repl) {
			next := repl[i+1]
			if next == '&' || next == '\\' {
				sb.WriteByte(next)
				i++
				continue
			}
		}
		sb.WriteByte(ch)
	}
}

func (vm *awkVM) builtinGensub(args []awkExpr) awkVal {
	if len(args) < 3 || len(args) > 4 {
		vm.failf("gensub() requires 3 or 4 arguments")
		return awkVal{}
	}
	re, ok := vm.evalRegexArg(args[0])
	if !ok {
		return awkVal{}
	}
	rv := vm.evalExpr(args[1])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	repl := vm.toStr(rv)

	hv := vm.evalExpr(args[2])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	howStr := vm.toStr(hv)
	global := len(howStr) > 0 && (howStr[0] == 'g' || howStr[0] == 'G')
	nth := 1
	if !global {
		nth = int(math.Trunc(vm.toNum(hv)))
		if nth <= 0 {
			nth = 1
		}
	}

	orig := vm.line
	if len(args) == 4 {
		tv := vm.evalExpr(args[3])
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		orig = vm.toStr(tv)
	}

	locs := re.FindAllStringSubmatchIndex(orig, -1)
	if len(locs) == 0 {
		return awkStrVal(orig)
	}
	if !global && nth > len(locs) {
		return awkStrVal(orig)
	}

	var sb strings.Builder
	sb.Grow(len(orig) + len(repl))
	prev := 0
	for idx, loc := range locs {
		if !global && idx+1 != nth {
			continue
		}
		sb.WriteString(orig[prev:loc[0]])
		writeGensubReplacement(&sb, repl, orig, loc)
		prev = loc[1]
		if !global {
			break
		}
	}
	sb.WriteString(orig[prev:])
	return awkStrVal(sb.String())
}

func writeGensubReplacement(sb *strings.Builder, repl string, orig string, loc []int) {
	for i := 0; i < len(repl); i++ {
		ch := repl[i]
		if ch == '&' {
			sb.WriteString(orig[loc[0]:loc[1]])
			continue
		}
		if ch == '\\' && i+1 < len(repl) {
			next := repl[i+1]
			if next == '&' || next == '\\' {
				sb.WriteByte(next)
				i++
				continue
			}
			if next >= '0' && next <= '9' {
				grp := int(next - '0')
				if 2*grp+1 < len(loc) && loc[2*grp] >= 0 {
					sb.WriteString(orig[loc[2*grp]:loc[2*grp+1]])
				}
				i++
				continue
			}
		}
		sb.WriteByte(ch)
	}
}

func (vm *awkVM) builtinMatch(args []awkExpr) awkVal {
	if len(args) < 2 || len(args) > 3 {
		vm.failf("match() requires 2 or 3 arguments")
		return awkVal{}
	}
	sv := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	s := vm.toStr(sv)
	re, ok := vm.evalRegexArg(args[1])
	if !ok {
		return awkVal{}
	}

	var arr map[string]awkVal
	if len(args) == 3 {
		arrIdent, isIdent := args[2].(*awkIdentExpr)
		if !isIdent {
			vm.failf("match() third argument must be an array name")
			return awkVal{}
		}
		slot := vm.getSlot(arrIdent.name)
		var err error
		arr, err = slot.ensureArray(arrIdent.name)
		if err != nil {
			vm.failf("%v", err)
			return awkVal{}
		}
		clear(arr)
	}

	if arr == nil {
		loc := re.FindStringIndex(s)
		if loc == nil {
			vm.rstart = 0
			vm.rlength = -1
			return awkNumVal(0)
		}
		rstart := awkRuneLen(s[:loc[0]]) + 1
		rlength := awkRuneLen(s[loc[0]:loc[1]])
		vm.rstart = int64(rstart)
		vm.rlength = int64(rlength)
		return awkNumVal(float64(rstart))
	}

	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		vm.rstart = 0
		vm.rlength = -1
		return awkNumVal(0)
	}
	rstart := awkRuneLen(s[:loc[0]]) + 1
	rlength := awkRuneLen(s[loc[0]:loc[1]])
	vm.rstart = int64(rstart)
	vm.rlength = int64(rlength)

	numGroups := len(loc) / 2
	for g := 0; g < numGroups; g++ {
		bStart, bEnd := loc[2*g], loc[2*g+1]
		if bStart < 0 {
			continue
		}
		gs := strconv.Itoa(g)
		gStart := awkRuneLen(s[:bStart]) + 1
		gLen := awkRuneLen(s[bStart:bEnd])
		arr[gs] = awkStrVal(s[bStart:bEnd])
		arr[gs+vm.subsep+"start"] = awkNumVal(float64(gStart))
		arr[gs+vm.subsep+"length"] = awkNumVal(float64(gLen))
	}
	return awkNumVal(float64(rstart))
}

func (vm *awkVM) builtinSprintf(args []awkExpr) awkVal {
	if len(args) == 0 {
		vm.failf("sprintf() requires at least 1 argument")
		return awkVal{}
	}
	fv := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	vals := make([]awkVal, 0, len(args)-1)
	for _, a := range args[1:] {
		av := vm.evalExpr(a)
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		vals = append(vals, av)
	}
	out, err := awkSprintf(vm.toStr(fv), vals)
	if err != nil {
		vm.failf("sprintf: %v", err)
		return awkVal{}
	}
	return awkStrVal(out)
}

func (vm *awkVM) builtinClose(args []awkExpr) awkVal {
	if len(args) != 1 {
		vm.failf("close() requires 1 argument")
		return awkVal{}
	}
	v := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	name := vm.toStr(v)
	if op, ok := vm.outPipes[name]; ok {
		_ = op.bw.Flush()
		_ = op.pw.Close()
		code := <-op.done
		delete(vm.outPipes, name)
		return awkNumVal(float64(code))
	}
	if ip, ok := vm.inPipes[name]; ok {
		_ = ip.pr.Close()
		if vm.ctx.Err() != nil {
			ip.cancel()
		}
		code := <-ip.done
		ip.cancel()
		delete(vm.inPipes, name)
		return awkNumVal(float64(code))
	}
	closed := false
	if out, ok := vm.outFiles[name]; ok {
		_ = out.bw.Flush()
		if out.closer != nil {
			_ = out.closer.Close()
		}
		delete(vm.outFiles, name)
		closed = true
	}
	if in, ok := vm.inFiles[name]; ok {
		if in.closer != nil {
			_ = in.closer.Close()
		}
		delete(vm.inFiles, name)
		closed = true
	}
	if closed {
		return awkNumVal(0)
	}
	return awkNumVal(-1)
}

func (vm *awkVM) builtinSystem(args []awkExpr) awkVal {
	if len(args) != 1 {
		vm.failf("system() requires 1 argument")
		return awkVal{}
	}
	v := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	_ = vm.stdout.Flush()
	for _, out := range vm.outFiles {
		_ = out.bw.Flush()
	}
	cmd := vm.toStr(v)
	if cmd == "" {
		return awkNumVal(0)
	}
	code := runAwkShellCmd(vm.ctx, vm.hc, cmd, nil, vm.stdoutRaw, vm.stderrRaw)
	return awkNumVal(float64(code))
}

func (vm *awkVM) builtinFflush(args []awkExpr) awkVal {
	if len(args) > 1 {
		vm.failf("fflush() takes at most 1 argument")
		return awkVal{}
	}
	if len(args) == 0 {
		_ = vm.stdout.Flush()
		for _, out := range vm.outFiles {
			_ = out.bw.Flush()
		}
		for _, op := range vm.outPipes {
			_ = op.bw.Flush()
		}
		return awkNumVal(0)
	}
	v := vm.evalExpr(args[0])
	if vm.ctrl != ctrlNone {
		return awkVal{}
	}
	name := vm.toStr(v)
	if name == "" {
		_ = vm.stdout.Flush()
		for _, out := range vm.outFiles {
			_ = out.bw.Flush()
		}
		for _, op := range vm.outPipes {
			_ = op.bw.Flush()
		}
		return awkNumVal(0)
	}
	if name == "/dev/stdout" || name == "-" {
		if err := vm.stdout.Flush(); err != nil {
			return awkNumVal(-1)
		}
		return awkNumVal(0)
	}
	if name == "/dev/stderr" {
		return awkNumVal(0)
	}
	if out, ok := vm.outFiles[name]; ok {
		if err := out.bw.Flush(); err != nil {
			return awkNumVal(-1)
		}
		return awkNumVal(0)
	}
	if op, ok := vm.outPipes[name]; ok {
		if err := op.bw.Flush(); err != nil {
			return awkNumVal(-1)
		}
		return awkNumVal(0)
	}
	return awkNumVal(-1)
}

func (vm *awkVM) builtinStrftime(args []awkExpr) awkVal {
	if len(args) > 3 {
		vm.failf("strftime() takes at most 3 arguments")
		return awkVal{}
	}
	format := "%a %b %e %H:%M:%S %Z %Y"
	if len(args) >= 1 {
		fv := vm.evalExpr(args[0])
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		if s := vm.toStr(fv); s != "" || fv.kind != awkUninit {
			format = s
		}
	}
	ts := time.Now()
	if len(args) >= 2 {
		tv := vm.evalExpr(args[1])
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		sec := int64(math.Trunc(vm.toNum(tv)))
		ts = time.Unix(sec, 0)
	}
	if len(args) == 3 {
		uv := vm.evalExpr(args[2])
		if vm.ctrl != ctrlNone {
			return awkVal{}
		}
		if isAwkTrue(uv) {
			ts = ts.UTC()
		}
	}
	return awkStrVal(awkFormatTime(format, ts))
}

func awkFormatTime(format string, t time.Time) string {
	var sb strings.Builder
	sb.Grow(len(format) + 16)
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			sb.WriteByte(format[i])
			continue
		}
		i++
		switch format[i] {
		case '%':
			sb.WriteByte('%')
		case 'Y':
			fmt.Fprintf(&sb, "%04d", t.Year())
		case 'y':
			fmt.Fprintf(&sb, "%02d", t.Year()%100)
		case 'C':
			fmt.Fprintf(&sb, "%02d", t.Year()/100)
		case 'm':
			fmt.Fprintf(&sb, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&sb, "%02d", t.Day())
		case 'e':
			fmt.Fprintf(&sb, "%2d", t.Day())
		case 'H':
			fmt.Fprintf(&sb, "%02d", t.Hour())
		case 'I':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			fmt.Fprintf(&sb, "%02d", h)
		case 'M':
			fmt.Fprintf(&sb, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&sb, "%02d", t.Second())
		case 'p':
			if t.Hour() < 12 {
				sb.WriteString("AM")
			} else {
				sb.WriteString("PM")
			}
		case 'P':
			if t.Hour() < 12 {
				sb.WriteString("am")
			} else {
				sb.WriteString("pm")
			}
		case 'a':
			sb.WriteString(t.Weekday().String()[:3])
		case 'A':
			sb.WriteString(t.Weekday().String())
		case 'b', 'h':
			sb.WriteString(t.Month().String()[:3])
		case 'B':
			sb.WriteString(t.Month().String())
		case 'j':
			fmt.Fprintf(&sb, "%03d", t.YearDay())
		case 'u':
			wd := int(t.Weekday())
			if wd == 0 {
				wd = 7
			}
			sb.WriteString(strconv.Itoa(wd))
		case 'w':
			sb.WriteString(strconv.Itoa(int(t.Weekday())))
		case 'V':
			_, wk := t.ISOWeek()
			fmt.Fprintf(&sb, "%02d", wk)
		case 'G':
			yr, _ := t.ISOWeek()
			fmt.Fprintf(&sb, "%04d", yr)
		case 'g':
			yr, _ := t.ISOWeek()
			fmt.Fprintf(&sb, "%02d", yr%100)
		case 'F':
			sb.WriteString(t.Format("2006-01-02"))
		case 'T', 'X':
			sb.WriteString(t.Format("15:04:05"))
		case 'R':
			sb.WriteString(t.Format("15:04"))
		case 'D', 'x':
			sb.WriteString(t.Format("01/02/06"))
		case 'r':
			sb.WriteString(t.Format("03:04:05 PM"))
		case 'c':
			sb.WriteString(awkFormatTime("%a %b %e %H:%M:%S %Y", t))
		case 's':
			sb.WriteString(strconv.FormatInt(t.Unix(), 10))
		case 'z':
			sb.WriteString(t.Format("-0700"))
		case 'Z':
			zn, _ := t.Zone()
			sb.WriteString(zn)
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		default:
			sb.WriteByte('%')
			sb.WriteByte(format[i])
		}
	}
	return sb.String()
}

// ---------- AWK printf / sprintf Engine ----------

func awkSprintf(format string, args []awkVal) (string, error) {
	var sb strings.Builder
	sb.Grow(len(format) + len(args)*8)
	argIdx := 0

	nextArg := func() (awkVal, error) {
		if argIdx >= len(args) {
			return awkVal{}, fmt.Errorf("not enough arguments for format string")
		}
		v := args[argIdx]
		argIdx++
		return v, nil
	}

	toNumArg := func(v awkVal) float64 {
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

	toStrArg := func(v awkVal) string {
		switch v.kind {
		case awkUninit:
			return ""
		case awkStr, awkStrNum:
			return v.str
		case awkNum:
			if math.IsNaN(v.num) {
				return "nan"
			}
			if math.IsInf(v.num, 1) {
				return "+inf"
			}
			if math.IsInf(v.num, -1) {
				return "-inf"
			}
			if v.num >= -9223372036854775808.0 && v.num < 9223372036854775808.0 && math.Trunc(v.num) == v.num {
				return strconv.FormatInt(int64(v.num), 10)
			}
			return strconv.FormatFloat(v.num, 'g', 6, 64)
		default:
			return ""
		}
	}

	for i := 0; i < len(format); {
		if format[i] != '%' {
			sb.WriteByte(format[i])
			i++
			continue
		}
		i++ // skip '%'
		if i >= len(format) {
			return "", fmt.Errorf("format string ends with '%%'")
		}
		if format[i] == '%' {
			sb.WriteByte('%')
			i++
			continue
		}

		// Parse flags: -, +, ' ', #, 0
		var flags []byte
		leftAlign := false
		for i < len(format) {
			c := format[i]
			if c == '-' || c == '+' || c == ' ' || c == '#' || c == '0' {
				if c == '-' {
					leftAlign = true
				}
				flags = append(flags, c)
				i++
			} else {
				break
			}
		}

		// Parse width (digits or '*')
		widthStr := ""
		if i < len(format) && format[i] == '*' {
			i++
			wv, err := nextArg()
			if err != nil {
				return "", err
			}
			w := int(math.Trunc(toNumArg(wv)))
			if w < 0 {
				if !leftAlign {
					flags = append(flags, '-')
					leftAlign = true
				}
				w = -w
			}
			widthStr = strconv.Itoa(w)
		} else {
			wStart := i
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
			widthStr = format[wStart:i]
		}

		// Parse precision ('.' followed by digits or '*')
		hasPrec := false
		precStr := ""
		if i < len(format) && format[i] == '.' {
			hasPrec = true
			i++
			if i < len(format) && format[i] == '*' {
				i++
				pv, err := nextArg()
				if err != nil {
					return "", err
				}
				p := int(math.Trunc(toNumArg(pv)))
				if p < 0 {
					// Negative dynamic precision is ignored in C/AWK printf
					hasPrec = false
				} else {
					precStr = strconv.Itoa(p)
				}
			} else {
				pStart := i
				for i < len(format) && format[i] >= '0' && format[i] <= '9' {
					i++
				}
				precStr = format[pStart:i]
				if precStr == "" {
					precStr = "0"
				}
			}
		}

		// Skip optional C length modifiers (h, hh, l, ll, L, z, j, t)
		for i < len(format) {
			c := format[i]
			if c == 'h' || c == 'l' || c == 'L' || c == 'z' || c == 'j' || c == 't' {
				i++
			} else {
				break
			}
		}

		if i >= len(format) {
			return "", fmt.Errorf("incomplete format specifier")
		}
		verb := format[i]
		i++

		val, err := nextArg()
		if err != nil {
			return "", err
		}

		buildGoSpec := func(goVerb byte) string {
			var spec strings.Builder
			spec.WriteByte('%')
			spec.Write(flags)
			spec.WriteString(widthStr)
			if hasPrec {
				spec.WriteByte('.')
				spec.WriteString(precStr)
			}
			spec.WriteByte(goVerb)
			return spec.String()
		}

		switch verb {
		case 's':
			sb.WriteString(fmt.Sprintf(buildGoSpec('s'), toStrArg(val)))

		case 'd', 'i':
			iv := int64(math.Trunc(toNumArg(val)))
			sb.WriteString(fmt.Sprintf(buildGoSpec('d'), iv))

		case 'u':
			f := math.Trunc(toNumArg(val))
			var uv uint64
			if f < 0 {
				uv = uint64(int64(f))
			} else {
				uv = uint64(f)
			}
			// Go's %d on uint64 formats unsigned decimal with full width/flag/prec parity!
			sb.WriteString(fmt.Sprintf(buildGoSpec('d'), uv))

		case 'o', 'x', 'X':
			f := math.Trunc(toNumArg(val))
			var uv uint64
			if f < 0 {
				uv = uint64(int64(f))
			} else {
				uv = uint64(f)
			}
			sb.WriteString(fmt.Sprintf(buildGoSpec(verb), uv))

		case 'f', 'F', 'e', 'E', 'g', 'G', 'a', 'A':
			f := toNumArg(val)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				var tok string
				switch {
				case math.IsNaN(f):
					tok = "nan"
				case math.IsInf(f, 1):
					tok = "+inf"
				default:
					tok = "-inf"
				}
				if verb == 'F' || verb == 'E' || verb == 'G' || verb == 'A' {
					tok = strings.ToUpper(tok)
				}
				if widthStr != "" {
					w, _ := strconv.Atoi(widthStr)
					if pad := w - len(tok); pad > 0 {
						if leftAlign {
							tok = tok + strings.Repeat(" ", pad)
						} else {
							tok = strings.Repeat(" ", pad) + tok
						}
					}
				}
				sb.WriteString(tok)
				break
			}
			goVerb := verb
			if goVerb == 'F' {
				goVerb = 'f'
			}
			if goVerb == 'a' {
				goVerb = 'x'
			}
			if goVerb == 'A' {
				goVerb = 'X'
			}
			sb.WriteString(fmt.Sprintf(buildGoSpec(goVerb), f))

		case 'c':
			var chStr string
			if val.kind == awkNum {
				code := int(math.Trunc(val.num))
				if code >= 0 && code < 0x110000 {
					chStr = string(rune(code))
				} else {
					chStr = string([]byte{byte(code)})
				}
			} else {
				s := toStrArg(val)
				if len(s) > 0 {
					r, _ := utf8.DecodeRuneInString(s)
					chStr = string(r)
				} else {
					chStr = "\x00"
				}
			}
			if widthStr == "" {
				sb.WriteString(chStr)
			} else {
				w, _ := strconv.Atoi(widthStr)
				pad := w - awkRuneLen(chStr)
				if pad <= 0 {
					sb.WriteString(chStr)
				} else if leftAlign {
					sb.WriteString(chStr)
					sb.WriteString(strings.Repeat(" ", pad))
				} else {
					sb.WriteString(strings.Repeat(" ", pad))
					sb.WriteString(chStr)
				}
			}

		default:
			return "", fmt.Errorf("invalid format specifier %%%c", verb)
		}
	}
	return sb.String(), nil
}
