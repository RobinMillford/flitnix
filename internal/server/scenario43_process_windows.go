//go:build windows

package server

import "os"

// terminateScenario43Child kills the child. Windows has no SIGKILL: os.Kill
// maps to TerminateProcess, which ends the process without running deferred or
// exit-time cleanup, so the durable state is exactly what reached the disk —
// the same abrupt-crash boundary Scenario 43 requires.
func terminateScenario43Child(p *os.Process) error {
	return p.Kill()
}

// scenario43AbruptTermination reports whether err is the exit error a
// terminated child produces. TerminateProcess ends the process with a nonzero
// exit status instead of a signal, so any exit error qualifies.
func scenario43AbruptTermination(err error) bool { return true }
