package unish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"mvdan.cc/sh/v3/interp"
)

type rgOptions struct {
	patterns          []string
	patternFiles      []string
	globs             []rgGlobRule
	types             []string
	typesNot          []string
	typeList          bool
	filesMode         bool
	fixedStrings      bool
	ignoreCase        bool
	caseSensitive     bool
	smartCase         bool
	wordRegexp        bool
	lineRegexp        bool
	invertMatch       bool
	onlyMatching      bool
	filesWithMatches  bool
	filesWithoutMatch bool
	count             bool
	countMatches      bool
	quiet             bool
	nullSep           bool
	vimgrep           bool
	passthru          bool
	hasReplace        bool
	replaceStr        string
	lineNumSet        bool
	lineNum           bool
	filenameSet       bool
	withFilename      bool
	hidden            bool
	noIgnore          bool
	text              bool
	followSymlinks    bool
	noMessages        bool
	unrestricted      int
	maxCount          int
	maxDepth          int // -1 = unlimited
	beforeCtx         int
	afterCtx          int
}

type rgGlobRule struct {
	negated  bool
	ignoreC  bool
	raw      string
	anchored bool
	dirOnly  bool
}

type rgIgnoreRule struct {
	baseDir  string
	negated  bool
	dirOnly  bool
	anchored bool
	pattern  string
}

type rgMatchSpan struct {
	start int
	end   int
	subs  []int // full FindSubmatchIndex slice when capture groups needed
}

type rgMatcher struct {
	emptyPatterns bool
	fixed         bool
	ignoreCase    bool
	wordRegexp    bool
	lineRegexp    bool
	needSubmatch  bool

	// Fast-path fields
	singleLit      []byte // exact case-sensitive literal when 1 pattern & !ignoreCase
	singleLitLower []byte // ASCII lowercase when 1 ASCII literal & ignoreCase
	prefilterLit   []byte // required case-sensitive substring from LiteralPrefix

	re *regexp.Regexp
}

var rgTypesOnce sync.Once
var rgTypesMap map[string][]string

// rgTypeTable lists "name[,alias...]=glob[ glob...]" entries separated by ';'.
// It is parsed lazily: an equivalent map literal costs ~5 KB of init code.
const rgTypeTable = "go=*.go;py,python=*.py *.pyi *.pyw;js,javascript=*.js *.mjs *.cjs *.jsx;" +
	"ts,typescript=*.ts *.mts *.cts *.tsx;rs,rust=*.rs;c=*.c *.h;" +
	"cpp,c++=*.cpp *.cc *.cxx *.hpp *.hh *.hxx *.h *.C *.H;h=*.h *.hpp *.hh *.hxx;java=*.java;" +
	"rb,ruby=*.rb *.rake Rakefile Gemfile;sh=*.sh *.bash *.zsh .bashrc .bash_profile;bash=*.sh *.bash;zsh=*.zsh;" +
	"md,markdown=*.md *.markdown *.mdx;json=*.json *.jsonc *.jsonl;yaml,yml=*.yaml *.yml;toml=*.toml;" +
	"html=*.html *.htm;css=*.css *.scss *.less;sql=*.sql;xml=*.xml *.xsd *.xsl *.svg;txt,text=*.txt"

func rgBuiltinTypes() map[string][]string {
	rgTypesOnce.Do(func() {
		rgTypesMap = map[string][]string{}
		for _, ent := range strings.Split(rgTypeTable, ";") {
			names, globs, _ := strings.Cut(ent, "=")
			gs := strings.Fields(globs)
			for _, n := range strings.Split(names, ",") {
				rgTypesMap[n] = gs
			}
		}
	})
	return rgTypesMap
}

func cmdRg(ctx context.Context, hc interp.HandlerContext, args []string) error {
	opts := rgOptions{
		maxDepth: -1,
	}
	positionals, err := parseRgArgs(hc.Stderr, args, &opts)
	if err == errToolHelp {
		fmt.Fprintln(hc.Stdout, "Usage: rg [OPTIONS] PATTERN [PATH ...] | rg --files [OPTIONS] [PATH ...]")
		return nil
	}
	if err != nil {
		return err
	}
	if opts.typeList {
		types := rgBuiltinTypes()
		keys := make([]string, 0, len(types))
		for k := range types {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if _, werr := fmt.Fprintf(hc.Stdout, "%s: %s\n", k, strings.Join(types[k], ", ")); werr != nil {
				if isPipeClosed(werr) {
					return werr
				}
				return exitError{2}
			}
		}
		return nil
	}

	if opts.unrestricted >= 1 {
		opts.noIgnore = true
	}
	if opts.unrestricted >= 2 {
		opts.hidden = true
	}
	if opts.unrestricted >= 3 {
		opts.text = true
	}

	// Validate -t / -T types upfront.
	typeIncludeGlobs, err := expandRgTypes(opts.types)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "rg: %v\n", err)
		return exitError{2}
	}
	typeExcludeGlobs, err := expandRgTypes(opts.typesNot)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "rg: %v\n", err)
		return exitError{2}
	}

	// --files mode: list searchable files without reading file contents.
	if opts.filesMode {
		targets := positionals
		if len(targets) == 0 {
			targets = []string{"."}
		}
		return runRgFiles(ctx, hc, &opts, targets, typeIncludeGlobs, typeExcludeGlobs)
	}

	// Load patterns from -e, -f, or first positional argument.
	stdinConsumedByPattern := false
	hasExplicitPatternSource := len(opts.patterns) > 0 || len(opts.patternFiles) > 0
	for _, pf := range opts.patternFiles {
		var data []byte
		var rerr error
		if pf == "-" || pf == "/dev/stdin" {
			stdinConsumedByPattern = true
			data, rerr = io.ReadAll(hc.Stdin)
		} else if isToolDevNull(pf) {
			data = nil
		} else {
			data, rerr = fsReadShellFile(ctx, hc.Dir, "rg", pf)
		}
		if rerr != nil {
			if !opts.noMessages {
				fmt.Fprintf(hc.Stderr, "rg: %s: %v\n", pf, rerr)
			}
			return exitError{2}
		}
		opts.patterns = append(opts.patterns, splitRgPatternLines(data)...)
	}

	var targets []string
	if !hasExplicitPatternSource {
		if len(positionals) == 0 {
			fmt.Fprintln(hc.Stderr, "rg: missing search pattern")
			return exitError{2}
		}
		opts.patterns = append(opts.patterns, positionals[0])
		targets = positionals[1:]
	} else {
		targets = positionals
	}

	matcher, err := compileRgMatcher(&opts)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "rg: regex parse error: %v\n", err)
		return exitError{2}
	}

	// Determine whether we are searching stdin vs filesystem targets.
	searchStdinOnly := false
	if len(targets) == 0 {
		if !stdinConsumedByPattern && rgIsPipedStdin(hc.Stdin) {
			searchStdinOnly = true
			targets = []string{"-"}
		} else {
			targets = []string{"."}
		}
	} else if len(targets) == 1 && (targets[0] == "-" || targets[0] == "/dev/stdin") {
		searchStdinOnly = true
	}

	// Smart defaults for line numbers and filename prefixes.
	hasDirTarget := false
	if !searchStdinOnly {
		for _, t := range targets {
			if t == "-" || t == "/dev/stdin" || isActiveProcSubstPath(t) {
				continue
			}
			if fi, serr := fsStat(ctx, hc.Dir, "rg", t); serr == nil && fi.IsDir() {
				hasDirTarget = true
				break
			}
		}
	}

	showLineNum := !searchStdinOnly
	if opts.lineNumSet {
		showLineNum = opts.lineNum
	}
	showFilename := hasDirTarget || len(targets) > 1
	if opts.filenameSet {
		showFilename = opts.withFilename
	}
	if opts.vimgrep {
		if !opts.lineNumSet {
			showLineNum = true
		}
		if !opts.filenameSet && !searchStdinOnly {
			showFilename = true
		}
	}

	// Fast path for stdin-only search (streams line-by-line to hc.Stdout so unbounded pipes & head -n work immediately).
	if searchStdinOnly {
		var out bytes.Buffer
		label := ""
		if showFilename {
			label = "<stdin>"
		}
		matched, _, serr := rgSearchReader(ctx, hc.Stdin, hc.Stdout, label, &opts, matcher, showFilename, showLineNum, &out)
		if serr != nil {
			if isPipeClosed(serr) {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !opts.noMessages {
				fmt.Fprintf(hc.Stderr, "rg: %v\n", serr)
			}
			return exitError{2}
		}
		if matched {
			return nil
		}
		return exitError{1}
	}

	return runRgSearchTargets(ctx, hc, &opts, matcher, targets, typeIncludeGlobs, typeExcludeGlobs, showFilename, showLineNum)
}

