package unish

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type jqTokKind uint8

const (
	jqTokEOF jqTokKind = iota
	jqTokIdent
	jqTokVar         // $foo
	jqTokFormat      // @csv, @base64, ...
	jqTokNum         // 123, 3.14
	jqTokStr         // plain "hello"
	jqTokInterpStart // "prefix \(
	jqTokInterpMid   // ) middle \(
	jqTokInterpEnd   // ) suffix"

	// Punctuation & Operators
	jqTokDot       // .
	jqTokDotDot    // ..
	jqTokPipe      // |
	jqTokComma     // ,
	jqTokSemi      // ;
	jqTokColon     // :
	jqTokQMark     // ?
	jqTokAltDestr  // ?//
	jqTokLParen    // (
	jqTokRParen    // )
	jqTokLBrack    // [
	jqTokRBrack    // ]
	jqTokLBrace    // {
	jqTokRBrace    // }
	jqTokPlus      // +
	jqTokMinus     // -
	jqTokStar      // *
	jqTokSlash     // /
	jqTokPercent   // %
	jqTokEq        // ==
	jqTokNeq       // !=
	jqTokLt        // <
	jqTokLe        // <=
	jqTokGt        // >
	jqTokGe        // >=
	jqTokAlt       // //
	jqTokAssign    // =
	jqTokUpdate    // |=
	jqTokPlusEq    // +=
	jqTokMinusEq   // -=
	jqTokStarEq    // *=
	jqTokSlashEq   // /=
	jqTokPercentEq // %=
	jqTokAltEq     // //=

	// Keywords
	jqTokDef
	jqTokAs
	jqTokIf
	jqTokThen
	jqTokElif
	jqTokElse
	jqTokEnd
	jqTokAnd
	jqTokOr
	jqTokNot
	jqTokTry
	jqTokCatch
	jqTokReduce
	jqTokForeach
	jqTokLabel
	jqTokBreak
	jqTokTrue
	jqTokFalse
	jqTokNull
)

type jqToken struct {
	kind jqTokKind
	str  string
	num  jqNum
	pos  int
	big  bool // integer literal beyond int64: keep verbatim text
	noWS bool // true when token immediately follows previous token with zero whitespace
}

func (t jqToken) keywordOrIdent() (string, bool) {
	switch t.kind {
	case jqTokIdent, jqTokDef, jqTokAs, jqTokIf, jqTokThen, jqTokElif, jqTokElse,
		jqTokEnd, jqTokAnd, jqTokOr, jqTokNot, jqTokTry, jqTokCatch,
		jqTokReduce, jqTokForeach, jqTokLabel, jqTokBreak,
		jqTokTrue, jqTokFalse, jqTokNull:
		return t.str, true
	}
	return "", false
}

type jqInterpFrame struct {
	parenDepth int
}

type jqLexer struct {
	src    string
	pos    int
	frames []jqInterpFrame
}

func jqLexAll(src string) ([]jqToken, error) {
	l := &jqLexer{src: src}
	var toks []jqToken
	for {
		tok, err := l.nextToken()
		if err != nil {
			return nil, err
		}
		toks = append(toks, tok)
		if tok.kind == jqTokEOF {
			break
		}
	}
	return toks, nil
}

func isJqIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func isJqIdentCont(c byte) bool {
	return isJqIdentStart(c) || (c >= '0' && c <= '9')
}

func (l *jqLexer) skipWSAndComments() bool {
	skipped := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			skipped = true
			l.pos++
			continue
		}
		if c == '#' {
			// In jq, comments extend to '\n' (with \\\n continuation in jq 1.6+, keep simple to '\n').
			skipped = true
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		break
	}
	return skipped
}

