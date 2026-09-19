package main

// Interactive readline core: raw-mode key reader, UTF-8 aware line
// buffer, emacs keymap, and minimal ANSI redraw. No external deps;
// works on Windows (console mode flags) and unix (termios via
// golang.org/x/term, already an indirect dep).

import (
	"os"
	"strings"
	"unicode/utf8"
)

// vtKey is a decoded input key.
type vtKey struct {
	r     rune   // printable rune (0 for special)
	code  uint32 // special code (see keyXXX)
	bytes []byte // raw bytes (for bracketed paste etc.)
}

const (
	keyNone uint32 = iota
	keyEnter
	keyCtrlC
	keyCtrlD
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyUp
	keyDown
	keyHome
	keyEnd
	keyTab
	keyCtrlLeft
	keyCtrlRight
	keyCtrlR
	keyCtrlS
	keyCtrlA
	keyCtrlE
	keyCtrlB
	keyCtrlF
	keyCtrlK
	keyCtrlU
	keyCtrlW
	keyAltB
	keyAltF
	keyAltD
	keyAltBackspace
	keyCtrlT
	keyCtrlL
	keyCtrlP
	keyCtrlN
	keyEscape
	keyUnknown
)

// keyReader decodes UTF-8 + ANSI escape sequences from a byte stream.
type keyReader struct {
	buf []byte
}

func (k *keyReader) feed(p []byte) { k.buf = append(k.buf, p...) }

// next returns the next key if a full sequence is buffered.
func (k *keyReader) next() (vtKey, bool) {
	if len(k.buf) == 0 {
		return vtKey{}, false
	}
	b0 := k.buf[0]
	// C0 controls.
	switch b0 {
	case '\r', '\n':
		k.buf = k.buf[1:]
		return vtKey{code: keyEnter}, true
	case 0x01:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlA}, true
	case 0x02:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlB}, true
	case 0x03:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlC}, true
	case 0x04:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlD}, true
	case 0x05:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlE}, true
	case 0x06:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlF}, true
	case 0x07:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlL}, true // Ctrl-G: redraw like clear-screen-lite
	case 0x09:
		k.buf = k.buf[1:]
		return vtKey{code: keyTab}, true
	case 0x0b:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlK}, true
	case 0x0c:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlL}, true
	case 0x0e:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlN}, true
	case 0x10:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlP}, true
	case 0x12:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlR}, true
	case 0x13:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlS}, true
	case 0x14:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlT}, true
	case 0x15:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlU}, true
	case 0x17:
		k.buf = k.buf[1:]
		return vtKey{code: keyCtrlW}, true
	case 0x7f:
		k.buf = k.buf[1:]
		return vtKey{code: keyBackspace}, true
	case 0x1b:
		return k.nextEsc()
	}
	if b0 < 0x80 {
		k.buf = k.buf[1:]
		return vtKey{r: rune(b0)}, true
	}
	if !utf8.FullRune(k.buf) {
		return vtKey{}, false // need more bytes
	}
	r, size := utf8.DecodeRune(k.buf)
	k.buf = k.buf[size:]
	if r == utf8.RuneError {
		return vtKey{code: keyUnknown}, true
	}
	return vtKey{r: r}, true
}

func (k *keyReader) nextEsc() (vtKey, bool) {
	if len(k.buf) < 2 {
		return vtKey{}, false
	}
	// Alt+key: ESC + single byte/rune (but NOT CSI/SS3).
	if k.buf[1] != '[' && k.buf[1] != 'O' {
		rest := k.buf[1:]
		if rest[0] < 0x80 {
			k.buf = k.buf[2:]
			switch rest[0] {
			case 'b', 'B':
				return vtKey{code: keyAltB}, true
			case 'f', 'F':
				return vtKey{code: keyAltF}, true
			case 'd', 'D':
				return vtKey{code: keyAltD}, true
			case 0x7f:
				k.buf = k.buf[1:] // already consumed ESC; fix below
				// consumed ESC + DEL
				return vtKey{code: keyAltBackspace}, true
			}
			return vtKey{r: rune(rest[0]), code: keyEscape}, true
		}
		if !utf8.FullRune(rest) {
			return vtKey{}, false
		}
		r, size := utf8.DecodeRune(rest)
		k.buf = k.buf[1+size:]
		return vtKey{r: r, code: keyEscape}, true
	}
	// CSI: ESC [ params final
	if k.buf[1] == '[' {
		i := 2
		for i < len(k.buf) && (k.buf[i] >= '0' && k.buf[i] <= '9' || k.buf[i] == ';') {
			i++
		}
		if i >= len(k.buf) {
			return vtKey{}, false
		}
		params := string(k.buf[2:i])
		final := k.buf[i]
		raw := append([]byte(nil), k.buf[:i+1]...)
		k.buf = k.buf[i+1:]
		isCtrl := strings.Contains(params, ";5")
		switch final {
		case 'A':
			return vtKey{code: keyUp, bytes: raw}, true
		case 'B':
			return vtKey{code: keyDown, bytes: raw}, true
		case 'C':
			if isCtrl {
				return vtKey{code: keyCtrlRight, bytes: raw}, true
			}
			return vtKey{code: keyRight, bytes: raw}, true
		case 'D':
			if isCtrl {
				return vtKey{code: keyCtrlLeft, bytes: raw}, true
			}
			return vtKey{code: keyLeft, bytes: raw}, true
		case 'H':
			return vtKey{code: keyHome, bytes: raw}, true
		case 'F':
			return vtKey{code: keyEnd, bytes: raw}, true
		case '~':
			switch params {
			case "1", "7":
				return vtKey{code: keyHome, bytes: raw}, true
			case "3":
				return vtKey{code: keyDelete, bytes: raw}, true
			case "4", "8":
				return vtKey{code: keyEnd, bytes: raw}, true
			}
		}
		return vtKey{code: keyUnknown, bytes: raw}, true
	}
	// SS3: ESC O X
	if len(k.buf) < 3 {
		return vtKey{}, false
	}
	final := k.buf[2]
	raw := append([]byte(nil), k.buf[:3]...)
	k.buf = k.buf[3:]
	switch final {
	case 'A':
		return vtKey{code: keyUp, bytes: raw}, true
	case 'B':
		return vtKey{code: keyDown, bytes: raw}, true
	case 'C':
		return vtKey{code: keyRight, bytes: raw}, true
	case 'D':
		return vtKey{code: keyLeft, bytes: raw}, true
	case 'H':
		return vtKey{code: keyHome, bytes: raw}, true
	case 'F':
		return vtKey{code: keyEnd, bytes: raw}, true
	case 'P':
		return vtKey{code: keyUnknown, bytes: raw}, true // F1, ignore
	}
	return vtKey{code: keyUnknown, bytes: raw}, true
}

