package transfer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRegistryAddFolderEnumeratesPrivateRelativeMetadata(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Photos")
	if err := os.MkdirAll(filepath.Join(folder, "2026", "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "cover.jpg"), []byte("cover"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "2026", "raw", "image.dng"), []byte("raw"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(folder, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}

	source, err := NewRegistry().AddFolder(folder, "session-key")
	if err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	if source.Kind != SourceKindFolder || source.RootName != "Photos" || source.Path == "" {
		t.Fatalf("unexpected source: %+v", source)
	}
	if len(source.Items) != 2 || source.Items[0].RelativePath != "2026/raw/image.dng" ||
		source.Items[1].RelativePath != "cover.jpg" {
		t.Fatalf("unexpected ordered items: %+v", source.Items)
	}
	if len(source.EmptyDirs) != 1 || source.EmptyDirs[0] != "empty" {
		t.Fatalf("empty directories = %v, want [empty]", source.EmptyDirs)
	}
	public, err := json.Marshal(sourceView(source))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), folder) || strings.Contains(string(public), "session-key") {
		t.Fatalf("public source view disclosed private metadata: %s", public)
	}
}

func TestRegistryAddFolderSkipsSymlinksAndSpecialFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture requires platform permissions on Windows")
	}
	root := t.TempDir()
	folder := filepath.Join(root, "root")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "ok.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(folder, "ok.txt"), filepath.Join(folder, "link.txt")); err != nil {
		t.Fatal(err)
	}
	source, err := NewRegistry().AddFolder(folder, "")
	if err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	if len(source.Items) != 1 || source.Items[0].RelativePath != "ok.txt" {
		t.Fatalf("symlink was not skipped: %+v", source.Items)
	}
}

func TestRegistryFileSourceDeduplicatesWithoutChangingLegacyAdd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.txt")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	first, err := registry.AddFileSource(path, "one.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.AddFileSource(path, "one.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || len(registry.ListSources()) != 1 {
		t.Fatalf("duplicate source registration: first=%+v second=%+v", first, second)
	}
	legacy, err := registry.Add(path, "one.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ID == first.ID {
		t.Fatal("legacy Add unexpectedly adopted source deduplication")
	}
}

func TestRegistryFolderSelectionsHaveIndependentIdentities(t *testing.T) {
	parent := t.TempDir()
	firstRoot := filepath.Join(parent, "one", "Same")
	secondRoot := filepath.Join(parent, "two", "Same")
	for _, root := range []string{firstRoot, secondRoot} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(root), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	registry := NewRegistry()
	first, err := registry.AddFolder(firstRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.AddFolder(secondRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.RootName != second.RootName {
		t.Fatalf("folder identities were conflated: first=%+v second=%+v", first, second)
	}
	if !registry.RemoveSource(first.ID) {
		t.Fatal("first folder was not removable")
	}
	sources := registry.ListSources()
	if len(sources) != 1 || sources[0].ID != second.ID {
		t.Fatalf("removing one folder changed unrelated sources: %+v", sources)
	}
}
