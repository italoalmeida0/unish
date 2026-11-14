package unish

import (
	"fmt"
	"math"
)

type awkParser struct {
	tokens    []awkToken
	pos       int
	userFuncs map[string]bool

	inPrint   bool // true while parsing top-level print/printf arguments
	paren     int  // nesting depth of (...) and [...]
	inFunc    bool // true inside a `function ... { ... }` body
	inSpecial string
}

func parseAwkProgram(src string) (*awkProgram, error) {
	lex := newAwkLexer(src)
	tokens, err := lex.tokenizeAll()
	if err != nil {
		return nil, err
	}

	userFuncs := map[string]bool{}
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i].kind == awkTokFunction && tokens[i+1].kind == awkTokIdent {
			userFuncs[tokens[i+1].str] = true
		}
	}

	p := &awkParser{
		tokens:    tokens,
		userFuncs: userFuncs,
	}
	return p.parseProgram()
}

func (p *awkParser) cur() awkToken {
	if p.pos >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[p.pos]
}

func (p *awkParser) peek(offset int) awkToken {
	idx := p.pos + offset
	if idx >= len(p.tokens) {
		return p.tokens[len(p.tokens)-1]
	}
	return p.tokens[idx]
}

func (p *awkParser) advance() awkToken {
	tok := p.cur()
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return tok
}

func (p *awkParser) skipNewlines() {
	for p.cur().kind == awkTokNewline {
		p.advance()
	}
}

func (p *awkParser) skipTerminators() {
	for p.cur().kind == awkTokNewline || p.cur().kind == awkTokSemi {
		p.advance()
	}
}

func (p *awkParser) expect(kind awkTokKind, desc string) (awkToken, error) {
	tok := p.cur()
	if tok.kind != kind {
		return awkToken{}, fmt.Errorf("line %d: expected %s, got %s", tok.line, desc, p.tokDesc(tok))
	}
	p.advance()
	return tok, nil
}

func (p *awkParser) tokDesc(tok awkToken) string {
	switch tok.kind {
	case awkTokEOF:
		return "end of program"
	case awkTokNewline:
		return "newline"
	case awkTokString:
		return fmt.Sprintf("%q", tok.str)
	case awkTokRegex:
		return "/" + tok.str + "/"
	default:
		if tok.str != "" {
			return fmt.Sprintf("'%s'", tok.str)
		}
		return fmt.Sprintf("token(%d)", tok.kind)
	}
}

func (p *awkParser) parseProgram() (*awkProgram, error) {
	prog := &awkProgram{
		funcs: make(map[string]*awkFuncDef),
	}
	p.skipTerminators()
	for p.cur().kind != awkTokEOF {
		switch p.cur().kind {
		case awkTokFunction:
			fn, err := p.parseFuncDef()
			if err != nil {
				return nil, err
			}
			if _, dup := prog.funcs[fn.name]; dup {
				return nil, fmt.Errorf("line %d: duplicate function %q", p.cur().line, fn.name)
			}
			prog.funcs[fn.name] = fn

		case awkTokBEGIN:
			p.advance()
			p.skipNewlines()
			p.inSpecial = "BEGIN"
			blk, err := p.parseBlock()
			p.inSpecial = ""
			if err != nil {
				return nil, err
			}
			prog.begins = append(prog.begins, blk)

		case awkTokEND:
			p.advance()
			p.skipNewlines()
			p.inSpecial = "END"
			blk, err := p.parseBlock()
			p.inSpecial = ""
			if err != nil {
				return nil, err
			}
			prog.ends = append(prog.ends, blk)

		case awkTokLBrace:
			blk, err := p.parseBlock()
			if err != nil {
				return nil, err
			}
			prog.rules = append(prog.rules, &awkRule{action: blk})

		default:
			pat1, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			var pat2 awkExpr
			if p.cur().kind == awkTokComma {
				p.advance()
				p.skipNewlines()
				pat2, err = p.parseExpr()
				if err != nil {
					return nil, err
				}
			}
			var action *awkBlockStmt
			if p.cur().kind == awkTokLBrace {
				action, err = p.parseBlock()
				if err != nil {
					return nil, err
				}
			} else if p.cur().kind != awkTokNewline && p.cur().kind != awkTokSemi && p.cur().kind != awkTokEOF {
				return nil, fmt.Errorf("line %d: expected '{' or newline after pattern, got %s", p.cur().line, p.tokDesc(p.cur()))
			}
			prog.rules = append(prog.rules, &awkRule{
				pat1:   pat1,
				pat2:   pat2,
				action: action,
			})
		}
		p.skipTerminators()
	}
	return prog, nil
}

