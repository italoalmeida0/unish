package main

// Interactive TTY loop: raw mode, prompt redraw (multiline-aware),
// emacs keymap dispatch, history browsing, Ctrl-R, Tab completion,
// fallback line reader for pipes/dumb terminals.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
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
		width:  termWidth(),
	}
	rp.inFile = os.Stdin
	// Runner with this process stdio (raw-mode stdin handled per-line).
	r, err := interp.New(
		interp.Interactive(true),
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.CallHandler(rp.callHandler),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bash:", err)
		return 1
	}
	rp.r = r
	setActiveRunner(r)
	defer setActiveRunner(nil)
	replHist = rp
	defer func() {
		if replHist == rp {
			replHist = nil
		}
	}()
	// Seed runner dir.
	if d, err := os.Getwd(); err == nil {
		r.Dir = d
	}
	// First interactive run: offer a starter ~/.unishrc when the user
	// has none, like a distro's default bashrc.
	rp.ensureRCFile()
	rp.rcFile()
	if rp.shouldExit {
		return rp.exitCode
	}
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
	rp.syncHistBounds()
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
	rawSess, ok := startRawSession(os.Stdin)
	if !ok {
		return rp.plainLoop()
	}
	defer rawSess.close()
	if f := rawSess.inFile(); f != nil {
		rp.inFile = f
	}
	rp.raw = rawSess
	rp.rawOn = true

	// Ensure fatal signals (SIGTERM/SIGHUP) while idle at the prompt restore
	// terminal cooked mode and save history instead of leaving the TTY raw.
	var exitingSig atomic.Bool
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, trapSignals()...)
	sigDone := make(chan struct{})
	defer func() {
		signal.Stop(sigCh)
		close(sigDone)
	}()
	go func() {
		for {
			select {
			case <-sigDone:
				return
			case sig := <-sigCh:
				// When rawOn is false we are inside execLine, which manages its own signals.
				if !rp.rawOn {
					continue
				}
				if sig == os.Interrupt {
					continue
				}
				if rp.r != nil && rp.r.QueueSignal(sig) {
					continue
				}
				if exitingSig.CompareAndSwap(false, true) {
					rawSess.close()
					rp.hist.save(rp.histNew)
					os.Exit(1)
				}
			}
		}
	}()

	code := rp.ttyLoop()
	// Persist new history (append, like bash).
	rp.hist.save(rp.histNew)
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

func (rp *repl) callHandler(ctx context.Context, args []string) ([]string, error) {
	if rp != nil && len(args) > 0 && args[0] == "shopt" {
		if handled, outArgs, err := rp.handleShopt(ctx, args); handled {
			return outArgs, err
		}
	}
	return callOverride(ctx, args)
}

// handleShopt implements `shopt [-qsu] histverify` for the interactive REPL
// (mvdan/sh lists histverify in bashOptsTable with supported=false).
func (rp *repl) handleShopt(ctx context.Context, args []string) (bool, []string, error) {
	mode := ""
	quiet := false
	posixOpts := false
	var flagArgs []string
	i := 1
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "-" {
		f := args[i]
		flagArgs = append(flagArgs, f)
		i++
		if f == "--" {
			break
		}
		for _, ch := range f[1:] {
			switch ch {
			case 's':
				mode = "-s"
			case 'u':
				mode = "-u"
			case 'q':
				quiet = true
			case 'o':
				posixOpts = true
			default:
				return false, nil, nil
			}
		}
	}
	if posixOpts || i >= len(args) {
		return false, nil, nil
	}
	hasHistVerify := false
	var otherOpts []string
	for _, opt := range args[i:] {
		if opt == "histverify" {
			hasHistVerify = true
		} else {
			otherOpts = append(otherOpts, opt)
		}
	}
	if !hasHistVerify {
		return false, nil, nil
	}
	switch mode {
	case "-s":
		rp.histVerify = true
	case "-u":
		rp.histVerify = false
	default:
		if !quiet {
			hc := interp.HandlerCtx(ctx)
			state := "off"
			if rp.histVerify {
				state = "on"
			}
			fmt.Fprintf(hc.Stdout, "histverify\t%s\n", state)
		}
	}
	if len(otherOpts) == 0 {
		if mode == "" && !rp.histVerify {
			return true, []string{"false"}, nil
		}
		return true, []string{"true"}, nil
	}
	rem := make([]string, 0, 1+len(flagArgs)+len(otherOpts))
	rem = append(rem, "shopt")
	rem = append(rem, flagArgs...)
	rem = append(rem, otherOpts...)
	return true, rem, nil
}

