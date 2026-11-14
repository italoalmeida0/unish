package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"mvdan.cc/sh/v3/interp"
)

var nlBytes = []byte{'\n'}

var bufPool64K = sync.Pool{
	New: func() any {
		b := make([]byte, 64*1024+8)
		return &b
	},
}

var bufioWriterPool64K = sync.Pool{
	New: func() any {
		return bufio.NewWriterSize(io.Discard, 64*1024)
	},
}

func getBufWriter64K(w io.Writer) *bufio.Writer {
	bw := bufioWriterPool64K.Get().(*bufio.Writer)
	bw.Reset(w)
	return bw
}

func putBufWriter64K(bw *bufio.Writer) {
	bw.Reset(io.Discard)
	bufioWriterPool64K.Put(bw)
}

func cmdGrep(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var (
		count, filesOnly, fixed, ignoreCase, invert, lineNum,
		noName, quiet, recursive, extended, onlyMatch bool
		lineRegexp, wordRegexp, withName, noMessages, textMode bool
		label                                                  string
		patterns                                               []string
		patternFiles                                           []string
		maxCount, after, before, context_                      uint64
		excludes, includes, excludeDirs                        stringList
		positionals                                            []string
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
				textMode = true
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
				textMode = true
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
					case 'a':
						textMode = true
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
	// Fast path: a single literal pattern over plain line output can use
	// SIMD byte primitives (bytes.Index) instead of compiling/running regexp.
	lit, litOK := "", false
	if len(patterns) == 1 && !ignoreCase && !lineRegexp && !wordRegexp &&
		after == 0 && before == 0 && maxCount == 0 && !onlyMatch && utf8.ValidString(patterns[0]) {
		if fixed {
			if !strings.ContainsRune(patterns[0], '\n') {
				lit, litOK = patterns[0], true
			}
		} else if p, ok := grepLiteralPattern(patterns[0]); ok {
			lit, litOK = p, true
		}
	}

	var re *regexp.Regexp
	if !litOK {
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
		var err error
		re, err = regexp.Compile(pat)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "grep:", err)
			return exitError{2}
		}
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
	grepStop := false
	var pipeStopErr error
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
		if name == "" || name == "-" || name == "/dev/stdin" || name == "standard input" {
			disp = "(standard input)"
		}
		if label != "" && (name == "" || name == "standard input" || name == "-") {
			disp = label
			showName = true
		}
		// Probe the first chunk for NUL bytes instead of slurping the whole
		// stream with io.ReadAll (which hangs/OOMs on infinite pipes like `yes | grep`).
		recordErr := func(gerr error) {
			if gerr == nil {
				return
			}
			if isPipeClosed(gerr) || errors.Is(gerr, epipeErr{}) {
				pipeStopErr = io.ErrClosedPipe
				grepStop = true
			} else if ctx.Err() != nil {
				grepStop = true
			}
		}
		if !quiet && !count && !filesOnly && !textMode {
			pp := bufPool64K.Get().(*[]byte)
			defer bufPool64K.Put(pp)
			probe := (*pp)[:32*1024]
			n, rerr := r.Read(probe)
			if n == 0 && rerr == io.EOF {
				return
			}
			var fullR io.Reader
			if rerr == io.EOF {
				fullR = bytes.NewReader(probe[:n])
			} else if n > 0 {
				fullR = io.MultiReader(bytes.NewReader(probe[:n]), r)
			} else {
				fullR = r
			}
			if bytes.IndexByte(probe[:n], 0) >= 0 {
				binMatched := false
				var litBytes []byte
				if litOK {
					litBytes = []byte(lit)
				}
				_ = streamLinesCtx(ctx, fullR, func(line []byte, _ bool) bool {
					var ok bool
					if litOK {
						ok = bytes.Contains(line, litBytes)
					} else {
						ok = re.Match(line)
					}
					if invert {
						ok = !ok
					}
					if ok {
						binMatched = true
						return false
					}
					return true
				})
				if binMatched {
					fmt.Fprintf(hc.Stderr, "grep: %s: binary file matches\n", disp)
					matchedAny = true
				}
				return
			}
			r = fullR
		}
		// Fast path: literal pattern over plain line output.
		if litOK {
			m, gerr := grepFastBuf(ctx, hc, disp, showName, lit, invert, lineNum, count, filesOnly, quiet, !textMode && !quiet && !count && !filesOnly, r)
			matchedAny = matchedAny || m
			if quiet && matchedAny {
				grepStop = true
			}
			recordErr(gerr)
			return
		}
		m, gerr := grepReaderWord(ctx, hc, disp, showName, re, wordRegexp, invert, lineNum, count, filesOnly, quiet, onlyMatch, !textMode && !quiet && !count && !filesOnly, int(maxCount), int(after), int(before), r)
		matchedAny = matchedAny || m
		if quiet && matchedAny {
			grepStop = true
		}
		recordErr(gerr)
	}
	if len(files) == 0 {
		if !recursive && len(rest) == 0 {
			emit("", hc.Stdin)
		}
	} else {
		for _, f := range files {
			if grepStop {
				break
			}
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
	if pipeStopErr != nil {
		return pipeStopErr
	}
	if err := ctx.Err(); err != nil {
		return err
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

// grepLiteralPattern reports whether a BRE/ERE pattern is a plain
// literal string (no metacharacters or newlines at all).
func grepLiteralPattern(pat string) (string, bool) {
	if pat == "" || strings.ContainsAny(pat, "\\.^$*+?()[]{}|\n") {
		return "", false
	}
	return pat, true
}

// streamLines feeds lines from r to fn in chunks with constant memory
// (GNU tools never slurp their input). Lines are newline-terminated
// except possibly the last one (terminated=false); a trailing newline
// does not produce an extra empty line. Returning false from fn stops
// the scan.
func streamLines(r io.Reader, fn func(line []byte, terminated bool) bool) error {
	return streamLinesChunked(context.Background(), r, fn, nil)
}

func streamLinesCtx(ctx context.Context, r io.Reader, fn func(line []byte, terminated bool) bool) error {
	return streamLinesChunked(ctx, r, fn, nil)
}

func streamLinesChunked(ctx context.Context, r io.Reader, fn func(line []byte, terminated bool) bool, afterChunk func() error) error {
	bp := bufPool64K.Get().(*[]byte)
	buf := (*bp)[:64*1024]
	defer bufPool64K.Put(bp)

	var carry []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.Read(buf)
		var data []byte
		hadCarry := len(carry) > 0
		if hadCarry {
			if n > 0 {
				carry = append(carry, buf[:n]...)
			}
			data = carry
		} else {
			data = buf[:n]
		}
		if len(data) == 0 {
			if err != nil && err != io.EOF {
				return err
			}
			return nil
		}
		var tail []byte
		if data[len(data)-1] != '\n' {
			if j := bytes.LastIndexByte(data, '\n'); j >= 0 {
				tail = data[j+1:]
				data = data[:j+1]
			} else {
				tail = data
				data = nil
			}
		}
		start := 0
		for start < len(data) {
			end := bytes.IndexByte(data[start:], '\n')
			if end < 0 {
				break
			}
			if !fn(data[start:start+end], true) {
				if afterChunk != nil {
					if cerr := afterChunk(); cerr != nil {
						return cerr
					}
				}
				return nil
			}
			start += end + 1
		}
		if len(tail) > 0 {
			if hadCarry {
				if len(tail) != len(carry) || &tail[0] != &carry[0] {
					nt := copy(carry[:cap(carry)], tail)
					carry = carry[:nt]
				}
			} else {
				carry = append(carry[:0], tail...)
			}
		} else {
			carry = carry[:0]
		}
		if afterChunk != nil && len(data) > 0 {
			if cerr := afterChunk(); cerr != nil {
				return cerr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	// Last line without a trailing newline.
	if len(carry) > 0 {
		fn(carry, false)
		if afterChunk != nil {
			if cerr := afterChunk(); cerr != nil {
				return cerr
			}
		}
	}
	return nil
}

// grepFastBuf scans a literal pattern with SIMD byte primitives (bytes.Index),
// streaming in 64 KB chunks (constant memory, zero per-line allocations).
// It covers plain output plus -c/-l/-n/-v/-q/-H.
func grepFastBuf(ctx context.Context, hc interp.HandlerContext, name string, showName bool, lit string,
	invert, lineNum, count, filesOnly, quiet, checkBin bool, r io.Reader) (bool, error) {
	prefix := ""
	if showName && name != "" {
		prefix = name + ":"
	}
	litB := []byte(lit)

	// Whole-chunk SIMD fast path when searching for a non-empty literal
	// without -v or -n: bytes.Index jumps directly between matches across
	// entire 64 KB chunks without splitting non-matching lines.
	if !invert && !lineNum && len(litB) > 0 && bytes.IndexByte(litB, '\n') < 0 {
		bp := bufPool64K.Get().(*[]byte)
		buf := (*bp)[:64*1024]
		defer bufPool64K.Put(bp)

		var bw *bufio.Writer
		if !quiet && !count && !filesOnly {
			bw = getBufWriter64K(hc.Stdout)
			defer func() {
				_ = bw.Flush()
				putBufWriter64K(bw)
			}()
		}

		matched, matchedAny := 0, false
		var carry []byte
		for {
			if err := ctx.Err(); err != nil {
				return matchedAny, err
			}
			n, rerr := r.Read(buf)
			var data []byte
			hadCarry := len(carry) > 0
			if hadCarry {
				if n > 0 {
					carry = append(carry, buf[:n]...)
				}
				data = carry
			} else {
				data = buf[:n]
			}
			if len(data) == 0 {
				if rerr != nil && rerr != io.EOF {
					return matchedAny, rerr
				}
				break
			}
			var tail []byte
			if data[len(data)-1] != '\n' {
				if j := bytes.LastIndexByte(data, '\n'); j >= 0 {
					tail = data[j+1:]
					data = data[:j+1]
				} else {
					tail = data
					data = nil
				}
			}
			if len(data) > 0 {
				if quiet || filesOnly {
					if bytes.Index(data, litB) >= 0 {
						if filesOnly && !quiet {
							if _, werr := fmt.Fprintln(hc.Stdout, name); werr != nil {
								if isPipeClosed(werr) {
									return true, epipeErr{}
								}
								return true, werr
							}
						}
						return true, nil
					}
				} else if count {
					pos := 0
					for pos < len(data) {
						idx := bytes.Index(data[pos:], litB)
						if idx < 0 {
							break
						}
						matchedAny = true
						matched++
						m := pos + idx + len(litB)
						nl := bytes.IndexByte(data[m:], '\n')
						if nl < 0 {
							break
						}
						pos = m + nl + 1
					}
				} else if checkBin && bytes.IndexByte(data, 0) >= 0 {
					start := 0
					for start < len(data) {
						end := bytes.IndexByte(data[start:], '\n')
						if end < 0 {
							break
						}
						line := data[start : start+end]
						if bytes.Index(line, litB) >= 0 {
							matchedAny = true
							if bytes.IndexByte(line, 0) >= 0 {
								if bw.Buffered() > 0 {
									if werr := bw.Flush(); werr != nil {
										return matchedAny, werr
									}
								}
								fmt.Fprintf(hc.Stderr, "grep: %s: binary file matches\n", name)
								return matchedAny, nil
							}
							if prefix != "" {
								bw.WriteString(prefix)
							}
							if _, werr := bw.Write(data[start : start+end+1]); werr != nil {
								return matchedAny, werr
							}
						}
						start += end + 1
					}
					if bw.Buffered() > 0 {
						if werr := bw.Flush(); werr != nil {
							return matchedAny, werr
						}
					}
				} else {
					pos := 0
					for pos < len(data) {
						idx := bytes.Index(data[pos:], litB)
						if idx < 0 {
							break
						}
						matchedAny = true
						matchPos := pos + idx
						lineStart := pos
						if prevNL := bytes.LastIndexByte(data[pos:matchPos], '\n'); prevNL >= 0 {
							lineStart = pos + prevNL + 1
						}
						m := matchPos + len(litB)
						nl := bytes.IndexByte(data[m:], '\n')
						lineEnd := len(data)
						if nl >= 0 {
							lineEnd = m + nl + 1
						}
						if prefix != "" {
							bw.WriteString(prefix)
						}
						if _, werr := bw.Write(data[lineStart:lineEnd]); werr != nil {
							return matchedAny, werr
						}
						pos = lineEnd
					}
					if bw.Buffered() > 0 {
						if werr := bw.Flush(); werr != nil {
							return matchedAny, werr
						}
					}
				}
			}
			if len(tail) > 0 {
				if hadCarry {
					if len(tail) != len(carry) || &tail[0] != &carry[0] {
						nt := copy(carry[:cap(carry)], tail)
						carry = carry[:nt]
					}
				} else {
					carry = append(carry[:0], tail...)
				}
			} else {
				carry = carry[:0]
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return matchedAny, rerr
			}
		}

		// Handle final unterminated line, if any.
		if len(carry) > 0 && bytes.Index(carry, litB) >= 0 {
			matchedAny = true
			if quiet {
				return true, nil
			}
			if filesOnly {
				if _, werr := fmt.Fprintln(hc.Stdout, name); werr != nil {
					if isPipeClosed(werr) {
						return true, epipeErr{}
					}
					return true, werr
				}
				return true, nil
			}
			if count {
				matched++
			} else if checkBin && bytes.IndexByte(carry, 0) >= 0 {
				if bw.Buffered() > 0 {
					if werr := bw.Flush(); werr != nil {
						return matchedAny, werr
					}
				}
				fmt.Fprintf(hc.Stderr, "grep: %s: binary file matches\n", name)
				return matchedAny, nil
			} else {
				if prefix != "" {
					bw.WriteString(prefix)
				}
				if _, werr := bw.Write(carry); werr != nil {
					return matchedAny, werr
				}
				if werr := bw.WriteByte('\n'); werr != nil {
					return matchedAny, werr
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return matchedAny, err
		}
		if count && !quiet {
			var outBuf [128]byte
			b := outBuf[:0]
			if len(prefix)+24 <= len(outBuf) {
				b = append(b, prefix...)
				b = strconv.AppendInt(b, int64(matched), 10)
				b = append(b, '\n')
				if _, werr := hc.Stdout.Write(b); werr != nil {
					return matchedAny, werr
				}
			} else {
				s := prefix + strconv.Itoa(matched) + "\n"
				if _, werr := io.WriteString(hc.Stdout, s); werr != nil {
					return matchedAny, werr
				}
			}
		} else if bw != nil && bw.Buffered() > 0 {
			if werr := bw.Flush(); werr != nil {
				return matchedAny, werr
			}
		}
		return matchedAny, nil
	}

	bw := getBufWriter64K(hc.Stdout)
	defer func() {
		_ = bw.Flush()
		putBufWriter64K(bw)
	}()
	matched, matchedAny := 0, false
	ln := 0
	var writeErr error

	handle := func(line []byte, terminated bool) bool {
		if ctx.Err() != nil {
			return false
		}
		ln++
		ok := bytes.Contains(line, litB)
		if invert {
			ok = !ok
		}
		if ok {
			matchedAny = true
			if quiet {
				return false
			}
			if filesOnly {
				bw.WriteString(name)
				bw.WriteByte('\n')
				return false
			}
			if checkBin && bytes.IndexByte(line, 0) >= 0 {
				if bw.Buffered() > 0 {
					if err := bw.Flush(); err != nil {
						writeErr = err
						return false
					}
				}
				fmt.Fprintf(hc.Stderr, "grep: %s: binary file matches\n", name)
				return false
			}
			if count {
				matched++
			} else {
				bw.WriteString(prefix)
				if lineNum {
					var numBuf [24]byte
					bw.Write(strconv.AppendInt(numBuf[:0], int64(ln), 10))
					bw.WriteByte(':')
				}
				bw.Write(line)
				if terminated {
					bw.WriteByte('\n')
				}
				if bw.Available() < 4096 {
					if err := bw.Flush(); err != nil {
						writeErr = err
						return false
					}
				}
			}
		}
		return true
	}
	err := streamLinesChunked(ctx, r, handle, func() error {
		if bw.Buffered() > 0 {
			return bw.Flush()
		}
		return nil
	})
	if writeErr != nil {
		return matchedAny, writeErr
	}
	if err != nil {
		return matchedAny, err
	}
	if err := ctx.Err(); err != nil {
		return matchedAny, err
	}
	if count && !quiet && !filesOnly {
		bw.WriteString(prefix)
		bw.WriteString(strconv.Itoa(matched))
		bw.WriteByte('\n')
		if err := bw.Flush(); err != nil {
			return matchedAny, err
		}
	}
	return matchedAny, bw.Flush()
}

func grepReader(ctx context.Context, hc interp.HandlerContext, name string, showName bool, re *regexp.Regexp, invert, lineNum, count, filesOnly, quiet, onlyMatch, checkBin bool, maxCount, after, before int, r io.Reader) (bool, error) {
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
	printLine := func(ln int, line string, isMatch bool) bool {
		if quiet {
			return true
		}
		if filesOnly {
			if _, err := fmt.Fprintln(hc.Stdout, name); err != nil && isPipeClosed(err) {
				return false
			}
			return true
		}
		if count {
			matched++
			return true
		}
		if onlyMatch {
			for _, m := range re.FindAllString(line, -1) {
				if m == "" {
					continue
				}
				if lineNum {
					if _, err := fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, ln, m); err != nil && isPipeClosed(err) {
						return false
					}
				} else {
					if _, err := fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, m); err != nil && isPipeClosed(err) {
						return false
					}
				}
			}
			return true
		}
		// GNU markers: with -n, "12:match"/"11-context"; with a
		// filename, "f:12:match"/"f-11-context"; otherwise no marker.
		if lineNum {
			sep := ":"
			if (after > 0 || before > 0) && !isMatch {
				sep = "-"
			}
			if _, err := fmt.Fprintf(hc.Stdout, "%s%d%s%s\n", prefix, ln, sep, line); err != nil && isPipeClosed(err) {
				return false
			}
		} else if prefix != "" && (after > 0 || before > 0) && !isMatch {
			if _, err := fmt.Fprintf(hc.Stdout, "%s-%s\n", prefix, line); err != nil && isPipeClosed(err) {
				return false
			}
		} else {
			if _, err := fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, line); err != nil && isPipeClosed(err) {
				return false
			}
		}
		return true
	}
	ln := 0
	lastOut := 0
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return matchedAny, err
		}
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
				if _, err := fmt.Fprintln(hc.Stdout, name); err != nil && isPipeClosed(err) {
					return true, epipeErr{}
				}
				return true, nil
			}
			if checkBin && strings.IndexByte(line, 0) >= 0 {
				fmt.Fprintf(hc.Stderr, "grep: %s: binary file matches\n", name)
				return true, nil
			}
			// GNU "--" separator between disjoint context groups.
			if (after > 0 || before > 0) && lastOut > 0 && ln-lastOut > 1 {
				if _, err := fmt.Fprintln(hc.Stdout, "--"); err != nil && isPipeClosed(err) {
					return matchedAny, epipeErr{}
				}
			}
			for _, pl := range pending {
				if !printLine(pl.ln, pl.text, false) {
					return matchedAny, epipeErr{}
				}
				lastOut = pl.ln
			}
			pending = nil
			if !printLine(ln, line, true) {
				return matchedAny, epipeErr{}
			}
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
					if !printLine(ln, line, false) {
						return matchedAny, epipeErr{}
					}
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
		if _, err := fmt.Fprintf(hc.Stdout, "%s%d\n", prefix, matched); err != nil && isPipeClosed(err) {
			return matchedAny, epipeErr{}
		}
	}
	return matchedAny, sc.Err()
}

// grepReaderWord wraps grepReader, fixing `-o -w` to print only the word
// (group 1) instead of the boundary characters consumed by the match.
func grepReaderWord(ctx context.Context, hc interp.HandlerContext, name string, showName bool, re *regexp.Regexp, wordMode, invert bool, lineNum, count, filesOnly, quiet, onlyMatch, checkBin bool, maxCount, after, before int, r io.Reader) (bool, error) {
	if !wordMode || !onlyMatch {
		return grepReader(ctx, hc, name, showName, re, invert, lineNum, count, filesOnly, quiet, onlyMatch, checkBin, maxCount, after, before, r)
	}
	sc := newLineReader(r)
	prefix := ""
	if showName && name != "" {
		prefix = name + ":"
	}
	matchedAny := false
	ln := 0
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return matchedAny, err
		}
		ln++
		line := sc.Text()
		subs := re.FindAllStringSubmatch(line, -1)
		if invert {
			if len(subs) == 0 {
				matchedAny = true
				if quiet {
					return true, nil
				}
				if checkBin && strings.IndexByte(line, 0) >= 0 {
					fmt.Fprintf(hc.Stderr, "grep: %s: binary file matches\n", name)
					return true, nil
				}
				if lineNum {
					if _, err := fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, ln, line); err != nil && isPipeClosed(err) {
						return matchedAny, epipeErr{}
					}
				} else {
					if _, err := fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, line); err != nil && isPipeClosed(err) {
						return matchedAny, epipeErr{}
					}
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
			if checkBin && strings.IndexByte(line, 0) >= 0 {
				fmt.Fprintf(hc.Stderr, "grep: %s: binary file matches\n", name)
				return true, nil
			}
			if lineNum {
				if _, err := fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, ln, m[1]); err != nil && isPipeClosed(err) {
					return matchedAny, epipeErr{}
				}
			} else {
				if _, err := fmt.Fprintf(hc.Stdout, "%s%s\n", prefix, m[1]); err != nil && isPipeClosed(err) {
					return matchedAny, epipeErr{}
				}
			}
		}
	}
	return matchedAny, sc.Err()
}

