package unish

import "fmt"

type jqNodeKind uint8

const (
	jqNodeLit jqNodeKind = iota
	jqNodeIdentity
	jqNodeRecurse
	jqNodeVar
	jqNodeFormat
	jqNodeInterp
	jqNodeField
	jqNodeIndex
	jqNodeSlice
	jqNodeIter
	jqNodePipe
	jqNodeComma
	jqNodeUnary
	jqNodeBinOp
	jqNodeUpdate
	jqNodeArray
	jqNodeObject
	jqNodeIf
	jqNodeTry
	jqNodeBind
	jqNodeReduce
	jqNodeForeach
	jqNodeDef
	jqNodeCall
	jqNodeLabel
	jqNodeBreak
)

type jqObjField struct {
	staticKey string
	keyExpr   *jqNode
	valExpr   *jqNode
}

type jqIfBranch struct {
	cond *jqNode
	then *jqNode
}

type jqDefParam struct {
	name  string
	isVar bool
}

// jqNode is a unified concrete AST node, avoiding interface dispatch, itab
// tables, and 25 separate runtime type descriptors in the compiled binary.
type jqNode struct {
	kind     jqNodeKind
	tokOp    jqTokKind
	optional bool
	str      string
	val      jqVal

	a, b, c  *jqNode
	args     []*jqNode
	parts    []string
	fields   []jqObjField
	branches []jqIfBranch
	pats     []*jqPattern
	params   []jqDefParam
}

// --- Destructuring Patterns ---

type jqPatKind uint8

const (
	jqPatVar jqPatKind = iota
	jqPatArray
	jqPatObject
)

type jqPatObjEntry struct {
	staticKey string
	keyExpr   *jqNode
	bindVar   string
	pat       *jqPattern
}

type jqPattern struct {
	kind       jqPatKind
	varName    string
	arrPats    []*jqPattern
	objEntries []jqPatObjEntry
}

var jqSharedIdentity = &jqNode{kind: jqNodeIdentity}

// --- Parser ---

type jqParser struct {
	toks []jqToken
	pos  int
}

func jqParse(src string) (*jqNode, error) {
	toks, err := jqLexAll(src)
	if err != nil {
		return nil, err
	}
	p := &jqParser{toks: toks}
	if p.peek().kind == jqTokEOF {
		return jqSharedIdentity, nil
	}
	expr, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != jqTokEOF {
		return nil, p.errAt(p.peek(), "unexpected token "+p.peek().str)
	}
	return expr, nil
}

func (p *jqParser) peek() jqToken {
	if p.pos >= len(p.toks) {
		return jqToken{kind: jqTokEOF}
	}
	return p.toks[p.pos]
}

func (p *jqParser) next() jqToken {
	t := p.peek()
	if p.pos < len(p.toks) {
		p.pos++
	}
	return t
}

func (p *jqParser) expect(k jqTokKind, desc string) (jqToken, error) {
	t := p.peek()
	if t.kind != k {
		return t, p.errAt(t, "expected "+desc+", got "+t.str)
	}
	p.pos++
	return t, nil
}

func (p *jqParser) errAt(t jqToken, msg string) error {
	return fmt.Errorf("jq: compile error at pos %d: %s", t.pos, msg)
}

func (p *jqParser) parsePipe(allowComma bool) (*jqNode, error) {
	if p.peek().kind == jqTokDef {
		return p.parseDef(allowComma)
	}
	if p.peek().kind == jqTokLabel {
		p.next()
		vt, err := p.expect(jqTokVar, "$label")
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(jqTokPipe, "'|'"); err != nil {
			return nil, err
		}
		body, err := p.parsePipe(allowComma)
		if err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodeLabel, str: vt.str, a: body}, nil
	}

	var left *jqNode
	var err error
	if allowComma {
		left, err = p.parseComma()
	} else {
		left, err = p.parseUpdate()
	}
	if err != nil {
		return nil, err
	}

	if p.peek().kind == jqTokAs {
		p.next()
		pats, err := p.parsePatterns()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(jqTokPipe, "'|'"); err != nil {
			return nil, err
		}
		body, err := p.parsePipe(allowComma)
		if err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodeBind, a: left, b: body, pats: pats}, nil
	}

	if p.peek().kind == jqTokPipe {
		p.next()
		right, err := p.parsePipe(allowComma)
		if err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodePipe, a: left, b: right}, nil
	}
	return left, nil
}

