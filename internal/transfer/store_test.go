package transfer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(StoreConfig{Dir: filepath.Join(t.TempDir(), "receive")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st
}

func mustOpenPart(t *testing.T, st *Store, name string) *PartFile {
	t.Helper()
	p, err := st.OpenPart(name)
	if err != nil {
		t.Fatalf("OpenPart(%q): %v", name, err)
	}
	return p
}

func writeAll(t *testing.T, p *PartFile, data []byte) {
	t.Helper()
	if _, err := p.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestStoreDefersDirectoryCreation(t *testing.T) {
	st := newTestStore(t)
	if _, err := os.Stat(st.Dir()); !os.IsNotExist(err) {
		t.Fatalf("receive directory exists before first use (err=%v)", err)
	}
}

func TestStoreOpenPartCreatesDirectoryOnFirstNeed(t *testing.T) {
	st := newTestStore(t)
	p := mustOpenPart(t, st, "photo.jpg")
	defer p.Abort()

	info, err := os.Stat(st.Dir())
	if err != nil {
		t.Fatalf("receive directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("receive path is not a directory")
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("directory permissions %v allow group/others access", perm)
	}
}

func TestStoreOpenPartRejectsNonBareNames(t *testing.T) {
	st := newTestStore(t)
	for _, name := range []string{"", ".", "..", "a/b", "../x", strings.Repeat("x", 300) + "//"} {
		if _, err := st.OpenPart(name); !errors.Is(err, ErrInvalidFilename) {
			t.Errorf("OpenPart(%q) err = %v, want ErrInvalidFilename", name, err)
		}
	}
}

func TestStorePartFilePathIsInsideDir(t *testing.T) {
	st := newTestStore(t)
	p := mustOpenPart(t, st, "photo.jpg")
	defer p.Abort()

	if dir := filepath.Dir(p.PartPath()); dir != st.Dir() {
		t.Errorf("part path %q is outside the receive dir %q", p.PartPath(), st.Dir())
	}
	if !strings.Contains(filepath.Base(p.PartPath()), partSuffixMarker) {
		t.Errorf("part name %q lacks the %q marker", filepath.Base(p.PartPath()), partSuffixMarker)
	}
}

func TestStoreCommitPublishesFinalName(t *testing.T) {
	st := newTestStore(t)

	p := mustOpenPart(t, st, "photo.jpg")
	writeAll(t, p, []byte("jpeg-data"))
	final, err := p.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if final != filepath.Join(st.Dir(), "photo.jpg") {
		t.Errorf("Commit returned %q, want the plain name", final)
	}
	if _, err := os.Stat(final); err != nil {
		t.Errorf("published file missing: %v", err)
	}
	if _, err := os.Stat(p.PartPath()); !os.IsNotExist(err) {
		t.Errorf("part file still exists after commit (err=%v)", err)
	}
	// Content must survive publication.
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "jpeg-data" {
		t.Errorf("published content = %q, want %q", data, "jpeg-data")
	}
	if perm := filePerms(t, final); perm&0o077 != 0 {
		t.Errorf("file permissions %v allow group/others access", perm)
	}
}

func filePerms(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestStoreCommitCollisionProducesNumberedVariant(t *testing.T) {
	st := newTestStore(t)

	p1 := mustOpenPart(t, st, "photo.jpg")
	writeAll(t, p1, []byte("first"))
	if _, err := p1.Commit(); err != nil {
		t.Fatalf("first commit: %v", err)
	}

	p2 := mustOpenPart(t, st, "photo.jpg")
	writeAll(t, p2, []byte("second"))
	final, err := p2.Commit()
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if final != filepath.Join(st.Dir(), "photo (1).jpg") {
		t.Errorf("collision commit returned %q, want the (1) variant", final)
	}

	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read variant: %v", err)
	}
	if string(data) != "second" {
		t.Errorf("variant content = %q, want %q; the original must never be overwritten", data, "second")
	}

	// The untouched original is exactly one hard link's worth of data.
	data1, err := os.ReadFile(filepath.Join(st.Dir(), "photo.jpg"))
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(data1) != "first" {
		t.Errorf("original content = %q, want %q", data1, "first")
	}
}

func TestStoreCommitDotfileCollision(t *testing.T) {
	st := newTestStore(t)

	p1 := mustOpenPart(t, st, ".hidden")
	writeAll(t, p1, []byte("a"))
	if _, err := p1.Commit(); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	p2 := mustOpenPart(t, st, ".hidden")
	writeAll(t, p2, []byte("b"))
	final, err := p2.Commit()
	if err != nil {
		t.Fatalf("second commit: %v", err)
	}
	// Dotfiles keep the whole name as the stem: ".hidden (1)", never
	// " (1).hidden".
	if filepath.Base(final) != ".hidden (1)" {
		t.Errorf("dotfile variant = %q, want %q", filepath.Base(final), ".hidden (1)")
	}
}

func TestStoreConcurrentCommitsOfSameName(t *testing.T) {
	st := newTestStore(t)
	const workers = 5

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	names := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := st.OpenPart("photo.jpg")
			if err != nil {
				errs <- fmt.Errorf("worker %d open: %w", i, err)
				return
			}
			payload := fmt.Sprintf("data-%d", i)
			if _, err := p.Write([]byte(payload)); err != nil {
				errs <- fmt.Errorf("worker %d write: %w", i, err)
				return
			}
			final, err := p.Commit()
			if err != nil {
				errs <- fmt.Errorf("worker %d commit: %w", i, err)
				return
			}
			names <- final
		}(i)
	}
	wg.Wait()
	close(errs)
	close(names)
	for err := range errs {
		t.Error(err)
	}

	collected := make([]string, 0, workers)
	for name := range names {
		collected = append(collected, name)
	}
	if len(collected) != workers {
		t.Fatalf("got %d published names, want %d", len(collected), workers)
	}
	// Exactly the base name and its numbered variants, no duplicates.
	uniq := make(map[string]bool, workers)
	for _, name := range collected {
		uniq[name] = true
	}
	if len(uniq) != workers {
		t.Errorf("published names are not unique: %v", collected)
	}
	for _, want := range []string{"photo.jpg", "photo (1).jpg", "photo (2).jpg", "photo (3).jpg", "photo (4).jpg"} {
		if !uniq[filepath.Join(st.Dir(), want)] {
			t.Errorf("expected %q among %v; concurrent commits must not collide or overwrite", want, collected)
		}
	}
}

func TestStoreAbortRemovesPartFile(t *testing.T) {
	st := newTestStore(t)
	p := mustOpenPart(t, st, "photo.jpg")
	writeAll(t, p, []byte("discard-me"))
	path := p.PartPath()

	if err := p.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("part file survives abort (err=%v)", err)
	}
	// Abort is idempotent; a second call must not panic or error.
	if err := p.Abort(); err != nil {
		t.Errorf("second Abort: %v", err)
	}
}

