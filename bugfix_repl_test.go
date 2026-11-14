package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// TestBugKeyReaderAltBackspacePanic verifies Alt+Backspace (\x1b\x7f) does not
// panic with slice bounds out of range or swallow the following byte, Ctrl-H
// (0x08) maps to backspace, and unhandled C0 controls are not inserted as text.
func TestBugKeyReaderAltBackspacePanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("feedKeys(\\x1b\\x7f) panicked: %v", r)
		}
	}()
	ks := feedKeys("\x1b\x7f")
	if len(ks) != 1 || ks[0].code != keyAltBackspace {
		t.Fatalf("Alt+Backspace = %+v, want [keyAltBackspace]", ks)
	}

	// Must not eat the byte immediately following \x1b\x7f.
	ks2 := feedKeys("\x1b\x7fa")
	if len(ks2) != 2 || ks2[0].code != keyAltBackspace || ks2[1].r != 'a' {
		t.Fatalf("Alt+Backspace+'a' = %+v", ks2)
	}

	// ASCII BS / Ctrl-H (0x08) and Alt+Ctrl-H (\x1b\x08).
	ks3 := feedKeys("\x08\x1b\x08")
	if len(ks3) != 2 || ks3[0].code != keyBackspace || ks3[1].code != keyAltBackspace {
		t.Fatalf("Ctrl-H / Alt+Ctrl-H = %+v", ks3)
	}

	// Unhandled C0 controls (NUL, Ctrl-Z, Ctrl-]) must not produce printable r != 0.
	for _, b := range []byte{0x00, 0x1a, 0x1d} {
		got := feedKeys(string([]byte{b}))
		if len(got) != 1 || got[0].r != 0 {
			t.Fatalf("control 0x%02x decoded as printable rune: %+v", b, got)
		}
	}
}

// TestBugKeyReaderCSIPrivateAndIntermediate verifies ECMA-48 CSI sequences
// with private parameters (?, <, >) or intermediate bytes (space) do not leak
// trailing parameter/final bytes as literal input characters.
func TestBugKeyReaderCSIPrivateAndIntermediate(t *testing.T) {
	ks := feedKeys("\x1b[?2004h\x1b[<0;10;20M\x1b[2 qok")
	var typed strings.Builder
	for _, k := range ks {
		if k.r != 0 && k.code == keyNone {
			typed.WriteRune(k.r)
		}
	}
	if typed.String() != "ok" {
		t.Fatalf("CSI leaked literal characters: got %q, want %q (keys=%+v)", typed.String(), "ok", ks)
	}
}

// TestBugTabCompleteUTF8PanicAndCorruption verifies Tab completion with
// multi-byte UTF-8 runes before the cursor does not panic or corrupt UTF-8,
// and commonPrefix never slices mid-UTF-8 rune.
func TestBugTabCompleteUTF8PanicAndCorruption(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "zfile.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := mustRunner(t)
	r.Dir = tmp
	rp := &repl{
		r:      r,
		hist:   newHistStore(),
		kbd:    &keyReader{},
		out:    io.Discard,
		errOut: io.Discard,
		width:  80,
	}

	// Case 1: "echo 中文     " (rune len 12, byte len 16) + Tab in a 1-file dir
	// previously set lb.pos > len(lb.text) and panicked inside doTab on lb.insert(' ').
	var lb lineBuf
	lb.insertStr("echo 中文     ")
	rn := newLineRenderer(io.Discard, "$ ", 80)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("doTab panicked on UTF-8 input: %v", rec)
			}
		}()
		rp.doTab(&lb, rn, func() { rn.draw(lb.text, lb.pos) })
	}()
	if lb.pos < 0 || lb.pos > len(lb.text) {
		t.Fatalf("invalid cursor pos %d for len %d", lb.pos, len(lb.text))
	}
	if lb.String() != "echo 中文     zfile.txt " {
		t.Fatalf("UTF-8 doTab got %q, want %q", lb.String(), "echo 中文     zfile.txt ")
	}

	// Case 2: Completing prefix after UTF-8 ("ls 中文 zf" -> "ls 中文 zfile.txt ").
	lb.clear()
	lb.insertStr("ls 中文 zf")
	rp.doTab(&lb, rn, func() { rn.draw(lb.text, lb.pos) })
	if lb.String() != "ls 中文 zfile.txt " {
		t.Fatalf("UTF-8 prefix doTab got %q, want %q", lb.String(), "ls 中文 zfile.txt ")
	}

	// Case 3: commonPrefix on multi-byte runes sharing lead bytes must not slice mid-rune.
	if got := commonPrefix([]string{"é1", "è2"}); got != "" {
		t.Fatalf("commonPrefix([é1, è2]) = %q (%x), want empty", got, []byte(got))
	}
	if got := commonPrefix([]string{"中a", "东b"}); got != "" {
		t.Fatalf("commonPrefix([中a, 东b]) = %q (%x), want empty", got, []byte(got))
	}
}