func (p *jqParser) parseDef(allowComma bool) (*jqNode, error) {
	p.next()
	nameTok := p.peek()
	name, ok := nameTok.keywordOrIdent()
	if !ok {
		return nil, p.errAt(nameTok, "expected function name")
	}
	p.next()

	var params []jqDefParam
	if p.peek().kind == jqTokLParen {
		p.next()
		if p.peek().kind != jqTokRParen {
			for {
				pt := p.peek()
				if pt.kind == jqTokVar {
					p.next()
					params = append(params, jqDefParam{name: pt.str, isVar: true})
				} else if pname, ok := pt.keywordOrIdent(); ok {
					p.next()
					params = append(params, jqDefParam{name: pname, isVar: false})
				} else {
					return nil, p.errAt(pt, "expected parameter name")
				}
				if p.peek().kind == jqTokSemi {
					p.next()
					continue
				}
				break
			}
		}
		if _, err := p.expect(jqTokRParen, "')'"); err != nil {
			return nil, err
		}
	}

	if _, err := p.expect(jqTokColon, "':'"); err != nil {
		return nil, err
	}
	fnBody, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokSemi, "';'"); err != nil {
		return nil, err
	}
	next := jqSharedIdentity
	switch p.peek().kind {
	case jqTokEOF, jqTokRParen, jqTokRBrack, jqTokRBrace:
	default:
		next, err = p.parsePipe(allowComma)
		if err != nil {
			return nil, err
		}
	}
	return &jqNode{kind: jqNodeDef, str: name, params: params, a: fnBody, b: next}, nil
}

func (p *jqParser) parsePatterns() ([]*jqPattern, error) {
	first, err := p.parseSinglePattern()
	if err != nil {
		return nil, err
	}
	pats := []*jqPattern{first}
	for p.peek().kind == jqTokAltDestr {
		p.next()
		nx, err := p.parseSinglePattern()
		if err != nil {
			return nil, err
		}
		pats = append(pats, nx)
	}
	return pats, nil
}

func (p *jqParser) parseSinglePattern() (*jqPattern, error) {
	t := p.peek()
	switch t.kind {
	case jqTokVar:
		p.next()
		return &jqPattern{kind: jqPatVar, varName: t.str}, nil
	case jqTokLBrack:
		p.next()
		var elems []*jqPattern
		if p.peek().kind != jqTokRBrack {
			for {
				el, err := p.parseSinglePattern()
				if err != nil {
					return nil, err
				}
				elems = append(elems, el)
				if p.peek().kind == jqTokComma {
					p.next()
					continue
				}
				break
			}
		}
		if _, err := p.expect(jqTokRBrack, "']'"); err != nil {
			return nil, err
		}
		return &jqPattern{kind: jqPatArray, arrPats: elems}, nil
	case jqTokLBrace:
		p.next()
		var entries []jqPatObjEntry
		if p.peek().kind != jqTokRBrace {
			for {
				ent, err := p.parsePatObjEntry()
				if err != nil {
					return nil, err
				}
				entries = append(entries, ent)
				if p.peek().kind == jqTokComma {
					p.next()
					if p.peek().kind == jqTokRBrace {
						break
					}
					continue
				}
				break
			}
		}
		if _, err := p.expect(jqTokRBrace, "'}'"); err != nil {
			return nil, err
		}
		return &jqPattern{kind: jqPatObject, objEntries: entries}, nil
	default:
		return nil, p.errAt(t, "expected pattern")
	}
}