func TestStoreDoubleCommitIsRefused(t *testing.T) {
	st := newTestStore(t)
	p := mustOpenPart(t, st, "photo.jpg")
	writeAll(t, p, []byte("x"))
	if _, err := p.Commit(); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	if _, err := p.Commit(); err == nil {
		t.Fatal("second Commit succeeded; the decided flag must be enforced")
	}
}

func TestStoreEmptyPartCommit(t *testing.T) {
	// A zero-byte declared upload is a legal receipt: publish an empty
	// file rather than failing the commit on EOF handling.
	st := newTestStore(t)
	p := mustOpenPart(t, st, "empty.txt")
	final, err := p.Commit()
	if err != nil {
		t.Fatalf("Commit of empty part: %v", err)
	}

	info, err := os.Stat(final)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("size = %d, want 0", info.Size())
	}
}

func TestStoreFolderPathPreservesHierarchy(t *testing.T) {
	st, err := NewStore(StoreConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root, err := st.AllocateFolderRoot("Photos")
	if err != nil {
		t.Fatal(err)
	}
	part, err := st.OpenPartPath(root, "2026/raw/image.dng")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("raw")); err != nil {
		t.Fatal(err)
	}
	final, err := part.Commit()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(st.Dir(), root, "2026", "raw", "image.dng")
	if final != want {
		t.Fatalf("final path = %q, want %q", final, want)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "raw" {
		t.Fatalf("content = %q, want raw", data)
	}
}

func TestStoreFolderRootsAreCollisionSafe(t *testing.T) {
	st, err := NewStore(StoreConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.AllocateFolderRoot("Photos")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.AllocateFolderRoot("Photos")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("folder roots collided: %q", first)
	}
	for _, root := range []string{first, second} {
		info, err := os.Stat(filepath.Join(st.Dir(), root))
		if err != nil || !info.IsDir() {
			t.Fatalf("allocated root %q is not a directory: %v", root, err)
		}
	}
}

func TestStoreDoesNotPublishPartFilesAsFinalNames(t *testing.T) {
	st := newTestStore(t)
	entries, err := os.ReadDir(st.Dir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("readdir: %v", err)
	}
	if err != nil {
		return // directory not created yet: nothing leaked
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), partSuffixMarker) {
			t.Errorf("stray part file left behind: %s", e.Name())
		}
	}
}
