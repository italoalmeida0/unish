package unish

import (
	"bytes"
	"encoding/base64"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

func isJqSpace(r rune) bool {
	if r <= 0xFF {
		return r == ' ' || (r >= '\t' && r <= '\r') || r == 0x85 || r == 0xA0
	}
	return (r >= 0x2000 && r <= 0x200A) || r == 0x1680 || r == 0x2028 || r == 0x2029 ||
		r == 0x202F || r == 0x205F || r == 0x3000 || r == 0xFEFF
}

// --- @format Implementations ---

func jqValToText(v jqVal) string {
	if v.kind == jqString {
		return v.str
	}
	var buf bytes.Buffer
	jqWriteCompact(&buf, v, false)
	return buf.String()
}

func jqApplyFormat(fmtName string, v jqVal) (string, error) {
	switch fmtName {
	case "@text":
		return jqValToText(v), nil

	case "@json":
		var buf bytes.Buffer
		jqWriteCompact(&buf, v, false)
		return buf.String(), nil

	case "@base64":
		return base64.StdEncoding.EncodeToString([]byte(jqValToText(v))), nil

	case "@base64d":
		if v.kind != jqString {
			return "", jqErrStr(v.shortDesc() + " is not a valid base64 string")
		}
		b, err := base64.StdEncoding.DecodeString(v.str)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(v.str, "="))
			if err != nil {
				return "", jqErrStr("invalid base64 data")
			}
		}
		return string(b), nil

	case "@uri":
		s := jqValToText(v)
		const hex = "0123456789ABCDEF"
		var sb strings.Builder
		sb.Grow(len(s))
		for i := 0; i < len(s); i++ {
			c := s[i]
			if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
				c == '-' || c == '_' || c == '.' || c == '~' {
				sb.WriteByte(c)
			} else {
				sb.WriteByte('%')
				sb.WriteByte(hex[c>>4])
				sb.WriteByte(hex[c&0x0f])
			}
		}
		return sb.String(), nil

	case "@urid":
		if v.kind != jqString {
			return "", jqErrStr(v.shortDesc() + " cannot be uri-decoded")
		}
		s := v.str
		out := make([]byte, 0, len(s))
		for i := 0; i < len(s); i++ {
			if s[i] == '%' {
				if i+2 >= len(s) {
					return "", jqErrStr("invalid uri encoding")
				}
				hi, ok1 := fromHexDigit(s[i+1])
				lo, ok2 := fromHexDigit(s[i+2])
				if !ok1 || !ok2 {
					return "", jqErrStr("invalid uri encoding")
				}
				out = append(out, (hi<<4)|lo)
				i += 2
			} else {
				out = append(out, s[i])
			}
		}
		if !utf8.Valid(out) {
			return "", jqErrStr("decoded uri is not valid UTF-8")
		}
		return string(out), nil

	case "@html":
		s := jqValToText(v)
		var sb strings.Builder
		sb.Grow(len(s))
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '&':
				sb.WriteString("&amp;")
			case '<':
				sb.WriteString("&lt;")
			case '>':
				sb.WriteString("&gt;")
			case '\'':
				sb.WriteString("&apos;")
			case '"':
				sb.WriteString("&quot;")
			default:
				sb.WriteByte(s[i])
			}
		}
		return sb.String(), nil

	case "@sh":
		if v.kind == jqArray {
			parts := make([]string, len(v.arr))
			for i, el := range v.arr {
				s, err := formatShScalar(el)
				if err != nil {
					return "", err
				}
				parts[i] = s
			}
			return strings.Join(parts, " "), nil
		}
		return formatShScalar(v)

	case "@csv":
		if v.kind != jqArray {
			return "", jqErrStr(v.shortDesc() + " cannot be csv-formatted, only array")
		}
		var sb strings.Builder
		for i, el := range v.arr {
			if i > 0 {
				sb.WriteByte(',')
			}
			switch el.kind {
			case jqNull:
			case jqBool:
				if el.b {
					sb.WriteString("true")
				} else {
					sb.WriteString("false")
				}
			case jqNumber:
				sb.WriteString(jqFormatNumV(el))
			case jqString:
				sb.WriteByte('"')
				sb.WriteString(strings.ReplaceAll(el.str, `"`, `""`))
				sb.WriteByte('"')
			default:
				return "", jqErrStr(el.shortDesc() + " is not valid in a csv row")
			}
		}
		return sb.String(), nil

	case "@tsv":
		if v.kind != jqArray {
			return "", jqErrStr(v.shortDesc() + " cannot be tsv-formatted, only array")
		}
		var sb strings.Builder
		for i, el := range v.arr {
			if i > 0 {
				sb.WriteByte('\t')
			}
			switch el.kind {
			case jqNull:
			case jqBool:
				if el.b {
					sb.WriteString("true")
				} else {
					sb.WriteString("false")
				}
			case jqNumber:
				sb.WriteString(jqFormatNumV(el))
			case jqString:
				for j := 0; j < len(el.str); j++ {
					switch el.str[j] {
					case '\t':
						sb.WriteString(`\t`)
					case '\r':
						sb.WriteString(`\r`)
					case '\n':
						sb.WriteString(`\n`)
					case '\\':
						sb.WriteString(`\\`)
					case 0:
						sb.WriteString(`\0`)
					default:
						sb.WriteByte(el.str[j])
					}
				}
			default:
				return "", jqErrStr(el.shortDesc() + " is not valid in a tsv row")
			}
		}
		return sb.String(), nil
	}
	return "", jqErrStr(fmtName + " is not a valid format")
}

func formatShScalar(el jqVal) (string, error) {
	switch el.kind {
	case jqNull:
		return "null", nil
	case jqBool:
		if el.b {
			return "true", nil
		}
		return "false", nil
	case jqNumber:
		return jqFormatNumV(el), nil
	case jqString:
		return "'" + strings.ReplaceAll(el.str, "'", `'\''`) + "'", nil
	default:
		return "", jqErrStr(el.shortDesc() + " cannot be escaped for shell")
	}
}

func fromHexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// --- Regex Compilation & Caching ---

type jqCompiledRegex struct {
	re          *regexp.Regexp
	global      bool
	ignoreEmpty bool
}

var (
	jqRegexMu    sync.Mutex
	jqRegexCache = make(map[string]*jqCompiledRegex, 64)
)

func jqCompileRegex(pattern, flags string) (*jqCompiledRegex, error) {
	key := flags + "\x00" + pattern
	jqRegexMu.Lock()
	if cached, ok := jqRegexCache[key]; ok {
		jqRegexMu.Unlock()
		return cached, nil
	}
	jqRegexMu.Unlock()

	var global, ignoreEmpty, flagI, flagM, flagS, flagX bool
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case 'g':
			global = true
		case 'n':
			ignoreEmpty = true
		case 'i':
			flagI = true
		case 'm', 'l':
			flagM = true
		case 's':
			flagS = true
		case 'p':
			flagM, flagS = true, true
		case 'x':
			flagX = true
		default:
			return nil, jqErrStr("unsupported regex flag")
		}
	}

	pat := pattern
	if flagX {
		pat = stripExtendedRegexWS(pat)
	}
	var prefix strings.Builder
	if flagI || flagM || flagS {
		prefix.WriteString("(?")
		if flagI {
			prefix.WriteByte('i')
		}
		if flagM {
			prefix.WriteByte('m')
		}
		if flagS {
			prefix.WriteByte('s')
		}
		prefix.WriteByte(')')
	}
	re, err := regexp.Compile(prefix.String() + pat)
	if err != nil {
		return nil, jqErrStr("Regex failure: " + err.Error())
	}
	compiled := &jqCompiledRegex{re: re, global: global, ignoreEmpty: ignoreEmpty}

	jqRegexMu.Lock()
	if len(jqRegexCache) >= 256 {
		clear(jqRegexCache)
	}
	jqRegexCache[key] = compiled
	jqRegexMu.Unlock()
	return compiled, nil
}

