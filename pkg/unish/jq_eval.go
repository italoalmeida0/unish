package unish

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
)

// --- Runtime Sentinels & Errors ---

type jqRuntimeError struct {
	val jqVal
}

func (e *jqRuntimeError) Error() string {
	if e.val.kind == jqString {
		return e.val.str
	}
	return jqFormatCompactStr(e.val)
}

func jqErrStr(msg string) error {
	return &jqRuntimeError{val: jqMakeStr(msg)}
}

type jqBreakError struct {
	label string
	id    int64
}

func (e *jqBreakError) Error() string {
	return "label $" + e.label + " is not defined"
}

type jqHaltError struct {
	code int
	err  jqVal
}

func (e *jqHaltError) Error() string {
	return "halt_error"
}

var errJqStopIter = errors.New("jq: stop iteration")

func jqFormatCompactStr(v jqVal) string {
	var buf bytes.Buffer
	jqWriteCompact(&buf, v, false)
	return buf.String()
}

// --- Environment & Closures ---

type jqFuncKey struct {
	name  string
	arity int
}

type jqClosure struct {
	params []jqDefParam
	body   *jqNode
	env    *jqEnv
}

type jqEnv struct {
	parent *jqEnv
	vars   map[string]jqVal
	defs   map[jqFuncKey]*jqClosure
}

func newJqEnv(parent *jqEnv) *jqEnv {
	return &jqEnv{parent: parent}
}

func (e *jqEnv) withVar(name string, val jqVal) *jqEnv {
	return &jqEnv{parent: e, vars: map[string]jqVal{name: val}}
}

func (e *jqEnv) setVar(name string, val jqVal) {
	if e.vars == nil {
		e.vars = make(map[string]jqVal, 4)
	}
	e.vars[name] = val
}

func (e *jqEnv) lookupVar(name string) (jqVal, bool) {
	for cur := e; cur != nil; cur = cur.parent {
		if cur.vars != nil {
			if v, ok := cur.vars[name]; ok {
				return v, true
			}
		}
	}
	return jqMakeNull(), false
}

func (e *jqEnv) lookupFunc(name string, arity int) (*jqClosure, bool) {
	key := jqFuncKey{name: name, arity: arity}
	for cur := e; cur != nil; cur = cur.parent {
		if cur.defs != nil {
			if fn, ok := cur.defs[key]; ok {
				return fn, true
			}
		}
	}
	return nil, false
}

// --- Evaluator State ---

type jqEvalState struct {
	ctx       context.Context
	stderr    io.Writer
	envObj    jqVal
	argsObj   jqVal
	nextInput func() (jqVal, error)
	steps     uint64
	depth     int
	labelSeq  int64
}

const jqMaxCallDepth = 2500

