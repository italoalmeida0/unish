package main

// `history` builtin + callOverride hook so the interactive history is
// visible inside the shell (history, history -c/-d/-s/-p/-a/-n/-r/-w).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// replHist points at the live REPL (nil in -c/script mode).
var replHist *repl

func (rp *repl) syncHistBounds() {
	rp.histIdx = len(rp.hist.items)
	if rp.searchIdx >= len(rp.hist.items) {
		rp.searchIdx = len(rp.hist.items) - 1
	}
}

func (rp *repl) recordHist(line string) {
	ctl := rp.getenv("HISTCONTROL")
	if !rp.hist.add(line, ctl) {
		return
	}
	if strings.Contains(strings.ToLower(ctl), "erasedups") {
		kept := rp.histNew[:0]
		for _, it := range rp.histNew {
			if it != line {
				kept = append(kept, it)
			}
		}
		rp.histNew = kept
	}
	rp.histNew = append(rp.histNew, line)
}

func (rp *repl) targetHistFile(explicit string) string {
	if explicit != "" {
		return resolve(rp.rDir(), explicit)
	}
	if envF := rp.getenv("HISTFILE"); envF != "" {
		if envF == "/dev/null" {
			return ""
		}
		rp.hist.file = envF
		return envF
	}
	return rp.hist.file
}

func cmdHistory(_ context.Context, hc interp.HandlerContext, args []string) error {
	rp := replHist
	// Non-interactive: behave like bash with empty history for reads,
	// support -s (add) so scripts can seed it.
	if rp == nil {
		for i := 1; i < len(args); i++ {
			if args[i] == "-s" {
				return nil
			}
		}
		if len(args) == 1 {
			return nil
		}
		return nil
	}
	a := args[1:]
	n := len(rp.hist.items)
	show := func(from, to int) {
		for i := from; i < to; i++ {
			fmt.Fprintf(hc.Stdout, "%5d  %s\n", i+1, rp.hist.items[i])
		}
	}
	optFileArg := func(idx int) string {
		if idx+1 < len(a) && !strings.HasPrefix(a[idx+1], "-") {
			return a[idx+1]
		}
		return ""
	}
	i := 0
	for i < len(a) {
		switch a[i] {
		case "-c":
			rp.hist.items = nil
			rp.histNew = nil
			rp.syncHistBounds()
			return nil
		case "-d":
			i++
			if i >= len(a) {
				fmt.Fprintln(hc.Stderr, "history: -d: option requires an argument")
				return exitError{1}
			}
			idx, err := strconv.Atoi(a[i])
			if err == nil && idx < 0 {
				idx = n + 1 + idx
			}
			if err != nil || idx < 1 || idx > n {
				fmt.Fprintf(hc.Stderr, "history: %s: history position out of range\n", a[i])
				return exitError{1}
			}
			deleted := rp.hist.items[idx-1]
			rp.hist.items = append(rp.hist.items[:idx-1], rp.hist.items[idx:]...)
			if idx-1 >= n-len(rp.histNew) {
				for j := len(rp.histNew) - 1; j >= 0; j-- {
					if rp.histNew[j] == deleted {
						rp.histNew = append(rp.histNew[:j], rp.histNew[j+1:]...)
						break
					}
				}
			}
			rp.syncHistBounds()
			return nil
		case "-s":
			i++
			for ; i < len(a); i++ {
				rp.recordHist(a[i])
			}
			rp.syncHistBounds()
			return nil
		case "-p":
			i++
			var out []string
			for ; i < len(a); i++ {
				exp, _, err := expandBang(a[i], rp.hist.items)
				if err != nil {
					fmt.Fprintln(hc.Stderr, err.Error())
					return exitError{1}
				}
				out = append(out, exp)
			}
			for _, l := range out {
				fmt.Fprintln(hc.Stdout, l)
			}
			return nil
		case "-a":
			explicit := optFileArg(i)
			target := rp.targetHistFile(explicit)
			rp.hist.saveTo(target, rp.histNew)
			if explicit == "" || target == rp.hist.file {
				rp.histNew = nil
			}
			return nil
		case "-n":
			target := rp.targetHistFile(optFileArg(i))
			rp.hist.loadUnread(target)
			rp.syncHistBounds()
			return nil
		case "-r":
			explicit := optFileArg(i)
			target := rp.targetHistFile(explicit)
			if explicit == "" {
				rp.hist.items = nil
				rp.hist.load(target, -1)
			} else {
				lines, err := readHistLines(target)
				if err == nil && rp.hist.maxIn != 0 {
					rp.hist.items = append(rp.hist.items, lines...)
					if rp.hist.maxIn > 0 && len(rp.hist.items) > rp.hist.maxIn {
						rp.hist.items = rp.hist.items[len(rp.hist.items)-rp.hist.maxIn:]
					}
				}
			}
			rp.syncHistBounds()
			return nil
		case "-w":
			// Write whole history to file (truncate + write).
			explicit := optFileArg(i)
			target := rp.targetHistFile(explicit)
			if target != "" {
				var content string
				if len(rp.hist.items) > 0 {
					content = strings.Join(rp.hist.items, "\n") + "\n"
				}
				_ = writeFileTrunc(target, content)
				if target == rp.hist.file {
					rp.hist.fileLinesRead = len(rp.hist.items)
					rp.histNew = nil
				}
			}
			return nil
		default:
			if strings.HasPrefix(a[i], "-") {
				fmt.Fprintf(hc.Stderr, "history: invalid option -- '%s'\n", strings.TrimPrefix(a[i], "-"))
				return exitError{1}
			}
			// history N: last N.
			cnt, err := strconv.Atoi(a[i])
			if err != nil || cnt < 0 {
				fmt.Fprintf(hc.Stderr, "history: %s: numeric argument required\n", a[i])
				return exitError{1}
			}
			from := n - cnt
			if from < 0 {
				from = 0
			}
			show(from, n)
			return nil
		}
	}
	show(0, n)
	return nil
}

func writeFileTrunc(path, content string) error {
	f, err := openFileRetry(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	cerr := f.Close()
	if err != nil {
		return err
	}
	return cerr
}
