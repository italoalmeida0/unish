package unish

// Interactive completion: Tab word completion over builtins, hashed
// commands, PATH executables, shell functions/aliases, variables and
// file paths. Single Tab completes (common prefix), double Tab lists.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

var (
	runnerCompletionHooks sync.Map // map[*interp.Runner]CapabilityHook
	runnerEnvIsolated     sync.Map // map[*interp.Runner]bool
)

func registerRunnerCompletionHook(r *interp.Runner, h CapabilityHook) {
	if r == nil {
		return
	}
	if h == nil {
		runnerCompletionHooks.Delete(r)
		return
	}
	runnerCompletionHooks.Store(r, h)
}

func unregisterRunnerCompletionHook(r *interp.Runner) {
	if r != nil {
		runnerCompletionHooks.Delete(r)
	}
}

func registerRunnerEnvIsolated(r *interp.Runner, isolated bool) {
	if r == nil {
		return
	}
	if !isolated {
		runnerEnvIsolated.Delete(r)
		return
	}
	runnerEnvIsolated.Store(r, true)
}

func unregisterRunnerEnvIsolated(r *interp.Runner) {
	if r != nil {
		runnerEnvIsolated.Delete(r)
	}
}

func isRunnerEnvIsolated(r *interp.Runner) bool {
	if r == nil {
		return false
	}
	v, ok := runnerEnvIsolated.Load(r)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func completionContextForRunner(ctx context.Context, r *interp.Runner) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if CapabilityHookFromContext(ctx) != nil || r == nil {
		return ctx
	}
	if v, ok := runnerCompletionHooks.Load(r); ok {
		if h, ok2 := v.(CapabilityHook); ok2 && h != nil {
			return WithCapabilityHook(ctx, h)
		}
	}
	return ctx
}

// completeWord returns (replacement, alternatives) for the word under
// the cursor (pos is a rune index into line). replacement is "" when
// ambiguous (caller rings bell / shows alternatives on second Tab).
func completeWord(line string, pos int, r *interp.Runner, hcArgs func() []string) (string, []string) {
	return completeWordCtx(context.Background(), line, pos, r, hcArgs)
}

func completeWordCtx(ctx context.Context, line string, pos int, r *interp.Runner, hcArgs func() []string) (string, []string) {
	_ = hcArgs
	ctx = completionContextForRunner(ctx, r)
	rr := []rune(line)
	wordStart, word := wordAt(line, pos)
	cands := candidatesForCtx(ctx, word, isCommandPos(rr, wordStart), r)
	if len(cands) == 0 {
		return "", nil
	}
	if len(cands) == 1 {
		return cands[0], []string{cands[0]}
	}
	prefix := commonPrefix(cands)
	if len(prefix) > len(word) {
		return prefix, cands
	}
	return "", cands
}

func isWordDelimRune(c rune) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')' || c == '<' || c == '>'
}

func isEscapedAt(rr []rune, idx int) bool {
	bs := 0
	for j := idx - 1; j >= 0 && rr[j] == '\\'; j-- {
		bs++
	}
	return bs%2 == 1
}

// isCommandPos reports whether wordStart sits in command position
// (start of line or immediately following |, &, ;, (, or newline).
func isCommandPos(rr []rune, wordStart int) bool {
	i := wordStart - 1
	for i >= 0 && (rr[i] == ' ' || rr[i] == '\t') && !isEscapedAt(rr, i) {
		i--
	}
	if i < 0 {
		return true
	}
	c := rr[i]
	return !isEscapedAt(rr, i) && (c == ';' || c == '|' || c == '&' || c == '(' || c == '\n')
}

// wordAt finds the rune start index and text of the word containing rune
// index pos (unescaped space/tab and shell operators delimit).
func wordAt(line string, pos int) (int, string) {
	rr := []rune(line)
	if pos < 0 {
		pos = 0
	}
	if pos > len(rr) {
		pos = len(rr)
	}
	i := pos
	for i > 0 {
		if isWordDelimRune(rr[i-1]) && !isEscapedAt(rr, i-1) {
			break
		}
		i--
	}
	return i, string(rr[i:pos])
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		n := 0
		for n < len(p) && n < len(s) && p[n] == s[n] {
			n++
		}
		if n < len(p) {
			for n > 0 && !utf8.RuneStart(p[n]) {
				n--
			}
		} else if n < len(s) {
			for n > 0 && !utf8.RuneStart(s[n]) {
				n--
			}
		}
		p = p[:n]
		if p == "" {
			break
		}
	}
	return p
}

