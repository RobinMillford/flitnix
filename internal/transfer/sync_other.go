//go:build !linux && !windows

package transfer

import "os"

// platformSupportsPipelinedCheckpoints reports whether the OS and filesystem
// support verified atomic metadata replacement with durable directory fsync.
// Non-Linux platforms (such as macOS/Darwin) require F_FULLFSYNC and cannot be
// verified natively in this environment, so they retain the safe synchronous path.
func platformSupportsPipelinedCheckpoints() bool {
	return false
}

// syncDir flushes directory metadata changes to physical disk on POSIX systems.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// renameFileWithRetry atomically renames oldpath to newpath on POSIX systems.
func renameFileWithRetry(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}
