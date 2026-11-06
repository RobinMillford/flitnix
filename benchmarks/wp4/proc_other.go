//go:build !unix && historical_wp4

package main

import "time"

// Windows and other non-Unix targets have no syscall.Rusage with Unix field
// names, so these report zero instead of breaking the cross-compile. The
// harness's portable per-run sampling (sampleVmRSSMB) reads /proc and likewise
// degrades to zero off Linux; throughput, latency and integrity measurements
// are unaffected because they never depend on these two helpers.
func getCPUTime() time.Duration { return 0 }

func getRSSMB() float64 { return 0 }