func (p *awkParser) parseFuncDef() (*awkFuncDef, error) {
	fnTok := p.advance() // consume 'function' or 'func'
	nameTok, err := p.expect(awkTokIdent, "function name")
	if err != nil {
		return nil, err
	}
	if resolveAwkBuiltinFunc(nameTok.str) != awkFnNone {
		return nil, fmt.Errorf("line %d: cannot redefine built-in function %q", fnTok.line, nameTok.str)
	}
	if _, err := p.expect(awkTokLParen, "'('"); err != nil {
		return nil, err
	}
	var params []string
	seenParam := map[string]bool{}
	p.skipNewlines()
	if p.cur().kind != awkTokRParen {
		for {
			p.skipNewlines()
			pt, err := p.expect(awkTokIdent, "parameter name")
			if err != nil {
				return nil, err
			}
			if seenParam[pt.str] {
				return nil, fmt.Errorf("line %d: duplicate parameter %q in function %q", pt.line, pt.str, nameTok.str)
			}
			seenParam[pt.str] = true
			params = append(params, pt.str)
			p.skipNewlines()
			if p.cur().kind != awkTokComma {
				break
			}
			p.advance()
		}
	}
	if _, err := p.expect(awkTokRParen, "')'"); err != nil {
		return nil, err
	}
	p.skipNewlines()
	prevInFunc := p.inFunc
	p.inFunc = true
	body, err := p.parseBlock()
	p.inFunc = prevInFunc
	if err != nil {
		return nil, err
	}
	return &awkFuncDef{
		name:   nameTok.str,
		params: params,
		body:   body,
	}, nil
}

func (p *awkParser) parseBlock() (*awkBlockStmt, error) {
	if _, err := p.expect(awkTokLBrace, "'{'"); err != nil {
		return nil, err
	}
	var stmts []awkStmt
	p.skipTerminators()
	for p.cur().kind != awkTokRBrace && p.cur().kind != awkTokEOF {
		st, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if st != nil {
			stmts = append(stmts, st)
		}
		// Statements inside blocks must be followed by ';', newline, or '}'
		// unless the statement itself ended with a block '}'.
		if p.cur().kind != awkTokRBrace && p.cur().kind != awkTokEOF {
			if p.cur().kind == awkTokNewline || p.cur().kind == awkTokSemi {
				p.skipTerminators()
			} else if !stmtEndsWithBlock(st) {
				return nil, fmt.Errorf("line %d: expected ';' or newline before %s", p.cur().line, p.tokDesc(p.cur()))
			}
		}
	}
	if _, err := p.expect(awkTokRBrace, "'}'"); err != nil {
		return nil, err
	}
	return &awkBlockStmt{stmts: stmts}, nil
}

func stmtEndsWithBlock(st awkStmt) bool {
	switch s := st.(type) {
	case *awkBlockStmt:
		return true
	case *awkIfStmt:
		if s.elseStmt != nil {
			return stmtEndsWithBlock(s.elseStmt)
		}
		return stmtEndsWithBlock(s.thenStmt)
	case *awkWhileStmt:
		return stmtEndsWithBlock(s.body)
	case *awkForStmt:
		return stmtEndsWithBlock(s.body)
	case *awkForInStmt:
		return stmtEndsWithBlock(s.body)
	default:
		return false
	}
}