// lineBuf is a rune-indexed editable buffer with point.
type lineBuf struct {
	text []rune
	pos  int // 0..len(text)
}

func (l *lineBuf) insert(r rune) {
	l.text = append(l.text[:l.pos], append([]rune{r}, l.text[l.pos:]...)...)
	l.pos++
}

func (l *lineBuf) insertStr(s string) {
	for _, r := range s {
		l.insert(r)
	}
}

func (l *lineBuf) backspace() bool {
	if l.pos == 0 {
		return false
	}
	l.text = append(l.text[:l.pos-1], l.text[l.pos:]...)
	l.pos--
	return true
}

func (l *lineBuf) deleteAt() bool {
	if l.pos >= len(l.text) {
		return false
	}
	l.text = append(l.text[:l.pos], l.text[l.pos+1:]...)
	return true
}

func (l *lineBuf) killToEnd() string {
	if l.pos >= len(l.text) {
		return ""
	}
	killed := string(l.text[l.pos:])
	l.text = l.text[:l.pos]
	return killed
}

func (l *lineBuf) killToStart() string {
	if l.pos == 0 {
		return ""
	}
	killed := string(l.text[:l.pos])
	l.text = append([]rune(nil), l.text[l.pos:]...)
	l.pos = 0
	return killed
}

func (l *lineBuf) killWordBack() string {
	if l.pos == 0 {
		return ""
	}
	end := l.pos
	i := end
	for i > 0 && isSpaceRune(l.text[i-1]) {
		i--
	}
	for i > 0 && !isSpaceRune(l.text[i-1]) {
		i--
	}
	killed := string(l.text[i:end])
	l.text = append(l.text[:i], l.text[end:]...)
	l.pos = i
	return killed
}

func (l *lineBuf) killWordFwd() string {
	if l.pos >= len(l.text) {
		return ""
	}
	start := l.pos
	i := start
	for i < len(l.text) && isSpaceRune(l.text[i]) {
		i++
	}
	for i < len(l.text) && !isSpaceRune(l.text[i]) {
		i++
	}
	killed := string(l.text[start:i])
	l.text = append(l.text[:start], l.text[i:]...)
	return killed
}

func (l *lineBuf) moveWordBack() {
	i := l.pos
	for i > 0 && isSpaceRune(l.text[i-1]) {
		i--
	}
	for i > 0 && !isSpaceRune(l.text[i-1]) {
		i--
	}
	l.pos = i
}

func (l *lineBuf) moveWordFwd() {
	i := l.pos
	for i < len(l.text) && isSpaceRune(l.text[i]) {
		i++
	}
	for i < len(l.text) && !isSpaceRune(l.text[i]) {
		i++
	}
	l.pos = i
}

func (l *lineBuf) transpose() {
	if len(l.text) < 2 || l.pos == 0 {
		return
	}
	i := l.pos
	if i >= len(l.text) {
		i = len(l.text) - 1
	}
	l.text[i-1], l.text[i] = l.text[i], l.text[i-1]
	if l.pos < len(l.text) {
		l.pos++
	}
}

func (l *lineBuf) clear() {
	l.text = nil
	l.pos = 0
}

func (l *lineBuf) String() string { return string(l.text) }

