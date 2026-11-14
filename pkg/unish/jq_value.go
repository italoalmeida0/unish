package unish

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func isUTF16Surrogate(r rune) bool {
	return r >= 0xD800 && r <= 0xDFFF
}

func decodeUTF16Surrogate(r1, r2 rune) rune {
	if r1 >= 0xD800 && r1 < 0xDC00 && r2 >= 0xDC00 && r2 <= 0xDFFF {
		return ((r1 - 0xD800) << 10) + (r2 - 0xDC00) + 0x10000
	}
	return utf8.RuneError
}

type jqKind uint8

const (
	jqNull jqKind = iota
	jqBool
	jqNumber
	jqString
	jqArray
	jqObject
)

// jqNum stores numbers with exact 64-bit signed integer precision when
// representable, falling back to float64 (and optional verbatim literal for
// ultra-large untouched JSON integers).
type jqNum struct {
	ii bool
	i  int64
	f  float64
}

// isInt reports exact-integer numbers: integers always carry f == float64(i),
// while non-integral floats keep i == 0 (and are never integral, non-zero).
func (n jqNum) isInt() bool { return n.ii }

type jqEntry struct {
	key string
	val jqVal
}

// jqObj preserves exact key insertion order while accelerating lookups on
// larger objects via a lazily populated key->entry-index map.
type jqObj struct {
	entries []jqEntry
	index   map[string]int
}

type jqVal struct {
	kind jqKind
	b    bool
	num  jqNum
	str  string
	arr  []jqVal
	obj  *jqObj
}

func jqMakeNull() jqVal        { return jqVal{kind: jqNull} }
func jqMakeBool(b bool) jqVal  { return jqVal{kind: jqBool, b: b} }
func jqMakeStr(s string) jqVal { return jqVal{kind: jqString, str: s} }

func jqMakeInt(i int64) jqVal {
	return jqVal{
		kind: jqNumber,
		num:  jqNum{ii: true, i: i, f: float64(i)},
	}
}

func jqMakeFloat(f float64) jqVal {
	if !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f &&
		f >= -9223372036854775808.0 && f < 9223372036854775808.0 {
		// Keep exact integer canonical if f is an exact integer in int64 range,
		// EXCEPT preserve -0.0 or float nature? In jq, 1.0 + 2.0 == 3, and
		// integer-valued arithmetic outputs without trailing .0 (e.g. 5/2*2 -> 5).
		i := int64(f)
		if float64(i) == f {
			return jqMakeInt(i)
		}
	}
	return jqVal{
		kind: jqNumber,
		num:  jqNum{f: f},
	}
}

func jqMakeArr(elems []jqVal) jqVal {
	if elems == nil {
		elems = []jqVal{}
	}
	return jqVal{kind: jqArray, arr: elems}
}

func jqMakeObj(entries []jqEntry) jqVal {
	o := &jqObj{entries: entries}
	if len(entries) > 12 {
		o.rebuildIndex()
	}
	return jqVal{kind: jqObject, obj: o}
}

func newJqObj(capHint int) *jqObj {
	if capHint < 0 {
		capHint = 0
	}
	return &jqObj{entries: make([]jqEntry, 0, capHint)}
}

func (o *jqObj) rebuildIndex() {
	if o == nil {
		return
	}
	if len(o.entries) <= 12 {
		o.index = nil
		return
	}
	m := make(map[string]int, len(o.entries))
	for idx, e := range o.entries {
		m[e.key] = idx
	}
	o.index = m
}

func (o *jqObj) get(k string) (jqVal, bool) {
	if o == nil {
		return jqMakeNull(), false
	}
	if o.index != nil {
		if idx, ok := o.index[k]; ok {
			return o.entries[idx].val, true
		}
		return jqMakeNull(), false
	}
	for i := range o.entries {
		if o.entries[i].key == k {
			return o.entries[i].val, true
		}
	}
	return jqMakeNull(), false
}

func (o *jqObj) set(k string, v jqVal) {
	if o.index != nil {
		if idx, ok := o.index[k]; ok {
			o.entries[idx].val = v
			return
		}
		o.index[k] = len(o.entries)
		o.entries = append(o.entries, jqEntry{key: k, val: v})
		return
	}
	for i := range o.entries {
		if o.entries[i].key == k {
			o.entries[i].val = v
			return
		}
	}
	o.entries = append(o.entries, jqEntry{key: k, val: v})
	if len(o.entries) > 12 {
		o.rebuildIndex()
	}
}

func (o *jqObj) deleteKey(k string) {
	if o == nil {
		return
	}
	pos := -1
	if o.index != nil {
		idx, ok := o.index[k]
		if !ok {
			return
		}
		pos = idx
	} else {
		for i := range o.entries {
			if o.entries[i].key == k {
				pos = i
				break
			}
		}
	}
	if pos < 0 {
		return
	}
	o.entries = append(o.entries[:pos], o.entries[pos+1:]...)
	o.rebuildIndex()
}

func (o *jqObj) clone() *jqObj {
	if o == nil {
		return &jqObj{}
	}
	entries := make([]jqEntry, len(o.entries))
	for i, e := range o.entries {
		entries[i] = jqEntry{key: e.key, val: e.val.clone()}
	}
	cp := &jqObj{entries: entries}
	if len(entries) > 12 {
		cp.rebuildIndex()
	}
	return cp
}

func (v jqVal) clone() jqVal {
	switch v.kind {
	case jqArray:
		cp := make([]jqVal, len(v.arr))
		for i := range v.arr {
			cp[i] = v.arr[i].clone()
		}
		return jqMakeArr(cp)
	case jqObject:
		return jqVal{kind: jqObject, obj: v.obj.clone()}
	default:
		return v
	}
}

func (v jqVal) isTruthy() bool {
	return v.kind != jqNull && !(v.kind == jqBool && !v.b)
}

