package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

func cmdGrep(_ context.Context, hc interp.HandlerContext, args []string) error {
	var (
		count, filesOnly, fixed, ignoreCase, invert, lineNum,
		noName, quiet, recursive, extended, onlyMatch bool
		patterns                                   []string
		patternFiles                               []string
		maxCount, after, before, context_           uint64
		excludes, includes, excludeDirs            stringList
		positionals                                []string
	)
	uintVal := func(v string) (uint64, error) {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid number: %s", v)
		}
		return n, nil
	}
	argv := splitAttached("grep", args)[1:]
	badFlag := ""
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "--") && len(a) > 2 {
			name, val, hasVal := a[2:], "", false
			if k := strings.IndexByte(name, '='); k >= 0 {
				name, val, hasVal = name[:k], name[k+1:], true
			}
			needVal := func() (string, bool) {
				if hasVal {
					return val, true
				}
				if i+1 < len(argv) {
					i++
					return argv[i], true
				}
				return "", false
			}
			switch name {
			case "regexp", "e":
				if v, ok := needVal(); ok {
					patterns = append(patterns, v)
				}
			case "file", "f":
				if v, ok := needVal(); ok {
					patternFiles = append(patternFiles, v)
				}
			case "max-count", "m":
				if v, ok := needVal(); ok {
					if n, err := uintVal(v); err == nil {
						maxCount = n
					}
				}
			case "after-context", "A":
				if v, ok := needVal(); ok {
					if n, err := uintVal(v); err == nil {
						after = n
					}
				}
			case "before-context", "B":
				if v, ok := needVal(); ok {
					if n, err := uintVal(v); err == nil {
						before = n
					}
				}
			case "context", "C":
				if v, ok := needVal(); ok {
					if n, err := uintVal(v); err == nil {
						context_ = n
					}
				}
			case "exclude":
				if v, ok := needVal(); ok {
					excludes = append(excludes, v)
				}
			case "include":
				if v, ok := needVal(); ok {
					includes = append(includes, v)
				}
			case "exclude-dir":
				if v, ok := needVal(); ok {
					excludeDirs = append(excludeDirs, v)
				}
			case "count", "c":
				count = true
			case "files-with-matches", "l":
				filesOnly = true
			case "fixed-strings", "F":
				fixed = true
			case "ignore-case", "i":
				ignoreCase = true
			case "invert-match", "v":
				invert = true
			case "line-number", "n":
				lineNum = true
			case "no-filename", "h":
				noName = true
			case "quiet", "silent", "q":
				quiet = true
			case "recursive", "r":
				recursive = true
			case "extended-regexp", "E":
				extended = true
			case "only-matching", "o":
				onlyMatch = true
			default:
				badFlag = a
			}
			continue
		}
		if len(a) == 2 && a[0] == '-' && a[1] != '-' {
			c := a[1]
		withVal := func() (string, bool) {
			if i+1 < len(argv) {
				i++
				return argv[i], true
			}
			return "", false
			}
			switch c {
			case 'c':
				count = true
			case 'l':
				filesOnly = true
			case 'F':
				fixed = true
			case 'i':
				ignoreCase = true
			case 'v':
				invert = true
			case 'n':
				lineNum = true
			case 'h':
				noName = true
			case 'q':
				quiet = true
			case 'r':
				recursive = true
			case 'R':
				recursive = true
			case 'a':
			case 'E':
				extended = true
			case 'o':
				onlyMatch = true
			case 'e':
				if v, ok := withVal(); ok {
					patterns = append(patterns, v)
				}
			case 'f':
				if v, ok := withVal(); ok {
					patternFiles = append(patternFiles, v)
				}
			case 'm', 'A', 'B', 'C':
				if v, ok := withVal(); ok {
					if n, err := uintVal(v); err == nil {
						switch c {
						case 'm':
							maxCount = n
						case 'A':
							after = n
						case 'B':
							before = n
						case 'C':
							context_ = n
						}
					}
				}
			default:
				badFlag = a
			}
			continue
		}
		positionals = append(positionals, a)
	}
	if badFlag != "" {
		fmt.Fprintf(hc.Stderr, "grep: invalid option %q\n", badFlag)
		return exitError{2}
	}
	for _, pf := range patternFiles {
		data, err := os.ReadFile(resolve(hc.Dir, pf))
		if err != nil {
			fmt.Fprintln(hc.Stderr, "grep:", err)
			return exitError{2}
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line != "" {
				patterns = append(patterns, line)
			}
		}
	}
	rest := positionals
	if len(patterns) == 0 {
		if len(rest) == 0 {
			fmt.Fprintln(hc.Stderr, "grep: missing pattern")
			return flag.ErrHelp
		}
		patterns = []string{rest[0]}
		rest = rest[1:]
	}
	if context_ > 0 {
		after, before = context_, context_
	}
	pat := "(?:" + strings.Join(quotePatterns(patterns, fixed), "|") + ")"
	if !fixed && !extended {
		pat = "(?:" + strings.Join(brePatterns(patterns), "|") + ")"
	}
	if ignoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "grep:", err)
		return exitError{2}
	}

	var files []string
	if recursive {
		if len(rest) == 0 {
			rest = []string{"."}
		}
		for _, p := range rest {
			full := resolve(hc.Dir, p)
			fi, e := os.Stat(full)
			if e != nil {
				fmt.Fprintln(hc.Stderr, "grep:", e)
				continue
			}
			if fi.IsDir() {
				filepath.Walk(full, func(path string, info os.FileInfo, e error) error {
					if e != nil {
						return nil
					}
					rel, _ := filepath.Rel(full, path)
					if info.IsDir() {
						for _, xd := range excludeDirs {
							if strings.Contains(rel, xd) || filepath.Base(path) == xd {
								return filepath.SkipDir
							}
						}
						return nil
					}
					base := filepath.Base(path)
					for _, x := range excludes {
						if ok, _ := filepath.Match(x, base); ok {
							return nil
						}
					}
					if len(includes) > 0 {
						hit := false
						for _, in := range includes {
							if ok, _ := filepath.Match(in, base); ok {
								hit = true
								break
							}
							if strings.HasPrefix(in, ".") && strings.HasSuffix(base, in) {
								hit = true
								break
							}
						}
						if !hit {
							return nil
						}
					}
					displayPath := ""
					if p == "." {
						displayPath = "./" + filepath.ToSlash(rel)
					} else {
						displayPath = filepath.ToSlash(filepath.Join(p, rel))
					}
					files = append(files, displayPath)
					return nil
				})
			} else {
				files = append(files, p)
			}
		}
	} else {
		files = rest
	}

	multi := len(files) > 1 || recursive
	matchedAny := false
	emit := func(name string, r io.Reader) {
		m, _ := grepReader(hc, name, multi && !noName, re, invert, lineNum, count, filesOnly, quiet, onlyMatch, int(maxCount), int(after), int(before), r)
		matchedAny = matchedAny || m
	}
	if len(files) == 0 {
		if !recursive && len(rest) == 0 {
			emit("", hc.Stdin)
		}
	} else {
		for _, f := range files {
			if f == "" {
				emit("", hc.Stdin)
				continue
			}
			fh, e := os.Open(resolve(hc.Dir, f))
			if e != nil {
				fmt.Fprintln(hc.Stderr, "grep:", e)
				continue
			}
			func() {
				defer fh.Close()
				emit(f, fh)
			}()
		}
	}
	if !matchedAny {
		return exitError{1}
	}
	return nil
}

