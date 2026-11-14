package unish

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type awkTokKind uint8

const (
	awkTokEOF awkTokKind = iota
	awkTokNewline
	awkTokSemi // ;

	// Grouping & punctuation
	awkTokLParen   // (
	awkTokRParen   // )
	awkTokLBrace   // {
	awkTokRBrace   // }
	awkTokLBracket // [
	awkTokRBracket // ]
	awkTokComma    // ,
	awkTokQuestion // ?
	awkTokColon    // :
	awkTokDollar   // $

	// Arithmetic & assignment operators
	awkTokAssign    // =
	awkTokAddAssign // +=
	awkTokSubAssign // -=
	awkTokMulAssign // *=
	awkTokDivAssign // /=
	awkTokModAssign // %=
	awkTokPowAssign // ^= or **=

	awkTokPlus    // +
	awkTokMinus   // -
	awkTokStar    // *
	awkTokSlash   // /
	awkTokPercent // %
	awkTokCaret   // ^ or **
	awkTokInc     // ++
	awkTokDec     // --

	// Logical, comparison & regex match operators
	awkTokOr       // ||
	awkTokAnd      // &&
	awkTokBang     // !
	awkTokMatch    // ~
	awkTokNotMatch // !~
	awkTokLT       // <
	awkTokLE       // <=
	awkTokGT       // >
	awkTokGE       // >=
	awkTokAppend   // >>
	awkTokEQ       // ==
	awkTokNE       // !=
	awkTokPipe     // |

	// Literals & identifiers
	awkTokIdent
	awkTokNumber
	awkTokString
	awkTokRegex

	// Keywords
	awkTokBEGIN
	awkTokEND
	awkTokIf
	awkTokElse
	awkTokWhile
	awkTokDo
	awkTokFor
	awkTokIn
	awkTokBreak
	awkTokContinue
	awkTokNext
	awkTokNextfile
	awkTokExit
	awkTokReturn
	awkTokDelete
	awkTokFunction
	awkTokPrint
	awkTokPrintf
	awkTokGetline
)

var awkKeywords = map[string]awkTokKind{
	"BEGIN":    awkTokBEGIN,
	"END":      awkTokEND,
	"if":       awkTokIf,
	"else":     awkTokElse,
	"while":    awkTokWhile,
	"do":       awkTokDo,
	"for":      awkTokFor,
	"in":       awkTokIn,
	"break":    awkTokBreak,
	"continue": awkTokContinue,
	"next":     awkTokNext,
	"nextfile": awkTokNextfile,
	"exit":     awkTokExit,
	"return":   awkTokReturn,
	"delete":   awkTokDelete,
	"function": awkTokFunction,
	"func":     awkTokFunction,
	"print":    awkTokPrint,
	"printf":   awkTokPrintf,
	"getline":  awkTokGetline,
}

type awkToken struct {
	kind     awkTokKind
	str      string
	num      float64
	line     int
	hadSpace bool // true if preceded by whitespace/comment on the same or continued line
}

type awkLexer struct {
	src           string
	pos           int
	line          int
	canPrecedeDiv bool
	suppressNL    bool
}

func newAwkLexer(src string) *awkLexer {
	return &awkLexer{
		src:        src,
		line:       1,
		suppressNL: true, // ignore leading blank lines
	}
}

func (l *awkLexer) tokenizeAll() ([]awkToken, error) {
	tokens := make([]awkToken, 0, len(l.src)/3+8)
	for {
		tok, err := l.nextToken()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, tok)
		if tok.kind == awkTokEOF {
			return tokens, nil
		}
	}
}