func stripExtendedRegexWS(pat string) string {
	var sb strings.Builder
	inClass := false
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		if c == '\\' && i+1 < len(pat) {
			sb.WriteByte(c)
			sb.WriteByte(pat[i+1])
			i++
			continue
		}
		if c == '[' && !inClass {
			inClass = true
			sb.WriteByte(c)
			continue
		}
		if c == ']' && inClass {
			inClass = false
			sb.WriteByte(c)
			continue
		}
		if !inClass {
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				continue
			}
			if c == '#' {
				for i < len(pat) && pat[i] != '\n' {
					i++
				}
				continue
			}
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

func buildByteToRuneTable(s string) []int {
	tbl := make([]int, len(s)+1)
	rIdx := 0
	for i := 0; i < len(s); {
		tbl[i] = rIdx
		_, w := utf8.DecodeRuneInString(s[i:])
		for k := 1; k < w && i+k <= len(s); k++ {
			tbl[i+k] = rIdx
		}
		i += w
		rIdx++
	}
	tbl[len(s)] = rIdx
	return tbl
}

func jqBuildMatchVal(s string, loc []int, subNames []string, byteToRune []int) jqVal {
	startB, endB := loc[0], loc[1]
	offsetR := byteToRune[startB]
	lenR := byteToRune[endB] - offsetR

	numGroups := len(loc)/2 - 1
	caps := make([]jqVal, numGroups)
	for g := 1; g <= numGroups; g++ {
		gs, ge := loc[2*g], loc[2*g+1]
		nameVal := jqMakeNull()
		if g < len(subNames) && subNames[g] != "" {
			nameVal = jqMakeStr(subNames[g])
		}
		if gs < 0 {
			caps[g-1] = jqMakeObj([]jqEntry{
				{key: "offset", val: jqMakeInt(-1)},
				{key: "length", val: jqMakeInt(0)},
				{key: "string", val: jqMakeNull()},
				{key: "name", val: nameVal},
			})
		} else {
			goff := byteToRune[gs]
			caps[g-1] = jqMakeObj([]jqEntry{
				{key: "offset", val: jqMakeInt(int64(goff))},
				{key: "length", val: jqMakeInt(int64(byteToRune[ge] - goff))},
				{key: "string", val: jqMakeStr(s[gs:ge])},
				{key: "name", val: nameVal},
			})
		}
	}
	return jqMakeObj([]jqEntry{
		{key: "offset", val: jqMakeInt(int64(offsetR))},
		{key: "length", val: jqMakeInt(int64(lenR))},
		{key: "string", val: jqMakeStr(s[startB:endB])},
		{key: "captures", val: jqMakeArr(caps)},
	})
}

func jqBuildCaptureObj(s string, loc []int, subNames []string) jqVal {
	obj := newJqObj(len(subNames))
	numGroups := len(loc)/2 - 1
	for g := 1; g <= numGroups; g++ {
		if g >= len(subNames) || subNames[g] == "" {
			continue
		}
		gs, ge := loc[2*g], loc[2*g+1]
		if gs < 0 {
			obj.set(subNames[g], jqMakeNull())
		} else {
			obj.set(subNames[g], jqMakeStr(s[gs:ge]))
		}
	}
	return jqVal{kind: jqObject, obj: obj}
}

func extractRegexSpec(rv jqVal, extraFlags string) (string, string, error) {
	if rv.kind == jqString {
		return rv.str, extraFlags, nil
	}
	if rv.kind == jqArray && len(rv.arr) >= 1 && len(rv.arr) <= 2 && rv.arr[0].kind == jqString {
		fl := ""
		if len(rv.arr) == 2 && rv.arr[1].kind == jqString {
			fl = rv.arr[1].str
		}
		return rv.arr[0].str, fl + extraFlags, nil
	}
	return "", "", jqErrStr("regex pattern must be a string or [regex, flags] array")
}

// --- Builtin Dispatcher ---

func (st *jqEvalState) evalBuiltin(name string, args []*jqNode, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	arity := len(args)

	if arity == 0 {
		if res, ok, err := jqEvalZeroArgSimple(name, v); ok {
			if err != nil {
				return err
			}
			return emit(res)
		}
	}

	switch name {
	case "empty":
		if arity == 0 {
			return nil
		}

	case "error":
		if arity == 0 {
			return &jqRuntimeError{val: v}
		}

	case "halt":
		if arity == 0 {
			return &jqHaltError{code: 0, err: jqMakeNull()}
		}

	case "halt_error":
		if arity == 0 {
			return &jqHaltError{code: 5, err: v}
		}
		if arity == 1 {
			vals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			code := 5
			if len(vals) > 0 && vals[0].kind == jqNumber {
				if ci, ok := vals[0].num.asInt64(); ok {
					code = int(ci)
				}
			}
			return &jqHaltError{code: code, err: v}
		}

	case "debug":
		if arity == 0 {
			if st.stderr != nil {
				var buf bytes.Buffer
				jqWriteCompact(&buf, jqMakeArr([]jqVal{jqMakeStr("DEBUG:"), v}), false)
				buf.WriteByte('\n')
				_, _ = st.stderr.Write(buf.Bytes())
			}
			return emit(v)
		}

	case "stderr":
		if arity == 0 {
			if st.stderr != nil {
				_, _ = io.WriteString(st.stderr, jqValToText(v))
			}
			return emit(v)
		}

	case "has", "contains", "ltrimstr", "rtrimstr", "trimstr", "startswith", "endswith", "bsearch":
		if arity == 1 {
			avals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			for _, av := range avals {
				res, berr := jqEvalBinarySimpleBuiltin(name, v, av)
				if berr != nil {
					return berr
				}
				if err := emit(res); err != nil {
					return err
				}
			}
			return nil
		}

	case "path":
		if arity == 1 {
			return st.pathOf(args[0], v, nil, env, func(p []jqPathStep, _ jqVal) error {
				return emit(jqPathStepsToVal(p))
			})
		}

	case "getpath":
		if arity == 1 {
			pvals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			for _, pv := range pvals {
				if pv.kind != jqArray {
					return jqErrStr("Path must be specified as an array")
				}
				steps, serr := jqValSliceToPathSteps(pv.arr)
				if serr != nil {
					return jqErrStr(serr.Error())
				}
				res, gerr := jqGetPath(v, steps)
				if gerr != nil {
					return jqErrStr(gerr.Error())
				}
				if err := emit(res); err != nil {
					return err
				}
			}
			return nil
		}

	case "setpath":
		if arity == 2 {
			pvals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			nvals, err := st.evalList(args[1], v, env)
			if err != nil {
				return err
			}
			for _, pv := range pvals {
				if pv.kind != jqArray {
					return jqErrStr("Path must be specified as an array")
				}
				steps, serr := jqValSliceToPathSteps(pv.arr)
				if serr != nil {
					return jqErrStr(serr.Error())
				}
				for _, nv := range nvals {
					res, uerr := jqSetPath(v.clone(), steps, nv)
					if uerr != nil {
						return jqErrStr(uerr.Error())
					}
					if err := emit(res); err != nil {
						return err
					}
				}
			}
			return nil
		}

	case "delpaths":
		if arity == 1 {
			psvals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			for _, psv := range psvals {
				if psv.kind != jqArray {
					return jqErrStr("Paths must be specified as an array")
				}
				paths := make([][]jqPathStep, len(psv.arr))
				for i, pel := range psv.arr {
					if pel.kind != jqArray {
						return jqErrStr("Path must be specified as an array")
					}
					stps, serr := jqValSliceToPathSteps(pel.arr)
					if serr != nil {
						return jqErrStr(serr.Error())
					}
					paths[i] = stps
				}
				res, derr := jqDelPaths(v, paths)
				if derr != nil {
					return jqErrStr(derr.Error())
				}
				if err := emit(res); err != nil {
					return err
				}
			}
			return nil
		}

	case "flatten":
		if arity == 1 {
			if v.kind != jqArray {
				return jqErrStr("Cannot flatten non-array " + v.shortDesc())
			}
			dvals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			for _, dv := range dvals {
				if dv.kind != jqNumber {
					return jqErrStr("flatten depth must be a number")
				}
				df := dv.num.asFloat()
				if df < 0 {
					return jqErrStr("flatten depth must not be negative")
				}
				if err := emit(jqMakeArr(jqFlattenSlice(v.arr, int(df)))); err != nil {
					return err
				}
			}
			return nil
		}

	case "range":
		return st.evalRange(args, v, env, emit)

	case "sort_by", "unique_by", "group_by":
		if arity == 1 {
			sorted, keys, err := st.sortByKeyExpr(v, args[0], env)
			if err != nil {
				return err
			}
			if name == "sort_by" {
				return emit(jqMakeArr(sorted))
			}
			if name == "unique_by" {
				out := make([]jqVal, 0, len(sorted))
				for i, el := range sorted {
					if i == 0 || !jqEqual(keys[i-1], keys[i]) {
						out = append(out, el)
					}
				}
				return emit(jqMakeArr(out))
			}
			if len(sorted) == 0 {
				return emit(jqMakeArr(nil))
			}
			var groups []jqVal
			curGroup := []jqVal{sorted[0]}
			for i := 1; i < len(sorted); i++ {
				if jqEqual(keys[i-1], keys[i]) {
					curGroup = append(curGroup, sorted[i])
				} else {
					groups = append(groups, jqMakeArr(curGroup))
					curGroup = []jqVal{sorted[i]}
				}
			}
			groups = append(groups, jqMakeArr(curGroup))
			return emit(jqMakeArr(groups))
		}

	case "min_by", "max_by":
		if arity == 1 {
			if v.kind != jqArray {
				return jqErrStr(v.shortDesc() + " cannot be searched, as it is not an array")
			}
			if len(v.arr) == 0 {
				return emit(jqMakeNull())
			}
			keys, err := st.computeTupleKeys(v.arr, args[0], env)
			if err != nil {
				return err
			}
			bestIdx := 0
			for i := 1; i < len(v.arr); i++ {
				c := jqCompare(keys[i], keys[bestIdx])
				if (name == "min_by" && c < 0) || (name == "max_by" && c > 0) {
					bestIdx = i
				}
			}
			return emit(v.arr[bestIdx])
		}

	case "indices", "index", "rindex":
		if arity == 1 {
			xvals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			for _, xv := range xvals {
				idxs, ierr := jqFindIndices(v, xv)
				if ierr != nil {
					return ierr
				}
				switch name {
				case "indices":
					out := make([]jqVal, len(idxs))
					for i, k := range idxs {
						out[i] = jqMakeInt(int64(k))
					}
					if err := emit(jqMakeArr(out)); err != nil {
						return err
					}
				case "index":
					if len(idxs) == 0 {
						err = emit(jqMakeNull())
					} else {
						err = emit(jqMakeInt(int64(idxs[0])))
					}
					if err != nil {
						return err
					}
				case "rindex":
					if len(idxs) == 0 {
						err = emit(jqMakeNull())
					} else {
						err = emit(jqMakeInt(int64(idxs[len(idxs)-1])))
					}
					if err != nil {
						return err
					}
				}
			}
			return nil
		}

	case "split":
		if arity == 1 {
			svals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			for _, sv := range svals {
				if v.kind != jqString || sv.kind != jqString {
					return jqErrStr("split input and separator must be strings")
				}
				if v.str == "" && sv.str == "" {
					if err := emit(jqMakeArr(nil)); err != nil {
						return err
					}
					continue
				}
				parts := strings.Split(v.str, sv.str)
				out := make([]jqVal, len(parts))
				for i, p := range parts {
					out[i] = jqMakeStr(p)
				}
				if err := emit(jqMakeArr(out)); err != nil {
					return err
				}
			}
			return nil
		}
		if arity == 2 {
			return st.evalRegexSplit(args[0], args[1], true, v, env, emit)
		}

	case "join":
		if arity == 1 {
			if v.kind != jqArray {
				return jqErrStr("Cannot iterate over " + v.shortDesc())
			}
			svals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			for _, sv := range svals {
				if sv.kind != jqString {
					return jqErrStr("join separator must be a string")
				}
				var sb strings.Builder
				for i, el := range v.arr {
					if i > 0 {
						sb.WriteString(sv.str)
					}
					switch el.kind {
					case jqNull:
					case jqString:
						sb.WriteString(el.str)
					case jqNumber:
						sb.WriteString(jqFormatNumV(el))
					case jqBool:
						if el.b {
							sb.WriteString("true")
						} else {
							sb.WriteString("false")
						}
					default:
						return jqErrStr("string and " + el.shortDesc() + " cannot be added")
					}
				}
				if err := emit(jqMakeStr(sb.String())); err != nil {
					return err
				}
			}
			return nil
		}

	case "test":
		if arity == 1 || arity == 2 {
			return st.evalRegexTest(args, v, env, emit)
		}

	case "match", "capture":
		if arity == 1 || arity == 2 {
			return st.evalRegexMatch(args, name == "capture", v, env, emit)
		}

	case "scan":
		if arity == 1 || arity == 2 {
			return st.evalRegexScan(args, v, env, emit)
		}

	case "sub", "gsub":
		if arity == 2 || arity == 3 {
			return st.evalRegexSub(args, name == "gsub", v, env, emit)
		}

	case "pow":
		if arity == 2 {
			xvals, err := st.evalList(args[0], v, env)
			if err != nil {
				return err
			}
			yvals, err := st.evalList(args[1], v, env)
			if err != nil {
				return err
			}
			for _, xv := range xvals {
				if xv.kind != jqNumber {
					return jqErrStr("pow requires numeric arguments")
				}
				for _, yv := range yvals {
					if yv.kind != jqNumber {
						return jqErrStr("pow requires numeric arguments")
					}
					if err := emit(jqMakeFloat(math.Pow(xv.num.asFloat(), yv.num.asFloat()))); err != nil {
						return err
					}
				}
			}
			return nil
		}

	case "while":
		if arity == 2 {
			return st.evalWhile(v, args[0], args[1], env, emit)
		}

	case "until":
		if arity == 2 {
			return st.evalUntil(v, args[0], args[1], env, emit)
		}

	case "input":
		if arity == 0 {
			if st.nextInput == nil {
				return jqErrStr("break")
			}
			iv, err := st.nextInput()
			if err == io.EOF {
				return jqErrStr("break")
			}
			if err != nil {
				return jqErrStr(err.Error())
			}
			return emit(iv)
		}

	case "inputs":
		if arity == 0 {
			if st.nextInput == nil {
				return nil
			}
			for {
				if err := st.tick(); err != nil {
					return err
				}
				iv, err := st.nextInput()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return jqErrStr(err.Error())
				}
				if err := emit(iv); err != nil {
					return err
				}
			}
		}
	}

	return jqErrStr(name + "/" + strconv.Itoa(arity) + " is not defined")
}

