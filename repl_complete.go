package main

// Interactive completion: Tab word completion over builtins, hashed
// commands, PATH executables, shell functions/aliases, variables and
// file paths. Single Tab completes (common prefix), double Tab lists.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// completeWord returns (replacement, alternatives) for the word under
// the cursor. replacement is "" when ambiguous (caller rings bell /
// shows alternatives on second Tab).
func completeWord(line string, pos int, r *interp.Runner, hcArgs func() []string) (string, []string) {
	wordStart, word := wordAt(line, pos)
	cands := candidatesFor(word, wordStart == 0, r)
	if len(cands) == 0 {
		return "", nil
	}
	if len(cands) == 1 {
		return cands[0], []string{cands[0]}
	}
	prefix := commonPrefix(cands)
	_ = wordStart
	if len(prefix) > len(word) {
		return prefix, cands
	}
	return "", cands
}

// wordAt finds the start of the word containing pos (space/tab and
// shell operators delimit; quotes group).
func wordAt(line string, pos int) (int, string) {
	if pos > len(line) {
		pos = len(line)
	}
	// Scan back to delimiter.
	i := pos
	for i > 0 {
		c := line[i-1]
		if c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')' || c == '<' || c == '>' {
			break
		}
		i--
	}
	return i, line[i:pos]
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
		for _, n := range pathCommands(word) {
			add(n)
		}
	}
	// File paths always (commands take args; later words are files).
	for _, n := range fileMatches(word) {
		add(n)
	}
	sort.Strings(out)
	return out
}

// builtinNames merges mvdan/sh builtins, unish extras and declared funcs.
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
		"umask", "unalias", "unset", "until", "wait", "while",
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
	}
	return out
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
	// r.Vars holds shell vars; writeEnv overlays Env — read via Params?
	// Use Funcs-adjacent exported map: Vars is exported on Runner.
	for n := range r.Vars {
		out = append(out, n)
	}
	for _, e := range os.Environ() {
		if i := strings.IndexByte(e, '='); i > 0 {
			out = append(out, e[:i])
		}
	}
	return out
}

func pathCommands(prefix string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		ents, err := os.ReadDir(dir)
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

// fileMatches completes word as a path (handles ~/ and dir/ prefix,
// appends / to directories like bash).
func fileMatches(word string) []string {
	dir, base := ".", word
	if i := strings.LastIndexAny(word, "/\\"); i >= 0 {
		dir, base = word[:i+1], word[i+1:]
		if dir == "" {
			dir = "/"
		}
	}
	// ~ expansion for completion scan only.
	scanDir := dir
	if strings.HasPrefix(dir, "~/") || dir == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			scanDir = h + dir[1:]
		}
	}
	ents, err := os.ReadDir(scanDir)
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
		if e.IsDir() {
			s += "/"
		}
		// Quote names with spaces like bash does (backslash).
		if strings.ContainsAny(n, " \t'\"()[]{}&;|$<>!#^*?") {
			s = dirQuote(s)
		}
		out = append(out, s)
	}
	return out
}

func dirQuote(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if strings.ContainsRune(" \t'\"()[]{}&;|$<>!#^*?", r) {
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

var _ = syntax.NewParser
var _ = filepath.Separator