func (p *jqParser) parsePatObjEntry() (jqPatObjEntry, error) {
	t := p.peek()
	if t.kind == jqTokVar {
		p.next()
		if p.peek().kind == jqTokColon {
			p.next()
			sub, err := p.parseSinglePattern()
			if err != nil {
				return jqPatObjEntry{}, err
			}
			return jqPatObjEntry{staticKey: t.str, bindVar: t.str, pat: sub}, nil
		}
		return jqPatObjEntry{staticKey: t.str, bindVar: t.str}, nil
	}
	if t.kind == jqTokLParen {
		p.next()
		ke, err := p.parsePipe(true)
		if err != nil {
			return jqPatObjEntry{}, err
		}
		if _, err := p.expect(jqTokRParen, "')'"); err != nil {
			return jqPatObjEntry{}, err
		}
		if _, err := p.expect(jqTokColon, "':'"); err != nil {
			return jqPatObjEntry{}, err
		}
		sub, err := p.parseSinglePattern()
		if err != nil {
			return jqPatObjEntry{}, err
		}
		return jqPatObjEntry{keyExpr: ke, pat: sub}, nil
	}
	if t.kind == jqTokStr {
		p.next()
		if _, err := p.expect(jqTokColon, "':'"); err != nil {
			return jqPatObjEntry{}, err
		}
		sub, err := p.parseSinglePattern()
		if err != nil {
			return jqPatObjEntry{}, err
		}
		return jqPatObjEntry{staticKey: t.str, pat: sub}, nil
	}
	if name, ok := t.keywordOrIdent(); ok {
		p.next()
		if _, err := p.expect(jqTokColon, "':'"); err != nil {
			return jqPatObjEntry{}, err
		}
		sub, err := p.parseSinglePattern()
		if err != nil {
			return jqPatObjEntry{}, err
		}
		return jqPatObjEntry{staticKey: name, pat: sub}, nil
	}
	return jqPatObjEntry{}, p.errAt(t, "invalid object pattern entry")
}

func (p *jqParser) parseComma() (*jqNode, error) {
	left, err := p.parseUpdate()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == jqTokComma {
		p.next()
		right, err := p.parseUpdate()
		if err != nil {
			return nil, err
		}
		left = &jqNode{kind: jqNodeComma, a: left, b: right}
	}
	return left, nil
}

func isUpdateOp(k jqTokKind) bool {
	switch k {
	case jqTokAssign, jqTokUpdate, jqTokPlusEq, jqTokMinusEq,
		jqTokStarEq, jqTokSlashEq, jqTokPercentEq, jqTokAltEq:
		return true
	}
	return false
}

func (p *jqParser) parseUpdate() (*jqNode, error) {
	if p.peek().kind == jqTokTry {
		p.next()
		body, err := p.parseUpdate()
		if err != nil {
			return nil, err
		}
		var catchExpr *jqNode
		if p.peek().kind == jqTokCatch {
			p.next()
			catchExpr, err = p.parseUpdate()
			if err != nil {
				return nil, err
			}
		}
		return &jqNode{kind: jqNodeTry, a: body, b: catchExpr}, nil
	}

	left, err := p.parseAlt()
	if err != nil {
		return nil, err
	}
	if isUpdateOp(p.peek().kind) {
		op := p.next().kind
		right, err := p.parseUpdate()
		if err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodeUpdate, tokOp: op, a: left, b: right}, nil
	}
	return left, nil
}

func (p *jqParser) parseAlt() (*jqNode, error) {
	left, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind == jqTokAlt {
		p.next()
		right, err := p.parseAlt()
		if err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodeBinOp, tokOp: jqTokAlt, a: left, b: right}, nil
	}
	return left, nil
}

func (p *jqParser) parseOr() (*jqNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == jqTokOr {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &jqNode{kind: jqNodeBinOp, tokOp: jqTokOr, a: left, b: right}
	}
	return left, nil
}

