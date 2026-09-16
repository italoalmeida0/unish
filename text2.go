package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	extraCommands = append(extraCommands,
		extraCmd{"od", cmdOd},
		extraCmd{"nl", cmdNl},
		extraCmd{"tac", cmdTac},
		extraCmd{"rev", cmdRev},
		extraCmd{"fold", cmdFold},
		extraCmd{"expand", cmdExpand},
		extraCmd{"unexpand", cmdUnexpand},
		extraCmd{"join", cmdJoin},
	)
}

func preparseOdArgs(args []string) []string {
	out := []string{args[0]}
	for _, a := range args[1:] {
		if a == "-An" {
			out = append(out, "-An")
			continue
		}
		if len(a) >= 2 && a[0] == '-' && a[1] != '-' {
			if a[1] == 't' && len(a) > 2 {
				out = append(out, "-t", a[2:])
				continue
			}
			if len(a) == 2 && strings.ContainsRune("cxof", rune(a[1])) {
				out = append(out, "-t", string(a[1]))
				continue
			}
			if a == "-d" {
				out = append(out, "-t", "D")
				continue
			}
			if len(a) > 2 && strings.ContainsRune("jNw", rune(a[1])) {
				out = append(out, "-"+string(a[1]), a[2:])
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

func cmdOd(_ context.Context, hc interp.HandlerContext, args []string) error {
	args = preparseOdArgs(args)
	fs := newFlagSet("od", hc.Stderr)
	noAddr := fs.Bool("An", false, "")
	types := []string{}
	fs.Var(stringListFlag(&types), "t", "")
	fs.Var(stringListFlag(&types), "format", "")
	verbose := fs.Bool("v", false, "")
	fs.Var(stringListFlag(nil), "output-duplicates", "")
	var limit, skip uint64
	fs.Uint64Var(&limit, "N", 0, "")
	fs.Uint64Var(&skip, "j", 0, "")
	addrHex := fs.Bool("x", false, "")
	addrDec := fs.Bool("d", false, "")
	addrModeFlag := fs.String("A", "", "")
	fs.StringVar(addrModeFlag, "address-radix", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *verbose {
	}
	addrMode := "o"
	if *noAddr {
		addrMode = "n"
	} else if *addrHex {
		addrMode = "x"
	} else if *addrDec {
		addrMode = "d"
	}
	if *addrModeFlag != "" {
		switch (*addrModeFlag)[:1] {
		case "n":
			addrMode = "n"
		case "x":
			addrMode = "x"
		case "d":
			addrMode = "d"
		default:
			addrMode = "o"
		}
	}
	specs := []string{}
	for _, t := range types {
		specs = append(specs, strings.Fields(t)...)
	}
	if len(specs) == 0 {
		specs = []string{"o2"}
	}
	type unit struct {
		kind string
		size int
	}
	var units []unit
	for _, s := range specs {
		kind := ""
		size := -1
		for _, r := range s {
			switch {
			case r >= '0' && r <= '9':
				if size < 0 {
					size = 0
				}
				size = size*10 + int(r-'0')
			case r == 'o' || r == 'u' || r == 'x' || r == 'd' || r == 'c' || r == 'f' || r == 'D':
				kind += string(r)
			}
		}
		for _, k := range kind {
			ks := string(k)
			sz := size
			if sz < 0 {
				switch ks {
				case "c":
					sz = 1
				case "f":
					sz = 4
				default:
					sz = 2
				}
			}
			if ks == "c" {
				sz = 1
			}
			if sz != 1 && sz != 2 && sz != 4 && sz != 8 && ks != "f" {
				fmt.Fprintf(hc.Stderr, "od: invalid type %q\n", s)
				return exitError{1}
			}
			if ks == "f" && sz != 4 && sz != 8 {
				fmt.Fprintf(hc.Stderr, "od: invalid type %q\n", s)
				return exitError{1}
			}
			units = append(units, unit{ks, sz})
		}
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "od:", err)
		return exitError{1}
	}
	defer closeAll()
	var data []byte
	for _, r := range readers {
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		data = append(data, b...)
	}
	if skip >= uint64(len(data)) {
		data = nil
	} else {
		data = data[skip:]
	}
	if limit > 0 && uint64(len(data)) > limit {
		data = data[:limit]
	}
	off := skip
	const perLine = 16
	var sb strings.Builder
	sb.Grow(len(data)*4 + 64)
	prevSame := false
	var prev []byte
	for i := 0; i < len(data); {
		end := i + perLine
		if end > len(data) {
			end = len(data)
		}
		chunk := data[i:end]
		if !*verbose && len(prev) == len(chunk) && string(prev) == string(chunk) && len(chunk) == perLine {
			if !prevSame {
				sb.WriteString("*\n")
				prevSame = true
			}
			off += uint64(len(chunk))
			i = end
			continue
		}
		prevSame = false
		prev = append(prev[:0], chunk...)
		if addrMode != "n" {
			switch addrMode {
			case "x":
				fmt.Fprintf(&sb, "%06x", off)
			case "d":
				fmt.Fprintf(&sb, "%07d", off)
			default:
				fmt.Fprintf(&sb, "%07o", off)
			}
		}
		for _, u := range units {
			bare := addrMode != "n" && (u.kind == "d" || u.kind == "u") && u.size == 2
			for j := 0; j < len(chunk); {
				rem := len(chunk) - j
				sz := u.size
				var val []byte
				if rem < sz {
					val = make([]byte, sz)
					copy(val, chunk[j:])
					j = len(chunk)
				} else {
					val = chunk[j : j+sz]
					j += sz
				}
				if !bare {
					sb.WriteByte(' ')
				}
				if bare && u.kind == "u" {
					writeOdU2Bare(&sb, val)
				} else {
					writeOdVal(&sb, u.kind, val)
				}
			}
		}
		sb.WriteByte('\n')
		off += uint64(len(chunk))
		i = end
	}
	if addrMode != "n" {
		switch addrMode {
		case "x":
			fmt.Fprintf(&sb, "%06x\n", off)
		case "d":
			fmt.Fprintf(&sb, "%07d\n", off)
		default:
			fmt.Fprintf(&sb, "%07o\n", off)
		}
	} else if len(data) > 0 {
	}
	_, err = io.WriteString(hc.Stdout, sb.String())
	return err
}

func fieldWidth(kind string, size int) int {
	switch kind {
	case "c":
		return 4
	case "f":
		if size == 4 {
			return 15
		}
		return 25
	}
	switch size {
	case 1:
		if kind == "x" {
			return 2
		}
		if kind == "o" {
			return 3
		}
		if kind == "u" {
			return 3
		}
		return 4
	case 2:
		if kind == "x" {
			return 4
		}
		if kind == "o" {
			return 6
		}
		if kind == "u" {
			return 5
		}
		if kind == "D" {
			return 5
		}
		return 6
	case 4:
		if kind == "x" {
			return 8
		}
		if kind == "o" {
			return 11
		}
		if kind == "u" {
			return 10
		}
		return 11
	default:
		if kind == "x" {
			return 16
		}
		if kind == "o" {
			return 22
		}
		if kind == "u" {
			return 20
		}
		return 20
	}
}

func writeOdVal(sb *strings.Builder, kind string, b []byte) {
	switch kind {
	case "c":
		c := b[0]
		var s string
		switch c {
		case 0:
			s = "\\0"
		case '\a':
			s = "\\a"
		case '\b':
			s = "\\b"
		case '\f':
			s = "\\f"
		case '\n':
			s = "\\n"
		case '\r':
			s = "\\r"
		case '\t':
			s = "\\t"
		case '\v':
			s = "\\v"
		default:
			if c >= 32 && c < 127 {
				fmt.Fprintf(sb, "%3c", c)
				return
			}
			fmt.Fprintf(sb, " %03o", c)
			return
		}
		fmt.Fprintf(sb, "%3s", s)
		return
	case "f":
		var s string
		if len(b) == 4 {
			v := math.Float32frombits(binary.LittleEndian.Uint32(b))
			s = trimFloatZeros(fmt.Sprintf("%.7e", float64(v)))
		} else {
			v := math.Float64frombits(binary.LittleEndian.Uint64(b))
			s = trimFloatZeros(fmt.Sprintf("%.7e", v))
		}
		fmt.Fprintf(sb, "%17s", s)
		return
	}
	if kind == "D" {
		var v uint64
		for i := len(b) - 1; i >= 0; i-- {
			v = v*256 + uint64(b[i])
		}
		fmt.Fprintf(sb, "%5d", int64Sign(v, len(b)))
		return
	}
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v*256 + uint64(b[i])
	}
	writeOdInt(sb, kind, v, int64Sign(v, len(b)), len(b))
}

func writeOdU2Bare(sb *strings.Builder, b []byte) {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v*256 + uint64(b[i])
	}
	fmt.Fprintf(sb, "%6d", v)
}

func trimFloatZeros(s string) string {
	e := strings.IndexByte(s, 'e')
	if e < 0 {
		return s
	}
	m, exp := s[:e], s[e:]
	if strings.Contains(m, ".") {
		m = strings.TrimRight(m, "0")
		m = strings.TrimRight(m, ".")
	}
	return m + exp
}

func int64Sign(v uint64, size int) int64 {
	switch size {
	case 1:
		return int64(int8(v))
	case 2:
		return int64(int16(v))
	case 4:
		return int64(int32(v))
	default:
		return int64(v)
	}
}

func writeOdInt(sb *strings.Builder, kind string, v uint64, sv int64, size int) {
	w := fieldWidth(kind, size)
	if kind == "u" && size == 2 && sb.Len() > 0 {
	}
	switch kind {
	case "o":
		fmt.Fprintf(sb, "%0*o", w, v)
	case "x":
		fmt.Fprintf(sb, "%0*x", w, v)
	case "d":
		fmt.Fprintf(sb, "%*d", w, sv)
	default:
		fmt.Fprintf(sb, "%*d", w, v)
	}
}
type stringListFlagT struct{ p *[]string }

func stringListFlag(p *[]string) flag.Value { return stringListFlagT{p} }

func (s stringListFlagT) String() string {
	if s.p == nil {
		return ""
	}
	return strings.Join(*s.p, " ")
}
func (s stringListFlagT) Set(v string) error {
	if s.p == nil {
		return nil
	}
	*s.p = append(*s.p, v)
	return nil
}

func cmdNl(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("nl", hc.Stderr)
	body := fs.String("b", "t", "")
	fs.StringVar(body, "body-numbering", "t", "")
	incr := fs.Uint64("i", 1, "")
	fs.Uint64Var(incr, "line-increment", 1, "")
	start := fs.Int64("v", 1, "")
	fs.Int64Var(start, "starting-line-number", 1, "")
	width := fs.Uint64("w", 6, "")
	fs.Uint64Var(width, "number-width", 6, "")
	sep := fs.String("s", "\t", "")
	fs.StringVar(sep, "number-separator", "\t", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	number := func(blank bool) bool {
		switch *body {
		case "a":
			return true
		case "n":
			return false
		default:
			return !blank
		}
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "nl:", err)
		return exitError{1}
	}
	defer closeAll()
	n := *start
	w := int(*width)
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if number(line == "") {
				fmt.Fprintf(hc.Stdout, "%*d%s%s\n", w, n, *sep, line)
				n += int64(*incr)
			} else if *body == "n" {
				fmt.Fprintf(hc.Stdout, "%*s%s\n", w+1, "", line)
			} else {
				fmt.Fprintln(hc.Stdout, line)
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "nl:", err)
			return exitError{1}
		}
	}
	return nil
}

