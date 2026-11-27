//go:build !race

package main

import "time"

// peerWait keeps the original deadlines for non-race runs so genuine failures
// are reported quickly.
func peerWait(d time.Duration) time.Duration {
	return d
}