func (v jqVal) typeName() string {
	switch v.kind {
	case jqNull:
		return "null"
	case jqBool:
		return "boolean"
	case jqNumber:
		return "number"
	case jqString:
		return "string"
	case jqArray:
		return "array"
	case jqObject:
		return "object"
	default:
		return "unknown"
	}
}

func (v jqVal) shortDesc() string {
	var buf bytes.Buffer
	jqWriteCompact(&buf, v, false)
	s := buf.String()
	if len(s) > 32 {
		s = s[:29] + "..."
	}
	return fmt.Sprintf("%s (%s)", v.typeName(), s)
}

func (n jqNum) asFloat() float64 {
	if n.isInt() {
		return float64(n.i)
	}
	return n.f
}

func (n jqNum) asInt64() (int64, bool) {
	if n.isInt() {
		return n.i, true
	}
	if math.IsNaN(n.f) || math.IsInf(n.f, 0) {
		return 0, false
	}
	if math.Trunc(n.f) == n.f && n.f >= -9223372036854775808.0 && n.f < 9223372036854775808.0 {
		return int64(n.f), true
	}
	return 0, false
}

// --- Exact 64-bit / float64 arithmetic ---

func jqAddInt64(a, b int64) (int64, bool) {
	c := int64(uint64(a) + uint64(b))
	if (b > 0 && c < a) || (b < 0 && c > a) {
		return 0, false
	}
	return c, true
}

func jqSubInt64(a, b int64) (int64, bool) {
	c := int64(uint64(a) - uint64(b))
	if (b < 0 && c < a) || (b > 0 && c > a) {
		return 0, false
	}
	return c, true
}

func jqMulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if (a == math.MinInt64 && b == -1) || (b == math.MinInt64 && a == -1) {
		return 0, false
	}
	c := a * b
	if c/a != b {
		return 0, false
	}
	return c, true
}

func jqAddNum(a, b jqNum) jqVal {
	if a.isInt() && b.isInt() {
		if sum, ok := jqAddInt64(a.i, b.i); ok {
			return jqMakeInt(sum)
		}
	}
	return jqMakeFloat(a.asFloat() + b.asFloat())
}

func jqSubNum(a, b jqNum) jqVal {
	if a.isInt() && b.isInt() {
		if diff, ok := jqSubInt64(a.i, b.i); ok {
			return jqMakeInt(diff)
		}
	}
	return jqMakeFloat(a.asFloat() - b.asFloat())
}

func jqMulNum(a, b jqNum) jqVal {
	if a.isInt() && b.isInt() {
		if prod, ok := jqMulInt64(a.i, b.i); ok {
			return jqMakeInt(prod)
		}
	}
	return jqMakeFloat(a.asFloat() * b.asFloat())
}

func jqDivNum(a, b jqNum) (jqVal, error) {
	if (b.isInt() && b.i == 0) || (!b.isInt() && b.f == 0) {
		return jqMakeNull(), fmt.Errorf("number (%s) and number (0) cannot be divided because the divisor is zero", jqFormatNum(a))
	}
	if a.isInt() && b.isInt() {
		if !(a.i == math.MinInt64 && b.i == -1) && a.i%b.i == 0 {
			return jqMakeInt(a.i / b.i), nil
		}
	}
	return jqMakeFloat(a.asFloat() / b.asFloat()), nil
}

func jqModNum(a, b jqNum) (jqVal, error) {
	if (b.isInt() && b.i == 0) || (!b.isInt() && b.f == 0) {
		return jqMakeNull(), fmt.Errorf("number (%s) and number (0) cannot be divided (remainder) because the divisor is zero", jqFormatNum(a))
	}
	if ai, ok1 := a.asInt64(); ok1 {
		if bi, ok2 := b.asInt64(); ok2 {
			if bi == 0 {
				return jqMakeNull(), fmt.Errorf("cannot divide by zero")
			}
			if ai == math.MinInt64 && bi == -1 {
				return jqMakeInt(0), nil
			}
			return jqMakeInt(ai % bi), nil
		}
	}
	// jq truncates floats to integer before % (like (int64)a % (int64)b or math.Mod)
	af, bf := math.Trunc(a.asFloat()), math.Trunc(b.asFloat())
	if bf == 0 {
		return jqMakeNull(), fmt.Errorf("cannot divide by zero")
	}
	return jqMakeFloat(math.Mod(af, bf)), nil
}

func jqNegNum(n jqNum) jqVal {
	if n.isInt() && n.i != math.MinInt64 {
		return jqMakeInt(-n.i)
	}
	return jqMakeFloat(-n.asFloat())
}

// --- Total comparison & equality ---
// jq type order: null < false < true < number < string < array < object

func jqTypeRank(v jqVal) int {
	switch v.kind {
	case jqNull:
		return 0
	case jqBool:
		if !v.b {
			return 1
		}
		return 2
	case jqNumber:
		return 3
	case jqString:
		return 4
	case jqArray:
		return 5
	case jqObject:
		return 6
	default:
		return 7
	}
}

func jqCompareNum(a, b jqNum) int {
	if a.isInt() && b.isInt() {
		if a.i < b.i {
			return -1
		}
		if a.i > b.i {
			return 1
		}
		return 0
	}
	af, bf := a.asFloat(), b.asFloat()
	aNaN, bNaN := math.IsNaN(af), math.IsNaN(bf)
	if aNaN || bNaN {
		if aNaN && bNaN {
			return 0
		}
		if aNaN {
			return -1
		}
		return 1
	}
	if a.isInt() {
		if bi, ok := b.asInt64(); ok {
			if a.i < bi {
				return -1
			}
			if a.i > bi {
				return 1
			}
			return 0
		}
	} else if b.isInt() {
		if ai, ok := a.asInt64(); ok {
			if ai < b.i {
				return -1
			}
			if ai > b.i {
				return 1
			}
			return 0
		}
	}
	if af < bf {
		return -1
	}
	if af > bf {
		return 1
	}
	return 0
}