func cmdTac(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("tac", hc.Stderr)
	before := fs.Bool("b", false, "")
	regex := fs.Bool("r", false, "")
	sep := fs.String("s", "\n", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_ = regex
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tac:", err)
		return exitError{1}
	}
	defer closeAll()
	var data []byte
	for _, r := range readers {
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		data = append(data, b...)
	}
	s := *sep
	if s == "" {
		s = "\n"
	}
	text := string(data)
	var parts []string
	var seps int
	t := text
	for len(t) > 0 {
		idx := strings.Index(t, s)
		if idx < 0 {
			parts = append(parts, t)
			break
		}
		parts = append(parts, t[:idx])
		seps++
		t = t[idx+len(s):]
	}
	var sb strings.Builder
	sb.Grow(len(data) + 16)
	if *before {
		trailing := seps == len(parts)
		if trailing {
			sb.WriteString(s)
		}
		for k := len(parts) - 1; k >= 0; k-- {
			if k > 0 {
				sb.WriteString(s)
			}
			sb.WriteString(parts[k])
		}
	} else {
		for k := len(parts) - 1; k >= 0; k-- {
			sb.WriteString(parts[k])
			if k < seps {
				sb.WriteString(s)
			}
		}
	}
	_, err = io.WriteString(hc.Stdout, sb.String())
	return err
}

