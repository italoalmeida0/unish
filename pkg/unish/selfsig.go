package unish

import (
	"os"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
)

// activeRunner is the interpreter currently running the script, so `kill`
// can enqueue a signal for its own trap instead of terminating the process.
var (
	activeRunnerMu sync.Mutex
	activeRunner   selfSignalRunner
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

// selfPID reports whether the operand names this very process ($$, or the
// literal pid), so the signal should be delivered to our own trap queue.
func selfPID(operand string) bool {
	operand = strings.TrimSpace(operand)
	if operand == "" {
		return false
	}
	me := os.Getpid()
	return operand == strconv.Itoa(me)
}

// queueSelfSignal hands the signal to the running interpreter's trap queue.
// It reports whether a trap or ignore disposition handled the signal.
func queueSelfSignal(sig string) bool {
	if sig == "0" {
		return true
	}
	if handled, hasActive := interp.QueueActiveSignalByName(sig); hasActive {
		return handled
	}
	activeRunnerMu.Lock()
	r := activeRunner
	activeRunnerMu.Unlock()
	if r == nil {
		return false
	}
	return r.QueueSignalByName(sig)
}
