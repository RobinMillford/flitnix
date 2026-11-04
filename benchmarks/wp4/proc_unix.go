//go:build unix

package main

import (
	"runtime"
	"syscall"
	"time"
)

// getCPUTime returns cumulative user+system CPU time consumed by this process.
func getCPUTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
	sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
	return user + sys
}

// getRSSMB returns the kernel lifetime peak RSS in MiB (rusage Maxrss).
//
// Maxrss is reported in KiB on Linux but in bytes on Darwin, so the divisor is
// platform-dependent. Note this value is the peak over the whole process
// lifetime and is therefore identical for every run of a single-process
// harness; sampleVmRSSMB provides the per-run sample.
func getRSSMB() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return float64(ru.Maxrss) / (1024 * 1024)
	}
	return float64(ru.Maxrss) / 1024
}
