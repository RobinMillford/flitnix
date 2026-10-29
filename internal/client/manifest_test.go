package client

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestBuildManifest(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test file hierarchy:
	// tmpDir/
	//   file1.txt (5 bytes)
	//   file2.txt (10 bytes)
	//   sub/
	//     nested.bin (20 bytes)
	//   emptyDir/
	file1 := filepath.Join(tmpDir, "file1.txt")
	file2 := filepath.Join(tmpDir, "file2.txt")
	subDir := filepath.Join(tmpDir, "sub")
	nested := filepath.Join(subDir, "nested.bin")
	emptyDir := filepath.Join(tmpDir, "emptyDir")

	if err := os.WriteFile(file1, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file2, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, make([]byte, 20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("single file", func(t *testing.T) {
		lm, err := BuildManifest([]string{file1}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lm.Manifest.Kind != transfer.BatchKindFiles {
			t.Errorf("Kind = %s, want %s", lm.Manifest.Kind, transfer.BatchKindFiles)
		}
		if len(lm.Manifest.Items) != 1 {
			t.Fatalf("len(Items) = %d, want 1", len(lm.Manifest.Items))
		}
		if lm.Manifest.Items[0].DisplayName != "file1.txt" {
			t.Errorf("DisplayName = %s, want file1.txt", lm.Manifest.Items[0].DisplayName)
		}
		if lm.TotalBytes != 5 {
			t.Errorf("TotalBytes = %d, want 5", lm.TotalBytes)
		}
	})

	t.Run("multiple files", func(t *testing.T) {
		lm, err := BuildManifest([]string{file1, file2}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lm.Manifest.Kind != transfer.BatchKindFiles {
			t.Errorf("Kind = %s, want %s", lm.Manifest.Kind, transfer.BatchKindFiles)
		}
		if len(lm.Manifest.Items) != 2 {
			t.Fatalf("len(Items) = %d, want 2", len(lm.Manifest.Items))
		}
		if lm.TotalBytes != 15 {
			t.Errorf("TotalBytes = %d, want 15", lm.TotalBytes)
		}
	})

	t.Run("directory hierarchy", func(t *testing.T) {
		lm, err := BuildManifest([]string{tmpDir}, "my-project")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lm.Manifest.Kind != transfer.BatchKindFolder {
			t.Errorf("Kind = %s, want %s", lm.Manifest.Kind, transfer.BatchKindFolder)
		}
		if lm.Manifest.RootName != "my-project" {
			t.Errorf("RootName = %s, want my-project", lm.Manifest.RootName)
		}
		if len(lm.Manifest.Items) != 3 {
			t.Fatalf("len(Items) = %d, want 3", len(lm.Manifest.Items))
		}
		if lm.TotalBytes != 35 {
			t.Errorf("TotalBytes = %d, want 35", lm.TotalBytes)
		}
	})

	t.Run("empty directory fails", func(t *testing.T) {
		_, err := BuildManifest([]string{emptyDir}, "")
		if !errors.Is(err, ErrEmptyDirectory) {
			t.Fatalf("expected ErrEmptyDirectory, got %v", err)
		}
	})

	t.Run("nonexistent path fails", func(t *testing.T) {
		_, err := BuildManifest([]string{filepath.Join(tmpDir, "does-not-exist")}, "")
		if !errors.Is(err, ErrPathNotFound) {
			t.Fatalf("expected ErrPathNotFound, got %v", err)
		}
	})

	t.Run("mixed directory and file fails", func(t *testing.T) {
		_, err := BuildManifest([]string{file1, subDir}, "")
		if !errors.Is(err, ErrMixedDirectory) {
			t.Fatalf("expected ErrMixedDirectory, got %v", err)
		}
	})

	t.Run("broken symlink fails", func(t *testing.T) {
		brokenSym := filepath.Join(tmpDir, "broken.link")
		_ = os.Symlink(filepath.Join(tmpDir, "nonexistent-target"), brokenSym)
		defer os.Remove(brokenSym)

		_, err := BuildManifest([]string{brokenSym}, "")
		if !errors.Is(err, ErrPathNotFound) && !errors.Is(err, ErrBrokenSymlink) {
			t.Fatalf("expected ErrPathNotFound or ErrBrokenSymlink, got %v", err)
		}
	})

	t.Run("symlink escaping directory tree fails", func(t *testing.T) {
		outsideDir := t.TempDir()
		escapeSym := filepath.Join(tmpDir, "outside_link")
		_ = os.Symlink(outsideDir, escapeSym)
		defer os.Remove(escapeSym)

		_, err := BuildManifest([]string{tmpDir}, "")
		if !errors.Is(err, ErrSymlinkEscape) {
			t.Fatalf("expected ErrSymlinkEscape, got %v", err)
		}
	})

	t.Run("empty input fails", func(t *testing.T) {
		_, err := BuildManifest(nil, "")
		if !errors.Is(err, ErrEmptyInput) {
			t.Fatalf("expected ErrEmptyInput, got %v", err)
		}
	})
}
