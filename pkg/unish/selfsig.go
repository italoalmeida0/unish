package unish

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
)

// activeRunner is the interpreter currently running the script, so `kill`
// can enqueue a signal for its own trap instead of terminating the process.
var (
	activeRunnerMu   sync.Mutex
	activeRunner     selfSignalRunner
	selfSigDeliverMu sync.Mutex
)

// selfSignalRunner is the small surface cmdKill needs from the interpreter.
type selfSignalRunner interface {
	QueueSignalByName(name string) bool
}

// setActiveRunner records the runner for the duration of a script.
func setActiveRunner(r selfSignalRunner) {
	activeRunnerMu.Lock()
	activeRunner = r
	activeRunnerMu.Unlock()
}

// selfPID reports whether the operand names this very process ($$, or any
// numeric representation of os.Getpid() such as 0$$ or +$$), so the signal is
// delivered to our own trap queue rather than OS Kill on the host process.
func selfPID(operand string) bool {
	operand = strings.TrimSpace(operand)
	if operand == "" {
		return false
	}
	pid, err := strconv.Atoi(operand)
	return err == nil && pid == os.Getpid()
}

func canonicalTrapSignalName(clean string) string {
	switch clean {
	case "1":
		return "HUP"
	case "2":
		return "INT"
	case "3":
		return "QUIT"
	case "6":
		return "ABRT"
	case "10":
		return "USR1"
	case "12":
		return "USR2"
	case "13":
		return "PIPE"
	case "14":
		return "ALRM"
	case "15":
		return "TERM"
	}
	return ""
}

// queueSelfSignalForHC hands a self-targeted signal to hc's active Runner
// (or standalone activeRunner fallback) while guaranteeing SIGKILL/SIGSTOP
// are never caught and concurrent embedded sessions never cross-deliver.
func queueSelfSignalForHC(hc interp.HandlerContext, sig string) bool {
	clean := strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(sig), "SIG"))
	if clean == "0" {
		return true
	}
	// POSIX: SIGKILL (9) and SIGSTOP cannot be caught or ignored.
	if clean == "KILL" || clean == "9" || clean == "STOP" {
		return false
	}

	selfSigDeliverMu.Lock()
	hc.PushActive()
	handled, hasActive := interp.QueueActiveSignalByName(clean)
	if !handled && hasActive {
		if alt := canonicalTrapSignalName(clean); alt != "" && alt != clean {
			handled, _ = interp.QueueActiveSignalByName(alt)
		}
	}
	hc.PopActive()
	selfSigDeliverMu.Unlock()

	if hasActive {
		return handled
	}
	if !standaloneCLIMode.Load() {
		return false
	}
	activeRunnerMu.Lock()
	r := activeRunner
	activeRunnerMu.Unlock()
	if r == nil {
		return false
	}
	if r.QueueSignalByName(clean) {
		return true
	}
	if alt := canonicalTrapSignalName(clean); alt != "" && alt != clean {
		return r.QueueSignalByName(alt)
	}
	return false
}

// queueSelfSignal hands the signal to the running interpreter's trap queue.
// It reports whether a trap or ignore disposition handled the signal.
func queueSelfSignal(sig string) bool {
	return queueSelfSignalForHC(interp.HandlerContext{}, sig)
}

// portableSelfSignalExitCode returns the shell exit status (128+signum) for an
// untrapped fatal self-signal, or 0 for signals whose default disposition is
// ignore or stop/continue. Works identically across Unix and Windows so
// embedded library sessions exit the script cleanly without killing the host.
func portableSelfSignalExitCode(sig string) int {
	clean := strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(sig), "SIG"))
	if clean == "" || clean == "0" {
		return 0
	}
	if runtime.GOOS != "windows" {
		return defaultSelfExitCode(clean)
	}
	switch clean {
	case "CHLD", "CLD", "CONT", "URG", "WINCH", "IO", "POLL", "STOP", "TSTP", "TTIN", "TTOU":
		return 0
	case "HUP":
		return 128 + 1
	case "INT":
		return 128 + 2
	case "QUIT":
		return 128 + 3
	case "ILL":
		return 128 + 4
	case "TRAP":
		return 128 + 5
	case "ABRT", "IOT":
		return 128 + 6
	case "BUS":
		return 128 + 7
	case "FPE":
		return 128 + 8
	case "KILL":
		return 128 + 9
	case "USR1":
		return 128 + 10
	case "SEGV":
		return 128 + 11
	case "USR2":
		return 128 + 12
	case "PIPE":
		return 128 + 13
	case "ALRM":
		return 128 + 14
	case "TERM":
		return 128 + 15
	case "XCPU":
		return 128 + 24
	case "XFSZ":
		return 128 + 25
	case "VTALRM":
		return 128 + 26
	case "PROF":
		return 128 + 27
	case "SYS":
		return 128 + 31
	}
	if n, err := strconv.Atoi(clean); err == nil && n > 0 && n <= 64 {
		switch n {
		case 17, 18, 19, 20, 21, 22, 23, 28, 29:
			return 0
		default:
			return 128 + n
		}
	}
	return 0
}
