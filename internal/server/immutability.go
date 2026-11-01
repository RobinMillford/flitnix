package server

import (
	"io"
	"os"
)

// checkOSImmutabilityFn is the function used to check OS-enforced immutability.
// In tests, it can be replaced with a mock or test hook to simulate OS-enforced immutable storage.
var checkOSImmutabilityFn = checkOSImmutabilityPlatform

// isOSImmutable reports whether the opened file handle and path have an active,
// OS-enforced immutability protection mechanism. It returns whether the file is
// immutable and an optional lock handle (such as a Windows mandatory non-shared read lock)
// that must be maintained to guarantee representation identity across continuation requests.
func isOSImmutable(f *os.File, path string) (bool, io.Closer) {
	if f == nil || path == "" {
		return false, nil
	}
	return checkOSImmutabilityFn(f, path)
}

// EvaluateDarwinImmutability reports whether macOS filesystem properties qualify
// for Tier 1 Range resumption. UF_IMMUTABLE (chflags uchg) alone MUST NOT qualify
// because the file owner can clear it at any time without elevated privileges.
// Only OS-enforced read-only APFS snapshots or read-only volume mounts qualify.
func EvaluateDarwinImmutability(mntReadOnly bool, ufImmutable bool) bool {
	if mntReadOnly {
		return true
	}
	// ufImmutable alone is rejected as owner-mutable
	return false
}

// EvaluateWindowsImmutability reports whether Windows file and volume attributes
// qualify for Tier 1 Range resumption. FILE_ATTRIBUTE_READONLY alone MUST NOT qualify
// because any user process can modify or clear it. Only verified read-only volumes
// (or VSS shadow copies) or mandatory non-shared read locks qualify.
func EvaluateWindowsImmutability(volumeReadOnly bool, fileAttributeReadOnly bool, hasMandatoryLock bool) bool {
	if volumeReadOnly || hasMandatoryLock {
		return true
	}
	// fileAttributeReadOnly alone is rejected as owner-changeable
	return false
}
