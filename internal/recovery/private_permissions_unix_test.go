//go:build !windows

package recovery

import (
	"os"
	"testing"
)

func assertPrivatePath(t *testing.T, _ string, info os.FileInfo) {
	t.Helper()
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("installation key mode = %o, want 600", mode)
	}
}
