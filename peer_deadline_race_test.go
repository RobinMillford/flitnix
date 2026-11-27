//go:build race

package main

import "time"

// peerWait widens wall-clock deadlines under the race detector. The Linux CI
// job runs `go test -race ./...`, so this integration test competes with the
// benchmarks package for CPU while every TLS handshake and approval poll is
// instrumented. A CI run stalled past the 30s folder-transfer deadline even
// though a normal run finishes in about a second.
func peerWait(d time.Duration) time.Duration {
	return 4 * d
}
