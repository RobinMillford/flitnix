package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DiskSpaceInfo describes raw OS filesystem capacity metrics for a target
// receive directory or its nearest existing ancestor.
type DiskSpaceInfo struct {
	TargetDir    string `json:"targetDir"`
	ProbeDir     string `json:"probeDir"`
	TargetExists bool   `json:"targetExists"`
	Available    bool   `json:"available"`
	FilesystemID string `json:"filesystemId,omitempty"`
	TotalBytes   uint64 `json:"totalBytes"`
	FreeBytes    uint64 `json:"freeBytes"`
	UsedBytes    uint64 `json:"usedBytes"`
	Error        string `json:"error,omitempty"`
}

// InspectDiskSpace inspects the underlying filesystem capacity for targetDir.
// If targetDir does not exist yet (because Flitnix creates its receive folder
// lazily), InspectDiskSpace walks upward to the nearest existing ancestor
// directory. If the filesystem metrics are unavailable or unsupported on the
// host OS, Available is false and Error describes why without returning a fatal error.
func InspectDiskSpace(targetDir string) DiskSpaceInfo {
	if strings.TrimSpace(targetDir) == "" {
		return DiskSpaceInfo{
			Available: false,
			Error:     "empty target directory",
		}
	}
	abs, err := filepath.Abs(targetDir)
	if err != nil {
		return DiskSpaceInfo{
			TargetDir: targetDir,
			Available: false,
			Error:     err.Error(),
		}
	}

	probe, targetExists, err := resolveProbeDir(abs)
	if err != nil {
		return DiskSpaceInfo{
			TargetDir:    abs,
			TargetExists: targetExists,
			Available:    false,
			Error:        err.Error(),
		}
	}

	info := probeDiskSpacePlatform(probe)
	info.TargetDir = abs
	info.ProbeDir = probe
	info.TargetExists = targetExists
	return info
}

// resolveProbeDir finds targetAbs if it exists, or the nearest existing
// ancestor directory when targetAbs has not been created yet.
func resolveProbeDir(targetAbs string) (probe string, targetExists bool, err error) {
	if st, statErr := os.Stat(targetAbs); statErr == nil {
		if !st.IsDir() {
			return "", true, errors.New("target path is not a directory")
		}
		return targetAbs, true, nil
	} else if !os.IsNotExist(statErr) {
		return "", false, statErr
	}

	cur := filepath.Dir(targetAbs)
	for {
		st, statErr := os.Stat(cur)
		if statErr == nil {
			if st.IsDir() {
				return cur, false, nil
			}
			return "", false, errors.New("ancestor path is not a directory")
		}
		if !os.IsNotExist(statErr) {
			return "", false, statErr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false, statErr
		}
		cur = parent
	}
}

// IsNoSpaceError reports whether err is an OS out-of-disk-space error such as
// ENOSPC on Unix or ERROR_DISK_FULL on Windows.
func IsNoSpaceError(err error) bool {
	return isNoSpaceErrorPlatform(err)
}