func jqCompare(a, b jqVal) int {
	ra, rb := jqTypeRank(a), jqTypeRank(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch a.kind {
	case jqNull, jqBool:
		return 0
	case jqNumber:
		return jqCompareNum(a.num, b.num)
	case jqString:
		return strings.Compare(a.str, b.str)
	case jqArray:
		minLen := len(a.arr)
		if len(b.arr) < minLen {
			minLen = len(b.arr)
		}
		for i := 0; i < minLen; i++ {
			if c := jqCompare(a.arr[i], b.arr[i]); c != 0 {
				return c
			}
		}
		if len(a.arr) < len(b.arr) {
			return -1
		}
		if len(a.arr) > len(b.arr) {
			return 1
		}
		return 0
	case jqObject:
		var ka, kb []string
		if a.obj != nil {
			ka = make([]string, len(a.obj.entries))
			for i, e := range a.obj.entries {
				ka[i] = e.key
			}
			sort.Strings(ka)
		}
		if b.obj != nil {
			kb = make([]string, len(b.obj.entries))
			for i, e := range b.obj.entries {
				kb[i] = e.key
			}
			sort.Strings(kb)
		}
		minK := len(ka)
		if len(kb) < minK {
			minK = len(kb)
		}
		for i := 0; i < minK; i++ {
			if c := strings.Compare(ka[i], kb[i]); c != 0 {
				return c
			}
		}
		if len(ka) < len(kb) {
			return -1
		}
		if len(ka) > len(kb) {
			return 1
		}
		for _, k := range ka {
			va, _ := a.obj.get(k)
			vb, _ := b.obj.get(k)
			if c := jqCompare(va, vb); c != 0 {
				return c
			}
		}
		return 0
	}
	return 0
}

func jqEqual(a, b jqVal) bool {
	if a.kind != b.kind {
		return false
	}
	if a.kind == jqNumber {
		if (!a.num.isInt() && math.IsNaN(a.num.f)) || (!b.num.isInt() && math.IsNaN(b.num.f)) {
			return false
		}
	}
	if a.kind == jqObject {
		var la, lb int
		if a.obj != nil {
			la = len(a.obj.entries)
		}
		if b.obj != nil {
			lb = len(b.obj.entries)
		}
		if la != lb {
			return false
		}
		if la == 0 {
			return true
		}
		for _, e := range a.obj.entries {
			vb, ok := b.obj.get(e.key)
			if !ok || !jqEqual(e.val, vb) {
				return false
			}
		}
		return true
	}
	return jqCompare(a, b) == 0
}

// jqDeepMergeObjects implements `obj1 * obj2` recursive object merging.
func jqDeepMergeObjects(a, b *jqObj) jqVal {
	out := a.clone()
	if b == nil {
		return jqVal{kind: jqObject, obj: out}
	}
	for _, e := range b.entries {
		if existing, ok := out.get(e.key); ok && existing.kind == jqObject && e.val.kind == jqObject {
			out.set(e.key, jqDeepMergeObjects(existing.obj, e.val.obj))
		} else {
			out.set(e.key, e.val.clone())
		}
	}
	return jqVal{kind: jqObject, obj: out}
}

// jqContains implements jq's recursive `contains(b)` semantics.
func jqContains(a, b jqVal) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case jqString:
		return strings.Contains(a.str, b.str)
	case jqArray:
		for _, bel := range b.arr {
			found := false
			for _, ael := range a.arr {
				if jqContains(ael, bel) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	case jqObject:
		if b.obj == nil {
			return true
		}
		for _, be := range b.obj.entries {
			av, ok := a.obj.get(be.key)
			if !ok || !jqContains(av, be.val) {
				return false
			}
		}
		return true
	default:
		return jqEqual(a, b)
	}
}

// --- Path get / set / del ---

type jqSliceSpec struct {
	hasStart bool
	start    int
	hasEnd   bool
	end      int
}

// jqPathStep is either a string key, an integer index, or an array slice.
type jqPathStep struct {
	isSlice bool
	slice   jqSliceSpec
	key     jqVal // jqString or jqNumber (int)
}

func jqResolveSliceBounds(length int, s jqSliceSpec) (int, int) {
	start := 0
	if s.hasStart {
		start = s.start
		if start < 0 {
			start += length
		}
		if start < 0 {
			start = 0
		}
		if start > length {
			start = length
		}
	}
	end := length
	if s.hasEnd {
		end = s.end
		if end < 0 {
			end += length
		}
		if end < 0 {
			end = 0
		}
		if end > length {
			end = length
		}
	}
	if end < start {
		end = start
	}
	return start, end
}

func jqGetStep(v jqVal, step jqPathStep) (jqVal, error) {
	if step.isSlice {
		switch v.kind {
		case jqNull:
			return jqMakeNull(), nil
		case jqArray:
			st, en := jqResolveSliceBounds(len(v.arr), step.slice)
			cp := make([]jqVal, en-st)
			copy(cp, v.arr[st:en])
			return jqMakeArr(cp), nil
		case jqString:
			runes := []rune(v.str)
			st, en := jqResolveSliceBounds(len(runes), step.slice)
			return jqMakeStr(string(runes[st:en])), nil
		default:
			return jqMakeNull(), fmt.Errorf("cannot slice %s", v.shortDesc())
		}
	}
	switch step.key.kind {
	case jqString:
		if v.kind == jqNull {
			return jqMakeNull(), nil
		}
		if v.kind != jqObject {
			return jqMakeNull(), fmt.Errorf("cannot index %s with string %q", v.typeName(), step.key.str)
		}
		if val, ok := v.obj.get(step.key.str); ok {
			return val, nil
		}
		return jqMakeNull(), nil
	case jqNumber:
		if v.kind == jqNull {
			return jqMakeNull(), nil
		}
		idx, ok := step.key.num.asInt64()
		if !ok {
			return jqMakeNull(), fmt.Errorf("cannot index %s with non-integer number", v.typeName())
		}
		switch v.kind {
		case jqArray:
			i := int(idx)
			if i < 0 {
				i += len(v.arr)
			}
			if i < 0 || i >= len(v.arr) {
				return jqMakeNull(), nil
			}
			return v.arr[i], nil
		case jqString:
			// jq supports string indexing in recent versions or slice; support .[i] cleanly:
			runes := []rune(v.str)
			i := int(idx)
			if i < 0 {
				i += len(runes)
			}
			if i < 0 || i >= len(runes) {
				return jqMakeNull(), nil
			}
			return jqMakeStr(string(runes[i])), nil
		default:
			return jqMakeNull(), fmt.Errorf("cannot index %s with number", v.typeName())
		}
	default:
		return jqMakeNull(), fmt.Errorf("cannot index %s with %s", v.typeName(), step.key.typeName())
	}
}

func jqGetPath(root jqVal, path []jqPathStep) (jqVal, error) {
	cur := root
	for _, st := range path {
		next, err := jqGetStep(cur, st)
		if err != nil {
			return jqMakeNull(), err
		}
		cur = next
	}
	return cur, nil
}

func jqSetPath(cur jqVal, path []jqPathStep, newVal jqVal) (jqVal, error) {
	if len(path) == 0 {
		return newVal.clone(), nil
	}
	step := path[0]
	rest := path[1:]
	if step.isSlice {
		if len(rest) != 0 {
			return jqMakeNull(), fmt.Errorf("cannot set nested path through slice")
		}
		if cur.kind == jqNull {
			cur = jqMakeArr(nil)
		}
		if cur.kind != jqArray {
			return jqMakeNull(), fmt.Errorf("cannot update slice on %s", cur.typeName())
		}
		if newVal.kind != jqArray {
			return jqMakeNull(), fmt.Errorf("cannot set array slice to non-array %s", newVal.typeName())
		}
		st, en := jqResolveSliceBounds(len(cur.arr), step.slice)
		out := make([]jqVal, 0, len(cur.arr)-(en-st)+len(newVal.arr))
		out = append(out, cur.arr[:st]...)
		out = append(out, newVal.arr...)
		out = append(out, cur.arr[en:]...)
		return jqMakeArr(out), nil
	}
	switch step.key.kind {
	case jqString:
		if cur.kind == jqNull {
			cur = jqMakeObj(nil)
		}
		if cur.kind != jqObject {
			return jqMakeNull(), fmt.Errorf("cannot index %s with string %q", cur.typeName(), step.key.str)
		}
		cp := cur.obj.clone()
		child, _ := cp.get(step.key.str)
		updatedChild, err := jqSetPath(child, rest, newVal)
		if err != nil {
			return jqMakeNull(), err
		}
		cp.set(step.key.str, updatedChild)
		return jqVal{kind: jqObject, obj: cp}, nil

	case jqNumber:
		idx64, ok := step.key.num.asInt64()
		if !ok {
			return jqMakeNull(), fmt.Errorf("array index must be an integer")
		}
		if cur.kind == jqNull {
			cur = jqMakeArr(nil)
		}
		if cur.kind != jqArray {
			return jqMakeNull(), fmt.Errorf("cannot index %s with number", cur.typeName())
		}
		idx := int(idx64)
		if idx < 0 {
			idx += len(cur.arr)
		}
		if idx < 0 {
			return jqMakeNull(), fmt.Errorf("Out of bounds negative array index")
		}
		if idx > 1<<22 {
			return jqMakeNull(), fmt.Errorf("Array index too large")
		}
		var cp []jqVal
		if idx >= len(cur.arr) {
			cp = make([]jqVal, idx+1)
			for i := 0; i < len(cur.arr); i++ {
				cp[i] = cur.arr[i].clone()
			}
			for i := len(cur.arr); i <= idx; i++ {
				cp[i] = jqMakeNull()
			}
		} else {
			cp = make([]jqVal, len(cur.arr))
			for i := range cur.arr {
				cp[i] = cur.arr[i].clone()
			}
		}
		updatedChild, err := jqSetPath(cp[idx], rest, newVal)
		if err != nil {
			return jqMakeNull(), err
		}
		cp[idx] = updatedChild
		return jqMakeArr(cp), nil

	default:
		return jqMakeNull(), fmt.Errorf("cannot index with %s", step.key.typeName())
	}
}

// jqDelPaths deletes multiple paths simultaneously without shifting-index bugs
// by grouping deletions recursively per container and deleting array indices in
// descending order.
func jqDelPaths(cur jqVal, paths [][]jqPathStep) (jqVal, error) {
	if len(paths) == 0 {
		return cur, nil
	}
	for _, p := range paths {
		if len(p) == 0 {
			return jqMakeNull(), nil
		}
	}
	switch cur.kind {
	case jqObject:
		if cur.obj == nil {
			return cur, nil
		}
		cp := cur.obj.clone()
		directDel := map[string]bool{}
		nested := map[string][][]jqPathStep{}
		for _, p := range paths {
			st := p[0]
			if st.isSlice || st.key.kind != jqString {
				continue
			}
			k := st.key.str
			if len(p) == 1 {
				directDel[k] = true
			} else {
				nested[k] = append(nested[k], p[1:])
			}
		}
		for k := range directDel {
			cp.deleteKey(k)
		}
		for k, subPaths := range nested {
			if directDel[k] {
				continue
			}
			if child, ok := cp.get(k); ok {
				updated, err := jqDelPaths(child, subPaths)
				if err != nil {
					return jqMakeNull(), err
				}
				cp.set(k, updated)
			}
		}
		return jqVal{kind: jqObject, obj: cp}, nil

	case jqArray:
		cp := make([]jqVal, len(cur.arr))
		for i := range cur.arr {
			cp[i] = cur.arr[i].clone()
		}
		delIdx := make([]bool, len(cp))
		nested := map[int][][]jqPathStep{}
		for _, p := range paths {
			st := p[0]
			if st.isSlice {
				if len(p) == 1 {
					lo, hi := jqResolveSliceBounds(len(cp), st.slice)
					for i := lo; i < hi; i++ {
						delIdx[i] = true
					}
				}
				continue
			}
			if st.key.kind != jqNumber {
				continue
			}
			idx64, ok := st.key.num.asInt64()
			if !ok {
				continue
			}
			idx := int(idx64)
			if idx < 0 {
				idx += len(cp)
			}
			if idx < 0 || idx >= len(cp) {
				continue
			}
			if len(p) == 1 {
				delIdx[idx] = true
			} else {
				nested[idx] = append(nested[idx], p[1:])
			}
		}
		for idx, subPaths := range nested {
			if delIdx[idx] {
				continue
			}
			updated, err := jqDelPaths(cp[idx], subPaths)
			if err != nil {
				return jqMakeNull(), err
			}
			cp[idx] = updated
		}
		out := make([]jqVal, 0, len(cp))
		for i := range cp {
			if !delIdx[i] {
				out = append(out, cp[i])
			}
		}
		return jqMakeArr(out), nil

	case jqNull:
		return cur, nil

	default:
		return cur, nil
	}
}

func jqValSliceToPathSteps(arr []jqVal) ([]jqPathStep, error) {
	steps := make([]jqPathStep, len(arr))
	for i, el := range arr {
		switch el.kind {
		case jqString, jqNumber:
			steps[i] = jqPathStep{key: el}
		case jqObject:
			// Slice representation {"start": ..., "end": ...}
			var spec jqSliceSpec
			if sv, ok := el.obj.get("start"); ok && sv.kind == jqNumber {
				if si, ok2 := sv.num.asInt64(); ok2 {
					spec.hasStart = true
					spec.start = int(si)
				}
			}
			if ev, ok := el.obj.get("end"); ok && ev.kind == jqNumber {
				if ei, ok2 := ev.num.asInt64(); ok2 {
					spec.hasEnd = true
					spec.end = int(ei)
				}
			}
			steps[i] = jqPathStep{isSlice: true, slice: spec}
		default:
			return nil, fmt.Errorf("path elements must be strings or numbers, got %s", el.typeName())
		}
	}
	return steps, nil
}

func jqPathStepsToVal(steps []jqPathStep) jqVal {
	out := make([]jqVal, len(steps))
	for i, st := range steps {
		if st.isSlice {
			entries := make([]jqEntry, 0, 2)
			if st.slice.hasStart {
				entries = append(entries, jqEntry{key: "start", val: jqMakeInt(int64(st.slice.start))})
			} else {
				entries = append(entries, jqEntry{key: "start", val: jqMakeNull()})
			}
			if st.slice.hasEnd {
				entries = append(entries, jqEntry{key: "end", val: jqMakeInt(int64(st.slice.end))})
			} else {
				entries = append(entries, jqEntry{key: "end", val: jqMakeNull()})
			}
			out[i] = jqMakeObj(entries)
		} else {
			out[i] = st.key
		}
	}
	return jqMakeArr(out)
}

// --- JSON Serialization ---

type jqFmtOpts struct {
	compact  bool
	sortKeys bool
	indent   string // e.g. "  " or "\t" or ""
	color    bool
}

const (
	jqAnsiReset = "\x1b[0m"
	jqAnsiNull  = "\x1b[1;30m"
	jqAnsiBool  = "\x1b[0;39m"
	jqAnsiNum   = "\x1b[0;39m"
	jqAnsiStr   = "\x1b[0;32m"
	jqAnsiKey   = "\x1b[1;34m"
	jqAnsiPunct = "\x1b[1;39m"
)

// jqFormatNumV formats a number value, preserving verbatim ultra-large literals.
func jqFormatNumV(v jqVal) string {
	if v.str != "" {
		return v.str
	}
	return jqFormatNum(v.num)
}

func jqFormatNum(n jqNum) string {
	if n.isInt() {
		return strconv.FormatInt(n.i, 10)
	}
	if math.IsNaN(n.f) {
		return "null"
	}
	if math.IsInf(n.f, 1) {
		return "1.7976931348623157e+308"
	}
	if math.IsInf(n.f, -1) {
		return "-1.7976931348623157e+308"
	}
	if n.f == 0 {
		if math.Signbit(n.f) {
			return "-0"
		}
		return "0"
	}
	abs := math.Abs(n.f)
	if abs < 1e-6 || abs >= 1e21 {
		return strconv.FormatFloat(n.f, 'e', -1, 64)
	}
	return strconv.FormatFloat(n.f, 'f', -1, 64)
}

func jqWriteJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c != 0x7f {
			continue
		}
		if c == 0x7f {
			// DEL (0x7f) is valid unescaped in JSON; jq outputs 0x7f raw or \u007f? RFC 8259 allows 0x7f raw.
			continue
		}
		if start < i {
			buf.WriteString(s[start:i])
		}
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			const hex = "0123456789abcdef"
			buf.WriteString(`\u00`)
			buf.WriteByte(hex[c>>4])
			buf.WriteByte(hex[c&0x0f])
		}
		start = i + 1
	}
	if start < len(s) {
		buf.WriteString(s[start:])
	}
	buf.WriteByte('"')
}