func cmdRev(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("rev", hc.Stderr)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "rev:", err)
		return exitError{1}
	}
	defer closeAll()
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			rs := []rune(sc.Text())
			for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
				rs[i], rs[j] = rs[j], rs[i]
			}
			fmt.Fprintln(hc.Stdout, string(rs))
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "rev:", err)
			return exitError{1}
		}
	}
	return nil
}

func cmdFold(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("fold", hc.Stderr)
	width := fs.Uint64("w", 80, "")
	fs.Uint64Var(width, "width", 80, "")
	spaces := fs.Bool("s", false, "")
	fs.BoolVar(spaces, "spaces", false, "")
	bytes := fs.Bool("b", false, "")
	fs.BoolVar(bytes, "bytes", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	w := int(*width)
	if w <= 0 {
		fmt.Fprintln(hc.Stderr, "fold: invalid width")
		return exitError{1}
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "fold:", err)
		return exitError{1}
	}
	defer closeAll()
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				fmt.Fprintln(hc.Stdout, "")
				continue
			}
			type cell struct {
				s string
				w int
			}
			var cells []cell
			for i := 0; i < len(line); {
				c, sz := utf8.DecodeRuneInString(line[i:])
				wc := 1
				if !*bytes {
					switch {
					case c == '\t':
						wc = 8
					case c < 32 || c == 127:
						wc = 0
					case c >= 0x1100 && (c <= 0x115F || c == 0x2329 || c == 0x232A ||
						(c >= 0x2E80 && c <= 0xA4CF) || (c >= 0xAC00 && c <= 0xD7A3) ||
						(c >= 0xF900 && c <= 0xFAFF) || (c >= 0xFE30 && c <= 0xFE4F) ||
						(c >= 0xFF00 && c <= 0xFF60) || (c >= 0xFFE0 && c <= 0xFFE6)):
						wc = 2
					}
				}
				cells = append(cells, cell{line[i : i+sz], wc})
				i += sz
			}
			start, cur := 0, 0
			for idx := 0; idx < len(cells); idx++ {
				if cur+cells[idx].w > w && idx > start {
					end := idx
					if *spaces {
						for k := idx - 1; k > start; k-- {
							if cells[k].s == " " {
								end = k + 1
								break
							}
						}
					}
					var sb strings.Builder
					for _, c := range cells[start:end] {
						sb.WriteString(c.s)
					}
					fmt.Fprintln(hc.Stdout, sb.String())
					start = end
					cur = 0
					for _, c := range cells[start:idx] {
						cur += c.w
					}
				}
				cur += cells[idx].w
			}
			var sb strings.Builder
			for _, c := range cells[start:] {
				sb.WriteString(c.s)
			}
			fmt.Fprintln(hc.Stdout, sb.String())
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "fold:", err)
			return exitError{1}
		}
	}
	return nil
}

