package main

// Interactive history: in-memory ring + histfile persistence,
// HISTCONTROL/HISTSIZE/HISTFILESIZE/builtins/history/!/Ctrl-R.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultHistFile = ".unish_history"

// histStore is the interactive history list (oldest..newest).
type histStore struct {
	items         []string
	file          string
	maxIn         int // HISTSIZE cap for memory (default 500; <0 = unlimited, 0 = disabled)
	maxF          int // HISTFILESIZE cap for file (<0 = unset, 0 = empty file)
	fileLinesRead int // total lines tracked in file (for `history -n`)
}

func defaultHistPath() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, defaultHistFile)
	}
	return defaultHistFile
}

func newHistStore() *histStore {
	return &histStore{maxIn: 500, maxF: -1}
}

// shouldSave reports HISTCONTROL filtering (ignorespace/ignoredups,
// erasedups handled at add time).
func shouldSave(line string, hc interface {
	Getenv(string) string
}) bool {
	return shouldSaveCtl(line, hc.Getenv("HISTCONTROL"))
}

func shouldSaveCtl(line, ctl string) bool {
	ctl = strings.ToLower(ctl)
	ignSpace := strings.Contains(ctl, "ignorespace") || strings.Contains(ctl, "ignoreboth")
	ignDup := strings.Contains(ctl, "ignoredups") || strings.Contains(ctl, "ignoreboth")
	if ignSpace && len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
		return false
	}
	_ = ignDup
	return true
}

// add appends line honoring HISTCONTROL, reporting whether the entry was kept.
func (h *histStore) add(line, histControl string) bool {
	if line == "" || h.maxIn == 0 {
		return false
	}
	ctl := strings.ToLower(histControl)
	if strings.Contains(ctl, "ignorespace") || strings.Contains(ctl, "ignoreboth") {
		if line[0] == ' ' || line[0] == '\t' {
			return false
		}
	}
	if strings.Contains(ctl, "erasedups") {
		kept := h.items[:0]
		for _, it := range h.items {
			if it != line {
				kept = append(kept, it)
			}
		}
		h.items = kept
	} else if strings.Contains(ctl, "ignoredups") || strings.Contains(ctl, "ignoreboth") {
		if len(h.items) > 0 && h.items[len(h.items)-1] == line {
			return false
		}
	}
	h.items = append(h.items, line)
	if h.maxIn > 0 && len(h.items) > h.maxIn {
		h.items = h.items[len(h.items)-h.maxIn:]
	}
	return true
}

func readHistLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	lines := strings.Split(string(data), "\n")
	// Drop trailing empty from final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	// Strip \r (file written with \n; tolerate CRLF).
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines, nil
}

// load reads the histfile (tolerates missing file). Pass maxFile < 0 to
// keep the existing h.maxF cap.
func (h *histStore) load(path string, maxFile int) {
	h.file = path
	if maxFile >= 0 {
		h.maxF = maxFile
	}
	if path == "" {
		return
	}
	lines, err := readHistLines(path)
	if err != nil {
		return
	}
	h.fileLinesRead = len(lines)
	if h.maxF == 0 {
		lines = nil
	} else if h.maxF > 0 && len(lines) > h.maxF {
		lines = lines[len(lines)-h.maxF:]
	}
	if h.maxIn == 0 {
		h.items = nil
		return
	}
	h.items = append(h.items, lines...)
	if h.maxIn > 0 && len(h.items) > h.maxIn {
		h.items = h.items[len(h.items)-h.maxIn:]
	}
}

// loadUnread appends only lines from path not yet read in this session
// (used by `history -n`).
func (h *histStore) loadUnread(path string) {
	if path == "" {
		return
	}
	lines, err := readHistLines(path)
	if err != nil {
		return
	}
	added := lines
	if path == h.file {
		start := h.fileLinesRead
		if start > len(lines) {
			start = len(lines)
		}
		added = lines[start:]
		h.fileLinesRead = len(lines)
	}
	if h.maxIn == 0 {
		h.items = nil
		return
	}
	h.items = append(h.items, added...)
	if h.maxIn > 0 && len(h.items) > h.maxIn {
		h.items = h.items[len(h.items)-h.maxIn:]
	}
}

// save appends new items to the histfile (bash histappend semantics:
// never truncates existing content except HISTFILESIZE cap).
func (h *histStore) save(newItems []string) {
	h.saveTo(h.file, newItems)
}