func (p *awkParser) parseStmt() (awkStmt, error) {
	switch p.cur().kind {
	case awkTokSemi, awkTokNewline:
		p.advance()
		return nil, nil

	case awkTokLBrace:
		return p.parseBlock()

	case awkTokIf:
		return p.parseIfStmt()

	case awkTokWhile:
		return p.parseWhileStmt()

	case awkTokDo:
		return p.parseDoWhileStmt()

	case awkTokFor:
		return p.parseForStmt()

	case awkTokBreak:
		p.advance()
		return &awkBreakStmt{}, nil

	case awkTokContinue:
		p.advance()
		return &awkContinueStmt{}, nil

	case awkTokNext:
		tok := p.advance()
		if p.inSpecial != "" {
			return nil, fmt.Errorf("line %d: 'next' is not allowed inside %s", tok.line, p.inSpecial)
		}
		return &awkNextStmt{}, nil

	case awkTokNextfile:
		tok := p.advance()
		if p.inSpecial != "" {
			return nil, fmt.Errorf("line %d: 'nextfile' is not allowed inside %s", tok.line, p.inSpecial)
		}
		return &awkNextfileStmt{}, nil

	case awkTokExit:
		p.advance()
		var code awkExpr
		if p.canStartExpr() {
			var err error
			code, err = p.parseExpr()
			if err != nil {
				return nil, err
			}
		}
		return &awkExitStmt{code: code}, nil

	case awkTokReturn:
		tok := p.advance()
		if !p.inFunc {
			return nil, fmt.Errorf("line %d: 'return' outside function", tok.line)
		}
		var val awkExpr
		if p.canStartExpr() {
			var err error
			val, err = p.parseExpr()
			if err != nil {
				return nil, err
			}
		}
		return &awkReturnStmt{val: val}, nil

	case awkTokDelete:
		return p.parseDeleteStmt()

	case awkTokPrint:
		return p.parsePrintStmt(false)

	case awkTokPrintf:
		return p.parsePrintStmt(true)

	default:
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		return &awkExprStmt{expr: expr}, nil
	}
}

func (p *awkParser) parseSimpleStmt() (awkStmt, error) {
	switch p.cur().kind {
	case awkTokDelete:
		return p.parseDeleteStmt()
	case awkTokPrint:
		return p.parsePrintStmt(false)
	case awkTokPrintf:
		return p.parsePrintStmt(true)
	default:
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		return &awkExprStmt{expr: expr}, nil
	}
}

func (p *awkParser) parseIfStmt() (*awkIfStmt, error) {
	p.advance() // 'if'
	if _, err := p.expect(awkTokLParen, "'('"); err != nil {
		return nil, err
	}
	p.paren++
	cond, err := p.parseExpr()
	p.paren--
	if err != nil {
		return nil, err
	}
	p.skipNewlines()
	if _, err := p.expect(awkTokRParen, "')'"); err != nil {
		return nil, err
	}
	p.skipNewlines()
	thenStmt, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	if thenStmt == nil {
		thenStmt = &awkBlockStmt{}
	}

	// Peek past optional ';' and newlines to see if 'else' follows.
	look := p.pos
	for look < len(p.tokens) && (p.tokens[look].kind == awkTokSemi || p.tokens[look].kind == awkTokNewline) {
		look++
	}
	var elseStmt awkStmt
	if look < len(p.tokens) && p.tokens[look].kind == awkTokElse {
		p.pos = look + 1 // consume terminators + 'else'
		p.skipNewlines()
		elseStmt, err = p.parseStmt()
		if err != nil {
			return nil, err
		}
		if elseStmt == nil {
			elseStmt = &awkBlockStmt{}
		}
	}
	return &awkIfStmt{cond: cond, thenStmt: thenStmt, elseStmt: elseStmt}, nil
}

func (p *awkParser) parseWhileStmt() (*awkWhileStmt, error) {
	p.advance() // 'while'
	if _, err := p.expect(awkTokLParen, "'('"); err != nil {
		return nil, err
	}
	p.paren++
	cond, err := p.parseExpr()
	p.paren--
	if err != nil {
		return nil, err
	}
	p.skipNewlines()
	if _, err := p.expect(awkTokRParen, "')'"); err != nil {
		return nil, err
	}
	p.skipNewlines()
	body, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	if body == nil {
		body = &awkBlockStmt{}
	}
	return &awkWhileStmt{cond: cond, body: body}, nil
}

func (p *awkParser) parseDoWhileStmt() (*awkDoWhileStmt, error) {
	p.advance() // 'do'
	p.skipNewlines()
	body, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	if body == nil {
		body = &awkBlockStmt{}
	}
	p.skipTerminators()
	if _, err := p.expect(awkTokWhile, "'while'"); err != nil {
		return nil, err
	}
	if _, err := p.expect(awkTokLParen, "'('"); err != nil {
		return nil, err
	}
	p.paren++
	cond, err := p.parseExpr()
	p.paren--
	if err != nil {
		return nil, err
	}
	p.skipNewlines()
	if _, err := p.expect(awkTokRParen, "')'"); err != nil {
		return nil, err
	}
	return &awkDoWhileStmt{body: body, cond: cond}, nil
}