func quotePatterns(patterns []string, fixed bool) []string {
	out := make([]string, len(patterns))
	for i, p := range patterns {
		if fixed {
			out[i] = regexp.QuoteMeta(p)
		} else {
			out[i] = p
		}
	}
	return out
}

func brePatterns(patterns []string) []string {
	out := make([]string, len(patterns))
	for i, p := range patterns {
		out[i] = breToGo(p)
	}
	return out
}

func breToGo(pat string) string {
	var sb strings.Builder
	for i := 0; i < len(pat); i++ {
		if pat[i] == '\\' && i+1 < len(pat) {
			n := pat[i+1]
			if strings.ContainsRune("(){}+?|", rune(n)) {
				sb.WriteByte(n)
			} else {
				sb.WriteByte('\\')
				sb.WriteByte(n)
			}
			i++
			continue
		}
		if strings.ContainsRune("+?(){}|", rune(pat[i])) {
			sb.WriteByte('\\')
		}
		sb.WriteByte(pat[i])
	}
	return sb.String()
}

func grepReader(hc interp.HandlerContext, name string, showName bool, re *regexp.Regexp, invert, lineNum, count, filesOnly, quiet, onlyMatch bool, maxCount, after, before int, r io.Reader) (bool, error) {	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	prefix := ""
	if showName && name != "" {
		prefix = name + ":"
	}
	var pending []ctxLine
	sinceMatch := -1
	matched, matchedAny := 0, false
	printed := 0
	printLine := func(ln int, line string) {
		if quiet {
			return
		}
		if filesOnly {
			fmt.Fprintln(hc.Stdout, name)
			return
		}
		if count {
			matched++
			return
		}
		if onlyMatch {
			for _, m := range re.FindAllString(line, -1) {
				if m == "" {
					continue
				}
				if lineNum {
					fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, ln, m)
				} else {
					fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, m)
				}
			}
			return
		}
		if lineNum {
			fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, ln, line)
		} else {
			fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, line)
		}
	}
	ln := 0
	for sc.Scan() {
		ln++
		line := sc.Text()
		ok := re.MatchString(line)
		if invert {
			ok = !ok
		}
		if ok {
			matchedAny = true
			if quiet {
				return true, nil
			}
			if filesOnly {
				fmt.Fprintln(hc.Stdout, name)
				return true, nil
			}
			for _, pl := range pending {
				printLine(pl.ln, pl.text)
			}
			pending = nil
			printLine(ln, line)
			sinceMatch = 0
			printed++
			if maxCount > 0 && !count && printed >= maxCount {
				break
			}
			if maxCount > 0 && count && matched >= maxCount {
				break
			}
		} else {
			if sinceMatch >= 0 {
				sinceMatch++
				if sinceMatch <= after {
					printLine(ln, line)
				} else {
					sinceMatch = -1
				}
			} else if before > 0 {
				pending = append(pending, ctxLine{ln, line})
				if len(pending) > before {
					pending = pending[1:]
				}
			}
		}
	}
	if count && !quiet && !filesOnly {
		fmt.Fprintf(hc.Stdout, "%s%d\n", prefix, matched)
	}
	return matchedAny, sc.Err()
}

type ctxLine struct {
	ln   int
	text string
}

func cmdHead(_ context.Context, hc interp.HandlerContext, args []string) error {
	n, rest := parseN(args[1:], 10)
	fs := newFlagSet("head", hc.Stderr)
	fsN := fs.Uint64("n", n, "")
	fsC := fs.String("c", "", "")
	quiet := fs.Bool("q", false, "")
	verbose := fs.Bool("v", false, "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	n = *fsN
	files := fs.Args()
	readers, names, closeAll, err := openInputs(hc.Dir, files, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "head:", err)
		return exitError{1}
	}
	defer closeAll()
	if *fsC != "" {
		cn, err := parseCount(*fsC)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "head:", err)
			return exitError{1}
		}
		var total uint64
		for _, r := range readers {
			left := cn - total
			if left == 0 {
				break
			}
			w, err := io.CopyN(hc.Stdout, r, int64(left))
			total += uint64(w)
			if err != nil {
				break
			}
		}
		return nil
	}
	multi := len(readers) > 1
	for i, r := range readers {
		if (multi && !*quiet) || *verbose {
			if i > 0 {
				fmt.Fprintln(hc.Stdout)
			}
			fmt.Fprintf(hc.Stdout, "==> %s <==\n", names[i])
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		var c uint64
		for c < n && sc.Scan() {
			fmt.Fprintln(hc.Stdout, sc.Text())
			c++
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "head:", err)
			return exitError{1}
		}
	}
	return nil
}

