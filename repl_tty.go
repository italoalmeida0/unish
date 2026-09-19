package main

// Interactive TTY loop: raw mode, prompt redraw (multiline-aware),
// emacs keymap dispatch, history browsing, Ctrl-R, Tab completion,
// fallback line reader for pipes/dumb terminals.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// isInteractive reports whether to start the REPL: stdin is a char
// device (terminal) or explicitly requested via -i.
func isInteractive(stdin *os.File) bool {
	if fi, err := stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		return true
	}
	return false
}

// runInteractive starts the full readline loop. Never returns an error
// to the caller that matters; exit code is communicated via os.Exit.
func runInteractive() int {
	rp := &repl{
		hist:   newHistStore(),
		kbd:    &keyReader{},
		out:    os.Stdout,
		errOut: os.Stderr,
		width:  80,
	}
	rp.inFile = os.Stdin
	// Runner with this process stdio (raw-mode stdin handled per-line).
	r, err := interp.New(
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.CallHandler(callOverride),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unish:", err)
		return 1
	}
	rp.r = r
	// Seed runner dir.
	if d, err := os.Getwd(); err == nil {
		r.Dir = d
	}
	rp.rcFile()
	// History file.
	histFile := rp.getenv("HISTFILE")
	if histFile == "" && os.Getenv("HISTFILE") == "" {
		histFile = defaultHistPath()
	} else if histFile == "" {
		histFile = os.Getenv("HISTFILE")
	}
	maxIn := histInt(rp.getenv("HISTSIZE"), 500)
	maxF := histInt(rp.getenv("HISTFILESIZE"), maxIn)
	if histFile == "/dev/null" {
		histFile = ""
	}
	rp.hist.maxIn = maxIn
	rp.hist.load(histFile, maxF)
	rp.histIdx = len(rp.hist.items)
	rp.histExpand = os.Getenv("HISTEXPAND") != "off"
	if v := rp.getenv("histexpand"); v == "off" {
		rp.histExpand = false
	}
	rp.histVerify = shoptEnabled(r, "histverify")

	// If stdin is not a TTY (piped), fall back to plain line reading.
	if !termIsTTY(os.Stdin) || os.Getenv("TERM") == "dumb" {
		return rp.plainLoop()
	}
	// Try raw mode; on failure fall back too.
	restore, ok := termRawOn()
	if !ok {
		return rp.plainLoop()
	}
	defer restore()
	rp.rawOn = true
	code := rp.ttyLoop()
	// Persist new history (append, like bash).
	rp.hist.save(rp.histNew)
	// Bash prints nothing extra on exit; ensure cursor on fresh line.
	return code
}

func histInt(s string, def int) int {
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return def
	}
	if n < 0 {
		return 0
	}
	return n
}

func shoptEnabled(r *interp.Runner, name string) bool {
	// No getter in mvdan/sh; track our own? Query via `shopt name`.
	// Cheap: default off; user rc may set -s histverify — detect by
	// running shopt and parsing output.
	if r == nil {
		return false
	}
	var out strings.Builder
	r2, err := interp.New(interp.StdIO(nil, &out, io.Discard))
	if err != nil {
		return false
	}
	r2.Vars = r.Vars
	r2.Funcs = r.Funcs
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	prog, err := syntax.NewParser().Parse(strings.NewReader("shopt "+name), "")
	if err != nil {
		return false
	}
	if err := r2.Run(ctx, prog); err != nil {
		return false
	}
	return strings.Contains(out.String(), "on")
}

// plainLoop reads lines without raw mode (pipes, dumb terms).
// It accumulates multiline input like the TTY loop (PS2 prompt when
// stdout is a TTY), so `unish < script.sh` keeps working.
// The runner inherits prompt env from the OS only (no rc side-effects
// leak into scripts): skip PROMPT_COMMAND in non-TTY mode to avoid
// surprising output, but still expand PS1 for prompts on a TTY.
func (rp *repl) plainLoop() int {
	br := newLineReader(os.Stdin)
	ttyOut := termIsTTY(os.Stdout)
	rp.plainNoPC = !ttyOut
	for {
		prompt, _ := rp.prompt(true)
		if ttyOut {
			fmt.Fprint(rp.out, prompt)
		}
		if !br.Scan() {
			if ttyOut {
				fmt.Fprintln(rp.out)
			}
			break
		}
		full := br.Text()
		for incomplete(full) {
			if ttyOut {
				p2, _ := rp.prompt(false)
				fmt.Fprint(rp.out, p2)
			}
			if !br.Scan() {
				break
			}
			full += "\n" + br.Text()
		}
		if full == "" || strings.TrimSpace(full) == "" {
			continue
		}
		code := rp.acceptLine(full)
		if rp.shouldExit {
			return code
		}
	}
	rp.hist.save(rp.histNew)
	return rp.exitCode
}

