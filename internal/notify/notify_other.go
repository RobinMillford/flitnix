//go:build !linux && !windows && !darwin

package notify

type noopBackend struct{}

func newPlatformBackend() backend               { return noopBackend{} }
func (noopBackend) Supported() bool             { return false }
func (noopBackend) Send(_ dispatchedNotification) error { return nil }
func (noopBackend) Close()                      {}