func parseTabStops(s string) ([]int, error) {
	var stops []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid tab stop %q", p)
		}
		stops = append(stops, n)
	}
	if len(stops) == 0 {
		return nil, fmt.Errorf("invalid tab stop %q", s)
	}
	return stops, nil
}

func nextTab(col int, stops []int) int {
	if len(stops) == 1 {
		n := stops[0]
		return col + (n - col%n)
	}
	for _, s := range stops {
		if s > col {
			return s
		}
	}
	last, prev := stops[len(stops)-1], stops[len(stops)-2]
	if len(stops) == 1 {
		last, prev = stops[0], 0
	}
	step := last - prev
	if step <= 0 {
		step = last
	}
	n := last
	for n <= col {
		n += step
	}
	return n
}

func cmdExpand(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("expand", hc.Stderr)
	tabs := fs.String("t", "8", "")
	fs.StringVar(tabs, "tabs", "8", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	stops, err := parseTabStops(*tabs)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "expand:", err)
		return exitError{1}
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "expand:", err)
		return exitError{1}
	}
	defer closeAll()
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			var sb strings.Builder
			sb.Grow(len(line) + 8)
			col := 0
			for _, c := range line {
				if c == '\t' {
					nt := nextTab(col, stops)
					for ; col < nt; col++ {
						sb.WriteByte(' ')
					}
				} else {
					sb.WriteRune(c)
					if c == '\b' {
						if col > 0 {
							col--
						}
					} else {
						col++
					}
				}
			}
			fmt.Fprintln(hc.Stdout, sb.String())
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "expand:", err)
			return exitError{1}
		}
	}
	return nil
}