func parseCount(s string) (uint64, error) {
	mult := uint64(1)
	up := strings.ToUpper(strings.TrimSpace(s))
	for _, suf := range []struct {
		suffix string
		m      uint64
	}{
		{"G", 1024 * 1024 * 1024}, {"M", 1024 * 1024}, {"K", 1024},
		{"B", 0},
	} {
		if strings.HasSuffix(up, suf.suffix) && suf.m > 0 {
			mult = suf.m
			up = up[:len(up)-len(suf.suffix)]
			break
		}
	}
	if strings.HasSuffix(up, "B") {
		up = strings.TrimSuffix(up, "B")
	}
	n, err := parseUint(strings.TrimSpace(up))
	if err != nil {
		return 0, fmt.Errorf("invalid number: %s", s)
	}
	return n * mult, nil
}

func cmdTail(_ context.Context, hc interp.HandlerContext, args []string) error {
	fromLine := uint64(0)
	filtered := args[:1:1]
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if (a == "-n" || a == "--lines") && i+1 < len(argv) &&
			len(argv[i+1]) > 1 && argv[i+1][0] == '+' {
			if v, err := parseUint(argv[i+1][1:]); err == nil && v >= 1 {
				fromLine = v
				i++
				continue
			}
		}
		num := ""
		switch {
		case len(a) > 1 && a[0] == '+':
			num = a[1:]
		case len(a) > 3 && a[0] == '-' && a[1] == 'n' && a[2] == '+':
			num = a[3:]
		}
		if num != "" {
			if v, err := parseUint(num); err == nil && v >= 1 {
				fromLine = v
				continue
			}
		}
		filtered = append(filtered, a)
	}
	n, rest := parseN(filtered[1:], 10)
	fs := newFlagSet("tail", hc.Stderr)
	nStr := fs.String("n", "", "")
	cStr := fs.String("c", "", "")
	quiet := fs.Bool("q", false, "")
	verbose := fs.Bool("v", false, "")
	follow := fs.Bool("f", false, "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	_ = follow
	files := fs.Args()
	if *nStr != "" {
		s := strings.TrimSpace(*nStr)
		if strings.HasPrefix(s, "+") {
			if v, err := parseUint(s[1:]); err == nil && v >= 1 {
				fromLine = v
			}
		} else if v, err := parseUint(s); err == nil {
			n = v
		}
	}
	readers, names, closeAll, err := openInputs(hc.Dir, files, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tail:", err)
		return exitError{1}
	}
	defer closeAll()
	if *cStr != "" {
		cn, err := parseCount(*cStr)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "tail:", err)
			return exitError{1}
		}
		var data []byte
		for _, r := range readers {
			chunk, err := io.ReadAll(r)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "tail:", err)
				return exitError{1}
			}
			data = append(data, chunk...)
		}
		if uint64(len(data)) > cn {
			data = data[len(data)-int(cn):]
		}
		_, err = hc.Stdout.Write(data)
		return err
	}
	_ = names
	multi := len(readers) > 1
	for i, r := range readers {
		if (multi && !*quiet) || *verbose {
			if i > 0 {
				fmt.Fprintln(hc.Stdout)
			}
			fmt.Fprintf(hc.Stdout, "==> %s <==\n", names[i])
		}
		if fromLine > 0 {
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 1024*1024), 1024*1024)
			var ln uint64
			for sc.Scan() {
				ln++
				if ln >= fromLine {
					fmt.Fprintln(hc.Stdout, sc.Text())
				}
			}
			if err := sc.Err(); err != nil {
				fmt.Fprintln(hc.Stderr, "tail:", err)
				return exitError{1}
			}
			continue
		}
		if n == 0 {
			_, _ = io.Copy(io.Discard, r)
			continue
		}
		ring := make([]string, n)
		var count, pos uint64
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			ring[pos] = sc.Text()
			pos = (pos + 1) % n
			count++
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "tail:", err)
			return exitError{1}
		}
		start := uint64(0)
		if count > n {
			start = pos
		}
		shown := count
		if shown > n {
			shown = n
		}
		for k := uint64(0); k < shown; k++ {
			fmt.Fprintln(hc.Stdout, ring[(start+k)%n])
		}
	}
	return nil
}

func cmdSort(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("sort", hc.Stderr)
	reverse := fs.Bool("r", false, "")
	unique := fs.Bool("u", false, "")
	ignoreCase := fs.Bool("f", false, "")
	numeric := fs.Bool("n", false, "")
	check := fs.Bool("c", false, "")
	sep := fs.String("t", "", "")
	key := fs.String("k", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "sort:", err)
		return exitError{1}
	}
	defer closeAll()
	var lines []string
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "sort:", err)
			return exitError{1}
		}
	}
	keySpec := parseSortKey(*key)
	keyFn := func(s string) string {
		if keySpec != nil {
			return sortKeyField(s, *sep, keySpec)
		}
		if *sep != "" {
			if i := strings.Index(s, *sep); i >= 0 {
				s = s[:i]
			}
		}
		if *ignoreCase {
			s = strings.ToLower(s)
		}
		return s
	}
	less := func(a, b sortItem) bool {
		x, y := a.key, b.key
		num := *numeric
		rev := *reverse
		if keySpec != nil {
			num = keySpec.numeric || num
			if keySpec.reverse {
				rev = !rev
			}
		}
		if num {
			if a.num != b.num {
				if rev {
					return a.num > b.num
				}
				return a.num < b.num
			}
			if x != y {
				if rev {
					return x > y
				}
				return x < y
			}
			return false
		}
		if rev {
			return x > y
		}
		return x < y
	}
	if *check {
		for i := 1; i < len(lines); i++ {
			if less(sortItem{lines[i], keyFn(lines[i]), numKey(keyFn(lines[i]))}, sortItem{lines[i-1], keyFn(lines[i-1]), numKey(keyFn(lines[i-1]))}) {
				name := "-"
				if len(fs.Args()) > 0 {
					name = fs.Args()[0]
				}
				fmt.Fprintf(hc.Stderr, "sort: %s:%d: disorder: %s\n", name, i+1, lines[i])
				return exitError{1}
			}
		}
		return nil
	}
	items := make([]sortItem, len(lines))
	for i, l := range lines {
		k := keyFn(l)
		items[i] = sortItem{line: l, key: k, num: numKey(k)}
	}
	sort.SliceStable(items, func(i, j int) bool { return less(items[i], items[j]) })
	prev := ""
	for i, it := range items {
		l := it.line
		if *unique && i > 0 && l == prev {
			continue
		}
		fmt.Fprintln(hc.Stdout, l)
		prev = l
	}
	return nil
}

