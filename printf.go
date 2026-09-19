package main

// printf bypass for mvdan/sh.
//
// mvdan/sh's printf lacks precision specs (`%.0s` errors with "invalid
// format char: ."), which real scripts use (e.g. `printf 'a%.0s'
// $(seq 1 500)` to repeat a string). This file implements printf with
// GNU-compatible semantics and routes it via callOverride:
//
//   - Verbs: %s %d %i %u %o %x %X %f %e %E %g %G %c %b %% with flags
//     (-, +, space, 0, #), width, and precision.
//   - %b expands backslash escapes in the argument.
//   - %c prints the first character of the argument (bash semantics:
//     `printf %c 65` prints "6", not "A").
//   - The format is reused across excess arguments; missing arguments
//     behave as empty/zero; no trailing newline is added.
//   - Unknown verbs report an error and exit 1.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// cmdPrintf implements `printf FORMAT [ARGS...]`.
func cmdPrintf(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 2 {
		fmt.Fprintln(hc.Stderr, "printf: missing operand")
		return exitError{1}
	}
	format, operands := args[1], args[2:]
	specs := parsePrintfSpecs(format)
	out, err := renderPrintf(format, specs, operands)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "printf: %v\n", err)
		return exitError{1}
	}
	fmt.Fprint(hc.Stdout, out)
	return nil
}

// printfSpec is one parsed conversion.
type printfSpec struct {
	flags string // any of "-+ #0"
	width string // "" or digits or "*"
	prec  string // "" (none) or digits or "*" (after '.')
	verb  byte
}

// parsePrintfSpecs scans the format for conversions (validating verbs).
func parsePrintfSpecs(format string) []printfSpec {
	var specs []printfSpec
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if i < len(format) && format[i] == '%' {
			continue
		}
		var sp printfSpec
		for i < len(format) && strings.IndexByte("-+ #0", format[i]) >= 0 {
			sp.flags += string(format[i])
			i++
		}
		if i < len(format) && (format[i] == '*' || (format[i] >= '0' && format[i] <= '9')) {
			start := i
			for i < len(format) && ((format[i] >= '0' && format[i] <= '9') || format[i] == '*') {
				i++
			}
			sp.width = format[start:i]
		}
		if i < len(format) && format[i] == '.' {
			i++
			start := i
			for i < len(format) && ((format[i] >= '0' && format[i] <= '9') || format[i] == '*') {
				i++
			}
			sp.prec = format[start:i]
		}
		if i < len(format) {
			sp.verb = format[i]
		}
		specs = append(specs, sp)
	}
	return specs
}

// renderPrintf formats operands with format, reusing the format for
// excess operands like GNU.
func renderPrintf(format string, specs []printfSpec, operands []string) (string, error) {
	for _, sp := range specs {
		switch sp.verb {
		case 's', 'd', 'i', 'u', 'o', 'x', 'X', 'f', 'e', 'E', 'g', 'G', 'c', 'b':
		case 0:
			return "", fmt.Errorf("format ends with %%")
		default:
			return "", fmt.Errorf("invalid format char: %c", sp.verb)
		}
	}
	if len(specs) == 0 {
		// No conversions: still expand backslash escapes in format
		// (and %% collapses to %).
		return strings.ReplaceAll(expandPrintfEscapes(format, false), "%%", "%"), nil
	}
	var sb strings.Builder
	ai := 0 // operand index
	// If there are no operands, run the format once with empties.
	rounds := 1
	if len(operands) > 0 {
		// Count operands consumed per round (excluding %% and * which
		// consume extra). Simplify: repeat while operands remain; each
		// round consumes up to len(specs) operands.
		rounds = (len(operands) + len(specs) - 1) / len(specs)
	}
	for r := 0; r < rounds; r++ {
		si := 0
		for i := 0; i < len(format); {
			if format[i] != '%' {
				// Copy literal run, expanding backslash escapes.
				j := i
				for j < len(format) && format[j] != '%' {
					j++
				}
				sb.WriteString(expandPrintfEscapes(format[i:j], false))
				i = j
				continue
			}
			i++ // '%'
			if i < len(format) && format[i] == '%' {
				sb.WriteByte('%')
				i++
				continue
			}
			sp := specs[si]
			si++
			// Advance past flags/width/precision/verb of this spec.
			for i < len(format) && strings.IndexByte("-+ #0", format[i]) >= 0 {
				i++
			}
			for i < len(format) && ((format[i] >= '0' && format[i] <= '9') || format[i] == '*') {
				i++
			}
			if i < len(format) && format[i] == '.' {
				i++
				for i < len(format) && ((format[i] >= '0' && format[i] <= '9') || format[i] == '*') {
					i++
				}
			}
			if i < len(format) {
				i++ // verb
			}
			// Resolve * width/precision from operands.
			width, prec, flags := sp.width, sp.prec, sp.flags
			if strings.Contains(width, "*") || strings.Contains(prec, "*") {
				// Only single * supported per field here.
				if width == "*" {
					width, ai = nextOperand(operands, ai)
				}
				if prec == "*" {
					var v string
					v, ai = nextOperand(operands, ai)
					prec = v
				}
			}
			arg := ""
			if sp.verb != '%' {
				arg, ai = nextOperand(operands, ai)
			}
			s, err := formatOne(sp.verb, flags, width, prec, arg)
			if err != nil {
				return "", err
			}
			sb.WriteString(s)
			_ = r
		}
	}
	return sb.String(), nil
}

