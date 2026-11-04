//go:build !windows

package server

import (
	"os"
	"strings"
	"syscall"
)

// terminateScenario43Child kills the child the way an abrupt crash would: an
// uncatchable signal, so no deferred or shutdown cleanup runs and the durable
// state is exactly what reached the disk.
func terminateScenario43Child(p *os.Process) error {
	return p.Signal(syscall.SIGKILL)
}

// scenario43AbruptTermination reports whether err is the exit error a killed
// child produces.
func scenario43AbruptTermination(err error) bool {
	return strings.Contains(err.Error(), "signal: killed")
}
