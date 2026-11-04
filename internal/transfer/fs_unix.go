//go:build !windows

package transfer

import (
	"os"
	"path/filepath"
	"syscall"
)

// sameFilesystem reports whether path1 and path2 reside on the same filesystem device.
func sameFilesystem(path1, path2 string) (bool, error) {
	resolved1 := existingPathOrParent(path1)
	resolved2 := existingPathOrParent(path2)

	fi1, err := os.Stat(resolved1)
	if err != nil {
		return false, err
	}
	fi2, err := os.Stat(resolved2)
	if err != nil {
		return false, err
	}

	stat1, ok1 := fi1.Sys().(*syscall.Stat_t)
	stat2, ok2 := fi2.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, nil
	}

	return stat1.Dev == stat2.Dev, nil
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
