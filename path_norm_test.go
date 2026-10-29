package main

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizePathArgument_Absolute(t *testing.T) {
	tmpDir := t.TempDir()
	f := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(f, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Regular file
	got, err := NormalizePathArgument(f, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected, _ := filepath.EvalSymlinks(f)
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}

	// Directory
	gotDir, err := NormalizePathArgument(tmpDir, "")
	if err != nil {
		t.Fatalf("unexpected error for directory: %v", err)
	}
	expectedDir, _ := filepath.EvalSymlinks(tmpDir)
	if gotDir != expectedDir {
		t.Fatalf("expected dir %q, got %q", expectedDir, gotDir)
	}
}

func TestNormalizePathArgument_Relative(t *testing.T) {
	tmpDir := t.TempDir()
	relName := "relative_sample.txt"
	absTarget := filepath.Join(tmpDir, relName)
	if err := os.WriteFile(absTarget, []byte("rel"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Resolve with explicit workingDir
	got, err := NormalizePathArgument(relName, tmpDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected, _ := filepath.EvalSymlinks(absTarget)
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}

	// Resolve with empty workingDir (falls back to process cwd)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cwdFile := filepath.Join(cwd, "main.go")
	if _, statErr := os.Stat(cwdFile); statErr == nil {
		gotCwd, err := NormalizePathArgument("main.go", "")
		if err != nil {
			t.Fatalf("unexpected error for cwd file: %v", err)
		}
		expectedCwd, _ := filepath.EvalSymlinks(cwdFile)
		if gotCwd != expectedCwd {
			t.Fatalf("expected %q, got %q", expectedCwd, gotCwd)
		}
	}
}

func TestNormalizePathArgument_SpacesAndUnicode(t *testing.T) {
	tmpDir := t.TempDir()

	// Spaces
	spaceFile := filepath.Join(tmpDir, "my sample file with spaces.txt")
	if err := os.WriteFile(spaceFile, []byte("spaces"), 0o644); err != nil {
		t.Fatal(err)
	}
	gotSpace, err := NormalizePathArgument(spaceFile, "")
	if err != nil {
		t.Fatalf("unexpected error for space file: %v", err)
	}
	expSpace, _ := filepath.EvalSymlinks(spaceFile)
	if gotSpace != expSpace {
		t.Fatalf("expected %q, got %q", expSpace, gotSpace)
	}

	// Unicode
	unicodeFile := filepath.Join(tmpDir, "日本語_報告書_2026.pdf")
	if err := os.WriteFile(unicodeFile, []byte("unicode"), 0o644); err != nil {
		t.Fatal(err)
	}
	gotUnicode, err := NormalizePathArgument(unicodeFile, "")
	if err != nil {
		t.Fatalf("unexpected error for unicode file: %v", err)
	}
	expUnicode, _ := filepath.EvalSymlinks(unicodeFile)
	if gotUnicode != expUnicode {
		t.Fatalf("expected %q, got %q", expUnicode, gotUnicode)
	}
}

func TestNormalizePathArgument_FileURI(t *testing.T) {
	tmpDir := t.TempDir()
	f := filepath.Join(tmpDir, "uri_file.txt")
	if err := os.WriteFile(f, []byte("uri"), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(f)

	// Standard file:///path
	standardURI := "file://" + filepath.ToSlash(f)
	got, err := NormalizePathArgument(standardURI, "")
	if err != nil {
		t.Fatalf("unexpected error for standard URI: %v", err)
	}
	if got != canonical {
		t.Fatalf("expected %q, got %q", canonical, got)
	}

	// file://localhost/path
	localhostURI := "file://localhost" + filepath.ToSlash(f)
	gotLH, err := NormalizePathArgument(localhostURI, "")
	if err != nil {
		t.Fatalf("unexpected error for localhost URI: %v", err)
	}
	if gotLH != canonical {
		t.Fatalf("expected %q, got %q", canonical, gotLH)
	}

	// Percent-encoded URI
	spaceFile := filepath.Join(tmpDir, "space and unicode 報告.txt")
	if err := os.WriteFile(spaceFile, []byte("space-uni"), 0o644); err != nil {
		t.Fatal(err)
	}
	expSpace, _ := filepath.EvalSymlinks(spaceFile)
	encodedURI := "file://" + (&url.URL{Path: filepath.ToSlash(spaceFile)}).String()
	gotEncoded, err := NormalizePathArgument(encodedURI, "")
	if err != nil {
		t.Fatalf("unexpected error for encoded URI: %v", err)
	}
	if gotEncoded != expSpace {
		t.Fatalf("expected %q, got %q", expSpace, gotEncoded)
	}

	// Remote authority rejection
	remoteURI := "file://remote-nas.lan/share/file.txt"
	_, errRemote := NormalizePathArgument(remoteURI, "")
	if !errors.Is(errRemote, ErrRemoteAuthority) {
		t.Fatalf("expected ErrRemoteAuthority, got: %v", errRemote)
	}

	// Unsupported scheme rejection
	httpURI := "http://example.com/file.txt"
	_, errHTTP := NormalizePathArgument(httpURI, "")
	if !errors.Is(errHTTP, ErrUnsupportedScheme) {
		t.Fatalf("expected ErrUnsupportedScheme, got: %v", errHTTP)
	}

	// Malformed URI rejection
	malformedURI := "file://%ZZ/bad"
	_, errMalformed := NormalizePathArgument(malformedURI, "")
	if errMalformed == nil {
		t.Fatalf("expected error for malformed percent encoding")
	}
}

func TestNormalizePathArgument_Symlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Top-level symlink to regular file
	targetFile := filepath.Join(tmpDir, "real_target.txt")
	if err := os.WriteFile(targetFile, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkFile := filepath.Join(tmpDir, "symlink_file.txt")
	if err := os.Symlink(targetFile, symlinkFile); err != nil {
		t.Skipf("symlinks not supported on this platform/environment: %v", err)
	}

	gotSymFile, err := NormalizePathArgument(symlinkFile, "")
	if err != nil {
		t.Fatalf("unexpected error for symlinked file: %v", err)
	}
	expTarget, _ := filepath.EvalSymlinks(targetFile)
	if gotSymFile != expTarget {
		t.Fatalf("expected canonical target %q, got %q", expTarget, gotSymFile)
	}

	// 2. Top-level symlink to directory
	targetDir := filepath.Join(tmpDir, "real_dir")
	if err := os.Mkdir(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkDir := filepath.Join(tmpDir, "symlink_dir")
	if err := os.Symlink(targetDir, symlinkDir); err != nil {
		t.Fatal(err)
	}

	gotSymDir, err := NormalizePathArgument(symlinkDir, "")
	if err != nil {
		t.Fatalf("unexpected error for symlinked dir: %v", err)
	}
	expTargetDir, _ := filepath.EvalSymlinks(targetDir)
	if gotSymDir != expTargetDir {
		t.Fatalf("expected canonical dir %q, got %q", expTargetDir, gotSymDir)
	}

	// 3. Broken symlink
	brokenSymlink := filepath.Join(tmpDir, "broken_link")
	_ = os.Symlink(filepath.Join(tmpDir, "missing_dest.txt"), brokenSymlink)
	_, errBroken := NormalizePathArgument(brokenSymlink, "")
	if errBroken == nil {
		t.Fatalf("expected error for broken symlink, got nil")
	}
}

func TestNormalizePathArgument_InaccessibleAndSpecial(t *testing.T) {
	// Empty string
	_, errEmpty := NormalizePathArgument("", "")
	if !errors.Is(errEmpty, ErrEmptyPath) {
		t.Fatalf("expected ErrEmptyPath, got: %v", errEmpty)
	}

	// Non-existent file
	_, errNonExistent := NormalizePathArgument("/path/does/not/exist/xyz123.bin", "")
	if errNonExistent == nil {
		t.Fatalf("expected error for non-existent file, got nil")
	}
}

func TestExtractSharePaths_DeduplicationAndFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	f1 := filepath.Join(tmpDir, "f1.txt")
	f2 := filepath.Join(tmpDir, "f2.txt")
	subDir := filepath.Join(tmpDir, "sub_folder")
	_ = os.WriteFile(f1, []byte("1"), 0o644)
	_ = os.WriteFile(f2, []byte("2"), 0o644)
	_ = os.Mkdir(subDir, 0o755)

	canonicalF1, _ := filepath.EvalSymlinks(f1)
	canonicalF2, _ := filepath.EvalSymlinks(f2)
	canonicalSub, _ := filepath.EvalSymlinks(subDir)

	var diagnostics []string
	diagCallback := func(raw string, err error) {
		diagnostics = append(diagnostics, raw)
	}

	args := []string{
		"-flag",
		"--verbose",
		"send",   // subcommand should be filtered
		"doctor", // subcommand should be filtered
		f1,
		f2,
		f1, // duplicate of f1
		subDir,
		subDir, // duplicate of subDir
		filepath.Join(tmpDir, "missing_file.txt"), // invalid path
	}

	results := ExtractSharePaths(args, "", diagCallback)

	// Should contain exactly f1, f2, subDir in order
	if len(results) != 3 {
		t.Fatalf("expected 3 unique items, got %d: %v", len(results), results)
	}
	if results[0] != canonicalF1 || results[1] != canonicalF2 || results[2] != canonicalSub {
		t.Fatalf("unexpected results ordering: %+v", results)
	}

	// Missing file reported in diagnostic callback
	if len(diagnostics) != 1 || diagnostics[0] != filepath.Join(tmpDir, "missing_file.txt") {
		t.Fatalf("expected 1 diagnostic for missing file, got %v", diagnostics)
	}
}