func (p *awkParser) parseForStmt() (awkStmt, error) {
	p.advance() // 'for'
	if _, err := p.expect(awkTokLParen, "'('"); err != nil {
		return nil, err
	}
	p.skipNewlines()
	// Check for `for (ident in arr)`
	if p.cur().kind == awkTokIdent && p.peek(1).kind == awkTokIn {
		varTok := p.advance()
		p.advance() // 'in'
		arrTok, err := p.expect(awkTokIdent, "array name")
		if err != nil {
			return nil, err
		}
		p.skipNewlines()
		if _, err := p.expect(awkTokRParen, "')'"); err != nil {
			return nil, err
		}
		p.skipNewlines()
		body, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		if body == nil {
			body = &awkBlockStmt{}
		}
		return &awkForInStmt{varName: varTok.str, arrName: arrTok.str, body: body}, nil
	}

	// C-style `for (init; cond; post)`
	p.paren++
	var initStmt awkStmt
	if p.cur().kind != awkTokSemi {
		var err error
		initStmt, err = p.parseSimpleStmt()
		if err != nil {
			p.paren--
			return nil, err
		}
	}
	if _, err := p.expect(awkTokSemi, "';'"); err != nil {
		p.paren--
		return nil, err
	}
	p.skipNewlines()
	var cond awkExpr
	if p.cur().kind != awkTokSemi {
		var err error
		cond, err = p.parseExpr()
		if err != nil {
			p.paren--
			return nil, err
		}
	}
	if _, err := p.expect(awkTokSemi, "';'"); err != nil {
		p.paren--
		return nil, err
	}
	p.skipNewlines()
	var postStmt awkStmt
	if p.cur().kind != awkTokRParen {
		var err error
		postStmt, err = p.parseSimpleStmt()
		if err != nil {
			p.paren--
			return nil, err
		}
	}
	p.paren--
	p.skipNewlines()
	if _, err := p.expect(awkTokRParen, "')'"); err != nil {
		return nil, err
	}
	p.skipNewlines()
	body, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	if body == nil {
		body = &awkBlockStmt{}
	}
	return &awkForStmt{init: initStmt, cond: cond, post: postStmt, body: body}, nil
}

func (p *awkParser) parseDeleteStmt() (*awkDeleteStmt, error) {
	p.advance() // 'delete'
	arrTok, err := p.expect(awkTokIdent, "array name")
	if err != nil {
		return nil, err
	}
	var indices []awkExpr
	if p.cur().kind == awkTokLBracket {
		p.advance()
		p.paren++
		indices, err = p.parseExprList()
		p.paren--
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(awkTokRBracket, "']'"); err != nil {
			return nil, err
		}
	}
	return &awkDeleteStmt{arrName: arrTok.str, indices: indices}, nil
}