func (l *jqLexer) nextToken() (jqToken, error) {
	hadWS := l.skipWSAndComments()
	noWS := !hadWS && l.pos > 0
	if l.pos >= len(l.src) {
		if len(l.frames) > 0 {
			return jqToken{}, fmt.Errorf("jq: compile error: unfinished string interpolation at end of filter")
		}
		return jqToken{kind: jqTokEOF, pos: l.pos}, nil
	}

	start := l.pos
	c := l.src[l.pos]

	// Closing ')' of a string interpolation `\(...)`!
	if c == ')' && len(l.frames) > 0 && l.frames[len(l.frames)-1].parenDepth == 0 {
		l.pos++ // consume ')'
		return l.scanStringSegment(false, start)
	}

	switch c {
	case '(':
		l.pos++
		if len(l.frames) > 0 {
			l.frames[len(l.frames)-1].parenDepth++
		}
		return jqToken{kind: jqTokLParen, str: "(", pos: start, noWS: noWS}, nil
	case ')':
		l.pos++
		if len(l.frames) > 0 {
			l.frames[len(l.frames)-1].parenDepth--
		}
		return jqToken{kind: jqTokRParen, str: ")", pos: start, noWS: noWS}, nil
	case '[':
		l.pos++
		return jqToken{kind: jqTokLBrack, str: "[", pos: start, noWS: noWS}, nil
	case ']':
		l.pos++
		return jqToken{kind: jqTokRBrack, str: "]", pos: start, noWS: noWS}, nil
	case '{':
		l.pos++
		return jqToken{kind: jqTokLBrace, str: "{", pos: start, noWS: noWS}, nil
	case '}':
		l.pos++
		return jqToken{kind: jqTokRBrace, str: "}", pos: start, noWS: noWS}, nil
	case ',':
		l.pos++
		return jqToken{kind: jqTokComma, str: ",", pos: start, noWS: noWS}, nil
	case ';':
		l.pos++
		return jqToken{kind: jqTokSemi, str: ";", pos: start, noWS: noWS}, nil
	case ':':
		l.pos++
		return jqToken{kind: jqTokColon, str: ":", pos: start, noWS: noWS}, nil
	case '?':
		if l.pos+2 < len(l.src) && l.src[l.pos+1] == '/' && l.src[l.pos+2] == '/' {
			l.pos += 3
			return jqToken{kind: jqTokAltDestr, str: "?//", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokQMark, str: "?", pos: start, noWS: noWS}, nil
	case '|':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokUpdate, str: "|=", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokPipe, str: "|", pos: start, noWS: noWS}, nil
	case '+':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokPlusEq, str: "+=", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokPlus, str: "+", pos: start, noWS: noWS}, nil
	case '-':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokMinusEq, str: "-=", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokMinus, str: "-", pos: start, noWS: noWS}, nil
	case '*':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokStarEq, str: "*=", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokStar, str: "*", pos: start, noWS: noWS}, nil
	case '%':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokPercentEq, str: "%=", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokPercent, str: "%", pos: start, noWS: noWS}, nil
	case '/':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '/' {
			if l.pos+2 < len(l.src) && l.src[l.pos+2] == '=' {
				l.pos += 3
				return jqToken{kind: jqTokAltEq, str: "//=", pos: start, noWS: noWS}, nil
			}
			l.pos += 2
			return jqToken{kind: jqTokAlt, str: "//", pos: start, noWS: noWS}, nil
		}
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokSlashEq, str: "/=", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokSlash, str: "/", pos: start, noWS: noWS}, nil
	case '=':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokEq, str: "==", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokAssign, str: "=", pos: start, noWS: noWS}, nil
	case '!':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokNeq, str: "!=", pos: start, noWS: noWS}, nil
		}
		return jqToken{}, fmt.Errorf("jq: compile error: unexpected '!' at position %d", start)
	case '<':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokLe, str: "<=", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokLt, str: "<", pos: start, noWS: noWS}, nil
	case '>':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
			l.pos += 2
			return jqToken{kind: jqTokGe, str: ">=", pos: start, noWS: noWS}, nil
		}
		l.pos++
		return jqToken{kind: jqTokGt, str: ">", pos: start, noWS: noWS}, nil
	case '.':
		if l.pos+1 < len(l.src) && l.src[l.pos+1] == '.' {
			l.pos += 2
			return jqToken{kind: jqTokDotDot, str: "..", pos: start, noWS: noWS}, nil
		}
		if l.pos+1 < len(l.src) && l.src[l.pos+1] >= '0' && l.src[l.pos+1] <= '9' {
			return l.scanNumber(noWS)
		}
		l.pos++
		return jqToken{kind: jqTokDot, str: ".", pos: start, noWS: noWS}, nil
	case '"':
		l.pos++ // consume opening '"'
		tok, err := l.scanStringSegment(true, start)
		tok.noWS = noWS
		return tok, err
	case '$':
		l.pos++
		vStart := l.pos
		for l.pos < len(l.src) && isJqIdentCont(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == vStart {
			return jqToken{}, fmt.Errorf("jq: compile error: expected variable name after '$' at position %d", start)
		}
		return jqToken{kind: jqTokVar, str: l.src[vStart:l.pos], pos: start, noWS: noWS}, nil
	case '@':
		l.pos++
		fStart := l.pos
		for l.pos < len(l.src) && isJqIdentCont(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == fStart {
			return jqToken{}, fmt.Errorf("jq: compile error: expected format name after '@' at position %d", start)
		}
		return jqToken{kind: jqTokFormat, str: "@" + l.src[fStart:l.pos], pos: start, noWS: noWS}, nil
	}

	if c >= '0' && c <= '9' {
		return l.scanNumber(noWS)
	}
	if isJqIdentStart(c) {
		for l.pos < len(l.src) && isJqIdentCont(l.src[l.pos]) {
			l.pos++
		}
		// Optional module namespace `mod::ident` (rare, keep simple unless ::)
		word := l.src[start:l.pos]
		kind := jqKeywordKind(word)
		return jqToken{kind: kind, str: word, pos: start, noWS: noWS}, nil
	}

	return jqToken{}, fmt.Errorf("jq: compile error: unexpected character %q at position %d", c, start)
}

