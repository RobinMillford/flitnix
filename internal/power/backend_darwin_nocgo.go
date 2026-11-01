//go:build darwin && !cgo

package power

import "errors"

type darwinBackend struct{}

func newNativeBackend() (backend, error) {
	return newDarwinBackend()
}

func newDarwinBackend() (*darwinBackend, error) {
	return &darwinBackend{}, nil
}

func (b *darwinBackend) Supported() bool {
	return false
}

func (b *darwinBackend) Inhibit(reason string) error {
	return errors.New("power: sleep inhibition on macOS requires cgo")
}

func (b *darwinBackend) Uninhibit() error {
	return nil
}

func (b *darwinBackend) Close() error {
	return nil
}