type sortItem struct {
	line string
	key  string
	num  float64
}

func numKey(k string) float64 {
	s := strings.TrimSpace(firstField(k))
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	dig := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		dig++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			dig++
		}
	}
	if dig == 0 {
		return 0
	}
	if j := i; j < len(s) && (s[j] == 'e' || s[j] == 'E') {
		k := j + 1
		if k < len(s) && (s[k] == '+' || s[k] == '-') {
			k++
		}
		d := 0
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
			d++
		}
		if d > 0 {
			i = k
		}
	}
	f, _ := strconv.ParseFloat(s[:i], 64)
	return f
}

type sortKey struct {
	start, end int
	numeric      bool
	reverse      bool
}

func parseSortKey(s string) *sortKey {
	if s == "" {
		return nil
	}
	k := &sortKey{end: -1}
	part := s
	if i := strings.IndexByte(s, ','); i >= 0 {
		part = s[:i]
		rest := s[i+1:]
		num := ""
		for _, c := range rest {
			if c >= '0' && c <= '9' {
				num += string(c)
			} else if c == 'n' {
				k.numeric = true
			} else if c == 'r' {
				k.reverse = true
			}
		}
		if num != "" {
				if v, err := strconv.Atoi(num); err == nil {
					k.end = v
				}
			}
	}
	num := ""
	for _, c := range part {
		if c >= '0' && c <= '9' {
			num += string(c)
		} else if c == 'n' {
			k.numeric = true
		} else if c == 'r' {
			k.reverse = true
		}
	}
	if num == "" {
		return nil
	}
	if v, err := strconv.Atoi(num); err == nil {
		k.start = v
	} else {
		return nil
	}
	return k
}

func sortKeyField(line, sep string, k *sortKey) string {
	if sep == "" {
		sep = " "
	}
	var fields []string
	if sep == " " {
		fields = strings.Fields(line)
	} else {
		fields = strings.Split(line, sep)
	}
	lo := k.start - 1
	if lo < 0 {
		lo = 0
	}
	hi := len(fields)
	if k.end > 0 && k.end < hi {
		hi = k.end
	}
	if lo >= len(fields) {
		return ""
	}
	return strings.Join(fields[lo:hi], sep)
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}

func cmdUniq(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("uniq", hc.Stderr)
	count := fs.Bool("c", false, "")
	dupOnly := fs.Bool("d", false, "")
	uniqueOnly := fs.Bool("u", false, "")
	ignoreCase := fs.Bool("i", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files := fs.Args()
	var in, out string
	if len(files) > 0 {
		in = files[0]
	}
	if len(files) > 1 {
		out = files[1]
	}
	var inputs []string
	if in != "" {
		inputs = []string{in}
	}
	readers, _, closeAll, err := openInputs(hc.Dir, inputs, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "uniq:", err)
		return exitError{1}
	}
	defer closeAll()
	w := io.Writer(hc.Stdout)
	if out != "" {
		f, e := os.Create(resolve(hc.Dir, out))
		if e != nil {
			fmt.Fprintln(hc.Stderr, "uniq:", e)
			return exitError{1}
		}
		defer f.Close()
		w = f
	}
	norm := func(s string) string {
		if *ignoreCase {
			return strings.ToLower(s)
		}
		return s
	}
	var prev, prevRaw string
	n := 0
	flush := func() {
		if n == 0 {
			return
		}
		show := true
		if *dupOnly && n < 2 {
			show = false
		}
		if *uniqueOnly && n > 1 {
			show = false
		}
		if show {
			if *count {
				fmt.Fprintf(w, "%7d %s\n", n, prevRaw)
			} else {
				fmt.Fprintln(w, prevRaw)
			}
		}
	}
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if n == 0 || norm(line) != prev {
				flush()
				prev, prevRaw, n = norm(line), line, 1
			} else {
				n++
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "uniq:", err)
			return exitError{1}
		}
	}
	flush()
	return nil
}

func cmdWc(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("wc", hc.Stderr)
	lines := fs.Bool("l", false, "")
	words := fs.Bool("w", false, "")
	bytes_ := fs.Bool("c", false, "")
	chars := fs.Bool("m", false, "")
	maxLine := fs.Bool("L", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !*lines && !*words && !*bytes_ && !*chars && !*maxLine {
		*lines, *words, *bytes_ = true, true, true
	}
	files := fs.Args()
	readers, names, closeAll, err := openInputs(hc.Dir, files, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "wc:", err)
		return exitError{1}
	}
	defer closeAll()
	tl, tw, tb, tc, tm := 0, 0, 0, 0, 0
	showTotal := len(readers) > 1
	width := 7
	if len(files) > 0 {
		var maxBytes int64
		for _, f := range files {
			if fi, err := os.Stat(resolve(hc.Dir, f)); err == nil {
				if fi.Size() > maxBytes {
					maxBytes = fi.Size()
				}
			}
		}
		width = len(strconv.FormatInt(maxBytes, 10))
		if width < 1 {
			width = 1
		}
	}
	row := func(vals ...int) string {
		if len(vals) == 1 {
			return strconv.Itoa(vals[0])
		}
		strs := make([]string, len(vals))
		for i, v := range vals {
			strs[i] = fmt.Sprintf("%*d", width, v)
		}
		return strings.Join(strs, " ")
	}
	colVals := func(l, w, b, nchars, maxLen int) []int {
		var vals []int
		if *lines {
			vals = append(vals, l)
		}
		if *words {
			vals = append(vals, w)
		}
		if *chars && !*bytes_ {
			vals = append(vals, nchars)
		} else if *bytes_ || *chars {
			vals = append(vals, b)
		}
		if *maxLine {
			vals = append(vals, maxLen)
		}
		return vals
	}
	for i, r := range readers {
		l, w, b := 0, 0, 0
		chars, maxLineLen := 0, 0
		data, err := io.ReadAll(r)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "wc:", err)
			return exitError{1}
		}
		b = len(data)
		text := string(data)
		chars = len([]rune(text))
		for _, line := range strings.Split(text, "\n") {
			if n := len([]rune(line)); n > maxLineLen {
				maxLineLen = n
			}
		}
		l = strings.Count(text, "\n")
		inWord := false
		for _, ch := range text {
			if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
				inWord = false
			} else if !inWord {
				inWord = true
				w++
			}
		}
		tl, tw, tb, tc, tm = tl+l, tw+w, tb+b, tc+chars, max(maxLineLen, tm)
		line := row(colVals(l, w, b, chars, maxLineLen)...)
		if len(files) > 0 {
			line += " " + names[i]
		}
		fmt.Fprintln(hc.Stdout, line)
	}
	if showTotal {
		fmt.Fprintln(hc.Stdout, row(colVals(tl, tw, tb, tc, tm)...)+" total")
	}
	return nil
}