func (h *histStore) saveTo(path string, newItems []string) {
	if path == "" {
		return
	}
	if path == h.file && h.maxF == 0 {
		_ = os.WriteFile(path, nil, 0o600)
		h.fileLinesRead = 0
		return
	}
	if len(newItems) == 0 {
		return
	}
	var sb strings.Builder
	for _, l := range newItems {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(sb.String())
	_ = f.Close()
	if path == h.file {
		h.fileLinesRead += len(newItems)
		if h.maxF > 0 {
			h.capFile()
		}
	}
}

func (h *histStore) capFile() {
	lines, err := readHistLines(h.file)
	if err != nil {
		return
	}
	if len(lines) > h.maxF {
		lines = lines[len(lines)-h.maxF:]
		var content string
		if len(lines) > 0 {
			content = strings.Join(lines, "\n") + "\n"
		}
		_ = os.WriteFile(h.file, []byte(content), 0o600)
	}
	h.fileLinesRead = len(lines)
}

// expandBang implements bash ! history expansion on the raw line.
// Returns (expanded, displayEcho, err). displayEcho is the line to show
// when histverify is set (caller re-prompts); err set on failed lookup
// (bash prints the error and does NOT execute).
func expandBang(line string, items []string) (string, string, error) {
	if !strings.Contains(line, "!") {
		return line, "", nil
	}
	var sb strings.Builder
	inS, inD := false, false
	atWordBoundary := true
	didExpand := false
	i := 0
	for i < len(line) {
		c := line[i]
		if inS {
			if c == '\'' {
				inS = false
			}
			sb.WriteByte(c)
			i++
			continue
		}
		if c == '\\' && i+1 < len(line) {
			if line[i+1] == '!' {
				sb.WriteByte('!')
				atWordBoundary = false
				i += 2
				continue
			}
			sb.WriteByte(c)
			sb.WriteByte(line[i+1])
			atWordBoundary = line[i+1] == '\n'
			i += 2
			continue
		}
		if c == '\'' && !inD {
			inS = true
			atWordBoundary = false
			sb.WriteByte(c)
			i++
			continue
		}
		if c == '"' {
			inD = !inD
			atWordBoundary = false
			sb.WriteByte(c)
			i++
			continue
		}
		if !inD && c == '#' && atWordBoundary {
			for i < len(line) && line[i] != '\n' {
				sb.WriteByte(line[i])
				i++
			}
			atWordBoundary = true
			continue
		}
		if c != '!' {
			sb.WriteByte(c)
			atWordBoundary = !inD && (c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' || c == '(' || c == ')')
			i++
			continue
		}
		atWordBoundary = false
		// Exempt $! (last background PID) and ${!...} (indirect expansion).
		if (i >= 1 && line[i-1] == '$') || (i >= 2 && line[i-2] == '$' && line[i-1] == '{') {
			sb.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(line) {
			sb.WriteByte(c)
			i++
			continue
		}
		n := line[i+1]
		switch {
		case n == '!':
			if len(items) == 0 {
				return "", "", errNoHist("!!")
			}
			sb.WriteString(items[len(items)-1])
			didExpand = true
			i += 2
		case n == '#':
			// !# = line so far.
			sb.WriteString(sb.String())
			didExpand = true
			i += 2
		case n == ' ' || n == '\t' || n == '\n' || n == '=' || n == '(':
			sb.WriteByte(c)
			i++
		case n == '-' || (n >= '0' && n <= '9'):
			j := i + 1
			neg := false
			if line[j] == '-' {
				neg = true
				j++
			}
			k := j
			for k < len(line) && line[k] >= '0' && line[k] <= '9' {
				k++
			}
			if k == j {
				// Bare "!-" (not followed by digits): literal unless prefix match?
				// In bash at end/delim, "!-" is literal.
				if j >= len(line) || isBangDelim(line[j]) {
					sb.WriteByte(c)
					i++
					continue
				}
				// Otherwise treat "-..." as a prefix event search (e.g. !-foo).
				k = j
				for k < len(line) && !isBangDelim(line[k]) {
					k++
				}
				pat := line[i+1 : k]
				found := -1
				for x := len(items) - 1; x >= 0; x-- {
					if strings.HasPrefix(strings.TrimLeft(items[x], " \t"), pat) {
						found = x
						break
					}
				}
				if found < 0 {
					return "", "", errNoHist(line[i:k])
				}
				sb.WriteString(items[found])
				didExpand = true
				i = k
				continue
			}
			num, _ := strconv.Atoi(line[j:k])
			var idx int
			if neg {
				idx = len(items) - num
			} else {
				idx = num - 1
			}
			if idx < 0 || idx >= len(items) {
				return "", "", errNoHist(line[i:k])
			}
			sb.WriteString(items[idx])
			didExpand = true
			i = k
		default:
			// !prefix or !?substr[?]
			j := i + 1
			sub := false
			if line[j] == '?' {
				sub = true
				j++
			}
			k := j
			for k < len(line) && !isBangDelim(line[k]) {
				k++
			}
			pat := line[j:k]
			end := k
			if sub && end < len(line) && line[end] == '?' {
				end++
			}
			if pat == "" {
				sb.WriteByte(c)
				i++
				continue
			}
			found := -1
			if sub {
				for x := len(items) - 1; x >= 0; x-- {
					if strings.Contains(items[x], pat) {
						found = x
						break
					}
				}
			} else {
				for x := len(items) - 1; x >= 0; x-- {
					if strings.HasPrefix(strings.TrimLeft(items[x], " \t"), pat) {
						found = x
						break
					}
				}
			}
			if found < 0 {
				return "", "", errNoHist(line[i:end])
			}
			sb.WriteString(items[found])
			didExpand = true
			i = end
		}
	}
	out := sb.String()
	displayEcho := ""
	if didExpand {
		displayEcho = out
	}
	return out, displayEcho, nil
}

func isBangDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\n', ';', '&', '|', '<', '>', '(', ')', '{', '}', '"', '\'', '\\', '?':
		return true
	}
	return false
}

func inSingleQuotes(line string, at int) bool {
	inS, inD, esc := false, false, false
	for i := 0; i < at && i < len(line); i++ {
		c := line[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && !inS {
			esc = true
			continue
		}
		if c == '\'' && !inD {
			inS = !inS
			continue
		}
		if c == '"' && !inS {
			inD = !inD
		}
	}
	return inS
}

type histExpandError struct{ ref string }

func (e *histExpandError) Error() string { return "bash: " + e.ref + ": event not found" }

func errNoHist(ref string) error { return &histExpandError{ref} }
