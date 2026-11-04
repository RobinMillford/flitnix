//go:build !windows

package transfer

import "os"

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
