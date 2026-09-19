package main

// Incremental line renderer: flicker-free redraw for the interactive
// shell. Full reprints on every keystroke flash (especially with
// colors) and clearing only the first row leaves garbage when the
// line wraps. Instead:
//
//   - typing at end of line: bytes are written directly, no redraw;
//   - backspace at end: \b + space + \b, no redraw;
//   - same-row cursor moves: bare ESC[C / ESC[D, no redraw;
//   - anything else (middle edits, history swap, completion):
//     clear exactly the rows the old block occupies, reprint once.
//
// All geometry is computed in terminal CELLS (CJK = 2, combining = 0,
// ANSI escapes = 0) so wrapped lines position the cursor correctly.

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// bell rings the terminal bell (bash feedback for failed edit).
func bell(out io.Writer) { fmt.Fprint(out, "\a") }

// layoutCells computes the block geometry for prompt+line at width:
// rows = rows occupied, (crow, ccol) = cursor cell for rune pos.
func layoutCells(prompt string, line []rune, pos, width int) (rows, crow, ccol int) {
	if width < 2 {
		width = 80
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	rows = 1
	col := 0
	place := func(cells int) {
		if cells <= 0 {
			return
		}
		if col+cells > width {
			rows++
			col = 0
		}
		col += cells
		if col >= width {
			rows++
			col = 0
		}
	}
	newline := func() {
		rows++
		col = 0
	}
	i := 0
	for i < len(prompt) {
		c := prompt[i]
		if c == '\n' {
			newline()
			i++
			continue
		}
		if c == 0x1b && i+1 < len(prompt) {
			switch prompt[i+1] {
			case '[':
				j := i + 2
				for j < len(prompt) && !(prompt[j] >= 0x40 && prompt[j] <= 0x7e) {
					j++
				}
				if j < len(prompt) {
					j++
				}
				i = j
				continue
			case ']':
				j := i + 2
				for j < len(prompt) {
					if prompt[j] == 0x07 {
						j++
						break
					}
					if prompt[j] == 0x1b && j+1 < len(prompt) && prompt[j+1] == '\\' {
						j += 2
						break
					}
					j++
				}
				i = j
				continue
			default:
				i += 2 // ESC + single char (charset select, etc.)
				continue
			}
		}
		if c < 0x80 {
			place(runeWidth(rune(c)))
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(prompt[i:])
		if r == utf8.RuneError && size <= 1 {
			i++
			continue
		}
		place(runeWidth(r))
		i += size
	}
	for idx, r := range line {
		if idx == pos {
			crow, ccol = rows-1, col
		}
		if r == '\n' {
			newline()
			continue
		}
		place(runeWidth(r))
	}
	if pos == len(line) {
		crow, ccol = rows-1, col
	}
	return rows, crow, ccol
}

// clearRows erases an n-row block starting at the current row and
// returns the cursor to the block start.
func clearRows(out io.Writer, n int) {
	if n <= 0 {
		return
	}
	fmt.Fprint(out, "\r")
	for i := 0; i < n; i++ {
		fmt.Fprint(out, "\x1b[K")
		if i < n-1 {
			fmt.Fprint(out, "\x1b[1B")
		}
	}
	if n > 1 {
		fmt.Fprintf(out, "\x1b[%dA", n-1)
	}
	fmt.Fprint(out, "\r")
}

// lineRenderer keeps the last drawn state for incremental updates.
type lineRenderer struct {
	out    io.Writer
	width  int
	prompt string
	line   string
	pos    int
	rows   int
	crow   int
	ccol   int
	drawn  bool
}

func newLineRenderer(out io.Writer, prompt string, width int) *lineRenderer {
	if width < 2 {
		width = 80
	}
	return &lineRenderer{out: out, prompt: prompt, width: width}
}

func (rn *lineRenderer) snap(s string, pos, rows, cr, cc int) {
	rn.line = s
	rn.pos = pos
	rn.rows = rows
	rn.crow = cr
	rn.ccol = cc
	rn.drawn = true
}

// invalidate forgets drawn state (caller printed something else).
func (rn *lineRenderer) invalidate() {
	rn.drawn = false
	rn.rows = 0
}

// draw updates the screen to show line with the cursor at pos.
func (rn *lineRenderer) draw(line []rune, pos int) {
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	s := string(line)
	if !rn.drawn {
		rows, cr, cc := layoutCells(rn.prompt, line, pos, rn.width)
		fmt.Fprint(rn.out, rn.prompt, s)
		rn.snap(s, pos, rows, cr, cc)
		rn.moveFromEnd(rows-1, cr, cc)
		return
	}
	if s == rn.line && pos == rn.pos {
		return
	}
	// Same text, cursor moved within one row: bare horizontal move.
	if s == rn.line {
		_, cr, cc := layoutCells(rn.prompt, line, pos, rn.width)
		if cr == rn.crow {
			if dc := cc - rn.ccol; dc > 0 {
				fmt.Fprintf(rn.out, "\x1b[%dC", dc)
			} else if dc < 0 {
				fmt.Fprintf(rn.out, "\x1b[%dD", -dc)
			}
			rn.pos, rn.crow, rn.ccol = pos, cr, cc
			return
		}
		rn.full(line, pos)
		return
	}
	// Append-only at end of line: write the new bytes directly.
	if rn.pos == len([]rune(rn.line)) && pos == len(line) &&
		len(s) > len(rn.line) && strings.HasPrefix(s, rn.line) {
		fmt.Fprint(rn.out, s[len(rn.line):])
		rows, cr, cc := layoutCells(rn.prompt, line, pos, rn.width)
		rn.snap(s, pos, rows, cr, cc)
		return
	}
	// Delete-only at end of line: back up over the removed cells — but
	// only when the block stays on the same rows (shrinking across a
	// wrap boundary needs a full clear+reprint instead).
	oldRows, _, _ := layoutCells(rn.prompt, []rune(rn.line), len([]rune(rn.line)), rn.width)
	newRows, _, _ := layoutCells(rn.prompt, line, pos, rn.width)
	if pos == len(line) && len(s) < len(rn.line) && strings.HasPrefix(rn.line, s) && newRows == oldRows {
		cells := 0
		for _, r := range []rune(rn.line)[len([]rune(s)):] {
			cells += runeWidth(r)
		}
		for i := 0; i < cells; i++ {
			fmt.Fprint(rn.out, "\b")
		}
		for i := 0; i < cells; i++ {
			fmt.Fprint(rn.out, " ")
		}
		for i := 0; i < cells; i++ {
			fmt.Fprint(rn.out, "\b")
		}
		rows, cr, cc := layoutCells(rn.prompt, line, pos, rn.width)
		rn.snap(s, pos, rows, cr, cc)
		return
	}
	rn.full(line, pos)
}

// full clears the old block and reprints.
func (rn *lineRenderer) full(line []rune, pos int) {
	rn.clear()
	s := string(line)
	fmt.Fprint(rn.out, rn.prompt, s)
	rows, cr, cc := layoutCells(rn.prompt, line, pos, rn.width)
	rn.snap(s, pos, rows, cr, cc)
	rn.moveFromEnd(rows-1, cr, cc)
}

// clear erases the last drawn block, cursor ends at block start.
func (rn *lineRenderer) clear() {
	if !rn.drawn || rn.rows <= 0 {
		rn.drawn = false
		rn.rows = 0
		return
	}
	if rn.crow > 0 {
		fmt.Fprintf(rn.out, "\x1b[%dA", rn.crow)
	}
	clearRows(rn.out, rn.rows)
	rn.drawn = false
	rn.rows = 0
}

// moveFromEnd moves from block end (endRow) to (crow, ccol).
// From the end of a single-row draw the cursor is already AT the end
// cell, so only go up (when wrapped) and left; never emit redundant
// right moves (they flash on some terminals).
func (rn *lineRenderer) moveFromEnd(endRow, cr, cc int) {
	if d := endRow - cr; d > 0 {
		fmt.Fprintf(rn.out, "\x1b[%dA", d)
		fmt.Fprint(rn.out, "\r")
		if cc > 0 {
			fmt.Fprintf(rn.out, "\x1b[%dC", cc)
		}
		return
	}
	// Same row: cursor sits at endCol; walk left to cc.
	endCol := 0
	{
		_, _, ec := layoutCells(rn.prompt, []rune(rn.line), len([]rune(rn.line)), rn.width)
		endCol = ec
	}
	if dc := endCol - cc; dc > 0 {
		fmt.Fprintf(rn.out, "\x1b[%dD", dc)
	}
}

// printCompletions lists candidates in terminal columns (like bash),
// fitting width. Column-major order.
func printCompletions(out io.Writer, cands []string, width int) {
	if len(cands) == 0 {
		return
	}
	if width < 10 {
		width = 80
	}
	maxw := 0
	for _, c := range cands {
		if w := strWidth(c); w > maxw {
			maxw = w
		}
	}
	colW := maxw + 2
	cols := width / colW
	if cols < 1 {
		cols = 1
	}
	if cols > len(cands) {
		cols = len(cands)
	}
	if cols == 1 {
		for _, s := range cands {
			fmt.Fprint(out, s+"\r\n")
		}
		return
	}
	rowsN := (len(cands) + cols - 1) / cols
	for r := 0; r < rowsN; r++ {
		for c := 0; c < cols; c++ {
			idx := c*rowsN + r
			if idx >= len(cands) {
				continue
			}
			s := cands[idx]
			if c == cols-1 {
				fmt.Fprint(out, s)
			} else {
				fmt.Fprint(out, s+strings.Repeat(" ", colW-strWidth(s)))
			}
		}
		fmt.Fprint(out, "\r\n")
	}
}
