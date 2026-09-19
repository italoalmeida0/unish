package main

// `history` builtin + callOverride hook so the interactive history is
// visible inside the shell (history, history -c/-d/-s/-p/-a/-n/-r/-w).

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// replHist points at the live REPL (nil in -c/script mode).
var replHist *repl

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
	i := 0
	for i < len(a) {
		switch a[i] {
		case "-c":
			rp.hist.items = nil
			rp.histNew = nil
			rp.histIdx = 0
			return nil
		case "-d":
			i++
			if i >= len(a) {
				fmt.Fprintln(hc.Stderr, "history: -d: option requires an argument")
				return exitError{1}
			}
			idx, err := strconv.Atoi(a[i])
			if err != nil || idx < 1 || idx > n {
				fmt.Fprintf(hc.Stderr, "history: %s: history position out of range\n", a[i])
				return exitError{1}
			}
			rp.hist.items = append(rp.hist.items[:idx-1], rp.hist.items[idx:]...)
			return nil
		case "-s":
			i++
			for ; i < len(a); i++ {
				rp.hist.add(a[i], rp.getenv("HISTCONTROL"))
				rp.histNew = append(rp.histNew, a[i])
			}
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
			rp.hist.save(rp.histNew)
			rp.histNew = nil
			return nil
		case "-n":
			// Re-read file additions (append-only).
			before := len(rp.hist.items)
			rp.hist.load(rp.hist.file, 0)
			_ = before
			return nil
		case "-r":
			rp.hist.items = nil
			rp.hist.load(rp.hist.file, 0)
			rp.histIdx = len(rp.hist.items)
			return nil
		case "-w":
			// Write whole history to file (truncate + write).
			if rp.hist.file != "" {
				_ = writeFileTrunc(rp.hist.file, strings.Join(rp.hist.items, "\n")+"\n")
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
	f, err := openFileRetry(path, 577, 0o600)
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