type ctxLine struct {
	ln   int
	text string
}

func cmdHead(ctx context.Context, hc interp.HandlerContext, args []string) error {
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
			if err := ctx.Err(); err != nil {
				return err
			}
			left := cn - total
			if left == 0 {
				break
			}
			w, err := io.CopyN(hc.Stdout, r, int64(left))
			total += uint64(w)
			if err != nil {
				if isPipeClosed(err) {
					return err
				}
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
					if _, err := fmt.Fprintln(hc.Stdout); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintf(hc.Stdout, "==> %s <==\n", names[i]); err != nil {
					return err
				}
			}
			var buf []string
			sc := newLineReader(r)
			for sc.Scan() {
				if err := ctx.Err(); err != nil {
					return err
				}
				buf = append(buf, sc.Text())
				if int64(len(buf)) > negLines {
					if _, err := fmt.Fprintln(hc.Stdout, buf[0]); err != nil {
						return err
					}
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
	bp := bufPool64K.Get().(*[]byte)
	buf := (*bp)[:64*1024]
	defer bufPool64K.Put(bp)
	bw := getBufWriter64K(hc.Stdout)
	defer func() {
		_ = bw.Flush()
		putBufWriter64K(bw)
	}()
	for i, r := range readers {
		if (multi && !*quiet) || *verbose {
			if i > 0 {
				if err := bw.WriteByte('\n'); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(bw, "==> %s <==\n", names[i]); err != nil {
				return err
			}
		}
		var c uint64
		for c < n {
			if err := ctx.Err(); err != nil {
				return err
			}
			nr, rerr := r.Read(buf)
			if nr > 0 {
				chunk := buf[:nr]
				need := n - c
				cnt := uint64(bytes.Count(chunk, nlBytes))
				if cnt < need {
					if _, werr := bw.Write(chunk); werr != nil {
						return werr
					}
					c += cnt
				} else {
					pos := 0
					for rem := need; rem > 0; rem-- {
						j := bytes.IndexByte(chunk[pos:], '\n')
						pos += j + 1
					}
					if _, werr := bw.Write(chunk[:pos]); werr != nil {
						return werr
					}
					c = n
					if nr > pos && r == hc.Stdin {
						if sk, ok := r.(io.Seeker); ok {
							_, _ = sk.Seek(-int64(nr-pos), io.SeekCurrent)
						}
					}
				}
				if bw.Buffered() > 0 {
					if werr := bw.Flush(); werr != nil {
						return werr
					}
				}
			}
			if c >= n || rerr == io.EOF {
				break
			}
			if rerr != nil {
				_ = bw.Flush()
				fmt.Fprintln(hc.Stderr, "head:", rerr)
				return exitError{1}
			}
		}
	}
	return bw.Flush()
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

func cmdTail(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fromLine := uint64(0)
	filtered := args[:1:1]
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			filtered = append(filtered, argv[i:]...)
			break
		}
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
		buf := make([]byte, 64*1024)
		for _, r := range readers {
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				nr, rerr := r.Read(buf)
				if nr > 0 && cn > 0 {
					if uint64(nr) >= cn {
						data = append(data[:0], buf[nr-int(cn):nr]...)
					} else {
						data = append(data, buf[:nr]...)
						if uint64(len(data)) > cn {
							data = append([]byte(nil), data[len(data)-int(cn):]...)
						}
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					fmt.Fprintln(hc.Stderr, "tail:", rerr)
					return exitError{1}
				}
			}
		}
		if len(data) == 0 {
			return nil
		}
		_, err = hc.Stdout.Write(data)
		return err
	}
	_ = names
	multi := len(readers) > 1
	for i, r := range readers {
		if (multi && !*quiet) || *verbose {
			if i > 0 {
				if _, err := fmt.Fprintln(hc.Stdout); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(hc.Stdout, "==> %s <==\n", names[i]); err != nil {
				return err
			}
		}
		if fromLine > 0 {
			sc := newLineReader(r)
			var ln uint64
			for sc.Scan() {
				if err := ctx.Err(); err != nil {
					return err
				}
				ln++
				if ln >= fromLine {
					nl := "\n"
					if !sc.EndedWithNewline() {
						nl = ""
					}
					if _, err := fmt.Fprint(hc.Stdout, sc.Text()+nl); err != nil {
						return err
					}
				}
			}
			if err := sc.Err(); err != nil {
				fmt.Fprintln(hc.Stderr, "tail:", err)
				return exitError{1}
			}
			continue
		}
		if n == 0 {
			buf := make([]byte, 64*1024)
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				_, rerr := r.Read(buf)
				if rerr != nil {
					break
				}
			}
			continue
		}
		ring := make([]string, n)
		ringNL := make([]bool, n)
		var count, pos uint64
		sc := newLineReader(r)
		for sc.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			ring[pos] = sc.Text()
			ringNL[pos] = sc.EndedWithNewline()
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
			idx := (start + k) % n
			nl := "\n"
			if k == shown-1 && !ringNL[idx] {
				nl = ""
			}
			if _, err := fmt.Fprint(hc.Stdout, ring[idx]+nl); err != nil {
				return err
			}
		}
	}
	return nil
}

func cmdSort(_ context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("sort", hc.Stderr)
	var keyFlagVals sortKeyFlagList
	fs.Var(&keyFlagVals, "k", "")
	fs.Var(&keyFlagVals, "key", "")
	reverse := fs.Bool("r", false, "")
	unique := fs.Bool("u", false, "")
	ignoreCase := fs.Bool("f", false, "")
	numeric := fs.Bool("n", false, "")
	check := fs.Bool("c", false, "")
	stable := fs.Bool("s", false, "")
	zeroTerm := fs.Bool("z", false, "")
	fs.BoolVar(zeroTerm, "zero-terminated", false, "")
	skipBlank := fs.Bool("b", false, "")
	fs.BoolVar(skipBlank, "ignore-leading-blanks", false, "")
	dictOrder := fs.Bool("d", false, "")
	fs.BoolVar(dictOrder, "dictionary-order", false, "")
	nonprint := fs.Bool("i", false, "")
	fs.BoolVar(nonprint, "ignore-nonprinting", false, "")
	general := fs.Bool("g", false, "")
	fs.BoolVar(general, "general-numeric-sort", false, "")
	human := fs.Bool("h", false, "")
	fs.BoolVar(human, "human-numeric-sort", false, "")
	month := fs.Bool("M", false, "")
	fs.BoolVar(month, "month-sort", false, "")
	sep := fs.String("t", "", "")
	fs.StringVar(sep, "field-separator", "", "")
	outFile := fs.String("o", "", "")
	fs.StringVar(outFile, "output", "", "")
	versionSort := fs.Bool("V", false, "")
	fs.BoolVar(versionSort, "version-sort", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	mode := sortModeByte
	modeFlags := 0
	for _, m := range []struct {
		on  bool
		m   int
		tag string
	}{{*numeric, sortModeNumeric, "n"}, {*general, sortModeGeneral, "g"},
		{*human, sortModeHuman, "h"}, {*month, sortModeMonth, "M"},
		{*versionSort, sortModeVersion, "V"}} {
		if m.on {
			modeFlags++
			mode = m.m
		}
	}
	if modeFlags > 1 {
		var tags string
		for _, m := range []struct {
			on  bool
			tag string
		}{{*numeric, "n"}, {*general, "g"}, {*human, "h"}, {*month, "M"}, {*versionSort, "V"}} {
			if m.on {
				tags += m.tag
			}
		}
		fmt.Fprintf(hc.Stderr, "sort: options '-%s' are incompatible\n", tags)
		return exitError{2}
	}
	if *sep != "" {
		if n := len([]rune(*sep)); n > 1 {
			fmt.Fprintf(hc.Stderr, "sort: multi-character tab \u2018%s\u2019\n", *sep)
			return exitError{2}
		}
	}
	var specs []sortKeySpec
	for _, ks := range keyFlagVals {
		sp, err := parseSortKeySpec(ks)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "sort: invalid key \u2018%s\u2019\n", ks)
			return exitError{2}
		}
		specs = append(specs, sp)
	}
	cfg := sortConfig{
		specs:     specs,
		sep:       *sep,
		mode:      mode,
		reverse:   *reverse,
		fold:      *ignoreCase,
		skipBlank: *skipBlank,
		stable:    *stable,
		unique:    *unique,
		dict:      *dictOrder,
		nonprint:  *nonprint,
	}
	if len(cfg.specs) == 0 {
		cfg.specs = []sortKeySpec{{}} // implicit whole-line key
	}
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
	all, err := readAllSortInputs(readers)
	if err != nil {
		fmt.Fprintln(hc.Stderr, "sort:", err)
		return exitError{1}
	}

	openSortOut := func() (*bufio.Writer, *os.File, error) {
		if *outFile != "" {
			p := resolve(hc.Dir, *outFile)
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if err != nil {
				fmt.Fprintln(hc.Stderr, "sort:", err)
				return nil, nil, exitError{1}
			}
			return getBufWriter64K(f), f, nil
		}
		return getBufWriter64K(hc.Stdout), nil, nil
	}
	outDelim := byte('\n')
	if *zeroTerm {
		outDelim = 0
	}

	// Fast path 1: plain whole-line byte sort over compact 16-byte spans with
	// an in-register 8-byte big-endian prefix (zero string slice allocs, zero
	// GC pointer scanning, and register-only comparisons for <=8B records).
	if !*check && len(all) <= math.MaxUint32 &&
		len(cfg.specs) == 1 && cfg.specs[0] == (sortKeySpec{}) &&
		cfg.mode == sortModeByte && !cfg.fold && !cfg.dict && !cfg.nonprint && !cfg.skipBlank {
		spans := splitSortSpans(all, delim)
		rev := cfg.reverse
		cmpSpan := func(a, b sortSpan) int {
			var c int
			if a.prefix != b.prefix {
				if a.prefix < b.prefix {
					c = -1
				} else {
					c = 1
				}
			} else {
				la := a.end - a.off
				lb := b.end - b.off
				if la <= 8 || lb <= 8 {
					if la < lb {
						c = -1
					} else if la > lb {
						c = 1
					}
				} else {
					c = bytes.Compare(all[a.off+8:a.end], all[b.off+8:b.end])
				}
			}
			if rev {
				return -c
			}
			return c
		}
		if cfg.stable {
			slices.SortStableFunc(spans, cmpSpan)
		} else {
			slices.SortFunc(spans, cmpSpan)
		}
		out, f, err := openSortOut()
		if err != nil {
			return err
		}
		if f != nil {
			defer f.Close()
		}
		defer func() {
			_ = out.Flush()
			putBufWriter64K(out)
		}()
		for i, s := range spans {
			if cfg.unique && i > 0 {
				p := spans[i-1]
				if p.prefix == s.prefix {
					lp := p.end - p.off
					ls := s.end - s.off
					if lp == ls && (ls <= 8 || bytes.Equal(all[p.off+8:p.end], all[s.off+8:s.end])) {
						continue
					}
				}
			}
			if int(s.end) < len(all) && all[s.end] == outDelim {
				out.Write(all[s.off : s.end+1])
			} else {
				out.Write(all[s.off:s.end])
				out.WriteByte(outDelim)
			}
		}
		return out.Flush()
	}

	// Fast path 2: single-key Schwartzian transform over pointer-free (noscan)
	// 16-byte/24-byte records directly indexing `all` (zero []string allocation,
	// zero GC pointer scanning, single pass over input).
	if !*check && len(all) <= math.MaxUint32 &&
		len(cfg.specs) == 1 && !cfg.fold && !cfg.dict && !cfg.nonprint &&
		!cfg.specs[0].fold && !cfg.specs[0].dict && !cfg.specs[0].nonprint &&
		cfg.specs[0].start.char == 0 && !(cfg.specs[0].hasEnd && cfg.specs[0].end.char > 0) {
		sp := &cfg.specs[0]
		kmode := sp.mode
		if kmode == sortModeByte {
			kmode = cfg.mode
		}
		if kmode == sortModeNumeric || kmode == sortModeGeneral || kmode == sortModeHuman || kmode == sortModeByte {
			if len(all) == 0 {
				out, f, err := openSortOut()
				if err != nil {
					return err
				}
				if f != nil {
					defer f.Close()
				}
				err = out.Flush()
				putBufWriter64K(out)
				return err
			}
			endPos := len(all)
			if all[endPos-1] == delim {
				endPos--
			}
			trimmed := all[:endPos]
			var delimBuf [1]byte
			delimBuf[0] = delim
			nLines := bytes.Count(trimmed, delimBuf[:]) + 1
			allS := unsafe.String(unsafe.SliceData(all), len(all))
			rev := sp.reverse != cfg.reverse
			needTie := !cfg.stable && !cfg.unique

			switch kmode {
			case sortModeNumeric:
				recs := make([]fastNumRec, nLines)
				var overflow []sortKeyVal
				idx, off := 0, 0
				for {
					j := bytes.IndexByte(trimmed[off:], delim)
					recEnd := endPos
					if j >= 0 {
						recEnd = off + j
					}
					l := allS[off:recEnd]
					as, ae := extractSingleKeyRangeB(l, cfg.sep, sp, cfg.skipBlank)
					ks := l[as:ae]
					var val int64
					if v, ok := parseSortIntFast(ks); ok {
						val = v
					} else {
						neg, ip, fp := sortParseNum(ks)
						ip = strings.TrimLeft(ip, "0")
						fp = strings.TrimRight(fp, "0")
						if ip == "" {
							ip = "0"
						}
						if ip == "0" && fp == "" {
							neg = false
						}
						val = sortNumOvBase - int64(len(overflow))
						overflow = append(overflow, sortKeyVal{neg: neg, ip: ip, fp: fp})
					}
					recs[idx] = fastNumRec{val: val, off: uint32(off), end: uint32(recEnd)}
					if j < 0 {
						break
					}
					idx++
					off = recEnd + 1
				}
				recs = recs[:idx+1]
				cmpNum := func(a, b fastNumRec) int {
					var c int
					if a.val > sortNumOvBase && b.val > sortNumOvBase {
						if a.val < b.val {
							c = -1
						} else if a.val > b.val {
							c = 1
						}
					} else {
						c = compareFastNumMixed(a.val, b.val, overflow)
					}
					if c != 0 {
						if rev {
							return -c
						}
						return c
					}
					if !needTie {
						return 0
					}
					c = strings.Compare(allS[a.off:a.end], allS[b.off:b.end])
					if cfg.reverse {
						return -c
					}
					return c
				}
				if cfg.stable || cfg.unique {
					slices.SortStableFunc(recs, cmpNum)
				} else {
					slices.SortFunc(recs, cmpNum)
				}
				out, f, err := openSortOut()
				if err != nil {
					return err
				}
				if f != nil {
					defer f.Close()
				}
				defer func() {
					_ = out.Flush()
					putBufWriter64K(out)
				}()
				for i, r := range recs {
					if cfg.unique && i > 0 {
						p := recs[i-1]
						if p.val > sortNumOvBase && r.val > sortNumOvBase {
							if p.val == r.val {
								continue
							}
						} else if compareFastNumMixed(p.val, r.val, overflow) == 0 {
							continue
						}
					}
					if int(r.end) < len(all) && all[r.end] == outDelim {
						out.Write(all[r.off : r.end+1])
					} else {
						out.Write(all[r.off:r.end])
						out.WriteByte(outDelim)
					}
				}
				return out.Flush()

			case sortModeGeneral, sortModeHuman:
				recs := make([]fastFloatRec, nLines)
				idx, off := 0, 0
				for {
					j := bytes.IndexByte(trimmed[off:], delim)
					recEnd := endPos
					if j >= 0 {
						recEnd = off + j
					}
					l := allS[off:recEnd]
					as, ae := extractSingleKeyRangeB(l, cfg.sep, sp, cfg.skipBlank)
					ks := l[as:ae]
					var fv float64
					if kmode == sortModeGeneral {
						fv = sortParseGeneral(ks)
					} else {
						fv = sortParseHuman(ks)
					}
					if fv == 0 {
						fv = 0
					}
					recs[idx] = fastFloatRec{val: fv, off: uint32(off), end: uint32(recEnd)}
					if j < 0 {
						break
					}
					idx++
					off = recEnd + 1
				}
				recs = recs[:idx+1]
				cmpFlt := func(a, b fastFloatRec) int {
					var c int
					if a.val < b.val {
						c = -1
					} else if a.val > b.val {
						c = 1
					}
					if c != 0 {
						if rev {
							return -c
						}
						return c
					}
					if !needTie {
						return 0
					}
					c = strings.Compare(allS[a.off:a.end], allS[b.off:b.end])
					if cfg.reverse {
						return -c
					}
					return c
				}
				if cfg.stable || cfg.unique {
					slices.SortStableFunc(recs, cmpFlt)
				} else {
					slices.SortFunc(recs, cmpFlt)
				}
				out, f, err := openSortOut()
				if err != nil {
					return err
				}
				if f != nil {
					defer f.Close()
				}
				defer func() {
					_ = out.Flush()
					putBufWriter64K(out)
				}()
				for i, r := range recs {
					if cfg.unique && i > 0 && recs[i-1].val == r.val {
						continue
					}
					if int(r.end) < len(all) && all[r.end] == outDelim {
						out.Write(all[r.off : r.end+1])
					} else {
						out.Write(all[r.off:r.end])
						out.WriteByte(outDelim)
					}
				}
				return out.Flush()

			case sortModeByte:
				recs := make([]fastByteKeyRec, nLines)
				idx, off := 0, 0
				for {
					j := bytes.IndexByte(trimmed[off:], delim)
					recEnd := endPos
					if j >= 0 {
						recEnd = off + j
					}
					l := allS[off:recEnd]
					as, ae := extractSingleKeyRangeB(l, cfg.sep, sp, cfg.skipBlank)
					ks := l[as:ae]
					recs[idx] = fastByteKeyRec{
						keyPfx: packPrefix8S(ks),
						keyOff: uint32(off + as),
						keyEnd: uint32(off + ae),
						off:    uint32(off),
						end:    uint32(recEnd),
					}
					if j < 0 {
						break
					}
					idx++
					off = recEnd + 1
				}
				recs = recs[:idx+1]
				cmpKey := func(a, b fastByteKeyRec) int {
					var c int
					if a.keyPfx != b.keyPfx {
						if a.keyPfx < b.keyPfx {
							c = -1
						} else {
							c = 1
						}
					} else {
						la := a.keyEnd - a.keyOff
						lb := b.keyEnd - b.keyOff
						if la <= 8 || lb <= 8 {
							if la < lb {
								c = -1
							} else if la > lb {
								c = 1
							}
						} else {
							c = strings.Compare(allS[a.keyOff+8:a.keyEnd], allS[b.keyOff+8:b.keyEnd])
						}
					}
					if c != 0 {
						if rev {
							return -c
						}
						return c
					}
					if !needTie {
						return 0
					}
					c = strings.Compare(allS[a.off:a.end], allS[b.off:b.end])
					if cfg.reverse {
						return -c
					}
					return c
				}
				if cfg.stable || cfg.unique {
					slices.SortStableFunc(recs, cmpKey)
				} else {
					slices.SortFunc(recs, cmpKey)
				}
				out, f, err := openSortOut()
				if err != nil {
					return err
				}
				if f != nil {
					defer f.Close()
				}
				defer func() {
					_ = out.Flush()
					putBufWriter64K(out)
				}()
				for i, r := range recs {
					if cfg.unique && i > 0 {
						p := recs[i-1]
						if p.keyPfx == r.keyPfx {
							lp := p.keyEnd - p.keyOff
							lr := r.keyEnd - r.keyOff
							if lp == lr && (lr <= 8 || allS[p.keyOff+8:p.keyEnd] == allS[r.keyOff+8:r.keyEnd]) {
								continue
							}
						}
					}
					if int(r.end) < len(all) && all[r.end] == outDelim {
						out.Write(all[r.off : r.end+1])
					} else {
						out.Write(all[r.off:r.end])
						out.WriteByte(outDelim)
					}
				}
				return out.Flush()
			}
		}
	}

	var allStr string
	if len(all) > 0 {
		allStr = unsafe.String(unsafe.SliceData(all), len(all))
	}
	lines := splitSortRecords(allStr, delim)

	items := make([]sortItem, len(lines))
	nSpec := len(cfg.specs)
	keyArena := make([]sortKeyVal, len(lines)*nSpec)
	for i, l := range lines {
		items[i] = cfg.materialize(l, keyArena[i*nSpec:(i+1)*nSpec])
	}
	if *check {
		for i := 1; i < len(items); i++ {
			if cfg.cmpItems(&items[i], &items[i-1]) < 0 || (*unique && cfg.keysEqualItems(&items[i], &items[i-1])) {
				name := "-"
				if len(fs.Args()) > 0 {
					name = fs.Args()[0]
				}
				fmt.Fprintf(hc.Stderr, "sort: %s:%d: disorder: %s\n", name, i+1, items[i].line)
				return exitError{1}
			}
		}
		return nil
	}
	if *stable || *unique {
		slices.SortStableFunc(items, func(a, b sortItem) int { return cfg.cmpItems(&a, &b) })
	} else {
		slices.SortFunc(items, func(a, b sortItem) int { return cfg.cmpItems(&a, &b) })
	}
	out, f, err := openSortOut()
	if err != nil {
		return err
	}
	if f != nil {
		defer f.Close()
	}
	defer func() {
		_ = out.Flush()
		putBufWriter64K(out)
	}()
	var prev *sortItem
	for i := range items {
		it := &items[i]
		if *unique && prev != nil && cfg.keysEqualItems(prev, it) {
			continue
		}
		out.WriteString(it.line)
		out.WriteByte(outDelim)
		prev = it
	}
	return out.Flush()
}

