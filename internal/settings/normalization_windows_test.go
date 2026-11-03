//go:build windows

package settings

import (
	"path/filepath"
	"testing"
)

// TestValidateAndNormalizeWindowsDriveLetter proves a drive-letter path is
// cleaned with Windows rules: mixed separators collapse to backslashes and
// parent segments are resolved without losing the volume.
func TestValidateAndNormalizeWindowsDriveLetter(t *testing.T) {
	norm := ValidateAndNormalize(Settings{DownloadDir: `C:/Users/Example/Downloads/../Received/`})
	const want = `C:\Users\Example\Received`
	if norm.DownloadDir != want {
		t.Fatalf("drive-letter downloadDir normalized to %q, want %q", norm.DownloadDir, want)
	}
}

// TestValidateAndNormalizeWindowsUNC proves a UNC share path keeps its volume
// and server components.
func TestValidateAndNormalizeWindowsUNC(t *testing.T) {
	norm := ValidateAndNormalize(Settings{DownloadDir: `\\server\share\dir\..\received`})
	const want = `\\server\share\received`
	if norm.DownloadDir != want {
		t.Fatalf("UNC downloadDir normalized to %q, want %q", norm.DownloadDir, want)
	}
}

// TestValidateAndNormalizeWindowsRelative proves a relative Windows path stays
// relative and is cleaned with backslash semantics.
func TestValidateAndNormalizeWindowsRelative(t *testing.T) {
	norm := ValidateAndNormalize(Settings{DownloadDir: `Received\..\Inbox`})
	if norm.DownloadDir != `Inbox` {
		t.Fatalf("relative downloadDir normalized to %q, want %q", norm.DownloadDir, `Inbox`)
	}
	if filepath.IsAbs(norm.DownloadDir) {
		t.Fatalf("normalization made a relative path absolute: %q", norm.DownloadDir)
	}
}