func nextOperand(operands []string, i int) (string, int) {
	if i < len(operands) {
		return operands[i], i + 1
	}
	return "", i
}

// formatOne renders a single conversion.
func formatOne(verb byte, flags, width, prec, arg string) (string, error) {
	if verb == 'b' {
		s := expandPrintfEscapes(arg, true)
		return applyStrFormat(flags, width, prec, s), nil
	}
	if verb == 'c' {
		s := arg
		if s != "" {
			// Bash %c: first character (byte) of the argument.
			s = s[:1]
		}
		return applyStrFormat(flags, width, "", s), nil
	}
	if verb == 's' {
		return applyStrFormat(flags, width, prec, arg), nil
	}
	// Numeric verbs.
	goVerb := "%" + flags
	if width != "" {
		goVerb += width
	}
	if prec != "" {
		goVerb += "." + prec
	}
	switch verb {
	case 'd', 'i':
		goVerb += "d"
		n, err := strconv.ParseInt(strings.TrimSpace(arg), 0, 64)
		if err != nil {
			// GNU treats empty/non-numeric as 0 with no error for %d?
			// Actually bash errors? No: printf '%d' foo -> 0 + error msg?
			// Coreutils: prints 0. Keep 0 without error.
			n = 0
		}
		// Go %d with 0x prefix input already handled by base 0.
		_ = n
		return fmt.Sprintf(goVerb, n), nil
	case 'u':
		goVerb += "d"
		// GNU wraps negatives modulo 2^64 for %u.
		var n uint64
		if i, err := strconv.ParseInt(strings.TrimSpace(arg), 0, 64); err == nil {
			n = uint64(i)
		} else if u, err := strconv.ParseUint(strings.TrimSpace(arg), 0, 64); err == nil {
			n = u
		}
		return fmt.Sprintf(goVerb, n), nil
	case 'o', 'x', 'X':
		goVerb += string(verb)
		n, err := strconv.ParseUint(strings.TrimSpace(arg), 0, 64)
		if err != nil {
			n = 0
		}
		return fmt.Sprintf(goVerb, n), nil
	case 'f', 'e', 'E':
		goVerb += string(verb)
		f, err := strconv.ParseFloat(strings.TrimSpace(arg), 64)
		if err != nil {
			f = 0
		}
		return fmt.Sprintf(goVerb, f), nil
	case 'g', 'G':
		f, err := strconv.ParseFloat(strings.TrimSpace(arg), 64)
		if err != nil {
			f = 0
		}
		return formatCFloat('g', flags, width, prec, verb == 'G', f), nil
	}
	return "", fmt.Errorf("invalid format char: %c", verb)
}

// applyStrFormat implements %s width/precision (precision truncates).
func applyStrFormat(flags, width, prec, s string) string {
	if prec != "" && prec != "*" {
		if n, err := strconv.Atoi(prec); err == nil && n < len(s) {
			s = s[:n]
		}
	}
	verb := "%"
	if strings.Contains(flags, "-") {
		verb += "-"
	}
	if width != "" {
		verb += width
	}
	verb += "s"
	if verb == "%s" {
		return s
	}
	return fmt.Sprintf(verb, s)
}

