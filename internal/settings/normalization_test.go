package settings

import (
	"path/filepath"
	"testing"
)

// TestValidateAndNormalizeKeepsRelativePathsRelative proves normalization uses
// the platform's own path rules rather than assuming POSIX semantics: a
// relative DownloadDir must stay relative, with redundant segments removed.
// Resolving it against the process working directory would silently relocate
// the receive folder.
func TestValidateAndNormalizeKeepsRelativePathsRelative(t *testing.T) {
	sep := string(filepath.Separator)
	norm := ValidateAndNormalize(Settings{DownloadDir: "  downloads" + sep + ".." + sep + "received  "})
	if want := "received"; norm.DownloadDir != want {
		t.Fatalf("relative downloadDir normalized to %q, want %q", norm.DownloadDir, want)
	}
	if filepath.IsAbs(norm.DownloadDir) {
		t.Fatalf("normalization made a relative path absolute: %q", norm.DownloadDir)
	}
}

// TestValidateAndNormalizeKeepsAbsolutePathsAbsolute proves the absolute form
// survives normalization and is cleaned with platform rules.
func TestValidateAndNormalizeKeepsAbsolutePathsAbsolute(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "received")
	norm := ValidateAndNormalize(Settings{DownloadDir: dir + string(filepath.Separator) + "."})
	if norm.DownloadDir != filepath.Clean(dir) {
		t.Fatalf("absolute downloadDir normalized to %q, want %q", norm.DownloadDir, filepath.Clean(dir))
	}
	if !filepath.IsAbs(norm.DownloadDir) {
		t.Fatalf("normalization dropped the absolute form: %q", norm.DownloadDir)
	}
}

// TestValidateAndNormalizeEmptyDirStaysEmpty proves an unset directory remains
// the empty sentinel that selects the platform default, rather than becoming
// "." from over-eager cleaning.
func TestValidateAndNormalizeEmptyDirStaysEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		norm := ValidateAndNormalize(Settings{DownloadDir: in})
		if norm.DownloadDir != "" {
			t.Fatalf("blank downloadDir %q normalized to %q, want the empty default sentinel", in, norm.DownloadDir)
		}
	}
}