const sortNumOvBase int64 = -4_000_000_000_000_000_000

type sortSpan struct {
	prefix uint64
	off    uint32
	end    uint32
}

type fastNumRec struct {
	val int64
	off uint32
	end uint32
}

type fastFloatRec struct {
	val float64
	off uint32
	end uint32
}

type fastByteKeyRec struct {
	keyPfx uint64
	keyOff uint32
	keyEnd uint32
	off    uint32
	end    uint32
}

func readAllSortInputs(readers []io.Reader) ([]byte, error) {
	var est int64
	for _, r := range readers {
		if st, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
			if fi, err := st.Stat(); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
				est += fi.Size()
			}
		}
	}
	var all []byte
	if est > 0 && est < 1<<30 {
		all = make([]byte, 0, est+64)
	}
	for _, r := range readers {
		for {
			if len(all) == cap(all) {
				grow := max(64*1024, cap(all))
				next := make([]byte, len(all), cap(all)+grow)
				copy(next, all)
				all = next
			}
			n, err := r.Read(all[len(all):cap(all)])
			all = all[:len(all)+n]
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return all, nil
}

func packPrefix8(b []byte) uint64 {
	if len(b) >= 8 {
		return binary.BigEndian.Uint64(b)
	}
	var p uint64
	for i := 0; i < len(b); i++ {
		p |= uint64(b[i]) << (56 - 8*i)
	}
	return p
}

func packPrefix8S(s string) uint64 {
	if len(s) >= 8 {
		return uint64(s[0])<<56 | uint64(s[1])<<48 | uint64(s[2])<<40 | uint64(s[3])<<32 |
			uint64(s[4])<<24 | uint64(s[5])<<16 | uint64(s[6])<<8 | uint64(s[7])
	}
	var p uint64
	for i := 0; i < len(s); i++ {
		p |= uint64(s[i]) << (56 - 8*i)
	}
	return p
}

func splitSortSpans(data []byte, delim byte) []sortSpan {
	if len(data) == 0 {
		return nil
	}
	endPos := len(data)
	if data[endPos-1] == delim {
		endPos--
	}
	trimmed := data[:endPos]
	var delimBuf [1]byte
	delimBuf[0] = delim
	n := bytes.Count(trimmed, delimBuf[:]) + 1
	spans := make([]sortSpan, n)
	idx := 0
	off := 0
	for {
		j := bytes.IndexByte(trimmed[off:], delim)
		if j < 0 {
			rec := trimmed[off:]
			spans[idx] = sortSpan{
				prefix: packPrefix8(rec),
				off:    uint32(off),
				end:    uint32(endPos),
			}
			break
		}
		recEnd := off + j
		rec := trimmed[off:recEnd]
		spans[idx] = sortSpan{
			prefix: packPrefix8(rec),
			off:    uint32(off),
			end:    uint32(recEnd),
		}
		idx++
		off = recEnd + 1
	}
	return spans[:idx+1]
}

func parseSortIntFast(s string) (int64, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	neg := false
	if i < len(s) && s[i] == '-' {
		neg = true
		i++
	}
	for i < len(s) && s[i] == '0' {
		i++
	}
	var u uint64
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		if digits < 19 {
			u = u*10 + uint64(s[i]-'0')
		}
		digits++
		i++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] == '0' {
			i++
		}
		if i < len(s) && s[i] >= '1' && s[i] <= '9' {
			return 0, false
		}
	}
	if digits > 18 {
		return 0, false
	}
	if u == 0 {
		return 0, true
	}
	if neg {
		return -int64(u), true
	}
	return int64(u), true
}