func (p *awkParser) parsePrintStmt(isPrintf bool) (*awkPrintStmt, error) {
	kwTok := p.advance() // 'print' or 'printf'
	prevInPrint := p.inPrint
	prevParen := p.paren
	p.inPrint = true
	p.paren = 0
	defer func() {
		p.inPrint = prevInPrint
		p.paren = prevParen
	}()

	var args []awkExpr

	// Handle parenthesized `print(a, b)` / `printf(fmt, a, b)` when applicable.
	if p.cur().kind == awkTokLParen {
		savedPos := p.pos
		p.advance() // '('
		p.paren++
		var trialArgs []awkExpr
		var trialErr error
		if p.cur().kind != awkTokRParen {
			trialArgs, trialErr = p.parseExprList()
		}
		p.skipNewlines()
		p.paren--
		if trialErr == nil && p.cur().kind == awkTokRParen {
			p.advance() // ')'
			// Accept as parenthesized print/printf arglist if either empty `print()`
			// or >=2 args and not followed by `in` (tuple membership) or binary operator.
			if (len(trialArgs) == 0 || len(trialArgs) >= 2) && p.isPrintEndOrRedir() {
				args = trialArgs
				goto parseRedir
			}
		}
		// Otherwise rewind and parse as standard expression list (e.g. `print ($1 > 2), $3` or `print (1+2)*3`).
		p.pos = savedPos
	}

	if !p.isPrintEndOrRedir() && p.canStartExpr() {
		for {
			arg, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
			if p.cur().kind != awkTokComma {
				break
			}
			p.advance()
			p.skipNewlines()
		}
	}

parseRedir:
	if isPrintf && len(args) == 0 {
		return nil, fmt.Errorf("line %d: printf requires at least a format argument", kwTok.line)
	}

	var redirOp awkRedirOp
	var redirTarget awkExpr
	if p.cur().kind == awkTokGT {
		p.advance()
		redirOp = awkRedirTrunc
		var err error
		redirTarget, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	} else if p.cur().kind == awkTokAppend {
		p.advance()
		redirOp = awkRedirAppend
		var err error
		redirTarget, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	} else if p.cur().kind == awkTokPipe {
		p.advance()
		redirOp = awkRedirPipe
		var err error
		redirTarget, err = p.parseExpr()
		if err != nil {
			return nil, err
		}
	}

	return &awkPrintStmt{
		isPrintf:    isPrintf,
		args:        args,
		redirOp:     redirOp,
		redirTarget: redirTarget,
	}, nil
}

func (p *awkParser) isPrintEndOrRedir() bool {
	switch p.cur().kind {
	case awkTokNewline, awkTokSemi, awkTokRBrace, awkTokRParen, awkTokEOF, awkTokElse, awkTokGT, awkTokAppend, awkTokPipe:
		return true
	default:
		return false
	}
}

func (p *awkParser) parseExprList() ([]awkExpr, error) {
	var list []awkExpr
	for {
		p.skipNewlines()
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if p.cur().kind != awkTokComma {
			break
		}
		p.advance()
	}
	return list, nil
}

func (p *awkParser) canStartExpr() bool {
	switch p.cur().kind {
	case awkTokIdent, awkTokNumber, awkTokString, awkTokRegex,
		awkTokDollar, awkTokLParen, awkTokPlus, awkTokMinus, awkTokBang,
		awkTokInc, awkTokDec, awkTokGetline:
		return true
	default:
		return false
	}
}

// canConcatAfter reports whether cur() starts a concatenated term right after
// an additive expression (note '+' and '-' are binary additive operators here,
// not concatenation).
func (p *awkParser) canConcatAfter() bool {
	switch p.cur().kind {
	case awkTokIdent, awkTokNumber, awkTokString, awkTokRegex,
		awkTokDollar, awkTokLParen, awkTokBang, awkTokInc, awkTokDec, awkTokGetline:
		return true
	default:
		return false
	}
}

func isAssignTok(k awkTokKind) bool {
	switch k {
	case awkTokAssign, awkTokAddAssign, awkTokSubAssign, awkTokMulAssign, awkTokDivAssign, awkTokModAssign, awkTokPowAssign:
		return true
	default:
		return false
	}
}

func isAwkLValue(e awkExpr) bool {
	switch e.(type) {
	case *awkIdentExpr, *awkFieldExpr, *awkIndexExpr:
		return true
	default:
		return false
	}
}

// ---------- Expression Precedence Hierarchy ----------

func (p *awkParser) parseExpr() (awkExpr, error) {
	return p.parseAssignExpr()
}

func (p *awkParser) parseAssignExpr() (awkExpr, error) {
	lhs, err := p.parseTernaryExpr()
	if err != nil {
		return nil, err
	}
	if isAssignTok(p.cur().kind) {
		opTok := p.advance()
		if !isAwkLValue(lhs) {
			return nil, fmt.Errorf("line %d: invalid lvalue in assignment", opTok.line)
		}
		rhs, err := p.parseAssignExpr()
		if err != nil {
			return nil, err
		}
		return &awkAssignExpr{op: opTok.kind, target: lhs, val: rhs}, nil
	}
	return lhs, nil
}

func (p *awkParser) parseTernaryExpr() (awkExpr, error) {
	cond, err := p.parseOrExpr()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == awkTokQuestion {
		p.advance()
		p.skipNewlines()
		thenExpr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(awkTokColon, "':'"); err != nil {
			return nil, err
		}
		p.skipNewlines()
		elseExpr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		return &awkTernaryExpr{cond: cond, thenExpr: thenExpr, elseExpr: elseExpr}, nil
	}
	return cond, nil
}