func cmdTee(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("tee", hc.Stderr)
	append_ := fs.Bool("a", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	writers := []io.Writer{hc.Stdout}
	for _, f := range fs.Args() {
		fl := os.O_CREATE | os.O_WRONLY
		if *append_ {
			fl |= os.O_APPEND
		} else {
			fl |= os.O_TRUNC
		}
		fh, err := os.OpenFile(resolve(hc.Dir, f), fl, 0644)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "tee:", err)
			return exitError{1}
		}
		defer fh.Close()
		writers = append(writers, fh)
	}
	_, err := io.Copy(io.MultiWriter(writers...), hc.Stdin)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func cmdTr(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("tr", hc.Stderr)
	del := fs.Bool("d", false, "")
	squeeze := fs.Bool("s", false, "")
	complement := fs.Bool("c", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(hc.Stderr, "tr: missing operand")
		return flag.ErrHelp
	}
	from := expandTrSet(rest[0])
	if *complement && !*del && len(rest) < 2 {
		_, err := io.Copy(hc.Stdout, hc.Stdin)
		return err
	}
	if *complement {
		present := make(map[byte]bool, len(from))
		for _, b := range from {
			present[b] = true
		}
		from = nil
		for i := 0; i < 256; i++ {
			if !present[byte(i)] {
				from = append(from, byte(i))
			}
		}
	}
	if *del {
		data, err := io.ReadAll(hc.Stdin)
		if err != nil {
			return err
		}
		delSet := make(map[byte]bool, len(from))
		for _, b := range from {
			delSet[b] = true
		}
		out := make([]byte, 0, len(data))
		for _, b := range data {
			if !delSet[b] {
				out = append(out, b)
			}
		}
		_, err = hc.Stdout.Write(out)
		return err
	}
	if len(rest) < 2 {
		if *squeeze {
			data, err := io.ReadAll(hc.Stdin)
			if err != nil {
				return err
			}
			_, err = hc.Stdout.Write(squeezeBytes(data, from))
			return err
		}
		fmt.Fprintln(hc.Stderr, "tr: missing operand after "+rest[0])
		return flag.ErrHelp
	}
	to := expandTrSet(rest[1])
	var table [256]byte
	for i := range table {
		table[i] = byte(i)
	}
	for i, b := range from {
		c := to[len(to)-1]
		if i < len(to) {
			c = to[i]
		}
		table[b] = c
	}
	data, err := io.ReadAll(hc.Stdin)
	if err != nil {
		return err
	}
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = table[b]
	}
	if *squeeze {
		out = squeezeBytes(out, to)
	}
	_, err = hc.Stdout.Write(out)
	return err
}

func squeezeBytes(data, set []byte) []byte {
	inSet := make(map[byte]bool, len(set))
	for _, b := range set {
		inSet[b] = true
	}
	out := data[:0:0]
	var prev byte
	hasPrev := false
	for _, b := range data {
		if hasPrev && b == prev && inSet[b] {
			continue
		}
		out = append(out, b)
		prev, hasPrev = b, true
	}
	return out
}

func expandTrSet(s string) []byte {
	var out []byte
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '0':
				out = append(out, 0)
			case 'a':
				out = append(out, '\a')
			case 'b':
				out = append(out, '\b')
			case 'v':
				out = append(out, '\v')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 't':
				out = append(out, '\t')
			case 'r':
				out = append(out, '\r')
			case '\\':
				out = append(out, '\\')
			default:
				out = append(out, s[i+1])
			}
			i += 2
			continue
		}
		if i+2 < len(s) && s[i+1] == '-' && s[i] <= s[i+2] {
			for c := s[i]; c <= s[i+2]; c++ {
				out = append(out, c)
			}
			i += 3
			continue
		}
		out = append(out, s[i])
		i++
	}
	return out
}

func cmdSeq(_ context.Context, hc interp.HandlerContext, args []string) error {
	nums, _ := parseN(args[1:], 0)
	_ = nums
	rest := args[1:]
	var first, step, last float64 = 1, 1, 0
	switch len(rest) {
	case 1:
		v, err := strconv.ParseFloat(rest[0], 64)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "seq: invalid number:", rest[0])
			return exitError{1}
		}
		last = v
	case 2:
		f, err1 := strconv.ParseFloat(rest[0], 64)
		l, err2 := strconv.ParseFloat(rest[1], 64)
		if err1 != nil || err2 != nil {
			fmt.Fprintln(hc.Stderr, "seq: invalid number")
			return exitError{1}
		}
		first, last = f, l
	case 3:
		f, err1 := strconv.ParseFloat(rest[0], 64)
		s, err2 := strconv.ParseFloat(rest[1], 64)
		l, err3 := strconv.ParseFloat(rest[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			fmt.Fprintln(hc.Stderr, "seq: invalid number")
			return exitError{1}
		}
		first, step, last = f, s, l
	default:
		fmt.Fprintln(hc.Stderr, "seq: usage: seq [first [step]] last")
		return flag.ErrHelp
	}
	if step == 0 {
		fmt.Fprintln(hc.Stderr, "seq: step cannot be zero")
		return exitError{1}
	}
	var sb strings.Builder
	sb.Grow(4096)
	for v := first; (step > 0 && v <= last) || (step < 0 && v >= last); v += step {
		if v == float64(int64(v)) {
			sb.WriteString(strconv.FormatInt(int64(v), 10))
		} else {
			sb.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		}
		sb.WriteByte('\n')
	}
	_, err := io.WriteString(hc.Stdout, sb.String())
	return err
}

