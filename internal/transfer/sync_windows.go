//go:build windows

package transfer

import (
	"os"
	"time"
)

// syncDir is a no-op on Windows because Win32 FlushFileBuffers is unsupported on directory handles.
func syncDir(dir string) error {
	return nil
}

// renameFileWithRetry renames oldpath to newpath, retrying on transient Windows file locks.
func renameFileWithRetry(oldpath, newpath string) error {
	var err error
	delays := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 160 * time.Millisecond}

	for _, delay := range delays {
		err = os.Rename(oldpath, newpath)
		if err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return os.Rename(oldpath, newpath)
}
