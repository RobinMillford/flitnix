//go:build !linux && !windows && !darwin

package tray

type noopBackend struct{}

func newPlatformBackend(_ []byte) backend {
	return noopBackend{}
}

func (noopBackend) Start(_ State, _ []MenuItem, _ func(string)) bool { return false }
func (noopBackend) Update(_ State, _ []MenuItem)                     {}
func (noopBackend) Stop()                                            {}