func isSpaceRune(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

// runeWidth returns the terminal cell width of r (0, 1 or 2).
// Combines shell.js wcwidth() + wcwidth_rd() semantics.
func runeWidth(r rune) int {
	if r == 0 {
		return 0
	}
	if r < 32 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	// Combining/zero-width BEFORE the <0x1100 fast path (diacriticals
	// U+0300..U+036F are < 0x1100 but width 0).
	if combiningRune(r) {
		return 0
	}
	if r < 0x1100 {
		return 1
	}
	// Wide ranges (W) from shell.js wcwidth table.
	switch {
	case r >= 0x1100 && r <= 0x115f:
		return 2
	case r == 0x2329 || r == 0x232a:
		return 2
	case r >= 0x2e80 && r <= 0x303e:
		return 2
	case r >= 0x3041 && r <= 0x33ff:
		return 2
	case r >= 0x3400 && r <= 0x4dbf:
		return 2
	case r >= 0x4e00 && r <= 0xa4cf:
		return 2
	case r >= 0xa960 && r <= 0xa97f:
		return 2
	case r >= 0xac00 && r <= 0xd7a3:
		return 2
	case r >= 0xf900 && r <= 0xfaff:
		return 2
	case r >= 0xfe10 && r <= 0xfe19:
		return 2
	case r >= 0xfe30 && r <= 0xfe6f:
		return 2
	case r >= 0xff00 && r <= 0xff60:
		return 2
	case r >= 0xffe0 && r <= 0xffe6:
		return 2
	case r >= 0x20000 && r <= 0x2fffd:
		return 2
	case r >= 0x30000 && r <= 0x3fffd:
		return 2
	}
	// Combining/zero-width (from shell.js combining table, sampled).
	if combiningRune(r) {
		return 0
	}
	return 1
}

func combiningRune(r rune) bool {
	// Zero-width characters: combining diacriticals, joiners, and
	// format chars (covers U+0301 and friends width-0 like bash).
	if r == 0x200b || r == 0x200c || r == 0x200d || r == 0xfeff {
		return true
	}
	switch {
	case r >= 0x0300 && r <= 0x036f:
		return true // includes U+0301 COMBINING ACUTE
	case r >= 0x0483 && r <= 0x0489:
		return true
	case r >= 0x0591 && r <= 0x05bd:
		return true
	case r == 0x05bf || r == 0x05c1 || r == 0x05c2 || r == 0x05c4 || r == 0x05c5 || r == 0x05c7:
		return true
	case r >= 0x0610 && r <= 0x061a:
		return true
	case r >= 0x0646 && r <= 0x0652:
		return true
	case r == 0x0670:
		return true
	case r >= 0x06df && r <= 0x06e4:
		return true
	case r >= 0x06e7 && r <= 0x06e8:
		return true
	case r >= 0x06ea && r <= 0x06ed:
		return true
	case r == 0x0711:
		return true
	case r >= 0x0730 && r <= 0x074a:
		return true
	case r >= 0x07a6 && r <= 0x07b0:
		return true
	case r >= 0x0901 && r <= 0x0902:
		return true
	case r == 0x093c || (r >= 0x0941 && r <= 0x0948) || r == 0x094d:
		return true
	case r >= 0x0951 && r <= 0x0957:
		return true
	case r >= 0x0962 && r <= 0x0963:
		return true
	case r == 0x0981 || (r >= 0x09bc && r <= 0x09cd):
		return true
	case r >= 0x0e31 && r <= 0x0e3a:
		return true
	case r >= 0x0e47 && r <= 0x0e4e:
		return true
	case r >= 0x0f18 && r <= 0x0f19:
		return true
	case r == 0x0f35 || r == 0x0f37 || r == 0x0f39:
		return true
	case r >= 0x0f71 && r <= 0x0f7e:
		return true
	case r >= 0x0f80 && r <= 0x0f84:
		return true
	case r >= 0x0f86 && r <= 0x0f87:
		return true
	case r >= 0x0f90 && r <= 0x0f97:
		return true
	case r >= 0x0f99 && r <= 0x0fbc:
		return true
	case r == 0x0fc6:
		return true
	case r == 0x102d || (r >= 0x1030 && r <= 0x1032) || (r >= 0x1036 && r <= 0x1039):
		return true
	case r >= 0x1a75 && r <= 0x1a7c:
		return true
	case r >= 0x1a97 && r <= 0x1a98:
		return true
	case r >= 0x1b00 && r <= 0x1b34:
		return true
	case r >= 0x1b36 && r <= 0x1b3a:
		return true
	case r == 0x1b3c || r == 0x1b42:
		return true
	case r >= 0x1b6b && r <= 0x1b73:
		return true
	case r >= 0x1b80 && r <= 0x1b81:
		return true
	case r >= 0x1dc0 && r <= 0x1de6:
		return true
	case r == 0x1dfd || r == 0x1dfe:
		return true
	case r >= 0x20d0 && r <= 0x20f0:
		return true
	case r >= 0xfe00 && r <= 0xfe0f:
		return true
	case r >= 0xfe20 && r <= 0xfe2f:
		return true
	case r >= 0xe0100 && r <= 0xe01ef:
		return true
	}
	return false
}

// strWidth sums runeWidth over s.
func strWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

var _ = os.Stdin
var _ = strings.Contains