func (l *awkLexer) nextToken() (awkToken, error) {
	hadSpace := false
	for {
		if l.pos >= len(l.src) {
			return awkToken{kind: awkTokEOF, line: l.line, hadSpace: hadSpace}, nil
		}
		ch := l.src[l.pos]
		// Horizontal whitespace and CR
		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\f' || ch == '\v' {
			if ch != '\r' {
				hadSpace = true
			}
			l.pos++
			continue
		}
		// Backslash line continuation
		if ch == '\\' {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\n' {
				l.pos += 2
				l.line++
				hadSpace = true
				continue
			}
			if l.pos+2 < len(l.src) && l.src[l.pos+1] == '\r' && l.src[l.pos+2] == '\n' {
				l.pos += 3
				l.line++
				hadSpace = true
				continue
			}
		}
		// Comments (# to end of line; keep \n for statement termination)
		if ch == '#' {
			hadSpace = true
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		// Newline
		if ch == '\n' {
			tokLine := l.line
			l.pos++
			l.line++
			if l.suppressNL {
				hadSpace = true
				continue
			}
			l.canPrecedeDiv = false
			l.suppressNL = true
			return awkToken{kind: awkTokNewline, line: tokLine, hadSpace: hadSpace}, nil
		}
		break
	}

	tokLine := l.line
	ch := l.src[l.pos]

	// Helper to emit a token and update context flags.
	emit := func(kind awkTokKind, str string, num float64) awkToken {
		switch kind {
		case awkTokIdent, awkTokNumber, awkTokString, awkTokRegex,
			awkTokRParen, awkTokRBracket, awkTokInc, awkTokDec, awkTokGetline:
			l.canPrecedeDiv = true
		default:
			l.canPrecedeDiv = false
		}
		switch kind {
		case awkTokComma, awkTokLBrace, awkTokLParen, awkTokLBracket, awkTokAnd, awkTokOr,
			awkTokDo, awkTokElse, awkTokQuestion, awkTokColon, awkTokSemi:
			l.suppressNL = true
		default:
			l.suppressNL = false
		}
		return awkToken{kind: kind, str: str, num: num, line: tokLine, hadSpace: hadSpace}
	}

	// String literal
	if ch == '"' {
		s, err := l.scanString()
		if err != nil {
			return awkToken{}, err
		}
		return emit(awkTokString, s, 0), nil
	}

	// Slash: disambiguate regex literal vs division (/ or /=)
	if ch == '/' {
		if l.canPrecedeDiv {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '=' {
				l.pos += 2
				return emit(awkTokDivAssign, "/=", 0), nil
			}
			l.pos++
			return emit(awkTokSlash, "/", 0), nil
		}
		pat, err := l.scanRegex()
		if err != nil {
			return awkToken{}, err
		}
		return emit(awkTokRegex, pat, 0), nil
	}

	// Numbers: digits or leading '.' followed by digit
	if (ch >= '0' && ch <= '9') || (ch == '.' && l.pos+1 < len(l.src) && l.src[l.pos+1] >= '0' && l.src[l.pos+1] <= '9') {
		raw, val, err := l.scanNumber()
		if err != nil {
			return awkToken{}, err
		}
		return emit(awkTokNumber, raw, val), nil
	}

	// Identifiers and keywords
	if isAwkIdentStart(ch) {
		start := l.pos
		l.pos++
		for l.pos < len(l.src) && isAwkIdentPart(l.src[l.pos]) {
			l.pos++
		}
		word := l.src[start:l.pos]
		if kw, ok := awkKeywords[word]; ok {
			return emit(kw, word, 0), nil
		}
		return emit(awkTokIdent, word, 0), nil
	}

	// Punctuation & multi-char operators
	l.pos++
	next := byte(0)
	if l.pos < len(l.src) {
		next = l.src[l.pos]
	}

	switch ch {
	case ';':
		return emit(awkTokSemi, ";", 0), nil
	case '(':
		return emit(awkTokLParen, "(", 0), nil
	case ')':
		return emit(awkTokRParen, ")", 0), nil
	case '{':
		return emit(awkTokLBrace, "{", 0), nil
	case '}':
		return emit(awkTokRBrace, "}", 0), nil
	case '[':
		return emit(awkTokLBracket, "[", 0), nil
	case ']':
		return emit(awkTokRBracket, "]", 0), nil
	case ',':
		return emit(awkTokComma, ",", 0), nil
	case '?':
		return emit(awkTokQuestion, "?", 0), nil
	case ':':
		return emit(awkTokColon, ":", 0), nil
	case '$':
		return emit(awkTokDollar, "$", 0), nil
	case '~':
		return emit(awkTokMatch, "~", 0), nil
	case '+':
		if next == '+' {
			l.pos++
			return emit(awkTokInc, "++", 0), nil
		}
		if next == '=' {
			l.pos++
			return emit(awkTokAddAssign, "+=", 0), nil
		}
		return emit(awkTokPlus, "+", 0), nil
	case '-':
		if next == '-' {
			l.pos++
			return emit(awkTokDec, "--", 0), nil
		}
		if next == '=' {
			l.pos++
			return emit(awkTokSubAssign, "-=", 0), nil
		}
		return emit(awkTokMinus, "-", 0), nil
	case '*':
		if next == '*' {
			l.pos++
			if l.pos < len(l.src) && l.src[l.pos] == '=' {
				l.pos++
				return emit(awkTokPowAssign, "**=", 0), nil
			}
			return emit(awkTokCaret, "**", 0), nil
		}
		if next == '=' {
			l.pos++
			return emit(awkTokMulAssign, "*=", 0), nil
		}
		return emit(awkTokStar, "*", 0), nil
	case '%':
		if next == '=' {
			l.pos++
			return emit(awkTokModAssign, "%=", 0), nil
		}
		return emit(awkTokPercent, "%", 0), nil
	case '^':
		if next == '=' {
			l.pos++
			return emit(awkTokPowAssign, "^=", 0), nil
		}
		return emit(awkTokCaret, "^", 0), nil
	case '=':
		if next == '=' {
			l.pos++
			return emit(awkTokEQ, "==", 0), nil
		}
		return emit(awkTokAssign, "=", 0), nil
	case '!':
		if next == '=' {
			l.pos++
			return emit(awkTokNE, "!=", 0), nil
		}
		if next == '~' {
			l.pos++
			return emit(awkTokNotMatch, "!~", 0), nil
		}
		return emit(awkTokBang, "!", 0), nil
	case '<':
		if next == '=' {
			l.pos++
			return emit(awkTokLE, "<=", 0), nil
		}
		return emit(awkTokLT, "<", 0), nil
	case '>':
		if next == '>' {
			l.pos++
			return emit(awkTokAppend, ">>", 0), nil
		}
		if next == '=' {
			l.pos++
			return emit(awkTokGE, ">=", 0), nil
		}
		return emit(awkTokGT, ">", 0), nil
	case '&':
		if next == '&' {
			l.pos++
			return emit(awkTokAnd, "&&", 0), nil
		}
		return awkToken{}, fmt.Errorf("line %d: unexpected character '&'", tokLine)
	case '|':
		if next == '|' {
			l.pos++
			return emit(awkTokOr, "||", 0), nil
		}
		return emit(awkTokPipe, "|", 0), nil
	default:
		return awkToken{}, fmt.Errorf("line %d: unexpected character %q", tokLine, ch)
	}
}

func isAwkIdentStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isAwkIdentPart(ch byte) bool {
	return isAwkIdentStart(ch) || (ch >= '0' && ch <= '9')
}

func isValidAwkIdent(s string) bool {
	if len(s) == 0 || !isAwkIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isAwkIdentPart(s[i]) {
			return false
		}
	}
	return true
}

func (l *awkLexer) scanNumber() (string, float64, error) {
	start := l.pos
	if l.src[l.pos] == '0' && l.pos+1 < len(l.src) && (l.src[l.pos+1] == 'x' || l.src[l.pos+1] == 'X') {
		l.pos += 2
		hexStart := l.pos
		for l.pos < len(l.src) && isAwkHexDigit(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == hexStart {
			return "", 0, fmt.Errorf("line %d: invalid hex literal", l.line)
		}
		raw := l.src[start:l.pos]
		u, err := strconv.ParseUint(l.src[hexStart:l.pos], 16, 64)
		if err != nil {
			f, ferr := strconv.ParseFloat(raw, 64)
			if ferr != nil {
				return "", 0, fmt.Errorf("line %d: invalid hex literal %s", l.line, raw)
			}
			return raw, f, nil
		}
		return raw, float64(u), nil
	}

	for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		l.pos++
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		expPos := l.pos
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		digitStart := l.pos
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
		if l.pos == digitStart {
			// e.g. `1e` followed by identifier -> rewind before 'e'
			l.pos = expPos
		}
	}
	raw := l.src[start:l.pos]
	val, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return "", 0, fmt.Errorf("line %d: invalid number %q", l.line, raw)
	}
	return raw, val, nil
}

