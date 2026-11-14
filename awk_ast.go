package main

import "regexp"

// awkBuiltinVarID identifies special AWK built-in scalar variables resolved at
// parse time so hot loops avoid map lookups and string comparisons.
type awkBuiltinVarID uint8

const (
	awkVarNone awkBuiltinVarID = iota
	awkVarFS
	awkVarRS
	awkVarOFS
	awkVarORS
	awkVarSUBSEP
	awkVarOFMT
	awkVarCONVFMT
	awkVarNR
	awkVarFNR
	awkVarNF
	awkVarFILENAME
	awkVarRSTART
	awkVarRLENGTH
	awkVarARGC
	awkVarRT
)

func resolveAwkBuiltinVar(name string) awkBuiltinVarID {
	switch name {
	case "FS":
		return awkVarFS
	case "RS":
		return awkVarRS
	case "OFS":
		return awkVarOFS
	case "ORS":
		return awkVarORS
	case "SUBSEP":
		return awkVarSUBSEP
	case "OFMT":
		return awkVarOFMT
	case "CONVFMT":
		return awkVarCONVFMT
	case "NR":
		return awkVarNR
	case "FNR":
		return awkVarFNR
	case "NF":
		return awkVarNF
	case "FILENAME":
		return awkVarFILENAME
	case "RSTART":
		return awkVarRSTART
	case "RLENGTH":
		return awkVarRLENGTH
	case "ARGC":
		return awkVarARGC
	case "RT":
		return awkVarRT
	default:
		return awkVarNone
	}
}

// awkBuiltinFuncID identifies built-in functions resolved at parse time.
type awkBuiltinFuncID uint8

const (
	awkFnNone awkBuiltinFuncID = iota
	awkFnLength
	awkFnSubstr
	awkFnIndex
	awkFnSplit
	awkFnSub
	awkFnGsub
	awkFnGensub
	awkFnMatch
	awkFnTolower
	awkFnToupper
	awkFnSprintf
	awkFnInt
	awkFnSqrt
	awkFnSin
	awkFnCos
	awkFnAtan2
	awkFnExp
	awkFnLog
	awkFnRand
	awkFnSrand
	awkFnClose
	awkFnFflush
	awkFnSystime
	awkFnStrftime
)

func resolveAwkBuiltinFunc(name string) awkBuiltinFuncID {
	switch name {
	case "length":
		return awkFnLength
	case "substr":
		return awkFnSubstr
	case "index":
		return awkFnIndex
	case "split":
		return awkFnSplit
	case "sub":
		return awkFnSub
	case "gsub":
		return awkFnGsub
	case "gensub":
		return awkFnGensub
	case "match":
		return awkFnMatch
	case "tolower":
		return awkFnTolower
	case "toupper":
		return awkFnToupper
	case "sprintf":
		return awkFnSprintf
	case "int":
		return awkFnInt
	case "sqrt":
		return awkFnSqrt
	case "sin":
		return awkFnSin
	case "cos":
		return awkFnCos
	case "atan2":
		return awkFnAtan2
	case "exp":
		return awkFnExp
	case "log":
		return awkFnLog
	case "rand":
		return awkFnRand
	case "srand":
		return awkFnSrand
	case "close":
		return awkFnClose
	case "fflush":
		return awkFnFflush
	case "systime":
		return awkFnSystime
	case "strftime":
		return awkFnStrftime
	default:
		return awkFnNone
	}
}

// awkRedirOp describes output redirection for print / printf.
type awkRedirOp uint8

const (
	awkRedirNone awkRedirOp = iota
	awkRedirTrunc
	awkRedirAppend
)

// awkProgram is a parsed and pre-compiled AWK program.
type awkProgram struct {
	begins []*awkBlockStmt
	rules  []*awkRule
	ends   []*awkBlockStmt
	funcs  map[string]*awkFuncDef
}

type awkRule struct {
	pat1   awkExpr       // nil means match all records
	pat2   awkExpr       // non-nil for range pattern: pat1, pat2
	action *awkBlockStmt // nil means default action: { print $0 }
}

type awkFuncDef struct {
	name   string
	params []string
	body   *awkBlockStmt
}

// awkStmt is the interface implemented by all AWK statement AST nodes.
type awkStmt interface {
	awkStmtNode()
}

type awkBlockStmt struct {
	stmts []awkStmt
}

