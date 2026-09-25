package main

// Interactive shell loop: TTY detection, prompt (PS1/PS2 +
// PROMPT_COMMAND + \[...\] + $ substitution + \#), rc files,
// multiline continuation (parser-driven), signal handling,
// bang expansion, history builtin, and graceful exit/EOF.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// repl holds interactive state shared across loop iterations.
type repl struct {
	r              *interp.Runner
	hist           *histStore
	histNew        []string // items added this session (for save)
	histIdx        int      // browsing index (len = live line)
	liveSaved      string   // stashed live line while browsing
	lastLine       string   // last accepted line (for live restore)
	searchMode     bool
	searchBuf      string
	searchRows     int    // rows occupied by the current search overlay
	searchSaved    string // line stashed when Ctrl-R started
	searchSavedPos int
	kbd            *keyReader
	out            io.Writer
	errOut         io.Writer
	inFile         *os.File
	rawOn          bool
	width          int
	exitCode       int
	shouldExit     bool
	lineNo         int
	searchIdx      int
	pendingVerify  bool
	plainNoPC      bool // plainLoop piped mode: skip PROMPT_COMMAND
	histVerify     bool
	histExpand     bool
	tabCount       int
	lastTab        string
	tabShown       bool
	killRing       []string // kill ring (newest last), bash-like
	killIdx        int      // yank pop position (-1 = not yanking)
	killLastLen    int      // runes inserted by last yank (for Alt-Y replace)
	undoStack      []undoState
	mu             sync.Mutex
}

// undoState snapshots the line for Ctrl-_ undo.
type undoState struct {
	text []rune
	pos  int
}

// envSnapshot copies the runner's live variables (writeEnv overlay is
// unexported, but a no-op run syncs the exported Vars map — Run ends
// with maps.Insert(r.Vars, r.writeEnv.Each), so after any command
// Vars holds the current shell state).
func (rp *repl) envSnapshot() map[string]expand.Variable {
	cp := map[string]expand.Variable{}
	if rp.r == nil || rp.r.Vars == nil {
		return cp
	}
	for k, v := range rp.r.Vars {
		cp[k] = v
	}
	return cp
}

func (rp *repl) getenv(key string) string {
	if rp.r != nil && rp.r.Vars != nil {
		if v, ok := rp.r.Vars[key]; ok && v.IsSet() {
			return v.Str
		}
	}
	return os.Getenv(key)
}

