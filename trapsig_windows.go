//go:build windows

package main

import "os"

// trapSignals on Windows: only the interrupt is deliverable to a console
// process; the rest do not exist as trappable signals.
func trapSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