// jqEvalZeroArgSimple handles pure 0-argument value->value builtins without closures.
func jqEvalZeroArgSimple(name string, v jqVal) (jqVal, bool, error) {
	switch name {
	case "not":
		return jqMakeBool(!v.isTruthy()), true, nil
	case "type":
		return jqMakeStr(v.typeName()), true, nil
	case "length":
		switch v.kind {
		case jqNull:
			return jqMakeInt(0), true, nil
		case jqString:
			return jqMakeInt(int64(utf8.RuneCountInString(v.str))), true, nil
		case jqArray:
			return jqMakeInt(int64(len(v.arr))), true, nil
		case jqObject:
			if v.obj == nil {
				return jqMakeInt(0), true, nil
			}
			return jqMakeInt(int64(len(v.obj.entries))), true, nil
		case jqNumber:
			if v.num.isInt() && v.num.i != math.MinInt64 {
				if v.num.i < 0 {
					return jqMakeInt(-v.num.i), true, nil
				}
				return v, true, nil
			}
			return jqMakeFloat(math.Abs(v.num.asFloat())), true, nil
		default:
			return jqMakeNull(), true, jqErrStr(v.shortDesc() + " has no length")
		}
	case "utf8bytelength":
		if v.kind != jqString {
			return jqMakeNull(), true, jqErrStr(v.shortDesc() + " only strings have UTF-8 byte length")
		}
		return jqMakeInt(int64(len(v.str))), true, nil
	case "keys", "keys_unsorted":
		switch v.kind {
		case jqArray:
			out := make([]jqVal, len(v.arr))
			for i := range v.arr {
				out[i] = jqMakeInt(int64(i))
			}
			return jqMakeArr(out), true, nil
		case jqObject:
			if v.obj == nil || len(v.obj.entries) == 0 {
				return jqMakeArr(nil), true, nil
			}
			ks := make([]string, len(v.obj.entries))
			for i, e := range v.obj.entries {
				ks[i] = e.key
			}
			if name == "keys" {
				sort.Strings(ks)
			}
			out := make([]jqVal, len(ks))
			for i, k := range ks {
				out[i] = jqMakeStr(k)
			}
			return jqMakeArr(out), true, nil
		default:
			return jqMakeNull(), true, jqErrStr(v.shortDesc() + " has no keys")
		}
	case "add":
		if v.kind != jqArray {
			return jqMakeNull(), true, jqErrStr("Cannot iterate over " + v.shortDesc())
		}
		acc := jqMakeNull()
		for _, el := range v.arr {
			var err error
			acc, err = jqApplyBinOp(jqTokPlus, acc, el)
			if err != nil {
				return jqMakeNull(), true, err
			}
		}
		return acc, true, nil
	case "flatten":
		if v.kind != jqArray {
			return jqMakeNull(), true, jqErrStr("Cannot flatten non-array " + v.shortDesc())
		}
		return jqMakeArr(jqFlattenSlice(v.arr, -1)), true, nil
	case "reverse":
		if v.kind != jqArray {
			return jqMakeNull(), true, jqErrStr("Cannot reverse " + v.shortDesc())
		}
		n := len(v.arr)
		out := make([]jqVal, n)
		for i := 0; i < n; i++ {
			out[i] = v.arr[n-1-i]
		}
		return jqMakeArr(out), true, nil
	case "sort", "unique":
		if v.kind != jqArray {
			return jqMakeNull(), true, jqErrStr(v.shortDesc() + " cannot be sorted, as it is not an array")
		}
		cp := make([]jqVal, len(v.arr))
		copy(cp, v.arr)
		sort.SliceStable(cp, func(i, j int) bool { return jqCompare(cp[i], cp[j]) < 0 })
		if name == "sort" {
			return jqMakeArr(cp), true, nil
		}
		out := make([]jqVal, 0, len(cp))
		for i, el := range cp {
			if i == 0 || !jqEqual(cp[i-1], el) {
				out = append(out, el)
			}
		}
		return jqMakeArr(out), true, nil
	case "min", "max":
		if v.kind != jqArray {
			return jqMakeNull(), true, jqErrStr(v.shortDesc() + " is not an array")
		}
		if len(v.arr) == 0 {
			return jqMakeNull(), true, nil
		}
		best := v.arr[0]
		want := -1
		if name == "max" {
			want = 1
		}
		for i := 1; i < len(v.arr); i++ {
			if c := jqCompare(v.arr[i], best); (want < 0 && c < 0) || (want > 0 && c > 0) {
				best = v.arr[i]
			}
		}
		return best, true, nil
	case "tonumber":
		if v.kind == jqNumber {
			return v, true, nil
		}
		if v.kind == jqString {
			s := strings.TrimSpace(v.str)
			if s != "" {
				if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
					return jqMakeInt(iv), true, nil
				}
				if fv, err := strconv.ParseFloat(s, 64); err == nil {
					return jqMakeFloat(fv), true, nil
				}
			}
		}
		return jqMakeNull(), true, jqErrStr(v.shortDesc() + " cannot be parsed as a number")
	case "toboolean":
		if v.kind == jqBool {
			return v, true, nil
		}
		if v.kind == jqString && (v.str == "true" || v.str == "false") {
			return jqMakeBool(v.str == "true"), true, nil
		}
		return jqMakeNull(), true, jqErrStr(v.shortDesc() + " cannot be parsed as a boolean")
	case "tostring":
		return jqMakeStr(jqValToText(v)), true, nil
	case "tojson":
		var buf bytes.Buffer
		jqWriteCompact(&buf, v, false)
		return jqMakeStr(buf.String()), true, nil
	case "fromjson":
		if v.kind != jqString {
			return jqMakeNull(), true, jqErrStr(v.shortDesc() + " only strings can be parsed")
		}
		switch strings.ToLower(strings.TrimSpace(v.str)) {
		case "nan", "+nan", "-nan":
			return jqMakeFloat(math.NaN()), true, nil
		case "inf", "+inf", "infinity", "+infinity":
			return jqMakeFloat(math.Inf(1)), true, nil
		case "-inf", "-infinity":
			return jqMakeFloat(math.Inf(-1)), true, nil
		}
		parsed, err := jqParseSingleJSON(v.str)
		if err != nil {
			return jqMakeNull(), true, jqErrStr(err.Error())
		}
		return parsed, true, nil
	case "explode":
		if v.kind != jqString {
			return jqMakeNull(), true, jqErrStr("explode input must be a string")
		}
		runes := []rune(v.str)
		out := make([]jqVal, len(runes))
		for i, r := range runes {
			out[i] = jqMakeInt(int64(r))
		}
		return jqMakeArr(out), true, nil
	case "implode":
		if v.kind != jqArray {
			return jqMakeNull(), true, jqErrStr("implode input must be an array")
		}
		var sb strings.Builder
		for _, el := range v.arr {
			if el.kind != jqNumber {
				return jqMakeNull(), true, jqErrStr("Cannot implode non-number")
			}
			cp, ok := el.num.asInt64()
			if !ok || cp < 0 || cp > utf8.MaxRune || isUTF16Surrogate(rune(cp)) {
				sb.WriteRune(utf8.RuneError)
			} else {
				sb.WriteRune(rune(cp))
			}
		}
		return jqMakeStr(sb.String()), true, nil
	case "ascii_downcase", "ascii_upcase":
		if v.kind != jqString {
			return jqMakeNull(), true, jqErrStr(name + " input must be a string")
		}
		b := []byte(v.str)
		for i, c := range b {
			if name == "ascii_downcase" && c >= 'A' && c <= 'Z' {
				b[i] = c + ('a' - 'A')
			} else if name == "ascii_upcase" && c >= 'a' && c <= 'z' {
				b[i] = c - ('a' - 'A')
			}
		}
		return jqMakeStr(string(b)), true, nil
	case "trim", "ltrim", "rtrim":
		if v.kind != jqString {
			return jqMakeNull(), true, jqErrStr(name + " input must be a string")
		}
		switch name {
		case "trim":
			return jqMakeStr(strings.TrimFunc(v.str, isJqSpace)), true, nil
		case "ltrim":
			return jqMakeStr(strings.TrimLeftFunc(v.str, isJqSpace)), true, nil
		default:
			return jqMakeStr(strings.TrimRightFunc(v.str, isJqSpace)), true, nil
		}
	case "transpose":
		if v.kind != jqArray {
			return jqMakeNull(), true, jqErrStr("transpose requires an array of arrays")
		}
		maxCols := 0
		for _, row := range v.arr {
			if row.kind != jqArray {
				return jqMakeNull(), true, jqErrStr("Cannot index " + row.typeName() + " with number")
			}
			if len(row.arr) > maxCols {
				maxCols = len(row.arr)
			}
		}
		out := make([]jqVal, maxCols)
		for c := 0; c < maxCols; c++ {
			col := make([]jqVal, len(v.arr))
			for r, row := range v.arr {
				if c < len(row.arr) {
					col[r] = row.arr[c]
				} else {
					col[r] = jqMakeNull()
				}
			}
			out[c] = jqMakeArr(col)
		}
		return jqMakeArr(out), true, nil
	case "floor", "ceil", "round", "trunc", "sqrt", "abs", "fabs",
		"log", "log10", "log2", "exp", "exp2", "sin", "cos", "tan", "asin", "acos", "atan":
		if v.kind != jqNumber {
			return jqMakeNull(), true, jqErrStr(v.typeName() + " number required")
		}
		return jqEvalUnaryMath(name, v.num), true, nil
	case "isnan":
		if v.kind != jqNumber {
			return jqMakeNull(), true, jqErrStr("number required")
		}
		return jqMakeBool(!v.num.isInt() && math.IsNaN(v.num.f)), true, nil
	case "isinfinite":
		return jqMakeBool(v.kind == jqNumber && !v.num.isInt() && math.IsInf(v.num.f, 0)), true, nil
	case "isfinite":
		return jqMakeBool(v.kind == jqNumber && (v.num.isInt() || (!math.IsNaN(v.num.f) && !math.IsInf(v.num.f, 0)))), true, nil
	case "isnormal":
		if v.kind != jqNumber {
			return jqMakeBool(false), true, nil
		}
		f := v.num.asFloat()
		return jqMakeBool(f != 0 && !math.IsNaN(f) && !math.IsInf(f, 0)), true, nil
	case "nan":
		return jqMakeFloat(math.NaN()), true, nil
	case "infinite":
		return jqMakeFloat(math.Inf(1)), true, nil
	case "builtins":
		list := jqBuiltinSignatureList()
		out := make([]jqVal, len(list))
		for i, s := range list {
			out[i] = jqMakeStr(s)
		}
		return jqMakeArr(out), true, nil
	}
	return jqMakeNull(), false, nil
}

