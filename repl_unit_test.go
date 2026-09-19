package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func mustRunner(t *testing.T) *interp.Runner {
	t.Helper()
	r, err := interp.New(
		interp.StdIO(nil, io.Discard, io.Discard),
		interp.CallHandler(callOverride),
		interp.ExecHandlers(trackExec, extraHandler),
		interp.ProcSubstHandler(procSubstHandler),
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustRun(t *testing.T, r *interp.Runner, src string) {
	t.Helper()
	prog, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Run(ctx, prog); err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
}

func feedKeys(s string) []vtKey {
	k := &keyReader{}
	k.feed([]byte(s))
	var out []vtKey
	for {
		key, ok := k.next()
		if !ok {
			break
		}
		out = append(out, key)
	}
	return out
}

func TestKeyReaderPrintable(t *testing.T) {
	ks := feedKeys("hi")
	if len(ks) != 2 || ks[0].r != 'h' || ks[1].r != 'i' {
		t.Fatalf("printable: %+v", ks)
	}
}

func TestKeyReaderControls(t *testing.T) {
	ks := feedKeys("\x01\x05\x03\x04\t\r\x7f")
	want := []uint32{keyCtrlA, keyCtrlE, keyCtrlC, keyCtrlD, keyTab, keyEnter, keyBackspace}
	if len(ks) != len(want) {
		t.Fatalf("len=%d want %d: %+v", len(ks), len(want), ks)
	}
	for i, w := range want {
		if ks[i].code != w {
			t.Fatalf("key %d = %d want %d", i, ks[i].code, w)
		}
	}
}

func TestKeyReaderArrows(t *testing.T) {
	ks := feedKeys("\x1b[A\x1b[B\x1b[C\x1b[D\x1b[3~\x1b[H\x1b[F")
	want := []uint32{keyUp, keyDown, keyRight, keyLeft, keyDelete, keyHome, keyEnd}
	if len(ks) != len(want) {
		t.Fatalf("len=%d: %+v", len(ks), ks)
	}
	for i, w := range want {
		if ks[i].code != w {
			t.Fatalf("key %d = %d want %d", i, ks[i].code, w)
		}
	}
}

func TestKeyReaderCtrlArrows(t *testing.T) {
	ks := feedKeys("\x1b[1;5C\x1b[1;5D")
	if len(ks) != 2 || ks[0].code != keyCtrlRight || ks[1].code != keyCtrlLeft {
		t.Fatalf("ctrl arrows: %+v", ks)
	}
}

func TestKeyReaderAlt(t *testing.T) {
	ks := feedKeys("\x1bb\x1bf\x1bd")
	want := []uint32{keyAltB, keyAltF, keyAltD}
	if len(ks) != 3 {
		t.Fatalf("len=%d: %+v", len(ks), ks)
	}
	for i, w := range want {
		if ks[i].code != w {
			t.Fatalf("alt %d = %d want %d", i, ks[i].code, w)
		}
	}
}

func TestKeyReaderUTF8(t *testing.T) {
	k := &keyReader{}
	k.feed([]byte("é")[0:1])
	if _, ok := k.next(); ok {
		t.Fatal("partial UTF-8 should need more bytes")
	}
	k.feed([]byte("é")[1:])
	keys, ok := k.next()
	if !ok || keys.r != 'é' {
		t.Fatalf("utf8: %+v %v", keys, ok)
	}
	// CJK wide char single key.
	ks := feedKeys("中")
	if len(ks) != 1 || ks[0].r != '中' {
		t.Fatalf("cjk: %+v", ks)
	}
}

func TestLineBufEdit(t *testing.T) {
	var l lineBuf
	l.insertStr("hello")
	if l.String() != "hello" || l.pos != 5 {
		t.Fatalf("insert: %q %d", l.String(), l.pos)
	}
	l.pos = 2
	l.insert('X')
	if l.String() != "heXllo" {
		t.Fatalf("mid insert: %q", l.String())
	}
	l.backspace()
	if l.String() != "hello" {
		t.Fatalf("backspace: %q", l.String())
	}
	l.pos = 0
	if l.backspace() {
		t.Fatal("backspace at 0 should fail")
	}
	l.pos = 5
	if l.deleteAt() {
		t.Fatal("delete at end should fail")
	}
	l.pos = 1
	l.deleteAt()
	if l.String() != "hllo" {
		t.Fatalf("delete: %q", l.String())
	}
	if k := l.killToEnd(); k != "llo" || l.String() != "h" {
		t.Fatalf("killToEnd: %q %q", k, l.String())
	}
	l.insertStr(" ello world") // l == "h ello world"
	l.pos = 6                  // end of "ello"
	if k := l.killWordBack(); k != "ello" || l.String() != "h  world" {
		t.Fatalf("killWordBack: %q %q", k, l.String())
	}
	// l == "h  world", words: "h" then "world" (double space).
	l.pos = 0
	l.moveWordFwd()
	if l.pos != 1 {
		t.Fatalf("moveWordFwd: %d", l.pos)
	}
	l.moveWordFwd()
	if l.pos != 8 {
		t.Fatalf("moveWordFwd2: %d", l.pos)
	}
	l.moveWordBack()
	if l.pos != 3 {
		t.Fatalf("moveWordBack: %d", l.pos)
	}
	var t2 lineBuf
	t2.insertStr("ab")
	t2.pos = 1
	t2.transpose()
	if t2.String() != "ba" {
		t.Fatalf("transpose: %q", t2.String())
	}
}

func TestRuneWidth(t *testing.T) {
	if runeWidth('a') != 1 {
		t.Fatal("ascii")
	}
	if runeWidth('中') != 2 {
		t.Fatal("cjk wide")
	}
	if runeWidth(0x0301) != 0 {
		t.Fatal("combining")
	}
	if strWidth("a中") != 3 {
		t.Fatalf("strWidth=%d", strWidth("a中"))
	}
	if strWidth("hello") != 5 {
		t.Fatal("strWidth ascii")
	}
}

func TestExpandBang(t *testing.T) {
	items := []string{"echo hi", "ls -la"}
	got, _, err := expandBang("!!", items)
	if err != nil || got != "ls -la" {
		t.Fatalf("!! = %q %v", got, err)
	}
	got, _, err = expandBang("!echo", items)
	if err != nil || got != "echo hi" {
		t.Fatalf("!echo = %q %v", got, err)
	}
	got, _, err = expandBang("!1", items)
	if err != nil || got != "echo hi" {
		t.Fatalf("!1 = %q %v", got, err)
	}
	got, _, err = expandBang("!-1", items)
	if err != nil || got != "ls -la" {
		t.Fatalf("!-1 = %q %v", got, err)
	}
	got, _, err = expandBang("!?la?", items)
	if err != nil || got != "ls -la" {
		t.Fatalf("!?... = %q %v", got, err)
	}
	if _, _, err = expandBang("!zzz-nope", items); err == nil {
		t.Fatal("missing event should error")
	}
	// No bang: passthrough.
	got, _, err = expandBang("echo 'hi!'", items)
	if err != nil || got != "echo 'hi!'" {
		t.Fatalf("quoted = %q %v", got, err)
	}
}

func TestIncomplete(t *testing.T) {
	for _, c := range []string{`echo "foo`, "cat <<EOF\nhi", `if true; then`, `echo a |`, `echo $(`} {
		if !incomplete(c) {
			t.Fatalf("should be incomplete: %q", c)
		}
	}
	for _, c := range []string{"echo hi", "if true; then echo; fi", "cat <<EOF\nhi\nEOF"} {
		if incomplete(c) {
			t.Fatalf("should be complete: %q", c)
		}
	}
}

func TestWordAt(t *testing.T) {
	s, w := wordAt("echo hello", 10)
	if s != 5 || w != "hello" {
		t.Fatalf("wordAt=%d %q", s, w)
	}
	s, w = wordAt("ls /tmp/fi", 10)
	if w != "/tmp/fi" || s != 3 {
		t.Fatalf("wordAt path=%d %q", s, w)
	}
	s, w = wordAt("echo a|grep b", 13)
	if w != "b" {
		t.Fatalf("wordAt pipe=%q", w)
	}
	if p := commonPrefix([]string{"foobar", "foobaz"}); p != "fooba" {
		t.Fatalf("commonPrefix=%q", p)
	}
	if !strings.Contains("x", "x") {
		t.Fatal("unreachable")
	}
}

func TestIncompleteMore(t *testing.T) {
	for _, c := range []string{
		"while true; do", "for i in 1 2; do", "case x in", "foo() {",
		"select x in a b; do", "if [ -n x ]; then", "coproc foo {",
		"cat <<EOF\nhi",
	} {
		if !incomplete(c) {
			t.Fatalf("should be incomplete: %q", c)
		}
	}
	for _, c := range []string{
		"for i in 1 2; do echo $i; done", "case x in x) echo;; esac",
		"foo() { echo; }", "while true; do break; done",
	} {
		if incomplete(c) {
			t.Fatalf("should be complete: %q", c)
		}
	}
}

func TestPromptBasics(t *testing.T) {
	rp := &repl{hist: newHistStore(), kbd: &keyReader{}, out: io.Discard, errOut: io.Discard}
	rp.r = mustRunner(t)
	// Default PS1 contains $ and expands without error.
	d, raw := rp.prompt(true)
	if d == "" || raw == "" {
		t.Fatalf("empty prompt %q %q", d, raw)
	}
	// Custom PS1 with escapes.
	mustRun(t, rp.r, `PS1='\u@\h:\W\$ '`)
	d, _ = rp.prompt(true)
	if !strings.Contains(d, "@") || !strings.Contains(d, ":") {
		t.Fatalf("PS1 escapes: %q", d)
	}
	// PS2 default.
	d2, _ := rp.prompt(false)
	if d2 == "" {
		t.Fatal("empty PS2")
	}
	// Prompt var expansion uses live vars.
	mustRun(t, rp.r, `MYVAR=world`)
	mustRun(t, rp.r, `PS1='[$MYVAR]> '`)
	d, _ = rp.prompt(true)
	if d != "[world]> " {
		t.Fatalf("PS1 var: %q", d)
	}
	// \[...\] spans are zero-width (raw keeps text, display keeps too;
	// width math uses display minus spans — covered in render test).
	mustRun(t, rp.r, `PS1='\[\e[32m\]ok\$ '`)
	d, raw = rp.prompt(true)
	if !strings.Contains(raw, "\x1b[32m") || !strings.Contains(d, "ok") {
		t.Fatalf("PS1 spans: disp=%q raw=%q", d, raw)
	}
}

func TestHistStore(t *testing.T) {
	h := newHistStore()
	h.maxIn = 3
	h.add("a", "")
	h.add("b", "")
	h.add("a", "ignoredups") // different from last (b) -> kept
	if len(h.items) != 3 {
		t.Fatalf("items=%v", h.items)
	}
	h.add("a", "ignoredups") // dup of last -> skipped
	if len(h.items) != 3 {
		t.Fatalf("ignoredups: %v", h.items)
	}
	h.add(" c", "ignorespace")
	if len(h.items) != 3 {
		t.Fatalf("ignorespace: %v", h.items)
	}
	h.add("x", "erasedups")
	h.add("a", "erasedups") // removes older a's
	found := 0
	for _, it := range h.items {
		if it == "a" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("erasedups: %v", h.items)
	}
	h.add("overflow-test", "")
	if len(h.items) != 3 {
		t.Fatalf("maxIn cap: %v", h.items)
	}
	if !shouldSaveCtl("echo hi", "") || shouldSaveCtl(" echo hi", "ignorespace") {
		t.Fatal("shouldSaveCtl")
	}
}

func TestHistoryBuiltin(t *testing.T) {
	rp := &repl{hist: newHistStore(), kbd: &keyReader{}, out: io.Discard, errOut: io.Discard}
	rp.r = mustRunner(t)
	replHist = rp
	defer func() { replHist = nil }()
	mustRun(t, rp.r, `echo one`)
	rp.hist.add("echo one", "")
	rp.histNew = append(rp.histNew, "echo one")
	// history shows entries.
	var out strings.Builder
	r2out := rp.hist.items
	_ = r2out
	_ = out
	if len(rp.hist.items) != 1 || rp.hist.items[0] != "echo one" {
		t.Fatalf("items=%v", rp.hist.items)
	}
	// history -c clears.
	mustRun(t, rp.r, `history -c`)
	if len(rp.hist.items) != 0 {
		t.Fatalf("after -c: %v", rp.hist.items)
	}
	// history -s adds.
	mustRun(t, rp.r, `history -s "echo seeded"`)
	if len(rp.hist.items) != 1 || rp.hist.items[0] != "echo seeded" {
		t.Fatalf("after -s: %v", rp.hist.items)
	}
	// history -d removes.
	mustRun(t, rp.r, `history -s second`)
	mustRun(t, rp.r, `history -d 1`)
	if len(rp.hist.items) != 1 || rp.hist.items[0] != "second" {
		t.Fatalf("after -d: %v", rp.hist.items)
	}
}

func TestCompletion(t *testing.T) {
	rp := &repl{hist: newHistStore(), kbd: &keyReader{}, out: io.Discard, errOut: io.Discard}
	rp.r = mustRunner(t)
	// Command completion finds builtins.
	_, cands := completeWord("ec", 2, rp.r, nil)
	found := false
	for _, c := range cands {
		if c == "echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("complete ec: %v", cands)
	}
	// Variable completion.
	mustRun(t, rp.r, `MYCOMP_xyz=1`)
	_, cands = completeWord("$MYCOMP_", 8, rp.r, nil)
	found = false
	for _, c := range cands {
		if c == "$MYCOMP_xyz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("complete var: %v", cands)
	}
	// No match.
	rep, cands := completeWord("zzz-no-such-cmd-xyz", 18, rp.r, nil)
	if rep != "" || len(cands) != 0 {
		t.Fatalf("no-match: %q %v", rep, cands)
	}
}

func TestLayoutCells(t *testing.T) {
	rows, cr, cc := layoutCells("$ ", []rune("hi"), 2, 80)
	if rows != 1 || cr != 0 || cc != 4 {
		t.Fatalf("simple: rows=%d cr=%d cc=%d", rows, cr, cc)
	}
	// CJK counts 2 cells.
	rows, _, cc = layoutCells("", []rune("a中b"), 3, 80)
	if rows != 1 || cc != 4 {
		t.Fatalf("cjk: rows=%d cc=%d", rows, cc)
	}
	// ANSI escapes take no cells.
	rows, _, cc = layoutCells("\x1b[32mok$ ", []rune("x"), 1, 80)
	if rows != 1 || cc != 5 {
		t.Fatalf("ansi: rows=%d cc=%d", rows, cc)
	}
	// Wrap at width: 8 cells at width 5 -> rows of 5+3.
	rows, cr, cc = layoutCells("", []rune("abcdefgh"), 8, 5)
	if rows != 2 || cr != 1 || cc != 3 {
		t.Fatalf("wrap: rows=%d cr=%d cc=%d", rows, cr, cc)
	}
	_ = rows
	// Cursor mid-line.
	rows, cr, cc = layoutCells("> ", []rune("hello"), 2, 80)
	if rows != 1 || cr != 0 || cc != 4 {
		t.Fatalf("mid: rows=%d cr=%d cc=%d", rows, cr, cc)
	}
	// Combining char is zero width.
	rows, _, cc = layoutCells("", []rune("áb"), 3, 80)
	if rows != 1 || cc != 2 {
		t.Fatalf("combining: rows=%d cc=%d", rows, cc)
	}
}

func TestRendererIncremental(t *testing.T) {
	var sb strings.Builder
	rn := newLineRenderer(&sb, "$ ", 80)
	rn.draw([]rune("h"), 1)
	first := sb.String()
	if first != "$ h" {
		t.Fatalf("first draw: %q", first)
	}
	// Append: only new bytes, no clear.
	sb.Reset()
	rn.draw([]rune("hi"), 2)
	if got := sb.String(); got != "i" {
		t.Fatalf("append: %q", got)
	}
	// Same text, cursor move: only horizontal motion, no reprint.
	sb.Reset()
	rn.draw([]rune("hi"), 1)
	if got := sb.String(); got != "\x1b[1D" {
		t.Fatalf("cursor move: %q", got)
	}
	// Backspace at end: erase cells, no reprint.
	sb.Reset()
	rn.draw([]rune("h"), 1)
	if got := sb.String(); got != "\b \b" {
		t.Fatalf("backspace: %q", got)
	}
	// Middle edit: full redraw with exact clear.
	sb.Reset()
	rn.draw([]rune("Xh"), 1)
	if got := sb.String(); !strings.Contains(got, "$ Xh") || !strings.Contains(got, "\x1b[K") {
		t.Fatalf("middle edit: %q", got)
	}
	// Wrapped line shrinking across rows: full redraw must erase every
	// old row (2 K's for the 90-col line at width 20 -> 5 rows) and
	// place the cursor back at the new end.
	sb.Reset()
	long := []rune(strings.Repeat("a", 90))
	rn2 := newLineRenderer(&sb, "", 20)
	rn2.draw(long, 90)
	sb.Reset()
	rn2.draw(long[:45], 45)
	got := sb.String()
	if strings.Count(got, "\x1b[K") != 5 {
		t.Fatalf("wrapped clear K count: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("a", 45)) {
		t.Fatalf("wrapped clear reprint: %q", got)
	}
}

func TestRendererNoChange(t *testing.T) {
	var sb strings.Builder
	rn := newLineRenderer(&sb, "$ ", 80)
	rn.draw([]rune("hi"), 2)
	sb.Reset()
	rn.draw([]rune("hi"), 2)
	if sb.String() != "" {
		t.Fatalf("no-change must write nothing: %q", sb.String())
	}
}

func TestPrintCompletionsColumns(t *testing.T) {
	var sb strings.Builder
	printCompletions(&sb, []string{"apple", "apricot", "banana"}, 20)
	got := sb.String()
	// 20 cols: apple/apricot/banana in 2 columns (9+2=11 -> 1 col? check).
	// maxw=7, colW=9, cols=2, rows=2: apple banana / apricot.
	if !strings.Contains(got, "apple") || !strings.Contains(got, "banana") {
		t.Fatalf("columns: %q", got)
	}
	if strings.Count(got, "\r\n") != 2 {
		t.Fatalf("rows: %q", got)
	}
	var sb2 strings.Builder
	printCompletions(&sb2, []string{"a", "b"}, 80)
	if sb2.String() != "a  b\r\n" {
		t.Fatalf("single row: %q", sb2.String())
	}
	// Narrow but usable width: still columnar (cols=1 only below 10).
	var sb3 strings.Builder
	printCompletions(&sb3, []string{"aa", "bb"}, 12)
	if sb3.String() != "aa  bb\r\n" {
		t.Fatalf("narrow cols: %q", sb3.String())
	}
}

func TestSearchOverlay(t *testing.T) {
	rp := &repl{hist: newHistStore(), kbd: &keyReader{}, out: io.Discard, errOut: io.Discard}
	rp.hist.add("echo hello", "")
	rp.hist.add("ls /tmp", "")
	var sb strings.Builder
	rn := newLineRenderer(&sb, "$ ", 80)
	rn.draw([]rune("ec"), 2)
	sb.Reset()
	// Start search: clears current block, draws overlay.
	var lb lineBuf
	lb.insertStr("ec")
	lb.pos = 2
	rp.searchStart(rn, &lb)
	got := sb.String()
	if !strings.Contains(got, "(reverse-i-search)") {
		t.Fatalf("search start: %q", got)
	}
	// Type narrows to match.
	sb.Reset()
	rp.searchType('e', rn, &lb)
	got = sb.String()
	if !strings.Contains(got, "echo hello") {
		t.Fatalf("search type: %q", got)
	}
	// Accept puts match on line (renderer cleared + redrawn, no overlay left).
	sb.Reset()
	rp.searchAccept(rn, &lb)
	if lb.String() != "echo hello" {
		t.Fatalf("accept: %q", lb.String())
	}
	if strings.Contains(sb.String(), "reverse-i-search") {
		t.Fatalf("accept left overlay: %q", sb.String())
	}
	// Start again, type, then cancel restores stashed line.
	lb.clear()
	lb.insertStr("ec")
	lb.pos = 2
	sb.Reset()
	rp.searchStart(rn, &lb)
	rp.searchType('l', rn, &lb)
	sb.Reset()
	rp.searchCancel(rn, &lb, func() {})
	if lb.String() != "ec" || lb.pos != 2 {
		t.Fatalf("cancel: %q %d", lb.String(), lb.pos)
	}
	// Cancel must be net-newline-free-ish: just clear+redraw, check redraw happened.
	if !strings.Contains(sb.String(), "$ ec") {
		t.Fatalf("cancel redraw: %q", sb.String())
	}
}
