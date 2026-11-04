//go:build !linux && !windows && !darwin

package power

import "errors"

type otherBackend struct{}

func newNativeBackend() (backend, error) {
	return &otherBackend{}, nil
}

func (b *otherBackend) Supported() bool {
	return false
}

func (b *otherBackend) Inhibit(reason string) error {
	return errors.New("power: sleep inhibition not supported on this platform")
}

func (b *otherBackend) Uninhibit() error {
	return nil
}

func (b *otherBackend) Close() error {
	return nil
}