func compareIntVsNumKey(iv int64, b *sortKeyVal) int {
	negA := iv < 0
	if negA != b.neg {
		if negA {
			return -1
		}
		return 1
	}
	var u uint64
	if negA {
		u = uint64(-iv)
	} else {
		u = uint64(iv)
	}
	var numBuf [24]byte
	ipA := strconv.AppendUint(numBuf[:0], u, 10)
	sign := 1
	if negA {
		sign = -1
	}
	mag := 0
	if len(ipA) != len(b.ip) {
		if len(ipA) < len(b.ip) {
			mag = -1
		} else {
			mag = 1
		}
	} else {
		bp := unsafe.Slice(unsafe.StringData(b.ip), len(b.ip))
		if cc := bytes.Compare(ipA, bp); cc != 0 {
			mag = cc
		} else if len(b.fp) > 0 {
			mag = -1
		}
	}
	return sign * mag
}

func compareFastNumMixed(av, bv int64, overflow []sortKeyVal) int {
	if av <= sortNumOvBase && bv <= sortNumOvBase {
		return compareNumParts(&overflow[sortNumOvBase-av], &overflow[sortNumOvBase-bv])
	}
	if av > sortNumOvBase {
		return compareIntVsNumKey(av, &overflow[sortNumOvBase-bv])
	}
	return -compareIntVsNumKey(bv, &overflow[sortNumOvBase-av])
}

func extractSingleKeyRangeB(line, sep string, sp *sortKeySpec, skipGlobal bool) (int, int) {
	skip := sp.skipBlank || skipGlobal
	sf := sp.start.field
	hasEnd := sp.hasEnd
	ef := sp.end.field
	n := len(line)
	if sf <= 0 && !hasEnd && !skip {
		return 0, n
	}
	maxF := sf
	if hasEnd && ef > maxF {
		maxF = ef
	}
	fs, fe := n, n
	if sf <= 0 {
		fs, fe = 0, n
	}
	en := n
	if sep != "" {
		sepB := sep[0]
		pos := 0
		for cur := 1; cur <= maxF; cur++ {
			j := strings.IndexByte(line[pos:], sepB)
			end := n
			if j >= 0 {
				end = pos + j
			}
			if cur == sf {
				fs, fe = pos, end
			}
			if hasEnd && cur == ef {
				en = end
			}
			if j < 0 {
				break
			}
			pos = end + 1
		}
	} else {
		prevEnd := 0
		cur := 0
		i := 0
		for i < n && cur < maxF {
			for i < n && (line[i] == ' ' || line[i] == '\t') {
				i++
			}
			if i >= n {
				break
			}
			j := i + 1
			for j < n && line[j] != ' ' && line[j] != '\t' {
				j++
			}
			cur++
			start := prevEnd
			if cur == 1 {
				start = 0
			}
			if cur == sf {
				fs, fe = start, j
			}
			if hasEnd && cur == ef {
				en = j
			}
			prevEnd = j
			i = j
		}
		if cur == 0 {
			if sf == 1 {
				fs, fe = 0, n
			}
			if hasEnd && ef == 1 {
				en = n
			}
		}
	}
	st := fs
	if skip {
		for st < fe && (line[st] == ' ' || line[st] == '\t') {
			st++
		}
	}
	if st > n {
		st = n
	}
	if !hasEnd {
		en = n
	}
	if en < st {
		en = st
	}
	return st, en
}

// splitSortRecords splits the whole input into records as zero-copy
// substrings, so no per-line allocation is needed. Inputs are read as a
// single stream, like GNU sort does across multiple files.
func splitSortRecords(data string, delim byte) []string {
	if data == "" {
		return nil
	}
	if data[len(data)-1] == delim {
		data = data[:len(data)-1]
	}
	var delimBuf [1]byte
	delimBuf[0] = delim
	n := 1
	if len(data) > 0 {
		n += bytes.Count(unsafe.Slice(unsafe.StringData(data), len(data)), delimBuf[:])
	}
	lines := make([]string, n)
	idx := 0
	for {
		j := strings.IndexByte(data, delim)
		if j < 0 {
			lines[idx] = data
			break
		}
		lines[idx] = data[:j]
		idx++
		data = data[j+1:]
	}
	return lines[:idx+1]
}

// sortKeyVal is one precomputed key of one line: the key text plus any
// parsed numeric form, computed once per line instead of per comparison.
type sortKeyVal struct {
	s    string // key text (already -d/-i/-f processed)
	mode int    // effective compare mode
	neg  bool   // sortModeNumeric parts (normalized)
	ip   string
	fp   string
	f    float64 // sortModeGeneral / sortModeHuman
	m    int     // sortModeMonth
}

// sortItem is one input line with its precomputed keys.
type sortItem struct {
	line string
	keys []sortKeyVal
}

// materialize extracts and pre-parses every key of a line once, filling
// the caller-provided slice so sorting needs no per-item allocation.
func (sc *sortConfig) materialize(line string, keys []sortKeyVal) sortItem {
	var frB [][2]int
	haveB := false
	var rs []rune
	var frR [][2]int
	for i := range sc.specs {
		sp := &sc.specs[i]
		var key string
		if sp.start.char > 0 || (sp.hasEnd && sp.end.char > 0) {
			// rune-accurate path for F.C character offsets
			if rs == nil {
				rs = []rune(line)
				frR = sortFieldRanges(rs, sc.sep)
			}
			as, ae := sp.keyRange(rs, frR, sc.skipBlank)
			key = string(rs[as:ae])
		} else {
			if !haveB {
				frB = sortFieldRangesB(sc.frScr[:0], line, sc.sep)
				sc.frScr = frB
				haveB = true
			}
			as, ae := sp.keyRangeB(line, frB, sc.skipBlank)
			key = line[as:ae] // zero-copy slice of the line
		}
		kv := sortKeyVal{s: key}
		if sp.dict || sc.dict || sp.nonprint || sc.nonprint {
			kv.s = sortFilterKey(kv.s, sp.dict || sc.dict, sp.nonprint || sc.nonprint)
		}
		if sp.fold || sc.fold {
			kv.s = strings.ToLower(kv.s)
		}
		mode := sp.mode
		if mode == sortModeByte {
			mode = sc.mode
		}
		kv.mode = mode
		switch mode {
		case sortModeNumeric:
			kv.neg, kv.ip, kv.fp = sortParseNum(kv.s)
			kv.ip = strings.TrimLeft(kv.ip, "0")
			kv.fp = strings.TrimRight(kv.fp, "0")
			if kv.ip == "" {
				kv.ip = "0"
			}
			if kv.ip == "0" && kv.fp == "" {
				kv.neg = false
			}
		case sortModeGeneral:
			kv.f = sortParseGeneral(kv.s)
		case sortModeHuman:
			kv.f = sortParseHuman(kv.s)
		case sortModeMonth:
			kv.m = sortParseMonth(kv.s)
		}
		keys[i] = kv
	}
	return sortItem{line: line, keys: keys}
}

// compareVal compares two precomputed keys under one key's modifiers.
func (sc *sortConfig) compareVal(a, b *sortKeyVal, sp *sortKeySpec) int {
	var c int
	switch a.mode {
	case sortModeNumeric:
		c = compareNumParts(a, b)
	case sortModeGeneral, sortModeHuman:
		if a.f < b.f {
			c = -1
		} else if a.f > b.f {
			c = 1
		}
	case sortModeMonth:
		if a.m < b.m {
			c = -1
		} else if a.m > b.m {
			c = 1
		}
	case sortModeVersion:
		c = compareVersion(a.s, b.s)
	default:
		c = strings.Compare(a.s, b.s)
	}
	if sp.reverse != sc.reverse {
		c = -c
	}
	return c
}

// compareNumParts compares pre-parsed GNU -n numbers with arbitrary
// precision (int/frac parts are already normalized).
func compareNumParts(a, b *sortKeyVal) int {
	if a.neg != b.neg {
		if a.neg {
			return -1
		}
		return 1
	}
	sign := 1
	if a.neg {
		sign = -1
	}
	mag := 0
	if a.ip != b.ip {
		if len(a.ip) != len(b.ip) {
			if len(a.ip) < len(b.ip) {
				mag = -1
			} else {
				mag = 1
			}
		} else if a.ip < b.ip {
			mag = -1
		} else {
			mag = 1
		}
	} else {
		// Fracs have trailing zeros trimmed: compare the common prefix,
		// and the longer tail is always bigger.
		n, m := len(a.fp), len(b.fp)
		mm := n
		if m < mm {
			mm = m
		}
		if c := strings.Compare(a.fp[:mm], b.fp[:mm]); c != 0 {
			mag = c
		} else if n != m {
			if n < m {
				mag = -1
			} else {
				mag = 1
			}
		}
	}
	return sign * mag
}

// cmpItems applies every key in order, then GNU's last-resort whole-line
// comparison (reversed by global -r only).
func (sc *sortConfig) cmpItems(a, b *sortItem) int {
	for i := range sc.specs {
		if c := sc.compareVal(&a.keys[i], &b.keys[i], &sc.specs[i]); c != 0 {
			return c
		}
	}
	if sc.stable || sc.unique {
		return 0
	}
	c := strings.Compare(a.line, b.line)
	if sc.reverse {
		c = -c
	}
	return c
}

// keysEqualItems reports whether two lines have equal sort keys (no last
// resort) — GNU's notion of "equal" for -u dedup and -cu checking.
func (sc *sortConfig) keysEqualItems(a, b *sortItem) bool {
	for i := range sc.specs {
		if sc.compareVal(&a.keys[i], &b.keys[i], &sc.specs[i]) != 0 {
			return false
		}
	}
	return true
}

// compareVersion implements GNU sort -V / ls -v version ordering, a
// faithful port of gnulib's filevercmp/filenvercmp. It first cuts a
// file suffix (matching (\.[A-Za-z~][A-Za-z0-9~]*)*$) and compares the
// prefixes with the Debian verrevcmp algorithm, falling back to a
// comparison of the whole strings when the prefixes (and suffixes)
// compare equal. Byte ordering inside a run follows gnulib's order():
// '~' < end-of-string < digits < letters < all other bytes.
func compareVersion(a, b string) int {
	if a == "" {
		if b == "" {
			return 0
		}
		return -1
	}
	if b == "" {
		return 1
	}
	// Leading "." special cases: "." then ".." then other dotfiles.
	if a[0] == '.' {
		if b[0] != '.' {
			return -1
		}
		adot := len(a) == 1
		bdot := len(b) == 1
		if adot || bdot {
			switch {
			case adot && bdot:
				return 0
			case adot:
				return -1
			default:
				return 1
			}
		}
		adotdot := len(a) == 2 && a[1] == '.'
		bdotdot := len(b) == 2 && b[1] == '.'
		if adotdot || bdotdot {
			switch {
			case adotdot && bdotdot:
				return 0
			case adotdot:
				return -1
			default:
				return 1
			}
		}
	} else if b[0] == '.' {
		return 1
	}

	aPrefix := verPrefixLen(a)
	bPrefix := verPrefixLen(b)
	onePass := aPrefix == len(a) && bPrefix == len(b)
	if r := verrevcmp(a, aPrefix, b, bPrefix); r != 0 {
		return r
	}
	if onePass {
		return 0
	}
	return verrevcmp(a, len(a), b, len(b))
}

// verPrefixLen returns the length of a prefix of s that leaves a suffix
// matching (\.[A-Za-z~][A-Za-z0-9~]*)*$ — a port of gnulib's
// file_prefixlen, matching coreutils 8.32/Git-Bash behavior: a suffix
// also starts at a '.' that ends the string (never returns the whole
// nonempty string).
func verPrefixLen(s string) int {
	n := len(s)
	prefix := 0
	for i := 0; ; {
		if i == n {
			return prefix
		}
		i++
		prefix = i
		for i+1 <= n && s[i] == '.' && (i+1 == n || isVerAlpha(s[i+1]) || s[i+1] == '~') {
			i++
			for i < n && (isVerAlnum(s[i]) || s[i] == '~') {
				i++
			}
		}
	}
}

func isVerAlpha(c byte) bool { return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }
func isVerDigit(c byte) bool { return c >= '0' && c <= '9' }
func isVerAlnum(c byte) bool { return isVerAlpha(c) || isVerDigit(c) }

// verOrder returns the sort weight of s[pos]; pos==len sorts before every
// byte except '~'. (gnulib order())
func verOrder(s string, pos, length int) int {
	if pos >= length {
		return -1
	}
	c := s[pos]
	switch {
	case isVerDigit(c):
		return 0
	case isVerAlpha(c):
		return int(c)
	case c == '~':
		return -2
	default:
		return int(c) + 256
	}
}

// verrevcmp is a port of gnulib's verrevcmp (Debian version compare)
// over the first n1 bytes of s1 and n2 bytes of s2.
func verrevcmp(s1 string, n1 int, s2 string, n2 int) int {
	p1, p2 := 0, 0
	for p1 < n1 || p2 < n2 {
		firstDiff := 0
		for (p1 < n1 && !isVerDigit(s1[p1])) || (p2 < n2 && !isVerDigit(s2[p2])) {
			c1 := verOrder(s1, p1, n1)
			c2 := verOrder(s2, p2, n2)
			if c1 != c2 {
				return c1 - c2
			}
			p1++
			p2++
		}
		for p1 < n1 && s1[p1] == '0' {
			p1++
		}
		for p2 < n2 && s2[p2] == '0' {
			p2++
		}
		for p1 < n1 && p2 < n2 && isVerDigit(s1[p1]) && isVerDigit(s2[p2]) {
			if firstDiff == 0 {
				firstDiff = int(s1[p1]) - int(s2[p2])
			}
			p1++
			p2++
		}
		if p1 < n1 && isVerDigit(s1[p1]) {
			return 1
		}
		if p2 < n2 && isVerDigit(s2[p2]) {
			return -1
		}
		if firstDiff != 0 {
			return firstDiff
		}
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

// ---------- GNU sort key machinery ----------
//
// Field/key semantics follow GNU sort exactly:
//   - default (blank-separated) fields are "leading blanks + non-blank
//     run"; field 1 starts at column 0, so leading blanks belong to the
//     field; trailing blanks at end of line belong to no field
//   - F.C character offsets count from the field start (from the first
//     non-blank with -b) and are not clamped to the field end, only to
//     the end of the line
//   - a key with no end position extends to the end of the line
//   - ties fall back to a whole-line byte comparison (last resort)
//     unless -s/-u; that fallback is reversed by global -r only

type sortPos struct {
	field int // 1-based field number
	char  int // 1-based char offset within the field; 0 = field boundary
}

type sortKeySpec struct {
	start, end sortPos
	hasEnd     bool
	mode       int // sortMode*
	reverse    bool
	fold       bool
	skipBlank  bool
	dict       bool // -d
	nonprint   bool // -i
}

type sortKeyFlagList []string

func (k *sortKeyFlagList) String() string { return strings.Join(*k, " ") }
func (k *sortKeyFlagList) Set(v string) error {
	*k = append(*k, v)
	return nil
}

// parseSortPos parses F[.C][bfnr]* and folds the modifiers into spec.
func parseSortPos(s string, spec *sortKeySpec) (sortPos, string, error) {
	p := sortPos{}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		p.field = p.field*10 + int(s[i]-'0')
		i++
	}
	if i == 0 {
		return p, s, errors.New("invalid key")
	}
	if i < len(s) && s[i] == '.' {
		i++
		j := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			p.char = p.char*10 + int(s[i]-'0')
			i++
		}
		if i == j {
			return p, s, errors.New("invalid key")
		}
	}
	for i < len(s) {
		switch s[i] {
		case 'b':
			spec.skipBlank = true
		case 'f':
			spec.fold = true
		case 'n':
			spec.mode = sortModeNumeric
		case 'g':
			spec.mode = sortModeGeneral
		case 'h':
			spec.mode = sortModeHuman
		case 'M':
			spec.mode = sortModeMonth
		case 'd':
			spec.dict = true
		case 'i':
			spec.nonprint = true
		case 'r':
			spec.reverse = true
		default:
			return p, s[i:], nil
		}
		i++
	}
	return p, "", nil
}

