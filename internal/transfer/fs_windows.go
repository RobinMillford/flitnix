//go:build windows

package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procGetVolumeInformationW = kernel32.NewProc("GetVolumeInformationW")
)

// sameFilesystem reports whether path1 and path2 reside on the same Windows volume/filesystem.
func sameFilesystem(path1, path2 string) (bool, error) {
	resolved1 := existingPathOrParent(path1)
	resolved2 := existingPathOrParent(path2)

	vol1 := filepath.VolumeName(resolved1)
	vol2 := filepath.VolumeName(resolved2)

	if vol1 != "" && vol2 != "" && !strings.EqualFold(vol1, vol2) {
		return false, nil
	}

	serial1, err1 := getVolumeSerialNumber(vol1, resolved1)
	serial2, err2 := getVolumeSerialNumber(vol2, resolved2)
	if err1 == nil && err2 == nil && serial1 != 0 && serial2 != 0 {
		return serial1 == serial2, nil
	}

	if vol1 != "" && vol2 != "" {
		return strings.EqualFold(vol1, vol2), nil
	}

	return true, nil
}

func getVolumeSerialNumber(vol, path string) (uint32, error) {
	root := vol
	if root == "" {
		root = filepath.VolumeName(path)
	}
	if root == "" {
		root = path
	}
	if len(root) > 0 && root[len(root)-1] != '\\' {
		root += "\\"
	}
	rootPtr, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return 0, err
	}
	var serial uint32
	r1, _, errSys := procGetVolumeInformationW.Call(
		uintptr(unsafe.Pointer(rootPtr)),
		0, 0,
		uintptr(unsafe.Pointer(&serial)),
		0, 0, 0, 0,
	)
	if r1 == 0 {
		return 0, errSys
	}
	return serial, nil
}

// existingPathOrParent ascends until an existing filesystem path is found.
func existingPathOrParent(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	curr := abs
	for {
		if _, err := os.Stat(curr); err == nil {
			return curr
		}
		parent := filepath.Dir(curr)
		if parent == curr || parent == "." {
			return curr
		}
		curr = parent
	}
}
