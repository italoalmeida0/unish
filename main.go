package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

var command = flag.String("c", "", "entire bash command to run")

var interactiveFlag = flag.Bool("i", false, "force interactive shell")

var version = "dev"

func versionString() string {
	if version == "" {
		return "dev"
	}
	return version
}

func init() {
	flag.BoolFunc("version", "print version and exit", func(string) error {
		fmt.Println("unish " + versionString())
		os.Exit(0)
		return nil
	})
	flag.BoolFunc("V", "print version and exit", func(string) error {
		fmt.Println("unish " + versionString())
		os.Exit(0)
		return nil
	})
}

func main() {
	flag.Parse()

	var src, name string
	var params []string
	if *interactiveFlag {
		os.Exit(runInteractive())
	}
	switch {
	case flag.NFlag() > 0:
		src = *command
		if len(flag.Args()) > 0 {
			name = flag.Args()[0]
			params = flag.Args()[1:]
		} else {
			name = "unish"
		}
		if strings.TrimSpace(src) == "" {
			os.Exit(0)
		}
	default:
		// No args and stdin is a TTY: start the interactive shell
		// (like bash with no args). Piped stdin keeps old behavior.
		if isInteractive(os.Stdin) {
			os.Exit(runInteractive())
		}
		rest := flag.Args()
		if len(rest) > 0 {
			data, err := os.ReadFile(rest[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "unish: %s: No such file or directory\n", rest[0])
				os.Exit(127)
			}
			src = string(data)
			name = rest[0]
			params = rest[1:]
		} else {
			if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
				fmt.Fprintln(os.Stderr, "usage: unish -c \"command\" | unish script.sh [args] | unish < script")
				flag.PrintDefaults()
				os.Exit(2)
			}
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintln(os.Stderr, "unish:", err)
				os.Exit(1)
			}
			src = string(data)
			name = ""
			params = nil
		}
	}
	if strings.TrimSpace(src) == "" {
		os.Exit(0)
	}

	prog, err := syntax.NewParser().Parse(strings.NewReader(src), name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unish:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runnerOpts := []interp.RunnerOption{
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.CallHandler(callOverride),
		interp.ExecHandlers(
			trackExec,
			extraHandler,
		),
		interp.ProcSubstHandler(procSubstHandler),
	}
	if len(params) > 0 {
		runnerOpts = append(runnerOpts, interp.Params(params...))
	}
	r, err := interp.New(runnerOpts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unish:", err)
		os.Exit(1)
	}

	if err := r.Run(ctx, prog); err != nil {
		var es interp.ExitStatus
		if errors.As(err, &es) {
			os.Exit(int(es))
		}
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			os.Exit(124)
		}
		fmt.Fprintln(os.Stderr, "unish:", err)
		os.Exit(1)
	}
}