func jqWriteCompact(buf *bytes.Buffer, v jqVal, sortKeys bool) {
	jqWriteFormatted(buf, v, jqFmtOpts{compact: true, sortKeys: sortKeys}, 0)
}

func jqWriteFormatted(buf *bytes.Buffer, v jqVal, opts jqFmtOpts, depth int) {
	wrap := func(ansi string, fn func()) {
		if opts.color && ansi != "" {
			buf.WriteString(ansi)
			fn()
			buf.WriteString(jqAnsiReset)
		} else {
			fn()
		}
	}
	switch v.kind {
	case jqNull:
		wrap(jqAnsiNull, func() { buf.WriteString("null") })
	case jqBool:
		wrap(jqAnsiBool, func() {
			if v.b {
				buf.WriteString("true")
			} else {
				buf.WriteString("false")
			}
		})
	case jqNumber:
		s := jqFormatNumV(v)
		if s == "null" {
			wrap(jqAnsiNull, func() { buf.WriteString("null") })
		} else {
			wrap(jqAnsiNum, func() { buf.WriteString(s) })
		}
	case jqString:
		wrap(jqAnsiStr, func() { jqWriteJSONString(buf, v.str) })
	case jqArray:
		if len(v.arr) == 0 {
			wrap(jqAnsiPunct, func() { buf.WriteString("[]") })
			return
		}
		pretty := !opts.compact
		wrap(jqAnsiPunct, func() { buf.WriteByte('[') })
		for i, el := range v.arr {
			if i > 0 {
				wrap(jqAnsiPunct, func() { buf.WriteByte(',') })
			}
			if pretty {
				buf.WriteByte('\n')
				for d := 0; d <= depth; d++ {
					buf.WriteString(opts.indent)
				}
			}
			jqWriteFormatted(buf, el, opts, depth+1)
		}
		if pretty {
			buf.WriteByte('\n')
			for d := 0; d < depth; d++ {
				buf.WriteString(opts.indent)
			}
		}
		wrap(jqAnsiPunct, func() { buf.WriteByte(']') })

	case jqObject:
		var entries []jqEntry
		if v.obj != nil {
			entries = v.obj.entries
		}
		if len(entries) == 0 {
			wrap(jqAnsiPunct, func() { buf.WriteString("{}") })
			return
		}
		if opts.sortKeys && len(entries) > 1 {
			sorted := make([]jqEntry, len(entries))
			copy(sorted, entries)
			sort.Slice(sorted, func(i, j int) bool {
				return sorted[i].key < sorted[j].key
			})
			entries = sorted
		}
		pretty := !opts.compact
		wrap(jqAnsiPunct, func() { buf.WriteByte('{') })
		for i, e := range entries {
			if i > 0 {
				wrap(jqAnsiPunct, func() { buf.WriteByte(',') })
			}
			if pretty {
				buf.WriteByte('\n')
				for d := 0; d <= depth; d++ {
					buf.WriteString(opts.indent)
				}
			}
			wrap(jqAnsiKey, func() { jqWriteJSONString(buf, e.key) })
			if !opts.compact {
				wrap(jqAnsiPunct, func() { buf.WriteString(": ") })
			} else {
				wrap(jqAnsiPunct, func() { buf.WriteByte(':') })
			}
			jqWriteFormatted(buf, e.val, opts, depth+1)
		}
		if pretty {
			buf.WriteByte('\n')
			for d := 0; d < depth; d++ {
				buf.WriteString(opts.indent)
			}
		}
		wrap(jqAnsiPunct, func() { buf.WriteByte('}') })
	}
}