func (p *awkParser) parseOrExpr() (awkExpr, error) {
	left, err := p.parseAndExpr()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == awkTokOr {
		op := p.advance().kind
		p.skipNewlines()
		right, err := p.parseAndExpr()
		if err != nil {
			return nil, err
		}
		if isAssignTok(p.cur().kind) && isAwkLValue(right) {
			opTok := p.advance()
			rhs, err := p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
			right = &awkAssignExpr{op: opTok.kind, target: right, val: rhs}
		}
		left = &awkBinaryExpr{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *awkParser) parseAndExpr() (awkExpr, error) {
	left, err := p.parseInExpr()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == awkTokAnd {
		op := p.advance().kind
		p.skipNewlines()
		right, err := p.parseInExpr()
		if err != nil {
			return nil, err
		}
		if isAssignTok(p.cur().kind) && isAwkLValue(right) {
			opTok := p.advance()
			rhs, err := p.parseAssignExpr()
			if err != nil {
				return nil, err
			}
			right = &awkAssignExpr{op: opTok.kind, target: right, val: rhs}
		}
		left = &awkBinaryExpr{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *awkParser) parseInExpr() (awkExpr, error) {
	left, err := p.parseMatchExpr()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == awkTokIn {
		p.advance()
		arrTok, err := p.expect(awkTokIdent, "array name after 'in'")
		if err != nil {
			return nil, err
		}
		left = &awkInExpr{indices: []awkExpr{left}, arrName: arrTok.str}
	}
	return left, nil
}

func (p *awkParser) parseMatchExpr() (awkExpr, error) {
	left, err := p.parseRelExpr()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == awkTokMatch || p.cur().kind == awkTokNotMatch {
		op := p.advance().kind
		right, err := p.parseRelExpr()
		if err != nil {
			return nil, err
		}
		left = &awkBinaryExpr{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *awkParser) parseRelExpr() (awkExpr, error) {
	left, err := p.parsePipeGetlineExpr()
	if err != nil {
		return nil, err
	}
	for {
		k := p.cur().kind
		if k == awkTokGT && p.inPrint && p.paren == 0 {
			// Leave '>' for output redirection in unparenthesized print/printf.
			break
		}
		if k == awkTokLT || k == awkTokLE || k == awkTokGT || k == awkTokGE || k == awkTokEQ || k == awkTokNE {
			op := p.advance().kind
			right, err := p.parsePipeGetlineExpr()
			if err != nil {
				return nil, err
			}
			left = &awkBinaryExpr{op: op, left: left, right: right}
			continue
		}
		break
	}
	return left, nil
}

// parsePipeGetlineExpr parses `concatExpr ('|' 'getline' [lvalue])?`, binding
// tighter than relational comparisons so `while ("cmd" | getline l > 0)` works.
func (p *awkParser) parsePipeGetlineExpr() (awkExpr, error) {
	expr, err := p.parseConcatExpr()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == awkTokPipe && p.peek(1).kind == awkTokGetline {
		p.advance() // '|'
		p.advance() // 'getline'
		var target awkExpr
		switch p.cur().kind {
		case awkTokIdent, awkTokDollar:
			lval, err := p.parseFieldOrPrimary(false)
			if err != nil {
				return nil, err
			}
			if !isAwkLValue(lval) {
				return nil, fmt.Errorf("line %d: invalid target for getline", p.cur().line)
			}
			target = lval
		}
		return &awkGetlineExpr{pipeCmd: expr, target: target}, nil
	}
	return expr, nil
}

func (p *awkParser) parseConcatExpr() (awkExpr, error) {
	first, err := p.parseAddExpr()
	if err != nil {
		return nil, err
	}
	if !p.canConcatAfter() {
		return first, nil
	}
	parts := []awkExpr{first}
	for p.canConcatAfter() {
		next, err := p.parseAddExpr()
		if err != nil {
			return nil, err
		}
		parts = append(parts, next)
	}
	return &awkConcatExpr{parts: parts}, nil
}

func (p *awkParser) parseAddExpr() (awkExpr, error) {
	left, err := p.parseMulExpr()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == awkTokPlus || p.cur().kind == awkTokMinus {
		op := p.advance().kind
		right, err := p.parseMulExpr()
		if err != nil {
			return nil, err
		}
		left = &awkBinaryExpr{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *awkParser) parseMulExpr() (awkExpr, error) {
	left, err := p.parseUnaryExpr()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == awkTokStar || p.cur().kind == awkTokSlash || p.cur().kind == awkTokPercent {
		op := p.advance().kind
		right, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		left = &awkBinaryExpr{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *awkParser) parseUnaryExpr() (awkExpr, error) {
	switch p.cur().kind {
	case awkTokPlus, awkTokMinus, awkTokBang:
		op := p.advance().kind
		sub, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		return &awkUnaryExpr{op: op, expr: sub}, nil
	case awkTokGetline:
		return p.parseGetlineExpr()
	default:
		return p.parsePowExpr()
	}
}

func (p *awkParser) parseGetlineExpr() (awkExpr, error) {
	p.advance() // 'getline'
	var target awkExpr
	// Optional lvalue target: ident, ident[...], or $field
	switch p.cur().kind {
	case awkTokIdent, awkTokDollar:
		lval, err := p.parseFieldOrPrimary(false)
		if err != nil {
			return nil, err
		}
		if !isAwkLValue(lval) {
			return nil, fmt.Errorf("line %d: invalid target for getline", p.cur().line)
		}
		target = lval
	}
	var file awkExpr
	if p.cur().kind == awkTokLT {
		p.advance()
		var err error
		file, err = p.parseConcatExpr()
		if err != nil {
			return nil, err
		}
	}
	return &awkGetlineExpr{target: target, file: file}, nil
}

func (p *awkParser) parsePowExpr() (awkExpr, error) {
	base, err := p.parseIncDec()
	if err != nil {
		return nil, err
	}
	if p.cur().kind == awkTokCaret {
		op := p.advance().kind
		exp, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		return &awkBinaryExpr{op: op, left: base, right: exp}, nil
	}
	return base, nil
}

func (p *awkParser) parseIncDec() (awkExpr, error) {
	if p.cur().kind == awkTokInc || p.cur().kind == awkTokDec {
		opTok := p.advance()
		target, err := p.parseFieldOrPrimary(false)
		if err != nil {
			return nil, err
		}
		if !isAwkLValue(target) {
			return nil, fmt.Errorf("line %d: invalid operand for %s", opTok.line, opTok.str)
		}
		return &awkIncDecExpr{op: opTok.kind, pre: true, target: target}, nil
	}

	expr, err := p.parseFieldOrPrimary(true)
	if err != nil {
		return nil, err
	}
	if p.cur().kind == awkTokInc || p.cur().kind == awkTokDec {
		opTok := p.advance()
		if !isAwkLValue(expr) {
			return nil, fmt.Errorf("line %d: invalid operand for %s", opTok.line, opTok.str)
		}
		return &awkIncDecExpr{op: opTok.kind, pre: false, target: expr}, nil
	}
	return expr, nil
}

// parseFieldOrPrimary parses `$target` or a primary expression.
func (p *awkParser) parseFieldOrPrimary(allowCall bool) (awkExpr, error) {
	if p.cur().kind == awkTokDollar {
		p.advance()
		idx, err := p.parseFieldIndex()
		if err != nil {
			return nil, err
		}
		constIdx := -1
		if ne, ok := idx.(*awkNumExpr); ok && ne.val >= 0 && ne.val <= 1048576 && math.Trunc(ne.val) == ne.val {
			constIdx = int(ne.val)
		}
		return &awkFieldExpr{index: idx, constIdx: constIdx}, nil
	}
	return p.parsePrimary(allowCall)
}

// parseFieldIndex parses the operand of `$`, where `$++i` is `$(++i)` while
// `$i++` leaves postfix `++` for the outer `$i` lvalue (`($i)++`).
func (p *awkParser) parseFieldIndex() (awkExpr, error) {
	switch p.cur().kind {
	case awkTokInc, awkTokDec:
		opTok := p.advance()
		target, err := p.parseFieldOrPrimary(false)
		if err != nil {
			return nil, err
		}
		if !isAwkLValue(target) {
			return nil, fmt.Errorf("line %d: invalid operand for %s", opTok.line, opTok.str)
		}
		return &awkIncDecExpr{op: opTok.kind, pre: true, target: target}, nil
	case awkTokPlus, awkTokMinus, awkTokBang:
		op := p.advance().kind
		sub, err := p.parseFieldIndex()
		if err != nil {
			return nil, err
		}
		return &awkUnaryExpr{op: op, expr: sub}, nil
	case awkTokDollar:
		return p.parseFieldOrPrimary(true)
	default:
		return p.parsePrimary(true)
	}
}

func (p *awkParser) parsePrimary(allowCall bool) (awkExpr, error) {
	tok := p.cur()
	switch tok.kind {
	case awkTokNumber:
		p.advance()
		return &awkNumExpr{val: tok.num}, nil

	case awkTokString:
		p.advance()
		return &awkStrExpr{val: tok.str}, nil

	case awkTokRegex:
		p.advance()
		re, err := compileAwkRegex(tok.str)
		if err != nil {
			return nil, fmt.Errorf("line %d: invalid regex /%s/: %v", tok.line, tok.str, err)
		}
		return &awkRegexExpr{pat: tok.str, re: re}, nil

	case awkTokLParen:
		p.advance() // '('
		p.paren++
		first, err := p.parseExpr()
		if err != nil {
			p.paren--
			return nil, err
		}
		// Check for multi-dimensional `(i, j, ...) in arr`
		if p.cur().kind == awkTokComma {
			indices := []awkExpr{first}
			for p.cur().kind == awkTokComma {
				p.advance()
				p.skipNewlines()
				next, err := p.parseExpr()
				if err != nil {
					p.paren--
					return nil, err
				}
				indices = append(indices, next)
			}
			p.paren--
			p.skipNewlines()
			if _, err := p.expect(awkTokRParen, "')'"); err != nil {
				return nil, err
			}
			if _, err := p.expect(awkTokIn, "'in' after parenthesized subscript list"); err != nil {
				return nil, err
			}
			arrTok, err := p.expect(awkTokIdent, "array name after 'in'")
			if err != nil {
				return nil, err
			}
			return &awkInExpr{indices: indices, arrName: arrTok.str}, nil
		}
		p.paren--
		p.skipNewlines()
		if _, err := p.expect(awkTokRParen, "')'"); err != nil {
			return nil, err
		}
		return first, nil

	case awkTokIdent:
		p.advance()
		fnID := resolveAwkBuiltinFunc(tok.str)
		isUserFn := p.userFuncs[tok.str]

		// Function call:
		// - Built-in or declared user function followed by '(' (even with space)
		// - Any identifier immediately followed by '(' with NO space (`foo(x)`)
		if allowCall && p.cur().kind == awkTokLParen && (fnID != awkFnNone || isUserFn || !p.cur().hadSpace) {
			p.advance() // '('
			p.paren++
			var args []awkExpr
			p.skipNewlines()
			if p.cur().kind != awkTokRParen {
				var err error
				args, err = p.parseExprList()
				if err != nil {
					p.paren--
					return nil, err
				}
			}
			p.paren--
			p.skipNewlines()
			if _, err := p.expect(awkTokRParen, "')'"); err != nil {
				return nil, err
			}
			return &awkCallExpr{name: tok.str, builtinID: fnID, args: args}, nil
		}

		// Bare `length` without parentheses means `length($0)` in POSIX AWK!
		if allowCall && fnID == awkFnLength && p.cur().kind != awkTokLBracket && !isAssignTok(p.cur().kind) {
			return &awkCallExpr{name: "length", builtinID: awkFnLength}, nil
		}

		// Array subscript `arr[k1, k2, ...]`
		if p.cur().kind == awkTokLBracket {
			p.advance() // '['
			p.paren++
			indices, err := p.parseExprList()
			p.paren--
			if err != nil {
				return nil, err
			}
			p.skipNewlines()
			if _, err := p.expect(awkTokRBracket, "']'"); err != nil {
				return nil, err
			}
			return &awkIndexExpr{arrName: tok.str, indices: indices}, nil
		}

		return &awkIdentExpr{
			name:       tok.str,
			builtinVar: resolveAwkBuiltinVar(tok.str),
		}, nil

	default:
		return nil, fmt.Errorf("line %d: unexpected %s in expression", tok.line, p.tokDesc(tok))
	}
}