// acceptLine runs bang expansion, history save, and execution.
func (rp *repl) acceptLine(line string) int {
	if rp.histExpand && strings.Contains(line, "!") {
		exp, _, err := expandBang(line, rp.hist.items)
		if err != nil {
			fmt.Fprintln(rp.errOut, err.Error())
			rp.exitCode = 1
			return 1
		}
		if exp != line {
			fmt.Fprintln(rp.out, exp)
			if rp.histVerify {
				// Re-edit expanded line instead of executing.
				rp.lastLine = exp
				return -1
			}
			line = exp
		}
	}
	rp.hist.add(line, rp.getenv("HISTCONTROL"))
	rp.histNew = append(rp.histNew, line)
	rp.histIdx = len(rp.hist.items)
	rp.liveSaved = ""
	// history builtin needs access: register per-line? Handled via
	// callOverride consulting replHist global (set below).
	replHist = rp
	code := rp.execLine(line)
	if histsz := histInt(rp.getenv("HISTSIZE"), 0); histsz > 0 && len(rp.hist.items) > histsz {
		rp.hist.items = rp.hist.items[len(rp.hist.items)-histsz:]
	}
	return code
}

// ttyLoop is the raw-mode readline loop.
func (rp *repl) ttyLoop() int {
	var lb lineBuf
	var promptDisp, promptRaw string
	redraw := func() {}
	redraw = func() {
		renderPrompt(rp.out, promptDisp, promptRaw, &lb, rp.width)
	}
	for {
		promptDisp, promptRaw = rp.prompt(true)
		lb.clear()
		rp.histIdx = len(rp.hist.items)
		rp.liveSaved = ""
		rp.tabCount = 0
		// histverify re-edit?
		if rp.lastLine != "" && rp.pendingVerify {
			lb.insertStr(rp.lastLine)
			rp.lastLine = ""
			rp.pendingVerify = false
		}
		redraw()
		line, ctrl := rp.readEditedLine(&lb, promptDisp, promptRaw, redraw)
		if ctrl == readCtrlD && lb.String() == "" {
			fmt.Fprintln(rp.out)
			break
		}
		if ctrl == readCtrlC {
			rp.exitCode = 130
			continue
		}
		full := line
		// Multiline continuation.
		for incomplete(full) {
			p2d, p2r := rp.prompt(false)
			var lb2 lineBuf
			fmt.Fprintln(rp.out)
			renderPrompt(rp.out, p2d, p2r, &lb2, rp.width)
			cont, c2 := rp.readEditedLine(&lb2, p2d, p2r, func() {
				renderPrompt(rp.out, p2d, p2r, &lb2, rp.width)
			})
			if c2 == readCtrlC {
				full = ""
				rp.exitCode = 130
				break
			}
			if c2 == readCtrlD {
				break
			}
			full += "\n" + cont
		}
		fmt.Fprintln(rp.out)
		if full == "" {
			continue
		}
		if strings.TrimSpace(full) == "" {
			continue
		}
		code := rp.acceptLine(full)
		if code == -1 {
			// histverify: re-edit expanded line.
			rp.pendingVerify = true
			rp.lastLine = rp.histVerifyBuf()
			continue
		}
		if rp.shouldExit {
			return code
		}
	}
	return rp.exitCode
}

func (rp *repl) histVerifyBuf() string {
	if rp.lastLine != "" {
		s := rp.lastLine
		rp.lastLine = ""
		return s
	}
	return ""
}

type readCtrl int

const (
	readOK readCtrl = iota
	readCtrlC
	readCtrlD
)

