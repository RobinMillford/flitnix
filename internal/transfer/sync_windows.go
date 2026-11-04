//go:build windows

package transfer

import (
	"os"
	"time"
)

// platformSupportsPipelinedCheckpoints reports whether the OS and filesystem
// support verified atomic metadata replacement with durable directory fsync.
// Windows NTFS does not support directory FlushFileBuffers and MoveFileEx has
// non-POSIX replacement semantics, so Windows retains the safe synchronous path.
func platformSupportsPipelinedCheckpoints() bool {
	return false
}

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
