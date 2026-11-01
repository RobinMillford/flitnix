//go:build !windows

package transfer

import (
	"syscall"
)

// getPlatformFreeSpace returns the free disk bytes available to unprivileged users.
func getPlatformFreeSpace(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	// stat.Bavail is free blocks available to unprivileged user
	// stat.Bsize is block size
	return stat.Bavail * uint64(stat.Bsize), nil
}