// prompt expands PS1/PS2: \[...\] (non-printing), \u \h \w \W \$ \#
// \! \d \t \@ \A \n \\, then $VAR/command substitution via the runner,
// then strips \[ \] markers for width math (bash keeps them zero-width).
func (rp *repl) prompt(primary bool) (display, raw string) {
	varKey := "PS1"
	// Distro-bashrc style default: green user@host, blue working dir.
	def := `\[\e[1;32m\]\u@\h\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]\$ `
	if !primary {
		varKey = "PS2"
		def = "> "
	}
	tmpl := rp.getenv(varKey)
	if tmpl == "" {
		tmpl = def
	}
	// PROMPT_COMMAND runs before each primary prompt (may set vars).
	// Skipped in piped plainLoop mode (plainNoPC) so scripts that set
	// PROMPT_COMMAND don't get unexpected output/side effects.
	if primary && !rp.plainNoPC {
		if pc := rp.getenv("PROMPT_COMMAND"); pc != "" {
			_ = rp.execLine(pc)
		}
	}
	rp.lineNo++
	s := tmpl
	// Protect \[...\] spans: replace with \x00 markers, expand rest.
	type span struct{ text string }
	var spans []span
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			c := s[i+1]
			switch c {
			case '[', ']':
				b.WriteByte(0)
				spans = append(spans, span{})
				// mark open/close by position parity; store kind in spans order
				spans[len(spans)-1] = span{text: string([]byte{c})}
				i += 2
				continue
			case 'u':
				b.WriteString(rp.promptUser())
				i += 2
				continue
			case 'h', 'H':
				if h, err := os.Hostname(); err == nil {
					if c == 'h' {
						if k := strings.IndexByte(h, '.'); k > 0 {
							h = h[:k]
						}
					}
					b.WriteString(h)
				}
				i += 2
				continue
			case 'w', 'W':
				b.WriteString(rp.promptDir(c == 'W'))
				i += 2
				continue
			case '$':
				if rp.exitCode != 0 {
					b.WriteString("#")
				} else {
					b.WriteString("$")
				}
				i += 2
				continue
			case '#':
				b.WriteString(strconv.Itoa(rp.lineNo))
				i += 2
				continue
			case '!':
				b.WriteString(strconv.Itoa(len(rp.hist.items) + 1))
				i += 2
				continue
			case 'd':
				b.WriteString(time.Now().Format("Mon Jan 02"))
				i += 2
				continue
			case 't':
				b.WriteString(time.Now().Format("15:04:05"))
				i += 2
				continue
			case 'T':
				b.WriteString(time.Now().Format("03:04:05"))
				i += 2
				continue
			case '@':
				b.WriteString(time.Now().Format("03:04 PM"))
				i += 2
				continue
			case 'A':
				b.WriteString(time.Now().Format("15:04"))
				i += 2
				continue
			case 'n':
				b.WriteByte('\n')
				i += 2
				continue
			case 'e', 'E':
				b.WriteByte(0x1b) // \e = ESC (colors: \[\e[32m\])
				i += 2
				continue
			case 'r':
				b.WriteByte('\r')
				i += 2
				continue
			case 'a':
				b.WriteByte('\a')
				i += 2
				continue
			case '\\':
				b.WriteByte('\\')
				i += 2
				continue
			default:
				b.WriteByte('\\')
				b.WriteByte(c)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	expanded := rp.expandPromptVars(b.String())
	// Reinsert spans: \x00 toggles in/out of non-printing.
	var disp, rawB strings.Builder
	inSpan := false
	spanText := ""
	for _, ch := range expanded {
		if ch == 0 {
			if !inSpan {
				inSpan = true
				spanText = ""
			} else {
				inSpan = false
				rawB.WriteString(spanText)
			}
			continue
		}
		if inSpan {
			spanText += string(ch)
			disp.WriteRune(ch)
		} else {
			disp.WriteRune(ch)
			rawB.WriteRune(ch)
		}
	}
	_ = spans
	return disp.String(), rawB.String()
}

func (rp *repl) promptUser() string {
	if u, err := userCurrent(); err == nil && u != "" {
		if i := strings.LastIndexAny(u, `\/`); i >= 0 {
			u = u[i+1:]
		}
		return u
	}
	return "user"
}

func (rp *repl) promptDir(short bool) string {
	d := rp.rDir()
	if h, err := os.UserHomeDir(); err == nil && h != "" && (d == h || strings.HasPrefix(d, h+"/")) {
		d = "~" + strings.TrimPrefix(d, h)
	}
	if short {
		if i := strings.LastIndex(d, "/"); i >= 0 && i+1 < len(d) {
			d = d[i+1:]
		}
	}
	return d
}

func (rp *repl) rDir() string {
	if rp.r != nil && rp.r.Dir != "" {
		return rp.r.Dir
	}
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "."
}

// expandPromptVars runs $VAR/“ substitution on the prompt against a
// snapshot of the live runner's environment (like bash: prompt sees
// current shell vars, PROMPT_COMMAND effects, functions and builtins).
// The snapshot is taken via a no-op `:` run that syncs r.Vars, then
// copied into the sub-runner (sharing the map would race with later
// writes; Vars entries are values so a shallow copy is enough).
func (rp *repl) expandPromptVars(s string) string {
	if !strings.Contains(s, "$") && !strings.Contains(s, "`") {
		return s
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader("printf '%s' \""+promptEscape(s)+"\""), "")
	if err != nil {
		return s
	}
	var out strings.Builder
	// Run a subshell runner sharing Vars/Funcs/Env with the live
	// runner (prompt may reference shell vars/functions).
	r2, err := interp.New(
		interp.Env(expand.ListEnviron()),
		interp.StdIO(nil, &out, io.Discard),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		return s
	}
	if rp.r != nil {
		snap := rp.envSnapshot()
		// Seed the sub-runner's ENV with the snapshot as exported pairs
		// (writeEnv overlay reads parent Env first; Vars alone is NOT
		// consulted by lookupVar). Unexported values stay shell-only.
		pairs := make([]string, 0, len(snap))
		for k, v := range snap {
			if v.Exported && v.IsSet() && v.Kind == expand.String {
				pairs = append(pairs, k+"="+v.Str)
			}
		}
		// Non-exported shell vars must ALSO be visible to the prompt
		// (bash expands $MYVAR in PS1 even when not exported): pass
		// them as pairs too — Env is only the prompt's reader here.
		for k, v := range snap {
			if !v.Exported && v.IsSet() && v.Kind == expand.String {
				pairs = append(pairs, k+"="+v.Str)
			}
		}
		r2.Vars = snap
		_ = pairs
		r2.Funcs = rp.r.Funcs
		r2.Dir = rp.r.Dir
		r2.Env = expand.ListEnviron(pairs...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r2.Run(ctx, prog); err != nil {
		return s
	}
	return out.String()
}

// promptEscape quotes s for embedding inside "..." while keeping
// $VAR/${VAR}/$(...)/`...` substitutions live (like bash PS1).
func promptEscape(s string) string {
	// Quote for embedding inside "...", keeping $VAR/${VAR}/$(...)/`...`
	// live. Strategy: protect backslash-escapes that are NOT parameter
	// expansion (\$, \`, \\, \" stay literal), double remaining
	// backslashes, escape double quotes, then restore the literals.
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ReplaceAll(s, `\$`, "\x00DS\x00")
	s = strings.ReplaceAll(s, "\\`", "\x00BT\x00")
	s = strings.ReplaceAll(s, `\\`, "\x00BS\x00")
	s = strings.ReplaceAll(s, `\"`, "\x00DQ\x00")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\x00BS\x00", `\\`)
	s = strings.ReplaceAll(s, "\x00DS\x00", `\$`)
	s = strings.ReplaceAll(s, "\x00BT\x00", "\\`")
	s = strings.ReplaceAll(s, "\x00DQ\x00", `\"`)
	return s
}

// execLine parses + runs one accepted (possibly multiline) line,
// returning the exit code. History-expand already applied by caller.
func (rp *repl) execLine(line string) int {
	prog, err := syntax.NewParser().Parse(strings.NewReader(line), "")
	if err != nil {
		fmt.Fprintln(rp.errOut, "bash:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err = rp.r.Run(ctx, prog)
	code := 0
	if err != nil {
		var es interp.ExitStatus
		if errors.As(err, &es) {
			code = int(es)
		} else if errors.Is(err, context.Canceled) {
			code = 130
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = 124
		} else {
			fmt.Fprintln(rp.errOut, "bash:", err)
			code = 1
		}
		if rp.r.Exited() {
			rp.shouldExit = true
		}
	}
	rp.exitCode = code
	return code
}

// incomplete reports whether src needs more input (unclosed quote,
// bracket, or trailing operator), via parser error text.
func incomplete(src string) bool {
	_, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sub := range []string{
		"reached EOF without",
		"reached end of file",
		"must be followed by",
		"unexpected token \";\"",
		"expected a command",
		"unclosed here-document",
		"must be followed by a statement list", // if/while/for/select/func bodies
		"must end with",                        // case...esac, coproc...}
		"must be followed by a case list",      // case x in <newline>
		"reached EOF in",                       // $(, $((, backquote, quotes variants
	} {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	// Trailing pipe/and/or/backslash continuations.
	t := strings.TrimRight(src, " \t\n")
	if strings.HasSuffix(t, "|") || strings.HasSuffix(t, "&&") || strings.HasSuffix(t, "||") {
		return true
	}
	if strings.HasSuffix(t, "\\") && !strings.HasSuffix(t, "\\\\") {
		return true
	}
	return false
}

// defaultRC is the starter ~/.unishrc written on the first interactive
// run (like a distro's default bashrc). Bash leans on /etc/bash.bashrc,
// but no system template can be guaranteed to exist — especially on
// Windows — so the shell ships its own.
const defaultRC = `# ~/.unishrc — sourced by unish at interactive startup (like ~/.bashrc).
# Created automatically on the first run; edit it freely.
# It has no effect on scripts or "unish -c".

# Handy aliases (colors on when writing to a terminal)
alias ll='ls -l --color=auto'
alias la='ls -la --color=auto'
alias l='ls --color=auto'
alias ls='ls --color=auto'
alias grep='grep --color=auto'

# Suggestions (uncomment to taste)
# alias ..='cd ..'
# alias g='git'
# export EDITOR=vim
# export PAGER=less
# The default prompt is already colored (green user@host, blue path);
# override it here if you want, e.g.:
# export PS1='\[\e[1;36m\]\u@\h\[\e[0m\] \w\$ '
`

// oldDefaultRC is the first version of the starter file; when the
// current ~/.unishrc is still exactly that, it is upgraded in place.
const oldDefaultRC = `# ~/.unishrc — sourced by unish at interactive startup (like ~/.bashrc).
# Created automatically on the first run; edit it freely.
# It has no effect on scripts or "unish -c".

# Handy aliases
alias ll='ls -l'
alias la='ls -la'
alias l='ls'

# Suggestions (uncomment to taste)
# alias grep='grep --color=auto'
# alias ..='cd ..'
# export EDITOR=vim
# export PAGER=less
`

// ensureRCFile writes the starter ~/.unishrc on the first interactive
// run; it never overwrites user content, but does upgrade a pristine
// copy of an older shipped template. Best-effort either way.
func (rp *repl) ensureRCFile() {
	h, err := os.UserHomeDir()
	if err != nil {
		return
	}
	p := filepath.Join(h, ".unishrc")
	data, err := os.ReadFile(p)
	if err == nil {
		if string(data) == oldDefaultRC {
			_ = os.WriteFile(p, []byte(defaultRC), 0o644)
		}
		return
	}
	_ = os.WriteFile(p, []byte(defaultRC), 0o644)
}

// rcFile sources ~/.unishrc when present (non-fatal).
func (rp *repl) rcFile() {
	h, err := os.UserHomeDir()
	if err != nil {
		return
	}
	p := filepath.Join(h, ".unishrc")
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader(string(data)), p)
	if err != nil {
		fmt.Fprintln(rp.errOut, "bash: ~/.unishrc:", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = rp.r.Run(ctx, prog)
}

var _ = filepath.Separator