func (p *jqParser) parseAnd() (*jqNode, error) {
	left, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == jqTokAnd {
		p.next()
		right, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		left = &jqNode{kind: jqNodeBinOp, tokOp: jqTokAnd, a: left, b: right}
	}
	return left, nil
}

func isCmpOp(k jqTokKind) bool {
	switch k {
	case jqTokEq, jqTokNeq, jqTokLt, jqTokLe, jqTokGt, jqTokGe:
		return true
	}
	return false
}

func (p *jqParser) parseCmp() (*jqNode, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for isCmpOp(p.peek().kind) {
		op := p.next().kind
		right, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		left = &jqNode{kind: jqNodeBinOp, tokOp: op, a: left, b: right}
	}
	return left, nil
}

func (p *jqParser) parseAdd() (*jqNode, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == jqTokPlus || p.peek().kind == jqTokMinus {
		op := p.next().kind
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left = &jqNode{kind: jqNodeBinOp, tokOp: op, a: left, b: right}
	}
	return left, nil
}

func (p *jqParser) parseMul() (*jqNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == jqTokStar || p.peek().kind == jqTokSlash || p.peek().kind == jqTokPercent {
		op := p.next().kind
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &jqNode{kind: jqNodeBinOp, tokOp: op, a: left, b: right}
	}
	return left, nil
}

func (p *jqParser) parseUnary() (*jqNode, error) {
	if p.peek().kind == jqTokMinus || p.peek().kind == jqTokPlus {
		op := p.next().kind
		sub, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if op == jqTokMinus && sub.kind == jqNodeLit && sub.val.kind == jqNumber {
			return &jqNode{kind: jqNodeLit, val: jqNegNum(sub.val.num)}, nil
		}
		return &jqNode{kind: jqNodeUnary, tokOp: op, a: sub}, nil
	}
	return p.parsePostfix()
}

func (p *jqParser) parsePostfix() (*jqNode, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().kind {
		case jqTokQMark:
			p.next()
			if (expr.kind == jqNodeField || expr.kind == jqNodeIndex || expr.kind == jqNodeSlice || expr.kind == jqNodeIter) && !expr.optional {
				expr.optional = true
				continue
			}
			expr = &jqNode{kind: jqNodeTry, a: expr}

		case jqTokDot:
			p.next()
			nt := p.peek()
			if nt.kind == jqTokLBrack {
				expr, err = p.parseBracketSuffix(expr)
				if err != nil {
					return nil, err
				}
				continue
			}
			if nt.kind == jqTokStr {
				p.next()
				expr = &jqNode{kind: jqNodeField, a: expr, str: nt.str}
				continue
			}
			if nt.kind == jqTokInterpStart {
				ie, err := p.parseInterpString()
				if err != nil {
					return nil, err
				}
				expr = &jqNode{kind: jqNodeField, a: expr, b: ie}
				continue
			}
			if fname, ok := nt.keywordOrIdent(); ok {
				p.next()
				expr = &jqNode{kind: jqNodeField, a: expr, str: fname}
				continue
			}
			return nil, p.errAt(nt, "expected field name or '[' after '.'")

		case jqTokLBrack:
			expr, err = p.parseBracketSuffix(expr)
			if err != nil {
				return nil, err
			}

		default:
			return expr, nil
		}
	}
}