type awkExprStmt struct {
	expr awkExpr
}

type awkIfStmt struct {
	cond     awkExpr
	thenStmt awkStmt
	elseStmt awkStmt
}

type awkWhileStmt struct {
	cond awkExpr
	body awkStmt
}

type awkDoWhileStmt struct {
	body awkStmt
	cond awkExpr
}

type awkForStmt struct {
	init awkStmt
	cond awkExpr
	post awkStmt
	body awkStmt
}

type awkForInStmt struct {
	varName string
	arrName string
	body    awkStmt
}

type awkBreakStmt struct{}
type awkContinueStmt struct{}
type awkNextStmt struct{}
type awkNextfileStmt struct{}

type awkExitStmt struct {
	code awkExpr
}

type awkReturnStmt struct {
	val awkExpr
}

type awkDeleteStmt struct {
	arrName string
	indices []awkExpr // nil for whole-array `delete arr`
}

type awkPrintStmt struct {
	isPrintf    bool
	args        []awkExpr
	redirOp     awkRedirOp
	redirTarget awkExpr
}

func (*awkBlockStmt) awkStmtNode()    {}
func (*awkExprStmt) awkStmtNode()     {}
func (*awkIfStmt) awkStmtNode()       {}
func (*awkWhileStmt) awkStmtNode()    {}
func (*awkDoWhileStmt) awkStmtNode()  {}
func (*awkForStmt) awkStmtNode()      {}
func (*awkForInStmt) awkStmtNode()    {}
func (*awkBreakStmt) awkStmtNode()    {}
func (*awkContinueStmt) awkStmtNode() {}
func (*awkNextStmt) awkStmtNode()     {}
func (*awkNextfileStmt) awkStmtNode() {}
func (*awkExitStmt) awkStmtNode()     {}
func (*awkReturnStmt) awkStmtNode()   {}
func (*awkDeleteStmt) awkStmtNode()   {}
func (*awkPrintStmt) awkStmtNode()    {}

// awkExpr is the interface implemented by all AWK expression AST nodes.
type awkExpr interface {
	awkExprNode()
}

type awkNumExpr struct {
	val float64
}

type awkStrExpr struct {
	val string
}

type awkRegexExpr struct {
	pat string
	re  *regexp.Regexp
}

type awkIdentExpr struct {
	name       string
	builtinVar awkBuiltinVarID
}

type awkFieldExpr struct {
	index    awkExpr
	constIdx int // >= 0 when index is a small non-negative integer constant, else -1
}

type awkIndexExpr struct {
	arrName string
	indices []awkExpr
}

type awkUnaryExpr struct {
	op   awkTokKind
	expr awkExpr
}

type awkBinaryExpr struct {
	op          awkTokKind
	left, right awkExpr
}

type awkConcatExpr struct {
	parts []awkExpr
}

type awkTernaryExpr struct {
	cond, thenExpr, elseExpr awkExpr
}

type awkAssignExpr struct {
	op     awkTokKind
	target awkExpr
	val    awkExpr
}

type awkIncDecExpr struct {
	op     awkTokKind
	pre    bool
	target awkExpr
}

type awkInExpr struct {
	indices []awkExpr
	arrName string
}

type awkCallExpr struct {
	name      string
	builtinID awkBuiltinFuncID
	args      []awkExpr
}

type awkGetlineExpr struct {
	target awkExpr // nil means $0
	file   awkExpr // nil means current input stream
}

func (*awkNumExpr) awkExprNode()     {}
func (*awkStrExpr) awkExprNode()     {}
func (*awkRegexExpr) awkExprNode()   {}
func (*awkIdentExpr) awkExprNode()   {}
func (*awkFieldExpr) awkExprNode()   {}
func (*awkIndexExpr) awkExprNode()   {}
func (*awkUnaryExpr) awkExprNode()   {}
func (*awkBinaryExpr) awkExprNode()  {}
func (*awkConcatExpr) awkExprNode()  {}
func (*awkTernaryExpr) awkExprNode() {}
func (*awkAssignExpr) awkExprNode()  {}
func (*awkIncDecExpr) awkExprNode()  {}
func (*awkInExpr) awkExprNode()      {}
func (*awkCallExpr) awkExprNode()    {}
func (*awkGetlineExpr) awkExprNode() {}