// expandPrintfEscapes expands backslash escapes; pct escapes (%% handled
// by the caller). octal/hex/unicode supported.
func expandPrintfEscapes(s string, pctToo bool) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			sb.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			sb.WriteByte('\n')
		case 't':
			sb.WriteByte('\t')
		case 'r':
			sb.WriteByte('\r')
		case 'a':
			sb.WriteByte('\a')
		case 'b':
			sb.WriteByte('\b')
		case 'v':
			sb.WriteByte('\v')
		case 'f':
			sb.WriteByte('\f')
		case '\\':
			sb.WriteByte('\\')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			val := 0
			for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
				val = val*8 + int(s[j]-'0')
				j++
			}
			sb.WriteByte(byte(val))
			i = j - 1
		case 'x':
			// GNU: at most 2 hex digits after \x.
			j := i + 1
			val := 0
			for j < len(s) && j < i+3 && printfIsHex(s[j]) {
				val = val*16 + hexVal(s[j])
				j++
			}
			sb.WriteByte(byte(val))
			i = j - 1
		default:
			// Unknown escape: backslash stays (GNU) except \c (stop).
			if s[i] == 'c' && pctToo {
				return sb.String()
			}
			sb.WriteByte('\\')
			sb.WriteByte(s[i])
		}
	}
	_ = pctToo
	return sb.String()
}

func printfIsHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// formatCFloat implements C-style %g/%G (default precision 6, %e/%f
// selection by exponent, trailing-zero stripping). Go's %g uses
// shortest-repr instead, which diverges (e.g. 1234567).
func formatCFloat(kind byte, flags, width, prec string, upper bool, f float64) string {
	_ = kind
	p := 6
	if prec != "" {
		if n, err := strconv.Atoi(prec); err == nil {
			p = n
		}
	}
	if p == 0 {
		p = 1
	}
	verb := "%"
	alt := strings.Contains(flags, "#")
	left := strings.Contains(flags, "-")
	plus := strings.Contains(flags, "+")
	space := strings.Contains(flags, " ")
	zero := strings.Contains(flags, "0") && !left
	if left {
		verb += "-"
	} else if zero && width != "" {
		verb += "0"
	}
	if width != "" {
		verb += width
	}
	// NOTE: no precision on the final %s (body is already formatted).
	// Choose %e or %f by decimal exponent like C.
	exp := 0
	if f != 0 {
		for a := f; a >= 10; a /= 10 {
			exp++
		}
		for a := f; a < 1 && a > 0; a *= 10 {
			exp--
		}
		for a := -f; a >= 10; a /= 10 {
			exp++
		}
		for a := -f; a < 1 && a > 0; a *= 10 {
			exp--
		}
	}
	useExp := exp < -4 || exp >= p
	var body string
	if useExp {
		e := "%." + strconv.Itoa(p-1)
		if upper {
			e += "E"
		} else {
			e += "e"
		}
		body = fmt.Sprintf(e, f)
	} else {
		body = fmt.Sprintf("%."+strconv.Itoa(p-1-exp)+"f", f)
	}
	if !alt {
		// Strip trailing zeros + lone decimal point.
		if i := strings.IndexByte(body, '.'); i >= 0 {
			epos := strings.IndexAny(body, "eE")
			mant, expart := body, ""
			if epos >= 0 {
				mant, expart = body[:epos], body[epos:]
			}
			mant = strings.TrimRight(mant, "0")
			mant = strings.TrimSuffix(mant, ".")
			body = mant + expart
		}
	}
	// Normalize exponent to 2+ digits like C (e+06, not e+6).
	if i := strings.IndexAny(body, "eE"); i >= 0 {
		m, e := body[:i+1], body[i+1:]
		sign := ""
		if strings.HasPrefix(e, "+") || strings.HasPrefix(e, "-") {
			sign, e = e[:1], e[1:]
		} else {
			sign = "+"
		}
		for len(e) < 2 {
			e = "0" + e
		}
		body = m + sign + e
	}
	if plus && !strings.HasPrefix(body, "-") && !strings.HasPrefix(body, "+") {
		body = "+" + body
	} else if space && !strings.HasPrefix(body, "-") && !strings.HasPrefix(body, "+") {
		body = " " + body
	}
	return fmt.Sprintf(verb+"s", body)
}