func (p *jqParser) parseBracketSuffix(target *jqNode) (*jqNode, error) {
	p.next()
	if p.peek().kind == jqTokRBrack {
		p.next()
		return &jqNode{kind: jqNodeIter, a: target}, nil
	}
	if p.peek().kind == jqTokColon {
		p.next()
		var endExpr *jqNode
		if p.peek().kind != jqTokRBrack {
			var err error
			endExpr, err = p.parsePipe(true)
			if err != nil {
				return nil, err
			}
		}
		if _, err := p.expect(jqTokRBrack, "']'"); err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodeSlice, a: target, c: endExpr}, nil
	}
	first, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	if p.peek().kind == jqTokColon {
		p.next()
		var endExpr *jqNode
		if p.peek().kind != jqTokRBrack {
			endExpr, err = p.parsePipe(true)
			if err != nil {
				return nil, err
			}
		}
		if _, err := p.expect(jqTokRBrack, "']'"); err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodeSlice, a: target, b: first, c: endExpr}, nil
	}
	if _, err := p.expect(jqTokRBrack, "']'"); err != nil {
		return nil, err
	}
	return &jqNode{kind: jqNodeIndex, a: target, b: first}, nil
}

func (p *jqParser) parsePrimary() (*jqNode, error) {
	t := p.peek()
	switch t.kind {
	case jqTokDot:
		p.next()
		nt := p.peek()
		if nt.kind == jqTokStr {
			p.next()
			return &jqNode{kind: jqNodeField, a: jqSharedIdentity, str: nt.str}, nil
		}
		if nt.kind == jqTokInterpStart {
			ie, err := p.parseInterpString()
			if err != nil {
				return nil, err
			}
			return &jqNode{kind: jqNodeField, a: jqSharedIdentity, b: ie}, nil
		}
		if fname, ok := nt.keywordOrIdent(); ok {
			if nt.noWS || !isJqClauseDelimiter(nt.kind) {
				p.next()
				return &jqNode{kind: jqNodeField, a: jqSharedIdentity, str: fname}, nil
			}
		}
		return jqSharedIdentity, nil

	case jqTokDotDot:
		p.next()
		return &jqNode{kind: jqNodeRecurse}, nil

	case jqTokNull:
		p.next()
		return &jqNode{kind: jqNodeLit, val: jqMakeNull()}, nil

	case jqTokTrue:
		p.next()
		return &jqNode{kind: jqNodeLit, val: jqMakeBool(true)}, nil

	case jqTokFalse:
		p.next()
		return &jqNode{kind: jqNodeLit, val: jqMakeBool(false)}, nil

	case jqTokNum:
		p.next()
		lit := jqVal{kind: jqNumber, num: t.num}
		if t.big {
			lit.str = t.str
		}
		return &jqNode{kind: jqNodeLit, val: lit}, nil

	case jqTokStr:
		p.next()
		return &jqNode{kind: jqNodeLit, val: jqMakeStr(t.str)}, nil

	case jqTokInterpStart:
		return p.parseInterpString()

	case jqTokFormat:
		p.next()
		if p.peek().kind == jqTokStr {
			st := p.next()
			return &jqNode{kind: jqNodeLit, val: jqMakeStr(st.str)}, nil
		}
		if p.peek().kind == jqTokInterpStart {
			ie, err := p.parseInterpString()
			if err != nil {
				return nil, err
			}
			return &jqNode{kind: jqNodeFormat, str: t.str, a: ie}, nil
		}
		return &jqNode{kind: jqNodeFormat, str: t.str}, nil

	case jqTokVar:
		p.next()
		return &jqNode{kind: jqNodeVar, str: t.str}, nil

	case jqTokNot:
		p.next()
		return &jqNode{kind: jqNodeCall, str: "not"}, nil

	case jqTokLParen:
		p.next()
		inner, err := p.parsePipe(true)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(jqTokRParen, "')'"); err != nil {
			return nil, err
		}
		return inner, nil

	case jqTokLBrack:
		p.next()
		if p.peek().kind == jqTokRBrack {
			p.next()
			return &jqNode{kind: jqNodeArray}, nil
		}
		inner, err := p.parsePipe(true)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(jqTokRBrack, "']'"); err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodeArray, a: inner}, nil

	case jqTokLBrace:
		return p.parseObjectConstructor()

	case jqTokIf:
		return p.parseIf()

	case jqTokReduce:
		return p.parseReduce()

	case jqTokForeach:
		return p.parseForeach()

	case jqTokBreak:
		p.next()
		vt, err := p.expect(jqTokVar, "$label")
		if err != nil {
			return nil, err
		}
		return &jqNode{kind: jqNodeBreak, str: vt.str}, nil

	case jqTokIdent:
		p.next()
		var args []*jqNode
		if p.peek().kind == jqTokLParen {
			p.next()
			if p.peek().kind != jqTokRParen {
				for {
					arg, err := p.parsePipe(true)
					if err != nil {
						return nil, err
					}
					args = append(args, arg)
					if p.peek().kind == jqTokSemi {
						p.next()
						continue
					}
					break
				}
			}
			if _, err := p.expect(jqTokRParen, "')'"); err != nil {
				return nil, err
			}
		}
		return &jqNode{kind: jqNodeCall, str: t.str, args: args}, nil

	default:
		return nil, p.errAt(t, "unexpected token "+t.str)
	}
}