func cmdUnexpand(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("unexpand", hc.Stderr)
	tabs := fs.String("t", "8", "")
	fs.StringVar(tabs, "tabs", "8", "")
	all := fs.Bool("a", false, "")
	fs.BoolVar(all, "all", false, "")
	firstOnly := fs.Bool("first-only", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	stops, err := parseTabStops(*tabs)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "unexpand:", err)
		return exitError{1}
	}
	convertAll := *all || !*firstOnly && *all
	_ = convertAll
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "unexpand:", err)
		return exitError{1}
	}
	defer closeAll()
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			var sb strings.Builder
			sb.Grow(len(line))
			col := 0
			i := 0
			for i < len(line) {
				if line[i] != ' ' {
					if line[i] == '\t' {
						sb.WriteByte('\t')
						col = nextTab(col, stops)
					} else {
						sb.WriteByte(line[i])
						col++
					}
					i++
					continue
				}
				j := i
				for j < len(line) && line[j] == ' ' {
					j++
				}
				n := j - i
				if *firstOnly && sb.Len() > 0 {
					sb.WriteString(line[i:j])
					col += n
				} else if n >= 2 {
					for n > 0 {
						nt := nextTab(col, stops)
						if nt-col <= n {
							sb.WriteByte('\t')
							n -= nt - col
							col = nt
						} else {
							break
						}
					}
					for ; n > 0; n-- {
						sb.WriteByte(' ')
						col++
					}
				} else {
					sb.WriteByte(' ')
					col++
				}
				i = j
			}
			fmt.Fprintln(hc.Stdout, sb.String())
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "unexpand:", err)
			return exitError{1}
		}
	}
	return nil
}

