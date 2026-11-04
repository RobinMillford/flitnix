//go:build !windows

package recovery

import (
	"fmt"
	"os"
	"syscall"
)

// instanceLock is an advisory exclusive lock held for the lifetime of the
// process. The kernel releases it automatically when the process exits, so a
// crash cannot leave a stale lock behind.
type instanceLock struct{ f *os.File }

func acquireInstanceLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("recovery: open instance lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s: %v", ErrLocked, path, err)
	}
	return &instanceLock{f: f}, nil
}

func (l *instanceLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// syncDir flushes a directory entry so an atomic rename survives a crash.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
