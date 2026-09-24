package main

import (
	"bufio"
	"bytes"
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
		lineRegexp, wordRegexp, withName, noMessages bool
		label                                        string
		patterns                                     []string
		patternFiles                                 []string
		maxCount, after, before, context_            uint64
		excludes, includes, excludeDirs              stringList
		positionals                                  []string
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
	endFlags := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if endFlags {
			positionals = append(positionals, a)
			continue
		}
		if a == "--" {
			endFlags = true
			continue
		}
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
			case "with-filename", "H":
				withName = true
			case "no-messages", "s":
				noMessages = true
			case "line-regexp", "x":
				lineRegexp = true
			case "word-regexp", "w":
				wordRegexp = true
			case "label":
				if v, ok := needVal(); ok {
					label = v
				}
			case "quiet", "silent", "q":
				quiet = true
			case "recursive", "r":
				recursive = true
			case "extended-regexp", "E":
				extended = true
			case "only-matching", "o":
				onlyMatch = true
			case "text", "a":
				// GNU -a/--text: process binary files as text.
				// This is already the default behavior (no NUL
				// short-circuit skips content), so just accept it.
			case "devices", "directories", "binary-files", "D", "d", "I", "U":
				// Accepted for compatibility; consume optional value.
				if !hasVal && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
					i++
				}
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
			case 'H':
				withName = true
			case 's':
				noMessages = true
			case 'x':
				lineRegexp = true
			case 'w':
				wordRegexp = true
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
		if len(a) > 2 && a[0] == '-' && a[1] != '-' {
			ok := true
			for k := 1; k < len(a); k++ {
				switch a[k] {
				case 'c', 'l', 'F', 'i', 'v', 'n', 'h', 'H', 's', 'x', 'w', 'q', 'r', 'R', 'a', 'E', 'o':
				default:
					ok = false
				}
			}
			if ok {
				for k := 1; k < len(a); k++ {
					switch a[k] {
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
					case 'H':
						withName = true
					case 's':
						noMessages = true
					case 'x':
						lineRegexp = true
					case 'w':
						wordRegexp = true
					case 'q':
						quiet = true
					case 'r', 'R':
						recursive = true
					case 'E':
						extended = true
					case 'o':
						onlyMatch = true
					}
				}
				continue
			}
		}
		positionals = append(positionals, a)
	}
	if badFlag != "" {
		fmt.Fprintf(hc.Stderr, "grep: invalid option %q\n", badFlag)
		return exitError{2}
	}
	for _, pf := range patternFiles {
		data, err := readShellFile(resolve(hc.Dir, pf))
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
	if lineRegexp {
		pat = "^(?:" + strings.Join(quotePatterns(patterns, fixed), "|") + ")$"
		if !fixed && !extended {
			pat = "^(?:" + strings.Join(brePatterns(patterns), "|") + ")$"
		}
	}
	if wordRegexp {
		inner := strings.Join(quotePatterns(patterns, fixed), "|")
		if !fixed && !extended {
			inner = strings.Join(brePatterns(patterns), "|")
		}
		// Capture the word itself in group 1; boundary chars stay
		// outside so `-o` prints exactly the match (no trailing space).
		pat = "(?:^|[^A-Za-z0-9_])((?:" + inner + "))(?:[^A-Za-z0-9_]|$)"
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
	matchedAny := false
	hadError := false
	if recursive {
		if len(rest) == 0 {
			rest = []string{"."}
		}
		for _, p := range rest {
			full := resolve(hc.Dir, p)
			fi, e := os.Stat(full)
			if e != nil {
				if !noMessages {
					fmt.Fprintln(hc.Stderr, "grep:", e)
				}
				hadError = true
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

	// GNU shows filenames only with multiple files or when actually
	// descending into a directory (-r with a FILE operand behaves like a
	// direct file search: no filename prefix).
	descendedDir := false
	if recursive {
		for _, f := range rest {
			if f == "" || f == "-" {
				continue
			}
			if fi, err := os.Stat(resolve(hc.Dir, f)); err == nil && fi.IsDir() {
				descendedDir = true
				break
			}
		}
	}
	showName := (len(files) > 1 || descendedDir) && !noName
	if withName {
		showName = true
	}
	emit := func(name string, r io.Reader) {
		disp := name
		if label != "" && (name == "" || name == "standard input" || name == "-") {
			disp = label
			showName = true
		}
		// GNU: a matching BINARY file prints "Binary file X matches"
		// instead of raw bytes (unless -a given, which we treat as text).
		if name != "" && name != "-" && !quiet && !count && !filesOnly {
			if data, err := io.ReadAll(r); err == nil {
				if bytes.IndexByte(data, 0) >= 0 {
					if reMatchAny(re, invert, data) {
						fmt.Fprintf(hc.Stderr, "grep: %s: binary file matches\n", disp)
						matchedAny = true
					}
					return
				}
				m, _ := grepReaderWord(hc, disp, showName, re, wordRegexp, invert, lineNum, count, filesOnly, quiet, onlyMatch, int(maxCount), int(after), int(before), bytes.NewReader(data))
				matchedAny = matchedAny || m
				return
			}
		}
		m, _ := grepReaderWord(hc, disp, showName, re, wordRegexp, invert, lineNum, count, filesOnly, quiet, onlyMatch, int(maxCount), int(after), int(before), r)
		matchedAny = matchedAny || m
	}
	if len(files) == 0 {
		if !recursive && len(rest) == 0 {
			emit("", hc.Stdin)
		}
	} else {
		for _, f := range files {
			if f == "" || f == "-" || f == "/dev/stdin" {
				emit("", hc.Stdin)
				continue
			}
			fh, e := openShellFile(resolve(hc.Dir, f))
			if e != nil {
				if !noMessages {
					fmt.Fprintln(hc.Stderr, "grep:", e)
				}
				hadError = true
				continue
			}
			func() {
				defer fh.Close()
				emit(f, fh)
			}()
		}
	}
	if hadError {
		return exitError{2}
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

func grepReader(hc interp.HandlerContext, name string, showName bool, re *regexp.Regexp, invert, lineNum, count, filesOnly, quiet, onlyMatch bool, maxCount, after, before int, r io.Reader) (bool, error) {
	sc := newLineReader(r)
	prefix := ""
	if showName && name != "" {
		prefix = name + ":"
	}
	var pending []ctxLine
	sinceMatch := -1
	matched, matchedAny := 0, false
	printed := 0
	// GNU context format: match lines use ":" separators, context
	// lines use "-" (e.g. "12: match" vs "11- context").
	printLine := func(ln int, line string, isMatch bool) {
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
		// GNU markers: with -n, "12:match"/"11-context"; with a
		// filename, "f:12:match"/"f-11-context"; otherwise no marker.
		if lineNum {
			sep := ":"
			if (after > 0 || before > 0) && !isMatch {
				sep = "-"
			}
			fmt.Fprintf(hc.Stdout, "%s%d%s%s\n", prefix, ln, sep, line)
		} else if prefix != "" && (after > 0 || before > 0) && !isMatch {
			fmt.Fprintf(hc.Stdout, "%s-%s\n", prefix, line)
		} else {
			fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, line)
		}
	}
	ln := 0
	lastOut := 0
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
			// GNU "--" separator between disjoint context groups.
			if (after > 0 || before > 0) && lastOut > 0 && ln-lastOut > 1 {
				fmt.Fprintln(hc.Stdout, "--")
			}
			for _, pl := range pending {
				printLine(pl.ln, pl.text, false)
				lastOut = pl.ln
			}
			pending = nil
			printLine(ln, line, true)
			lastOut = ln
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
					printLine(ln, line, false)
					lastOut = ln
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

// grepReaderWord wraps grepReader, fixing `-o -w` to print only the word
// (group 1) instead of the boundary characters consumed by the match.
func grepReaderWord(hc interp.HandlerContext, name string, showName bool, re *regexp.Regexp, wordMode, invert bool, lineNum, count, filesOnly, quiet, onlyMatch bool, maxCount, after, before int, r io.Reader) (bool, error) {
	if !wordMode || !onlyMatch {
		return grepReader(hc, name, showName, re, invert, lineNum, count, filesOnly, quiet, onlyMatch, maxCount, after, before, r)
	}
	sc := newLineReader(r)
	prefix := ""
	if showName && name != "" {
		prefix = name + ":"
	}
	matchedAny := false
	ln := 0
	for sc.Scan() {
		ln++
		line := sc.Text()
		subs := re.FindAllStringSubmatch(line, -1)
		if invert {
			if len(subs) == 0 {
				matchedAny = true
				if lineNum {
					fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, ln, line)
				} else {
					fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, line)
				}
			}
			continue
		}
		for _, m := range subs {
			if len(m) < 2 || m[1] == "" {
				continue
			}
			matchedAny = true
			if quiet {
				return true, nil
			}
			if lineNum {
				fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, ln, m[1])
			} else {
				fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, m[1])
			}
		}
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
	fsN := fs.String("n", strconv.FormatUint(n, 10), "")
	fsC := fs.String("c", "", "")
	quiet := fs.Bool("q", false, "")
	verbose := fs.Bool("v", false, "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	// GNU `head -n -N` (negative) prints all but the last N lines.
	negLines := int64(-1)
	if strings.HasPrefix(*fsN, "-") {
		if v, err := strconv.ParseInt(*fsN, 10, 64); err == nil && v < 0 {
			negLines = -v
		} else {
			fmt.Fprintln(hc.Stderr, "head: invalid number:", *fsN)
			return exitError{1}
		}
	} else {
		var err error
		n, err = parseCountUint(*fsN)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "head:", err)
			return exitError{1}
		}
	}
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
	if negLines >= 0 {
		// All but last negLines lines: ring buffer.
		multi := len(readers) > 1
		for i, r := range readers {
			if (multi && !*quiet) || *verbose {
				if i > 0 {
					fmt.Fprintln(hc.Stdout)
				}
				fmt.Fprintf(hc.Stdout, "==> %s <==\n", names[i])
			}
			var buf []string
			sc := newLineReader(r)
			for sc.Scan() {
				buf = append(buf, sc.Text())
				if int64(len(buf)) > negLines {
					fmt.Fprintln(hc.Stdout, buf[0])
					buf = buf[1:]
				}
			}
			if err := sc.Err(); err != nil {
				fmt.Fprintln(hc.Stderr, "head:", err)
				return exitError{1}
			}
		}
		return nil
	}
	if n == 0 {
		// GNU `head -n 0` prints nothing and exits 0 without
		// consuming input (avoids SIGPIPE/broken-pipe noise on
		// Windows named pipes when the writer keeps writing).
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
		sc := newLineReader(r)
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

// parseCountUint parses GNU counts (head/tail -n) without sign.
func parseCountUint(s string) (uint64, error) {
	return parseCount(s)
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
		} else if strings.HasPrefix(s, "-") {
			// GNU `tail -n -N` = last N lines.
			if v, err := parseUint(strings.TrimPrefix(s, "-")); err == nil {
				n = v
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
			sc := newLineReader(r)
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
		sc := newLineReader(r)
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
	stable := fs.Bool("s", false, "")
	zeroTerm := fs.Bool("z", false, "")
	fs.BoolVar(zeroTerm, "zero-terminated", false, "")
	sep := fs.String("t", "", "")
	key := fs.String("k", "", "")
	outFile := fs.String("o", "", "")
	versionSort := fs.Bool("V", false, "")
	fs.BoolVar(versionSort, "version-sort", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_ = stable // SliceStable below is already stable; flag accepted for parity.
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "sort:", err)
		return exitError{1}
	}
	defer closeAll()
	delim := byte('\n')
	if *zeroTerm {
		delim = 0
	}
	var lines []string
	for _, r := range readers {
		for rec := range splitRecords(r, delim) {
			lines = append(lines, rec)
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
		if *versionSort {
			c := compareVersion(a.line, b.line)
			if c != 0 {
				if *reverse {
					return c > 0
				}
				return c < 0
			}
			if a.line != b.line {
				if *reverse {
					return a.line > b.line
				}
				return a.line < b.line
			}
			return false
		}
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
	var out io.Writer = hc.Stdout
	if *outFile != "" {
		p := resolve(hc.Dir, *outFile)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "sort:", err)
			return exitError{1}
		}
		defer f.Close()
		out = f
	}
	prev := ""
	for i, it := range items {
		l := it.line
		if *unique && i > 0 && l == prev {
			continue
		}
		if *zeroTerm {
			out.Write([]byte(l))
			out.Write([]byte{0})
		} else {
			fmt.Fprintln(out, l)
		}
		prev = l
	}
	return nil
}

// compareVersion implements GNU sort -V (version sort): splits strings
// into alternating digit/non-digit runs; digit runs compare numerically
// (leading zeros ignored, longer-wins on tie), other runs compare
// byte-wise. Empty compares less than non-empty.
func compareVersion(a, b string) int {
	splitV := func(s string) []string {
		var parts []string
		i := 0
		for i < len(s) {
			j := i
			if s[j] >= '0' && s[j] <= '9' {
				for j < len(s) && s[j] >= '0' && s[j] <= '9' {
					j++
				}
			} else {
				for j < len(s) && (s[j] < '0' || s[j] > '9') {
					j++
				}
			}
			parts = append(parts, s[i:j])
			i = j
		}
		return parts
	}
	isNum := func(p string) bool { return len(p) > 0 && p[0] >= '0' && p[0] <= '9' }
	pa, pb := splitV(a), splitV(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, y := pa[i], pb[i]
		if isNum(x) && isNum(y) {
			sx, sy := strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			if len(sx) != len(sy) {
				if len(sx) < len(sy) {
					return -1
				}
				return 1
			}
			if sx != sy {
				if sx < sy {
					return -1
				}
				return 1
			}
			if len(x) != len(y) {
				// Numeric tie (e.g. 01 vs 1): fewer leading
				// zeros (shorter raw) sorts first.
				if len(x) < len(y) {
					return -1
				}
				return 1
			}
			continue
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(pa) < len(pb):
		return -1
	case len(pa) > len(pb):
		return 1
	}
	return 0
}

// splitRecords yields records split on delim (newline or NUL for -z),
// without the delimiter. A trailing delimiter does not create an extra
// empty record (matches GNU sort -z).
func splitRecords(r io.Reader, delim byte) <-chan string {
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		data, err := io.ReadAll(r)
		if err != nil {
			return
		}
		if len(data) == 0 {
			return
		}
		if data[len(data)-1] == delim {
			data = data[:len(data)-1]
		}
		for _, rec := range bytes.Split(data, []byte{delim}) {
			ch <- string(rec)
		}
	}()
	return ch
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
	numeric    bool
	reverse    bool
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

// skipNFields drops the first N blank-separated fields like GNU uniq -f.
func skipNFields(s string, n int) string {
	i := 0
	for k := 0; k < n; k++ {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			return ""
		}
		for i < len(s) && s[i] != ' ' && s[i] != '\t' {
			i++
		}
	}
	return s[i:]
}

func cmdUniq(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("uniq", hc.Stderr)
	count := fs.Bool("c", false, "")
	dupOnly := fs.Bool("d", false, "")
	uniqueOnly := fs.Bool("u", false, "")
	ignoreCase := fs.Bool("i", false, "")
	skipFields := fs.Uint64("f", 0, "")
	skipChars := fs.Uint64("s", 0, "")
	checkChars := fs.Uint64("w", 0, "")
	group := fs.String("group", "separate", "")
	fs.StringVar(group, "g", "separate", "")
	preArgs := make([]string, 0, len(args))
	for _, a := range args[1:] {
		if a == "--group" || a == "-g" {
			a = "--group=separate"
		}
		preArgs = append(preArgs, a)
	}
	if err := fs.Parse(preArgs); err != nil {
		return err
	}
	groupMode := ""
	for _, a := range args[1:] {
		if a == "--group" || a == "-g" {
			groupMode = "separate"
			break
		}
		if strings.HasPrefix(a, "--group=") {
			groupMode = strings.TrimPrefix(a, "--group=")
			break
		}
	}
	_ = group
	_ = groupMode
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
		// GNU comparison key: skip N fields, then N chars, then
		// optionally truncate to M chars (-w). Raw line is printed.
		k := s
		if *skipFields > 0 {
			k = skipNFields(k, int(*skipFields))
		}
		if *skipChars > 0 {
			runes := []rune(k)
			if len(runes) > int(*skipChars) {
				k = string(runes[*skipChars:])
			} else {
				k = ""
			}
		}
		if *checkChars > 0 {
			runes := []rune(k)
			if len(runes) > int(*checkChars) {
				k = string(runes[:*checkChars])
			}
		}
		if *ignoreCase {
			return strings.ToLower(k)
		}
		return k
	}
	var prev, prevRaw string
	n := 0
	firstGroup := true
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
		if groupMode != "" {
			// GNU --group: print EVERY line, groups separated by blank line.
			if !firstGroup {
				fmt.Fprintln(w)
			}
			firstGroup = false
			for i := 0; i < n; i++ {
				fmt.Fprintln(w, prevRaw)
			}
			return
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
		sc := newLineReader(r)
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
	defWc := !*lines && !*words && !*bytes_ && !*chars && !*maxLine
	if defWc {
		*lines, *words, *bytes_ = true, true, true
	}
	files := fs.Args()
	readers, names, closeAll, err := openInputs(hc.Dir, files, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "wc:", err)
		return exitError{1}
	}
	defer closeAll()
	// Regularity per input: file operands stat the file; stdin checks
	// whether the reader is a regular file (redirect) or pipe.
	isReg := make([]bool, len(readers))
	for k := range readers {
		if len(files) > 0 && names[k] != "" && names[k] != "standard input" && names[k] != "-" {
			if fi, err := os.Stat(resolve(hc.Dir, files[k])); err == nil {
				isReg[k] = fi.Mode().IsRegular()
			}
		} else {
			isReg[k] = stdinIsRegular(hc.Stdin)
		}
	}
	type wcRow struct {
		vals []int
		name string
	}
	var rows []wcRow
	tl, tw, tb, tc, tm := 0, 0, 0, 0, 0
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
		name := ""
		if len(files) > 0 && names[i] != "" && names[i] != "standard input" {
			name = names[i]
		} else if len(files) > 0 && names[i] == "standard input" {
			// Explicit "-" operand (alone or mixed with files) is
			// displayed as "-" like GNU.
			name = "-"
		}
		rows = append(rows, wcRow{colVals(l, w, b, chars, maxLineLen), name})
	}
	showTotal := len(readers) > 1
	if showTotal {
		rows = append(rows, wcRow{colVals(tl, tw, tb, tc, tm), "total"})
	}
	// GNU width rule (coreutils 9.4, verified): single column prints
	// bare; multiple columns pad to width 7 when ANY input is not a
	// regular file (pipe), else to the max digits needed (min 1).
	width := 1
	anyPipe := false
	for _, r := range rows {
		for _, v := range r.vals {
			if d := len(strconv.Itoa(v)); d > width {
				width = d
			}
		}
	}
	for _, v := range isReg {
		if !v {
			anyPipe = true
		}
	}
	if anyPipe {
		width = 7
	}
	for _, r := range rows {
		line := ""
		if len(r.vals) == 1 {
			line = strconv.Itoa(r.vals[0])
		} else {
			strs := make([]string, len(r.vals))
			for i, v := range r.vals {
				strs[i] = fmt.Sprintf("%*d", width, v)
			}
			line = strings.Join(strs, " ")
		}
		if r.name != "" {
			line += " " + r.name
		}
		fmt.Fprintln(hc.Stdout, line)
	}
	return nil
}

// stdinIsRegular reports whether the wc input reader is a regular file
// (stdin redirected from a file) as opposed to a pipe. GNU wc pads
// columns to width 7 for pipes but uses natural width for regular files.
func stdinIsRegular(r io.Reader) bool {
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err == nil {
			return fi.Mode().IsRegular()
		}
	}
	return false
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
	truncate := fs.Bool("t", false, "")
	fs.BoolVar(truncate, "truncate-set1", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_ = truncate // accepted for parity; mapping loop below already truncates SET1 to len(SET2)
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
	if *truncate {
		// GNU -t: truncate SET1 to length of SET2 (extra SET1 chars
		// map to themselves instead of to the last SET2 char).
		if len(from) > len(to) {
			from = from[:len(to)]
		}
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
			if c := s[i+1]; c >= '0' && c <= '7' {
				k, val := i+1, 0
				for k < len(s) && k < i+4 && s[k] >= '0' && s[k] <= '7' {
					val = val*8 + int(s[k]-'0')
					k++
				}
				out = append(out, byte(val&0xFF))
				i = k
				continue
			}
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
		if s[i] == '[' && strings.HasPrefix(s[i:], "[:") {
			if e := strings.Index(s[i:], ":]"); e >= 0 {
				out = append(out, expandTrClass(s[i:i+e+2])...)
				i += e + 2
				continue
			}
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

// expandTrClass expands POSIX classes like [:upper:] for tr.
func expandTrClass(class string) []byte {
	var out []byte
	add := func(lo, hi byte) {
		for c := lo; c <= hi; c++ {
			out = append(out, c)
		}
	}
	switch class {
	case "[:upper:]":
		add('A', 'Z')
	case "[:lower:]":
		add('a', 'z')
	case "[:digit:]":
		add('0', '9')
	case "[:alpha:]":
		add('A', 'Z')
		add('a', 'z')
	case "[:alnum:]":
		add('A', 'Z')
		add('a', 'z')
		add('0', '9')
	case "[:space:]":
		out = append(out, ' ', '\t', '\n', '\r', '\v', '\f')
	case "[:blank:]":
		out = append(out, ' ', '\t')
	case "[:xdigit:]":
		add('0', '9')
		add('A', 'F')
		add('a', 'f')
	default:
		out = append(out, class...)
	}
	return out
}

func cmdSeq(_ context.Context, hc interp.HandlerContext, args []string) error {
	// GNU seq flags: -w (equal width), -s SEP (separator), -f FMT.
	var padWidth bool
	sep := "\n"
	format := ""
	var rest []string
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-w" || a == "--equal-width":
			padWidth = true
		case a == "-s" || a == "--separator":
			if i+1 >= len(argv) {
				fmt.Fprintln(hc.Stderr, "seq: option requires an argument -- 's'")
				return exitError{1}
			}
			i++
			sep = argv[i]
		case strings.HasPrefix(a, "-s") && len(a) > 2:
			sep = a[2:]
		case a == "-f" || a == "--format":
			if i+1 >= len(argv) {
				fmt.Fprintln(hc.Stderr, "seq: option requires an argument -- 'f'")
				return exitError{1}
			}
			i++
			format = argv[i]
		case strings.HasPrefix(a, "-f") && len(a) > 2:
			format = a[2:]
		case a == "--":
			rest = append(rest, argv[i+1:]...)
			i = len(argv)
		case strings.HasPrefix(a, "-") && len(a) > 1 && a != "-":
			// Negative numbers are operands, not options.
			if _, _, err := parseSeqNum(a); err == nil {
				rest = append(rest, a)
				break
			}
			fmt.Fprintf(hc.Stderr, "seq: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
			return exitError{1}
		default:
			rest = append(rest, a)
		}
	}
	var first, step, last float64 = 1, 1, 0
	decimals := 0
	switch len(rest) {
	case 1:
		v, dec, err := parseSeqNum(rest[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, "seq: invalid number:", rest[0])
			return exitError{1}
		}
		last = v
		decimals = dec
	case 2:
		f, df, err1 := parseSeqNum(rest[0])
		l, dl, err2 := parseSeqNum(rest[1])
		if err1 != nil || err2 != nil {
			fmt.Fprintln(hc.Stderr, "seq: invalid number")
			return exitError{1}
		}
		first, last = f, l
		decimals = max(df, dl)
	case 3:
		f, df, err1 := parseSeqNum(rest[0])
		s, ds, err2 := parseSeqNum(rest[1])
		l, dl, err3 := parseSeqNum(rest[2])
		if err1 != nil || err2 != nil || err3 != nil {
			fmt.Fprintln(hc.Stderr, "seq: invalid number")
			return exitError{1}
		}
		first, step, last = f, s, l
		decimals = max(df, max(ds, dl))
	default:
		fmt.Fprintln(hc.Stderr, "seq: usage: seq [first [step]] last")
		return flag.ErrHelp
	}
	if step == 0 {
		fmt.Fprintf(hc.Stderr, "seq: invalid Zero increment value: \u2018%s\u2019\n", rest[len(rest)-2])
		fmt.Fprintln(hc.Stderr, "Try 'seq --help' for more information.")
		return exitError{1}
	}
	var sb strings.Builder
	sb.Grow(4096)
	fmtNum := func(v float64) string {
		if format != "" {
			return sprintfSeq(format, v)
		}
		if decimals > 0 {
			return strconv.FormatFloat(v, 'f', decimals, 64)
		}
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	var vals []string
	for v := first; (step > 0 && v <= last+1e-12) || (step < 0 && v >= last-1e-12); v += step {
		vals = append(vals, fmtNum(v))
		if len(vals) > 10000000 {
			break
		}
	}
	if padWidth {
		w := 0
		for _, s := range vals {
			if len(s) > w {
				w = len(s)
			}
		}
		for i, s := range vals {
			if len(s) < w {
				vals[i] = strings.Repeat("0", w-len(s)) + s
			}
		}
	}
	if sep == "\n" {
		for _, s := range vals {
			sb.WriteString(s)
			sb.WriteByte('\n')
		}
	} else {
		sb.WriteString(strings.Join(vals, sep))
		sb.WriteByte('\n')
	}
	_, err := io.WriteString(hc.Stdout, sb.String())
	return err
}

// parseSeqNum parses a seq operand, returning value + decimals shown.
func parseSeqNum(s string) (float64, int, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, 0, err
	}
	dec := 0
	if i := strings.IndexByte(s, '.'); i >= 0 {
		dec = len(s) - i - 1
		for _, c := range s[i+1:] {
			if c < '0' || c > '9' {
				dec = 0
				break
			}
		}
	}
	return v, dec, nil
}

// sprintfSeq formats v with a %g/%f/%e-style format (GNU seq -f).
func sprintfSeq(format string, v float64) string {
	if strings.Contains(format, "%") {
		f := format
		// support %g, %f, %e with optional width/precision/0-pad
		var verb byte
		for k := len(f) - 1; k >= 0; k-- {
			if f[k] == '%' {
				break
			}
			if f[k] == 'g' || f[k] == 'f' || f[k] == 'e' || f[k] == 'E' {
				verb = f[k]
			}
		}
		_ = verb
		out := fmt.Sprintf(f, v)
		// fmt handles %03g etc. natively for floats
		return out
	}
	return format
}

func cmdCut(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("cut", hc.Stderr)
	delim := fs.String("d", "\t", "")
	fields := fs.String("f", "", "")
	chars := fs.String("c", "", "")
	bytes_ := fs.String("b", "", "")
	onlyDelim := fs.Bool("s", false, "")
	complement := fs.Bool("complement", false, "")
	outDelim := fs.String("output-delimiter", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// -b (bytes) behaves like -c for UTF-8-unaware output; GNU differs
	// only on multibyte chars, where byte slicing can split runes.
	if *bytes_ != "" && *chars == "" {
		*chars = *bytes_
	}
	if *chars != "" {
		return cmdCutChars(hc, *chars, fs.Args(), *complement)
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
		sel := false
		for _, s := range spans {
			if idx >= s.lo && (s.hi == -1 || idx <= s.hi) {
				sel = true
				break
			}
		}
		if *complement {
			return !sel
		}
		return sel
	}
	joiner := d
	if *outDelim != "" {
		joiner = *outDelim
	}
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cut:", err)
		return exitError{1}
	}
	defer closeAll()
	for _, r := range readers {
		sc := newLineReader(r)
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
			fmt.Fprintln(hc.Stdout, strings.Join(out, joiner))
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

func cmdCutChars(hc interp.HandlerContext, list string, files []string, complement bool) error {
	spans, err := parseFieldList(list)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cut:", err)
		return exitError{1}
	}
	in := func(idx int) bool {
		sel := false
		for _, s := range spans {
			if idx >= s.lo && (s.hi == -1 || idx <= s.hi) {
				sel = true
				break
			}
		}
		if complement {
			return !sel
		}
		return sel
	}
	readers, _, closeAll, err := openInputs(hc.Dir, files, hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cut:", err)
		return exitError{1}
	}
	defer closeAll()
	// NOTE: byte-oriented (C-locale semantics). GNU cut -c is
	// character-oriented only under a resolvable UTF-8 locale; the
	// reference environment resolves to C (bytes). Tracked for future
	// locale-dependent multibyte support.
	for _, r := range readers {
		sc := newLineReader(r)
		for sc.Scan() {
			line := sc.Text()
			var out []byte
			for i := 0; i < len(line); i++ {
				if in(i + 1) {
					out = append(out, line[i])
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
	// GNU: repeated "-" operands read SUCCESSIVE lines from the same
	// stdin stream. Buffer stdin once; each "-" scanner consumes the
	// next line round-robin (shared cursor), not an independent copy.
	var stdinLines []string
	stdinBuffered := false
	var openFiles []io.Closer
	defer func() {
		for _, f := range openFiles {
			f.Close()
		}
	}()
	type src2 struct {
		sc    *lineReader
		lines *[]string
		pos   *int
	}
	var srcs []src2
	var shared []string
	var sharedPos int
	for _, f := range files {
		if f == "-" || f == "/dev/stdin" {
			if !stdinBuffered {
				data, err := io.ReadAll(hc.Stdin)
				if err != nil {
					fmt.Fprintln(hc.Stderr, "paste:", err)
					return exitError{1}
				}
				stdinLines = splitLinesDropLast(string(data))
				shared = stdinLines
				stdinBuffered = true
			}
			srcs = append(srcs, src2{lines: &shared, pos: &sharedPos})
			continue
		}
		fh, err := openShellFile(resolve(hc.Dir, f))
		if err != nil {
			fmt.Fprintln(hc.Stderr, "paste:", err)
			return exitError{1}
		}
		openFiles = append(openFiles, fh)
		sc := newLineReader(fh)
		srcs = append(srcs, src2{sc: sc})
	}
	nextLine := func(s src2) (string, bool) {
		if s.sc != nil {
			if s.sc.Scan() {
				return s.sc.Text(), true
			}
			return "", false
		}
		if *s.pos < len(*s.lines) {
			l := (*s.lines)[*s.pos]
			*s.pos++
			return l, true
		}
		return "", false
	}
	if *serial {
		for i, s := range srcs {
			var parts []string
			for {
				l, ok := nextLine(s)
				if !ok {
					break
				}
				parts = append(parts, l)
			}
			fmt.Fprintln(hc.Stdout, strings.Join(parts, string(d[i%len(d)])))
		}
		return nil
	}
	for {
		var parts []string
		any := false
		for _, s := range srcs {
			if l, ok := nextLine(s); ok {
				parts = append(parts, l)
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
	fs.Bool("check-order", false, "")
	noCheck := fs.Bool("nocheck-order", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_ = noCheck
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
		sc := newLineReader(r)
		for sc.Scan() {
			out = append(out, sc.Text())
		}
		return out
	}
	a, b := readAll(readers[0]), readAll(readers[1])
	// GNU comm verifies sort order (unless --nocheck-order): warn on
	// stderr and exit 1 on disorder, while still printing the table.
	commCode := 0
	skipCheck := *noCheck
	for k := 1; !skipCheck && k < len(a); k++ {
		if a[k] < a[k-1] {
			fmt.Fprintf(hc.Stderr, "comm: file 1 is not in sorted order\n")
			fmt.Fprintln(hc.Stderr, "comm: input is not in sorted order")
			commCode = 1
			break
		}
	}
	for k := 1; !skipCheck && k < len(b); k++ {
		if b[k] < b[k-1] {
			fmt.Fprintf(hc.Stderr, "comm: file 2 is not in sorted order\n")
			fmt.Fprintln(hc.Stderr, "comm: input is not in sorted order")
			commCode = 1
			break
		}
	}
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
	if commCode != 0 {
		return exitError{commCode}
	}
	return nil
}

func cmdSplit(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("split", hc.Stderr)
	lines := fs.Uint64("l", 1000, "")
	sufLen := fs.Uint64("a", 2, "")
	fs.Uint64Var(sufLen, "suffix-length", 2, "")
	numeric := fs.Bool("d", false, "")
	verbose := fs.Bool("v", false, "")
	_ = verbose // -v/--verbose: GNU prints chunk names; accepted (names still created).
	number := fs.String("n", "", "")
	fs.StringVar(number, "number", "", "")
	byteStr := fs.String("b", "", "")
	fs.StringVar(byteStr, "bytes", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *number != "" {
		return splitByNumber(hc, *number, fs.Args())
	}
	var byteCount int64 = -1
	if *byteStr != "" {
		n, err := parseSize(*byteStr)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "split:", err)
			return exitError{1}
		}
		byteCount = n
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
	var outBytes int64
	closeOut := func() {
		if out != nil {
			out.Close()
			out = nil
		}
	}
	defer closeOut()
	newPart := func() error {
		closeOut()
		p := resolve(hc.Dir, prefix+suffix(part))
		f, e := os.Create(p)
		if e != nil {
			fmt.Fprintln(hc.Stderr, "split:", e)
			return e
		}
		out = f
		part++
		count = 0
		outBytes = 0
		return nil
	}
	if byteCount >= 0 {
		for _, r := range readers {
			data, err := io.ReadAll(r)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "split:", err)
				return exitError{1}
			}
			for len(data) > 0 {
				if out == nil || outBytes >= byteCount {
					if err := newPart(); err != nil {
						return exitError{1}
					}
				}
				room := byteCount - outBytes
				take := int64(len(data))
				if take > room {
					take = room
				}
				if _, err := out.Write(data[:take]); err != nil {
					fmt.Fprintln(hc.Stderr, "split:", err)
					return exitError{1}
				}
				outBytes += take
				data = data[take:]
			}
		}
		return nil
	}
	for _, r := range readers {
		sc := newLineReader(r)
		for sc.Scan() {
			if out == nil || count >= *lines {
				if err := newPart(); err != nil {
					return exitError{1}
				}
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

// parseSize parses GNU size specs like 4, 1K, 2M (split -b).
func parseSize(s string) (int64, error) {
	mult := int64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'K', 'k':
			mult = 1024
			s = s[:len(s)-1]
		case 'M', 'm':
			mult = 1024 * 1024
			s = s[:len(s)-1]
		case 'G', 'g':
			mult = 1024 * 1024 * 1024
			s = s[:len(s)-1]
		case 'c':
			s = s[:len(s)-1]
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size: %s", s)
	}
	return n * mult, nil
}

func cmdDiff(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("diff", hc.Stderr)
	brief := fs.Bool("q", false, "")
	unified := fs.Bool("u", false, "")
	reportSame := fs.Bool("s", false, "")
	var labels stringList
	fs.Var(&labels, "label", "")
	fs.Var(&labels, "L", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) != 2 {
		fmt.Fprintln(hc.Stderr, "diff: need 2 files")
		return flag.ErrHelp
	}
	// GNU: repeated stdin operands (`- -`, `/dev/stdin /dev/stdin`)
	// read SUCCESSIVE chunks — but a pipe delivers one stream, so the
	// first read consumes everything and later ones see EOF (empty).
	// Buffer stdin once and hand out each read a successive LINE-chunk
	// would be wrong for diff (byte-exact compare); instead buffer once
	// and give every stdin operand the SAME full content (matches the
	// observable GNU result for identical-input compares).
	var stdinData []byte
	stdinBuffered := false
	readOne := func(name string) ([]byte, error) {
		if name == "-" || name == "/dev/stdin" {
			if !stdinBuffered {
				var err error
				stdinData, err = io.ReadAll(hc.Stdin)
				if err != nil {
					return nil, err
				}
				stdinBuffered = true
			}
			return stdinData, nil
		}
		return readShellFile(resolve(hc.Dir, name))
	}
	ra, err := readOne(files[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, "diff:", err)
		return exitError{2}
	}
	rb, err := readOne(files[1])
	if err != nil {
		fmt.Fprintln(hc.Stderr, "diff:", err)
		return exitError{2}
	}
	// GNU diff reports binary files as "Binary files X and Y differ".
	if bytes.IndexByte(ra, 0) >= 0 || bytes.IndexByte(rb, 0) >= 0 {
		if !bytes.Equal(ra, rb) {
			fmt.Fprintf(hc.Stdout, "Binary files %s and %s differ\n", files[0], files[1])
			return exitError{1}
		}
		return nil
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
		emitUnified(hc, files, a, b, ops, labels)
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

func emitUnified(hc interp.HandlerContext, files []string, a, b []string, ops []diffOp, labels []string) {
	_ = a
	_ = b
	la0, lb0 := files[0], files[1]
	if len(labels) > 0 {
		la0 = labels[0]
	}
	if len(labels) > 1 {
		lb0 = labels[1]
	}
	if len(labels) >= 2 {
		// GNU: explicit --label pins the header with NO timestamp.
		fmt.Fprintf(hc.Stdout, "--- %s\n", la0)
		fmt.Fprintf(hc.Stdout, "+++ %s\n", lb0)
	} else {
		fmt.Fprintf(hc.Stdout, "--- %s\t%s\n", la0, fileTime(files[0]))
		fmt.Fprintf(hc.Stdout, "+++ %s\t%s\n", lb0, fileTime(files[1]))
	}
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
	fa, err := openShellFile(resolve(hc.Dir, files[0]))
	if err != nil {
		fmt.Fprintln(hc.Stderr, "cmp:", err)
		return exitError{2}
	}
	defer fa.Close()
	fb, err := openShellFile(resolve(hc.Dir, files[1]))
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
	verbose := fs.Bool("v", false, "")
	limit := fs.Uint64("n", 0, "")
	skip := fs.Uint64("s", 0, "")
	format := fs.String("e", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	_ = verbose // -v: no line suppression in our output anyway; accept for parity.
	_ = format // -e custom formats: accepted; raw format string echoed? no — GNU applies it; we apply simple %02x passthrough below
	readers, _, closeAll, err := openInputs(hc.Dir, fs.Args(), hc.Stdin)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "hexdump:", err)
		return exitError{1}
	}
	defer closeAll()
	var off uint64
	hexCode := 0
	// GNU: -s on a non-seekable stdin CANNOT skip: warns and exits 1
	// (even for small skips); on regular files it seeks and adjusts
	// the displayed offsets.
	stdinIsFile := true
	if *skip > 0 && len(fs.Args()) == 0 {
		if f, ok := hc.Stdin.(*os.File); ok {
			if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
				stdinIsFile = true
			} else {
				stdinIsFile = false
			}
		} else {
			stdinIsFile = false
		}
		if !stdinIsFile {
			fmt.Fprintln(hc.Stderr, "hexdump: stdin: Illegal seek")
			return exitError{1}
		}
	}
	for _, r := range readers {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		if *skip > 0 {
			if uint64(len(data)) <= *skip {
				fmt.Fprintln(hc.Stderr, "hexdump: stdin: Illegal seek")
				hexCode = 1
				continue
			}
			data = data[*skip:]
			off += *skip
		}
		if *limit > 0 && uint64(len(data)) > *limit {
			data = data[:*limit]
		}
		if *format != "" {
			// GNU -e 'FMT': minimal support for the common
			// '1/1 "%02x "' byte-dump idiom used in probes.
			// No trailing newline, no final offset (matches GNU).
			for i := 0; i < len(data); i++ {
				fmt.Fprintf(hc.Stdout, "%02x ", data[i])
			}
			off += uint64(len(data))
			continue
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
	// GNU hexdump prints no final offset for empty input — nor any
	// offset at all when -e custom formats are used.
	if off > 0 && *format == "" {
		if *canonical {
			fmt.Fprintf(hc.Stdout, "%08x\n", off)
		} else {
			fmt.Fprintf(hc.Stdout, "%07o\n", off)
		}
	}
	if hexCode != 0 {
		return exitError{hexCode}
	}
	return nil
}

func cmdStrings(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("strings", hc.Stderr)
	minLen := fs.Uint64("n", 4, "")
	offsetFmt := fs.String("t", "", "")
	fs.StringVar(offsetFmt, "radix", "", "")
	offO := fs.Bool("o", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *offO && *offsetFmt == "" {
		*offsetFmt = "o"
	}
	// GNU strings -t R: prefix each string with its byte offset in
	// radix R (d decimal, o octal, x hex). -o is an alias for -t o.
	showOff := ""
	if *offsetFmt != "" {
		showOff = *offsetFmt
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
		curOff := 0
		flush := func() {
			if uint64(len(cur)) >= *minLen {
				if showOff != "" {
					fmt.Fprintf(hc.Stdout, "%s %s\n", formatOffset(curOff, showOff), string(cur))
				} else {
					fmt.Fprintln(hc.Stdout, string(cur))
				}
			}
			cur = nil
		}
		for k, b := range data {
			if b >= 32 && b < 127 || b == '\t' {
				if cur == nil {
					curOff = k
				}
				cur = append(cur, b)
			} else {
				flush()
			}
		}
		flush()
	}
	return nil
}

func splitLinesDropLast(s string) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func eolIf(lines []string) string {
	if len(lines) > 0 {
		return "\n"
	}
	return ""
}

// reMatchAny reports whether re matches any line of data (honoring invert).
func reMatchAny(re *regexp.Regexp, invert bool, data []byte) bool {
	matched := false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if re.Match(line) {
			matched = true
			break
		}
	}
	if invert {
		return !matched
	}
	return matched
}

// formatOffset formats a strings(1) byte offset in radix d/o/x.
func formatOffset(off int, radix string) string {
	switch radix {
	case "o":
		return fmt.Sprintf("%7o", off)
	case "x":
		return fmt.Sprintf("%7x", off)
	default:
		return fmt.Sprintf("%7d", off)
	}
}

func splitByNumber(hc interp.HandlerContext, spec string, rest []string) error {
	// GNU split -n CHUNKS: N | l/N | r/N (+ K/N variants to stdout).
	// We implement N, l/N, r/N (write N files, round-robin for r/).
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
	mode := "size"
	nstr := spec
	if strings.HasPrefix(spec, "l/") {
		mode = "lines"
		nstr = strings.TrimPrefix(spec, "l/")
	} else if strings.HasPrefix(spec, "r/") {
		mode = "roundrobin"
		nstr = strings.TrimPrefix(spec, "r/")
	}
	var n int
	if _, err := fmt.Sscanf(nstr, "%d", &n); err != nil || n <= 0 {
		fmt.Fprintf(hc.Stderr, "split: invalid number of chunks: %q\n", spec)
		return exitError{1}
	}
	var allLines []string
	for _, r := range readers {
		sc := newLineReader(r)
		for sc.Scan() {
			allLines = append(allLines, sc.Text())
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "split:", err)
			return exitError{1}
		}
	}
	if n > len(allLines) && len(allLines) > 0 {
		n = len(allLines)
	}
	if n == 0 {
		n = 1
	}
	outs := make([][]string, n)
	switch mode {
	case "roundrobin":
		for i, l := range allLines {
			outs[i%n] = append(outs[i%n], l)
		}
	case "lines":
		per := (len(allLines) + n - 1) / n
		for i, l := range allLines {
			k := i / per
			if k >= n {
				k = n - 1
			}
			outs[k] = append(outs[k], l)
		}
	default:
		// By size (bytes): approximate by lines.
		per := (len(allLines) + n - 1) / n
		for i, l := range allLines {
			k := i / per
			if k >= n {
				k = n - 1
			}
			outs[k] = append(outs[k], l)
		}
	}
	for i, chunk := range outs {
		p := resolve(hc.Dir, prefix+splitSuffixDefault(i))
		f, err := openFileRetry(p, 577, 0644)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "split:", err)
			return exitError{1}
		}
		for _, l := range chunk {
			fmt.Fprintln(f, l)
		}
		f.Close()
	}
	return nil
}

func splitSuffixDefault(n int) string {
	a := 'a' + byte(n/26%26)
	b := 'a' + byte(n%26)
	return string([]byte{a, b})
}
