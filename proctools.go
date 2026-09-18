package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	extraCommands = append(extraCommands,
		extraCmd{"pgrep", cmdPgrep},
		extraCmd{"pkill", cmdPkill},
	)
}

func cmdPgrep(ctx context.Context, hc interp.HandlerContext, args []string) error {
	_ = ctx
	fs := newFlagSet("pgrep", hc.Stderr)
	full := fs.Bool("f", false, "")
	ignoreCase := fs.Bool("i", false, "")
	listName := fs.Bool("l", false, "")
	exact := fs.Bool("x", false, "")
	count := fs.Bool("c", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(hc.Stderr, "pgrep: no matching criteria specified")
		return flag.ErrHelp
	}
	pattern := rest[0]
	matched, err := matchProcs(pattern, *full, *ignoreCase, *exact)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "pgrep: %v\n", err)
		return exitError{2}
	}
	if *count {
		fmt.Fprintln(hc.Stdout, len(matched))
		if len(matched) == 0 {
			return exitError{1}
		}
		return nil
	}
	for _, p := range matched {
		if *listName {
			fmt.Fprintf(hc.Stdout, "%d %s\n", p.pid, p.cmd)
		} else {
			fmt.Fprintln(hc.Stdout, p.pid)
		}
	}
	if len(matched) == 0 {
		return exitError{1}
	}
	return nil
}

func cmdPkill(ctx context.Context, hc interp.HandlerContext, args []string) error {
	_ = ctx
	sig := "TERM"
	var cleanArgs []string
	cleanArgs = append(cleanArgs, args[0])
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") && len(a) > 1 && (a[1] < '0' || a[1] > '9') {
			up := strings.ToUpper(strings.TrimPrefix(a, "-"))
			if isSignalName(up) {
				sig = strings.TrimPrefix(up, "SIG")
				continue
			}
		} else if strings.HasPrefix(a, "-") && len(a) > 1 && (a[1] >= '0' && a[1] <= '9') {
			sig = a[1:]
			continue
		}
		cleanArgs = append(cleanArgs, a)
	}
	fs := newFlagSet("pkill", hc.Stderr)
	full := fs.Bool("f", false, "")
	ignoreCase := fs.Bool("i", false, "")
	exact := fs.Bool("x", false, "")
	echo := fs.Bool("e", false, "")
	sigFlag := fs.String("signal", "", "")
	if err := fs.Parse(cleanArgs[1:]); err != nil {
		return err
	}
	if *sigFlag != "" {
		sig = strings.ToUpper(strings.TrimPrefix(strings.TrimPrefix(*sigFlag, "-"), "SIG"))
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprintln(hc.Stderr, "pkill: no matching criteria specified")
		return flag.ErrHelp
	}
	pattern := rest[0]
	matched, err := matchProcs(pattern, *full, *ignoreCase, *exact)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "pkill: %v\n", err)
		return exitError{2}
	}
	if len(matched) == 0 {
		return exitError{1}
	}
	killed := 0
	for _, p := range matched {
		proc, err := os.FindProcess(p.pid)
		if err != nil {
			continue
		}
		if err := killProc(proc, sig); err == nil {
			killed++
			if *echo {
				fmt.Fprintf(hc.Stdout, "%s killed (pid %d)\n", p.cmd, p.pid)
			}
		}
	}
	if killed == 0 {
		return exitError{1}
	}
	return nil
}

func isSignalName(s string) bool {
	clean := strings.TrimPrefix(s, "SIG")
	switch clean {
	case "HUP", "INT", "QUIT", "KILL", "TERM", "USR1", "USR2", "STOP", "CONT", "9", "15":
		return true
	}
	return false
}

func matchProcs(pattern string, full, ignoreCase, exact bool) ([]procInfo, error) {
	procs, err := listProcs()
	if err != nil {
		return nil, err
	}
	myPid := os.Getpid()
	rePattern := pattern
	if exact {
		rePattern = "^" + pattern + "$"
	}
	if ignoreCase {
		rePattern = "(?i)" + rePattern
	}
	re, err := regexp.Compile(rePattern)
	if err != nil {
		return nil, err
	}
	var matched []procInfo
	for _, p := range procs {
		if p.pid == myPid {
			continue
		}
		target := p.cmd
		if !full {
			target = filepath.Base(p.cmd)
		}
		if re.MatchString(target) {
			matched = append(matched, p)
		}
	}
	return matched, nil
}