func cmdCut(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("cut", hc.Stderr)
	delim := fs.String("d", "\t", "")
	fields := fs.String("f", "", "")
	chars := fs.String("c", "", "")
	onlyDelim := fs.Bool("s", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *chars != "" {
		return cmdCutChars(hc, *chars, fs.Args())
	}
	if *fields == "" {
		fmt.Fprintln(hc.Stderr, "cut: specify -f (fields) or -c (characters)")
		return flag.ErrHelp
	}
	d := *delim
	if d == "" {
		d = "\t"
	}
	spans, err := parseFieldList(*fields)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cut:", err)
		return exitError{1}
	}
	in := func(idx int) bool {
		for _, s := range spans {
			if idx >= s.lo && (s.hi == -1 || idx <= s.hi) {
				return true
			}
		}
		return false
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cut:", err)
		return exitError{1}
	}
	defer closeAll()
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.Contains(line, d) {
				if !*onlyDelim {
					fmt.Fprintln(hc.Stdout, line)
				}
				continue
			}
			parts := strings.Split(line, d)
			var out []string
			for i := range parts {
				if in(i + 1) {
					out = append(out, parts[i])
				}
			}
			fmt.Fprintln(hc.Stdout, strings.Join(out, d))
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "cut:", err)
			return exitError{1}
		}
	}
	return nil
}

type fieldSpan struct{ lo, hi int }

func parseFieldList(s string) ([]fieldSpan, error) {
	var spans []fieldSpan
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			ab := strings.SplitN(part, "-", 2)
			lo, hi := 1, -1
			if ab[0] != "" {
				v, err := strconv.Atoi(ab[0])
				if err != nil {
					return nil, fmt.Errorf("invalid field: %s", part)
				}
				lo = v
			}
			if ab[1] != "" {
				v, err := strconv.Atoi(ab[1])
				if err != nil {
					return nil, fmt.Errorf("invalid field: %s", part)
				}
				hi = v
			}
			spans = append(spans, fieldSpan{lo, hi})
		} else {
			v, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid field: %s", part)
			}
			spans = append(spans, fieldSpan{v, v})
		}
	}
	if len(spans) == 0 {
		return nil, fmt.Errorf("invalid field list")
	}
	return spans, nil
}

func cmdCutChars(hc interp.HandlerContext, list string, files []string) error {
	spans, err := parseFieldList(list)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cut:", err)
		return exitError{1}
	}
	readers, _, closeAll, err := openInputs(hc.Dir, files, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cut:", err)
		return exitError{1}
	}
	defer closeAll()
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			runes := []rune(sc.Text())
			var out []rune
			for i := range runes {
				for _, s := range spans {
					if i+1 >= s.lo && (s.hi == -1 || i+1 <= s.hi) {
						out = append(out, runes[i])
						break
					}
				}
			}
			fmt.Fprintln(hc.Stdout, string(out))
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "cut:", err)
			return exitError{1}
		}
	}
	return nil
}

func cmdPaste(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("paste", hc.Stderr)
	delims := fs.String("d", "\t", "")
	serial := fs.Bool("s", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) == 0 {
		data, err := io.ReadAll(hc.Stdin)
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		d := []rune(*delims)
		if len(d) == 0 {
			d = []rune{'\t'}
		}
		if *serial {
			fmt.Fprintln(hc.Stdout, strings.Join(lines, string(d[0])))
			return nil
		}
		for _, l := range lines {
			fmt.Fprintln(hc.Stdout, l)
		}
		return nil
	}
	d := []rune(*delims)
	if len(d) == 0 {
		d = []rune{'\t'}
	}
	readers, _, closeAll, err := openInputs(hc.Dir, files, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "paste:", err)
		return exitError{1}
	}
	defer closeAll()
	scanners := make([]*bufio.Scanner, len(readers))
	for i, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		scanners[i] = sc
	}
	if *serial {
		for i, sc := range scanners {
			var parts []string
			for sc.Scan() {
				parts = append(parts, sc.Text())
			}
			fmt.Fprintln(hc.Stdout, strings.Join(parts, string(d[i%len(d)])))
		}
		return nil
	}
	for {
		var parts []string
		any := false
		for _, sc := range scanners {
			if sc.Scan() {
				parts = append(parts, sc.Text())
				any = true
			} else {
				parts = append(parts, "")
			}
		}
		if !any {
			break
		}
		var sb strings.Builder
		for i, p := range parts {
			if i > 0 {
				sb.WriteRune(d[(i-1)%len(d)])
			}
			sb.WriteString(p)
		}
		fmt.Fprintln(hc.Stdout, sb.String())
	}
	return nil
}

func cmdComm(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("comm", hc.Stderr)
	sup1 := fs.Bool("1", false, "")
	sup2 := fs.Bool("2", false, "")
	sup3 := fs.Bool("3", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) != 2 {
		fmt.Fprintln(hc.Stderr, "comm: need 2 files")
		return flag.ErrHelp
	}
	readers, _, closeAll, err := openInputs(hc.Dir, files, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "comm:", err)
		return exitError{1}
	}
	defer closeAll()
	readAll := func(r io.Reader) []string {
		var out []string
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			out = append(out, sc.Text())
		}
		return out
	}
	a, b := readAll(readers[0]), readAll(readers[1])
	i, j := 0, 0
	tabs := func(col int) string {
		n := 0
		for c := 1; c < col; c++ {
				suppressed := (c == 1 && *sup1) || (c == 2 && *sup2)
				if !suppressed {
					n++
				}
			}
			return strings.Repeat("\t", n)
		}
	for i < len(a) || j < len(b) {
		switch {
		case j >= len(b) || (i < len(a) && a[i] < b[j]):
			if !*sup1 {
				fmt.Fprintln(hc.Stdout, tabs(1)+a[i])
			}
			i++
		case i >= len(a) || (j < len(b) && b[j] < a[i]):
			if !*sup2 {
				fmt.Fprintln(hc.Stdout, tabs(2)+b[j])
			}
			j++
		default:
			if !*sup3 {
				fmt.Fprintln(hc.Stdout, tabs(3)+a[i])
			}
			i++
			j++
		}
	}
	return nil
}

