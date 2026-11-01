//go:build darwin

package server

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func checkOSImmutabilityPlatform(f *os.File, path string) (bool, io.Closer) {
	var statfs unix.Statfs_t
	if err := unix.Statfs(path, &statfs); err == nil {
		// Only accept the approved read-only APFS snapshot or OS-enforced read-only volume mechanism.
		if statfs.Flags&unix.MNT_RDONLY != 0 {
			return true, nil
		}
	}
	// UF_IMMUTABLE (chflags uchg) alone MUST NOT qualify:
	// on macOS, the file owner can clear uchg at any time without privileges.
	// When protection cannot be guaranteed, use Tier 2.
	return false, nil
}