// candidatesFor lists completions for word; firstWord enables command
// names (builtins, PATH), otherwise files + variables.
func candidatesFor(word string, firstWord bool, r *interp.Runner) []string {
	return candidatesForCtx(completionContextForRunner(context.Background(), r), word, firstWord, r)
}

func candidatesForCtx(ctx context.Context, word string, firstWord bool, r *interp.Runner) []string {
	ctx = completionContextForRunner(ctx, r)
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if strings.HasPrefix(word, "$") {
		prefix := word[1:]
		br := "{"
		_ = br
		// ${VAR...} form.
		if strings.HasPrefix(prefix, "{") {
			prefix = prefix[1:]
			for _, v := range shellVarNames(r) {
				if strings.HasPrefix(v, prefix) {
					add("${" + v + "}")
				}
			}
			sort.Strings(out)
			return out
		}
		for _, v := range shellVarNames(r) {
			if strings.HasPrefix(v, prefix) {
				add("$" + v)
			}
		}
		sort.Strings(out)
		return out
	}
	if firstWord {
		// Builtins (mvdan/sh + ours).
		for _, n := range builtinNames(r) {
			if strings.HasPrefix(n, word) {
				add(n)
			}
		}
		// PATH executables.
		for _, n := range pathCommandsCtx(ctx, word, r) {
			add(n)
		}
	}
	// File paths always (commands take args; later words are files).
	for _, n := range fileMatchesWithRunnerCtx(ctx, word, r) {
		add(n)
	}
	sort.Strings(out)
	return out
}

// builtinNames merges mvdan/sh builtins, unish extras, declared funcs and aliases.
func builtinNames(r *interp.Runner) []string {
	var out []string
	// Static list mirrors interp builtin table + our extras.
	for _, n := range []string{
		":", ".", "source", "alias", "unalias", "bg", "bind", "break",
		"builtin", "caller", "case", "cd", "command", "compgen", "complete",
		"compactivate", "continue", "coproc", "declare", "dirs", "disown",
		"echo", "enable", "eval", "exec", "exit", "export", "false",
		"fc", "fg", "for", "function", "getopts", "hash", "help",
		"history", "if", "jobs", "kill", "let", "local", "logout",
		"mapfile", "popd", "printf", "pushd", "pwd", "read", "readarray",
		"return", "select", "set", "shift", "shopt", "suspend", "test",
		"time", "times", "trap", "true", "type", "typeset", "ulimit",
		"umask", "unset", "until", "wait", "while",
	} {
		if interp.IsBuiltin(n) || isUnishCommand(n) || lookupExtra(n) != nil {
			out = append(out, n)
		}
	}
	for _, n := range extraCommandNames() {
		out = append(out, n)
	}
	if r != nil {
		for n := range r.Funcs {
			out = append(out, n)
		}
		out = append(out, aliasNames(r)...)
	}
	return out
}

func aliasNames(r *interp.Runner) []string {
	if r == nil {
		return nil
	}
	var out strings.Builder
	r2 := r.Subshell()
	_ = interp.StdIO(nil, &out, io.Discard)(r2)
	prog, err := syntax.NewParser().Parse(strings.NewReader("compgen -a"), "")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r2.Run(ctx, prog); err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names
}

func extraCommandNames() []string {
	out := make([]string, 0, len(extraCommands))
	for i := range extraCommands {
		out = append(out, extraCommands[i].name)
	}
	return out
}