func cmdSplit(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("split", hc.Stderr)
	lines := fs.Uint64("l", 1000, "")
	sufLen := fs.Uint64("a", 2, "")
	numeric := fs.Bool("d", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	var in string
	prefix := "x"
	if len(rest) > 0 {
		in = rest[0]
	}
	if len(rest) > 1 {
		prefix = rest[1]
	}
	var inputs []string
	if in != "" {
		inputs = []string{in}
	}
	readers, _, closeAll, err := openInputs(hc.Dir, inputs, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "split:", err)
		return exitError{1}
	}
	defer closeAll()
	suffix := func(n int) string {
		if *numeric {
			return fmt.Sprintf("%0*d", *sufLen, n)
		}
		s := ""
		for i := uint64(0); i < *sufLen; i++ {
			s = string(rune('a'+n%26)) + s
			n /= 26
		}
		return s
	}
	part, count := 0, uint64(0)
	var out *os.File
	closeOut := func() {
		if out != nil {
			out.Close()
			out = nil
		}
	}
	defer closeOut()
	for _, r := range readers {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			if out == nil || count >= *lines {
				closeOut()
				p := resolve(hc.Dir, prefix+suffix(part))
				f, e := os.Create(p)
				if e != nil {
					fmt.Fprintln(hc.Stderr, "split:", e)
					return exitError{1}
				}
				out = f
				part++
				count = 0
			}
			fmt.Fprintln(out, sc.Text())
			count++
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "split:", err)
			return exitError{1}
		}
	}
	return nil
}

func cmdDiff(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("diff", hc.Stderr)
	brief := fs.Bool("q", false, "")
	unified := fs.Bool("u", false, "")
	reportSame := fs.Bool("s", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) != 2 {
		fmt.Fprintln(hc.Stderr, "diff: need 2 files")
		return flag.ErrHelp
	}
	ra, err := os.ReadFile(resolve(hc.Dir, files[0]))
	if err != nil {
		fmt.Fprintln(hc.Stderr, "diff:", err)
		return exitError{2}
	}
	rb, err := os.ReadFile(resolve(hc.Dir, files[1]))
	if err != nil {
		fmt.Fprintln(hc.Stderr, "diff:", err)
		return exitError{2}
	}
	a := splitLines(string(ra))
	b := splitLines(string(rb))
	if strings.Join(a, "\n") == strings.Join(b, "\n") {
		if *reportSame {
			fmt.Fprintf(hc.Stdout, "Files %s and %s are identical\n", files[0], files[1])
		}
		return nil
	}
	if *brief {
		fmt.Fprintf(hc.Stdout, "Files %s and %s differ\n", files[0], files[1])
		return exitError{1}
	}
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for x := range dp {
		dp[x] = make([]int, m+1)
	}
	for x := n - 1; x >= 0; x-- {
		for y := m - 1; y >= 0; y-- {
			if a[x] == b[y] {
				dp[x][y] = dp[x+1][y+1] + 1
			} else if dp[x+1][y] >= dp[x][y+1] {
				dp[x][y] = dp[x+1][y]
			} else {
				dp[x][y] = dp[x][y+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, diffOp{'=', a[i]})
			i++
			j++
		case i >= n:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		case j >= m:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	if *unified {
		emitUnified(hc, files, a, b, ops)
		return exitError{1}
	}
	la, lb := 0, 0
	k := 0
	for k < len(ops) {
		if ops[k].kind == '=' {
			la++
			lb++
			k++
			continue
		}
		sa, sb := la, lb
		var dels, adds []string
		for k < len(ops) && ops[k].kind != '=' {
			if ops[k].kind == '-' {
				dels = append(dels, ops[k].text)
				la++
			} else {
				adds = append(adds, ops[k].text)
				lb++
			}
			k++
		}
		ea, eb := la, lb
		switch {
		case len(dels) > 0 && len(adds) > 0:
			fmt.Fprintf(hc.Stdout, "%sc%s\n", arange(sa+1, ea), crange(sb+1, eb))
			for _, d := range dels {
				fmt.Fprintln(hc.Stdout, "< "+d)
			}
			fmt.Fprintln(hc.Stdout, "---")
			for _, ad := range adds {
				fmt.Fprintln(hc.Stdout, "> "+ad)
			}
		case len(dels) > 0:
			fmt.Fprintf(hc.Stdout, "%sd%d\n", arange(sa+1, ea), sb)
			for _, d := range dels {
				fmt.Fprintln(hc.Stdout, "< "+d)
			}
		default:
			fmt.Fprintf(hc.Stdout, "%da%s\n", sa, arange(sb+1, eb))
			for _, ad := range adds {
				fmt.Fprintln(hc.Stdout, "> "+ad)
			}
		}
	}
	return exitError{1}
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func arange(lo, hi int) string {
	if lo == hi {
		return itoa(lo)
	}
	return itoa(lo) + "," + itoa(hi)
}

func crange(lo, hi int) string {
	if lo == hi {
		return itoa(lo)
	}
	return itoa(lo) + "," + itoa(hi)
}

func itoa(n int) string { return strconv.Itoa(n) }

type diffOp struct {
	kind byte
	text string
}

func emitUnified(hc interp.HandlerContext, files []string, a, b []string, ops []diffOp) {
	_ = a
	_ = b
	fmt.Fprintf(hc.Stdout, "--- %s\t%s\n", files[0], fileTime(files[0]))
	fmt.Fprintf(hc.Stdout, "+++ %s\t%s\n", files[1], fileTime(files[1]))
	const ctx = 3
	changed := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind != '=' {
			for d := -ctx; d <= ctx; d++ {
				if k+d >= 0 && k+d < len(ops) {
					changed[k+d] = true
				}
			}
		}
	}
	la, lb := 1, 1
	k := 0
	for k < len(ops) {
		if !changed[k] {
			if ops[k].kind == '=' {
				la++
				lb++
			} else if ops[k].kind == '-' {
				la++
			} else {
				lb++
			}
			k++
			continue
		}
		ha, hb := la, lb
		var body []string
		ca, cb := 0, 0
		for k < len(ops) && changed[k] {
			switch ops[k].kind {
			case '=':
				body = append(body, " "+ops[k].text)
				la++
				lb++
				ca++
				cb++
			case '-':
				body = append(body, "-"+ops[k].text)
				la++
				ca++
			case '+':
				body = append(body, "+"+ops[k].text)
				lb++
				cb++
			}
			k++
		}
		fmt.Fprintf(hc.Stdout, "@@ -%s +%s @@\n", hunkRange(ha, ca), hunkRange(hb, cb))
		for _, l := range body {
			fmt.Fprintln(hc.Stdout, l)
		}
	}
}

func hunkRange(start, count int) string {
	if count == 1 {
		return itoa(start)
	}
	return itoa(start) + "," + itoa(count)
}

func fileTime(path string) string {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime().Format("2006-01-02 15:04:05.000000000 -0700")
	}
	return ""
}

func cmdCmp(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("cmp", hc.Stderr)
	silent := fs.Bool("s", false, "")
	listAll := fs.Bool("l", false, "")
	maxBytes := fs.Uint64("n", 0, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) < 2 {
		fmt.Fprintln(hc.Stderr, "cmp: missing operand")
		return flag.ErrHelp
	}
	fa, err := os.Open(resolve(hc.Dir, files[0]))
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cmp:", err)
		return exitError{2}
	}
	defer fa.Close()
	fb, err := os.Open(resolve(hc.Dir, files[1]))
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cmp:", err)
		return exitError{2}
	}
	defer fb.Close()
	ba := bufio.NewReader(fa)
	bb := bufio.NewReader(fb)
	var pos uint64
	lineNo := 1
	differ := false
	for {
		if *maxBytes > 0 && pos >= *maxBytes {
			break
		}
		ca, ea := ba.ReadByte()
		cb, eb := bb.ReadByte()
		if ea != nil || eb != nil {
			if (ea != nil) != (eb != nil) {
				if !*silent {
					fmt.Fprintf(hc.Stdout, "%s %s differ: EOF\n", files[0], files[1])
				}
				differ = true
			}
			break
		}
		pos++
		if ca != cb {
			differ = true
			if *listAll {
				fmt.Fprintf(hc.Stdout, "%d %o %o\n", pos, ca, cb)
				continue
			}
			if !*silent {
				fmt.Fprintf(hc.Stdout, "%s %s differ: byte %d, line %d\n", files[0], files[1], pos, lineNo)
			}
			break
		}
		if ca == '\n' {
			lineNo++
		}
	}
	if differ {
		return exitError{1}
	}
	return nil
}

