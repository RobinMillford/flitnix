//go:build !linux && !windows && !darwin

package netenv

import "context"

func defaultNativeNotifier(ctx context.Context, notify chan<- struct{}) {
	// Fallback platform: no native OS socket used; periodic reconciliation handles detection.
	<-ctx.Done()
}