// readEditedLine reads one (single-line buffer) edited line.
func (rp *repl) readEditedLine(lb *lineBuf, promptDisp, promptRaw string, redraw func()) (string, readCtrl) {
	var killBuf string
	tmp := make([]byte, 256)
	for {
		n, err := rp.inFile.Read(tmp)
		if err != nil {
			if err == io.EOF {
				return lb.String(), readCtrlD
			}
			continue
		}
		rp.kbd.feed(tmp[:n])
		for {
			k, ok := rp.kbd.next()
			if !ok {
				break
			}
			rp.tabCountReset(k)
			switch {
			case k.code == keyEnter:
				return lb.String(), readOK
			case k.code == keyCtrlC:
				fmt.Fprintln(rp.out)
				if rp.searchMode {
					rp.searchMode = false
					continue
				}
				return "", readCtrlC
			case k.code == keyCtrlD:
				if len(lb.text) == 0 {
					return "", readCtrlD
				}
				lb.deleteAt()
				redraw()
			case k.code == keyBackspace:
				if rp.searchMode {
					rp.searchBackspace(redraw)
					continue
				}
				lb.backspace()
				redraw()
			case k.code == keyDelete:
				lb.deleteAt()
				redraw()
			case k.code == keyLeft || k.code == keyCtrlB:
				if lb.pos > 0 {
					lb.pos--
					redraw()
				}
			case k.code == keyRight || k.code == keyCtrlF:
				if lb.pos < len(lb.text) {
					lb.pos++
					redraw()
				}
			case k.code == keyHome || k.code == keyCtrlA:
				lb.pos = 0
				redraw()
			case k.code == keyEnd || k.code == keyCtrlE:
				lb.pos = len(lb.text)
				redraw()
			case k.code == keyUp || k.code == keyCtrlP:
				if rp.searchMode {
					rp.searchPrev(redraw)
					continue
				}
				rp.histMove(-1, lb)
				redraw()
			case k.code == keyDown || k.code == keyCtrlN:
				if rp.searchMode {
					rp.searchNext(redraw)
					continue
				}
				rp.histMove(1, lb)
				redraw()
			case k.code == keyCtrlK:
				killBuf = lb.killToEnd()
				redraw()
			case k.code == keyCtrlU:
				killBuf = lb.killToStart()
				redraw()
			case k.code == keyCtrlW:
				killBuf = lb.killWordBack()
				redraw()
			case k.code == keyAltD:
				killBuf = lb.killWordFwd()
				redraw()
			case k.code == keyAltB || k.code == keyCtrlLeft:
				lb.moveWordBack()
				redraw()
			case k.code == keyAltF || k.code == keyCtrlRight:
				lb.moveWordFwd()
				redraw()
			case k.code == keyAltBackspace:
				killBuf = lb.killWordBack()
				redraw()
			case k.code == keyCtrlT:
				lb.transpose()
				redraw()
			case k.code == keyCtrlL:
				// Clear screen, redraw prompt+line.
				fmt.Fprint(rp.out, "\x1b[H\x1b[2J")
				redraw()
			case k.code == keyCtrlR:
				rp.searchStart(lb, redraw)
			case k.code == keyTab:
				rp.doTab(lb, redraw)
				_ = killBuf
			case k.code == keyCtrlC:
				return "", readCtrlC
			case k.r != 0:
				if rp.searchMode {
					rp.searchType(k.r, lb, redraw)
					continue
				}
				if k.code == keyEscape {
					continue // lone Alt ignored
				}
				lb.insert(k.r)
				redraw()
			default:
				// Unknown escape: ignore.
			}
		}
	}
}

func (rp *repl) tabCountReset(k vtKey) {
	if k.code != keyTab {
		rp.tabCount = 0
		rp.lastTab = ""
		rp.tabShown = false
	}
}

// histMove browses history (-1 older, +1 newer), stashing live line.
func (rp *repl) histMove(dir int, lb *lineBuf) {
	if len(rp.hist.items) == 0 {
		return
	}
	if rp.histIdx == len(rp.hist.items) {
		rp.liveSaved = lb.String()
	}
	ni := rp.histIdx + dir
	if ni < 0 || ni > len(rp.hist.items) {
		return
	}
	rp.histIdx = ni
	lb.clear()
	if ni == len(rp.hist.items) {
		lb.insertStr(rp.liveSaved)
	} else {
		lb.insertStr(rp.hist.items[ni])
	}
}

