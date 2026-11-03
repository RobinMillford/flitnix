//go:build !unix && !windows

package main

import "time"

// The Phase 2 harness supports Linux, Windows and macOS. On any other target
// there is no portable process-CPU-time API available in this build, so the
// metric is reported as unavailable (zero) instead of fabricated, matching
// the benchmarks/wp4 proc_other.go convention. Latency, throughput and
// integrity measurements are unaffected.
func processCPUTime() time.Duration { return 0 }
