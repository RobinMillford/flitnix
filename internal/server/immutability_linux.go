//go:build linux

package server

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// fsImmutableFL is FS_IMMUTABLE_FL from <linux/fs.h> (0x00000010).
const fsImmutableFL = 0x00000010

func checkOSImmutabilityPlatform(f *os.File, path string) (bool, io.Closer) {
	// 1. Check for FS_IMMUTABLE_FL (e.g. set via chattr +i)
	if f != nil {
		flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
		if err == nil && (flags&fsImmutableFL != 0) {
			return true, nil
		}
	}

	// 2. Check if the file resides on an OS-enforced read-only mount
	var statfs unix.Statfs_t
	if err := unix.Statfs(path, &statfs); err == nil {
		if statfs.Flags&unix.ST_RDONLY != 0 {
			return true, nil
		}
	}

	return false, nil
}