// doTab performs completion: first Tab completes common prefix,
// second Tab on same word lists alternatives.
func (rp *repl) doTab(lb *lineBuf, redraw func()) {
	line := lb.String()
	start, word := wordAt(line, lb.pos)
	repl, cands := completeWord(line, lb.pos, rp.r, nil)
	_ = start
	if len(cands) == 0 {
		return // bell? stay silent like bash with no matches (actually rings; skip)
	}
	if repl != "" && repl != word {
		// Replace word with completion.
		rest := line[lb.pos:]
		nl := line[:start] + repl + rest
		lb.text = []rune(nl)
		lb.pos = start + len([]rune(repl))
		redraw()
		rp.tabCount = 0
		// Trailing space for command/word completion like bash? Only
		// when single file/dir match without further ambiguity — the
		// candidate already includes "/" for dirs.
		if len(cands) == 1 && !strings.HasSuffix(repl, "/") {
			lb.insert(' ')
			redraw()
		}
		return
	}
	// Ambiguous.
	if rp.tabCount == 0 || rp.lastTab != word {
		rp.tabCount = 1
		rp.lastTab = word
		return // first Tab: show nothing (bash rings bell)
	}
	// Second Tab: list.
	fmt.Fprintln(rp.out)
	for _, c := range cands {
		fmt.Fprintln(rp.out, c)
	}
	rp.tabCount = 0
	rp.tabShown = true
	redraw()
}

// renderPrompt redraws prompt + buffer, positioning the cursor.
// Multiline: wraps by terminal width; cursor via absolute moves.
func renderPrompt(out io.Writer, promptDisp, promptRaw string, lb *lineBuf, width int) {
	// CR + clear to end, then full redraw (simplest correct).
	fmt.Fprint(out, "\r\x1b[K")
	fmt.Fprint(out, promptDisp)
	fmt.Fprint(out, lb.String())
	// Move cursor back from end to pos.
	back := len(lb.text) - lb.pos
	if back > 0 {
		// Move left by CELL width, not runes.
		cells := 0
		for _, r := range lb.text[lb.pos:] {
			cells += runeWidth(r)
		}
		if cells > 0 {
			fmt.Fprintf(out, "\x1b[%dD", cells)
		}
	}
	_ = promptRaw
	_ = width
}

// search (Ctrl-R) methods.
func (rp *repl) searchStart(lb *lineBuf, redraw func()) {
	rp.searchMode = true
	rp.searchBuf = ""
	rp.searchIdx = len(rp.hist.items)
	rp.searchDraw(lb, redraw)
}

func (rp *repl) searchDraw(lb *lineBuf, redraw func()) {
	// Draw search line in place of prompt redraw: simplest is to print
	// "(reverse-i-search)`text': matched" then restore on exit.
	fmt.Fprint(rp.out, "\r\x1b[K")
	match := rp.searchMatch()
	if match >= 0 {
		fmt.Fprintf(rp.out, "(reverse-i-search)`%s': %s", rp.searchBuf, rp.hist.items[match])
	} else {
		fmt.Fprintf(rp.out, "(failed reverse-i-search)`%s': ", rp.searchBuf)
	}
	_ = lb
	_ = redraw
}

func (rp *repl) searchMatch() int {
	if rp.searchBuf == "" {
		return len(rp.hist.items) - 1
	}
	for i := len(rp.hist.items) - 1; i >= 0; i-- {
		if strings.Contains(rp.hist.items[i], rp.searchBuf) {
			return i
		}
	}
	return -1
}

func (rp *repl) searchType(r rune, lb *lineBuf, redraw func()) {
	rp.searchBuf += string(r)
	m := rp.searchMatch()
	if m >= 0 {
		rp.searchIdx = m
	}
	rp.searchDraw(lb, redraw)
}

func (rp *repl) searchBackspace(redraw func()) {
	if len(rp.searchBuf) > 0 {
		rr := []rune(rp.searchBuf)
		rp.searchBuf = string(rr[:len(rr)-1])
	}
	rp.searchDraw(nil, redraw)
}

func (rp *repl) searchPrev(redraw func()) {
	// Older match.
	for i := rp.searchIdx - 1; i >= 0; i-- {
		if rp.searchBuf == "" || strings.Contains(rp.hist.items[i], rp.searchBuf) {
			rp.searchIdx = i
			break
		}
	}
	rp.searchDraw(nil, redraw)
}

func (rp *repl) searchNext(redraw func()) {
	for i := rp.searchIdx + 1; i < len(rp.hist.items); i++ {
		if rp.searchBuf == "" || strings.Contains(rp.hist.items[i], rp.searchBuf) {
			rp.searchIdx = i
			break
		}
	}
	rp.searchDraw(nil, redraw)
}

// acceptSearch puts the current search match (or typed text) on the line.
func (rp *repl) acceptSearch(lb *lineBuf) {
	if m := rp.searchMatch(); m >= 0 && rp.searchBuf != "" {
		lb.clear()
		lb.insertStr(rp.hist.items[m])
		rp.histIdx = m
	}
	rp.searchMode = false
	rp.searchBuf = ""
}

var _ = expand.ListEnviron
