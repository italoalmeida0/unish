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
	"math"
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// reHexFloatExp normalizes hex-float exponents (p+00 -> p+0).
var reHexFloatExp = regexp.MustCompile(`(?P<p>[pP])(?P<sign>[+-])0+(?P<exp>[0-9]+)`)

// cmdPrintf implements `printf FORMAT [ARGS...]`.
func cmdPrintf(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 2 {
		fmt.Fprintln(hc.Stderr, "printf: missing operand")
		return exitError{2}
	}
	if args[1] == "--" {
		args = append(args[:1], args[2:]...)
		if len(args) < 2 {
			fmt.Fprintln(hc.Stderr, "printf: missing operand")
			return exitError{2}
		}
	}
	format, operands := args[1], args[2:]
	specs := parsePrintfSpecs(format)
	out, warnings, err := renderPrintf(format, specs, operands)
	for _, w := range warnings {
		fmt.Fprintf(hc.Stderr, "printf: %s\n", w)
	}
	if err != nil {
		fmt.Fprintf(hc.Stderr, "printf: %v\n", err)
		return exitError{1}
	}
	fmt.Fprint(hc.Stdout, out)
	for _, w := range warnings {
		if !strings.HasPrefix(w, "warning:") {
			return exitError{1}
		}
	}
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
// excess operands like GNU. Warnings (bad numeric conversions) do not
// stop the formatting; they make the exit status 1 like bash.
func renderPrintf(format string, specs []printfSpec, operands []string) (string, []string, error) {
	for _, sp := range specs {
		switch sp.verb {
		case 's', 'd', 'i', 'u', 'o', 'x', 'X', 'f', 'e', 'E', 'g', 'G', 'c', 'b', 'a', 'A', 'q':
		case 0:
			return "", nil, fmt.Errorf("format ends with %%")
		default:
			return "", nil, fmt.Errorf("invalid format char: %c", sp.verb)
		}
	}
	if len(specs) == 0 {
		// No conversions: still expand backslash escapes in format
		// (and %% collapses to %).
		return strings.ReplaceAll(expandPrintfEscapes(format, false), "%%", "%"), nil, nil
	}
	var sb strings.Builder
	var warnings []string
	ai := 0 // operand index
	// The format is reused as often as necessary to consume all the
	// arguments; stop when a pass consumes nothing new.
	for pass := 0; ; pass++ {
		startAI := ai
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
				if width == "*" {
					width, _, ai = nextOperand(operands, ai)
				}
				if prec == "*" {
					var v string
					v, _, ai = nextOperand(operands, ai)
					prec = v
				}
			}
			arg := ""
			argOK := true
			if sp.verb != '%' {
				arg, argOK, ai = nextOperand(operands, ai)
			}
			s, warn, err := formatOne(sp.verb, flags, width, prec, arg, argOK)
			if warn != "" && argOK {
				warnings = append(warnings, warn)
			}
			if err != nil {
				return "", warnings, err
			}
			sb.WriteString(s)
		}
		if ai >= len(operands) || ai == startAI {
			break
		}
	}
	return sb.String(), warnings, nil
}

func nextOperand(operands []string, i int) (string, bool, int) {
	if i < len(operands) {
		return operands[i], true, i + 1
	}
	return "", false, i
}

// printfIntArg parses an integer operand the way bash/GNU printf do:
// strtoimax-style with base detection (0x hex, 0 octal, else decimal).
// It returns the value and a diagnostic for bad conversions.
func printfIntArg(arg string) (int64, string) {
	s := arg
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\v' || s[i] == '\f' || s[i] == '\r') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	base := 10
	if i < len(s) && s[i] == '0' {
		if i+2 < len(s) && (s[i+1] == 'x' || s[i+1] == 'X') && printfIsHex(s[i+2]) {
			base = 16
			i += 2
		} else {
			base = 8
			i++
		}
	}
	var val int64
	digits := 0
	const maxI = int64(^uint64(0) >> 1)
	clamped := false
	for i < len(s) {
		var d int
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			d = -1
		}
		if d < 0 || d >= base {
			break
		}
		if val > (maxI-int64(d))/int64(base) {
			clamped = true
			val = maxI
		} else {
			val = val*int64(base) + int64(d)
		}
		digits++
		i++
	}
	if digits == 0 {
		return 0, arg + ": invalid number"
	}
	if clamped {
		if neg {
			val = -maxI - 1
		}
		return val, "warning: " + arg + ": Numerical result out of range"
	}
	if neg {
		val = -val
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\v' || s[i] == '\f' || s[i] == '\r') {
		i++
	}
	if i < len(s) {
		return val, arg + ": invalid number"
	}
	return val, ""
}