func isJqClauseDelimiter(k jqTokKind) bool {
	switch k {
	case jqTokThen, jqTokElif, jqTokElse, jqTokEnd,
		jqTokAnd, jqTokOr, jqTokAs, jqTokCatch:
		return true
	}
	return false
}

func (p *jqParser) parseInterpString() (*jqNode, error) {
	first := p.next()
	parts := []string{first.str}
	var exprs []*jqNode
	for {
		ex, err := p.parsePipe(true)
		if err != nil {
			return nil, err
		}
		exprs = append(exprs, ex)
		sep := p.peek()
		if sep.kind == jqTokInterpMid {
			p.next()
			parts = append(parts, sep.str)
			continue
		}
		if sep.kind == jqTokInterpEnd {
			p.next()
			parts = append(parts, sep.str)
			break
		}
		return nil, p.errAt(sep, "expected ')' closing string interpolation")
	}
	return &jqNode{kind: jqNodeInterp, parts: parts, args: exprs}, nil
}

func (p *jqParser) parseObjectConstructor() (*jqNode, error) {
	p.next()
	var fields []jqObjField
	for p.peek().kind != jqTokRBrace && p.peek().kind != jqTokEOF {
		f, err := p.parseObjectField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
		if p.peek().kind == jqTokComma {
			p.next()
			continue
		}
		break
	}
	if _, err := p.expect(jqTokRBrace, "'}'"); err != nil {
		return nil, err
	}
	return &jqNode{kind: jqNodeObject, fields: fields}, nil
}

func (p *jqParser) parseObjectField() (jqObjField, error) {
	t := p.peek()
	if t.kind == jqTokLParen {
		p.next()
		ke, err := p.parsePipe(true)
		if err != nil {
			return jqObjField{}, err
		}
		if _, err := p.expect(jqTokRParen, "')'"); err != nil {
			return jqObjField{}, err
		}
		if _, err := p.expect(jqTokColon, "':'"); err != nil {
			return jqObjField{}, err
		}
		ve, err := p.parsePipe(false)
		if err != nil {
			return jqObjField{}, err
		}
		return jqObjField{keyExpr: ke, valExpr: ve}, nil
	}

	if t.kind == jqTokVar {
		p.next()
		if p.peek().kind == jqTokColon {
			p.next()
			ve, err := p.parsePipe(false)
			if err != nil {
				return jqObjField{}, err
			}
			return jqObjField{keyExpr: &jqNode{kind: jqNodeVar, str: t.str}, valExpr: ve}, nil
		}
		return jqObjField{staticKey: t.str, valExpr: &jqNode{kind: jqNodeVar, str: t.str}}, nil
	}

	if t.kind == jqTokStr {
		p.next()
		if p.peek().kind == jqTokColon {
			p.next()
			ve, err := p.parsePipe(false)
			if err != nil {
				return jqObjField{}, err
			}
			return jqObjField{staticKey: t.str, valExpr: ve}, nil
		}
		return jqObjField{
			staticKey: t.str,
			valExpr:   &jqNode{kind: jqNodeField, a: jqSharedIdentity, str: t.str},
		}, nil
	}

	if t.kind == jqTokInterpStart {
		ie, err := p.parseInterpString()
		if err != nil {
			return jqObjField{}, err
		}
		if p.peek().kind == jqTokColon {
			p.next()
			ve, err := p.parsePipe(false)
			if err != nil {
				return jqObjField{}, err
			}
			return jqObjField{keyExpr: ie, valExpr: ve}, nil
		}
		return jqObjField{
			keyExpr: ie,
			valExpr: &jqNode{kind: jqNodeField, a: jqSharedIdentity, b: ie},
		}, nil
	}

	if name, ok := t.keywordOrIdent(); ok {
		p.next()
		if p.peek().kind == jqTokColon {
			p.next()
			ve, err := p.parsePipe(false)
			if err != nil {
				return jqObjField{}, err
			}
			return jqObjField{staticKey: name, valExpr: ve}, nil
		}
		return jqObjField{
			staticKey: name,
			valExpr:   &jqNode{kind: jqNodeField, a: jqSharedIdentity, str: name},
		}, nil
	}

	return jqObjField{}, p.errAt(t, "expected object key")
}