// TestBugTabCompleteRunnerDirAndEscapedSpaces verifies completion uses r.Dir
// after cd, handles backslash-escaped spaces without double-escaping, and
// completes commands after pipes/operators/leading spaces.
func TestBugTabCompleteRunnerDirAndEscapedSpaces(t *testing.T) {
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "my dir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "hello world.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := mustRunner(t)
	r.Dir = tmp // simulate `cd <tmp>` without os.Chdir

	// First Tab completes "my dir/" -> `my\ dir/`.
	rep, cands := completeWord("ls my", 5, r, nil)
	if rep != `my\ dir/` || len(cands) != 1 {
		t.Fatalf("complete dir in r.Dir: rep=%q cands=%v", rep, cands)
	}

	// Second Tab inside `ls my\ dir/he` completes `my\ dir/hello\ world.txt` without double-escaping.
	line2 := `ls my\ dir/he`
	rep2, cands2 := completeWord(line2, len([]rune(line2)), r, nil)
	want2 := `my\ dir/hello\ world.txt`
	if rep2 != want2 || len(cands2) != 1 {
		t.Fatalf("nested escaped space complete: rep=%q cands=%v, want %q", rep2, cands2, want2)
	}

	// Command completion after pipe and leading whitespace.
	_, pipeCands := completeWord("echo hi | pr", 12, r, nil)
	foundPrintf := false
	for _, c := range pipeCands {
		if c == "printf" {
			foundPrintf = true
		}
	}
	if !foundPrintf {
		t.Fatalf("expected command completion after pipe to include printf, got %v", pipeCands)
	}
}

// TestBugYankPopBoundsPanic verifies Alt-Y after non-yank cursor/history/delete
// actions does not panic with slice bounds out of range.
func TestBugYankPopBoundsPanic(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	rp := &repl{
		hist:   newHistStore(),
		kbd:    &keyReader{},
		inFile: pr,
		out:    io.Discard,
		errOut: io.Discard,
		width:  80,
	}
	rp.hist.add("hi", "")
	rp.histIdx = len(rp.hist.items)
	rp.kill("first-long-entry")
	rp.kill("second-long-entry")

	// Ctrl-Y (yank 17 runes), Up arrow (switches line to "hi", len 2), Alt-Y, Enter.
	go func() {
		_, _ = pw.Write([]byte("\x19\x1b[A\x1by\r"))
		_ = pw.Close()
	}()

	var lb lineBuf
	rn := newLineRenderer(io.Discard, "$ ", 80)
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Alt-Y after history move panicked: %v", rec)
		}
	}()
	got, ctrl := rp.readEditedLine(&lb, rn, func() { rn.draw(lb.text, lb.pos) })
	if ctrl != readOK || got != "hi" {
		t.Fatalf("got=%q ctrl=%v, want %q", got, ctrl, "hi")
	}
}

// TestBugBracketedPasteTabsAndCRLF verifies bracketed paste preserves literal
// tabs and normalizes CRLF to a single LF instead of doubling newlines.
func TestBugBracketedPasteTabsAndCRLF(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	rp := &repl{
		hist:   newHistStore(),
		kbd:    &keyReader{},
		inFile: pr,
		out:    io.Discard,
		errOut: io.Discard,
		width:  80,
	}
	go func() {
		_, _ = pw.Write([]byte("\x1b[200~a\tb\r\nc\r\n\x1b[201~\r"))
		_ = pw.Close()
	}()

	var lb lineBuf
	rn := newLineRenderer(io.Discard, "$ ", 80)
	got, ctrl := rp.readEditedLine(&lb, rn, func() { rn.draw(lb.text, lb.pos) })
	if ctrl != readOK || got != "a\tb\nc" {
		t.Fatalf("bracketed paste got %q (ctrl=%v), want %q", got, ctrl, "a\tb\nc")
	}
}