func jqEvalBinarySimpleBuiltin(name string, v, av jqVal) (jqVal, error) {
	switch name {
	case "has":
		return jqCheckHas(v, av)
	case "contains":
		return jqMakeBool(jqContains(v, av)), nil
	case "ltrimstr", "rtrimstr", "trimstr", "startswith", "endswith":
		if v.kind != jqString || av.kind != jqString {
			return jqMakeNull(), jqErrStr(name + " requires string inputs")
		}
		switch name {
		case "ltrimstr":
			return jqMakeStr(strings.TrimPrefix(v.str, av.str)), nil
		case "rtrimstr":
			return jqMakeStr(strings.TrimSuffix(v.str, av.str)), nil
		case "trimstr":
			return jqMakeStr(strings.TrimSuffix(strings.TrimPrefix(v.str, av.str), av.str)), nil
		case "startswith":
			return jqMakeBool(strings.HasPrefix(v.str, av.str)), nil
		default:
			return jqMakeBool(strings.HasSuffix(v.str, av.str)), nil
		}
	case "bsearch":
		if v.kind != jqArray {
			return jqMakeNull(), jqErrStr(v.shortDesc() + " cannot be searched from")
		}
		lo, hi := 0, len(v.arr)-1
		for lo <= hi {
			mid := int(uint(lo+hi) >> 1)
			c := jqCompare(v.arr[mid], av)
			if c == 0 {
				return jqMakeInt(int64(mid)), nil
			}
			if c < 0 {
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		return jqMakeInt(int64(-1 - lo)), nil
	}
	return jqMakeNull(), jqErrStr("unknown builtin")
}

func jqCheckHas(container, key jqVal) (jqVal, error) {
	switch container.kind {
	case jqObject:
		if key.kind != jqString {
			return jqMakeNull(), jqErrStr("Cannot check whether object has a " + key.typeName() + " key")
		}
		_, ok := container.obj.get(key.str)
		return jqMakeBool(ok), nil
	case jqArray:
		if key.kind != jqNumber {
			return jqMakeNull(), jqErrStr("Cannot check whether array has a " + key.typeName() + " key")
		}
		idx, ok := key.num.asInt64()
		return jqMakeBool(ok && idx >= 0 && idx < int64(len(container.arr))), nil
	case jqNull:
		return jqMakeBool(false), nil
	default:
		return jqMakeNull(), jqErrStr("Cannot check whether " + container.typeName() + " has a " + key.typeName() + " key")
	}
}

// --- Builtin Helpers ---

func jqFlattenSlice(arr []jqVal, depth int) []jqVal {
	out := make([]jqVal, 0, len(arr))
	for _, el := range arr {
		if el.kind == jqArray && depth != 0 {
			nextDepth := depth
			if nextDepth > 0 {
				nextDepth--
			}
			out = append(out, jqFlattenSlice(el.arr, nextDepth)...)
		} else {
			out = append(out, el)
		}
	}
	return out
}

func (st *jqEvalState) runRangeLoop(from, upto, step jqNum, emit func(jqVal) error) error {
	if step.isInt() && from.isInt() && upto.isInt() {
		s := step.i
		if s == 0 {
			return nil
		}
		for cur := from.i; (s > 0 && cur < upto.i) || (s < 0 && cur > upto.i); cur += s {
			if err := st.tick(); err != nil {
				return err
			}
			if err := emit(jqMakeInt(cur)); err != nil {
				return err
			}
		}
		return nil
	}
	sf := step.asFloat()
	if sf == 0 || math.IsNaN(sf) {
		return nil
	}
	uf := upto.asFloat()
	for cur := from.asFloat(); (sf > 0 && cur < uf) || (sf < 0 && cur > uf); cur += sf {
		if err := st.tick(); err != nil {
			return err
		}
		if err := emit(jqMakeFloat(cur)); err != nil {
			return err
		}
	}
	return nil
}

func (st *jqEvalState) evalRange(args []*jqNode, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	zero := []jqVal{jqMakeInt(0)}
	one := []jqVal{jqMakeInt(1)}
	var froms, uptos, steps []jqVal
	var err error
	switch len(args) {
	case 1:
		froms, steps = zero, one
		uptos, err = st.evalList(args[0], v, env)
	case 2:
		steps = one
		froms, err = st.evalList(args[0], v, env)
		if err == nil {
			uptos, err = st.evalList(args[1], v, env)
		}
	case 3:
		froms, err = st.evalList(args[0], v, env)
		if err == nil {
			uptos, err = st.evalList(args[1], v, env)
		}
		if err == nil {
			steps, err = st.evalList(args[2], v, env)
		}
	default:
		return jqErrStr("invalid range arity")
	}
	if err != nil {
		return err
	}
	for _, fv := range froms {
		if fv.kind != jqNumber {
			return jqErrStr("Range bounds must be numeric")
		}
		for _, uv := range uptos {
			if uv.kind != jqNumber {
				return jqErrStr("Range bounds must be numeric")
			}
			for _, sv := range steps {
				if sv.kind != jqNumber {
					return jqErrStr("Range step must be numeric")
				}
				if err := st.runRangeLoop(fv.num, uv.num, sv.num, emit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (st *jqEvalState) computeTupleKeys(arr []jqVal, keyExpr *jqNode, env *jqEnv) ([]jqVal, error) {
	keys := make([]jqVal, len(arr))
	for i, el := range arr {
		tuple, err := st.evalList(keyExpr, el, env)
		if err != nil {
			return nil, err
		}
		cp := make([]jqVal, len(tuple))
		copy(cp, tuple)
		keys[i] = jqMakeArr(cp)
	}
	return keys, nil
}

func (st *jqEvalState) sortByKeyExpr(v jqVal, keyExpr *jqNode, env *jqEnv) ([]jqVal, []jqVal, error) {
	if v.kind != jqArray {
		return nil, nil, jqErrStr(v.shortDesc() + " cannot be sorted, as it is not an array")
	}
	n := len(v.arr)
	if n == 0 {
		return nil, nil, nil
	}
	keys, err := st.computeTupleKeys(v.arr, keyExpr, env)
	if err != nil {
		return nil, nil, err
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		return jqCompare(keys[order[i]], keys[order[j]]) < 0
	})
	sortedVals := make([]jqVal, n)
	sortedKeys := make([]jqVal, n)
	for i, idx := range order {
		sortedVals[i] = v.arr[idx]
		sortedKeys[i] = keys[idx]
	}
	return sortedVals, sortedKeys, nil
}

func jqFindIndices(haystack, needle jqVal) ([]int, error) {
	switch haystack.kind {
	case jqString:
		if needle.kind != jqString {
			return nil, jqErrStr("Cannot search string for " + needle.shortDesc())
		}
		if needle.str == "" {
			return nil, nil
		}
		hr, nr := []rune(haystack.str), []rune(needle.str)
		var idxs []int
		for i := 0; i <= len(hr)-len(nr); i++ {
			match := true
			for j := 0; j < len(nr); j++ {
				if hr[i+j] != nr[j] {
					match = false
					break
				}
			}
			if match {
				idxs = append(idxs, i)
			}
		}
		return idxs, nil

	case jqArray:
		if needle.kind == jqArray {
			if len(needle.arr) == 0 || len(needle.arr) > len(haystack.arr) {
				return nil, nil
			}
			var idxs []int
			for i := 0; i <= len(haystack.arr)-len(needle.arr); i++ {
				match := true
				for j := 0; j < len(needle.arr); j++ {
					if !jqEqual(haystack.arr[i+j], needle.arr[j]) {
						match = false
						break
					}
				}
				if match {
					idxs = append(idxs, i)
				}
			}
			return idxs, nil
		}
		var idxs []int
		for i, el := range haystack.arr {
			if jqEqual(el, needle) {
				idxs = append(idxs, i)
			}
		}
		return idxs, nil

	case jqNull:
		return nil, nil
	}
	return nil, jqErrStr(haystack.shortDesc() + " cannot be searched for indices")
}

func jqEvalUnaryMath(name string, n jqNum) jqVal {
	if (name == "abs" || name == "fabs") && n.isInt() && n.i != math.MinInt64 {
		if n.i < 0 {
			return jqMakeInt(-n.i)
		}
		return jqVal{kind: jqNumber, num: n}
	}
	if (name == "floor" || name == "ceil" || name == "round" || name == "trunc") && n.isInt() {
		return jqVal{kind: jqNumber, num: n}
	}
	f := n.asFloat()
	switch name {
	case "floor":
		f = math.Floor(f)
	case "ceil":
		f = math.Ceil(f)
	case "round":
		f = math.Round(f)
	case "trunc":
		f = math.Trunc(f)
	case "sqrt":
		f = math.Sqrt(f)
	case "abs", "fabs":
		f = math.Abs(f)
	case "log":
		f = math.Log(f)
	case "log10":
		f = math.Log10(f)
	case "log2":
		f = math.Log2(f)
	case "exp":
		f = math.Exp(f)
	case "exp2":
		f = math.Exp2(f)
	case "sin":
		f = math.Sin(f)
	case "cos":
		f = math.Cos(f)
	case "tan":
		f = math.Tan(f)
	case "asin":
		f = math.Asin(f)
	case "acos":
		f = math.Acos(f)
	case "atan":
		f = math.Atan(f)
	}
	return jqMakeFloat(f)
}

func (st *jqEvalState) evalWhile(cur jqVal, cond, update *jqNode, env *jqEnv, emit func(jqVal) error) error {
	if err := st.tick(); err != nil {
		return err
	}
	cvals, err := st.evalList(cond, cur, env)
	if err != nil {
		return err
	}
	for _, cv := range cvals {
		if !cv.isTruthy() {
			continue
		}
		if err := emit(cur); err != nil {
			return err
		}
		nvals, nerr := st.evalList(update, cur, env)
		if nerr != nil {
			return nerr
		}
		for _, nv := range nvals {
			if err := st.evalWhile(nv, cond, update, env, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (st *jqEvalState) evalUntil(cur jqVal, cond, next *jqNode, env *jqEnv, emit func(jqVal) error) error {
	if err := st.tick(); err != nil {
		return err
	}
	cvals, err := st.evalList(cond, cur, env)
	if err != nil {
		return err
	}
	for _, cv := range cvals {
		if cv.isTruthy() {
			if err := emit(cur); err != nil {
				return err
			}
			continue
		}
		nvals, nerr := st.evalList(next, cur, env)
		if nerr != nil {
			return nerr
		}
		for _, nv := range nvals {
			if err := st.evalUntil(nv, cond, next, env, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (st *jqEvalState) compileRegexArgs(reExpr, flagsExpr *jqNode, defaultFlags string, v jqVal, env *jqEnv) ([]*jqCompiledRegex, error) {
	rvals, err := st.evalList(reExpr, v, env)
	if err != nil {
		return nil, err
	}
	var fvals []jqVal
	if flagsExpr == nil {
		fvals = []jqVal{jqMakeNull()}
	} else {
		fvals, err = st.evalList(flagsExpr, v, env)
		if err != nil {
			return nil, err
		}
	}
	var out []*jqCompiledRegex
	for _, rv := range rvals {
		for _, fv := range fvals {
			fl := ""
			if fv.kind == jqString {
				fl = fv.str
			} else if fv.kind != jqNull {
				return nil, jqErrStr("regex flags must be a string")
			}
			pat, fullFlags, serr := extractRegexSpec(rv, fl+defaultFlags)
			if serr != nil {
				return nil, serr
			}
			cr, cerr := jqCompileRegex(pat, fullFlags)
			if cerr != nil {
				return nil, cerr
			}
			out = append(out, cr)
		}
	}
	return out, nil
}

func optFlagsArg(args []*jqNode, idx int) *jqNode {
	if len(args) > idx {
		return args[idx]
	}
	return nil
}

func (st *jqEvalState) evalRegexTest(args []*jqNode, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	if v.kind != jqString {
		return jqErrStr(v.shortDesc() + " cannot be matched, as it is not a string")
	}
	crs, err := st.compileRegexArgs(args[0], optFlagsArg(args, 1), "", v, env)
	if err != nil {
		return err
	}
	for _, cr := range crs {
		matched := false
		if cr.ignoreEmpty {
			for _, loc := range cr.re.FindAllStringIndex(v.str, -1) {
				if loc[1] > loc[0] {
					matched = true
					break
				}
			}
		} else {
			matched = cr.re.MatchString(v.str)
		}
		if err := emit(jqMakeBool(matched)); err != nil {
			return err
		}
	}
	return nil
}

func (st *jqEvalState) evalRegexMatch(args []*jqNode, isCapture bool, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	if v.kind != jqString {
		return jqErrStr(v.shortDesc() + " cannot be matched, as it is not a string")
	}
	crs, err := st.compileRegexArgs(args[0], optFlagsArg(args, 1), "", v, env)
	if err != nil {
		return err
	}
	var b2r []int
	if !isCapture {
		b2r = buildByteToRuneTable(v.str)
	}
	for _, cr := range crs {
		limit := 1
		if cr.global {
			limit = -1
		}
		locs := cr.re.FindAllStringSubmatchIndex(v.str, limit)
		subNames := cr.re.SubexpNames()
		for _, loc := range locs {
			if cr.ignoreEmpty && loc[0] == loc[1] {
				continue
			}
			var out jqVal
			if isCapture {
				out = jqBuildCaptureObj(v.str, loc, subNames)
			} else {
				out = jqBuildMatchVal(v.str, loc, subNames, b2r)
			}
			if err := emit(out); err != nil {
				return err
			}
		}
	}
	return nil
}

func (st *jqEvalState) evalRegexScan(args []*jqNode, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	if v.kind != jqString {
		return jqErrStr(v.shortDesc() + " cannot be matched, as it is not a string")
	}
	crs, err := st.compileRegexArgs(args[0], optFlagsArg(args, 1), "g", v, env)
	if err != nil {
		return err
	}
	for _, cr := range crs {
		locs := cr.re.FindAllStringSubmatchIndex(v.str, -1)
		numGroups := cr.re.NumSubexp()
		for _, loc := range locs {
			if cr.ignoreEmpty && loc[0] == loc[1] {
				continue
			}
			if numGroups == 0 {
				if err := emit(jqMakeStr(v.str[loc[0]:loc[1]])); err != nil {
					return err
				}
			} else {
				arr := make([]jqVal, numGroups)
				for g := 1; g <= numGroups; g++ {
					gs, ge := loc[2*g], loc[2*g+1]
					if gs >= 0 {
						arr[g-1] = jqMakeStr(v.str[gs:ge])
					}
				}
				if err := emit(jqMakeArr(arr)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (st *jqEvalState) evalRegexSplit(reExpr, flagsExpr *jqNode, asArray bool, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	if v.kind != jqString {
		return jqErrStr(v.shortDesc() + " cannot be split, as it is not a string")
	}
	crs, err := st.compileRegexArgs(reExpr, flagsExpr, "g", v, env)
	if err != nil {
		return err
	}
	for _, cr := range crs {
		parts := cr.re.Split(v.str, -1)
		if asArray {
			out := make([]jqVal, len(parts))
			for i, p := range parts {
				out[i] = jqMakeStr(p)
			}
			if err := emit(jqMakeArr(out)); err != nil {
				return err
			}
		} else {
			for _, p := range parts {
				if err := emit(jqMakeStr(p)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (st *jqEvalState) evalRegexSub(args []*jqNode, isGlobal bool, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	if v.kind != jqString {
		return jqErrStr(v.shortDesc() + " cannot be matched, as it is not a string")
	}
	defFlags := ""
	if isGlobal {
		defFlags = "g"
	}
	crs, err := st.compileRegexArgs(args[0], optFlagsArg(args, 2), defFlags, v, env)
	if err != nil {
		return err
	}
	for _, cr := range crs {
		limit := 1
		if cr.global || isGlobal {
			limit = -1
		}
		locs := cr.re.FindAllStringSubmatchIndex(v.str, limit)
		if len(locs) == 0 {
			if err := emit(v); err != nil {
				return err
			}
			continue
		}
		if err := st.evalSubStep(v.str, locs, cr.re.SubexpNames(), cr.ignoreEmpty, 0, 0, "", args[1], env, emit); err != nil {
			return err
		}
	}
	return nil
}

func (st *jqEvalState) evalSubStep(
	s string,
	locs [][]int,
	subNames []string,
	ignoreEmpty bool,
	idx int,
	lastEnd int,
	prefix string,
	repExpr *jqNode,
	env *jqEnv,
	emit func(jqVal) error,
) error {
	if idx >= len(locs) {
		return emit(jqMakeStr(prefix + s[lastEnd:]))
	}
	loc := locs[idx]
	if ignoreEmpty && loc[0] == loc[1] {
		return st.evalSubStep(s, locs, subNames, ignoreEmpty, idx+1, lastEnd, prefix, repExpr, env, emit)
	}
	capObj := jqBuildCaptureObj(s, loc, subNames)
	reps, err := st.evalList(repExpr, capObj, env)
	if err != nil {
		return err
	}
	segBefore := s[lastEnd:loc[0]]
	for _, repVal := range reps {
		if repVal.kind != jqString {
			return jqErrStr("sub/gsub replacement must produce a string")
		}
		if err := st.evalSubStep(s, locs, subNames, ignoreEmpty, idx+1, loc[1], prefix+segBefore+repVal.str, repExpr, env, emit); err != nil {
			return err
		}
	}
	return nil
}

func jqBuiltinSignatureList() []string {
	return []string{
		"INDEX/1", "INDEX/2", "IN/1", "IN/2",
		"abs/0", "acos/0", "add/0", "add/1", "all/0", "all/1", "all/2",
		"any/0", "any/1", "any/2", "arrays/0", "ascii_downcase/0", "ascii_upcase/0",
		"asin/0", "atan/0", "booleans/0", "bsearch/1", "builtins/0",
		"capture/1", "capture/2", "ceil/0", "combinations/0", "combinations/1",
		"contains/1", "cos/0", "debug/0", "del/1", "delpaths/1",
		"empty/0", "endswith/1", "env/0", "error/0", "error/1",
		"exp/0", "exp2/0", "explode/0", "fabs/0", "finites/0",
		"first/0", "first/1", "flatten/0", "flatten/1", "floor/0",
		"from_entries/0", "fromjson/0", "getpath/1", "group_by/1",
		"gsub/2", "gsub/3", "halt/0", "halt_error/0", "halt_error/1",
		"has/1", "implode/0", "in/1", "index/1", "indices/1",
		"infinite/0", "input/0", "inputs/0", "inside/1", "isempty/1",
		"isfinite/0", "isinfinite/0", "isnan/0", "isnormal/0", "iterables/0",
		"join/1", "keys/0", "keys_unsorted/0", "last/0", "last/1",
		"leaf_paths/0", "length/0", "limit/2", "log/0", "log10/0", "log2/0",
		"ltrim/0", "ltrimstr/1", "map/1", "map_values/1", "match/1", "match/2",
		"max/0", "max_by/1", "min/0", "min_by/1", "nan/0", "normals/0",
		"not/0", "nth/1", "nth/2", "nulls/0", "numbers/0", "objects/0",
		"path/1", "paths/0", "paths/1", "pick/1", "pow/2",
		"range/1", "range/2", "range/3", "recurse/0", "recurse/1", "recurse/2",
		"repeat/1", "reverse/0", "rindex/1", "round/0", "rtrim/0", "rtrimstr/1",
		"scalars/0", "scan/1", "scan/2", "select/1", "setpath/2",
		"sin/0", "sort/0", "sort_by/1", "split/1", "split/2", "splits/1", "splits/2",
		"sqrt/0", "startswith/1", "stderr/0", "strings/0", "sub/2", "sub/3",
		"tan/0", "test/1", "test/2", "to_entries/0", "toboolean/0",
		"tojson/0", "tonumber/0", "tostring/0", "transpose/0",
		"trim/0", "trimstr/1", "trunc/0", "type/0",
		"unique/0", "unique_by/1", "until/2", "utf8bytelength/0",
		"values/0", "walk/1", "while/2", "with_entries/1",
	}
}

// --- jq-defined Standard Library Prelude ---
//
// Higher-order builtins are defined in jq itself (as upstream jq does). This
// keeps them path-capable (del(.[] | select(...)), paths, first, last, ...)
// and keeps the linked Go code small.
const jqPreludeSrc = `
def select(f): if f then . else empty end;
def values: select(. != null);
def nulls: select(. == null);
def booleans: select(type == "boolean");
def numbers: select(type == "number");
def strings: select(type == "string");
def arrays: select(type == "array");
def objects: select(type == "object");
def iterables: select(type | . == "array" or . == "object");
def scalars: select(type | . != "array" and . != "object");
def normals: select(type == "number" and isnormal);
def finites: select(type == "number" and (isinfinite or isnan | not));
def recurse: ..;
def recurse(f): def r: ., (f | select(. != null) | r); r;
def recurse(f; cond): def r: ., (f | select(cond) | r); r;
def repeat(f): def r: f, r; r;
def map(f): [.[] | f];
def map_values(f): .[] |= f;
def to_entries: [keys_unsorted[] as $k | {key: $k, value: .[$k]}];
def from_entries: map({key: (if has("key") then .key elif has("Key") then .Key elif has("k") then .k elif has("K") then .K elif has("name") then .name else .Name end | if type == "string" then . else tostring end), value: (if has("value") then .value elif has("Value") then .Value elif has("v") then .v elif has("V") then .V else null end)}) | reduce .[] as $x ({}; .[$x.key] = $x.value);
def with_entries(f): to_entries | map(f) | from_entries;
def add(f): reduce f as $x (null; . + $x);
def any: reduce .[] as $x (false; . or $x);
def all: reduce .[] as $x (true; . and $x);
def any(f): reduce (.[] | f) as $x (false; . or $x);
def all(f): reduce (.[] | f) as $x (true; . and $x);
def first(f): label $out | (f | ., break $out);
def isempty(g): first((g | false), true);
def any(g; cond): isempty(first(g | cond | select(.))) | not;
def all(g; cond): isempty(first(g | cond | select(. | not)));
def last(g): reduce g as $item ([false, null]; [true, $item]) | if .[0] then .[1] else empty end;
def first: .[0];
def last: .[-1];
def nth($n): .[$n];
def limit($n; f): if $n > 0 then label $out | foreach f as $item (0; . + 1; $item, if . >= $n then break $out else empty end) elif $n == 0 then empty else error("limit doesn't support negative count") end;
def nth($n; g): if $n < 0 then error("nth doesn't support negative indices") else label $out | foreach g as $item (-1; . + 1; if . == $n then $item, break $out else empty end) end;
def in(xs): . as $x | xs | has($x);
def inside(xs): . as $x | xs | contains($x);
def IN(s): any(s == .; .);
def IN(src; s): any(src == s; .);
def INDEX(stream; idx_expr): reduce stream as $row ({}; .[$row | idx_expr | tostring] = $row);
def INDEX(idx_expr): INDEX(.[]; idx_expr);
def del(f): delpaths([path(f)]);
def paths: path(..) | select(length > 0);
def paths(node_filter): . as $dot | paths | select(. as $p | $dot | getpath($p) | node_filter);
def leaf_paths: paths(scalars);
def pick(pathexps): . as $top | reduce path(pathexps) as $p (null; setpath($p; $top | getpath($p)));
def walk(f): def w: if type == "object" then map_values(w) elif type == "array" then map(w) else . end | f; w;
def env: $ENV;
def splits($re): split($re; "") | .[];
def splits($re; flags): split($re; flags) | .[];
def combinations: if length == 0 then [] else .[0][] as $x | (.[1:] | combinations) as $w | [$x] + $w end;
def combinations(n): . as $dot | [range(n)] | map($dot) | combinations;
def error(msg): msg | error;
.`

var (
	jqPreludeOnce sync.Once
	jqPreludeRoot *jqEnv
)

// jqPreludeEnv parses the prelude once per process and returns an env whose
// defs are all mutually visible (closures share the same env).
func jqPreludeEnv() *jqEnv {
	jqPreludeOnce.Do(func() {
		ast, err := jqParse(jqPreludeSrc)
		if err != nil {
			panic("jq prelude: " + err.Error())
		}
		env := newJqEnv(nil)
		env.defs = map[jqFuncKey]*jqClosure{}
		for n := ast; n != nil && n.kind == jqNodeDef; n = n.b {
			env.defs[jqFuncKey{name: n.str, arity: len(n.params)}] = &jqClosure{params: n.params, body: n.a, env: env}
		}
		jqPreludeRoot = env
	})
	return jqPreludeRoot
}