func (p *jqParser) parseIf() (*jqNode, error) {
	p.next()
	cond, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokThen, "'then'"); err != nil {
		return nil, err
	}
	thenExpr, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	branches := []jqIfBranch{{cond: cond, then: thenExpr}}
	for p.peek().kind == jqTokElif {
		p.next()
		ec, err := p.parsePipe(true)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(jqTokThen, "'then'"); err != nil {
			return nil, err
		}
		et, err := p.parsePipe(true)
		if err != nil {
			return nil, err
		}
		branches = append(branches, jqIfBranch{cond: ec, then: et})
	}
	elseExpr := jqSharedIdentity
	if p.peek().kind == jqTokElse {
		p.next()
		elseExpr, err = p.parsePipe(true)
		if err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(jqTokEnd, "'end'"); err != nil {
		return nil, err
	}
	return &jqNode{kind: jqNodeIf, branches: branches, a: elseExpr}, nil
}

func (p *jqParser) parseReduce() (*jqNode, error) {
	p.next()
	src, err := p.parseComma()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokAs, "'as'"); err != nil {
		return nil, err
	}
	pat, err := p.parseSinglePattern()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokLParen, "'('"); err != nil {
		return nil, err
	}
	initExpr, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokSemi, "';'"); err != nil {
		return nil, err
	}
	updateExpr, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokRParen, "')'"); err != nil {
		return nil, err
	}
	return &jqNode{kind: jqNodeReduce, a: src, b: initExpr, c: updateExpr, pats: []*jqPattern{pat}}, nil
}

func (p *jqParser) parseForeach() (*jqNode, error) {
	p.next()
	src, err := p.parseComma()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokAs, "'as'"); err != nil {
		return nil, err
	}
	pat, err := p.parseSinglePattern()
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokLParen, "'('"); err != nil {
		return nil, err
	}
	initExpr, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	if _, err := p.expect(jqTokSemi, "';'"); err != nil {
		return nil, err
	}
	updateExpr, err := p.parsePipe(true)
	if err != nil {
		return nil, err
	}
	var extractExpr *jqNode
	if p.peek().kind == jqTokSemi {
		p.next()
		extractExpr, err = p.parsePipe(true)
		if err != nil {
			return nil, err
		}
	}
	if _, err := p.expect(jqTokRParen, "')'"); err != nil {
		return nil, err
	}
	var extra []*jqNode
	if extractExpr != nil {
		extra = []*jqNode{extractExpr}
	}
	return &jqNode{kind: jqNodeForeach, a: src, b: initExpr, c: updateExpr, args: extra, pats: []*jqPattern{pat}}, nil
}