func (rp *repl) rawNewline() {
	if rp.rawOn {
		fmt.Fprint(rp.out, "\r\n")
	} else {
		fmt.Fprintln(rp.out)
	}
}

func shoptEnabled(r *interp.Runner, name string) bool {
	if r == nil {
		return false
	}
	if name == "histverify" && replHist != nil && replHist.r == r && replHist.histVerify {
		return true
	}
	var out strings.Builder
	r2 := r.Subshell()
	_ = interp.StdIO(nil, &out, io.Discard)(r2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	prog, err := syntax.NewParser().Parse(strings.NewReader("shopt -q "+name), "")
	if err != nil {
		return false
	}
	return r2.Run(ctx, prog) == nil
}

// readPlainLine reads a single line ending in '\n' one byte at a time so
// no unread bytes belonging to `read` or child processes are buffered/stolen
// from the underlying file descriptor.
func readPlainLine(r io.Reader) (string, bool) {
	var buf []byte
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		if n > 0 {
			if one[0] == '\n' {
				if len(buf) > 0 && buf[len(buf)-1] == '\r' {
					buf = buf[:len(buf)-1]
				}
				return string(buf), true
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			if len(buf) > 0 {
				if buf[len(buf)-1] == '\r' {
					buf = buf[:len(buf)-1]
				}
				return string(buf), true
			}
			return "", false
		}
	}
}

// plainLoop reads lines without raw mode (pipes, dumb terms).
// It accumulates multiline input like the TTY loop (PS2 prompt when
// stdout is a TTY), so `unish < script.sh` keeps working.
func (rp *repl) plainLoop() int {
	in := rp.inFile
	if in == nil {
		in = os.Stdin
	}
	ttyOut := termIsTTY(os.Stdout)
	rp.plainNoPC = !ttyOut
	for {
		prompt, _ := rp.prompt(true)
		if rp.shouldExit {
			return rp.exitCode
		}
		if ttyOut {
			fmt.Fprint(rp.out, prompt)
		}
		line, ok := readPlainLine(in)
		if !ok {
			if ttyOut {
				fmt.Fprintln(rp.out)
			}
			break
		}
		full := line
		for incomplete(full) {
			if ttyOut {
				p2, _ := rp.prompt(false)
				fmt.Fprint(rp.out, p2)
			}
			cont, ok2 := readPlainLine(in)
			if !ok2 {
				break
			}
			full += "\n" + cont
		}
		if full == "" || strings.TrimSpace(full) == "" {
			continue
		}
		code := rp.acceptLine(full)
		if rp.shouldExit {
			return code
		}
	}
	rp.runExitTrap()
	rp.hist.save(rp.histNew)
	return rp.exitCode
}

// acceptLine runs bang expansion, history save, and execution.
func (rp *repl) acceptLine(line string) int {
	if rp.histExpand && strings.Contains(line, "!") {
		exp, _, err := expandBang(line, rp.hist.items)
		if err != nil {
			if rp.rawOn {
				fmt.Fprint(rp.errOut, err.Error()+"\r\n")
			} else {
				fmt.Fprintln(rp.errOut, err.Error())
			}
			rp.exitCode = 1
			return 1
		}
		if exp != line {
			fmt.Fprint(rp.out, exp)
			rp.rawNewline()
			if rp.histVerify && (rp.inTTY || rp.rawOn || rp.raw != nil) {
				// Re-edit expanded line instead of executing.
				rp.lastLine = exp
				return -1
			}
			line = exp
		}
	}
	rp.recordHist(line)
	rp.syncHistBounds()
	rp.liveSaved = ""
	replHist = rp
	code := rp.execLine(line)
	if v := rp.getenv("HISTSIZE"); v != "" {
		histsz := histInt(v, 0)
		rp.hist.maxIn = histsz
		if histsz == 0 {
			rp.hist.items = nil
		} else if len(rp.hist.items) > histsz {
			rp.hist.items = rp.hist.items[len(rp.hist.items)-histsz:]
		}
		rp.syncHistBounds()
	}
	if strings.Contains(line, "shopt") {
		rp.histVerify = shoptEnabled(rp.r, "histverify")
	}
	if v := rp.getenv("histexpand"); v != "" {
		rp.histExpand = v != "off"
	}
	return code
}

// ttyLoop is the raw-mode readline loop.
func (rp *repl) ttyLoop() int {
	rp.inTTY = true
	defer func() { rp.inTTY = false }()
	var lb lineBuf
	var promptDisp string
	rn := newLineRenderer(rp.out, "", rp.width)
	redraw := func() {}
	redraw = func() {
		rn.prompt = promptDisp
		rn.draw(lb.text, lb.pos)
	}
	for {
		if rp.raw != nil {
			rp.width = termWidth()
			rn.width = rp.width
		}
		promptDisp, _ = rp.prompt(true)
		if rp.shouldExit {
			return rp.exitCode
		}
		rn.prompt = promptDisp
		rn.invalidate() // new prompt (PROMPT_COMMAND may change it)
		lb.clear()
		rp.syncHistBounds()
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
			rp.rawNewline()
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
			rp.rawNewline()
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
				if cont != "" {
					full += "\n" + cont
				}
				break
			}
			full += "\n" + cont
		}
		rp.rawNewline()
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
	rp.runExitTrap()
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

// kill pushes text onto the kill ring (bash appends consecutive kills;
// we keep it simple: every kill pushes, capped at 32 entries).
func (rp *repl) kill(s string) {
	if s == "" {
		return
	}
	rp.killRing = append(rp.killRing, s)
	if len(rp.killRing) > 32 {
		rp.killRing = rp.killRing[len(rp.killRing)-32:]
	}
	rp.killIdx = -1
	rp.killLastLen = 0
}

// pushUndo snapshots the line before a mutating key (cap 64).
func (rp *repl) pushUndo(lb *lineBuf) {
	snap := undoState{text: append([]rune(nil), lb.text...), pos: lb.pos}
	n := len(rp.undoStack)
	if n > 0 && string(rp.undoStack[n-1].text) == string(snap.text) && rp.undoStack[n-1].pos == snap.pos {
		return
	}
	rp.undoStack = append(rp.undoStack, snap)
	if len(rp.undoStack) > 64 {
		rp.undoStack = rp.undoStack[len(rp.undoStack)-64:]
	}
}

func (rp *repl) doUndo(lb *lineBuf) bool {
	n := len(rp.undoStack)
	if n == 0 {
		return false
	}
	s := rp.undoStack[n-1]
	rp.undoStack = rp.undoStack[:n-1]
	lb.text = append([]rune(nil), s.text...)
	if s.pos < 0 {
		s.pos = 0
	}
	if s.pos > len(lb.text) {
		s.pos = len(lb.text)
	}
	lb.pos = s.pos
	return true
}

// anyEdit resets yank-pop state: a non-yank key breaks the Alt-Y chain.
func (rp *repl) anyEdit() {
	rp.killIdx = -1
	rp.killLastLen = 0
}

// insertPaste inserts bracketed-paste content literally: newlines become
// real newlines in the buffer (multiline continuation handles them via
// the normal incomplete() path after submit), tabs stay tabs (no
// completion), and no history expansion applies.
func (rp *repl) insertPaste(lb *lineBuf, s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	// Strip a single trailing newline terminals often include.
	s = strings.TrimSuffix(s, "\n")
	lb.insertStr(s)
}

// readEditedLine reads one (single-line buffer) edited line.
func (rp *repl) readEditedLine(lb *lineBuf, rn *lineRenderer, redraw func()) (string, readCtrl) {
	defer func() { rp.searchMode = false }()
	rp.killIdx = -1
	rp.killLastLen = 0
	rp.undoStack = nil
	var pasting bool
	var pasteBuf strings.Builder
	var one [1]byte
	for {
		for {
			k, ok := rp.kbd.next()
			if !ok {
				break
			}
			rp.tabCountReset(k)
			// Bracketed paste: consume literally (no submit, no Tab
			// completion, no history expansion mid-paste).
			if pasting {
				if k.code == keyPasteEnd {
					pasting = false
					rp.pushUndo(lb)
					rp.insertPaste(lb, pasteBuf.String())
					rp.anyEdit()
					redraw()
				} else if k.code == keyEnter {
					if len(k.bytes) > 0 {
						pasteBuf.Write(k.bytes)
					} else {
						pasteBuf.WriteByte('\n')
					}
				} else if k.code == keyTab {
					pasteBuf.WriteByte('\t')
				} else if k.r != 0 {
					pasteBuf.WriteRune(k.r)
				}
				continue
			}
			switch {
			case k.code == keyPasteStart:
				if rp.searchMode {
					rp.searchCancel(rn, lb, redraw)
				}
				pasting = true
				pasteBuf.Reset()
				continue
			case k.code == keyPasteEnd:
				continue // stray end marker: ignore
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
				rp.rawNewline()
				return "", readCtrlC
			case k.code == keyCtrlD:
				if rp.searchMode {
					rp.searchCancel(rn, lb, redraw)
					continue
				}
				if len(lb.text) == 0 {
					return "", readCtrlD
				}
				rp.pushUndo(lb)
				if lb.deleteAt() {
					rp.anyEdit()
					redraw()
				} else {
					rp.undoStack = rp.undoStack[:len(rp.undoStack)-1]
					bell(rp.out)
				}
			case k.code == keyBackspace:
				if rp.searchMode {
					rp.searchBackspace(rn, lb)
					continue
				}
				rp.pushUndo(lb)
				if lb.backspace() {
					rp.anyEdit()
					redraw()
				} else {
					rp.undoStack = rp.undoStack[:len(rp.undoStack)-1]
					bell(rp.out)
				}
			case k.code == keyDelete:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				if lb.deleteAt() {
					rp.anyEdit()
					redraw()
				} else {
					rp.undoStack = rp.undoStack[:len(rp.undoStack)-1]
					bell(rp.out)
				}
			case k.code == keyLeft || k.code == keyCtrlB:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.anyEdit()
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
				rp.anyEdit()
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
				rp.anyEdit()
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
				rp.anyEdit()
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
				rp.pushUndo(lb)
				if rp.histMove(-1, lb) {
					rp.anyEdit()
					redraw()
				} else {
					rp.undoStack = rp.undoStack[:len(rp.undoStack)-1]
					bell(rp.out)
				}
			case k.code == keyDown || k.code == keyCtrlN:
				if rp.searchMode {
					rp.searchNext(rn, lb)
					continue
				}
				rp.pushUndo(lb)
				if rp.histMove(1, lb) {
					rp.anyEdit()
					redraw()
				} else {
					rp.undoStack = rp.undoStack[:len(rp.undoStack)-1]
					bell(rp.out)
				}
			case k.code == keyCtrlK:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				rp.kill(lb.killToEnd())
				rp.anyEdit()
				redraw()
			case k.code == keyCtrlU:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				rp.kill(lb.killToStart())
				rp.anyEdit()
				redraw()
			case k.code == keyCtrlW:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				rp.kill(lb.killWordBack())
				rp.anyEdit()
				redraw()
			case k.code == keyAltD:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				rp.kill(lb.killWordFwd())
				rp.anyEdit()
				redraw()
			case k.code == keyAltB || k.code == keyCtrlLeft:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.anyEdit()
				lb.moveWordBack()
				redraw()
			case k.code == keyAltF || k.code == keyCtrlRight:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.anyEdit()
				lb.moveWordFwd()
				redraw()
			case k.code == keyAltBackspace:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				rp.kill(lb.killWordBack())
				rp.anyEdit()
				redraw()
			case k.code == keyCtrlT:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				lb.transpose()
				rp.anyEdit()
				redraw()
			case k.code == keyCtrlY:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				if len(rp.killRing) == 0 {
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				s := rp.killRing[len(rp.killRing)-1]
				rp.killIdx = len(rp.killRing) - 1
				rp.killLastLen = len([]rune(s))
				lb.insertStr(s)
				redraw()
			case k.code == keyAltY:
				if rp.searchMode || rp.killIdx < 0 || len(rp.killRing) == 0 ||
					lb.pos < rp.killLastLen || lb.pos > len(lb.text) {
					rp.anyEdit()
					bell(rp.out)
					continue
				}
				rp.pushUndo(lb)
				startPos := lb.pos - rp.killLastLen
				lb.text = append(lb.text[:startPos], lb.text[lb.pos:]...)
				lb.pos = startPos
				rp.killIdx--
				if rp.killIdx < 0 {
					rp.killIdx = len(rp.killRing) - 1
				}
				s := rp.killRing[rp.killIdx]
				rp.killLastLen = len([]rune(s))
				lb.insertStr(s)
				redraw()
			case k.code == keyCtrlUnderscore:
				if rp.searchMode {
					bell(rp.out)
					continue
				}
				if rp.doUndo(lb) {
					rp.anyEdit()
					redraw()
				} else {
					bell(rp.out)
				}
			case k.code == keyCtrlL:
				// Clear screen, invalidate renderer state, and redraw prompt+line.
				fmt.Fprint(rp.out, "\x1b[H\x1b[2J")
				if rp.searchMode {
					rp.searchRender(rn)
				} else {
					rn.invalidate()
					redraw()
				}
			case k.code == keyCtrlR:
				if rp.searchMode {
					rp.searchPrev(rn, lb)
				} else {
					rp.anyEdit()
					rp.searchStart(rn, lb)
				}
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
				rp.pushUndo(lb)
				lb.insert(k.r)
				rp.anyEdit()
				redraw()
			default:
				// Unknown escape: ignore.
			}
		}
		if rp.inFile == nil {
			return lb.String(), readCtrlD
		}
		n, err := rp.inFile.Read(one[:])
		if n > 0 {
			rp.kbd.feed(one[:n])
			continue
		}
		if err != nil {
			// Treat io.EOF as well as PTY hangup (EIO on Linux) / closed pipe as EOF
			// instead of spinning at 100% CPU.
			return lb.String(), readCtrlD
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
	if rp.histIdx > len(rp.hist.items) {
		rp.histIdx = len(rp.hist.items)
	}
	if rp.histIdx < 0 {
		rp.histIdx = 0
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
	lb.clamp()
	line := lb.String()
	start, word := wordAt(line, lb.pos)
	repl, cands := completeWord(line, lb.pos, rp.r, nil)
	if len(cands) == 0 {
		bell(rp.out)
		return
	}
	if repl != "" && repl != word {
		// Replace word with completion using rune offsets (never byte slicing).
		rp.pushUndo(lb)
		rp.anyEdit()
		if start < 0 {
			start = 0
		}
		if start > lb.pos {
			start = lb.pos
		}
		prefix := string(lb.text[:start])
		rest := string(lb.text[lb.pos:])
		replRunes := []rune(repl)
		lb.text = []rune(prefix + repl + rest)
		lb.pos = start + len(replRunes)
		lb.clamp()
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
	rp.rawNewline()
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

func (rp *repl) findSearchMatch(from, dir int) int {
	n := len(rp.hist.items)
	if n == 0 {
		return -1
	}
	if dir < 0 {
		if from >= n {
			from = n - 1
		}
		for i := from; i >= 0; i-- {
			if rp.searchBuf == "" || strings.Contains(rp.hist.items[i], rp.searchBuf) {
				return i
			}
		}
		return -1
	}
	if from < 0 {
		from = 0
	}
	for i := from; i < n; i++ {
		if rp.searchBuf == "" || strings.Contains(rp.hist.items[i], rp.searchBuf) {
			return i
		}
	}
	return -1
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
	rp.searchIdx = len(rp.hist.items) - 1
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

// searchMatchIdx returns the active search match index (honoring Ctrl-R /
// Up / Down navigation in searchMode), or scans newest-first when outside
// searchMode.
func (rp *repl) searchMatchIdx() int {
	if rp.searchMode {
		if rp.searchIdx >= 0 && rp.searchIdx < len(rp.hist.items) {
			if rp.searchBuf == "" || strings.Contains(rp.hist.items[rp.searchIdx], rp.searchBuf) {
				return rp.searchIdx
			}
		}
		return -1
	}
	return rp.findSearchMatch(len(rp.hist.items)-1, -1)
}

func (rp *repl) searchType(r rune, rn *lineRenderer, lb *lineBuf) {
	rp.searchBuf += string(r)
	anchor := rp.searchIdx
	if anchor < 0 || anchor >= len(rp.hist.items) {
		anchor = len(rp.hist.items) - 1
	}
	rp.searchIdx = rp.findSearchMatch(anchor, -1)
	rp.searchRender(rn)
	_ = lb
}

func (rp *repl) searchBackspace(rn *lineRenderer, lb *lineBuf) {
	if len(rp.searchBuf) > 0 {
		rr := []rune(rp.searchBuf)
		rp.searchBuf = string(rr[:len(rr)-1])
	}
	rp.searchIdx = rp.findSearchMatch(len(rp.hist.items)-1, -1)
	rp.searchRender(rn)
	_ = lb
}

func (rp *repl) searchPrev(rn *lineRenderer, lb *lineBuf) {
	// Older match.
	if m := rp.findSearchMatch(rp.searchIdx-1, -1); m >= 0 {
		rp.searchIdx = m
	} else {
		bell(rp.out)
	}
	rp.searchRender(rn)
	_ = lb
}

func (rp *repl) searchNext(rn *lineRenderer, lb *lineBuf) {
	if m := rp.findSearchMatch(rp.searchIdx+1, 1); m >= 0 {
		rp.searchIdx = m
	} else {
		bell(rp.out)
	}
	rp.searchRender(rn)
	_ = lb
}

// searchAccept puts the current match on the editing line (Enter in
// search mode, like bash) and redraws through the renderer.
func (rp *repl) searchAccept(rn *lineRenderer, lb *lineBuf) {
	if m := rp.searchMatchIdx(); m >= 0 && (rp.searchBuf != "" || m != len(rp.hist.items)-1) {
		rp.pushUndo(lb)
		rp.anyEdit()
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
	lb.clamp()
	rp.syncHistBounds()
	rn.clear()
	rn.invalidate()
	rn.draw(lb.text, lb.pos)
}

// acceptSearch puts the current search match (or typed text) on the line.
// Kept for tests; the live loop uses searchAccept.
func (rp *repl) acceptSearch(lb *lineBuf) {
	if m := rp.searchMatchIdx(); m >= 0 && (rp.searchBuf != "" || m != len(rp.hist.items)-1) {
		rp.pushUndo(lb)
		rp.anyEdit()
		lb.clear()
		lb.insertStr(rp.hist.items[m])
		rp.histIdx = m
	}
	rp.searchMode = false
	rp.searchBuf = ""
}

var _ = expand.ListEnviron
