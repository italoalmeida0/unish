package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
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
	if operand == strconv.Itoa(me) {
		return true
	}
	return false
}

// queueSelfSignal hands the signal to the running interpreter's trap queue.
// It reports whether a trap was registered (and thus queued).
func queueSelfSignal(sig string) bool {
	activeRunnerMu.Lock()
	r := activeRunner
	activeRunnerMu.Unlock()
	if r == nil {
		return false
	}
	return r.QueueSignalByName(sig)
}