var rgOptTable = toolOptParser{tool: "rg", opts: []toolOpt{
	{"h", "help", false},                // 0
	{"e", "regexp", true},               // 1
	{"f", "file", true},                 // 2
	{"F", "fixed-strings", false},       // 3
	{"", "no-fixed-strings", false},     // 4
	{"i", "ignore-case", false},         // 5
	{"s", "case-sensitive", false},      // 6
	{"S", "smart-case", false},          // 7
	{"w", "word-regexp", false},         // 8
	{"x", "line-regexp", false},         // 9
	{"v", "invert-match", false},        // 10
	{"n", "line-number", false},         // 11
	{"N", "no-line-number", false},      // 12
	{"H", "with-filename", false},       // 13
	{"I", "no-filename", false},         // 14
	{"l", "files-with-matches", false},  // 15
	{"", "files-without-match", false},  // 16
	{"c", "count", false},               // 17
	{"", "count-matches", false},        // 18
	{"o", "only-matching", false},       // 19
	{"m", "max-count", true},            // 20
	{"A", "after-context", true},        // 21
	{"B", "before-context", true},       // 22
	{"C", "context", true},              // 23
	{"g", "glob", true},                 // 24
	{"", "iglob", true},                 // 25
	{"t", "type", true},                 // 26
	{"T", "type-not", true},             // 27
	{"", "type-list", false},            // 28
	{"", "files", false},                // 29
	{"d", "max-depth maxdepth", true},   // 30
	{"q", "quiet", false},               // 31
	{"0", "null", false},                // 32
	{"", "vimgrep", false},              // 33
	{"", "passthru passthrough", false}, // 34
	{"r", "replace", true},              // 35
	{".", "hidden", false},              // 36
	{"", "no-hidden", false},            // 37
	{"", "no-ignore no-ignore-vcs no-ignore-dot no-ignore-parent no-ignore-global", false}, // 38
	{"", "ignore", false},        // 39
	{"u", "unrestricted", false}, // 40
	{"a", "text", false},         // 41
	{"", "binary", false},        // 42
	{"L", "follow", false},       // 43
	{"", "no-follow", false},     // 44
	{"", "no-messages", false},   // 45
	{"j", "threads", true},       // 46
	{"", "heading no-heading column no-column stats pcre2 no-config line-buffered block-buffered trim", false}, // 47: accepted
	{"", "color colors", true}, // 48: accepted (output is never colored)
}}

// parseRgArgs parses args (args[0] is "rg") into opts and returns the
// positionals. Usage errors are reported on stderr (exit status 2);
// errToolHelp is returned for -h/--help.
func parseRgArgs(stderr io.Writer, args []string, opts *rgOptions) ([]string, error) {
	return rgOptTable.parse(stderr, args, func(idx int, val string) error {
		n := 0
		switch idx {
		case 20, 21, 22, 23, 30, 46:
			var perr error
			if n, perr = strconv.Atoi(val); perr != nil || n < 0 {
				return toolBadArg(stderr, "rg", "value for --"+strings.Fields(rgOptTable.opts[idx].long)[0], val)
			}
		}
		switch idx {
		case 0:
			return errToolHelp
		case 1:
			opts.patterns = append(opts.patterns, val)
		case 2:
			opts.patternFiles = append(opts.patternFiles, val)
		case 3, 4:
			opts.fixedStrings = idx == 3
		case 5, 6, 7:
			opts.ignoreCase, opts.caseSensitive, opts.smartCase = idx == 5, idx == 6, idx == 7
		case 8:
			opts.wordRegexp = true
		case 9:
			opts.lineRegexp = true
		case 10:
			opts.invertMatch = true
		case 11, 12:
			opts.lineNumSet, opts.lineNum = true, idx == 11
		case 13, 14:
			opts.filenameSet, opts.withFilename = true, idx == 13
		case 15:
			opts.filesWithMatches = true
		case 16:
			opts.filesWithoutMatch = true
		case 17:
			opts.count = true
		case 18:
			opts.count, opts.countMatches = true, true
		case 19:
			opts.onlyMatching = true
		case 20:
			opts.maxCount = n
		case 21:
			opts.afterCtx = n
		case 22:
			opts.beforeCtx = n
		case 23:
			opts.beforeCtx, opts.afterCtx = n, n
		case 24, 25:
			opts.globs = append(opts.globs, parseRgGlobRule(val, idx == 25))
		case 26:
			opts.types = append(opts.types, val)
		case 27:
			opts.typesNot = append(opts.typesNot, val)
		case 28:
			opts.typeList = true
		case 29:
			opts.filesMode = true
		case 30:
			opts.maxDepth = n
		case 31:
			opts.quiet = true
		case 32:
			opts.nullSep = true
		case 33:
			opts.vimgrep = true
		case 34:
			opts.passthru = true
		case 35:
			opts.hasReplace, opts.replaceStr = true, val
		case 36, 37:
			opts.hidden = idx == 36
		case 38, 39:
			opts.noIgnore = idx == 38
		case 40:
			opts.unrestricted++
		case 41, 42:
			opts.text = idx == 41
		case 43, 44:
			opts.followSymlinks = idx == 43
		case 45:
			opts.noMessages = true
		}
		return nil
	})
}

func rgIsPipedStdin(r io.Reader) bool {
	if r == nil {
		return false
	}
	switch v := r.(type) {
	case *os.File:
		if v == nil {
			return false
		}
		fi, err := v.Stat()
		if err != nil {
			return false
		}
		mode := fi.Mode()
		// Like real ripgrep (is_readable_stdin): treat stdin as piped ONLY when it is
		// a FIFO/pipe, socket, or regular file — NEVER a TTY or char device (/dev/null, NUL).
		return mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0 || mode.IsRegular()
	case *strings.Reader:
		// extra.go substitutes strings.NewReader("") when hc.Stdin was nil.
		return v.Size() > 0
	case *bytes.Reader:
		return v.Size() > 0
	default:
		return true
	}
}

func splitRgPatternLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := string(data)
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

func expandRgTypes(names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		if n == "all" {
			for _, gs := range rgBuiltinTypes() {
				out = append(out, gs...)
			}
			continue
		}
		gs, ok := rgBuiltinTypes()[strings.ToLower(n)]
		if !ok {
			return nil, fmt.Errorf("unrecognized file type: %s", n)
		}
		out = append(out, gs...)
	}
	return out, nil
}

func parseRgGlobRule(raw string, ignoreCase bool) rgGlobRule {
	r := rgGlobRule{ignoreC: ignoreCase}
	if strings.HasPrefix(raw, "!") {
		r.negated = true
		raw = raw[1:]
	}
	if strings.HasSuffix(raw, "/") && len(raw) > 1 {
		r.dirOnly = true
		raw = strings.TrimSuffix(raw, "/")
	}
	if strings.HasPrefix(raw, "./") {
		raw = raw[2:]
	}
	if strings.HasPrefix(raw, "/") {
		r.anchored = true
		raw = strings.TrimPrefix(raw, "/")
	} else if strings.Contains(raw, "/") && !strings.HasPrefix(raw, "**/") {
		r.anchored = true
	}
	r.raw = raw
	return r
}

