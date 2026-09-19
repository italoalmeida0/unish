package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

var shellUmask = 0o022

// cmdUmask implements `umask [MODE]`: prints the current mask (0022) or
// sets it. Mirrors bash behavior for the common forms.
func cmdUmask(_ context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) == 1 {
		fmt.Fprintf(hc.Stdout, "%04o\n", shellUmask)
		return nil
	}
	for _, a := range args[1:] {
		if a == "-S" {
			fmt.Fprintln(hc.Stdout, umaskSymbolic(shellUmask))
			continue
		}
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(hc.Stderr, "umask: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
			return flag.ErrHelp
		}
		v, err := strconv.ParseUint(a, 8, 32)
		if err != nil || v > 0o777 {
			fmt.Fprintf(hc.Stderr, "umask: '%s': invalid octal number\n", a)
			return exitError{1}
		}
		shellUmask = int(v)
		applyUmask(shellUmask)
	}
	return nil
}

func umaskSymbolic(m int) string {
	var sb strings.Builder
	for i, c := range []byte{'u', 'g', 'o'} {
		bits := (m >> (6 - 3*i)) & 7
		sb.WriteByte(c)
		sb.WriteByte('=')
		if bits&4 == 0 {
			sb.WriteByte('r')
		}
		if bits&2 == 0 {
			sb.WriteByte('w')
		}
		if bits&1 == 0 {
			sb.WriteByte('x')
		}
		if i < 2 {
			sb.WriteByte(',')
		}
	}
	return sb.String()
}

// cmdUlimit implements a useful subset of `ulimit`: `ulimit -n` (open
// files), `ulimit -a` (all), with optional `ulimit -n N` to set.
func cmdUlimit(_ context.Context, hc interp.HandlerContext, args []string) error {
	showAll := false
	resource := "n"
	var setVal *uint64
	argv := args[1:]
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-a":
			showAll = true
		case a == "-n" || a == "-u" || a == "-f" || a == "-c" || a == "-d" || a == "-s" || a == "-v" || a == "-m":
			resource = strings.TrimPrefix(a, "-")
		case (len(a) == 3 && a[0] == '-' && (a[1] == 'H' || a[1] == 'S')) ||
			(len(a) == 4 && a[0] == '-' && a[1] == 'H' && a[2] == 'S') ||
			(len(a) == 4 && a[0] == '-' && a[1] == 'S' && a[2] == 'H'):
			// Combined hardness+resource like -Sn/-Hn/-HSn: hardness
			// is accepted and ignored; resource is the last letter.
			resource = a[len(a)-1:]
		case a == "-H" || a == "-S":
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(hc.Stderr, "ulimit: invalid option -- '%s'\n", strings.TrimPrefix(a, "-"))
			return flag.ErrHelp
		default:
			if v, err := strconv.ParseUint(a, 10, 64); err == nil {
				setVal = &v
			} else {
				fmt.Fprintf(hc.Stderr, "ulimit: '%s': invalid number\n", a)
				return exitError{1}
			}
		}
	}
	if showAll {
		nofile, _ := ulimitNofile()
		fmt.Fprintf(hc.Stdout, "open files                      (-n) %d\n", nofile)
		return nil
	}
	if setVal != nil {
		if err := ulimitSetNofile(*setVal); err != nil {
			fmt.Fprintln(hc.Stderr, "ulimit:", err)
			return exitError{1}
		}
		return nil
	}
	switch resource {
	case "n":
		nofile, err := ulimitNofile()
		if err != nil {
			fmt.Fprintln(hc.Stderr, "ulimit:", err)
			return exitError{1}
		}
		fmt.Fprintln(hc.Stdout, nofile)
	default:
		fmt.Fprintln(hc.Stdout, "unlimited")
	}
	_ = runtime.GOOS
	return nil
}
