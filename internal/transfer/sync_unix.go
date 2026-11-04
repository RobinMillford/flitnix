//go:build linux

package transfer

import "os"

// platformSupportsPipelinedCheckpoints reports whether the OS and filesystem
// support verified atomic metadata replacement with durable directory fsync.
// Linux POSIX rename(2) and directory fsync(2) are verified on ext4/xfs/btrfs.
func platformSupportsPipelinedCheckpoints() bool {
	return true
}

// syncDir flushes directory metadata changes to physical disk on Linux.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// renameFileWithRetry atomically renames oldpath to newpath on Linux.
func renameFileWithRetry(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}
