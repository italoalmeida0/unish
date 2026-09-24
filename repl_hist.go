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
	items []string
	file  string
	maxIn int // HISTSIZE cap for memory (0 = default 500)
	maxF  int // HISTFILESIZE cap for file (0 = default: same as maxIn)
}

func defaultHistPath() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, defaultHistFile)
	}
	return defaultHistFile
}

func newHistStore() *histStore {
	return &histStore{maxIn: 500}
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

// add appends line honoring HISTCONTROL (caller passes current last for
// ignoredups; erasedups removes older dups).
func (h *histStore) add(line, histControl string) {
	if line == "" {
		return
	}
	ctl := strings.ToLower(histControl)
	if strings.Contains(ctl, "ignorespace") || strings.Contains(ctl, "ignoreboth") {
		if line[0] == ' ' || line[0] == '\t' {
			return
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
			return
		}
	}
	h.items = append(h.items, line)
	if h.maxIn > 0 && len(h.items) > h.maxIn {
		h.items = h.items[len(h.items)-h.maxIn:]
	}
}

// load reads the histfile (tolerates missing file).
func (h *histStore) load(path string, maxFile int) {
	h.file = path
	h.maxF = maxFile
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	// Drop trailing empty from final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if maxFile > 0 && len(lines) > maxFile {
		lines = lines[len(lines)-maxFile:]
	}
	// Strip \r (file written with \n; tolerate CRLF).
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	h.items = append(h.items, lines...)
	if h.maxIn > 0 && len(h.items) > h.maxIn {
		h.items = h.items[len(h.items)-h.maxIn:]
	}
}

// save appends new items to the histfile (bash histappend semantics:
// never truncates existing content except HISTFILESIZE cap).
func (h *histStore) save(newItems []string) {
	if h.file == "" || len(newItems) == 0 {
		return
	}
	var sb strings.Builder
	for _, l := range newItems {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	f, err := os.OpenFile(h.file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(sb.String())
	_ = f.Close()
	if h.maxF > 0 {
		h.capFile()
	}
}

func (h *histStore) capFile() {
	data, err := os.ReadFile(h.file)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > h.maxF {
		lines = lines[len(lines)-h.maxF:]
		_ = os.WriteFile(h.file, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	}
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
	i := 0
	for i < len(line) {
		c := line[i]
		if c != '!' {
			sb.WriteByte(c)
			i++
			continue
		}
		// Inside single quotes: literal.
		if inSingleQuotes(line, i) {
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
			i += 2
		case n == '#':
			// !# = line so far.
			sb.WriteString(sb.String())
			i += 2
		case n == ' ' || n == '\t' || n == '=' || n == '(':
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
			i = end
		}
	}
	return sb.String(), sb.String(), nil
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
