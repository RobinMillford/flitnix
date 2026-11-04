//go:build windows

package transfer

import (
	"syscall"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// getPlatformFreeSpace returns the free disk bytes available to unprivileged users on Windows.
func getPlatformFreeSpace(dir string) (uint64, error) {
	dirPtr, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}

	var freeBytesAvailable uint64
	var totalNumberOfBytes uint64
	var totalNumberOfFreeBytes uint64

	r1, _, errSys := getDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(dirPtr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalNumberOfBytes)),
		uintptr(unsafe.Pointer(&totalNumberOfFreeBytes)),
	)
	if r1 == 0 {
		return 0, errSys
	}
	return freeBytesAvailable, nil
}