// --- Streaming Zero-Dependency JSON Parser ---

type jqJSONDecoder struct {
	r       io.Reader
	buf     []byte
	pos     int
	end     int
	eof     bool
	line    int
	col     int
	readAny bool
}

const jqMaxJSONDepth = 1000

func newJqJSONDecoder(r io.Reader) *jqJSONDecoder {
	return &jqJSONDecoder{
		r:    r,
		buf:  make([]byte, 32*1024),
		line: 1,
		col:  1,
	}
}

func newJqJSONBytesDecoder(b []byte) *jqJSONDecoder {
	return &jqJSONDecoder{
		buf:  b,
		end:  len(b),
		eof:  true,
		line: 1,
		col:  1,
	}
}

func (d *jqJSONDecoder) fill() error {
	if d.eof || d.r == nil {
		d.eof = true
		return io.EOF
	}
	if d.pos > 0 {
		rem := d.end - d.pos
		if rem > 0 {
			copy(d.buf, d.buf[d.pos:d.end])
		}
		d.pos = 0
		d.end = rem
	}
	if d.end == len(d.buf) {
		nb := make([]byte, len(d.buf)*2)
		copy(nb, d.buf[:d.end])
		d.buf = nb
	}
	for {
		n, err := d.r.Read(d.buf[d.end:])
		if n > 0 {
			d.end += n
			return nil
		}
		if err != nil {
			d.eof = true
			return err
		}
	}
}

