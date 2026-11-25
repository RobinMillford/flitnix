//go:build windows

package transfer

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	winErrHandleDiskFull syscall.Errno = 39
	winErrDiskFull       syscall.Errno = 112
)

func probeDiskSpacePlatform(probeDir string) DiskSpaceInfo {
	ptr, err := windows.UTF16PtrFromString(probeDir)
	if err != nil {
		return DiskSpaceInfo{
			Available: false,
			Error:     err.Error(),
		}
	}

	var freeAvail, total, freeTotal uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &freeAvail, &total, &freeTotal); err != nil {
		return DiskSpaceInfo{
			Available: false,
			Error:     err.Error(),
		}
	}
	if total == 0 && freeTotal == 0 && freeAvail == 0 {
		return DiskSpaceInfo{
			Available: false,
			Error:     "filesystem capacity metrics unavailable",
		}
	}

	var used uint64
	if total >= freeTotal {
		used = total - freeTotal
	}

	vol := strings.ToUpper(filepath.VolumeName(probeDir))
	if vol != "" {
		vol = "vol:" + vol
	} else {
		vol = "path:" + strings.ToLower(probeDir)
	}
	_ = unsafe.Sizeof(freeAvail) // keep unsafe imported cleanly if needed for extensions

	return DiskSpaceInfo{
		Available:    true,
		FilesystemID: vol,
		TotalBytes:   total,
		FreeBytes:    freeAvail,
		UsedBytes:    used,
	}
}

func isNoSpaceErrorPlatform(err error) bool {
	return errors.Is(err, winErrDiskFull) || errors.Is(err, winErrHandleDiskFull) || errors.Is(err, syscall.ENOSPC)
}
