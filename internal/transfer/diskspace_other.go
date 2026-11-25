//go:build !unix && !windows

package transfer

func probeDiskSpacePlatform(_ string) DiskSpaceInfo {
	return DiskSpaceInfo{
		Available: false,
		Error:     "disk space inspection not supported on this platform",
	}
}

func isNoSpaceErrorPlatform(_ error) bool {
	return false
}
