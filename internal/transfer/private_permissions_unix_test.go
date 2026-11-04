//go:build !windows

package transfer

import (
	"os"
	"testing"
)

func assertPrivatePath(t *testing.T, _ string, info os.FileInfo) {
	t.Helper()
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("directory permissions %v allow group/others access", perm)
	}
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	if perm := filePerms(t, path); perm&0o077 != 0 {
		t.Errorf("file permissions %v allow group/others access", perm)
	}
}
