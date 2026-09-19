package main

import (
	"bufio"
	"io"
)

// lineReader is a drop-in replacement for bufio.Scanner (Scan/Text/Err
// loop idiom) with NO token length limit.
//
// bufio.Scanner caps tokens at bufio.MaxScanTokenSize (64KB) by default,
// and this tree raised it to 1MB — still silently DROPPING any longer
// line (Scan returns false, Err reports ErrTooLong, callers treat it as
// EOF). GNU tools have no line-length limit; a real-world VSCode file
// with a 1.3MB minified line was silently skipped by grep, producing
// wrong counts (31894 instead of 31895).
//
// Semantics match bufio.ScanLines exactly:
//   - lines split on '\n'; trailing '\n' does not yield an extra record;
//   - final unterminated line is still returned;
//   - a trailing '\r' is stripped (CRLF handling, like ScanLines);
//   - streams incrementally (safe on infinite inputs like `yes | head`).
type lineReader struct {
	br   *bufio.Reader
	line string
	err  error
	done bool
}

func newLineReader(r io.Reader) *lineReader {
	if br, ok := r.(*bufio.Reader); ok {
		return &lineReader{br: br}
	}
	return &lineReader{br: bufio.NewReader(r)}
}

// Scan advances to the next line, reporting false on EOF/error.
func (l *lineReader) Scan() bool {
	if l.done {
		return false
	}
	var buf []byte
	for {
		frag, err := l.br.ReadBytes('\n')
		buf = append(buf, frag...)
		if err != nil {
			if err == io.EOF {
				if len(buf) == 0 {
					l.done = true
					return false
				}
				l.line = dropTrailingNewline(buf)
				l.done = true
				return true
			}
			l.err = err
			l.done = true
			return false
		}
		// err == nil: frag ends with '\n'.
		l.line = dropTrailingNewline(buf)
		return true
	}
}

// Text returns the most recent line (without line ending).
func (l *lineReader) Text() string { return l.line }

// Err returns the first non-EOF read error, if any.
func (l *lineReader) Err() error { return l.err }

// dropTrailingNewline strips one trailing '\n' and, like bufio.ScanLines,
// one trailing '\r' before it (CRLF).
func dropTrailingNewline(buf []byte) string {
	if n := len(buf); n > 0 && buf[n-1] == '\n' {
		buf = buf[:n-1]
		if n := len(buf); n > 0 && buf[n-1] == '\r' {
			buf = buf[:n-1]
		}
	}
	return string(buf)
}