func (st *jqEvalState) tick() error {
	st.steps++
	if st.steps&255 == 0 {
		if err := st.ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// evalList evaluates expr into a slice using a 1-element stack buffer to avoid
// heap allocations and synthetic closure proliferation on common expressions.
func (st *jqEvalState) evalList(expr *jqNode, v jqVal, env *jqEnv) ([]jqVal, error) {
	if expr.kind == jqNodeLit {
		return []jqVal{expr.val}, nil
	}
	if expr.kind == jqNodeIdentity {
		return []jqVal{v}, nil
	}
	var inline [2]jqVal
	out := inline[:0]
	err := st.eval(expr, v, env, func(x jqVal) error {
		out = append(out, x)
		return nil
	})
	return out, err
}

func (st *jqEvalState) eval(e *jqNode, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	if err := st.tick(); err != nil {
		return err
	}
	st.depth++
	if st.depth > jqMaxCallDepth {
		st.depth--
		return jqErrStr("exceeded maximum recursion depth")
	}
	defer func() { st.depth-- }()

	switch e.kind {
	case jqNodeLit:
		return emit(e.val)

	case jqNodeIdentity:
		return emit(v)

	case jqNodeRecurse:
		return st.evalRecurseAll(v, emit)

	case jqNodeVar:
		if e.str == "ENV" {
			return emit(st.envObj)
		}
		if e.str == "ARGS" {
			return emit(st.argsObj)
		}
		val, ok := env.lookupVar(e.str)
		if !ok {
			return jqErrStr("$" + e.str + " is not defined")
		}
		return emit(val)

	case jqNodeFormat:
		if e.a != nil {
			return st.evalInterp(e.a, e.str, v, env, emit)
		}
		s, err := jqApplyFormat(e.str, v)
		if err != nil {
			return err
		}
		return emit(jqMakeStr(s))

	case jqNodeInterp:
		return st.evalInterp(e, "@text", v, env, emit)

	case jqNodeField:
		tvals, err := st.evalList(e.a, v, env)
		if err != nil {
			return err
		}
		for _, tv := range tvals {
			if e.b != nil {
				kvals, kerr := st.evalList(e.b, v, env)
				if kerr != nil {
					return kerr
				}
				for _, kv := range kvals {
					if kv.kind != jqString {
						if e.optional {
							continue
						}
						return jqErrStr("cannot index " + tv.typeName() + " with " + kv.typeName())
					}
					res, gerr := jqGetStep(tv, jqPathStep{key: kv})
					if gerr != nil {
						if e.optional {
							continue
						}
						return jqErrStr(gerr.Error())
					}
					if err := emit(res); err != nil {
						return err
					}
				}
				continue
			}
			res, gerr := jqGetStep(tv, jqPathStep{key: jqMakeStr(e.str)})
			if gerr != nil {
				if e.optional {
					continue
				}
				return jqErrStr(gerr.Error())
			}
			if err := emit(res); err != nil {
				return err
			}
		}
		return nil

	case jqNodeIndex:
		tvals, err := st.evalList(e.a, v, env)
		if err != nil {
			return err
		}
		for _, tv := range tvals {
			ivals, ierr := st.evalList(e.b, v, env)
			if ierr != nil {
				return ierr
			}
			for _, iv := range ivals {
				res, gerr := jqGetStep(tv, jqPathStep{key: iv})
				if gerr != nil {
					if e.optional {
						continue
					}
					return jqErrStr(gerr.Error())
				}
				if err := emit(res); err != nil {
					return err
				}
			}
		}
		return nil

	case jqNodeSlice:
		tvals, err := st.evalList(e.a, v, env)
		if err != nil {
			return err
		}
		specs, serr := st.evalSliceSpecs(e, v, env)
		if serr != nil {
			return serr
		}
		for _, tv := range tvals {
			for _, spec := range specs {
				res, gerr := jqGetStep(tv, jqPathStep{isSlice: true, slice: spec})
				if gerr != nil {
					if e.optional {
						continue
					}
					return jqErrStr(gerr.Error())
				}
				if err := emit(res); err != nil {
					return err
				}
			}
		}
		return nil

	case jqNodeIter:
		tvals, err := st.evalList(e.a, v, env)
		if err != nil {
			return err
		}
		for _, tv := range tvals {
			switch tv.kind {
			case jqArray:
				for _, el := range tv.arr {
					if err := emit(el); err != nil {
						return err
					}
				}
			case jqObject:
				if tv.obj != nil {
					for _, ent := range tv.obj.entries {
						if err := emit(ent.val); err != nil {
							return err
						}
					}
				}
			default:
				if !e.optional {
					return jqErrStr("Cannot iterate over " + tv.shortDesc())
				}
			}
		}
		return nil

	case jqNodePipe:
		return st.eval(e.a, v, env, func(lv jqVal) error {
			return st.eval(e.b, lv, env, emit)
		})

	case jqNodeComma:
		if err := st.eval(e.a, v, env, emit); err != nil {
			return err
		}
		return st.eval(e.b, v, env, emit)

	case jqNodeUnary:
		uvals, err := st.evalList(e.a, v, env)
		if err != nil {
			return err
		}
		for _, uv := range uvals {
			if uv.kind != jqNumber {
				return jqErrStr(uv.shortDesc() + " cannot be negated")
			}
			if e.tokOp == jqTokMinus {
				if err := emit(jqNegNum(uv.num)); err != nil {
					return err
				}
			} else if err := emit(uv); err != nil {
				return err
			}
		}
		return nil

	case jqNodeBinOp:
		return st.evalBinOp(e, v, env, emit)

	case jqNodeUpdate:
		return st.evalUpdate(e, v, env, emit)

	case jqNodeArray:
		if e.a == nil {
			return emit(jqMakeArr(nil))
		}
		elems, err := st.evalList(e.a, v, env)
		if err != nil {
			return err
		}
		cp := make([]jqVal, len(elems))
		copy(cp, elems)
		return emit(jqMakeArr(cp))

	case jqNodeObject:
		return st.evalObjectFields(e.fields, 0, nil, v, env, emit)

	case jqNodeIf:
		return st.evalIfBranches(e.branches, 0, e.a, v, env, emit)

	case jqNodeTry:
		err := st.eval(e.a, v, env, emit)
		if err == nil {
			return nil
		}
		var rtErr *jqRuntimeError
		if errors.As(err, &rtErr) {
			if e.b == nil {
				return nil
			}
			return st.eval(e.b, rtErr.val, env, emit)
		}
		return err

	case jqNodeBind:
		return st.eval(e.a, v, env, func(sv jqVal) error {
			return st.bindPatternsAndEval(e.pats, sv, v, env, e.b, emit)
		})

	case jqNodeReduce:
		inits, err := st.evalList(e.b, v, env)
		if err != nil {
			return err
		}
		pat := e.pats[0]
		for _, initAcc := range inits {
			acc := initAcc
			serr := st.eval(e.a, v, env, func(item jqVal) error {
				stepEnv := newJqEnv(env)
				ok, berr := st.matchPattern(pat, item, v, env, stepEnv)
				if berr != nil {
					return berr
				}
				if !ok {
					return jqErrStr("cannot destructure " + item.shortDesc())
				}
				uvals, uerr := st.evalList(e.c, acc, stepEnv)
				if uerr != nil {
					return uerr
				}
				if len(uvals) > 0 {
					acc = uvals[len(uvals)-1]
				}
				return nil
			})
			if serr != nil {
				return serr
			}
			if err := emit(acc); err != nil {
				return err
			}
		}
		return nil

	case jqNodeForeach:
		inits, err := st.evalList(e.b, v, env)
		if err != nil {
			return err
		}
		pat := e.pats[0]
		var extract *jqNode
		if len(e.args) > 0 {
			extract = e.args[0]
		}
		for _, initAcc := range inits {
			acc := initAcc
			serr := st.eval(e.a, v, env, func(item jqVal) error {
				stepEnv := newJqEnv(env)
				ok, berr := st.matchPattern(pat, item, v, env, stepEnv)
				if berr != nil {
					return berr
				}
				if !ok {
					return jqErrStr("cannot destructure " + item.shortDesc())
				}
				uvals, uerr := st.evalList(e.c, acc, stepEnv)
				if uerr != nil {
					return uerr
				}
				for _, uv := range uvals {
					acc = uv
					if extract == nil {
						if err := emit(acc); err != nil {
							return err
						}
					} else {
						if err := st.eval(extract, acc, stepEnv, emit); err != nil {
							return err
						}
					}
				}
				return nil
			})
			if serr != nil {
				return serr
			}
		}
		return nil

	case jqNodeDef:
		return st.eval(e.b, v, jqDefEnv(e, env), emit)

	case jqNodeLabel:
		return st.withLabel(e, env, func(le *jqEnv) error { return st.eval(e.a, v, le, emit) })

	case jqNodeBreak:
		lv, ok := env.lookupVar("*label*" + e.str)
		if !ok {
			return jqErrStr("$*label-" + e.str + " is not defined")
		}
		return &jqBreakError{label: e.str, id: lv.num.i}

	case jqNodeCall:
		if fn, ok := env.lookupFunc(e.str, len(e.args)); ok {
			return st.callUser(fn, e.args, v, env, func(ce *jqEnv) error { return st.eval(fn.body, v, ce, emit) })
		}
		return st.evalBuiltin(e.str, e.args, v, env, emit)
	}
	return jqErrStr("unsupported expression")
}

func jqDefEnv(e *jqNode, env *jqEnv) *jqEnv {
	defEnv := newJqEnv(env)
	defEnv.defs = map[jqFuncKey]*jqClosure{
		{name: e.str, arity: len(e.params)}: {params: e.params, body: e.a, env: defEnv},
	}
	return defEnv
}

func (st *jqEvalState) withLabel(e *jqNode, env *jqEnv, run func(*jqEnv) error) error {
	st.labelSeq++
	id := st.labelSeq
	err := run(env.withVar("*label*"+e.str, jqMakeInt(id)))
	var brk *jqBreakError
	if errors.As(err, &brk) && brk.id == id {
		return nil
	}
	return err
}

func (st *jqEvalState) callUser(fn *jqClosure, args []*jqNode, v jqVal, callerEnv *jqEnv, run func(*jqEnv) error) error {
	callEnv := newJqEnv(fn.env)
	callEnv.defs = make(map[jqFuncKey]*jqClosure, len(fn.params))
	for i, p := range fn.params {
		callEnv.defs[jqFuncKey{name: p.name}] = &jqClosure{body: args[i], env: callerEnv}
	}
	return st.bindUserParams(fn, args, 0, callEnv, v, callerEnv, run)
}

func (st *jqEvalState) bindUserParams(
	fn *jqClosure,
	args []*jqNode,
	idx int,
	callEnv *jqEnv,
	v jqVal,
	callerEnv *jqEnv,
	run func(*jqEnv) error,
) error {
	if idx >= len(fn.params) {
		return run(callEnv)
	}
	param := fn.params[idx]
	argExpr := args[idx]
	if !param.isVar {
		return st.bindUserParams(fn, args, idx+1, callEnv, v, callerEnv, run)
	}
	avals, err := st.evalList(argExpr, v, callerEnv)
	if err != nil {
		return err
	}
	for _, av := range avals {
		if err := st.bindUserParams(fn, args, idx+1, callEnv.withVar(param.name, av), v, callerEnv, run); err != nil {
			return err
		}
	}
	return nil
}

func (st *jqEvalState) evalRecurseAll(v jqVal, emit func(jqVal) error) error {
	if err := st.tick(); err != nil {
		return err
	}
	if err := emit(v); err != nil {
		return err
	}
	switch v.kind {
	case jqArray:
		for _, el := range v.arr {
			if err := st.evalRecurseAll(el, emit); err != nil {
				return err
			}
		}
	case jqObject:
		if v.obj != nil {
			for _, ent := range v.obj.entries {
				if err := st.evalRecurseAll(ent.val, emit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (st *jqEvalState) evalInterp(ie *jqNode, fmtName string, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	return st.evalInterpStep(ie, fmtName, 0, ie.parts[0], v, env, emit)
}

func (st *jqEvalState) evalInterpStep(ie *jqNode, fmtName string, idx int, prefix string, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	if idx >= len(ie.args) {
		return emit(jqMakeStr(prefix))
	}
	ivals, err := st.evalList(ie.args[idx], v, env)
	if err != nil {
		return err
	}
	for _, iv := range ivals {
		formatted, ferr := jqApplyFormat(fmtName, iv)
		if ferr != nil {
			return ferr
		}
		if err := st.evalInterpStep(ie, fmtName, idx+1, prefix+formatted+ie.parts[idx+1], v, env, emit); err != nil {
			return err
		}
	}
	return nil
}

func (st *jqEvalState) evalObjectFields(
	fields []jqObjField,
	idx int,
	acc []jqEntry,
	v jqVal,
	env *jqEnv,
	emit func(jqVal) error,
) error {
	if idx >= len(fields) {
		obj := newJqObj(len(acc))
		for _, ent := range acc {
			obj.set(ent.key, ent.val)
		}
		return emit(jqVal{kind: jqObject, obj: obj})
	}
	f := fields[idx]
	var keys []string
	if f.keyExpr == nil {
		keys = []string{f.staticKey}
	} else {
		kvals, err := st.evalList(f.keyExpr, v, env)
		if err != nil {
			return err
		}
		keys = make([]string, len(kvals))
		for i, kv := range kvals {
			if kv.kind != jqString {
				return jqErrStr("Cannot use " + kv.shortDesc() + " as object key")
			}
			keys[i] = kv.str
		}
	}
	fvals, err := st.evalList(f.valExpr, v, env)
	if err != nil {
		return err
	}
	for _, k := range keys {
		for _, fv := range fvals {
			nextAcc := make([]jqEntry, len(acc)+1)
			copy(nextAcc, acc)
			nextAcc[len(acc)] = jqEntry{key: k, val: fv}
			if err := st.evalObjectFields(fields, idx+1, nextAcc, v, env, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (st *jqEvalState) evalIfBranches(
	branches []jqIfBranch,
	idx int,
	elseExpr *jqNode,
	v jqVal,
	env *jqEnv,
	emit func(jqVal) error,
) error {
	if idx >= len(branches) {
		return st.eval(elseExpr, v, env, emit)
	}
	br := branches[idx]
	cvals, err := st.evalList(br.cond, v, env)
	if err != nil {
		return err
	}
	for _, cv := range cvals {
		if cv.isTruthy() {
			if err := st.eval(br.then, v, env, emit); err != nil {
				return err
			}
		} else {
			if err := st.evalIfBranches(branches, idx+1, elseExpr, v, env, emit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (st *jqEvalState) evalSliceSpecs(se *jqNode, v jqVal, env *jqEnv) ([]jqSliceSpec, error) {
	var starts []jqVal
	if se.b == nil {
		starts = []jqVal{jqMakeNull()}
	} else {
		var err error
		starts, err = st.evalList(se.b, v, env)
		if err != nil {
			return nil, err
		}
	}
	var ends []jqVal
	if se.c == nil {
		ends = []jqVal{jqMakeNull()}
	} else {
		var err error
		ends, err = st.evalList(se.c, v, env)
		if err != nil {
			return nil, err
		}
	}
	var out []jqSliceSpec
	for _, sv := range starts {
		var hasStart bool
		var si int
		if sv.kind != jqNull {
			if sv.kind != jqNumber {
				if se.optional {
					continue
				}
				return nil, jqErrStr("Slice index must be a number")
			}
			hasStart = true
			if sv.num.isInt() {
				si = int(sv.num.i)
			} else {
				si = int(sv.num.f)
			}
		}
		for _, ev := range ends {
			var hasEnd bool
			var ei int
			if ev.kind != jqNull {
				if ev.kind != jqNumber {
					if se.optional {
						continue
					}
					return nil, jqErrStr("Slice index must be a number")
				}
				hasEnd = true
				if ev.num.isInt() {
					ei = int(ev.num.i)
				} else {
					ei = int(ev.num.f)
				}
			}
			out = append(out, jqSliceSpec{hasStart: hasStart, start: si, hasEnd: hasEnd, end: ei})
		}
	}
	return out, nil
}

// --- Destructuring Pattern Matching ---

func (st *jqEvalState) bindPatternsAndEval(
	pats []*jqPattern,
	sv jqVal,
	origInput jqVal,
	env *jqEnv,
	body *jqNode,
	emit func(jqVal) error,
) error {
	var lastErr error
	for i, pat := range pats {
		subEnv := newJqEnv(env)
		ok, err := st.matchPattern(pat, sv, origInput, env, subEnv)
		if err != nil {
			var rtErr *jqRuntimeError
			if i+1 < len(pats) && errors.As(err, &rtErr) {
				lastErr = err
				continue
			}
			return err
		}
		if ok {
			return st.eval(body, origInput, subEnv, emit)
		}
		lastErr = jqErrStr("cannot destructure " + sv.shortDesc())
	}
	if lastErr != nil {
		return lastErr
	}
	return jqErrStr("cannot destructure " + sv.shortDesc())
}

func (st *jqEvalState) matchPattern(
	pat *jqPattern,
	sv jqVal,
	origInput jqVal,
	evalEnv *jqEnv,
	targetEnv *jqEnv,
) (bool, error) {
	switch pat.kind {
	case jqPatVar:
		targetEnv.setVar(pat.varName, sv)
		return true, nil

	case jqPatArray:
		if sv.kind != jqArray && sv.kind != jqNull {
			return false, nil
		}
		for i, sub := range pat.arrPats {
			elem := jqMakeNull()
			if sv.kind == jqArray && i < len(sv.arr) {
				elem = sv.arr[i]
			}
			ok, err := st.matchPattern(sub, elem, origInput, evalEnv, targetEnv)
			if !ok || err != nil {
				return ok, err
			}
		}
		return true, nil

	case jqPatObject:
		if sv.kind != jqObject && sv.kind != jqNull {
			return false, nil
		}
		for _, ent := range pat.objEntries {
			key := ent.staticKey
			if ent.keyExpr != nil {
				kvals, err := st.evalList(ent.keyExpr, origInput, evalEnv)
				if err != nil {
					return false, err
				}
				if len(kvals) == 0 {
					return false, nil
				}
				if kvals[0].kind != jqString {
					return false, jqErrStr("Cannot use " + kvals[0].shortDesc() + " as object key")
				}
				key = kvals[0].str
			}
			fieldVal := jqMakeNull()
			if sv.kind == jqObject && sv.obj != nil {
				if fv, exists := sv.obj.get(key); exists {
					fieldVal = fv
				}
			}
			if ent.bindVar != "" {
				targetEnv.setVar(ent.bindVar, fieldVal)
			}
			if ent.pat != nil {
				ok, err := st.matchPattern(ent.pat, fieldVal, origInput, evalEnv, targetEnv)
				if !ok || err != nil {
					return ok, err
				}
			}
		}
		return true, nil
	}
	return false, nil
}

// --- Binary Operators ---

func (st *jqEvalState) evalBinOp(e *jqNode, v jqVal, env *jqEnv, emit func(jqVal) error) error {
	switch e.tokOp {
	case jqTokAlt:
		anyTruthy := false
		err := st.eval(e.a, v, env, func(lv jqVal) error {
			if lv.isTruthy() {
				anyTruthy = true
				return emit(lv)
			}
			return nil
		})
		if err != nil || anyTruthy {
			return err
		}
		return st.eval(e.b, v, env, emit)

	case jqTokAnd, jqTokOr:
		isOr := e.tokOp == jqTokOr
		lvals, err := st.evalList(e.a, v, env)
		if err != nil {
			return err
		}
		for _, lv := range lvals {
			if lv.isTruthy() == isOr {
				if err := emit(jqMakeBool(isOr)); err != nil {
					return err
				}
				continue
			}
			rvals, rerr := st.evalList(e.b, v, env)
			if rerr != nil {
				return rerr
			}
			for _, rv := range rvals {
				if err := emit(jqMakeBool(rv.isTruthy())); err != nil {
					return err
				}
			}
		}
		return nil
	}

	lvals, err := st.evalList(e.a, v, env)
	if err != nil {
		return err
	}
	for _, lv := range lvals {
		rvals, rerr := st.evalList(e.b, v, env)
		if rerr != nil {
			return rerr
		}
		for _, rv := range rvals {
			res, berr := jqApplyBinOp(e.tokOp, lv, rv)
			if berr != nil {
				return berr
			}
			if err := emit(res); err != nil {
				return err
			}
		}
	}
	return nil
}

func jqApplyBinOp(op jqTokKind, a, b jqVal) (jqVal, error) {
	switch op {
	case jqTokEq:
		return jqMakeBool(jqEqual(a, b)), nil
	case jqTokNeq:
		return jqMakeBool(!jqEqual(a, b)), nil
	case jqTokLt:
		return jqMakeBool(jqCompare(a, b) < 0), nil
	case jqTokLe:
		return jqMakeBool(jqCompare(a, b) <= 0), nil
	case jqTokGt:
		return jqMakeBool(jqCompare(a, b) > 0), nil
	case jqTokGe:
		return jqMakeBool(jqCompare(a, b) >= 0), nil

	case jqTokPlus:
		if a.kind == jqNull {
			return b, nil
		}
		if b.kind == jqNull {
			return a, nil
		}
		if a.kind == jqNumber && b.kind == jqNumber {
			return jqAddNum(a.num, b.num), nil
		}
		if a.kind == jqString && b.kind == jqString {
			return jqMakeStr(a.str + b.str), nil
		}
		if a.kind == jqArray && b.kind == jqArray {
			out := make([]jqVal, 0, len(a.arr)+len(b.arr))
			out = append(out, a.arr...)
			out = append(out, b.arr...)
			return jqMakeArr(out), nil
		}
		if a.kind == jqObject && b.kind == jqObject {
			out := a.obj.clone()
			if b.obj != nil {
				for _, ent := range b.obj.entries {
					out.set(ent.key, ent.val.clone())
				}
			}
			return jqVal{kind: jqObject, obj: out}, nil
		}
		return jqMakeNull(), jqErrStr(a.shortDesc() + " and " + b.shortDesc() + " cannot be added")

	case jqTokMinus:
		if a.kind == jqNumber && b.kind == jqNumber {
			return jqSubNum(a.num, b.num), nil
		}
		if a.kind == jqArray && b.kind == jqArray {
			out := make([]jqVal, 0, len(a.arr))
			for _, el := range a.arr {
				excluded := false
				for _, rem := range b.arr {
					if jqEqual(el, rem) {
						excluded = true
						break
					}
				}
				if !excluded {
					out = append(out, el)
				}
			}
			return jqMakeArr(out), nil
		}
		return jqMakeNull(), jqErrStr(a.shortDesc() + " and " + b.shortDesc() + " cannot be subtracted")

	case jqTokStar:
		if a.kind == jqNumber && b.kind == jqNumber {
			return jqMulNum(a.num, b.num), nil
		}
		if a.kind == jqString && b.kind == jqNumber {
			return jqRepeatStr(a.str, b.num)
		}
		if a.kind == jqNumber && b.kind == jqString {
			return jqRepeatStr(b.str, a.num)
		}
		if a.kind == jqObject && b.kind == jqObject {
			return jqDeepMergeObjects(a.obj, b.obj), nil
		}
		return jqMakeNull(), jqErrStr(a.shortDesc() + " and " + b.shortDesc() + " cannot be multiplied")

	case jqTokSlash:
		if a.kind == jqNumber && b.kind == jqNumber {
			res, err := jqDivNum(a.num, b.num)
			if err != nil {
				return jqMakeNull(), jqErrStr(err.Error())
			}
			return res, nil
		}
		if a.kind == jqString && b.kind == jqString {
			parts := strings.Split(a.str, b.str)
			arr := make([]jqVal, len(parts))
			for i, p := range parts {
				arr[i] = jqMakeStr(p)
			}
			return jqMakeArr(arr), nil
		}
		return jqMakeNull(), jqErrStr(a.shortDesc() + " and " + b.shortDesc() + " cannot be divided")

	case jqTokPercent:
		if a.kind == jqNumber && b.kind == jqNumber {
			res, err := jqModNum(a.num, b.num)
			if err != nil {
				return jqMakeNull(), jqErrStr(err.Error())
			}
			return res, nil
		}
		return jqMakeNull(), jqErrStr(a.shortDesc() + " and " + b.shortDesc() + " cannot be divided (remainder)")
	}
	return jqMakeNull(), jqErrStr("unsupported binary operator")
}

func jqRepeatStr(s string, n jqNum) (jqVal, error) {
	f := n.asFloat()
	if f < 0 {
		return jqMakeNull(), nil
	}
	cnt := int(f)
	if cnt == 0 || s == "" {
		return jqMakeStr(""), nil
	}
	if len(s)*cnt > 64*1024*1024 {
		return jqMakeNull(), jqErrStr("Repeating string result too long")
	}
	return jqMakeStr(strings.Repeat(s, cnt)), nil
}

// --- Path Evaluator & Update Assignments ---

func appendPathStep(base []jqPathStep, st jqPathStep) []jqPathStep {
	out := make([]jqPathStep, len(base)+1)
	copy(out, base)
	out[len(base)] = st
	return out
}

func normalizeStepForTarget(tv jqVal, step jqPathStep) jqPathStep {
	if !step.isSlice && step.key.kind == jqNumber && tv.kind == jqArray {
		if idx, ok := step.key.num.asInt64(); ok && idx < 0 {
			norm := idx + int64(len(tv.arr))
			if norm >= 0 {
				return jqPathStep{key: jqMakeInt(norm)}
			}
		}
	}
	return step
}

type jqPathTarget struct {
	path []jqPathStep
	val  jqVal
}

func (st *jqEvalState) collectPathTargets(expr *jqNode, curVal jqVal, curPath []jqPathStep, env *jqEnv) ([]jqPathTarget, error) {
	var out []jqPathTarget
	err := st.pathOf(expr, curVal, curPath, env, func(p []jqPathStep, v jqVal) error {
		cp := make([]jqPathStep, len(p))
		copy(cp, p)
		out = append(out, jqPathTarget{path: cp, val: v})
		return nil
	})
	return out, err
}

func (st *jqEvalState) pathOf(
	e *jqNode,
	curVal jqVal,
	curPath []jqPathStep,
	env *jqEnv,
	emitPath func([]jqPathStep, jqVal) error,
) error {
	if err := st.tick(); err != nil {
		return err
	}
	switch e.kind {
	case jqNodeIdentity:
		return emitPath(curPath, curVal)

	case jqNodeField:
		targets, err := st.collectPathTargets(e.a, curVal, curPath, env)
		if err != nil {
			return err
		}
		var names []string
		if e.b == nil {
			names = []string{e.str}
		} else {
			kvals, kerr := st.evalList(e.b, curVal, env)
			if kerr != nil {
				return kerr
			}
			for _, kv := range kvals {
				if kv.kind != jqString {
					if e.optional {
						continue
					}
					return jqErrStr("Cannot index with " + kv.typeName())
				}
				names = append(names, kv.str)
			}
		}
		for _, t := range targets {
			for _, fname := range names {
				if t.val.kind != jqObject && t.val.kind != jqNull {
					if e.optional {
						continue
					}
					return jqErrStr("Cannot index " + t.val.typeName() + " with string")
				}
				step := jqPathStep{key: jqMakeStr(fname)}
				child, _ := jqGetStep(t.val, step)
				if err := emitPath(appendPathStep(t.path, step), child); err != nil {
					return err
				}
			}
		}
		return nil

	case jqNodeIndex:
		targets, err := st.collectPathTargets(e.a, curVal, curPath, env)
		if err != nil {
			return err
		}
		ivals, err := st.evalList(e.b, curVal, env)
		if err != nil {
			return err
		}
		for _, t := range targets {
			for _, iv := range ivals {
				okType := (iv.kind == jqString && (t.val.kind == jqObject || t.val.kind == jqNull)) ||
					(iv.kind == jqNumber && (t.val.kind == jqArray || t.val.kind == jqNull))
				if !okType {
					if e.optional {
						continue
					}
					return jqErrStr("Cannot index " + t.val.typeName() + " with " + iv.typeName())
				}
				step := normalizeStepForTarget(t.val, jqPathStep{key: iv})
				child, gerr := jqGetStep(t.val, step)
				if gerr != nil {
					if e.optional {
						continue
					}
					return jqErrStr(gerr.Error())
				}
				if err := emitPath(appendPathStep(t.path, step), child); err != nil {
					return err
				}
			}
		}
		return nil

	case jqNodeSlice:
		targets, err := st.collectPathTargets(e.a, curVal, curPath, env)
		if err != nil {
			return err
		}
		specs, err := st.evalSliceSpecs(e, curVal, env)
		if err != nil {
			return err
		}
		for _, t := range targets {
			if t.val.kind != jqArray && t.val.kind != jqNull {
				if e.optional {
					continue
				}
				return jqErrStr("Cannot slice " + t.val.shortDesc())
			}
			for _, spec := range specs {
				step := jqPathStep{isSlice: true, slice: spec}
				child, _ := jqGetStep(t.val, step)
				if err := emitPath(appendPathStep(t.path, step), child); err != nil {
					return err
				}
			}
		}
		return nil

	case jqNodeIter:
		targets, err := st.collectPathTargets(e.a, curVal, curPath, env)
		if err != nil {
			return err
		}
		for _, t := range targets {
			switch t.val.kind {
			case jqArray:
				for i, el := range t.val.arr {
					if err := emitPath(appendPathStep(t.path, jqPathStep{key: jqMakeInt(int64(i))}), el); err != nil {
						return err
					}
				}
			case jqObject:
				if t.val.obj != nil {
					for _, ent := range t.val.obj.entries {
						if err := emitPath(appendPathStep(t.path, jqPathStep{key: jqMakeStr(ent.key)}), ent.val); err != nil {
							return err
						}
					}
				}
			default:
				if !e.optional {
					return jqErrStr("Cannot iterate over " + t.val.shortDesc())
				}
			}
		}
		return nil

	case jqNodePipe:
		return st.pathOf(e.a, curVal, curPath, env, func(lp []jqPathStep, lv jqVal) error {
			return st.pathOf(e.b, lv, lp, env, emitPath)
		})

	case jqNodeComma:
		if err := st.pathOf(e.a, curVal, curPath, env, emitPath); err != nil {
			return err
		}
		return st.pathOf(e.b, curVal, curPath, env, emitPath)

	case jqNodeRecurse:
		return st.pathOfRecurseAll(curVal, curPath, emitPath)

	case jqNodeTry:
		err := st.pathOf(e.a, curVal, curPath, env, emitPath)
		var rtErr *jqRuntimeError
		if err != nil && errors.As(err, &rtErr) {
			if e.b == nil {
				return nil
			}
			return st.pathOf(e.b, rtErr.val, curPath, env, emitPath)
		}
		return err

	case jqNodeIf:
		return st.pathOfIf(e.branches, 0, e.a, curVal, curPath, env, emitPath)

	case jqNodeBind:
		svals, err := st.evalList(e.a, curVal, env)
		if err != nil {
			return err
		}
		for _, sv := range svals {
			matched := false
			for _, pat := range e.pats {
				subEnv := newJqEnv(env)
				ok, merr := st.matchPattern(pat, sv, curVal, env, subEnv)
				if merr == nil && ok {
					matched = true
					if err := st.pathOf(e.b, curVal, curPath, subEnv, emitPath); err != nil {
						return err
					}
					break
				}
			}
			if !matched {
				return jqErrStr("cannot destructure " + sv.shortDesc())
			}
		}
		return nil

	case jqNodeCall:
		if fn, ok := env.lookupFunc(e.str, len(e.args)); ok {
			return st.callUser(fn, e.args, curVal, env, func(ce *jqEnv) error { return st.pathOf(fn.body, curVal, curPath, ce, emitPath) })
		}
		return st.pathOfBuiltin(e.str, e.args, curVal, curPath, env, emitPath)

	case jqNodeDef:
		return st.pathOf(e.b, curVal, curPath, jqDefEnv(e, env), emitPath)

	case jqNodeLabel:
		return st.withLabel(e, env, func(le *jqEnv) error { return st.pathOf(e.a, curVal, curPath, le, emitPath) })

	case jqNodeBreak:
		return st.eval(e, curVal, env, nil)
	}
	return jqErrStr("Invalid path expression")
}

func (st *jqEvalState) pathOfRecurseAll(curVal jqVal, curPath []jqPathStep, emitPath func([]jqPathStep, jqVal) error) error {
	if err := st.tick(); err != nil {
		return err
	}
	if err := emitPath(curPath, curVal); err != nil {
		return err
	}
	switch curVal.kind {
	case jqArray:
		for i, el := range curVal.arr {
			if err := st.pathOfRecurseAll(el, appendPathStep(curPath, jqPathStep{key: jqMakeInt(int64(i))}), emitPath); err != nil {
				return err
			}
		}
	case jqObject:
		if curVal.obj != nil {
			for _, ent := range curVal.obj.entries {
				if err := st.pathOfRecurseAll(ent.val, appendPathStep(curPath, jqPathStep{key: jqMakeStr(ent.key)}), emitPath); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (st *jqEvalState) pathOfIf(
	branches []jqIfBranch,
	idx int,
	elseExpr *jqNode,
	curVal jqVal,
	curPath []jqPathStep,
	env *jqEnv,
	emitPath func([]jqPathStep, jqVal) error,
) error {
	if idx >= len(branches) {
		return st.pathOf(elseExpr, curVal, curPath, env, emitPath)
	}
	br := branches[idx]
	cvals, err := st.evalList(br.cond, curVal, env)
	if err != nil {
		return err
	}
	for _, cv := range cvals {
		if cv.isTruthy() {
			if err := st.pathOf(br.then, curVal, curPath, env, emitPath); err != nil {
				return err
			}
		} else if err := st.pathOfIf(branches, idx+1, elseExpr, curVal, curPath, env, emitPath); err != nil {
			return err
		}
	}
	return nil
}

func (st *jqEvalState) pathOfBuiltin(
	name string,
	args []*jqNode,
	curVal jqVal,
	curPath []jqPathStep,
	env *jqEnv,
	emitPath func([]jqPathStep, jqVal) error,
) error {
	switch {
	case name == "empty" && len(args) == 0:
		return nil
	case name == "error":
		return st.evalBuiltin(name, args, curVal, env, nil)
	case name == "getpath" && len(args) == 1:
		pvals, err := st.evalList(args[0], curVal, env)
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
			full := append(append([]jqPathStep(nil), curPath...), steps...)
			sub, gerr := jqGetPath(curVal, steps)
			if gerr != nil {
				return jqErrStr(gerr.Error())
			}
			if err := emitPath(full, sub); err != nil {
				return err
			}
		}
		return nil
	}
	return jqErrStr("Invalid path expression near call to " + name)
}

func (st *jqEvalState) collectPaths(pathExpr *jqNode, root jqVal, env *jqEnv) ([][]jqPathStep, error) {
	var paths [][]jqPathStep
	err := st.pathOf(pathExpr, root, nil, env, func(p []jqPathStep, _ jqVal) error {
		cp := make([]jqPathStep, len(p))
		copy(cp, p)
		paths = append(paths, cp)
		return nil
	})
	return paths, err
}

func (st *jqEvalState) evalUpdate(e *jqNode, root jqVal, env *jqEnv, emit func(jqVal) error) error {
	paths, err := st.collectPaths(e.a, root, env)
	if err != nil {
		return err
	}
	if e.tokOp == jqTokUpdate {
		doc := root.clone()
		var delList [][]jqPathStep
		for _, p := range paths {
			oldVal, gerr := jqGetPath(doc, p)
			if gerr != nil {
				return jqErrStr(gerr.Error())
			}
			var newVal jqVal
			got := false
			err := st.eval(e.b, oldVal, env, func(nv jqVal) error {
				newVal = nv
				got = true
				return errJqStopIter
			})
			if err != nil && !errors.Is(err, errJqStopIter) {
				return err
			}
			if !got {
				delList = append(delList, p)
			} else {
				var serr error
				doc, serr = jqSetPath(doc, p, newVal)
				if serr != nil {
					return jqErrStr(serr.Error())
				}
			}
		}
		if len(delList) > 0 {
			var derr error
			doc, derr = jqDelPaths(doc, delList)
			if derr != nil {
				return jqErrStr(derr.Error())
			}
		}
		return emit(doc)
	}

	rvals, err := st.evalList(e.b, root, env)
	if err != nil {
		return err
	}
	if e.tokOp == jqTokAssign {
		for _, rv := range rvals {
			doc := root.clone()
			for _, p := range paths {
				var serr error
				doc, serr = jqSetPath(doc, p, rv)
				if serr != nil {
					return jqErrStr(serr.Error())
				}
			}
			if err := emit(doc); err != nil {
				return err
			}
		}
		return nil
	}

	binOp := updateOpToBinOp(e.tokOp)
	for _, rv := range rvals {
		doc := root.clone()
		for _, p := range paths {
			oldVal, gerr := jqGetPath(doc, p)
			if gerr != nil {
				return jqErrStr(gerr.Error())
			}
			var updated jqVal
			if binOp == jqTokAlt {
				if oldVal.isTruthy() {
					updated = oldVal
				} else {
					updated = rv
				}
			} else {
				var berr error
				updated, berr = jqApplyBinOp(binOp, oldVal, rv)
				if berr != nil {
					return berr
				}
			}
			var serr error
			doc, serr = jqSetPath(doc, p, updated)
			if serr != nil {
				return jqErrStr(serr.Error())
			}
		}
		if err := emit(doc); err != nil {
			return err
		}
	}
	return nil
}

func updateOpToBinOp(op jqTokKind) jqTokKind {
	switch op {
	case jqTokPlusEq:
		return jqTokPlus
	case jqTokMinusEq:
		return jqTokMinus
	case jqTokStarEq:
		return jqTokStar
	case jqTokSlashEq:
		return jqTokSlash
	case jqTokPercentEq:
		return jqTokPercent
	case jqTokAltEq:
		return jqTokAlt
	default:
		return jqTokPlus
	}
}