func rgHasUppercasePattern(pat string, fixed bool) bool {
	if fixed {
		for _, r := range pat {
			if unicode.IsUpper(r) {
				return true
			}
		}
		return false
	}
	escaped := false
	for _, r := range pat {
		if escaped {
			escaped = false
			// Regex escape sequences like \S, \W, \D, \B, \A, \P are meta, not literal uppercase.
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func compileRgMatcher(opts *rgOptions) (*rgMatcher, error) {
	m := &rgMatcher{
		fixed:        opts.fixedStrings,
		wordRegexp:   opts.wordRegexp,
		lineRegexp:   opts.lineRegexp,
		needSubmatch: opts.hasReplace && strings.ContainsRune(opts.replaceStr, '$') && !opts.fixedStrings,
	}
	if len(opts.patterns) == 0 {
		m.emptyPatterns = true
		return m, nil
	}

	ignoreCase := opts.ignoreCase
	if opts.smartCase && !opts.ignoreCase && !opts.caseSensitive {
		hasUpper := false
		for _, p := range opts.patterns {
			if rgHasUppercasePattern(p, opts.fixedStrings) {
				hasUpper = true
				break
			}
		}
		if !hasUpper {
			ignoreCase = true
		}
	}
	m.ignoreCase = ignoreCase

	// Build combined regexp (always available for multi-pattern, captures, regexes).
	parts := make([]string, len(opts.patterns))
	for i, p := range opts.patterns {
		if opts.fixedStrings {
			p = regexp.QuoteMeta(p)
		}
		if opts.lineRegexp {
			p = "^(?:" + p + ")$"
		}
		parts[i] = "(?:" + p + ")"
	}
	combined := strings.Join(parts, "|")
	if len(parts) == 1 && !opts.fixedStrings && !opts.lineRegexp {
		// Preserve user capture group numbering $1, $2, ... when there is a single pattern!
		combined = opts.patterns[0]
	} else if len(parts) == 1 && !opts.fixedStrings && opts.lineRegexp {
		combined = "^(?:" + opts.patterns[0] + ")$"
	}
	if ignoreCase {
		combined = "(?i)" + combined
	}

	re, err := regexp.Compile(combined)
	if err != nil {
		return nil, err
	}
	m.re = re

	// Set up SIMD literal fast-paths!
	if len(opts.patterns) == 1 {
		p := opts.patterns[0]
		if opts.fixedStrings {
			if !ignoreCase {
				m.singleLit = []byte(p)
				m.prefilterLit = m.singleLit
			} else if isPureASCII(p) {
				m.singleLitLower = []byte(strings.ToLower(p))
			}
		} else if !m.needSubmatch {
			if pfx, complete := re.LiteralPrefix(); len(pfx) > 0 && !ignoreCase {
				if complete && !opts.lineRegexp {
					m.singleLit = []byte(pfx)
				}
				m.prefilterLit = []byte(pfx)
			}
		} else if pfx, _ := re.LiteralPrefix(); len(pfx) > 0 && !ignoreCase {
			m.prefilterLit = []byte(pfx)
		}
	}
	return m, nil
}

func isPureASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func isRgWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func rgWordBoundaryOK(line []byte, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRune(line[:start])
		if isRgWordRune(r) {
			return false
		}
	}
	if end < len(line) {
		r, _ := utf8.DecodeRune(line[end:])
		if isRgWordRune(r) {
			return false
		}
	}
	return true
}

func asciiIndexFold(haystack, needleLower []byte) int {
	n := len(needleLower)
	if n == 0 {
		return 0
	}
	if n > len(haystack) {
		return -1
	}
	c0 := needleLower[0]
	c0Up := c0
	if c0 >= 'a' && c0 <= 'z' {
		c0Up = c0 - ('a' - 'A')
	}
	limit := len(haystack) - n
	for i := 0; i <= limit; i++ {
		if c0 == c0Up {
			rel := bytes.IndexByte(haystack[i:limit+1], c0)
			if rel < 0 {
				return -1
			}
			i += rel
		} else {
			b := haystack[i]
			if b != c0 && b != c0Up {
				continue
			}
		}
		ok := true
		for j := 1; j < n; j++ {
			hb := haystack[i+j]
			if hb >= 'A' && hb <= 'Z' {
				hb += 'a' - 'A'
			}
			if hb != needleLower[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// matchLine reports whether line matches (before invertMatch).
func (m *rgMatcher) matchLine(line []byte) bool {
	if m.emptyPatterns {
		return false
	}
	// Exact literal fast-path
	if m.singleLit != nil {
		lit := m.singleLit
		if m.lineRegexp {
			return bytes.Equal(line, lit) && (!m.wordRegexp || rgWordBoundaryOK(line, 0, len(line)))
		}
		if len(lit) == 0 {
			if !m.wordRegexp {
				return true
			}
			for pos := 0; pos <= len(line); {
				if rgWordBoundaryOK(line, pos, pos) {
					return true
				}
				if pos == len(line) {
					break
				}
				_, sz := utf8.DecodeRune(line[pos:])
				pos += max(sz, 1)
			}
			return false
		}
		off := 0
		for off <= len(line)-len(lit) {
			idx := bytes.Index(line[off:], lit)
			if idx < 0 {
				return false
			}
			s := off + idx
			e := s + len(lit)
			if !m.wordRegexp || rgWordBoundaryOK(line, s, e) {
				return true
			}
			off = s + 1
		}
		return false
	}

	// Case-insensitive ASCII literal fast-path
	if m.singleLitLower != nil {
		lit := m.singleLitLower
		if m.lineRegexp {
			if len(line) != len(lit) {
				return false
			}
			return asciiIndexFold(line, lit) == 0 && (!m.wordRegexp || rgWordBoundaryOK(line, 0, len(line)))
		}
		if len(lit) == 0 {
			return !m.wordRegexp || rgWordBoundaryOK(line, 0, 0)
		}
		off := 0
		for off <= len(line)-len(lit) {
			idx := asciiIndexFold(line[off:], lit)
			if idx < 0 {
				return false
			}
			s := off + idx
			e := s + len(lit)
			if !m.wordRegexp || rgWordBoundaryOK(line, s, e) {
				return true
			}
			off = s + 1
		}
		return false
	}

	if len(m.prefilterLit) > 0 && bytes.Index(line, m.prefilterLit) < 0 {
		return false
	}
	if !m.wordRegexp {
		return m.re.Match(line)
	}
	spans := m.findAllSpans(line, 1)
	return len(spans) > 0
}

// findAllSpans returns up to maxSpans (<=0 for all) non-overlapping matches on line.
func (m *rgMatcher) findAllSpans(line []byte, maxSpans int) []rgMatchSpan {
	if m.emptyPatterns {
		return nil
	}
	var out []rgMatchSpan

	if m.singleLit != nil && !m.needSubmatch {
		lit := m.singleLit
		if m.lineRegexp {
			if bytes.Equal(line, lit) && (!m.wordRegexp || rgWordBoundaryOK(line, 0, len(line))) {
				return []rgMatchSpan{{start: 0, end: len(line)}}
			}
			return nil
		}
		if len(lit) == 0 {
			for pos := 0; pos <= len(line); {
				if !m.wordRegexp || rgWordBoundaryOK(line, pos, pos) {
					out = append(out, rgMatchSpan{start: pos, end: pos})
					if maxSpans > 0 && len(out) >= maxSpans {
						return out
					}
				}
				if pos == len(line) {
					break
				}
				_, sz := utf8.DecodeRune(line[pos:])
				pos += max(sz, 1)
			}
			return out
		}
		off := 0
		for off <= len(line)-len(lit) {
			idx := bytes.Index(line[off:], lit)
			if idx < 0 {
				break
			}
			s := off + idx
			e := s + len(lit)
			if !m.wordRegexp || rgWordBoundaryOK(line, s, e) {
				out = append(out, rgMatchSpan{start: s, end: e})
				if maxSpans > 0 && len(out) >= maxSpans {
					return out
				}
				off = e
			} else {
				off = s + 1
			}
		}
		return out
	}

	// Fast direct full-line regexp pass when -w word boundary retry is not needed.
	if !m.wordRegexp {
		limit := -1
		if maxSpans > 0 {
			limit = maxSpans
		}
		if m.needSubmatch {
			locs := m.re.FindAllSubmatchIndex(line, limit)
			out = make([]rgMatchSpan, 0, len(locs))
			for _, loc := range locs {
				out = append(out, rgMatchSpan{start: loc[0], end: loc[1], subs: loc})
			}
			return out
		}
		locs := m.re.FindAllIndex(line, limit)
		out = make([]rgMatchSpan, 0, len(locs))
		for _, loc := range locs {
			out = append(out, rgMatchSpan{start: loc[0], end: loc[1]})
		}
		return out
	}

	// Word-regexp scan (-w): checks rgWordBoundaryOK and retries at next rune on boundary mismatch.
	pos := 0
	lastNonEmptyEnd := -1
	for pos <= len(line) {
		loc := m.findNextAtOrAfter(line, pos)
		if loc == nil {
			break
		}
		s, e := loc[0], loc[1]
		if s == e && s == lastNonEmptyEnd {
			if pos == len(line) {
				break
			}
			_, sz := utf8.DecodeRune(line[pos:])
			pos += max(sz, 1)
			continue
		}
		if m.wordRegexp && !rgWordBoundaryOK(line, s, e) {
			if pos >= len(line) {
				break
			}
			next := s + 1
			if next <= pos {
				next = pos + 1
			}
			for next < len(line) && !utf8.RuneStart(line[next]) {
				next++
			}
			pos = next
			continue
		}
		span := rgMatchSpan{start: s, end: e}
		if m.needSubmatch {
			span.subs = loc
		}
		out = append(out, span)
		if maxSpans > 0 && len(out) >= maxSpans {
			return out
		}
		if s == e {
			if s == len(line) {
				break
			}
			_, sz := utf8.DecodeRune(line[s:])
			pos = s + max(sz, 1)
		} else {
			lastNonEmptyEnd = e
			pos = e
		}
	}
	return out
}

func (m *rgMatcher) findNextAtOrAfter(line []byte, minPos int) []int {
	if m.needSubmatch {
		all := m.re.FindAllSubmatchIndex(line, -1)
		for _, loc := range all {
			if loc[0] >= minPos {
				return loc
			}
		}
		// Fallback slice search for overlapping word-regexp retry when pattern has no ^ or \b/\A anchor
		if !strings.ContainsAny(m.re.String(), `^\`) && minPos <= len(line) {
			loc := m.re.FindSubmatchIndex(line[minPos:])
			if loc != nil {
				for i := range loc {
					if loc[i] >= 0 {
						loc[i] += minPos
					}
				}
				return loc
			}
		}
		return nil
	}
	all := m.re.FindAllIndex(line, -1)
	for _, loc := range all {
		if loc[0] >= minPos {
			return loc
		}
	}
	if !strings.ContainsAny(m.re.String(), `^\`) && minPos <= len(line) {
		loc := m.re.FindIndex(line[minPos:])
		if loc != nil {
			loc[0] += minPos
			loc[1] += minPos
			return loc
		}
	}
	return nil
}

func (m *rgMatcher) expandReplacement(line []byte, sp rgMatchSpan, repl string) []byte {
	if !m.needSubmatch {
		// Still support $$ -> $ and $0 -> full match even in fixed-strings or literal-repl mode!
		if !strings.ContainsRune(repl, '$') {
			return []byte(repl)
		}
		var b bytes.Buffer
		for i := 0; i < len(repl); i++ {
			if repl[i] == '$' && i+1 < len(repl) {
				switch repl[i+1] {
				case '$':
					b.WriteByte('$')
					i++
					continue
				case '0':
					b.Write(line[sp.start:sp.end])
					i++
					continue
				case '{':
					if strings.HasPrefix(repl[i:], "${0}") {
						b.Write(line[sp.start:sp.end])
						i += 3
						continue
					}
				}
			}
			b.WriteByte(repl[i])
		}
		return b.Bytes()
	}
	return m.re.Expand(nil, []byte(repl), line, sp.subs)
}

func (m *rgMatcher) replaceLine(line []byte, spans []rgMatchSpan, repl string) []byte {
	if len(spans) == 0 {
		return line
	}
	var out bytes.Buffer
	out.Grow(len(line) + len(spans)*len(repl))
	prev := 0
	for _, sp := range spans {
		out.Write(line[prev:sp.start])
		out.Write(m.expandReplacement(line, sp, repl))
		prev = sp.end
	}
	out.Write(line[prev:])
	return out.Bytes()
}

// ---- .gitignore / .ignore & Glob Engine ----

func loadDirIgnoreRules(ctx context.Context, baseDir, rawDir, absDir string) []rgIgnoreRule {
	var rules []rgIgnoreRule
	for _, fname := range []string{".gitignore", ".ignore", ".rgignore"} {
		data, err := fsReadFile(ctx, baseDir, "rg", filepath.Join(rawDir, fname))
		if err != nil {
			continue
		}
		rules = append(rules, parseGitignoreBytes(absDir, data)...)
	}
	return rules
}

func loadAncestorIgnoreRules(ctx context.Context, startDir string) []rgIgnoreRule {
	abs := absFromDir("", startDir)
	if abs == "" {
		return nil
	}
	// Walk upward to repo root (.git), collecting ancestor directories.
	var chain []string
	cur := abs
	for {
		if _, cerr := checkFS(ctx, "", "rg", cur, OpList); cerr != nil {
			break
		}
		parent := filepath.Dir(cur)
		if _, gerr := fsLstat(ctx, "", "rg", filepath.Join(cur, ".git")); gerr == nil {
			// Include repo-level .git/info/exclude if it is a directory.
			var all []rgIgnoreRule
			if exData, eerr := fsReadFile(ctx, "", "rg", filepath.Join(cur, ".git", "info", "exclude")); eerr == nil {
				all = append(all, parseGitignoreBytes(cur, exData)...)
			}
			// Add rules from repo root down to parent of startDir (startDir itself is loaded on entry).
			if cur != abs {
				all = append(all, loadDirIgnoreRules(ctx, "", cur, cur)...)
			}
			for i := len(chain) - 1; i >= 0; i-- {
				all = append(all, loadDirIgnoreRules(ctx, "", chain[i], chain[i])...)
			}
			return all
		}
		if parent == cur {
			break
		}
		if cur != abs {
			chain = append(chain, cur)
		}
		cur = parent
	}
	return nil
}

func parseGitignoreBytes(baseDir string, data []byte) []rgIgnoreRule {
	var rules []rgIgnoreRule
	lines := strings.Split(string(data), "\n")
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Trim unescaped trailing spaces.
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
			line = line[:len(line)-1]
		}
		line = strings.ReplaceAll(line, `\ `, " ")
		if line == "" {
			continue
		}
		negated := false
		if strings.HasPrefix(line, "!") {
			negated = true
			line = line[1:]
		} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
			line = line[1:]
		}
		if line == "" {
			continue
		}
		dirOnly := false
		if strings.HasSuffix(line, "/") {
			dirOnly = true
			line = strings.TrimRight(line, "/")
			if line == "" {
				continue
			}
		}
		anchored := false
		if strings.HasPrefix(line, "/") {
			anchored = true
			line = strings.TrimLeft(line, "/")
		} else if strings.Contains(line, "/") {
			if strings.HasPrefix(line, "**/") {
				// **/foo matches at root or any subdir
				anchored = false
			} else {
				anchored = true
			}
		}
		if line == "" {
			continue
		}
		rules = append(rules, rgIgnoreRule{
			baseDir:  baseDir,
			negated:  negated,
			dirOnly:  dirOnly,
			anchored: anchored,
			pattern:  line,
		})
	}
	return rules
}

func isIgnoredByRules(rules []rgIgnoreRule, fullPath, baseName string, isDir bool) bool {
	ignored := false
	for i := range rules {
		r := &rules[i]
		if r.negated != ignored {
			// Optimization: if already ignored, non-negated rules don't change state;
			// if not ignored, negated rules don't change state!
			continue
		}
		if r.dirOnly && !isDir {
			continue
		}
		if matchIgnoreRule(r, fullPath, baseName) {
			ignored = !r.negated
		}
	}
	return ignored
}

func matchIgnoreRule(r *rgIgnoreRule, fullPath, baseName string) bool {
	if !r.anchored && !strings.Contains(r.pattern, "/") {
		return matchGitGlob(r.pattern, baseName, false)
	}
	rel, err := filepath.Rel(r.baseDir, fullPath)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return false
	}
	if strings.HasPrefix(r.pattern, "**/") {
		sub := r.pattern[3:]
		if matchGitGlob(sub, rel, false) {
			return true
		}
		for idx := 0; idx < len(rel); idx++ {
			if rel[idx] == '/' && matchGitGlob(sub, rel[idx+1:], false) {
				return true
			}
		}
		return false
	}
	return matchGitGlob(r.pattern, rel, false)
}

func matchUserGlobs(globs []rgGlobRule, relSlash, baseName string, isDir bool) (allowed bool) {
	if len(globs) == 0 {
		return true
	}
	relSlash = strings.TrimPrefix(relSlash, "./")
	hasPositive := false
	for i := range globs {
		if !globs[i].negated {
			hasPositive = true
			break
		}
	}
	// Directories: only prune a directory when an explicit negated glob excludes it!
	// Positive file globs like -g '*.go' must NOT block descending into subdirectories.
	if isDir {
		excluded := false
		for i := range globs {
			g := &globs[i]
			if rgGlobMatchesPath(g, relSlash, baseName, true) {
				excluded = g.negated
			}
		}
		return !excluded
	}

	matched := !hasPositive
	for i := range globs {
		g := &globs[i]
		if g.dirOnly {
			// Check if any parent directory of relSlash matches g.raw
			if g.negated && rgGlobMatchesAncestorDir(g, relSlash) {
				matched = false
			}
			continue
		}
		if rgGlobMatchesPath(g, relSlash, baseName, false) {
			matched = !g.negated
		}
	}
	return matched
}

func rgGlobMatchesAncestorDir(g *rgGlobRule, relSlash string) bool {
	for i := 0; i < len(relSlash); i++ {
		if relSlash[i] == '/' {
			dirPart := relSlash[:i]
			dirBase := dirPart
			if k := strings.LastIndexByte(dirPart, '/'); k >= 0 {
				dirBase = dirPart[k+1:]
			}
			if rgGlobMatchesPath(g, dirPart, dirBase, true) {
				return true
			}
		}
	}
	return false
}

func rgGlobMatchesPath(g *rgGlobRule, relSlash, baseName string, isDir bool) bool {
	if g.dirOnly && !isDir {
		return false
	}
	pat := g.raw
	if strings.HasPrefix(pat, "**/") {
		sub := pat[3:]
		if matchGitGlob(sub, relSlash, g.ignoreC) {
			return true
		}
		for idx := 0; idx < len(relSlash); idx++ {
			if relSlash[idx] == '/' && matchGitGlob(sub, relSlash[idx+1:], g.ignoreC) {
				return true
			}
		}
		return false
	}
	if g.anchored {
		if matchGitGlob(pat, relSlash, g.ignoreC) {
			return true
		}
		if isDir && strings.HasSuffix(pat, "/**") {
			prefix := strings.TrimSuffix(pat, "/**")
			if matchGitGlob(prefix, relSlash, g.ignoreC) {
				return true
			}
		}
		return false
	}
	return matchGitGlob(pat, baseName, g.ignoreC) || matchGitGlob(pat, relSlash, g.ignoreC)
}

func matchTypeGlobs(includeGlobs, excludeGlobs []string, baseName string) bool {
	if len(excludeGlobs) > 0 {
		for _, g := range excludeGlobs {
			if matchGitGlob(g, baseName, false) {
				return false
			}
		}
	}
	if len(includeGlobs) > 0 {
		for _, g := range includeGlobs {
			if matchGitGlob(g, baseName, false) {
				return true
			}
		}
		return false
	}
	return true
}

// matchGitGlob matches gitignore/rg glob patterns supporting *, **, ?, [...], {a,b}, and \ escapes.
func matchGitGlob(pattern, name string, ignoreCase bool) bool {
	// Expand top-level brace alternation {a,b} if present (e.g. -g '*.{go,rs}')
	if lb := strings.IndexByte(pattern, '{'); lb >= 0 {
		if rb := strings.IndexByte(pattern[lb:], '}'); rb > 0 {
			rb += lb
			prefix := pattern[:lb]
			suffix := pattern[rb+1:]
			for _, alt := range strings.Split(pattern[lb+1:rb], ",") {
				if matchGitGlob(prefix+alt+suffix, name, ignoreCase) {
					return true
				}
			}
			return false
		}
	}
	if ignoreCase {
		pattern = strings.ToLower(pattern)
		name = strings.ToLower(name)
	}
	return matchGitGlobRaw(pattern, name)
}

func matchGitGlobRaw(p, s string) bool {
	for len(p) > 0 {
		if strings.HasPrefix(p, "**/") {
			p = p[3:]
			if matchGitGlobRaw(p, s) {
				return true
			}
			for i := 0; i < len(s); i++ {
				if s[i] == '/' && matchGitGlobRaw(p, s[i+1:]) {
					return true
				}
			}
			return false
		}
		if p == "**" {
			return true
		}
		if strings.HasPrefix(p, "/**") && len(p) == 3 {
			// "dir/**" matches anything strictly inside "dir/"
			return len(s) > 0 && s[0] == '/'
		}

		switch p[0] {
		case '*':
			// Check if double-star without slash
			if len(p) > 1 && p[1] == '*' {
				p = p[2:]
				for i := 0; i <= len(s); i++ {
					if matchGitGlobRaw(p, s[i:]) {
						return true
					}
				}
				return false
			}
			p = p[1:]
			for i := 0; i <= len(s); i++ {
				if i > 0 && s[i-1] == '/' {
					break
				}
				if matchGitGlobRaw(p, s[i:]) {
					return true
				}
			}
			return false

		case '?':
			if len(s) == 0 || s[0] == '/' {
				return false
			}
			_, sz := utf8.DecodeRuneInString(s)
			p = p[1:]
			s = s[sz:]

		case '[':
			if len(s) == 0 || s[0] == '/' {
				return false
			}
			sr, sz := utf8.DecodeRuneInString(s)
			restP, ok := matchGlobClass(p[1:], sr)
			if !ok {
				return false
			}
			p = restP
			s = s[sz:]

		case '\\':
			if len(p) > 1 {
				p = p[1:]
			}
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p = p[1:]
			s = s[1:]

		default:
			if len(s) == 0 || p[0] != s[0] {
				return false
			}
			p = p[1:]
			s = s[1:]
		}
	}
	return len(s) == 0
}

func matchGlobClass(p string, sr rune) (string, bool) {
	neg := false
	if len(p) > 0 && (p[0] == '!' || p[0] == '^') {
		neg = true
		p = p[1:]
	}
	matched := false
	first := true
	for len(p) > 0 {
		if p[0] == ']' && !first {
			if neg {
				return p[1:], !matched
			}
			return p[1:], matched
		}
		first = false
		if p[0] == '\\' && len(p) > 1 {
			p = p[1:]
		}
		lo, sz := utf8.DecodeRuneInString(p)
		p = p[sz:]
		if len(p) >= 2 && p[0] == '-' && p[1] != ']' {
			p = p[1:]
			if p[0] == '\\' && len(p) > 1 {
				p = p[1:]
			}
			hi, hsz := utf8.DecodeRuneInString(p)
			p = p[hsz:]
			if lo <= sr && sr <= hi {
				matched = true
			}
		} else if sr == lo {
			matched = true
		}
	}
	return "", false
}

// ---- Directory Walker & Parallel Search Engine ----

type rgFileJob struct {
	idx         int
	baseDir     string
	rawPath     string
	absPath     string
	displayPath string
	isStdin     bool
	explicit    bool
}

type rgFileResult struct {
	idx         int
	displayPath string
	isStdin     bool
	explicit    bool
	out         []byte
	matched     bool
	err         error
}

type rgWriteFunc func([]byte) (int, error)

func (fn rgWriteFunc) Write(p []byte) (int, error) { return fn(p) }

func formatRgChildDisplay(parentDisplay, name string) string {
	if parentDisplay == "" || parentDisplay == "." {
		return name
	}
	if parentDisplay == "./" {
		return "./" + name
	}
	p := strings.TrimSuffix(parentDisplay, "/")
	return p + "/" + name
}

func walkRgTarget(
	ctx context.Context,
	hc interp.HandlerContext,
	opts *rgOptions,
	target string,
	typeInc, typeExc []string,
	emit func(rawPath, absPath, displayPath string, isStdin, explicit bool) bool,
	hadErr *atomic.Bool,
) {
	if ctx.Err() != nil {
		return
	}
	if target == "-" || target == "/dev/stdin" {
		emit("", "", "<stdin>", true, true)
		return
	}
	if isToolDevNull(target) {
		emit(target, os.DevNull, filepath.ToSlash(target), false, true)
		return
	}
	if isActiveProcSubstPath(target) {
		emit(target, target, filepath.ToSlash(target), false, true)
		return
	}
	full := resolveHostPath(hc.Dir, target)
	var fi os.FileInfo
	var err error
	if opts.followSymlinks {
		fi, err = fsStat(ctx, hc.Dir, "rg", target)
	} else {
		fi, err = fsLstat(ctx, hc.Dir, "rg", target)
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			// Explicit symlink on CLI: follow like ripgrep!
			sfi, serr := fsStat(ctx, hc.Dir, "rg", target)
			if serr == nil {
				fi = sfi
			} else if errors.Is(serr, os.ErrPermission) {
				err = serr
			}
		}
	}
	if err != nil {
		hadErr.Store(true)
		if !opts.noMessages {
			fmt.Fprintf(hc.Stderr, "rg: %s: %v\n", target, err)
		}
		return
	}

	disp := filepath.ToSlash(target)
	if !fi.IsDir() {
		// Explicit file target on CLI bypasses .gitignore/hidden, just like real ripgrep!
		if opts.filesMode {
			if _, cerr := checkFS(ctx, hc.Dir, "rg", target, OpRead); cerr != nil {
				hadErr.Store(true)
				if !opts.noMessages {
					fmt.Fprintf(hc.Stderr, "rg: %s: %v\n", target, cerr)
				}
				return
			}
		}
		emit(target, full, disp, false, true)
		return
	}

	absRoot := absFromDir(hc.Dir, target)
	hookActive := CapabilityHookFromContext(ctx) != nil
	var baseRules []rgIgnoreRule
	if !opts.noIgnore {
		baseRules = loadAncestorIgnoreRules(ctx, absRoot)
	}

	var walkDir func(absDir, rawDir, dispDir, relRoot string, depth int, rules []rgIgnoreRule) bool
	walkDir = func(absDir, rawDir, dispDir, relRoot string, depth int, rules []rgIgnoreRule) bool {
		if ctx.Err() != nil {
			return false
		}
		if opts.maxDepth >= 0 && depth > opts.maxDepth {
			return true
		}
		if !opts.noIgnore {
			dirRules := loadDirIgnoreRules(ctx, hc.Dir, rawDir, absDir)
			if len(dirRules) > 0 {
				nextRules := make([]rgIgnoreRule, len(rules)+len(dirRules))
				copy(nextRules, rules)
				copy(nextRules[len(rules):], dirRules)
				rules = nextRules
			}
		}

		entries, rerr := fsReadDir(ctx, hc.Dir, "rg", rawDir)
		if rerr != nil {
			if depth == 1 || !(hookActive && errors.Is(rerr, os.ErrPermission)) {
				hadErr.Store(true)
			}
			if !opts.noMessages {
				fmt.Fprintf(hc.Stderr, "rg: %s: %v\n", dispDir, rerr)
			}
			return true
		}

		for _, e := range entries {
			if ctx.Err() != nil {
				return false
			}
			name := e.Name()
			// Hidden check: skip dotfiles/dotdirs unless --hidden, and skip .git unless -uu (hidden && noIgnore).
			if strings.HasPrefix(name, ".") {
				if !opts.hidden {
					continue
				}
				if name == ".git" && !opts.noIgnore {
					continue
				}
			}

			childRaw := filepath.Join(rawDir, name)
			childDisp := formatRgChildDisplay(dispDir, name)

			typ := e.Type()
			isSymlink := typ&os.ModeSymlink != 0
			isDir := e.IsDir()
			if isSymlink {
				if !opts.followSymlinks {
					continue
				}
				sfi, serr := fsStat(ctx, hc.Dir, "rg", childRaw)
				if serr != nil {
					if hookActive && errors.Is(serr, os.ErrPermission) && !opts.noMessages {
						fmt.Fprintf(hc.Stderr, "rg: %s: %v\n", childDisp, serr)
					}
					continue
				}
				isDir = sfi.IsDir()
			} else if !isDir && !typ.IsRegular() && typ != 0 {
				// Skip sockets, FIFOs, devices
				continue
			}

			childAbs := filepath.Join(absDir, name)
			childRel := name
			if relRoot != "" {
				childRel = relRoot + "/" + name
			}

			if !opts.noIgnore && len(rules) > 0 && isIgnoredByRules(rules, childAbs, name, isDir) {
				continue
			}
			if len(opts.globs) > 0 && !matchUserGlobs(opts.globs, childRel, name, isDir) {
				continue
			}

			if isDir {
				if opts.maxDepth < 0 || depth < opts.maxDepth {
					if !walkDir(childAbs, childRaw, childDisp, childRel, depth+1, rules) {
						return false
					}
				}
				continue
			}

			if !matchTypeGlobs(typeInc, typeExc, name) {
				continue
			}
			if hookActive {
				if _, cerr := checkFS(ctx, hc.Dir, "rg", childRaw, OpRead); cerr != nil {
					if !opts.noMessages {
						fmt.Fprintf(hc.Stderr, "rg: %s: %v\n", childDisp, cerr)
					}
					if !errors.Is(cerr, os.ErrPermission) {
						hadErr.Store(true)
					}
					continue
				}
			}
			if !emit(childRaw, childAbs, childDisp, false, false) {
				return false
			}
		}
		return true
	}

	if opts.maxDepth == 0 {
		return
	}
	walkDir(absRoot, target, disp, "", 1, baseRules)
}

func runRgFiles(
	ctx context.Context,
	hc interp.HandlerContext,
	opts *rgOptions,
	targets, typeInc, typeExc []string,
) error {
	var hadErr atomic.Bool
	var count int
	var writeErr error
	term := byte('\n')
	if opts.nullSep {
		term = 0
	}
	var outBuf bytes.Buffer
	flush := func() bool {
		if outBuf.Len() == 0 {
			return true
		}
		if !opts.quiet {
			if _, err := hc.Stdout.Write(outBuf.Bytes()); err != nil {
				writeErr = err
				return false
			}
		}
		outBuf.Reset()
		return true
	}

	for _, t := range targets {
		stopped := false
		walkRgTarget(ctx, hc, opts, t, typeInc, typeExc, func(_, _, displayPath string, isStdin, _ bool) bool {
			if isStdin {
				return true
			}
			count++
			if opts.quiet {
				stopped = true
				return false
			}
			outBuf.WriteString(displayPath)
			outBuf.WriteByte(term)
			if outBuf.Len() >= 32*1024 {
				return flush()
			}
			return true
		}, &hadErr)
		if stopped || writeErr != nil || ctx.Err() != nil {
			break
		}
	}
	if writeErr == nil {
		flush()
	}
	if writeErr != nil {
		if isPipeClosed(writeErr) {
			return writeErr
		}
		return exitError{2}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if opts.quiet && count > 0 {
		return nil
	}
	if hadErr.Load() {
		return exitError{2}
	}
	if count > 0 {
		return nil
	}
	return exitError{1}
}

func runRgSearchTargets(
	ctx context.Context,
	hc interp.HandlerContext,
	opts *rgOptions,
	matcher *rgMatcher,
	targets, typeInc, typeExc []string,
	showFilename, showLineNum bool,
) error {
	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	numWorkers := runtime.GOMAXPROCS(0)
	if numWorkers < 1 {
		numWorkers = 1
	}
	if numWorkers > 16 {
		numWorkers = 16
	}

	jobs := make(chan rgFileJob, numWorkers*4)
	results := make(chan rgFileResult, numWorkers*4)

	var hadErr atomic.Bool
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var localBuf bytes.Buffer
			for job := range jobs {
				if searchCtx.Err() != nil {
					return
				}
				if job.isStdin {
					select {
					case results <- rgFileResult{idx: job.idx, displayPath: job.displayPath, isStdin: true}:
					case <-searchCtx.Done():
						return
					}
					continue
				}
				localBuf.Reset()
				label := ""
				if showFilename {
					label = job.displayPath
				}
				matched, err := rgSearchFile(searchCtx, job, label, opts, matcher, showFilename, showLineNum, &localBuf)
				var outCopy []byte
				if localBuf.Len() > 0 && !opts.quiet {
					outCopy = append([]byte(nil), localBuf.Bytes()...)
				}
				select {
				case results <- rgFileResult{idx: job.idx, explicit: job.explicit, out: outCopy, matched: matched, err: err}:
				case <-searchCtx.Done():
					return
				}
				if matched && opts.quiet {
					cancel()
					return
				}
			}
		}()
	}

	// Producer goroutine walks directories in deterministic os.ReadDir sorted order.
	go func() {
		idx := 0
		for _, t := range targets {
			if searchCtx.Err() != nil {
				break
			}
			walkRgTarget(searchCtx, hc, opts, t, typeInc, typeExc, func(rawPath, absPath, displayPath string, isStdin, explicit bool) bool {
				job := rgFileJob{
					idx:         idx,
					baseDir:     hc.Dir,
					rawPath:     rawPath,
					absPath:     absPath,
					displayPath: displayPath,
					isStdin:     isStdin,
					explicit:    explicit,
				}
				idx++
				select {
				case jobs <- job:
					return true
				case <-searchCtx.Done():
					return false
				}
			}, &hadErr)
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	// Ordered writer merges worker outputs strictly in deterministic file index order!
	pending := make(map[int]rgFileResult)
	nextIdx := 0
	anyMatch := false
	printedHunkFile := false
	needCtxFileSep := (opts.beforeCtx > 0 || opts.afterCtx > 0) &&
		!opts.filesWithMatches && !opts.filesWithoutMatch && !opts.count && !opts.vimgrep && !opts.onlyMatching && !opts.passthru
	var pipeErr error
	hookActive := CapabilityHookFromContext(ctx) != nil

	for res := range results {
		if res.err != nil {
			if !isPipeClosed(res.err) && ctx.Err() == nil {
				if res.explicit || !(hookActive && errors.Is(res.err, os.ErrPermission)) {
					hadErr.Store(true)
				}
				if !opts.noMessages {
					fmt.Fprintf(hc.Stderr, "rg: %v\n", res.err)
				}
			}
		}
		if res.matched {
			anyMatch = true
			if opts.quiet {
				cancel()
				break
			}
		}
		pending[res.idx] = res
		for {
			cur, ok := pending[nextIdx]
			if !ok {
				break
			}
			delete(pending, nextIdx)
			nextIdx++
			if cur.isStdin {
				var stdinBuf bytes.Buffer
				label := ""
				if showFilename {
					label = cur.displayPath
				}
				streamW := rgWriteFunc(func(p []byte) (int, error) {
					if len(p) == 0 || opts.quiet {
						return len(p), nil
					}
					if needCtxFileSep && printedHunkFile {
						if _, werr := hc.Stdout.Write([]byte("--\n")); werr != nil {
							return 0, werr
						}
					}
					printedHunkFile = true
					return hc.Stdout.Write(p)
				})
				sMatched, _, serr := rgSearchReader(searchCtx, hc.Stdin, streamW, label, opts, matcher, showFilename, showLineNum, &stdinBuf)
				if sMatched {
					anyMatch = true
					if opts.quiet {
						cancel()
						break
					}
				}
				if serr != nil {
					if isPipeClosed(serr) {
						pipeErr = serr
						cancel()
						break
					}
					if ctx.Err() == nil {
						hadErr.Store(true)
						if !opts.noMessages {
							fmt.Fprintf(hc.Stderr, "rg: %v\n", serr)
						}
					}
				}
				continue
			}
			if len(cur.out) > 0 && !opts.quiet && pipeErr == nil {
				if needCtxFileSep && printedHunkFile {
					if _, werr := hc.Stdout.Write([]byte("--\n")); werr != nil {
						pipeErr = werr
						cancel()
						break
					}
				}
				if _, werr := hc.Stdout.Write(cur.out); werr != nil {
					pipeErr = werr
					cancel()
					break
				}
				printedHunkFile = true
			}
		}
		if pipeErr != nil {
			break
		}
	}

	// Drain results channel if broken early so workers exit cleanly.
	for range results {
	}

	if pipeErr != nil {
		if isPipeClosed(pipeErr) {
			return pipeErr
		}
		return exitError{2}
	}
	if opts.quiet && anyMatch {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if hadErr.Load() {
		return exitError{2}
	}
	if anyMatch {
		return nil
	}
	return exitError{1}
}

func rgIsBinarySample(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	return bytes.IndexByte(probe, 0) >= 0
}

func rgSearchFile(
	ctx context.Context,
	job rgFileJob,
	label string,
	opts *rgOptions,
	matcher *rgMatcher,
	showFilename, showLineNum bool,
	out *bytes.Buffer,
) (bool, error) {
	var data []byte
	if job.absPath != os.DevNull {
		var err error
		data, err = fsReadShellFile(ctx, job.baseDir, "rg", job.rawPath)
		if err != nil {
			return false, fmt.Errorf("%s: %w", job.displayPath, err)
		}
	}
	if !opts.text && rgIsBinarySample(data) {
		return false, nil
	}

	// Whole-file SIMD bytes.Index prefilter!
	// When we have a required case-sensitive literal substring and are not inverting or passing through,
	// a single SIMD bytes.Index skips 98%+ of non-matching files with zero line-splitting!
	if len(matcher.prefilterLit) > 0 && !opts.invertMatch && !opts.passthru {
		if bytes.Index(data, matcher.prefilterLit) < 0 {
			if opts.filesWithoutMatch {
				writeRgFileMatchName(out, job.displayPath, opts.nullSep)
				return true, nil
			}
			return false, nil
		}
		// Ultra-fast path for -l / -q when pattern is an exact unrestricted literal without \n:
		if (opts.filesWithMatches || opts.quiet) && matcher.singleLit != nil &&
			!matcher.wordRegexp && !matcher.lineRegexp && bytes.IndexByte(matcher.singleLit, '\n') < 0 {
			if opts.filesWithMatches && !opts.quiet {
				writeRgFileMatchName(out, job.displayPath, opts.nullSep)
			}
			return true, nil
		}
	}

	return rgSearchBytes(ctx, data, job.displayPath, label, opts, matcher, showFilename, showLineNum, out), nil
}

func writeRgFileMatchName(out *bytes.Buffer, name string, nullSep bool) {
	out.WriteString(name)
	if nullSep {
		out.WriteByte(0)
	} else {
		out.WriteByte('\n')
	}
}

func rgSearchReader(
	ctx context.Context,
	r io.Reader,
	streamTo io.Writer,
	label string,
	opts *rgOptions,
	matcher *rgMatcher,
	showFilename, showLineNum bool,
	out *bytes.Buffer,
) (bool, int, error) {
	lr := newLineReader(r)
	displayName := label
	if displayName == "" {
		displayName = "<stdin>"
	}
	matched, matchedLines, serr := rgSearchStream(ctx, displayName, label, opts, matcher, showFilename, showLineNum, true, out, streamTo, func() ([]byte, bool) {
		if !lr.Scan() {
			return nil, false
		}
		line := strings.TrimSuffix(lr.Text(), "\r")
		if !opts.text && strings.IndexByte(line, 0) >= 0 {
			return nil, false
		}
		return []byte(line), true
	})
	if serr != nil {
		return matched, matchedLines, serr
	}
	if err := lr.Err(); err != nil {
		return matched, matchedLines, err
	}
	if ctx.Err() != nil {
		return matched, matchedLines, ctx.Err()
	}
	return matched, matchedLines, nil
}

func rgSearchBytes(
	ctx context.Context,
	data []byte,
	displayPath, label string,
	opts *rgOptions,
	matcher *rgMatcher,
	showFilename, showLineNum bool,
	out *bytes.Buffer,
) bool {
	pos := 0
	matched, _, _ := rgSearchStream(ctx, displayPath, label, opts, matcher, showFilename, showLineNum, false, out, nil, func() ([]byte, bool) {
		if pos >= len(data) {
			return nil, false
		}
		nl := bytes.IndexByte(data[pos:], '\n')
		var lineBytes []byte
		if nl < 0 {
			lineBytes = data[pos:]
			pos = len(data)
		} else {
			lineBytes = data[pos : pos+nl]
			pos += nl + 1
		}
		if len(lineBytes) > 0 && lineBytes[len(lineBytes)-1] == '\r' {
			lineBytes = lineBytes[:len(lineBytes)-1]
		}
		return lineBytes, true
	})
	return matched
}

func rgSearchStream(
	ctx context.Context,
	displayPath, label string,
	opts *rgOptions,
	matcher *rgMatcher,
	showFilename, showLineNum, copyRing bool,
	out *bytes.Buffer,
	streamTo io.Writer,
	nextLine func() ([]byte, bool),
) (bool, int, error) {
	type ctxLine struct {
		num  int
		text []byte
	}
	var beforeRing []ctxLine
	if opts.beforeCtx > 0 {
		beforeRing = make([]ctxLine, 0, opts.beforeCtx)
	}

	useContext := (opts.beforeCtx > 0 || opts.afterCtx > 0) &&
		!opts.filesWithMatches && !opts.filesWithoutMatch && !opts.count && !opts.vimgrep && !opts.onlyMatching && !opts.passthru

	flush := func() error {
		if streamTo == nil || out.Len() == 0 {
			return nil
		}
		var werr error
		if !opts.quiet {
			_, werr = streamTo.Write(out.Bytes())
		}
		out.Reset()
		return werr
	}

	lineNum := 0
	matchedLines := 0
	totalMatchSpans := 0
	afterRemaining := 0
	lastPrintedLine := 0
	emittedAnyHunk := false

	for {
		if (copyRing || lineNum&255 == 0) && ctx.Err() != nil {
			return matchedLines > 0, matchedLines, ctx.Err()
		}
		lineBytes, ok := nextLine()
		if !ok {
			break
		}
		lineNum++

		reachedMax := opts.maxCount > 0 && matchedLines >= opts.maxCount
		isMatch := false
		if !reachedMax {
			isMatch = matcher.matchLine(lineBytes) != opts.invertMatch
		}

		stopLoop := false
		if isMatch {
			matchedLines++
			if opts.quiet {
				return true, matchedLines, nil
			}
			if opts.filesWithMatches {
				writeRgFileMatchName(out, displayPath, opts.nullSep)
				return true, matchedLines, flush()
			}
			if opts.filesWithoutMatch {
				return false, matchedLines, nil
			}
			if opts.count {
				if opts.countMatches && !opts.invertMatch {
					totalMatchSpans += len(matcher.findAllSpans(lineBytes, 0))
				} else {
					totalMatchSpans++
				}
				if opts.maxCount > 0 && matchedLines >= opts.maxCount {
					break
				}
				continue
			}

			if useContext {
				firstToPrint := lineNum
				for _, bl := range beforeRing {
					if bl.num > lastPrintedLine {
						firstToPrint = bl.num
						break
					}
				}
				if emittedAnyHunk && firstToPrint > lastPrintedLine+1 {
					out.WriteString("--\n")
				}
				for _, bl := range beforeRing {
					if bl.num > lastPrintedLine {
						writeRgFormattedLine(out, label, showFilename, showLineNum, opts.nullSep, '-', bl.num, bl.text)
						lastPrintedLine = bl.num
					}
				}
				beforeRing = beforeRing[:0]
			}

			writeRgMatchingLine(out, lineBytes, lineNum, label, showFilename, showLineNum, opts, matcher)
			lastPrintedLine = lineNum
			emittedAnyHunk = true
			if useContext {
				afterRemaining = opts.afterCtx
			}
			if opts.maxCount > 0 && matchedLines >= opts.maxCount && afterRemaining == 0 && !opts.passthru {
				stopLoop = true
			}
		} else {
			if opts.passthru && !opts.filesWithMatches && !opts.filesWithoutMatch && !opts.count && !opts.quiet {
				writeRgFormattedLine(out, label, showFilename, showLineNum, opts.nullSep, '-', lineNum, lineBytes)
				lastPrintedLine = lineNum
			} else if useContext {
				if afterRemaining > 0 {
					writeRgFormattedLine(out, label, showFilename, showLineNum, opts.nullSep, '-', lineNum, lineBytes)
					lastPrintedLine = lineNum
					afterRemaining--
					if opts.maxCount > 0 && matchedLines >= opts.maxCount && afterRemaining == 0 {
						stopLoop = true
					}
				} else if opts.beforeCtx > 0 {
					if opts.maxCount > 0 && matchedLines >= opts.maxCount {
						break
					}
					saved := lineBytes
					if copyRing {
						saved = append([]byte(nil), lineBytes...)
					}
					if len(beforeRing) == opts.beforeCtx {
						copy(beforeRing, beforeRing[1:])
						beforeRing[len(beforeRing)-1] = ctxLine{num: lineNum, text: saved}
					} else {
						beforeRing = append(beforeRing, ctxLine{num: lineNum, text: saved})
					}
				}
			} else if opts.maxCount > 0 && matchedLines >= opts.maxCount {
				break
			}
		}

		if streamTo != nil && out.Len() > 0 {
			if werr := flush(); werr != nil {
				return matchedLines > 0, matchedLines, werr
			}
		}
		if stopLoop {
			break
		}
	}

	if opts.filesWithoutMatch {
		if matchedLines == 0 {
			writeRgFileMatchName(out, displayPath, opts.nullSep)
			return true, 0, flush()
		}
		return false, matchedLines, nil
	}
	if opts.count && totalMatchSpans > 0 {
		writeRgCountLine(out, label, showFilename, opts.nullSep, totalMatchSpans)
	}
	return matchedLines > 0, matchedLines, flush()
}

func writeRgCountLine(out *bytes.Buffer, label string, showFilename, nullSep bool, n int) {
	var numBuf [24]byte
	if showFilename && label != "" {
		out.WriteString(label)
		if nullSep {
			out.WriteByte(0)
		} else {
			out.WriteByte(':')
		}
	}
	out.Write(strconv.AppendInt(numBuf[:0], int64(n), 10))
	out.WriteByte('\n')
}

func writeRgFormattedLine(out *bytes.Buffer, label string, showFilename, showLineNum, nullSep bool, sep byte, lineNum int, text []byte) {
	var numBuf [24]byte
	if showFilename && label != "" {
		out.WriteString(label)
		if nullSep {
			out.WriteByte(0)
		} else {
			out.WriteByte(sep)
		}
	}
	if showLineNum {
		out.Write(strconv.AppendInt(numBuf[:0], int64(lineNum), 10))
		out.WriteByte(sep)
	}
	out.Write(text)
	out.WriteByte('\n')
}

func writeRgMatchingLine(
	out *bytes.Buffer,
	lineBytes []byte,
	lineNum int,
	label string,
	showFilename, showLineNum bool,
	opts *rgOptions,
	matcher *rgMatcher,
) {
	var numBuf [24]byte
	if opts.vimgrep {
		vLabel := label
		if vLabel == "" && showFilename {
			vLabel = "<stdin>"
		}
		if opts.invertMatch {
			if vLabel != "" {
				out.WriteString(vLabel)
				if opts.nullSep {
					out.WriteByte(0)
				} else {
					out.WriteByte(':')
				}
			}
			out.Write(strconv.AppendInt(numBuf[:0], int64(lineNum), 10))
			out.WriteString(":1:")
			out.Write(lineBytes)
			out.WriteByte('\n')
			return
		}
		spans := matcher.findAllSpans(lineBytes, 0)
		replacedLine := lineBytes
		if opts.hasReplace {
			replacedLine = matcher.replaceLine(lineBytes, spans, opts.replaceStr)
		}
		for _, sp := range spans {
			if vLabel != "" {
				out.WriteString(vLabel)
				if opts.nullSep {
					out.WriteByte(0)
				} else {
					out.WriteByte(':')
				}
			}
			out.Write(strconv.AppendInt(numBuf[:0], int64(lineNum), 10))
			out.WriteByte(':')
			out.Write(strconv.AppendInt(numBuf[:0], int64(sp.start+1), 10))
			out.WriteByte(':')
			out.Write(replacedLine)
			out.WriteByte('\n')
		}
		return
	}

	if opts.onlyMatching && !opts.invertMatch {
		spans := matcher.findAllSpans(lineBytes, 0)
		for _, sp := range spans {
			piece := lineBytes[sp.start:sp.end]
			if opts.hasReplace {
				piece = matcher.expandReplacement(lineBytes, sp, opts.replaceStr)
			}
			writeRgFormattedLine(out, label, showFilename, showLineNum, opts.nullSep, ':', lineNum, piece)
		}
		return
	}

	text := lineBytes
	if opts.hasReplace && !opts.invertMatch {
		spans := matcher.findAllSpans(lineBytes, 0)
		text = matcher.replaceLine(lineBytes, spans, opts.replaceStr)
	}
	writeRgFormattedLine(out, label, showFilename, showLineNum, opts.nullSep, ':', lineNum, text)
}