func (l *awkLexer) scanString() (string, error) {
	startLine := l.line
	l.pos++ // skip opening '"'
	var sb strings.Builder
	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		if ch == '"' {
			l.pos++
			return sb.String(), nil
		}
		if ch == '\n' {
			return "", fmt.Errorf("line %d: unterminated string literal", startLine)
		}
		if ch == '\\' {
			l.pos++
			if l.pos >= len(l.src) {
				return "", fmt.Errorf("line %d: unterminated string literal", startLine)
			}
			// Backslash-newline inside string literal continues string
			if l.src[l.pos] == '\n' {
				l.pos++
				l.line++
				continue
			}
			if l.src[l.pos] == '\r' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '\n' {
				l.pos += 2
				l.line++
				continue
			}
			b, consumed := awkDecodeEscape(l.src, l.pos)
			sb.WriteByte(b)
			l.pos += consumed
			continue
		}
		sb.WriteByte(ch)
		l.pos++
	}
	return "", fmt.Errorf("line %d: unterminated string literal", startLine)
}

// scanRegex scans a `/.../` regex literal, handling `\/` escapes and `[...]`
// bracket expressions containing unescaped `/`.
func (l *awkLexer) scanRegex() (string, error) {
	startLine := l.line
	l.pos++ // skip opening '/'
	var sb strings.Builder
	inBracket := false
	bracketStart := false

	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		if ch == '\n' {
			return "", fmt.Errorf("line %d: unterminated regexp", startLine)
		}
		if ch == '\\' {
			if l.pos+1 >= len(l.src) || l.src[l.pos+1] == '\n' {
				return "", fmt.Errorf("line %d: unterminated regexp", startLine)
			}
			next := l.src[l.pos+1]
			if next == '/' {
				sb.WriteByte('/')
			} else {
				sb.WriteByte('\\')
				sb.WriteByte(next)
			}
			l.pos += 2
			bracketStart = false
			continue
		}
		if !inBracket {
			if ch == '/' {
				l.pos++
				return sb.String(), nil
			}
			if ch == '[' {
				inBracket = true
				bracketStart = true
				sb.WriteByte('[')
				l.pos++
				if l.pos < len(l.src) && l.src[l.pos] == '^' {
					sb.WriteByte('^')
					l.pos++
				}
				continue
			}
			sb.WriteByte(ch)
			l.pos++
			continue
		}
		// Inside [...]
		if ch == '[' && l.pos+1 < len(l.src) && (l.src[l.pos+1] == ':' || l.src[l.pos+1] == '.' || l.src[l.pos+1] == '=') {
			delim := l.src[l.pos+1]
			sb.WriteByte('[')
			sb.WriteByte(delim)
			l.pos += 2
			for l.pos < len(l.src) {
				if l.src[l.pos] == delim && l.pos+1 < len(l.src) && l.src[l.pos+1] == ']' {
					sb.WriteByte(delim)
					sb.WriteByte(']')
					l.pos += 2
					break
				}
				if l.src[l.pos] == '\n' {
					return "", fmt.Errorf("line %d: unterminated regexp", startLine)
				}
				sb.WriteByte(l.src[l.pos])
				l.pos++
			}
			bracketStart = false
			continue
		}
		if ch == ']' && !bracketStart {
			inBracket = false
			sb.WriteByte(']')
			l.pos++
			continue
		}
		bracketStart = false
		sb.WriteByte(ch)
		l.pos++
	}
	return "", fmt.Errorf("line %d: unterminated regexp", startLine)
}

// awkUnescapeC unescapes C/AWK escape sequences in `-v var=val` and `-F fs`.
func awkUnescapeC(s string) string {
	if strings.IndexByte(s, '\\') < 0 {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '\\' || i+1 >= len(s) {
			sb.WriteByte(s[i])
			i++
			continue
		}
		b, consumed := awkDecodeEscape(s, i+1)
		sb.WriteByte(b)
		i += 1 + consumed
	}
	return sb.String()
}