// parseSortKeySpec parses one GNU -k spec: F[.C][bfnr]*[,F[.C][bfnr]*].
func parseSortKeySpec(s string) (sortKeySpec, error) {
	spec := sortKeySpec{}
	var err error
	spec.start, s, err = parseSortPos(s, &spec)
	if err != nil {
		return spec, err
	}
	if strings.HasPrefix(s, ",") {
		spec.end, s, err = parseSortPos(s[1:], &spec)
		if err != nil {
			return spec, err
		}
		spec.hasEnd = true
	}
	if s != "" {
		return spec, errors.New("invalid key")
	}
	return spec, nil
}

func isSortBlank(r rune) bool { return r == ' ' || r == '\t' }

// sortFieldRanges returns the rune range [start,end) of every GNU field.
func sortFieldRanges(rs []rune, sep string) [][2]int {
	if sep != "" {
		sepR := []rune(sep)[0]
		fr := make([][2]int, 0, 8)
		start := 0
		for i, r := range rs {
			if r == sepR {
				fr = append(fr, [2]int{start, i})
				start = i + 1
			}
		}
		return append(fr, [2]int{start, len(rs)})
	}
	var runs [][2]int
	i := 0
	for i < len(rs) {
		if isSortBlank(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && !isSortBlank(rs[j]) {
			j++
		}
		runs = append(runs, [2]int{i, j})
		i = j
	}
	if len(runs) == 0 {
		return [][2]int{{0, len(rs)}}
	}
	fr := make([][2]int, 0, len(runs))
	for k, run := range runs {
		start := 0
		if k > 0 {
			start = runs[k-1][1]
		}
		fr = append(fr, [2]int{start, run[1]})
	}
	return fr
}

func sortFieldBounds(fr [][2]int, rs []rune, field int) (int, int) {
	if field <= 0 {
		return 0, len(rs)
	}
	if field <= len(fr) {
		return fr[field-1][0], fr[field-1][1]
	}
	return len(rs), len(rs)
}

func isSortBlankB(b byte) bool { return b == ' ' || b == '\t' }

// sortFieldRangesB is the byte-based twin of sortFieldRanges (blanks and
// separators are ASCII, so byte scanning is UTF-8 safe). It appends to a
// caller-provided scratch buffer so the hot sort path allocates nothing
// per line.
func sortFieldRangesB(into [][2]int, line, sep string) [][2]int {
	into = into[:0]
	if sep != "" {
		sepB := sep[0]
		start := 0
		for i := 0; i < len(line); i++ {
			if line[i] == sepB {
				into = append(into, [2]int{start, i})
				start = i + 1
			}
		}
		return append(into, [2]int{start, len(line)})
	}
	// Fields are [end of previous run (or 0), end of run].
	prevEnd, first := 0, true
	i := 0
	for i < len(line) {
		if isSortBlankB(line[i]) {
			i++
			continue
		}
		j := i
		for j < len(line) && !isSortBlankB(line[j]) {
			j++
		}
		start := 0
		if !first {
			start = prevEnd
		}
		into = append(into, [2]int{start, j})
		prevEnd, first = j, false
		i = j
	}
	if len(into) == 0 {
		return append(into, [2]int{0, len(line)})
	}
	return into
}

func sortFieldBoundsB(fr [][2]int, n, field int) (int, int) {
	if field <= 0 {
		return 0, n
	}
	if field <= len(fr) {
		return fr[field-1][0], fr[field-1][1]
	}
	return n, n
}

// keyRangeB is the byte-based twin of keyRange.
func (sp sortKeySpec) keyRangeB(line string, fr [][2]int, skipGlobal bool) (int, int) {
	skip := sp.skipBlank || skipGlobal
	fs, fe := sortFieldBoundsB(fr, len(line), sp.start.field)
	st := fs
	if skip {
		for st < fe && isSortBlankB(line[st]) {
			st++
		}
	}
	if sp.start.char > 0 {
		st += sp.start.char - 1
	}
	if st > len(line) {
		st = len(line)
	}
	var en int
	switch {
	case !sp.hasEnd:
		en = len(line)
	case sp.end.char > 0:
		efs, efe := sortFieldBoundsB(fr, len(line), sp.end.field)
		if skip {
			for efs < efe && isSortBlankB(line[efs]) {
				efs++
			}
		}
		en = efs + sp.end.char
		if en > len(line) {
			en = len(line)
		}
	default:
		_, en = sortFieldBoundsB(fr, len(line), sp.end.field)
	}
	if en < st {
		en = st
	}
	return st, en
}

// keyRange returns the rune range [start,end) of the key within a line.
func (sp sortKeySpec) keyRange(rs []rune, fr [][2]int, skipGlobal bool) (int, int) {
	skip := sp.skipBlank || skipGlobal
	fs, fe := sortFieldBounds(fr, rs, sp.start.field)
	st := fs
	if skip {
		for st < fe && isSortBlank(rs[st]) {
			st++
		}
	}
	if sp.start.char > 0 {
		st += sp.start.char - 1
	}
	if st > len(rs) {
		st = len(rs)
	}
	var en int
	switch {
	case !sp.hasEnd:
		en = len(rs)
	case sp.end.char > 0:
		efs, efe := sortFieldBounds(fr, rs, sp.end.field)
		if skip {
			for efs < efe && isSortBlank(rs[efs]) {
				efs++
			}
		}
		en = efs + sp.end.char
		if en > len(rs) {
			en = len(rs)
		}
	default:
		_, en = sortFieldBounds(fr, rs, sp.end.field)
	}
	if en < st {
		en = st
	}
	return st, en
}

// sortParseNum extracts GNU's -n numeric prefix: blanks, optional '-',
// digits*, optional '.' digits*. Anything else is zero (GNU -n does not
// accept '+', exponents or 0x; it "aligns decimal points"). The parts
// are substrings of the key: no copying, no per-digit allocation.
func sortParseNum(s string) (bool, string, string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	neg := false
	if i < len(s) && s[i] == '-' {
		neg = true
		i++
	}
	intStart := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intp := s[intStart:i]
	frac := ""
	if i < len(s) && s[i] == '.' {
		i++
		fracStart := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		frac = s[fracStart:i]
	}
	if intp == "" && frac == "" {
		return false, "0", ""
	}
	if intp == "" {
		intp = "0"
	}
	return neg, intp, frac
}

// sortNumCompare compares -n numbers with arbitrary precision.
func sortNumCompare(a, b string) int {
	negA, ia, fa := sortParseNum(a)
	negB, ib, fb := sortParseNum(b)
	ia = strings.TrimLeft(ia, "0")
	ib = strings.TrimLeft(ib, "0")
	fa = strings.TrimRight(fa, "0")
	fb = strings.TrimRight(fb, "0")
	zeroA := ia == "" && fa == ""
	zeroB := ib == "" && fb == ""
	if ia == "" {
		ia = "0"
	}
	if ib == "" {
		ib = "0"
	}
	if zeroA {
		negA = false
	}
	if zeroB {
		negB = false
	}
	if negA != negB {
		if negA {
			return -1
		}
		return 1
	}
	sign := 1
	if negA {
		sign = -1
	}
	mag := 0
	if ia != ib {
		if len(ia) != len(ib) {
			if len(ia) < len(ib) {
				mag = -1
			} else {
				mag = 1
			}
		} else if ia < ib {
			mag = -1
		} else {
			mag = 1
		}
	} else {
		n := len(fa)
		if len(fb) > n {
			n = len(fb)
		}
		fa2 := fa + strings.Repeat("0", n-len(fa))
		fb2 := fb + strings.Repeat("0", n-len(fb))
		if fa2 < fb2 {
			mag = -1
		} else if fa2 > fb2 {
			mag = 1
		}
	}
	return sign * mag
}

// sortParseGeneral extracts a leading C float (with optional exponent)
// for sort -g. Unparseable text is zero, like GNU.
func sortParseGeneral(s string) float64 {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	j := i
	if j < len(s) && (s[j] == '+' || s[j] == '-') {
		j++
	}
	dig := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
		dig++
	}
	if j < len(s) && s[j] == '.' {
		j++
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			dig++
		}
	}
	if dig == 0 {
		return 0
	}
	if j < len(s) && (s[j] == 'e' || s[j] == 'E') {
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
			j = k
		}
	}
	f, _ := strconv.ParseFloat(s[i:j], 64)
	return f
}

// sortParseHuman parses GNU's -h number: a float with an optional
// 1024-power suffix [KMGTPEZY]. Unparseable text is zero.
func sortParseHuman(s string) float64 {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	j := i
	if j < len(s) && (s[j] == '+' || s[j] == '-') {
		j++
	}
	dig := 0
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
		dig++
	}
	if j < len(s) && s[j] == '.' {
		j++
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			dig++
		}
	}
	if dig == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(s[i:j], 64)
	if err != nil || j >= len(s) {
		return f
	}
	mult := 1024.0
	pow := 0
	switch s[j] {
	case 'K', 'k':
		pow = 1
	case 'M', 'm':
		pow = 2
	case 'G', 'g':
		pow = 3
	case 'T', 't':
		pow = 4
	case 'P', 'p':
		pow = 5
	case 'E', 'e':
		pow = 6
	case 'Z', 'z':
		pow = 7
	case 'Y', 'y':
		pow = 8
	default:
		return f
	}
	for n := 0; n < pow; n++ {
		f *= mult
	}
	return f
}

// sortParseMonth maps GNU's -M month names to numbers; unknown is 0.
func sortParseMonth(s string) int {
	s = strings.TrimLeft(s, " \t")
	if len(s) < 3 {
		return 0
	}
	m := strings.ToUpper(s[:3])
	switch m {
	case "JAN":
		return 1
	case "FEB":
		return 2
	case "MAR":
		return 3
	case "APR":
		return 4
	case "MAY":
		return 5
	case "JUN":
		return 6
	case "JUL":
		return 7
	case "AUG":
		return 8
	case "SEP":
		return 9
	case "OCT":
		return 10
	case "NOV":
		return 11
	case "DEC":
		return 12
	}
	return 0
}

// sortKeyOpts are the per-key comparison modes.
const (
	sortModeByte = iota
	sortModeNumeric
	sortModeGeneral
	sortModeHuman
	sortModeMonth
	sortModeVersion
)

type sortConfig struct {
	specs     []sortKeySpec
	sep       string // "" = blank-separated
	mode      int    // global mode when a key has none
	reverse   bool
	fold      bool
	skipBlank bool
	stable    bool
	unique    bool
	dict      bool
	nonprint  bool

	frScr [][2]int // scratch for field ranges (sort is single-threaded)
}

// sortFilterKey applies -d/-i character restrictions to a key.
func sortFilterKey(k string, dict, nonprint bool) string {
	if !dict && !nonprint {
		return k
	}
	var b strings.Builder
	for _, r := range k {
		if dict {
			if !(r == ' ' || r == '\t' || (r >= '0' && r <= '9') ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				continue
			}
		}
		if nonprint && (r < 0x20 || r == 0x7f) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// compareKey compares two key strings under one key's modifiers.
func (sc *sortConfig) compareKey(ka, kb string, sp *sortKeySpec) int {
	ka = sortFilterKey(ka, sp.dict || sc.dict, sp.nonprint || sc.nonprint)
	kb = sortFilterKey(kb, sp.dict || sc.dict, sp.nonprint || sc.nonprint)
	mode := sp.mode
	if mode == sortModeByte {
		mode = sc.mode
	}
	var c int
	switch mode {
	case sortModeNumeric:
		c = sortNumCompare(ka, kb)
	case sortModeGeneral:
		fa, fb := sortParseGeneral(ka), sortParseGeneral(kb)
		if fa < fb {
			c = -1
		} else if fa > fb {
			c = 1
		}
	case sortModeHuman:
		fa, fb := sortParseHuman(ka), sortParseHuman(kb)
		if fa < fb {
			c = -1
		} else if fa > fb {
			c = 1
		}
	case sortModeMonth:
		ma, mb := sortParseMonth(ka), sortParseMonth(kb)
		if ma < mb {
			c = -1
		} else if ma > mb {
			c = 1
		}
	case sortModeVersion:
		c = compareVersion(ka, kb)
	default:
		if sp.fold || sc.fold {
			c = strings.Compare(strings.ToLower(ka), strings.ToLower(kb))
		} else {
			c = strings.Compare(ka, kb)
		}
	}
	if sp.reverse != sc.reverse {
		c = -c
	}
	return c
}

// compare applies every key in order, then GNU's last-resort whole-line
// comparison (reversed by global -r only).
func (sc *sortConfig) compare(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	fa := sortFieldRanges(ra, sc.sep)
	fb := sortFieldRanges(rb, sc.sep)
	for i := range sc.specs {
		sp := &sc.specs[i]
		as, ae := sp.keyRange(ra, fa, sc.skipBlank)
		bs, be := sp.keyRange(rb, fb, sc.skipBlank)
		c := sc.compareKey(string(ra[as:ae]), string(rb[bs:be]), sp)
		if c != 0 {
			return c
		}
	}
	if sc.stable || sc.unique {
		return 0
	}
	c := strings.Compare(a, b)
	if sc.reverse {
		c = -c
	}
	return c
}

// keysEqual reports whether two lines have equal sort keys (no last
// resort) — GNU's notion of "equal" for -u dedup and -cu checking.
func (sc *sortConfig) keysEqual(a, b string) bool {
	ra, rb := []rune(a), []rune(b)
	fa := sortFieldRanges(ra, sc.sep)
	fb := sortFieldRanges(rb, sc.sep)
	for i := range sc.specs {
		sp := &sc.specs[i]
		as, ae := sp.keyRange(ra, fa, sc.skipBlank)
		bs, be := sp.keyRange(rb, fb, sc.skipBlank)
		if sc.compareKey(string(ra[as:ae]), string(rb[bs:be]), sp) != 0 {
			return false
		}
	}
	return true
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

func cmdUniq(ctx context.Context, hc interp.HandlerContext, args []string) error {
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
	wSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "w" {
			wSet = true
		}
	})
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
	bw := getBufWriter64K(w)
	defer func() {
		_ = bw.Flush()
		putBufWriter64K(bw)
	}()
	w = bw

	// Fast byte-streaming path for whole-line case-sensitive uniq (including
	// -c, -d, -u): zero per-line string allocations.
	if *skipFields == 0 && *skipChars == 0 && *checkChars == 0 && !wSet && !*ignoreCase && groupMode == "" {
		var prev []byte
		n := 0
		var numBuf [32]byte
		emitPrev := func() {
			if n == 0 {
				return
			}
			if (*dupOnly && n < 2) || (*uniqueOnly && n > 1) {
				return
			}
			if *count {
				digits := strconv.AppendInt(numBuf[:0], int64(n), 10)
				for pad := len(digits); pad < 7; pad++ {
					bw.WriteByte(' ')
				}
				bw.Write(digits)
				bw.WriteByte(' ')
			}
			bw.Write(prev)
			bw.WriteByte('\n')
		}
		for _, r := range readers {
			var writeErr error
			rerr := streamLinesChunked(ctx, r, func(line []byte, _ bool) bool {
				if ctx.Err() != nil {
					return false
				}
				if n > 0 && bytes.Equal(line, prev) {
					n++
					return true
				}
				emitPrev()
				if bw.Available() < 4096 {
					if err := bw.Flush(); err != nil {
						writeErr = err
						return false
					}
				}
				prev = append(prev[:0], line...)
				n = 1
				return true
			}, func() error {
				if bw.Buffered() > 0 {
					return bw.Flush()
				}
				return nil
			})
			if writeErr != nil {
				return writeErr
			}
			if rerr != nil {
				if isPipeClosed(rerr) || ctx.Err() != nil {
					return rerr
				}
				fmt.Fprintln(hc.Stderr, "uniq:", rerr)
				return exitError{1}
			}
		}
		emitPrev()
		return bw.Flush()
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
		if *checkChars > 0 || wSet {
			runes := []rune(k)
			if len(runes) > int(*checkChars) {
				runes = runes[:*checkChars]
			}
			k = string(runes)
		}
		if *ignoreCase {
			return strings.ToLower(k)
		}
		return k
	}
	var prev, prevRaw string
	n := 0
	firstGroup := true
	flush := func(forceFlush bool) error {
		if n == 0 {
			return nil
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
				io.WriteString(w, "\n")
			}
			firstGroup = false
			for i := 0; i < n; i++ {
				io.WriteString(w, prevRaw)
				io.WriteString(w, "\n")
			}
			if (forceFlush && bw.Buffered() > 0) || bw.Available() < 4096 {
				return bw.Flush()
			}
			return nil
		}
		if show {
			if *count {
				fmt.Fprintf(w, "%7d %s\n", n, prevRaw)
			} else {
				io.WriteString(w, prevRaw)
				io.WriteString(w, "\n")
			}
			if (forceFlush && bw.Buffered() > 0) || bw.Available() < 4096 {
				return bw.Flush()
			}
		}
		return nil
	}
	for _, r := range readers {
		sc := newLineReader(r)
		for sc.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			line := sc.Text()
			key := norm(line)
			if n == 0 || key != prev {
				if err := flush(sc.br.Buffered() == 0); err != nil {
					return err
				}
				prev, prevRaw, n = key, line, 1
			} else {
				n++
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintln(hc.Stderr, "uniq:", err)
			return exitError{1}
		}
	}
	if err := flush(true); err != nil {
		return err
	}
	return bw.Flush()
}

