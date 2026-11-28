//go:build !linux && !darwin && !windows

package clipboard

import "context"

type unsupportedBackend struct{}

func newPlatformBackend() Backend {
	return &unsupportedBackend{}
}

func (b *unsupportedBackend) Capabilities() Capabilities {
	return Capabilities{
		Supported:         false,
		BackendName:       "unsupported",
		UnsupportedReason: "OS clipboard monitoring is not supported on this platform",
	}
}

func (b *unsupportedBackend) Read(ctx context.Context) (Snapshot, error) {
	return Snapshot{}, ErrDisabled
}

func (b *unsupportedBackend) WriteText(ctx context.Context, text string) error {
	return ErrDisabled
}