func cmdHexdump(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("hexdump", hc.Stderr)
	canonical := fs.Bool("C", false, "")
	limit := fs.Uint64("n", 0, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "hexdump:", err)
		return exitError{1}
	}
	defer closeAll()
	var off uint64
	for _, r := range readers {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if *limit > 0 && uint64(len(data)) > *limit {
			data = data[:*limit]
		}
		if !*canonical {
			for i := 0; i < len(data); i += 16 {
				end := i + 16
				if end > len(data) {
					end = len(data)
				}
				chunk := data[i:end]
				var sb strings.Builder
				fmt.Fprintf(&sb, "%07o", off+uint64(i))
				for j := 0; j < 16; j += 2 {
					if j+1 < len(chunk) {
						fmt.Fprintf(&sb, " %02x%02x", chunk[j+1], chunk[j])
					} else if j < len(chunk) {
						fmt.Fprintf(&sb, "   %02x", chunk[j])
					} else {
						sb.WriteString("     ")
					}
				}
				fmt.Fprintln(hc.Stdout, sb.String())
			}
			off += uint64(len(data))
			continue
		}
		for i := 0; i < len(data); i += 16 {
			end := i + 16
			if end > len(data) {
				end = len(data)
			}
			chunk := data[i:end]
			fmt.Fprintf(hc.Stdout, "%08x  ", off+uint64(i))
			for j := 0; j < 16; j++ {
				if j < len(chunk) {
					fmt.Fprintf(hc.Stdout, "%02x ", chunk[j])
				} else {
					fmt.Fprint(hc.Stdout, "   ")
				}
				if j == 7 {
					fmt.Fprint(hc.Stdout, " ")
				}
			}
			if *canonical {
				fmt.Fprint(hc.Stdout, " |")
				for _, b := range chunk {
					if b >= 32 && b < 127 {
						fmt.Fprintf(hc.Stdout, "%c", b)
					} else {
						fmt.Fprint(hc.Stdout, ".")
					}
				}
				fmt.Fprint(hc.Stdout, "|")
			}
			fmt.Fprintln(hc.Stdout)
		}
		off += uint64(len(data))
	}
	if *canonical {
		fmt.Fprintf(hc.Stdout, "%08x\n", off)
	} else {
		fmt.Fprintf(hc.Stdout, "%07o\n", off)
	}
	return nil
}

func cmdStrings(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("strings", hc.Stderr)
	minLen := fs.Uint64("n", 4, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "strings:", err)
		return exitError{1}
	}
	defer closeAll()
	for _, r := range readers {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		var cur []byte
		flush := func() {
			if uint64(len(cur)) >= *minLen {
				fmt.Fprintln(hc.Stdout, string(cur))
			}
			cur = nil
		}
		for _, b := range data {
			if b >= 32 && b < 127 || b == '\t' {
				cur = append(cur, b)
			} else {
				flush()
			}
		}
		flush()
	}
	return nil
}
