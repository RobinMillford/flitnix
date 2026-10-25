package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// makeSharedable creates a regular file with the given contents and returns
// its absolute path.
func makeSharedable(t *testing.T, name string, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("create shared file: %v", err)
	}
	return path
}

func TestRegistryAddGetRoundtrip(t *testing.T) {
	r := NewRegistry()
	path := makeSharedable(t, "movie (final).mkv", randomBytes(t, 2048))

	sf, err := r.Add(path, "movie (final).mkv", "sess-key-reg")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sf.ID == "" {
		t.Error("Add returned an empty ID")
	}
	if sf.Name != "movie (final).mkv" {
		t.Errorf("Name = %q, want the sanitized input unchanged", sf.Name)
	}
	if sf.Size != 2048 {
		t.Errorf("Size = %d, want 2048 taken from lstat, not the client", sf.Size)
	}
	if sf.Path != path || sf.Session != "sess-key-reg" || sf.Created.IsZero() {
		t.Errorf("record fields not preserved: %+v", sf)
	}
	if len(r.List()) != 1 || r.Len() != 1 {
		t.Errorf("registry holds %d files, want 1", r.Len())
	}

	got, ok := r.Get(sf.ID)
	if !ok || got != sf {
		t.Errorf("Get = (%+v, %v), want the same record back", got, ok)
	}

	// Desktop-owned files carry an empty session key.
	own, err := r.Add(path, "movie (final).mkv", "")
	if err != nil {
		t.Fatalf("Add desktop-owned: %v", err)
	}
	if own.Session != "" || own.ID == sf.ID {
		t.Errorf("desktop-owned record wrong: %+v (previous ID %q)", own, sf.ID)
	}
}

func TestRegistrySizeTakenAtAddTime(t *testing.T) {
	r := NewRegistry()
	const initialLen = 1024
	path := makeSharedable(t, "grows.txt", randomBytes(t, initialLen))

	sf, err := r.Add(path, "grows.txt", "s")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if sf.Size != int64(initialLen) {
		t.Fatalf("Size = %d, want %d from lstat", sf.Size, initialLen)
	}
	// The file grows on disk after registration: the snapshot stays honest
	// to Add-time, because the download handler streams actual content and
	// must not trust client-side size claims.
	if err := os.WriteFile(path, randomBytes(t, initialLen*4), 0o600); err != nil {
		t.Fatalf("grow file: %v", err)
	}
	if got, ok := r.Get(sf.ID); !ok || got.Size != int64(initialLen) {
		t.Fatalf("Get after growth: ok=%v Size=%d, want the Add-time %d", ok, got.Size, initialLen)
	}
}

func TestRegistryRejectsNonRegularFiles(t *testing.T) {
	r := NewRegistry()
	dir := t.TempDir()

	if _, err := r.Add(dir, "anything.zip", "s"); !errors.Is(err, ErrNotRegularFile) {
		t.Errorf("directory err = %v, want ErrNotRegularFile", err)
	}
	if _, err := r.Add(filepath.Join(dir, "ghost.jpg"), "ghost.jpg", "s"); !errors.Is(err, ErrMissingFile) {
		t.Errorf("missing file err = %v, want ErrMissingFile", err)
	}
	if _, err := r.Add("", "x.jpg", "s"); !errors.Is(err, ErrMissingFile) {
		t.Errorf("empty path err = %v, want ErrMissingFile", err)
	}
	if _, err := r.Add(dir, "../escape.bin", "s"); !errors.Is(err, ErrInvalidFilename) {
		t.Errorf("traversal name err = %v, want ErrInvalidFilename", err)
	}
	if got := r.Len(); got != 0 {
		t.Errorf("rejected adds left %d entries behind, want 0", got)
	}
}