type eioReader struct {
	data []byte
	err  error
}

func (e *eioReader) Read(p []byte) (int, error) {
	if len(e.data) > 0 {
		n := copy(p, e.data)
		e.data = e.data[n:]
		return n, nil
	}
	return 0, e.err
}

// TestBugReadEditedLineErrorSpinAndTypeahead verifies:
// 1. Non-EOF read errors (like Linux PTY EIO on hangup) terminate instead of spinning forever.
// 2. Multiple lines or `read` builtin input in the same stream are not dropped or stolen.
func TestBugReadEditedLineErrorSpinAndTypeahead(t *testing.T) {
	// 1. Closed *os.File returns a non-io.EOF error (*fs.PathError); must not spin infinitely.
	prClosed, pwClosed, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = pwClosed.Close()
	_ = prClosed.Close() // Read now returns os.ErrClosed (err != io.EOF)!

	rp := &repl{
		hist:   newHistStore(),
		kbd:    &keyReader{},
		inFile: prClosed,
		out:    io.Discard,
		errOut: io.Discard,
		width:  80,
	}
	done := make(chan readCtrl, 1)
	go func() {
		var lb lineBuf
		rn := newLineRenderer(io.Discard, "$ ", 80)
		_, ctrl := rp.readEditedLine(&lb, rn, func() {})
		done <- ctrl
	}()
	select {
	case ctrl := <-done:
		if ctrl != readCtrlD {
			t.Fatalf("closed stdin returned ctrl=%v, want readCtrlD", ctrl)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("readEditedLine hung spinning on non-EOF Read error!")
	}

	// 2. ttyLoop with `read x` followed by data line then `echo got:$x` on shared stdin pipe.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	var outBuf bytes.Buffer
	r, err := interp.New(
		interp.StdIO(pr, &outBuf, io.Discard),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	rp2 := &repl{
		r:      r,
		hist:   newHistStore(),
		kbd:    &keyReader{},
		inFile: pr,
		out:    io.Discard,
		errOut: io.Discard,
		width:  80,
	}
	go func() {
		_, _ = pw.Write([]byte("read x\nhello_tty\necho got:$x\n"))
		_ = pw.Close()
	}()
	ttyDone := make(chan int, 1)
	go func() {
		ttyDone <- rp2.ttyLoop()
	}()
	select {
	case <-ttyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ttyLoop hung on multi-line input / read builtin!")
	}
	if !strings.Contains(outBuf.String(), "got:hello_tty") {
		t.Fatalf("ttyLoop stole stdin from read builtin; out=%q", outBuf.String())
	}
}

// TestBugPlainLoopDoesNotStealChildStdin verifies plainLoop reads unbuffered
// lines from rp.inFile without stealing subsequent stdin lines meant for `read`.
func TestBugPlainLoopDoesNotStealChildStdin(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	var outBuf bytes.Buffer
	r, err := interp.New(
		interp.StdIO(pr, &outBuf, io.Discard),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	rp := &repl{
		r:      r,
		hist:   newHistStore(),
		kbd:    &keyReader{},
		inFile: pr,
		out:    io.Discard,
		errOut: io.Discard,
		width:  80,
	}
	go func() {
		_, _ = pw.Write([]byte("read linevar\r\nhello from pipe\r\necho RESULT:$linevar\r\n"))
		_ = pw.Close()
	}()
	rp.plainLoop()
	if !strings.Contains(outBuf.String(), "RESULT:hello from pipe") {
		t.Fatalf("plainLoop stole child stdin or mishandled CRLF: out=%q", outBuf.String())
	}
}

type mockRawCtl struct {
	suspends int
	resumes  int
	active   bool
}

func (m *mockRawCtl) suspend() {
	m.suspends++
	m.active = false
}

func (m *mockRawCtl) resume() {
	m.resumes++
	m.active = true
}

func (m *mockRawCtl) close() {
	m.active = false
}

func (m *mockRawCtl) inFile() *os.File { return nil }

// TestBugRawModeLifecycleExitZeroAndExitTrap verifies:
// 1. Raw mode is suspended during command execution and resumed afterwards (and stays suspended on exit).
// 2. `exit` / `exit 0` sets shouldExit and exits the loop with code 0.
// 3. `trap ... EXIT` does NOT fire after every interactive command, only on shell exit.
// 4. `PROMPT_COMMAND` does not overwrite `rp.exitCode` of the previous command.
// 5. Interactive mode enables alias expansion across REPL lines.
func TestBugRawModeLifecycleExitZeroAndExitTrap(t *testing.T) {
	var outBuf bytes.Buffer
	rp := &repl{
		hist:   newHistStore(),
		kbd:    &keyReader{},
		out:    io.Discard,
		errOut: io.Discard,
		rawOn:  true,
		width:  80,
	}
	rawMock := &mockRawCtl{active: true}
	rp.raw = rawMock

	var sawRawActiveDuringCmd bool
	r, err := interp.New(
		interp.Interactive(true),
		interp.StdIO(nil, &outBuf, io.Discard),
		interp.CallHandler(func(ctx context.Context, args []string) ([]string, error) {
			if len(args) > 0 && args[0] == "echo" {
				if rawMock.active || rp.isRawOn() {
					sawRawActiveDuringCmd = true
				}
			}
			return rp.callHandler(ctx, args)
		}),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	rp.r = r

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	rp.inFile = pr

	replHist = rp
	defer func() { replHist = nil }()

	// 1. PROMPT_COMMAND must not clobber rp.exitCode.
	_ = rp.execLine("PROMPT_COMMAND='true'")
	_ = rp.execLine("false")
	if rp.exitCode != 1 {
		t.Fatalf("after false: exitCode=%d, want 1", rp.exitCode)
	}
	_, _ = rp.prompt(true)
	if rp.exitCode != 1 {
		t.Fatalf("PROMPT_COMMAND clobbered exitCode to %d, want 1", rp.exitCode)
	}
	if !rawMock.active || !rp.isRawOn() {
		t.Fatal("expected raw mode to be resumed after non-exiting execLine")
	}

	// 2. Alias expansion works across lines, EXIT trap runs only on exit, and `exit 0` leaves raw suspended.
	go func() {
		_, _ = pw.Write([]byte("alias saymid='echo MID'\ntrap 'echo ON_EXIT' EXIT\nsaymid\nexit 0\necho UNREACHABLE\n"))
		_ = pw.Close()
	}()
	code := rp.ttyLoop()
	if code != 0 {
		t.Fatalf("exit 0 returned code=%d, want 0", code)
	}
	if sawRawActiveDuringCmd {
		t.Fatal("raw mode remained active while executing foreground command!")
	}
	if rawMock.active || rp.isRawOn() {
		t.Fatal("raw mode was re-enabled after `exit 0`!")
	}
	got := outBuf.String()
	if strings.Contains(got, "UNREACHABLE") {
		t.Fatalf("`exit 0` failed to exit ttyLoop! output=%q", got)
	}
	if got != "MID\nON_EXIT\n" {
		t.Fatalf("EXIT trap / alias output wrong: got %q, want %q", got, "MID\nON_EXIT\n")
	}
}

// TestBugIncompleteBackslashAndSyntaxErrors verifies trailing backslash
// continuations are detected as incomplete while genuine syntax errors do not
// trap the REPL in an infinite PS2 continuation loop.
func TestBugIncompleteBackslashAndSyntaxErrors(t *testing.T) {
	// Trailing unescaped backslash on an otherwise valid command MUST be incomplete.
	for _, src := range []string{
		`echo hello \`,
		`echo hello \\\`,
		`echo "hi" \`,
	} {
		if !incomplete(src) {
			t.Fatalf("expected incomplete(%q) == true", src)
		}
	}

	// Escaped backslash, single-quoted backslash, or comment backslash MUST be complete.
	for _, src := range []string{
		`echo hello \\`,
		`echo 'hello\'`,
		"echo hi # trailing comment \\",
	} {
		if incomplete(src) {
			t.Fatalf("expected incomplete(%q) == false", src)
		}
	}

	// Real mid-line syntax errors MUST NOT be treated as incomplete (would hang PS2 forever).
	for _, bad := range []string{
		"if ; then",
		"ls > | cat",
		"echo | | cat",
	} {
		if incomplete(bad) {
			t.Fatalf("syntax error %q wrongly reported as incomplete (infinite PS2 trap)", bad)
		}
	}
}

// TestBugSearchCtrlRCyclingAndCtrlLRedraw verifies Ctrl-R / Up cycles through
// older matches, Ctrl-L forces a redraw, and layoutCells wraps wide runes properly.
func TestBugSearchCtrlRCyclingAndCtrlLRedraw(t *testing.T) {
	rp := &repl{
		hist:   newHistStore(),
		kbd:    &keyReader{},
		out:    io.Discard,
		errOut: io.Discard,
		width:  80,
	}
	rp.hist.add("echo first", "")
	rp.hist.add("echo second", "")

	var sb strings.Builder
	rn := newLineRenderer(&sb, "$ ", 80)
	var lb lineBuf
	rp.searchStart(rn, &lb)
	rp.searchType('e', rn, &lb)
	rp.searchType('c', rn, &lb)
	// Move to older match via searchPrev, then accept.
	rp.searchPrev(rn, &lb)
	rp.searchAccept(rn, &lb)
	if lb.String() != "echo first" {
		t.Fatalf("Ctrl-R searchPrev+accept got %q, want %q", lb.String(), "echo first")
	}

	// Pressing Ctrl-R again while already in searchMode must cycle to older match, not wipe searchBuf.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	var screenBuf bytes.Buffer
	rp.inFile = pr
	rp.out = &screenBuf
	go func() {
		// Ctrl-R, "echo", Ctrl-R (older), Enter (accept), Ctrl-L (clear+redraw), Enter (submit).
		_, _ = pw.Write([]byte("\x12echo\x12\r\x0c\r"))
		_ = pw.Close()
	}()
	lb.clear()
	rn2 := newLineRenderer(&screenBuf, "$ ", 80)
	rn2.draw(lb.text, lb.pos)
	gotLine, ctrl := rp.readEditedLine(&lb, rn2, func() { rn2.draw(lb.text, lb.pos) })
	if ctrl != readOK || gotLine != "echo first" {
		t.Fatalf("Ctrl-R repeat got %q (ctrl=%v), want %q", gotLine, ctrl, "echo first")
	}
	// After \x1b[H\x1b[2J (Ctrl-L), prompt+line must be reprinted!
	idxClear := strings.LastIndex(screenBuf.String(), "\x1b[H\x1b[2J")
	if idxClear < 0 || !strings.Contains(screenBuf.String()[idxClear:], "$ echo first") {
		t.Fatalf("Ctrl-L failed to redraw prompt+line; tail=%q", screenBuf.String())
	}

	// Wide CJK rune at column 4 of width 5 wraps to row 1 col 0; cursor at pos=4 must match.
	rows, cr, cc := layoutCells("", []rune("abcd中"), 4, 5)
	if rows != 2 || cr != 1 || cc != 0 {
		t.Fatalf("wide rune wrap layoutCells = (rows=%d, cr=%d, cc=%d), want (2, 1, 0)", rows, cr, cc)
	}
}

// TestBugHistoryControlPersistenceAndBangAndVerify verifies:
// 1. HISTCONTROL=ignorespace / ignoredups / history -d does not leak ignored/deleted lines into HISTFILE.
// 2. `history -c; history -w` truncates cleanly with no phantom empty entry on reload.
// 3. `history -n` appends only unread lines instead of duplicating existing ones.
// 4. `expandBang` handles `\!`, `${!VAR}`, `$!`, `!-`, and `# !comment`.
// 5. `shopt -s histverify` works in REPL and pauses `!` expansion for re-editing in ttyLoop.
func TestBugHistoryControlPersistenceAndBangAndVerify(t *testing.T) {
	histPath := filepath.Join(t.TempDir(), "hist")
	var outBuf bytes.Buffer
	rp := &repl{
		hist:       newHistStore(),
		kbd:        &keyReader{},
		out:        io.Discard,
		errOut:     io.Discard,
		histExpand: true,
		width:      80,
	}
	r, err := interp.New(
		interp.Interactive(true),
		interp.StdIO(nil, &outBuf, io.Discard),
		interp.CallHandler(rp.callHandler),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	rp.r = r
	mustRun(t, r, "HISTCONTROL=ignoreboth")

	rp.hist.load(histPath, 500)
	replHist = rp
	defer func() { replHist = nil }()

	rp.acceptLine("echo public")
	rp.acceptLine(" echo secret_token") // ignorespace!
	rp.acceptLine("echo public")        // ignoredups!
	rp.acceptLine("echo delete_me")
	mustRun(t, r, "history -d 2") // deleted session entry must not be saved either!
	rp.hist.save(rp.histNew)

	data, err := os.ReadFile(histPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "echo public\n" {
		t.Fatalf("HISTCONTROL / history -d violated in saved histfile: got %q, want %q", string(data), "echo public\n")
	}

	// history -n when nothing new was written externally must NOT duplicate entries,
	// and when 1 external line is appended, must read only that 1 new line.
	rp.histNew = nil
	mustRun(t, r, "history -n")
	if len(rp.hist.items) != 1 {
		t.Fatalf("history -n duplicated existing entries: %v", rp.hist.items)
	}
	f, err := os.OpenFile(histPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("echo from_other_shell\n")
	_ = f.Close()
	mustRun(t, r, "history -n")
	if len(rp.hist.items) != 2 || rp.hist.items[1] != "echo from_other_shell" {
		t.Fatalf("history -n failed to read appended line: %v", rp.hist.items)
	}

	// history -c; history -w must truncate to 0 bytes (no phantom "" item on reload).
	mustRun(t, r, "history -c")
	mustRun(t, r, "history -w")
	h2 := newHistStore()
	h2.load(histPath, 500)
	if len(h2.items) != 0 {
		t.Fatalf("reloading cleared+written histfile produced phantom items: %q", h2.items)
	}

	// HISTSIZE=0 and HISTFILESIZE=0 must disable memory/file storage rather than acting unlimited.
	hZero := newHistStore()
	hZero.maxIn = histInt("0", 500)
	if hZero.add("echo nope", "") || len(hZero.items) != 0 {
		t.Fatalf("HISTSIZE=0 allowed items: %v", hZero.items)
	}

	// expandBang edge cases: \!, ${!VAR}, $!, !-, and `# !comment`.
	items := []string{"echo old"}
	if got, _, err := expandBang(`echo \!echo`, items); err != nil || got != "echo !echo" {
		t.Fatalf(`expandBang(\!echo) = %q, %v`, got, err)
	}
	if got, _, err := expandBang(`echo ${!HOME}`, items); err != nil || got != "echo ${!HOME}" {
		t.Fatalf(`expandBang(${!HOME}) = %q, %v`, got, err)
	}
	if got, _, err := expandBang(`kill $!`, items); err != nil || got != "kill $!" {
		t.Fatalf(`expandBang($!) = %q, %v`, got, err)
	}
	if got, _, err := expandBang(`echo !-`, items); err != nil || got != "echo !-" {
		t.Fatalf(`expandBang(!-) = %q, %v`, got, err)
	}
	if got, _, err := expandBang(`echo ok # !nonexistent_event`, items); err != nil || got != "echo ok # !nonexistent_event" {
		t.Fatalf(`expandBang(# comment) = %q, %v`, got, err)
	}

	// shopt -s histverify + ttyLoop re-edit before execution + shopt -u histverify.
	mustRun(t, r, "shopt -s histverify")
	if !rp.histVerify || !shoptEnabled(r, "histverify") {
		t.Fatal("shoptEnabled(r, \"histverify\") returned false after `shopt -s histverify`")
	}
	outBuf.Reset()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	rp.inFile = pr
	go func() {
		// Seed command -> `!!` (histverify loads "echo seed" onto edit line) -> append "ed_ok" + Enter.
		_, _ = pw.Write([]byte("echo seed\r!!\red_ok\r"))
		_ = pw.Close()
	}()
	_ = rp.ttyLoop()
	if outBuf.String() != "seed\nseeded_ok\n" {
		t.Fatalf("histverify re-edit output = %q, want %q", outBuf.String(), "seed\nseeded_ok\n")
	}
	mustRun(t, r, "shopt -u histverify")
	if rp.histVerify || shoptEnabled(r, "histverify") {
		t.Fatal("histverify remained enabled after `shopt -u histverify`")
	}

	_ = errors.New
	_ = context.Background
	_ = syntax.NewParser
}

// TestBugExitTrapPreservesLastExitStatus verifies `runExitTrap()` preserves `$?`
// (such as 1 after `false` or 130 after `Ctrl-C`) inside `trap ... EXIT` on EOF/Ctrl-D.
func TestBugExitTrapPreservesLastExitStatus(t *testing.T) {
	runCase := func(name, input string, wantCode int, wantOut string) {
		t.Helper()
		var outBuf bytes.Buffer
		rp := &repl{
			hist:   newHistStore(),
			kbd:    &keyReader{},
			out:    io.Discard,
			errOut: io.Discard,
			width:  80,
		}
		r, err := interp.New(
			interp.Interactive(true),
			interp.StdIO(nil, &outBuf, io.Discard),
			interp.CallHandler(rp.callHandler),
			interp.OpenHandler(shellOpenHandler()),
			interp.ExecHandlers(trackExec, extraHandler),
			interp.ProcSubstHandler(procSubstHandler),
		)
		if err != nil {
			t.Fatal(err)
		}
		rp.r = r
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer pr.Close()
		rp.inFile = pr
		go func() {
			_, _ = pw.Write([]byte(input))
			_ = pw.Close()
		}()
		code := rp.ttyLoop()
		if code != wantCode {
			t.Fatalf("%s: ttyLoop exit code = %d, want %d (out=%q)", name, code, wantCode, outBuf.String())
		}
		if outBuf.String() != wantOut {
			t.Fatalf("%s: EXIT trap output = %q, want %q", name, outBuf.String(), wantOut)
		}
	}

	runCase("false+EOF", "trap 'echo rc=$?' EXIT\rfalse\r\x04", 1, "rc=1\n")
	runCase("ctrlC+CtrlD", "trap 'echo rc=$?' EXIT\r\x03\x04", 130, "rc=130\n")
}

type fakeFatalSignal struct{}

func (fakeFatalSignal) String() string { return "fake-fatal" }
func (fakeFatalSignal) Signal()        {}

// TestBugPromptSignalSyncAndStaleDrain verifies:
//  1. Signals buffered in promptSigCh while raw mode is suspended (`execLine`) are
//     drained before `resumeRaw()` sets `rawOn = true`, preventing stale fatal exits.
//  2. Concurrent `suspendRaw`/`resumeRaw`/`recordHist`/`cmdHistory`/`handlePromptSignal`
//     calls synchronize cleanly via `rp.mu`.
func TestBugPromptSignalSyncAndStaleDrain(t *testing.T) {
	histPath := filepath.Join(t.TempDir(), "sig_hist")
	rawMock := &mockRawCtl{active: true}
	sigCh := make(chan os.Signal, 4)
	rp := &repl{
		r:           mustRunner(t),
		hist:        newHistStore(),
		kbd:         &keyReader{},
		raw:         rawMock,
		rawOn:       true,
		promptSigCh: sigCh,
		out:         io.Discard,
		errOut:      io.Discard,
		width:       80,
	}
	rp.hist.load(histPath, 500)
	replHist = rp
	defer func() { replHist = nil }()

	// Simulate entering execLine (`suspendRaw`) and a signal arriving on promptSigCh
	// right as/while raw mode is suspended; `resumeRaw` must drain it before `rawOn = true`.
	rp.suspendRaw()
	sigCh <- fakeFatalSignal{}
	rp.resumeRaw()
	select {
	case s := <-sigCh:
		t.Fatalf("stale signal %v was not drained by resumeRaw()!", s)
	default:
	}

	// Concurrent stress across raw state transitions, history mutations, and prompt signals.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rp.suspendRaw()
			rp.resumeRaw()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rp.recordHist("echo item")
			_ = cmdHistory(context.Background(), interp.HandlerContext{Stdout: io.Discard, Stderr: io.Discard}, []string{"history", "-a"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = rp.handlePromptSignal(os.Interrupt)
		}
	}()
	wg.Wait()

	// Finally, an unhandled fatal signal while idle at prompt (`rawOn == true`) must
	// close raw mode, persist histNew, and report true.
	rp.resumeRaw()
	rp.recordHist("echo saved_on_term")
	if !rp.handlePromptSignal(fakeFatalSignal{}) {
		t.Fatal("expected handlePromptSignal on fatal signal while rawOn=true to return true")
	}
	if rawMock.active || rp.isRawOn() {
		t.Fatal("expected handlePromptSignal to close raw mode")
	}
	data, err := os.ReadFile(histPath)
	if err != nil || !strings.Contains(string(data), "echo saved_on_term\n") {
		t.Fatalf("expected handlePromptSignal to persist histNew, got %q (err=%v)", string(data), err)
	}
}

// TestBugTTYHangupDoesNotExecuteUnsubmittedLine verifies EOF / PTY hangup on an
// unsubmitted line (both primary prompt and PS2 continuation) never executes it.
func TestBugTTYHangupDoesNotExecuteUnsubmittedLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"primary_unsubmitted", "echo SHOULD_NOT_EXECUTE"},
		{"ps2_quote_unsubmitted", "echo \"hello\rSHOULD_NOT_EXECUTE\""},
		{"ps2_backslash_eof", "echo SHOULD_NOT_EXECUTE \\\r"},
	} {
		var outBuf bytes.Buffer
		rp := &repl{
			hist:   newHistStore(),
			kbd:    &keyReader{},
			out:    io.Discard,
			errOut: io.Discard,
			width:  80,
		}
		r, err := interp.New(
			interp.Interactive(true),
			interp.StdIO(nil, &outBuf, io.Discard),
			interp.CallHandler(rp.callHandler),
			interp.OpenHandler(shellOpenHandler()),
			interp.ExecHandlers(trackExec, extraHandler),
			interp.ProcSubstHandler(procSubstHandler),
		)
		if err != nil {
			t.Fatal(err)
		}
		rp.r = r
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		rp.inFile = pr
		go func() {
			_, _ = pw.Write([]byte(tc.input))
			_ = pw.Close()
		}()
		_ = rp.ttyLoop()
		_ = pr.Close()
		if strings.Contains(outBuf.String(), "SHOULD_NOT_EXECUTE") {
			t.Fatalf("%s: executed unsubmitted/incomplete command on hangup! out=%q", tc.name, outBuf.String())
		}
	}
}

// TestBugEscapedBangNoEchoAndHistverifySafe verifies `\!` suppresses history
// expansion without echoing the command to rp.out or trapping `shopt -s histverify`
// in a stripped-backslash re-edit failure, while `history -p` still outputs `!`.
func TestBugEscapedBangNoEchoAndHistverifySafe(t *testing.T) {
	var cmdOut, ttyOut, errOut bytes.Buffer
	rp := &repl{
		hist:       newHistStore(),
		kbd:        &keyReader{},
		out:        &ttyOut,
		errOut:     &errOut,
		histExpand: true,
		width:      80,
	}
	r, err := interp.New(
		interp.Interactive(true),
		interp.StdIO(nil, &cmdOut, &errOut),
		interp.CallHandler(rp.callHandler),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	rp.r = r
	mustRun(t, r, "PS1='$ '")
	mustRun(t, r, "shopt -s histverify")
	replHist = rp
	defer func() { replHist = nil }()

	// 1. Direct expandBang check: displayEcho must be "" when only `\!` was unescaped.
	exp, disp, err := expandBang(`echo \!hello`, []string{"echo prev"})
	if err != nil || exp != "echo !hello" || disp != "" {
		t.Fatalf(`expandBang(echo \!hello) = (%q, %q, %v), want ("echo !hello", "", nil)`, exp, disp, err)
	}

	// 2. In ttyLoop with `shopt -s histverify`, `echo \!hello` must execute immediately
	// without echoing "echo !hello" as an expanded line or failing with event not found.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	rp.inFile = pr
	go func() {
		_, _ = pw.Write([]byte("echo \\!hello\rhistory -p 'echo \\!world'\r"))
		_ = pw.Close()
	}()
	code := rp.ttyLoop()
	if code != 0 {
		t.Fatalf("ttyLoop exited %d (errOut=%q)", code, errOut.String())
	}
	if cmdOut.String() != "!hello\necho !world\n" {
		t.Fatalf("cmdOut = %q, want %q", cmdOut.String(), "!hello\necho !world\n")
	}
	if errOut.String() != "" {
		t.Fatalf("unexpected stderr: %q", errOut.String())
	}
	if strings.Contains(ttyOut.String(), "echo !hello") {
		t.Fatalf("escaped bang falsely echoed expanded line to ttyOut: %q", ttyOut.String())
	}
}