// formatOne renders a single conversion. The second return is an
// optional warning (bad numeric operand).
func formatOne(verb byte, flags, width, prec, arg string, _ bool) (string, string, error) {
	if verb == 'b' {
		s := expandPrintfEscapes(arg, true)
		return applyStrFormat(flags, width, prec, s), "", nil
	}
	if verb == 'q' {
		return applyStrFormat(flags, width, "", printfQuote(arg)), "", nil
	}
	if verb == 'c' {
		s := arg
		if s != "" {
			// Bash %c: first character (byte) of the argument.
			s = s[:1]
		}
		return applyStrFormat(flags, width, "", s), "", nil
	}
	if verb == 's' {
		return applyStrFormat(flags, width, prec, arg), "", nil
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
		n, warn := printfIntArg(arg)
		return fmt.Sprintf(goVerb, n), warn, nil
	case 'u':
		goVerb += "d"
		n, warn := printfIntArg(arg)
		// bash/GNU wrap negatives modulo 2^64 for %u.
		return fmt.Sprintf(goVerb, uint64(n)), warn, nil
	case 'o', 'x', 'X':
		goVerb += string(verb)
		n, warn := printfIntArg(arg)
		return fmt.Sprintf(goVerb, uint64(n)), warn, nil
	case 'f', 'e', 'E', 'a', 'A':
		f, warn := parseFloatArg(arg)
		if s, ok := printfNonFinite(flags, width, f); ok {
			return s, warn, nil
		}
		if verb == 'a' {
			verb = 'x' // Go prints C99 hex floats via %x
		} else if verb == 'A' {
			verb = 'X'
		}
		goVerb += string(verb)
		s := fmt.Sprintf(goVerb, f)
		// C prints hex-float exponents with the fewest digits; Go pads to two.
		s = reHexFloatExp.ReplaceAllString(s, "${p}${sign}${exp}")
		return s, warn, nil
	case 'g', 'G':
		f, warn := parseFloatArg(arg)
		if s, ok := printfNonFinite(flags, width, f); ok {
			return s, warn, nil
		}
		return formatCFloat('g', flags, width, prec, verb == 'G', f), warn, nil
	}
	return "", "", fmt.Errorf("invalid format char: %c", verb)
}

func parseFloatArg(arg string) (float64, string) {
	f, err := strconv.ParseFloat(strings.TrimSpace(arg), 64)
	if err != nil {
		return 0, arg + ": invalid number"
	}
	return f, ""
}

// printfNonFinite renders nan/inf like GNU/bash printf (lowercase).
func printfNonFinite(flags, width string, f float64) (string, bool) {
	switch {
	case math.IsNaN(f):
		return applyStrFormat(flags, width, "", "nan"), true
	case math.IsInf(f, 1):
		return applyStrFormat(flags, width, "", "inf"), true
	case math.IsInf(f, -1):
		return applyStrFormat(flags, width, "", "-inf"), true
	}
	return "", false
}

// printfQuote renders an argument like bash's printf %q.
func printfQuote(s string) string {
	if s == "" {
		return "''"
	}
	hasCtrl := false
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			hasCtrl = true
		}
	}
	var b strings.Builder
	if hasCtrl {
		b.WriteString("$'")
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch c {
			case '\n':
				b.WriteString(`\n`)
			case '\t':
				b.WriteString(`\t`)
			case '\r':
				b.WriteString(`\r`)
			case '\\':
				b.WriteString(`\\`)
			case '\'':
				b.WriteString(`\'`)
			default:
				if c < 0x20 || c == 0x7f {
					fmt.Fprintf(&b, `\%03o`, c)
				} else {
					b.WriteByte(c)
				}
			}
		}
		b.WriteString("'")
		return b.String()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '@' || c == '%' || c == '+' || c == '=' || c == ':' ||
			c == ',' || c == '.' || c == '/' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	return b.String()
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
		case '"':
			sb.WriteByte('"')
		case 'e':
			sb.WriteByte(0x1b)
		case 'u', 'U':
			// \uHHHH (up to 4 hex) / \UHHHHHHHH (up to 8 hex) -> UTF-8.
			max := 4
			if s[i] == 'U' {
				max = 8
			}
			j := i + 1
			val, n := 0, 0
			for j < len(s) && n < max && printfIsHex(s[j]) {
				val = val*16 + hexVal(s[j])
				j++
				n++
			}
			if n > 0 && val <= 0x10FFFF {
				sb.WriteRune(rune(val))
				i = j - 1
			} else {
				sb.WriteByte('\\')
				sb.WriteByte(s[i])
			}
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
