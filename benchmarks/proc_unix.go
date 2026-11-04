//go:build unix

package main

import (
	"syscall"
	"time"
)

// processCPUTime returns cumulative user+system CPU time consumed by this
// process, used as the delta across a single benchmark run. Semantics are
// identical to the historical implementation: getrusage(RUSAGE_SELF) with
// user+stime summed, zero only if the kernel call itself fails.
//
// Split into per-OS files (see proc_windows.go / proc_other.go) because
// syscall.Getrusage does not exist on Windows, which previously broke
// `go vet ./...` in the Windows CI job. Same idiom as benchmarks/wp4.
func processCPUTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
		sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
		return user + sys
	}
	return 0
}
