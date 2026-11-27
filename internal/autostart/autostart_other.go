//go:build !linux && !windows && !darwin

package autostart

type unsupportedBackend struct{}

func newPlatformBackend(_ Config) backend {
	return unsupportedBackend{}
}

func (unsupportedBackend) Supported() bool          { return false }
func (unsupportedBackend) IsEnabled() (bool, error) { return false, nil }
func (unsupportedBackend) Enable() error            { return ErrUnsupported }
func (unsupportedBackend) Disable() error           { return nil }
