//go:build windows

package recovery

import (
	"fmt"
	"syscall"
)

// instanceLock is an exclusive (share-mode zero) handle on the state
// directory's lock file. Windows refuses any other open of the path while it
// is held, and the kernel closes the handle when the process exits, so a crash
// cannot leave a stale lock behind.
type instanceLock struct{ h syscall.Handle }

func acquireInstanceLock(path string) (*instanceLock, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("recovery: encode instance lock path: %w", err)
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing: exclusive ownership
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrLocked, path, err)
	}
	return &instanceLock{h: h}, nil
}

func (l *instanceLock) release() error {
	if l == nil || l.h == 0 {
		return nil
	}
	err := syscall.CloseHandle(l.h)
	l.h = 0
	return err
}

// syncDir is a no-op on Windows: FlushFileBuffers requires a file handle and
// the platform does not expose a portable directory handle. Atomic replace is
// still provided by os.Rename over the same volume.
func syncDir(string) error { return nil }