func jqKeywordKind(w string) jqTokKind {
	switch w {
	case "def":
		return jqTokDef
	case "as":
		return jqTokAs
	case "if":
		return jqTokIf
	case "then":
		return jqTokThen
	case "elif":
		return jqTokElif
	case "else":
		return jqTokElse
	case "end":
		return jqTokEnd
	case "and":
		return jqTokAnd
	case "or":
		return jqTokOr
	case "not":
		return jqTokNot
	case "try":
		return jqTokTry
	case "catch":
		return jqTokCatch
	case "reduce":
		return jqTokReduce
	case "foreach":
		return jqTokForeach
	case "label":
		return jqTokLabel
	case "break":
		return jqTokBreak
	case "true":
		return jqTokTrue
	case "false":
		return jqTokFalse
	case "null":
		return jqTokNull
	default:
		return jqTokIdent
	}
}

func (l *jqLexer) scanNumber(noWS bool) (jqToken, error) {
	start := l.pos
	isFloat := false
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		isFloat = true
		l.pos++
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
	} else {
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
		if l.pos < len(l.src) && l.src[l.pos] == '.' {
			// Check it's not `..`!
			if l.pos+1 >= len(l.src) || (l.src[l.pos+1] != '.' && !isJqIdentStart(l.src[l.pos+1])) {
				isFloat = true
				l.pos++
				for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
					l.pos++
				}
			}
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		isFloat = true
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		expStart := l.pos
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
		if l.pos == expStart {
			return jqToken{}, fmt.Errorf("jq: compile error: invalid numeric exponent at position %d", start)
		}
	}
	raw := l.src[start:l.pos]
	if !isFloat {
		if iv, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return jqToken{
				kind: jqTokNum,
				str:  raw,
				num:  jqNum{ii: true, i: iv, f: float64(iv)},
				pos:  start,
				noWS: noWS,
			}, nil
		}
	}
	fv, err := strconv.ParseFloat(raw, 64)
	if err != nil && !strings.Contains(err.Error(), "value out of range") {
		return jqToken{}, fmt.Errorf("jq: compile error: invalid number %q at position %d", raw, start)
	}
	var num jqNum
	num = jqNum{f: fv}
	return jqToken{
		kind: jqTokNum,
		str:  raw,
		num:  num,
		pos:  start,
		noWS: noWS,
		big:  !isFloat,
	}, nil
}

func (l *jqLexer) scanStringSegment(isOpening bool, startPos int) (jqToken, error) {
	var sb strings.Builder
	for {
		if l.pos >= len(l.src) {
			return jqToken{}, fmt.Errorf("jq: compile error: unterminated string literal at position %d", startPos)
		}
		c := l.src[l.pos]
		if c == '"' {
			l.pos++
			if isOpening {
				return jqToken{kind: jqTokStr, str: sb.String(), pos: startPos}, nil
			}
			// Closing an interpolated string
			l.frames = l.frames[:len(l.frames)-1]
			return jqToken{kind: jqTokInterpEnd, str: sb.String(), pos: startPos}, nil
		}
		if c != '\\' {
			r, w := utf8.DecodeRuneInString(l.src[l.pos:])
			sb.WriteRune(r)
			l.pos += w
			continue
		}
		l.pos++ // consume '\'
		if l.pos >= len(l.src) {
			return jqToken{}, fmt.Errorf("jq: compile error: unterminated escape in string at position %d", startPos)
		}
		esc := l.src[l.pos]
		l.pos++
		switch esc {
		case '(':
			// String interpolation `\(expr)`!
			if isOpening {
				l.frames = append(l.frames, jqInterpFrame{parenDepth: 0})
				return jqToken{kind: jqTokInterpStart, str: sb.String(), pos: startPos}, nil
			}
			// Middle interpolation segment `) ... \(`
			l.frames[len(l.frames)-1].parenDepth = 0
			return jqToken{kind: jqTokInterpMid, str: sb.String(), pos: startPos}, nil
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
			if l.pos+4 > len(l.src) {
				return jqToken{}, fmt.Errorf("jq: compile error: unfinished \\uXXXX escape in string")
			}
			var h1 [4]byte
			copy(h1[:], l.src[l.pos:l.pos+4])
			r1, ok := parseHex4(h1)
			if !ok {
				return jqToken{}, fmt.Errorf("jq: compile error: invalid \\uXXXX escape in string")
			}
			l.pos += 4
			if isUTF16Surrogate(r1) && l.pos+6 <= len(l.src) && l.src[l.pos] == '\\' && l.src[l.pos+1] == 'u' {
				var h2 [4]byte
				copy(h2[:], l.src[l.pos+2:l.pos+6])
				if r2, ok2 := parseHex4(h2); ok2 && r2 >= 0xDC00 && r2 <= 0xDFFF {
					l.pos += 6
					sb.WriteRune(decodeUTF16Surrogate(r1, r2))
					continue
				}
			}
			sb.WriteRune(r1)
		default:
			return jqToken{}, fmt.Errorf("jq: compile error: invalid escape \\%c in string", esc)
		}
	}
}
