//go:build windows

package server

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	modkernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetVolumeInformationW = modkernel32.NewProc("GetVolumeInformationW")
)

const (
	fileReadOnlyVolume = 0x00080000
)

func checkOSImmutabilityPlatform(f *os.File, path string) (bool, io.Closer) {
	// 1. Check if the volume is a verified read-only volume or VSS shadow copy
	if isWindowsVolumeReadOnly(path) {
		return true, nil
	}

	// 2. Note: FILE_ATTRIBUTE_READONLY alone MUST NOT qualify a file for Tier 1.
	// It is owner-changeable and does not prevent concurrent writes.
	// We deliberately do not check or accept FILE_ATTRIBUTE_READONLY here.

	// 3. Mandatory Non-Shared Read Lock:
	// Open Win32 CreateFile with dwShareMode = FILE_SHARE_READ (omitting FILE_SHARE_WRITE and FILE_SHARE_DELETE).
	// If acquired, the Windows NT kernel enforces mandatory share locking, preventing any other process from
	// writing or deleting the file while this handle remains open.
	lock, err := openWindowsMandatoryReadLock(path)
	if err == nil && lock != nil {
		return true, lock
	}

	// When protection cannot be guaranteed, fall back to Tier 2.
	return false, nil
}

func isWindowsVolumeReadOnly(path string) bool {
	clean := filepath.Clean(path)
	// Check for VSS Volume Shadow Copy prefix
	if strings.HasPrefix(clean, `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy`) ||
		strings.HasPrefix(clean, `\??\GLOBALROOT\Device\HarddiskVolumeShadowCopy`) {
		return true
	}

	vol := filepath.VolumeName(path)
	if vol == "" {
		vol = path
	}
	if len(vol) > 0 && vol[len(vol)-1] != '\\' {
		vol += "\\"
	}
	volPath, err := syscall.UTF16PtrFromString(vol)
	if err != nil {
		return false
	}
	var flags uint32
	r1, _, _ := procGetVolumeInformationW.Call(
		uintptr(unsafe.Pointer(volPath)),
		0, 0, 0, 0,
		uintptr(unsafe.Pointer(&flags)),
		0, 0,
	)
	return r1 != 0 && (flags&fileReadOnlyVolume != 0)
}

type windowsLockCloser struct {
	handle syscall.Handle
}

func (w *windowsLockCloser) Close() error {
	if w.handle != syscall.InvalidHandle {
		err := syscall.CloseHandle(w.handle)
		w.handle = syscall.InvalidHandle
		return err
	}
	return nil
}

func openWindowsMandatoryReadLock(path string) (io.Closer, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// dwDesiredAccess = syscall.GENERIC_READ
	// dwShareMode = syscall.FILE_SHARE_READ (omits FILE_SHARE_WRITE and FILE_SHARE_DELETE)
	h, err := syscall.CreateFile(
		pathp,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	return &windowsLockCloser{handle: h}, nil
}