// awkDecodeEscape decodes the escape sequence starting at s[idx] (the byte
// immediately after '\\') and returns (decodedByte, bytesConsumed).
func awkDecodeEscape(s string, idx int) (byte, int) {
	ch := s[idx]
	switch ch {
	case 'a':
		return '\a', 1
	case 'b':
		return '\b', 1
	case 'f':
		return '\f', 1
	case 'n':
		return '\n', 1
	case 'r':
		return '\r', 1
	case 't':
		return '\t', 1
	case 'v':
		return '\v', 1
	case '\\':
		return '\\', 1
	case '"':
		return '"', 1
	case '/':
		return '/', 1
	case 'x':
		if idx+1 < len(s) && isAwkHexDigit(s[idx+1]) {
			v := awkHexVal(s[idx+1])
			consumed := 2
			if idx+2 < len(s) && isAwkHexDigit(s[idx+2]) {
				v = v*16 + awkHexVal(s[idx+2])
				consumed = 3
			}
			return v, consumed
		}
		return 'x', 1
	default:
		if ch >= '0' && ch <= '7' {
			v := int(ch - '0')
			consumed := 1
			for consumed < 3 && idx+consumed < len(s) && s[idx+consumed] >= '0' && s[idx+consumed] <= '7' {
				v = v*8 + int(s[idx+consumed]-'0')
				consumed++
			}
			return byte(v), consumed
		}
		return ch, 1
	}
}

func isAwkHexDigit(ch byte) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}

func awkHexVal(ch byte) byte {
	switch {
	case ch >= '0' && ch <= '9':
		return ch - '0'
	case ch >= 'a' && ch <= 'f':
		return ch - 'a' + 10
	default:
		return ch - 'A' + 10
	}
}

// compileAwkRegex compiles an AWK ERE pattern into Go's regexp.Regexp with
// POSIX AWK semantics (`.` matches `\n`, `^`/`$` match whole-string boundaries).
func compileAwkRegex(pat string) (*regexp.Regexp, error) {
	norm := normalizeAwkRegex(pat)
	return regexp.Compile("(?s)" + norm)
}

func normalizeAwkRegex(pat string) string {
	if strings.IndexByte(pat, '\\') < 0 {
		return pat
	}
	var sb strings.Builder
	sb.Grow(len(pat) + 4)
	inBracket := false
	bracketStart := false
	for i := 0; i < len(pat); i++ {
		ch := pat[i]
		if ch == '\\' && i+1 < len(pat) {
			next := pat[i+1]
			switch next {
			case '/', '"':
				sb.WriteByte(next)
				i++
			case '<', '>', 'y':
				if !inBracket {
					sb.WriteString(`\b`)
				} else {
					sb.WriteByte(next)
				}
				i++
			case 'B':
				if !inBracket {
					sb.WriteString(`\B`)
				} else {
					sb.WriteByte('B')
				}
				i++
			default:
				if next >= '0' && next <= '7' {
					v := int(next - '0')
					consumed := 1
					for consumed < 3 && i+1+consumed < len(pat) && pat[i+1+consumed] >= '0' && pat[i+1+consumed] <= '7' {
						v = v*8 + int(pat[i+1+consumed]-'0')
						consumed++
					}
					fmt.Fprintf(&sb, `\x%02x`, byte(v))
					i += consumed
				} else {
					sb.WriteByte('\\')
					sb.WriteByte(next)
					i++
				}
			}
			bracketStart = false
			continue
		}
		if !inBracket {
			if ch == '[' {
				inBracket = true
				bracketStart = true
				sb.WriteByte('[')
				if i+1 < len(pat) && pat[i+1] == '^' {
					sb.WriteByte('^')
					i++
				}
				continue
			}
			sb.WriteByte(ch)
			continue
		}
		if ch == '[' && i+1 < len(pat) && (pat[i+1] == ':' || pat[i+1] == '.' || pat[i+1] == '=') {
			delim := pat[i+1]
			sb.WriteByte('[')
			sb.WriteByte(delim)
			i += 2
			for i < len(pat) {
				if pat[i] == delim && i+1 < len(pat) && pat[i+1] == ']' {
					sb.WriteByte(delim)
					sb.WriteByte(']')
					i++
					break
				}
				sb.WriteByte(pat[i])
				i++
			}
			bracketStart = false
			continue
		}
		if ch == ']' && !bracketStart {
			inBracket = false
		} else {
			bracketStart = false
		}
		sb.WriteByte(ch)
	}
	return sb.String()
}
