//go:build windows

package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizePathArgument_WindowsDriveLetterURI verifies that a file:// URI
// carrying a Windows drive letter resolves to the same filesystem path as the
// plain path. A drive letter cannot exist on other platforms, so this lives in a
// windows-only file; the platform-independent URI shapes are covered in
// path_norm_test.go.
func TestNormalizePathArgument_WindowsDriveLetterURI(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "drive letter 報告.txt")
	if err := os.WriteFile(f, []byte("drive"), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(f)
	if err != nil {
		t.Fatal(err)
	}

	slashPath := filepath.ToSlash(canonical)
	volume := filepath.VolumeName(canonical)
	if len(volume) != 2 || volume[1] != ':' {
		t.Skipf("temporary directory is not on a drive-letter volume: %q", canonical)
	}

	for _, tc := range []struct {
		name string
		raw  string
	}{
		// file:///C:/dir/file: the drive letter stays in the path component.
		{"drive letter in path", (&url.URL{
			Scheme: "file",
			Path:   "/" + strings.TrimPrefix(slashPath, "/"),
		}).String()},
		// file://C:/dir/file: the drive letter is the authority.
		{"drive letter in authority", (&url.URL{
			Scheme: "file",
			Host:   volume,
			Path:   slashPath[len(volume):],
		}).String()},
	} {
		if tc.name == "drive letter in path" && !strings.HasPrefix(tc.raw, "file:///") {
			t.Fatalf("%s: expected a file:/// URI, got %q", tc.name, tc.raw)
		}
		got, err := NormalizePathArgument(tc.raw, "")
		if err != nil {
			t.Fatalf("%s: unexpected error for %q: %v", tc.name, tc.raw, err)
		}
		if got != canonical {
			t.Fatalf("%s: expected %q, got %q (uri %q)", tc.name, canonical, got, tc.raw)
		}
	}
}
