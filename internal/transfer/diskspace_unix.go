//go:build unix

package transfer

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func probeDiskSpacePlatform(probeDir string) DiskSpaceInfo {
	var st unix.Statfs_t
	if err := unix.Statfs(probeDir, &st); err != nil {
		return DiskSpaceInfo{
			Available: false,
			Error:     err.Error(),
		}
	}
	if st.Bsize <= 0 {
		return DiskSpaceInfo{
			Available: false,
			Error:     "filesystem reported invalid block size",
		}
	}

	// On Linux, FreeBSD, and macOS, unix.Statfs_t exposes fundamental block size
	// via Bsize (int64 on Linux, int32 on Darwin). Virtual filesystems that
	// report 0 total and 0 free blocks do not provide reliable quota data.
	bsize := uint64(st.Bsize)
	if st.Blocks == 0 && st.Bfree == 0 && st.Bavail == 0 {
		return DiskSpaceInfo{
			Available: false,
			Error:     "filesystem capacity metrics unavailable",
		}
	}

	total := st.Blocks * bsize
	freeAvail := st.Bavail * bsize
	freeAll := st.Bfree * bsize
	var used uint64
	if total >= freeAll {
		used = total - freeAll
	}

	var fsID string
	if fi, err := os.Stat(probeDir); err == nil {
		if sysStat, ok := fi.Sys().(*syscall.Stat_t); ok && sysStat != nil {
			fsID = fmt.Sprintf("dev:%d", sysStat.Dev)
		}
	}

	return DiskSpaceInfo{
		Available:    true,
		FilesystemID: fsID,
		TotalBytes:   total,
		FreeBytes:    freeAvail,
		UsedBytes:    used,
	}
}

func isNoSpaceErrorPlatform(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