func shellVarNames(r *interp.Runner) []string {
	var out []string
	if r == nil {
		for _, e := range os.Environ() {
			if i := strings.IndexByte(e, '='); i > 0 {
				out = append(out, e[:i])
			}
		}
		return out
	}
	for n, v := range r.Vars {
		if v.IsSet() {
			out = append(out, n)
		}
	}
	if !isRunnerEnvIsolated(r) {
		for _, e := range os.Environ() {
			if i := strings.IndexByte(e, '='); i > 0 {
				name := e[:i]
				if v, exists := r.Vars[name]; exists && !v.IsSet() {
					continue
				}
				out = append(out, name)
			}
		}
	}
	return out
}

func pathCommands(prefix string, r *interp.Runner) []string {
	return pathCommandsCtx(completionContextForRunner(context.Background(), r), prefix, r)
}

func pathCommandsCtx(ctx context.Context, prefix string, r *interp.Runner) []string {
	ctx = completionContextForRunner(ctx, r)
	path := ""
	rDir := ""
	if r != nil {
		rDir = r.Dir
		if r.Vars != nil {
			if v, ok := r.Vars["PATH"]; ok && v.IsSet() {
				path = v.Str
			}
		}
	}
	if path == "" && !isRunnerEnvIsolated(r) {
		path = os.Getenv("PATH")
	}
	if path == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		ents, err := fsReadDir(ctx, rDir, "complete", dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			n := e.Name()
			if !strings.HasPrefix(n, prefix) || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

const compQuoteMeta = " \t'\"()[]{}&;|$<>!#^*?"

func unescapeCompWord(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	rr := []rune(s)
	var sb strings.Builder
	for i := 0; i < len(rr); i++ {
		if rr[i] == '\\' && i+1 < len(rr) && (rr[i+1] == '\\' || strings.ContainsRune(compQuoteMeta, rr[i+1])) {
			i++
			sb.WriteRune(rr[i])
			continue
		}
		sb.WriteRune(rr[i])
	}
	return sb.String()
}

// fileMatches completes word as a path against CWD (kept for callers without Runner).
func fileMatches(word string) []string {
	return fileMatchesWithRunner(word, nil)
}

// fileMatchesWithRunner completes word as a path relative to r.Dir
// (handles ~/ and dir/ prefix, escaped spaces, and appends / to dirs).
func fileMatchesWithRunner(word string, r *interp.Runner) []string {
	return fileMatchesWithRunnerCtx(completionContextForRunner(context.Background(), r), word, r)
}

func fileMatchesWithRunnerCtx(ctx context.Context, word string, r *interp.Runner) []string {
	ctx = completionContextForRunner(ctx, r)
	rawWord := unescapeCompWord(word)
	dir, base := ".", rawWord
	if i := strings.LastIndexAny(rawWord, "/\\"); i >= 0 {
		dir, base = rawWord[:i+1], rawWord[i+1:]
		if dir == "" {
			dir = "/"
		}
	}
	rDir := ""
	if r != nil {
		rDir = r.Dir
	}
	// ~ expansion and Runner.Dir resolution for completion scan.
	scanDir := dir
	if strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, "~\\") || dir == "~" {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			scanDir = h + dir[1:]
		}
	} else if !filepath.IsAbs(scanDir) && rDir != "" {
		if scanDir == "." {
			scanDir = rDir
		} else {
			scanDir = filepath.Join(rDir, scanDir)
		}
	}
	ents, err := fsReadDir(ctx, rDir, "complete", scanDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, base) {
			continue
		}
		s := dir + n
		if dir == "." {
			s = n
		}
		isDir := e.IsDir()
		if !isDir && e.Type()&os.ModeSymlink != 0 {
			if fi, err := fsStat(ctx, rDir, "complete", filepath.Join(scanDir, n)); err == nil && fi.IsDir() {
				isDir = true
			}
		}
		if isDir {
			s += "/"
		}
		// Quote shell metacharacters (preserving leading ~/ unescaped).
		tildePrefix := ""
		rest := s
		if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, "~\\") {
			tildePrefix = s[:2]
			rest = s[2:]
		}
		if strings.ContainsAny(rest, compQuoteMeta) {
			s = tildePrefix + dirQuote(rest)
		}
		out = append(out, s)
	}
	return out
}

func dirQuote(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if strings.ContainsRune(compQuoteMeta, r) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

var _ = syntax.NewParser
var _ = filepath.Separator