func cmdJoin(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("join", hc.Stderr)
	f1 := fs.Uint64("1", 1, "")
	f2 := fs.Uint64("2", 1, "")
	fj := fs.Uint64("j", 0, "")
	sep := fs.String("t", "", "")
	empty := fs.String("e", "", "")
	af1 := fs.Bool("a1", false, "")
	af2 := fs.Bool("a2", false, "")
	af := []string{}
	fs.Var(stringListFlag(&af), "a", "")
	outFmt := fs.String("o", "", "")
	ignoreCase := fs.Bool("i", false, "")
	fs.BoolVar(ignoreCase, "ignore-case", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	pos := fs.Args()
	if len(pos) != 2 {
		fmt.Fprintln(hc.Stderr, "join: need 2 files")
		return flag.ErrHelp
	}
	k1, k2 := int(*f1), int(*f2)
	if *fj > 0 {
		k1, k2 = int(*fj), int(*fj)
	}
	if k1 < 1 || k2 < 1 {
		fmt.Fprintln(hc.Stderr, "join: invalid field number")
		return exitError{1}
	}
	printUnpaired := map[int]bool{}
	if *af1 {
		printUnpaired[1] = true
	}
	if *af2 {
		printUnpaired[2] = true
	}
	for _, a := range af {
		for _, p := range strings.Fields(a) {
			if p == "1" {
				printUnpaired[1] = true
			} else if p == "2" {
				printUnpaired[2] = true
			}
		}
	}
	type ospec struct{ file, field int }
	var ofmt []ospec
	if *outFmt != "" {
		for _, p := range strings.Split(*outFmt, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if p == "0" {
				ofmt = append(ofmt, ospec{0, 0})
				continue
			}
			dot := strings.IndexByte(p, '.')
			if dot < 0 {
				fmt.Fprintf(hc.Stderr, "join: invalid -o field %q\n", p)
				return exitError{1}
			}
			fn, err1 := strconv.Atoi(p[:dot])
			fd, err2 := strconv.Atoi(p[dot+1:])
			if err1 != nil || err2 != nil || (fn != 1 && fn != 2) || fd < 1 {
				fmt.Fprintf(hc.Stderr, "join: invalid -o field %q\n", p)
				return exitError{1}
			}
			ofmt = append(ofmt, ospec{fn, fd})
		}
	}
	splitter := func(line string) []string {
		if *sep == "" {
			return strings.Fields(line)
		}
		return strings.Split(line, *sep)
	}
	joiner := " "
	if *sep != "" {
		joiner = *sep
	}
	readTable := func(path string) ([][]string, error) {
		if path == "-" {
			var rows [][]string
			sc := bufio.NewScanner(hc.Stdin)
			sc.Buffer(make([]byte, 1024*1024), 1024*1024)
			for sc.Scan() {
				rows = append(rows, splitter(sc.Text()))
			}
			return rows, sc.Err()
		}
		p := resolve(hc.Dir, path)
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		var rows [][]string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			rows = append(rows, splitter(sc.Text()))
		}
		return rows, sc.Err()
	}
	t1, err := readTable(pos[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, "join:", err)
		return exitError{1}
	}
	t2, err := readTable(pos[1])
	if err != nil {
		fmt.Fprintln(hc.Stderr, "join:", err)
		return exitError{1}
	}
	keyOf := func(row []string, k int) (string, bool) {
		if k-1 < len(row) {
			return row[k-1], true
		}
		return "", false
	}
	norm := func(s string) string {
		if *ignoreCase {
			return strings.ToLower(s)
		}
		return s
	}
	field := func(row []string, f int) string {
		if f-1 < len(row) {
			return row[f-1]
		}
		return *empty
	}
	emit := func(key string, r1, r2 []string) {
		if len(ofmt) > 0 {
			parts := make([]string, 0, len(ofmt))
			for _, o := range ofmt {
				switch o.file {
				case 0:
					parts = append(parts, key)
				case 1:
					parts = append(parts, field(r1, o.field))
				default:
					parts = append(parts, field(r2, o.field))
				}
			}
			fmt.Fprintln(hc.Stdout, strings.Join(parts, joiner))
			return
		}
		var parts []string
		parts = append(parts, key)
		for i, v := range r1 {
			if i == k1-1 {
				continue
			}
			parts = append(parts, v)
		}
		for i, v := range r2 {
			if i == k2-1 {
				continue
			}
			parts = append(parts, v)
		}
		fmt.Fprintln(hc.Stdout, strings.Join(parts, joiner))
	}
	emitUnpaired := func(which int, row []string) {
		if len(ofmt) > 0 {
			parts := make([]string, 0, len(ofmt))
			for _, o := range ofmt {
				switch {
				case o.file == 0:
					k, _ := keyOf(row, map[int]int{1: k1, 2: k2}[which])
					parts = append(parts, k)
				case o.file == which:
					parts = append(parts, field(row, o.field))
				default:
					parts = append(parts, *empty)
				}
			}
			fmt.Fprintln(hc.Stdout, strings.Join(parts, joiner))
			return
		}
		emit(map[int]string{1: mustKey(row, k1), 2: mustKey(row, k2)}[which], row, nil)
	}
	i, j := 0, 0
	for i < len(t1) && j < len(t2) {
		k1s, ok1 := keyOf(t1[i], k1)
		k2s, ok2 := keyOf(t2[j], k2)
		if !ok1 {
			i++
			continue
		}
		if !ok2 {
			j++
			continue
		}
		c := strings.Compare(norm(k1s), norm(k2s))
		switch {
		case c == 0:
			i2 := i
			for i2 < len(t1) {
				kk, ok := keyOf(t1[i2], k1)
				if !ok || strings.Compare(norm(kk), norm(k1s)) != 0 {
					break
				}
				i2++
			}
			j2 := j
			for j2 < len(t2) {
				kk, ok := keyOf(t2[j2], k2)
				if !ok || strings.Compare(norm(kk), norm(k2s)) != 0 {
					break
				}
				j2++
			}
			for a := i; a < i2; a++ {
				for b := j; b < j2; b++ {
					emit(k1s, t1[a], t2[b])
				}
			}
			i, j = i2, j2
		case c < 0:
			if printUnpaired[1] {
				emitUnpaired(1, t1[i])
			}
			i++
		default:
			if printUnpaired[2] {
				emitUnpaired(2, t2[j])
			}
			j++
		}
	}
	for ; i < len(t1); i++ {
		if printUnpaired[1] {
			emitUnpaired(1, t1[i])
		}
	}
	for ; j < len(t2); j++ {
		if printUnpaired[2] {
			emitUnpaired(2, t2[j])
		}
	}
	return nil
}

func mustKey(row []string, k int) string {
	if k-1 < len(row) {
		return row[k-1]
	}
	return ""
}
