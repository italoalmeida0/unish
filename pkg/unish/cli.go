package unish

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

func versionString(ver string) string {
	if ver == "" {
		return "dev"
	}
	return ver
}

// RunCLI executes the standalone unish command-line interface using a fresh
// local FlagSet so importing package unish never pollutes global flag state.
func RunCLI(version string, args []string) int {
	prevCLI := standaloneCLIMode.Swap(true)
	defer standaloneCLIMode.Store(prevCLI)

	progName := "unish"
	var rawArgs []string
	if len(args) > 0 {
		if args[0] != "" {
			progName = args[0]
		}
		rawArgs = args[1:]
	}

	fs := flag.NewFlagSet(progName, flag.ContinueOnError)
	command := fs.String("c", "", "entire bash command to run")
	interactiveFlag := fs.Bool("i", false, "force interactive shell")

	showVersion := false
	fs.BoolFunc("version", "print version and exit", func(string) error {
		showVersion = true
		return nil
	})
	fs.BoolFunc("V", "print version and exit", func(string) error {
		showVersion = true
		return nil
	})

	if err := fs.Parse(rawArgs); err != nil {
		if showVersion {
			fmt.Println("unish " + versionString(version))
			return 0
		}
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if showVersion {
		fmt.Println("unish " + versionString(version))
		return 0
	}

	var src, name string
	var params []string
	if *interactiveFlag {
		return runInteractive()
	}
	switch {
	case fs.NFlag() > 0:
		src = *command
		if len(fs.Args()) > 0 {
			name = fs.Args()[0]
			params = fs.Args()[1:]
		} else {
			name = "bash"
		}
		if strings.TrimSpace(src) == "" {
			return 0
		}
	default:
		rest := fs.Args()
		if len(rest) > 0 {
			data, err := os.ReadFile(rest[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "bash: %s: No such file or directory\n", rest[0])
				return 127
			}
			src = string(data)
			name = rest[0]
			params = rest[1:]
		} else if termIsTTY(os.Stdin) {
			// No args and stdin is a real terminal TTY: start the interactive
			// shell (like bash with no args). Non-TTY char devices such as
			// /dev/null or NUL must NOT enter the interactive REPL.
			return runInteractive()
		} else {
			if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
				fmt.Fprintln(os.Stderr, "usage: unish -c \"command\" | unish script.sh [args] | unish < script")
				fs.PrintDefaults()
				return 2
			}
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintln(os.Stderr, "bash:", err)
				return 1
			}
			src = string(data)
			name = ""
			params = nil
		}
	}
	if strings.TrimSpace(src) == "" {
		return 0
	}

	prog, err := syntax.NewParser().Parse(strings.NewReader(src), name)
	if err != nil {
		// Parse errors already carry "name:line:col:" (e.g. bash:1:10: ...),
		// like bash's "bash: -c: line 1: ..."; print raw, no double prefix.
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runnerOpts := []interp.RunnerOption{
		interp.StdIO(os.Stdin, os.Stdout, os.Stderr),
		interp.CallHandler(callOverride),
		interp.OpenHandler(shellOpenHandler()),
		interp.ExecHandlers(
			trackExec,
			extraHandler,
		),
		interp.ProcSubstHandler(procSubstHandler),
	}
	if len(params) > 0 {
		runnerOpts = append(runnerOpts, interp.Params(append([]string{"--"}, params...)...))
	}
	r, err := interp.New(runnerOpts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bash:", err)
		return 1
	}

	// Let kill enqueue signals for this runner's own traps.
	setActiveRunner(r)
	defer setActiveRunner(nil)

	// Ctrl-C: run the script's INT trap when it has one (bash semantics),
	// otherwise interrupt the shell like before.
	sigCh := make(chan os.Signal, 1)
	// Catch the signals a script can trap, not just Ctrl-C: bash runs the
	// INT/TERM/HUP/QUIT trap when the shell receives one. Without Notify the
	// default action kills the process before any trap can run.
	signal.Notify(sigCh, trapSignals()...)
	defer signal.Stop(sigCh)
	sigDone := make(chan struct{})
	defer close(sigDone)
	go func() {
		for {
			select {
			case <-sigDone:
				return
			case sig := <-sigCh:
				// setSigTrap already registered signal.Notify(r.sigCh, sig);
				// checking TrapRegistered avoids queueing OS signals twice.
				if r.TrapRegistered(sig) {
					continue
				}
				cancel()
			}
		}
	}()

	if err := r.Run(ctx, prog); err != nil {
		var es interp.ExitStatus
		if errors.As(err, &es) {
			return int(es)
		}
		if errors.Is(err, context.Canceled) {
			return 130
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 124
		}
		fmt.Fprintln(os.Stderr, "bash:", err)
		return 1
	}
	return 0
}