var wcWordTbl = func() [256]uint8 {
	var t [256]uint8
	for i := 0; i < 128; i++ {
		switch byte(i) {
		case ' ', '\t', '\n', '\r', '\v', '\f':
		default:
			t[i] = 1
		}
	}
	return t
}()

func cmdWc(ctx context.Context, hc interp.HandlerContext, args []string) error {
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
	// Regularity per input: inspect open file handles directly without
	// redundant path-based os.Stat syscalls.
	isReg := make([]bool, len(readers))
	regInfo := make([]os.FileInfo, len(readers))
	isFileArg := make([]bool, len(readers))
	for k := range readers {
		if len(files) > 0 && names[k] != "" && names[k] != "standard input" && names[k] != "-" {
			isFileArg[k] = true
			if fi, ok := readerRegularStat(readers[k]); ok {
				isReg[k] = true
				regInfo[k] = fi
			} else if fi, err := os.Stat(resolve(hc.Dir, files[k])); err == nil && fi.Mode().IsRegular() {
				isReg[k] = true
				regInfo[k] = fi
			}
		} else if fi, ok := readerRegularStat(hc.Stdin); ok {
			isReg[k] = true
			regInfo[k] = fi
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
	wantLines, wantWords, wantChars, wantMax := *lines, *words, *chars, *maxLine
	bytesOnly := *bytes_ && !wantLines && !wantWords && !wantChars && !wantMax

	bp := bufPool64K.Get().(*[]byte)
	rawBuf := *bp // len == 64*1024 + 8; [0:4] reserved for UTF-8 chunk carry
	defer bufPool64K.Put(bp)

	for i, r := range readers {
		l, w, b := 0, 0, 0
		nchars, maxLineLen := 0, 0

		// Fast Path 1: `wc -c` on a non-empty regular file needs zero Read syscalls.
		if bytesOnly && isReg[i] && regInfo[i] != nil && regInfo[i].Size() > 0 {
			if isFileArg[i] {
				b = int(regInfo[i].Size())
				goto recordRow
			}
			if sk, ok := r.(io.Seeker); ok {
				cur, err1 := sk.Seek(0, io.SeekCurrent)
				end, err2 := sk.Seek(0, io.SeekEnd)
				if err1 == nil && err2 == nil && end >= cur {
					b = int(end - cur)
					goto recordRow
				}
			}
		}

		// Fast Path 2: `-l` and/or `-c` without `-w`/`-m`/`-L`: 64 KB chunks + SIMD bytes.Count.
		if !wantWords && !wantChars && !wantMax {
			buf := rawBuf[:64*1024]
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				nr, rerr := r.Read(buf)
				if nr > 0 {
					b += nr
					if wantLines {
						l += bytes.Count(buf[:nr], nlBytes)
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					fmt.Fprintln(hc.Stderr, "wc:", rerr)
					return exitError{1}
				}
			}
			goto recordRow
		}

		// Fast Path 3: `-w` (with optional `-l`/`-c`, without `-m`/`-L`):
		// branchless 8-byte ASCII scanner + SIMD bytes.Count, falling back
		// to UTF-8 rune decode only if byte >= 0x80 is encountered.
		if wantWords && !wantChars && !wantMax {
			readSlice := rawBuf[4 : 4+64*1024]
			var inWord uint8
			var utf8Carry [4]byte
			carryLen := 0
			scanUTF8Words := func(work []byte) {
				p := 0
				for p < len(work) {
					c := work[p]
					if c < 0x80 {
						cur := wcWordTbl[c]
						w += int(cur &^ inWord)
						inWord = cur
						p++
						continue
					}
					rn, sz := utf8.DecodeRune(work[p:])
					if rn != utf8.RuneError && rn != '\u00a0' && unicode.IsSpace(rn) {
						inWord = 0
					} else {
						w += int(1 &^ inWord)
						inWord = 1
					}
					p += sz
				}
			}
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				nr, rerr := r.Read(readSlice)
				if nr > 0 {
					b += nr
					chunk := readSlice[:nr]
					if wantLines {
						l += bytes.Count(chunk, nlBytes)
					}
					j := 0
					if carryLen == 0 {
						for ; j+8 <= nr; j += 8 {
							u := binary.LittleEndian.Uint64(chunk[j:])
							if u&0x8080808080808080 != 0 {
								break
							}
							c0 := wcWordTbl[byte(u)]
							w += int(c0 &^ inWord)
							c1 := wcWordTbl[byte(u>>8)]
							w += int(c1 &^ c0)
							c2 := wcWordTbl[byte(u>>16)]
							w += int(c2 &^ c1)
							c3 := wcWordTbl[byte(u>>24)]
							w += int(c3 &^ c2)
							c4 := wcWordTbl[byte(u>>32)]
							w += int(c4 &^ c3)
							c5 := wcWordTbl[byte(u>>40)]
							w += int(c5 &^ c4)
							c6 := wcWordTbl[byte(u>>48)]
							w += int(c6 &^ c5)
							c7 := wcWordTbl[byte(u>>56)]
							w += int(c7 &^ c6)
							inWord = c7
						}
						for ; j < nr && chunk[j] < 0x80; j++ {
							cur := wcWordTbl[chunk[j]]
							w += int(cur &^ inWord)
							inWord = cur
						}
					}
					if j < nr || carryLen > 0 {
						var work []byte
						if carryLen > 0 {
							copy(rawBuf[4-carryLen:4], utf8Carry[:carryLen])
							work = rawBuf[4-carryLen : 4+nr]
						} else {
							work = chunk[j:]
						}
						carryLen = 0
						if rerr == nil && len(work) > 0 {
							k := len(work) - 1
							limit := max(0, len(work)-3)
							for k > limit && (work[k]&0xC0) == 0x80 {
								k--
							}
							if work[k] >= 0x80 && !utf8.FullRune(work[k:]) {
								carryLen = copy(utf8Carry[:], work[k:])
								work = work[:k]
							}
						}
						scanUTF8Words(work)
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					fmt.Fprintln(hc.Stderr, "wc:", rerr)
					return exitError{1}
				}
			}
			if carryLen > 0 {
				scanUTF8Words(utf8Carry[:carryLen])
			}
			goto recordRow
		}

		// Path 4: streaming 64 KB chunk scanner for `-m` and/or `-L` (with UTF-8 carry).
		{
			readSlice := rawBuf[4 : 4+64*1024]
			inWord := false
			col := 0
			var utf8Carry [4]byte
			carryLen := 0
			scanFull := func(work []byte) {
				p := 0
				for p < len(work) {
					c := work[p]
					if c == '\n' {
						l++
						if wantChars {
							nchars++
						}
						if col > maxLineLen {
							maxLineLen = col
						}
						col = 0
						inWord = false
						p++
						continue
					}
					if c < 0x80 {
						if wantChars {
							nchars++
						}
						if wantWords {
							if wcWordTbl[c] == 0 {
								inWord = false
							} else if !inWord {
								inWord = true
								w++
							}
						}
						if wantMax {
							if c == '\t' {
								col = (col/8 + 1) * 8
							} else {
								col++
							}
						}
						p++
						continue
					}
					rn, sz := utf8.DecodeRune(work[p:])
					if wantChars {
						nchars++
					}
					if wantWords {
						if rn != utf8.RuneError && rn != '\u00a0' && unicode.IsSpace(rn) {
							inWord = false
						} else if !inWord {
							inWord = true
							w++
						}
					}
					if wantMax {
						for q := p; q < p+sz; q++ {
							if work[q]&0xC0 != 0x80 {
								col++
							}
						}
					}
					p += sz
				}
			}
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				nr, rerr := r.Read(readSlice)
				if nr > 0 {
					b += nr
					copy(rawBuf[4-carryLen:4], utf8Carry[:carryLen])
					work := rawBuf[4-carryLen : 4+nr]
					carryLen = 0
					if rerr == nil && len(work) > 0 {
						k := len(work) - 1
						limit := max(0, len(work)-3)
						for k > limit && (work[k]&0xC0) == 0x80 {
							k--
						}
						if work[k] >= 0x80 && !utf8.FullRune(work[k:]) {
							carryLen = copy(utf8Carry[:], work[k:])
							work = work[:k]
						}
					}
					scanFull(work)
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					fmt.Fprintln(hc.Stderr, "wc:", rerr)
					return exitError{1}
				}
			}
			if carryLen > 0 {
				scanFull(utf8Carry[:carryLen])
			}
			if col > maxLineLen {
				maxLineLen = col
			}
		}

	recordRow:
		tl, tw, tb, tc, tm = tl+l, tw+w, tb+b, tc+nchars, max(maxLineLen, tm)
		name := ""
		if len(files) > 0 && names[i] != "" && names[i] != "standard input" {
			name = names[i]
		} else if len(files) > 0 && names[i] == "standard input" {
			// Explicit "-" operand (alone or mixed with files) is
			// displayed as "-" like GNU.
			name = "-"
		}
		rows = append(rows, wcRow{colVals(l, w, b, nchars, maxLineLen), name})
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
	multi := len(rows) > 1
	// GNU wc pads the columns when listing several inputs, or when a
	// non-regular input is listed across several columns.
	ncols := 1
	if len(rows) > 0 {
		ncols = len(rows[0].vals)
	}
	pad := multi || (anyPipe && ncols > 1)
	for _, r := range rows {
		line := ""
		if !pad {
			strs := make([]string, len(r.vals))
			for i, v := range r.vals {
				strs[i] = strconv.Itoa(v)
			}
			line = strings.Join(strs, " ")
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

// readerRegularStat inspects an open reader's Stat() (supporting both *os.File
// and *refcountedFileShim) and returns its FileInfo if it is a regular file.
func readerRegularStat(r io.Reader) (os.FileInfo, bool) {
	if st, ok := r.(interface{ Stat() (os.FileInfo, error) }); ok {
		if fi, err := st.Stat(); err == nil && fi.Mode().IsRegular() {
			return fi, true
		}
	}
	return nil, false
}

// stdinIsRegular reports whether the wc input reader is a regular file
// (stdin redirected from a file) as opposed to a pipe. GNU wc pads
// columns to width 7 for pipes but uses natural width for regular files.
func stdinIsRegular(r io.Reader) bool {
	_, ok := readerRegularStat(r)
	return ok
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
		// /dev/null is a POSIX device that does not exist on Windows:
		// resolve() would turn it into a relative path. Map it to NUL
		// (and drop O_TRUNC, which NUL rejects), like the shell's own
		// open handler does for redirections.
		path := resolve(hc.Dir, f)
		if runtime.GOOS == "windows" && (f == "/dev/null" || path == "/dev/null") {
			path = "NUL"
			fl &^= os.O_TRUNC
		}
		fh, err := os.OpenFile(path, fl, 0644)
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

func cmdTr(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("tr", hc.Stderr)
	del := fs.Bool("d", false, "")
	squeeze := fs.Bool("s", false, "")
	complement := fs.Bool("c", false, "")
	truncate := fs.Bool("t", false, "")
	fs.BoolVar(truncate, "truncate-set1", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(hc.Stderr, "tr: missing operand")
		return flag.ErrHelp
	}
	from, err := expandTrSet(rest[0])
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tr:", err)
		return exitError{1}
	}
	bp := bufPool64K.Get().(*[]byte)
	buf := (*bp)[:64*1024]
	defer bufPool64K.Put(bp)

	if *complement && !*del && len(rest) < 2 {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, rerr := hc.Stdin.Read(buf)
			if n > 0 {
				if _, werr := hc.Stdout.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if rerr == io.EOF {
				return nil
			}
			if rerr != nil {
				return rerr
			}
		}
	}
	if *complement {
		var present [256]bool
		for _, b := range from {
			present[b] = true
		}
		from = from[:0]
		for i := 0; i < 256; i++ {
			if !present[i] {
				from = append(from, byte(i))
			}
		}
	}
	if *del {
		var delSet [256]bool
		for _, b := range from {
			delSet[b] = true
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, err := hc.Stdin.Read(buf)
			if n > 0 {
				k := 0
				for _, b := range buf[:n] {
					if !delSet[b] {
						buf[k] = b
						k++
					}
				}
				if k > 0 {
					if _, werr := hc.Stdout.Write(buf[:k]); werr != nil {
						return werr
					}
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	if len(rest) < 2 {
		if *squeeze {
			var inSet [256]bool
			for _, b := range from {
				inSet[b] = true
			}
			var carry byte
			hasCarry := false
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				n, err := hc.Stdin.Read(buf)
				if n > 0 {
					chunk := squeezeBytesInPlace(buf[:n], &inSet, &carry, &hasCarry)
					if len(chunk) > 0 {
						if _, werr := hc.Stdout.Write(chunk); werr != nil {
							return werr
						}
					}
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
			}
			return nil
		}
		fmt.Fprintln(hc.Stderr, "tr: missing operand after "+rest[0])
		return flag.ErrHelp
	}
	to, err := expandTrSet(rest[1])
	if err != nil {
		fmt.Fprintln(hc.Stderr, "tr:", err)
		return exitError{1}
	}
	if !*truncate && len(to) == 0 && len(from) > 0 {
		fmt.Fprintln(hc.Stderr, "tr: when not truncating set1, string2 must be non-empty")
		return exitError{1}
	}
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
	var sqSet [256]bool
	if *squeeze {
		for _, b := range to {
			sqSet[b] = true
		}
	}
	// Stream in pooled 64 KB chunks with in-place 8x unrolled lookup table.
	var carry byte
	hasCarry := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := hc.Stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			i := 0
			for ; i+8 <= n; i += 8 {
				s := chunk[i : i+8 : i+8]
				s[0] = table[s[0]]
				s[1] = table[s[1]]
				s[2] = table[s[2]]
				s[3] = table[s[3]]
				s[4] = table[s[4]]
				s[5] = table[s[5]]
				s[6] = table[s[6]]
				s[7] = table[s[7]]
			}
			for ; i < n; i++ {
				chunk[i] = table[chunk[i]]
			}
			if *squeeze {
				chunk = squeezeBytesInPlace(chunk, &sqSet, &carry, &hasCarry)
			}
			if len(chunk) > 0 {
				if _, werr := hc.Stdout.Write(chunk); werr != nil {
					return werr
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func squeezeBytesInPlace(data []byte, inSet *[256]bool, carry *byte, hasCarry *bool) []byte {
	k := 0
	for _, b := range data {
		if *hasCarry && b == *carry && inSet[b] {
			continue
		}
		data[k] = b
		k++
		*carry, *hasCarry = b, true
	}
	return data[:k]
}

func nextTrAtom(s string, i int) (byte, int, bool) {
	if i >= len(s) {
		return 0, i, false
	}
	if s[i] == '[' && strings.HasPrefix(s[i:], "[:") {
		if strings.Index(s[i:], ":]") >= 0 {
			return 0, i, false
		}
	}
	if s[i] == '\\' && i+1 < len(s) {
		if c := s[i+1]; c >= '0' && c <= '7' {
			k, val := i+1, 0
			for k < len(s) && k < i+4 && s[k] >= '0' && s[k] <= '7' {
				val = val*8 + int(s[k]-'0')
				k++
			}
			return byte(val & 0xFF), k, true
		}
		var b byte
		switch s[i+1] {
		case 'a':
			b = '\a'
		case 'b':
			b = '\b'
		case 'v':
			b = '\v'
		case 'f':
			b = '\f'
		case 'n':
			b = '\n'
		case 't':
			b = '\t'
		case 'r':
			b = '\r'
		case '\\':
			b = '\\'
		default:
			b = s[i+1]
		}
		return b, i + 2, true
	}
	return s[i], i + 1, true
}

func expandTrSet(s string) ([]byte, error) {
	var out []byte
	i := 0
	for i < len(s) {
		if s[i] == '[' && strings.HasPrefix(s[i:], "[:") {
			if e := strings.Index(s[i:], ":]"); e >= 0 {
				out = append(out, expandTrClass(s[i:i+e+2])...)
				i += e + 2
				continue
			}
		}
		lo, next1, ok := nextTrAtom(s, i)
		if !ok {
			out = append(out, s[i])
			i++
			continue
		}
		if next1 < len(s) && s[next1] == '-' && next1+1 < len(s) {
			if hi, next2, ok2 := nextTrAtom(s, next1+1); ok2 {
				if lo > hi {
					return nil, fmt.Errorf("range-endpoints of '%s' are in reverse collating sequence order", s[i:next2])
				}
				for c := int(lo); c <= int(hi); c++ {
					out = append(out, byte(c))
				}
				i = next2
				continue
			}
		}
		out = append(out, lo)
		i = next1
	}
	return out, nil
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

func cmdSeq(ctx context.Context, hc interp.HandlerContext, args []string) error {
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
	parseSeqInt := func(s string) (int64, bool) {
		s = strings.TrimSpace(s)
		if s == "" || strings.ContainsAny(s, ".eExX") {
			return 0, false
		}
		v, err := strconv.ParseInt(s, 10, 64)
		return v, err == nil
	}
	if format == "" {
		iv, istep, ilast := int64(1), int64(1), int64(0)
		intOK := false
		switch len(rest) {
		case 1:
			ilast, intOK = parseSeqInt(rest[0])
		case 2:
			var ok1, ok2 bool
			iv, ok1 = parseSeqInt(rest[0])
			ilast, ok2 = parseSeqInt(rest[1])
			intOK = ok1 && ok2
		case 3:
			var ok1, ok2, ok3 bool
			iv, ok1 = parseSeqInt(rest[0])
			istep, ok2 = parseSeqInt(rest[1])
			ilast, ok3 = parseSeqInt(rest[2])
			intOK = ok1 && ok2 && ok3
		}
		if intOK && istep != 0 {
			if (istep > 0 && iv > ilast) || (istep < 0 && iv < ilast) {
				return nil
			}
			bp := bufPool64K.Get().(*[]byte)
			raw := (*bp)[:64*1024]
			defer bufPool64K.Put(bp)
			outBuf := raw[:0]

			if !padWidth && sep == "\n" {
				for {
					outBuf = strconv.AppendInt(outBuf, iv, 10)
					outBuf = append(outBuf, '\n')
					if len(outBuf) >= 60*1024 {
						if err := ctx.Err(); err != nil {
							return err
						}
						if _, err := hc.Stdout.Write(outBuf); err != nil {
							return err
						}
						outBuf = outBuf[:0]
					}
					if istep > 0 {
						if iv > 0 && istep > math.MaxInt64-iv {
							break
						}
						iv += istep
						if iv > ilast {
							break
						}
					} else {
						if iv < 0 && istep < math.MinInt64-iv {
							break
						}
						iv += istep
						if iv < ilast {
							break
						}
					}
				}
				if len(outBuf) > 0 {
					if _, err := hc.Stdout.Write(outBuf); err != nil {
						return err
					}
				}
				return nil
			}

			padW := 0
			if padWidth {
				var endVal int64
				if istep > 0 {
					diff := uint64(ilast) - uint64(iv)
					endVal = int64(uint64(ilast) - diff%uint64(istep))
				} else {
					diff := uint64(iv) - uint64(ilast)
					endVal = int64(uint64(ilast) + diff%(-uint64(istep)))
				}
				padW = max(len(strconv.FormatInt(iv, 10)), len(strconv.FormatInt(endVal, 10)))
			}
			var numBuf [32]byte
			firstOut := true
			for {
				if (istep > 0 && iv > ilast) || (istep < 0 && iv < ilast) {
					break
				}
				need := len(sep) + max(padW, 24) + 2
				if len(outBuf)+need > len(raw) && len(outBuf) > 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
					if _, err := hc.Stdout.Write(outBuf); err != nil {
						return err
					}
					outBuf = outBuf[:0]
				}
				if !firstOut && sep != "\n" {
					outBuf = append(outBuf, sep...)
				}
				firstOut = false
				nb := strconv.AppendInt(numBuf[:0], iv, 10)
				if padW > len(nb) {
					if nb[0] == '-' {
						outBuf = append(outBuf, '-')
						for z := 0; z < padW-len(nb); z++ {
							outBuf = append(outBuf, '0')
						}
						outBuf = append(outBuf, nb[1:]...)
					} else {
						for z := 0; z < padW-len(nb); z++ {
							outBuf = append(outBuf, '0')
						}
						outBuf = append(outBuf, nb...)
					}
				} else {
					outBuf = append(outBuf, nb...)
				}
				if sep == "\n" {
					outBuf = append(outBuf, '\n')
				}
				if istep > 0 && iv > 0 && istep > math.MaxInt64-iv {
					break
				}
				if istep < 0 && iv < 0 && istep < math.MinInt64-iv {
					break
				}
				iv += istep
			}
			if !firstOut && sep != "\n" {
				outBuf = append(outBuf, '\n')
			}
			if len(outBuf) > 0 {
				if _, err := hc.Stdout.Write(outBuf); err != nil {
					return err
				}
			}
			return nil
		}
	}
	bw := getBufWriter64K(hc.Stdout)
	defer func() {
		_ = bw.Flush()
		putBufWriter64K(bw)
	}()
	steps := 0
	checkStep := func() error {
		steps++
		if steps&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if bw.Available() < 4096 {
			if err := bw.Flush(); err != nil {
				return err
			}
		}
		return nil
	}
	fmtNum := func(v float64) string {
		if format != "" {
			return sprintfSeq(format, v)
		}
		if decimals > 0 {
			return strconv.FormatFloat(v, 'f', decimals, 64)
		}
		if v >= -9e18 && v <= 9e18 && v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	inBounds := func(v float64) bool {
		return (step > 0 && v <= last+1e-12) || (step < 0 && v >= last-1e-12)
	}
	if !inBounds(first) {
		return nil
	}
	padW := 0
	if padWidth && format == "" {
		padW = len(fmtNum(first))
		estSteps := math.Floor((last - first) / step)
		if estSteps < 0 || math.IsInf(estSteps, 0) || math.IsNaN(estSteps) {
			estSteps = 0
		}
		vEnd := first + estSteps*step
		for adj := 0; adj < 8 && inBounds(vEnd+step) && vEnd+step != vEnd; adj++ {
			estSteps++
			vEnd = first + estSteps*step
		}
		for adj := 0; adj < 8 && estSteps > 0 && !inBounds(vEnd); adj++ {
			estSteps--
			vEnd = first + estSteps*step
		}
		if w2 := len(fmtNum(vEnd)); w2 > padW {
			padW = w2
		}
	}
	padStr := func(s string) string {
		if padW == 0 || len(s) >= padW {
			return s
		}
		if strings.HasPrefix(s, "-") {
			return "-" + strings.Repeat("0", padW-len(s)) + s[1:]
		}
		return strings.Repeat("0", padW-len(s)) + s
	}
	firstOut := true
	var prevV float64
	for k := uint64(0); ; k++ {
		v := first + float64(k)*step
		if k > 0 && v == prevV {
			break
		}
		prevV = v
		if !inBounds(v) {
			break
		}
		if !firstOut && sep != "\n" {
			bw.WriteString(sep)
		}
		firstOut = false
		bw.WriteString(padStr(fmtNum(v)))
		if sep == "\n" {
			bw.WriteByte('\n')
		}
		if err := checkStep(); err != nil {
			return err
		}
	}
	if !firstOut && sep != "\n" {
		bw.WriteByte('\n')
	}
	return bw.Flush()
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

func cmdCut(ctx context.Context, hc interp.HandlerContext, args []string) error {
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
	// GNU cut validates the operand shapes up front.
	dGiven := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "d" {
			dGiven = true
		}
	})
	lists := 0
	for _, s := range []string{*bytes_, *chars, *fields} {
		if s != "" {
			lists++
		}
	}
	if lists > 1 {
		fmt.Fprintln(hc.Stderr, "cut: only one list may be specified")
		fmt.Fprintln(hc.Stderr, "Try 'cut --help' for more information.")
		return exitError{1}
	}
	if dGiven && len([]rune(*delim)) != 1 {
		fmt.Fprintln(hc.Stderr, "cut: the delimiter must be a single character")
		fmt.Fprintln(hc.Stderr, "Try 'cut --help' for more information.")
		return exitError{1}
	}
	// -b (bytes) behaves like -c for UTF-8-unaware output; GNU differs
	// only on multibyte chars, where byte slicing can split runes.
	if *bytes_ != "" && *chars == "" {
		*chars = *bytes_
	}
	if *chars != "" {
		return cmdCutChars(ctx, hc, *chars, fs.Args(), *complement)
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
	bw := getBufWriter64K(hc.Stdout)
	defer func() {
		_ = bw.Flush()
		putBufWriter64K(bw)
	}()
	if len(d) == 1 {
		// Fast SIMD byte-level path for single-byte delimiters using bytes.IndexByte
		// with early exit past maxField when bounded.
		delimB := d[0]
		joinerB := joiner
		maxField := 0
		unbounded := *complement
		if !unbounded {
			for _, s := range spans {
				if s.hi == -1 {
					unbounded = true
					break
				}
				if s.hi > maxField {
					maxField = s.hi
				}
			}
		}
		var fastMask [65]bool
		useFastMask := !unbounded && maxField <= 64
		if useFastMask {
			for f := 1; f <= maxField; f++ {
				fastMask[f] = in(f)
			}
		}
		for _, r := range readers {
			var writeErr error
			err := streamLinesChunked(ctx, r, func(line []byte, _ bool) bool {
				if ctx.Err() != nil {
					return false
				}
				firstIdx := bytes.IndexByte(line, delimB)
				if firstIdx < 0 {
					if !*onlyDelim {
						bw.Write(line)
						bw.WriteByte('\n')
						if bw.Available() < 4096 {
							if werr := bw.Flush(); werr != nil {
								writeErr = werr
								return false
							}
						}
					}
					return true
				}
				wrote := false
				// Field 1 is already known from firstIdx!
				var sel1 bool
				if useFastMask {
					sel1 = fastMask[1]
				} else {
					sel1 = in(1)
				}
				if sel1 {
					bw.Write(line[:firstIdx])
					wrote = true
				}
				if !useFastMask || maxField > 1 {
					fstart := firstIdx + 1
					fs2 := 1
					for {
						fs2++
						j := bytes.IndexByte(line[fstart:], delimB)
						fend := len(line)
						if j >= 0 {
							fend = fstart + j
						}
						var sel bool
						if useFastMask {
							sel = fs2 <= maxField && fastMask[fs2]
						} else {
							sel = in(fs2)
						}
						if sel {
							if wrote {
								bw.WriteString(joinerB)
							}
							bw.Write(line[fstart:fend])
							wrote = true
						}
						if j < 0 || (!unbounded && fs2 >= maxField) {
							break
						}
						fstart = fend + 1
					}
				}
				bw.WriteByte('\n')
				if bw.Available() < 4096 {
					if werr := bw.Flush(); werr != nil {
						writeErr = werr
						return false
					}
				}
				return true
			}, func() error {
				if bw.Buffered() > 0 {
					return bw.Flush()
				}
				return nil
			})
			if writeErr != nil {
				err = writeErr
			}
			if err != nil {
				if isPipeClosed(err) || ctx.Err() != nil {
					return err
				}
				fmt.Fprintln(hc.Stderr, "cut:", err)
				return exitError{1}
			}
		}
		return bw.Flush()
	}
	for _, r := range readers {
		sc := newLineReader(r)
		for sc.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			line := sc.Text()
			if !strings.Contains(line, d) {
				if !*onlyDelim {
					if _, werr := fmt.Fprintln(hc.Stdout, line); werr != nil {
						return werr
					}
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
			if _, werr := fmt.Fprintln(hc.Stdout, strings.Join(out, joiner)); werr != nil {
				return werr
			}
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
			if ab[0] == "" && ab[1] == "" {
				return nil, fmt.Errorf("invalid range with no endpoint: -")
			}
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
			if lo < 1 || (hi != -1 && hi < 1) {
				return nil, fmt.Errorf("fields and positions are numbered from 1")
			}
			if hi != -1 && hi < lo {
				return nil, fmt.Errorf("invalid decreasing range")
			}
			spans = append(spans, fieldSpan{lo, hi})
		} else {
			v, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("invalid field: %s", part)
			}
			if v < 1 {
				return nil, fmt.Errorf("fields and positions are numbered from 1")
			}
			spans = append(spans, fieldSpan{v, v})
		}
	}
	if len(spans) == 0 {
		return nil, fmt.Errorf("invalid field list")
	}
	return spans, nil
}

func cmdCutChars(ctx context.Context, hc interp.HandlerContext, list string, files []string, complement bool) error {
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
	bw := bufio.NewWriterSize(hc.Stdout, 256*1024)
	defer bw.Flush()
	out := make([]byte, 0, 256)
	for _, r := range readers {
		var writeErr error
		err := streamLinesChunked(ctx, r, func(line []byte, terminated bool) bool {
			if ctx.Err() != nil {
				return false
			}
			out = out[:0]
			for j := 0; j < len(line); j++ {
				if in(j + 1) {
					out = append(out, line[j])
				}
			}
			bw.Write(out)
			bw.WriteByte('\n')
			if bw.Buffered() >= 4096 {
				if werr := bw.Flush(); werr != nil {
					writeErr = werr
					return false
				}
			}
			return true
		}, func() error {
			if bw.Buffered() > 0 {
				return bw.Flush()
			}
			return nil
		})
		if writeErr != nil {
			err = writeErr
		}
		if err != nil {
			if isPipeClosed(err) || ctx.Err() != nil {
				return err
			}
			fmt.Fprintln(hc.Stderr, "cut:", err)
			return exitError{1}
		}
	}
	return bw.Flush()
}

func cmdPaste(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("paste", hc.Stderr)
	delims := fs.String("d", "\t", "")
	serial := fs.Bool("s", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	files := fs.Args()
	d := []rune(*delims)
	if len(d) == 0 {
		d = []rune{'\t'}
	}
	if len(files) == 0 {
		sc := newLineReader(hc.Stdin)
		if *serial {
			bw := bufio.NewWriterSize(hc.Stdout, 64*1024)
			first := true
			for sc.Scan() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !first {
					bw.WriteRune(d[0])
				}
				first = false
				bw.WriteString(sc.Text())
				if bw.Buffered() >= 4096 {
					if err := bw.Flush(); err != nil {
						return err
					}
				}
			}
			if err := sc.Err(); err != nil {
				return err
			}
			bw.WriteByte('\n')
			return bw.Flush()
		}
		for sc.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(hc.Stdout, sc.Text()); err != nil {
				return err
			}
		}
		return sc.Err()
	}
	// GNU: repeated "-" operands read SUCCESSIVE lines from the same
	// stdin stream. Share a single lineReader across "-" operands so
	// infinite streams (e.g. `yes | paste - - | head -n 2`) stream in O(1) memory.
	var openFiles []io.Closer
	defer func() {
		for _, f := range openFiles {
			f.Close()
		}
	}()
	var srcs []*lineReader
	var stdinSC *lineReader
	for _, f := range files {
		if f == "-" || f == "/dev/stdin" {
			if stdinSC == nil {
				stdinSC = newLineReader(hc.Stdin)
			}
			srcs = append(srcs, stdinSC)
			continue
		}
		fh, err := openShellFile(resolve(hc.Dir, f))
		if err != nil {
			fmt.Fprintln(hc.Stderr, "paste:", err)
			return exitError{1}
		}
		openFiles = append(openFiles, fh)
		srcs = append(srcs, newLineReader(fh))
	}
	if *serial {
		bw := bufio.NewWriterSize(hc.Stdout, 64*1024)
		for i, sc := range srcs {
			first := true
			for sc.Scan() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !first {
					bw.WriteRune(d[i%len(d)])
				}
				first = false
				bw.WriteString(sc.Text())
				if bw.Buffered() >= 4096 {
					if err := bw.Flush(); err != nil {
						return err
					}
				}
			}
			if err := sc.Err(); err != nil {
				fmt.Fprintln(hc.Stderr, "paste:", err)
				return exitError{1}
			}
			bw.WriteByte('\n')
			if err := bw.Flush(); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var parts []string
		any := false
		for _, sc := range srcs {
			if sc.Scan() {
				parts = append(parts, sc.Text())
				any = true
			} else {
				if err := sc.Err(); err != nil {
					fmt.Fprintln(hc.Stderr, "paste:", err)
					return exitError{1}
				}
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
		if _, err := fmt.Fprintln(hc.Stdout, sb.String()); err != nil {
			return err
		}
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

func cmdDiff(ctx context.Context, hc interp.HandlerContext, args []string) error {
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
	p := 0
	for p < n && p < m && a[p] == b[p] {
		p++
	}
	s := 0
	for s < n-p && s < m-p && a[n-1-s] == b[m-1-s] {
		s++
	}
	midOps, err := computeDiffOps(ctx, a[p:n-s], b[p:m-s])
	if err != nil {
		return err
	}
	ops := make([]diffOp, 0, p+len(midOps)+s)
	for i := 0; i < p; i++ {
		ops = append(ops, diffOp{'=', a[i]})
	}
	ops = append(ops, midOps...)
	for i := n - s; i < n; i++ {
		ops = append(ops, diffOp{'=', a[i]})
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

func computeDiffOps(ctx context.Context, a, b []string) ([]diffOp, error) {
	n, m := len(a), len(b)
	if n == 0 {
		ops := make([]diffOp, m)
		for j := 0; j < m; j++ {
			ops[j] = diffOp{'+', b[j]}
		}
		return ops, nil
	}
	if m == 0 {
		ops := make([]diffOp, n)
		for i := 0; i < n; i++ {
			ops[i] = diffOp{'-', a[i]}
		}
		return ops, nil
	}
	// Use exact flat 1D LCS DP for small/medium windows (<= ~4MB),
	// and bounded Myers O(ND) diff for large inputs to avoid O(N*M) OOM/hangs.
	if int64(n+1)*int64(m+1) <= 1_000_000 {
		stride := m + 1
		dp := make([]int32, (n+1)*stride)
		for x := n - 1; x >= 0; x-- {
			if x&63 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			row := x * stride
			nextRow := (x + 1) * stride
			ax := a[x]
			for y := m - 1; y >= 0; y-- {
				if ax == b[y] {
					dp[row+y] = dp[nextRow+y+1] + 1
				} else if dp[nextRow+y] >= dp[row+y+1] {
					dp[row+y] = dp[nextRow+y]
				} else {
					dp[row+y] = dp[row+y+1]
				}
			}
		}
		ops := make([]diffOp, 0, n+m)
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
			case dp[(i+1)*stride+j] >= dp[i*stride+j+1]:
				ops = append(ops, diffOp{'-', a[i]})
				i++
			default:
				ops = append(ops, diffOp{'+', b[j]})
				j++
			}
		}
		return ops, nil
	}
	return myersDiffOps(ctx, a, b, 2048)
}

func myersDiffOps(ctx context.Context, a, b []string, maxEditDist int) ([]diffOp, error) {
	n, m := len(a), len(b)
	limit := n + m
	if limit > maxEditDist {
		limit = maxEditDist
	}
	size := 2*limit + 3
	offset := limit + 1
	v := make([]int, size)
	var trace [][]int
	foundD := -1
	for d := 0; d <= limit; d++ {
		if d&31 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x == n && y == m {
				snap := make([]int, size)
				copy(snap, v)
				trace = append(trace, snap)
				foundD = d
				break
			}
		}
		if foundD >= 0 {
			break
		}
		snap := make([]int, size)
		copy(snap, v)
		trace = append(trace, snap)
	}
	if foundD < 0 {
		ops := make([]diffOp, 0, n+m)
		for i := 0; i < n; i++ {
			ops = append(ops, diffOp{'-', a[i]})
		}
		for j := 0; j < m; j++ {
			ops = append(ops, diffOp{'+', b[j]})
		}
		return ops, nil
	}
	x, y := n, m
	rev := make([]diffOp, 0, n+m)
	for d := foundD; d > 0; d-- {
		prevV := trace[d-1]
		k := x - y
		var prevK int
		if k == -d || (k != d && prevV[offset+k-1] < prevV[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := prevV[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, diffOp{'=', a[x-1]})
			x--
			y--
		}
		if x == prevX {
			rev = append(rev, diffOp{'+', b[y-1]})
			y--
		} else {
			rev = append(rev, diffOp{'-', a[x-1]})
			x--
		}
	}
	for x > 0 && y > 0 {
		rev = append(rev, diffOp{'=', a[x-1]})
		x--
		y--
	}
	slices.Reverse(rev)
	return rev, nil
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

func readStreamChunk(ctx context.Context, r io.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := r.Read(buf[total:])
		total += n
		if err == io.EOF {
			return total, io.EOF
		}
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func discardStreamBytes(ctx context.Context, r io.Reader, skip uint64) (uint64, error) {
	var discarded uint64
	var scratch [32 * 1024]byte
	for discarded < skip {
		if err := ctx.Err(); err != nil {
			return discarded, err
		}
		want := skip - discarded
		if want > uint64(len(scratch)) {
			want = uint64(len(scratch))
		}
		n, err := r.Read(scratch[:want])
		discarded += uint64(n)
		if err != nil {
			return discarded, err
		}
	}
	return discarded, nil
}

func cmdHexdump(ctx context.Context, hc interp.HandlerContext, args []string) error {
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
	var chunkBuf [16]byte
	for _, r := range readers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *skip > 0 {
			got, derr := discardStreamBytes(ctx, r, *skip)
			if derr != nil && derr != io.EOF {
				return derr
			}
			var one [1]byte
			n1, rerr := readStreamChunk(ctx, r, one[:])
			if rerr != nil && rerr != io.EOF {
				return rerr
			}
			if got < *skip || n1 == 0 {
				fmt.Fprintln(hc.Stderr, "hexdump: stdin: Illegal seek")
				hexCode = 1
				continue
			}
			off += *skip
			r = io.MultiReader(bytes.NewReader(one[:1]), r)
		}
		if *limit > 0 {
			r = io.LimitReader(r, int64(*limit))
		}
		if *format != "" {
			if err := applyHexdumpFormatStream(ctx, hc, r, *format, &off); err != nil {
				return err
			}
			continue
		}
		for {
			n, rerr := readStreamChunk(ctx, r, chunkBuf[:])
			if n > 0 {
				chunk := chunkBuf[:n]
				var sb strings.Builder
				if !*canonical {
					fmt.Fprintf(&sb, "%07o", off)
					for j := 0; j < 16; j += 2 {
						if j+1 < len(chunk) {
							fmt.Fprintf(&sb, " %02x%02x", chunk[j+1], chunk[j])
						} else if j < len(chunk) {
							fmt.Fprintf(&sb, "   %02x", chunk[j])
						} else {
							sb.WriteString("     ")
						}
					}
				} else {
					fmt.Fprintf(&sb, "%08x  ", off)
					for j := 0; j < 16; j++ {
						if j < len(chunk) {
							fmt.Fprintf(&sb, "%02x ", chunk[j])
						} else {
							sb.WriteString("   ")
						}
						if j == 7 {
							sb.WriteByte(' ')
						}
					}
					sb.WriteString(" |")
					for _, b := range chunk {
						if b >= 32 && b < 127 {
							sb.WriteByte(b)
						} else {
							sb.WriteByte('.')
						}
					}
					sb.WriteByte('|')
				}
				if _, werr := fmt.Fprintln(hc.Stdout, sb.String()); werr != nil {
					return werr
				}
				off += uint64(n)
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return rerr
			}
		}
	}
	// GNU hexdump prints no final offset for empty input — nor any
	// offset at all when -e custom formats are used.
	if off > 0 && *format == "" {
		var werr error
		if *canonical {
			_, werr = fmt.Fprintf(hc.Stdout, "%08x\n", off)
		} else {
			_, werr = fmt.Fprintf(hc.Stdout, "%07o\n", off)
		}
		if werr != nil {
			return werr
		}
	}
	if hexCode != 0 {
		return exitError{hexCode}
	}
	return nil
}

func cmdStrings(ctx context.Context, hc interp.HandlerContext, args []string) error {
	fs := newFlagSet("strings", hc.Stderr)
	minLen := fs.Uint64("n", 4, "")
	offsetFmt := fs.String("t", "", "")
	fs.StringVar(offsetFmt, "radix", "", "")
	offO := fs.Bool("o", false, "")
	// -a/--all scans the whole file (GNU's default; accepted for parity).
	all := fs.Bool("a", false, "")
	fs.BoolVar(all, "all", false, "")
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
	buf := make([]byte, 64*1024)
	for _, r := range readers {
		var cur []byte
		curOff := 0
		pos := 0
		flush := func() error {
			if uint64(len(cur)) >= *minLen {
				var werr error
				if showOff != "" {
					_, werr = fmt.Fprintf(hc.Stdout, "%s %s\n", formatOffset(curOff, showOff), string(cur))
				} else {
					_, werr = fmt.Fprintln(hc.Stdout, string(cur))
				}
				cur = nil
				return werr
			}
			cur = nil
			return nil
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, rerr := r.Read(buf)
			for i := 0; i < n; i++ {
				b := buf[i]
				if (b >= 32 && b < 127) || b == '\t' {
					if cur == nil {
						curOff = pos
					}
					cur = append(cur, b)
				} else if werr := flush(); werr != nil {
					return werr
				}
				pos++
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				return rerr
			}
		}
		if werr := flush(); werr != nil {
			return werr
		}
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

// applyHexdumpFormat implements hexdump -e FORMAT. It understands the
// common "N/ITER \"FMT\"" shape: N bytes per line, ITER repetitions of
// the printf format. A bare quoted format applies once per byte. This
// covers the idioms used in practice (16/1 "%02x ", 16/1 "%c", etc.).
func applyHexdumpFormatStream(ctx context.Context, hc interp.HandlerContext, r io.Reader, format string, off *uint64) error {
	perLine, iter, f := parseHexdumpFormat(format)
	if f == "" {
		f = "%02x "
	}
	if perLine <= 0 {
		perLine = 16
	}
	if iter <= 0 {
		iter = 1
	}
	buf := make([]byte, perLine)
	for {
		n, rerr := readStreamChunk(ctx, r, buf)
		if n > 0 {
			for j := 0; j < n; j++ {
				if _, werr := fmt.Fprintf(hc.Stdout, f, buf[j]); werr != nil {
					return werr
				}
			}
			*off += uint64(n)
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// parseHexdumpFormat splits "N/ITER \"FMT\"" into its parts.
func parseHexdumpFormat(format string) (perLine, iter int, f string) {
	s := strings.TrimSpace(format)
	// Leading N/ITER (both optional).
	if q := strings.IndexByte(s, '"'); q >= 0 {
		spec := strings.TrimSpace(s[:q])
		rest := s[q:]
		if i := strings.LastIndexByte(rest, '"'); i > 0 {
			f = rest[1:i]
		}
		if spec != "" {
			parts := strings.SplitN(spec, "/", 2)
			if n, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
				perLine = n
			}
			if len(parts) == 2 {
				if n, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
					iter = n
				}
			}
		}
		return
	}
	return 0, 0, s
}