func (d *jqJSONDecoder) peek() (byte, error) {
	if d.pos >= d.end {
		if err := d.fill(); err != nil {
			if d.pos >= d.end {
				return 0, err
			}
		}
	}
	return d.buf[d.pos], nil
}

func (d *jqJSONDecoder) next() (byte, error) {
	b, err := d.peek()
	if err != nil {
		return 0, err
	}
	d.pos++
	if b == '\n' {
		d.line++
		d.col = 1
	} else {
		d.col++
	}
	return b, nil
}

func (d *jqJSONDecoder) skipWS() error {
	if !d.readAny {
		d.readAny = true
		// Strip optional leading UTF-8 BOM (EF BB BF).
		if b0, err := d.peek(); err == nil && b0 == 0xEF {
			for d.end-d.pos < 3 && !d.eof {
				_ = d.fill()
			}
			if d.end-d.pos >= 3 && d.buf[d.pos] == 0xEF && d.buf[d.pos+1] == 0xBB && d.buf[d.pos+2] == 0xBF {
				d.pos += 3
			}
		}
	}
	for {
		b, err := d.peek()
		if err != nil {
			return err
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			_, _ = d.next()
			continue
		}
		return nil
	}
}

func (d *jqJSONDecoder) syntaxErr(msg string) error {
	return fmt.Errorf("parse error: %s at line %d, column %d", msg, d.line, d.col)
}

// Next reads the next JSON value from the stream, returning io.EOF when only
// whitespace remains.
func (d *jqJSONDecoder) Next() (jqVal, error) {
	if err := d.skipWS(); err != nil {
		if err == io.EOF {
			return jqMakeNull(), io.EOF
		}
		return jqMakeNull(), err
	}
	return d.parseValue(0)
}