func TestRegistryReservationCap(t *testing.T) {
	r := NewRegistry()
	path := makeSharedable(t, "one.file", []byte("x"))

	var first SharedFile
	for i := 0; i < registryMaxFiles; i++ {
		sf, err := r.Add(path, "one.file", "s")
		if err != nil {
			t.Fatalf("add %d: %v", i+1, err)
		}
		if i == 0 {
			first = sf
		}
	}
	// The 513th distinct registration must bounce off the backstop.
	if _, err := r.Add(path, "one.file", "s"); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("add beyond cap err = %v, want ErrNoCapacity", err)
	}
	// Revocation frees its slot for the next registration.
	if !r.Remove(first.ID) {
		t.Fatal("Remove of a real ID reported false")
	}
	if _, err := r.Add(path, "one.file", "s"); err != nil {
		t.Errorf("add after revocation err = %v, want success", err)
	}
}

func TestRegistryListSessionAndClear(t *testing.T) {
	r := NewRegistry()
	p1 := makeSharedable(t, "alpha.jpg", []byte("a"))
	p2 := makeSharedable(t, "beta.png", []byte("b"))
	p3 := makeSharedable(t, "gamma.txt", []byte("g"))

	a1, err := r.Add(p1, "alpha.jpg", "sess-a")
	if err != nil {
		t.Fatalf("Add a1: %v", err)
	}
	a2, err := r.Add(p2, "beta.png", "sess-a")
	if err != nil {
		t.Fatalf("Add a2: %v", err)
	}
	owned, err := r.Add(p3, "gamma.txt", "")
	if err != nil {
		t.Fatalf("Add owned: %v", err)
	}

	sessionA := r.ListSession("sess-a")
	if len(sessionA) != 2 || sessionA[0].ID != a1.ID || sessionA[1].ID != a2.ID {
		t.Errorf("ListSession(sess-a) = %+v, want exactly the two session files", sessionA)
	}
	// Desktop-owned files are exactly the ones with the '' session key.
	if desk := r.ListSession(""); len(desk) != 1 || desk[0].ID != owned.ID {
		t.Errorf("ListSession('') = %+v, want only the desktop-owned file", desk)
	}
	if all := r.List(); len(all) != 3 {
		t.Errorf("List = %d entries, want 3 in insertion order", len(all))
	}

	// Removing lists-scope only: the disk file must survive untouched.
	if !r.Remove(a2.ID) {
		t.Fatal("Remove(a2) reported false")
	}
	if r.Remove(a2.ID) {
		t.Error("double Remove reported true; unlisting is not idempotent")
	}
	if _, ok := r.Get(a2.ID); ok {
		t.Error("removed file is still reachable by Get")
	}
	if _, err := os.Stat(p2); err != nil {
		t.Errorf("Remove must not touch the disk file: %v", err)
	}

	// Clear empties everything: the desktop's end-of-session wipe.
	r.Clear()
	if r.Len() != 0 || len(r.List()) != 0 || len(r.ListSession("sess-a")) != 0 {
		t.Errorf("Clear left content behind (len=%d)", r.Len())
	}
	if _, ok := r.Get(a1.ID); ok {
		t.Error("Get after Clear still returned a record")
	}
}

// TestRegistryConcurrentAddRemoveList hammers the registry from several
// goroutines to confirm the mutex keeps every view consistent (the -race
// build cares); the cap may deny overflow adds, which is acceptable here.
func TestRegistryConcurrentAddRemoveList(t *testing.T) {
	r := NewRegistry()
	path := makeSharedable(t, "race.file", []byte("data"))

	done := make(chan struct{})
	for g := 0; g < 4; g++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 50; i++ {
				sf, err := r.Add(path, "race.file", "s")
				if err != nil {
					continue
				}
				if _, ok := r.Get(sf.ID); !ok {
					t.Errorf("freshly added ID vanished")
					return
				}
				if !r.Remove(sf.ID) {
					t.Errorf("freshly added ID cannot be removed")
					return
				}
				_ = r.List()
			}
		}()
	}
	for g := 0; g < 4; g++ {
		<-done
	}
	if r.Len() != 0 {
		t.Errorf("after paired add/remove cycles %d entries remain, want 0", r.Len())
	}
}
