//go:build windows

package main

import (
	"time"

	"golang.org/x/sys/windows"
)

// processCPUTime returns cumulative user+kernel CPU time consumed by this
// process, the native Windows equivalent of getrusage(RUSAGE_SELF) on Unix.
// GetProcessTimes on the pseudo current-process handle is reliable for the
// lifetime of the process, so the metric is genuinely measured on Windows
// rather than reported as unavailable. A failed kernel call reports zero.
func processCPUTime() time.Duration {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &creation, &exit, &kernel, &user); err != nil {
		return 0
	}
	return filetimeDuration(&kernel) + filetimeDuration(&user)
}

// filetimeDuration interprets a Filetime that carries a time *amount*
// (GetProcessTimes kernel/user fields) as a time.Duration. The value is in
// 100-nanosecond ticks.
func filetimeDuration(ft *windows.Filetime) time.Duration {
	ticks := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
	return time.Duration(ticks) * 100
}