func (d *jqJSONDecoder) parseValue(depth int) (jqVal, error) {
	if depth > jqMaxJSONDepth {
		return jqMakeNull(), d.syntaxErr("exceeded maximum nesting depth")
	}
	if err := d.skipWS(); err != nil {
		if err == io.EOF {
			return jqMakeNull(), d.syntaxErr("unexpected EOF")
		}
		return jqMakeNull(), err
	}
	b, err := d.peek()
	if err != nil {
		return jqMakeNull(), d.syntaxErr("unexpected EOF")
	}
	switch b {
	case '{':
		return d.parseObject(depth + 1)
	case '[':
		return d.parseArray(depth + 1)
	case '"':
		s, err := d.parseString()
		if err != nil {
			return jqMakeNull(), err
		}
		return jqMakeStr(s), nil
	case 't':
		if err := d.expectLiteral("true"); err != nil {
			return jqMakeNull(), err
		}
		return jqMakeBool(true), nil
	case 'f':
		if err := d.expectLiteral("false"); err != nil {
			return jqMakeNull(), err
		}
		return jqMakeBool(false), nil
	case 'n':
		if err := d.expectLiteral("null"); err != nil {
			return jqMakeNull(), err
		}
		return jqMakeNull(), nil
	default:
		if b == '-' || (b >= '0' && b <= '9') {
			return d.parseNumber()
		}
		return jqMakeNull(), d.syntaxErr(fmt.Sprintf("Invalid numeric literal / unexpected character %q", b))
	}
}

func isJSONAtomBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ',', ']', '}', ':', '{', '[', '"':
		return true
	}
	return false
}

func (d *jqJSONDecoder) expectLiteral(lit string) error {
	for i := 0; i < len(lit); i++ {
		b, err := d.next()
		if err != nil || b != lit[i] {
			return d.syntaxErr(fmt.Sprintf("Invalid literal (expected %s)", lit))
		}
	}
	if nextB, err := d.peek(); err == nil && !isJSONAtomBoundary(nextB) {
		return d.syntaxErr(fmt.Sprintf("Invalid literal after %s", lit))
	}
	return nil
}

func (d *jqJSONDecoder) parseNumber() (jqVal, error) {
	var raw [64]byte
	buf := raw[:0]

	b, _ := d.peek()
	if b == '-' {
		_, _ = d.next()
		buf = append(buf, '-')
	}
	b, err := d.peek()
	if err != nil {
		return jqMakeNull(), d.syntaxErr("Invalid numeric literal at EOF")
	}
	if b == '0' {
		_, _ = d.next()
		buf = append(buf, '0')
		if nb, err := d.peek(); err == nil && nb >= '0' && nb <= '9' {
			return jqMakeNull(), d.syntaxErr("Invalid numeric literal (leading zero)")
		}
	} else if b >= '1' && b <= '9' {
		for {
			nb, err := d.peek()
			if err != nil || nb < '0' || nb > '9' {
				break
			}
			_, _ = d.next()
			buf = append(buf, nb)
		}
	} else {
		return jqMakeNull(), d.syntaxErr("Invalid numeric literal")
	}

	isFloat := false
	if nb, err := d.peek(); err == nil && nb == '.' {
		isFloat = true
		_, _ = d.next()
		buf = append(buf, '.')
		fb, err := d.peek()
		if err != nil || fb < '0' || fb > '9' {
			return jqMakeNull(), d.syntaxErr("Invalid numeric literal (expected digit after '.')")
		}
		for {
			db, err := d.peek()
			if err != nil || db < '0' || db > '9' {
				break
			}
			_, _ = d.next()
			buf = append(buf, db)
		}
	}
	if nb, err := d.peek(); err == nil && (nb == 'e' || nb == 'E') {
		isFloat = true
		_, _ = d.next()
		buf = append(buf, nb)
		sb, err := d.peek()
		if err == nil && (sb == '+' || sb == '-') {
			_, _ = d.next()
			buf = append(buf, sb)
		}
		eb, err := d.peek()
		if err != nil || eb < '0' || eb > '9' {
			return jqMakeNull(), d.syntaxErr("Invalid numeric literal (expected digit in exponent)")
		}
		for {
			db, err := d.peek()
			if err != nil || db < '0' || db > '9' {
				break
			}
			_, _ = d.next()
			buf = append(buf, db)
		}
	}
	if nb, err := d.peek(); err == nil && !isJSONAtomBoundary(nb) {
		return jqMakeNull(), d.syntaxErr("Invalid numeric literal")
	}

	s := string(buf)
	if !isFloat {
		if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
			return jqMakeInt(iv), nil
		}
		// Ultra-large integer (> int64): parse float64 for math, keep raw for lossless output!
		fv, err := strconv.ParseFloat(s, 64)
		if err != nil && !strings.Contains(err.Error(), "value out of range") {
			return jqMakeNull(), d.syntaxErr("Invalid numeric literal")
		}
		return jqVal{kind: jqNumber, num: jqNum{f: fv}, str: s}, nil
	}
	fv, err := strconv.ParseFloat(s, 64)
	if err != nil && !strings.Contains(err.Error(), "value out of range") {
		return jqMakeNull(), d.syntaxErr("Invalid numeric literal")
	}
	return jqVal{kind: jqNumber, num: jqNum{f: fv}, str: s}, nil
}

