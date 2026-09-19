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
	// On Windows, stdin may be piped while CONIN$ exists (child with
	// redirected pipes): rawOnHandle stashes the console file in
	// conInFile and reads come from there.
	restore, ok := termRawOn()
	if !ok {
		return rp.plainLoop()
	}
	defer restore()
	if conInFile != nil {
		rp.inFile = conInFile
	}
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
	var promptDisp string
	rn := newLineRenderer(rp.out, "", rp.width)
	redraw := func() {}
	redraw = func() {
		rn.prompt = promptDisp
		rn.draw(lb.text, lb.pos)
	}
	for {
		promptDisp, _ = rp.prompt(true)
		rn.prompt = promptDisp
		rn.invalidate() // new prompt (PROMPT_COMMAND may change it)
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
		line, ctrl := rp.readEditedLine(&lb, rn, redraw)
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
			p2d, _ := rp.prompt(false)
			var lb2 lineBuf
			// Commit the current row: move to a fresh line so the
			// previous block is never redrawn over.
			fmt.Fprintln(rp.out)
			rn2 := newLineRenderer(rp.out, p2d, rp.width)
			rn2.draw(lb2.text, lb2.pos)
			cont, c2 := rp.readEditedLine(&lb2, rn2, func() {
				rn2.prompt = p2d
				rn2.draw(lb2.text, lb2.pos)
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
func (rp *repl) readEditedLine(lb *lineBuf, rn *lineRenderer, redraw func()) (string, readCtrl) {
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
				if rp.searchMode {
					rp.searchAccept(rn, lb)
					continue
				}
				return lb.String(), readOK
			case k.code == keyCtrlC:
				if rp.searchMode {
					rp.searchCancel(rn, lb, redraw)
					continue
				}
				rn.clear()
				fmt.Fprintln(rp.out)
				return "", readCtrlC
			case k.code == keyCtrlD:
				if len(lb.text) == 0 {
					return "", readCtrlD
				}
				lb.deleteAt()
				redraw()
			case k.code == keyBackspace:
				if rp.searchMode {
					rp.searchBackspace(rn, lb)
					continue
				}
				if lb.backspace() {
					redraw()
				} else {
					bell(rp.out)
				}
			case k.code == keyDelete:
				if lb.deleteAt() {
					redraw()
				} else {
					bell(rp.out)
				}
			case k.code == keyLeft || k.code == keyCtrlB:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				if lb.pos > 0 {
					lb.pos--
					redraw()
				} else {
					bell(rp.out)
				}
			case k.code == keyRight || k.code == keyCtrlF:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				if lb.pos < len(lb.text) {
					lb.pos++
					redraw()
				} else {
					bell(rp.out)
				}
			case k.code == keyHome || k.code == keyCtrlA:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				if lb.pos == 0 {
					bell(rp.out)
					continue
				}
				lb.pos = 0
				redraw()
			case k.code == keyEnd || k.code == keyCtrlE:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				if lb.pos == len(lb.text) {
					bell(rp.out)
					continue
				}
				lb.pos = len(lb.text)
				redraw()
			case k.code == keyUp || k.code == keyCtrlP:
				if rp.searchMode {
					rp.searchPrev(rn, lb)
					continue
				}
				if rp.histMove(-1, lb) {
					redraw()
				} else {
					bell(rp.out)
				}
			case k.code == keyDown || k.code == keyCtrlN:
				if rp.searchMode {
					rp.searchNext(rn, lb)
					continue
				}
				if rp.histMove(1, lb) {
					redraw()
				} else {
					bell(rp.out)
				}
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
				rp.searchStart(rn, lb)
			case k.code == keyCtrlS:
				if rp.searchMode {
					rp.searchNext(rn, lb)
				} else {
					bell(rp.out)
				}
			case k.code == keyTab:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.doTab(lb, rn, redraw)
				_ = killBuf
			case k.r != 0:
				if rp.searchMode {
					if k.code == keyEscape {
						rp.searchCancel(rn, lb, redraw)
						continue
					}
					rp.searchType(k.r, rn, lb)
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
// Reports whether the line changed (caller bells when false).
func (rp *repl) histMove(dir int, lb *lineBuf) bool {
	if len(rp.hist.items) == 0 {
		return false
	}
	if rp.histIdx == len(rp.hist.items) {
		rp.liveSaved = lb.String()
	}
	ni := rp.histIdx + dir
	if ni < 0 || ni > len(rp.hist.items) {
		return false
	}
	if ni == rp.histIdx {
		return false
	}
	rp.histIdx = ni
	lb.clear()
	if ni == len(rp.hist.items) {
		lb.insertStr(rp.liveSaved)
	} else {
		lb.insertStr(rp.hist.items[ni])
	}
	return true
}

// doTab performs completion: first Tab completes common prefix,
// second Tab on same word lists alternatives in columns.
func (rp *repl) doTab(lb *lineBuf, rn *lineRenderer, redraw func()) {
	line := lb.String()
	start, word := wordAt(line, lb.pos)
	repl, cands := completeWord(line, lb.pos, rp.r, nil)
	_ = start
	if len(cands) == 0 {
		bell(rp.out)
		return
	}
	if repl != "" && repl != word {
		// Replace word with completion.
		rest := line[lb.pos:]
		nl := line[:start] + repl + rest
		lb.text = []rune(nl)
		lb.pos = start + len([]rune(repl))
		redraw()
		rp.tabCount = 0
		// Trailing space for single matches (bash behavior); the
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
		bell(rp.out)
		return
	}
	// Second Tab: commit the current row, list in columns, redraw.
	rn.clear()
	fmt.Fprintln(rp.out)
	printCompletions(rp.out, cands, rp.width)
	rp.tabCount = 0
	rp.tabShown = true
	rn.invalidate()
	redraw()
}

// renderPrompt is kept for plainLoop-less callers (tests); the TTY
// loop now draws through lineRenderer.
func renderPrompt(out io.Writer, promptDisp, promptRaw string, lb *lineBuf, width int) {
	rn := newLineRenderer(out, promptDisp, width)
	rn.draw(lb.text, lb.pos)
	_ = promptRaw
}

// searchLine builds the Ctrl-R overlay text for the current state.
func (rp *repl) searchLine() string {
	m := rp.searchMatchIdx()
	if m >= 0 {
		return fmt.Sprintf("(reverse-i-search)`%s': %s", rp.searchBuf, rp.hist.items[m])
	}
	return fmt.Sprintf("(failed reverse-i-search)`%s': ", rp.searchBuf)
}

// searchRender draws the search overlay through the renderer: the
// prompt row is replaced by the search text, keeping the block shape
// managed by rn (no stray scrollback).
func (rp *repl) searchRender(rn *lineRenderer) {
	savedPrompt := rn.prompt
	rn.prompt = rp.searchLine()
	rn.invalidate()
	rn.draw(nil, 0)
	rn.prompt = savedPrompt
	rp.searchRows = rn.rows
}

// search (Ctrl-R) methods.
func (rp *repl) searchStart(rn *lineRenderer, lb *lineBuf) {
	rp.searchMode = true
	rp.searchBuf = ""
	rp.searchSaved = lb.String()
	rp.searchSavedPos = lb.pos
	rp.searchIdx = len(rp.hist.items)
	rn.clear()
	rp.searchRender(rn)
}

func (rp *repl) searchDraw(lb *lineBuf, redraw func()) {
	// Legacy redraw-callback form (kept for tests); live loop uses
	// searchRender through the renderer.
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
	return rp.searchMatchIdx()
}

// searchMatchIdx is the newest history entry containing the search
// buffer (or the newest entry when the buffer is empty).
func (rp *repl) searchMatchIdx() int {
	if rp.searchBuf == "" {
		if len(rp.hist.items) == 0 {
			return -1
		}
		return len(rp.hist.items) - 1
	}
	for i := len(rp.hist.items) - 1; i >= 0; i-- {
		if strings.Contains(rp.hist.items[i], rp.searchBuf) {
			return i
		}
	}
	return -1
}

func (rp *repl) searchType(r rune, rn *lineRenderer, lb *lineBuf) {
	rp.searchBuf += string(r)
	if m := rp.searchMatchIdx(); m >= 0 {
		rp.searchIdx = m
	}
	rp.searchRender(rn)
}

func (rp *repl) searchBackspace(rn *lineRenderer, lb *lineBuf) {
	if len(rp.searchBuf) > 0 {
		rr := []rune(rp.searchBuf)
		rp.searchBuf = string(rr[:len(rr)-1])
	}
	rp.searchRender(rn)
}

func (rp *repl) searchPrev(rn *lineRenderer, lb *lineBuf) {
	// Older match.
	for i := rp.searchIdx - 1; i >= 0; i-- {
		if rp.searchBuf == "" || strings.Contains(rp.hist.items[i], rp.searchBuf) {
			rp.searchIdx = i
			break
		}
	}
	rp.searchRender(rn)
}

func (rp *repl) searchNext(rn *lineRenderer, lb *lineBuf) {
	for i := rp.searchIdx + 1; i < len(rp.hist.items); i++ {
		if rp.searchBuf == "" || strings.Contains(rp.hist.items[i], rp.searchBuf) {
			rp.searchIdx = i
			break
		}
	}
	rp.searchRender(rn)
}

// searchAccept puts the current match on the editing line (Enter in
// search mode, like bash) and redraws through the renderer.
func (rp *repl) searchAccept(rn *lineRenderer, lb *lineBuf) {
	if m := rp.searchMatchIdx(); m >= 0 && rp.searchBuf != "" {
		lb.clear()
		lb.insertStr(rp.hist.items[m])
		rp.histIdx = m
	}
	rp.searchMode = false
	rp.searchBuf = ""
	rn.clear()
	rn.invalidate()
	rn.draw(lb.text, lb.pos)
}

// searchCancel aborts the search (Ctrl-C/Esc): restores the stashed
// line exactly as it was, with no history pollution.
func (rp *repl) searchCancel(rn *lineRenderer, lb *lineBuf, _ func()) {
	rp.searchMode = false
	rp.searchBuf = ""
	lb.clear()
	lb.insertStr(rp.searchSaved)
	lb.pos = rp.searchSavedPos
	if lb.pos < 0 {
		lb.pos = 0
	}
	if lb.pos > len(lb.text) {
		lb.pos = len(lb.text)
	}
	rp.histIdx = len(rp.hist.items)
	rn.clear()
	rn.invalidate()
	rn.draw(lb.text, lb.pos)
}

// acceptSearch puts the current search match (or typed text) on the line.
// Kept for tests; the live loop uses searchAccept.
func (rp *repl) acceptSearch(lb *lineBuf) {
	if m := rp.searchMatchIdx(); m >= 0 && rp.searchBuf != "" {
		lb.clear()
		lb.insertStr(rp.hist.items[m])
		rp.histIdx = m
	}
	rp.searchMode = false
	rp.searchBuf = ""
}

var _ = expand.ListEnviron