func parseHex4(s [4]byte) (rune, bool) {
	var v rune
	for i := 0; i < 4; i++ {
		c := s[i]
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			v |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			v |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return v, true
}

func (d *jqJSONDecoder) readHex4() (rune, error) {
	var h [4]byte
	for i := 0; i < 4; i++ {
		b, err := d.next()
		if err != nil {
			return 0, d.syntaxErr("unfinished \\uXXXX escape")
		}
		h[i] = b
	}
	r, ok := parseHex4(h)
	if !ok {
		return 0, d.syntaxErr("invalid \\uXXXX escape")
	}
	return r, nil
}

func (d *jqJSONDecoder) parseString() (string, error) {
	q, err := d.next()
	if err != nil || q != '"' {
		return "", d.syntaxErr("expected '\"'")
	}
	// Fast path: scan unescaped string directly in buffer!
	start := d.pos
	for d.pos < d.end {
		c := d.buf[d.pos]
		if c == '"' {
			s := string(d.buf[start:d.pos])
			d.col += (d.pos - start) + 1
			d.pos++
			return s, nil
		}
		if c == '\\' || c < 0x20 {
			break
		}
		d.pos++
	}
	// Rewind to start for general builder path so col/escapes stay exact.
	d.pos = start

	var sb strings.Builder
	for {
		b, err := d.next()
		if err != nil {
			return "", d.syntaxErr("Unfinished string at EOF")
		}
		if b == '"' {
			return sb.String(), nil
		}
		if b < 0x20 {
			return "", d.syntaxErr("Invalid control character in string literal")
		}
		if b != '\\' {
			sb.WriteByte(b)
			continue
		}
		esc, err := d.next()
		if err != nil {
			return "", d.syntaxErr("Unfinished escape at EOF")
		}
		switch esc {
		case '"', '\\', '/':
			sb.WriteByte(esc)
		case 'b':
			sb.WriteByte('\b')
		case 'f':
			sb.WriteByte('\f')
		case 'n':
			sb.WriteByte('\n')
		case 'r':
			sb.WriteByte('\r')
		case 't':
			sb.WriteByte('\t')
		case 'u':
			r1, err := d.readHex4()
			if err != nil {
				return "", err
			}
			if isUTF16Surrogate(r1) {
				// Check for \uXXXX low surrogate
				for d.end-d.pos < 6 && !d.eof {
					_ = d.fill()
				}
				if d.end-d.pos >= 6 && d.buf[d.pos] == '\\' && d.buf[d.pos+1] == 'u' {
					var h2 [4]byte
					copy(h2[:], d.buf[d.pos+2:d.pos+6])
					if r2, ok := parseHex4(h2); ok && r2 >= 0xDC00 && r2 <= 0xDFFF {
						d.pos += 6
						d.col += 6
						sb.WriteRune(decodeUTF16Surrogate(r1, r2))
						continue
					}
				}
				sb.WriteRune(utf8.RuneError)
				continue
			}
			sb.WriteRune(r1)
		default:
			return "", d.syntaxErr(fmt.Sprintf("Invalid escape \\%c", esc))
		}
	}
}

func (d *jqJSONDecoder) parseArray(depth int) (jqVal, error) {
	_, _ = d.next() // '['
	if err := d.skipWS(); err != nil {
		return jqMakeNull(), d.syntaxErr("Unfinished JSON array at EOF")
	}
	if b, err := d.peek(); err == nil && b == ']' {
		_, _ = d.next()
		return jqMakeArr(nil), nil
	}
	elems := make([]jqVal, 0, 4)
	for {
		val, err := d.parseValue(depth)
		if err != nil {
			return jqMakeNull(), err
		}
		elems = append(elems, val)
		if err := d.skipWS(); err != nil {
			return jqMakeNull(), d.syntaxErr("Unfinished JSON array at EOF")
		}
		b, err := d.next()
		if err != nil {
			return jqMakeNull(), d.syntaxErr("Unfinished JSON array at EOF")
		}
		if b == ']' {
			return jqMakeArr(elems), nil
		}
		if b != ',' {
			return jqMakeNull(), d.syntaxErr(fmt.Sprintf("Expected ',' or ']' in array, got %q", b))
		}
	}
}

func (d *jqJSONDecoder) parseObject(depth int) (jqVal, error) {
	_, _ = d.next() // '{'
	if err := d.skipWS(); err != nil {
		return jqMakeNull(), d.syntaxErr("Unfinished JSON object at EOF")
	}
	if b, err := d.peek(); err == nil && b == '}' {
		_, _ = d.next()
		return jqMakeObj(nil), nil
	}
	obj := newJqObj(4)
	for {
		if err := d.skipWS(); err != nil {
			return jqMakeNull(), d.syntaxErr("Unfinished JSON object at EOF")
		}
		kb, err := d.peek()
		if err != nil || kb != '"' {
			return jqMakeNull(), d.syntaxErr("Expected string key in object")
		}
		key, err := d.parseString()
		if err != nil {
			return jqMakeNull(), err
		}
		if err := d.skipWS(); err != nil {
			return jqMakeNull(), d.syntaxErr("Expected ':' after object key")
		}
		cb, err := d.next()
		if err != nil || cb != ':' {
			return jqMakeNull(), d.syntaxErr("Expected ':' after object key")
		}
		val, err := d.parseValue(depth)
		if err != nil {
			return jqMakeNull(), err
		}
		obj.set(key, val)
		if err := d.skipWS(); err != nil {
			return jqMakeNull(), d.syntaxErr("Unfinished JSON object at EOF")
		}
		sep, err := d.next()
		if err != nil {
			return jqMakeNull(), d.syntaxErr("Unfinished JSON object at EOF")
		}
		if sep == '}' {
			return jqVal{kind: jqObject, obj: obj}, nil
		}
		if sep != ',' {
			return jqMakeNull(), d.syntaxErr(fmt.Sprintf("Expected ',' or '}' in object, got %q", sep))
		}
	}
}

// jqParseSingleJSON parses exactly one JSON value from s and rejects trailing
// non-whitespace tokens (used for --argjson and fromjson).
func jqParseSingleJSON(s string) (jqVal, error) {
	dec := newJqJSONBytesDecoder([]byte(s))
	val, err := dec.Next()
	if err != nil {
		if err == io.EOF {
			return jqMakeNull(), fmt.Errorf("expected JSON value, got empty input")
		}
		return jqMakeNull(), err
	}
	if err := dec.skipWS(); err != io.EOF {
		return jqMakeNull(), dec.syntaxErr("unexpected trailing data after JSON value")
	}
	return val, nil
}
