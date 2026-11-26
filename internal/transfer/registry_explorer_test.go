package transfer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryBrowseSourceAndPagination(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Album")
	subA := filepath.Join(root, "a_photos")
	subB := filepath.Join(root, "b_empty")
	nested := filepath.Join(subA, "raw")
	for _, d := range []string{subA, subB, nested} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "readme.txt"):     "hello",
		filepath.Join(root, "cover.jpg"):      "coverbytes",
		filepath.Join(subA, "01.jpg"):         "one",
		filepath.Join(subA, "02.jpg"):         "two2",
		filepath.Join(nested, "01.dng"):       "rawdata",
		filepath.Join(nested, "02.dng"):       "moredata",
		filepath.Join(root, "notes/todo.txt"): "",
	}
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	reg := NewRegistry()
	src, err := reg.AddFolder(root, "sess-1")
	if err != nil {
		t.Fatalf("AddFolder: %v", err)
	}

	// Root listing: dirs sorted before files
	rootPage, err := reg.BrowseSource(src.ID, "", 0, 10)
	if err != nil {
		t.Fatalf("BrowseSource root: %v", err)
	}
	if rootPage.TotalEntries != 5 { // a_photos, b_empty, notes, cover.jpg, readme.txt
		t.Fatalf("TotalEntries = %d, want 5 (%+v)", rootPage.TotalEntries, rootPage.Items)
	}
	if rootPage.Items[0].Kind != "dir" || rootPage.Items[0].Name != "a_photos" {
		t.Fatalf("first entry = %+v, want dir a_photos", rootPage.Items[0])
	}
	if rootPage.Items[0].ItemCount != 4 || rootPage.Items[0].TotalBytes != int64(len("one")+len("two2")+len("rawdata")+len("moredata")) {
		t.Fatalf("a_photos summary = %+v", rootPage.Items[0])
	}
	if rootPage.Items[3].Kind != "file" || rootPage.Items[3].Name != "cover.jpg" || rootPage.Items[3].ID == "" {
		t.Fatalf("fourth entry = %+v", rootPage.Items[3])
	}

	// Serialized JSON must never leak host filesystem paths
	raw, _ := json.Marshal(rootPage)
	if strings.Contains(string(raw), root) {
		t.Fatalf("BrowseSource output leaked host root path %q: %s", root, raw)
	}

	// Nested listing
	subPage, err := reg.BrowseSource(src.ID, "a_photos", 0, 10)
	if err != nil {
		t.Fatalf("BrowseSource sub: %v", err)
	}
	if subPage.Prefix != "a_photos" || subPage.TotalEntries != 3 { // raw/, 01.jpg, 02.jpg
		t.Fatalf("subPage = %+v", subPage)
	}

	// Empty directory listing returns 0 items cleanly (not NotFound)
	emptyPage, err := reg.BrowseSource(src.ID, "b_empty", 0, 10)
	if err != nil {
		t.Fatalf("BrowseSource empty dir: %v", err)
	}
	if len(emptyPage.Items) != 0 || emptyPage.TotalEntries != 0 {
		t.Fatalf("emptyPage = %+v", emptyPage)
	}

	// Pagination across root entries with limit=2
	p1, err := reg.BrowseSource(src.ID, "", 0, 2)
	if err != nil || len(p1.Items) != 2 || !p1.HasMore || p1.NextCursor == nil || *p1.NextCursor != 2 {
		t.Fatalf("page 1 = %+v, err=%v", p1, err)
	}
	p2, err := reg.BrowseSource(src.ID, "", *p1.NextCursor, 2)
	if err != nil || len(p2.Items) != 2 || !p2.HasMore || p2.NextCursor == nil || *p2.NextCursor != 4 {
		t.Fatalf("page 2 = %+v, err=%v", p2, err)
	}
	p3, err := reg.BrowseSource(src.ID, "", *p2.NextCursor, 2)
	if err != nil || len(p3.Items) != 1 || p3.HasMore || p3.NextCursor != nil {
		t.Fatalf("page 3 = %+v, err=%v", p3, err)
	}
}

func TestRegistryBrowseSecurityAndResolveItems(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SafeFolder")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	f1 := filepath.Join(root, "sub", "one.txt")
	f2 := filepath.Join(root, "two.txt")
	_ = os.WriteFile(f1, []byte("111"), 0o600)
	_ = os.WriteFile(f2, []byte("2222"), 0o600)

	reg := NewRegistry()
	src, err := reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"..", "../etc", "sub/../sub", "a\\b", "./sub", "sub//deep", "bad\x00name"} {
		if _, err := reg.BrowseSource(src.ID, bad, 0, 10); !errors.Is(err, ErrInvalidBrowsePrefix) {
			t.Errorf("BrowseSource(%q) err = %v, want ErrInvalidBrowsePrefix", bad, err)
		}
	}
	if _, err := reg.BrowseSource(src.ID, "", -1, 10); !errors.Is(err, ErrInvalidBrowsePrefix) {
		t.Errorf("negative cursor err = %v", err)
	}
	if _, err := reg.BrowseSource(src.ID, "nonexistent", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing dir err = %v, want ErrNotFound", err)
	}

	// Resolve valid items across levels
	rootPage, _ := reg.BrowseSource(src.ID, "", 0, 50)
	subPage, _ := reg.BrowseSource(src.ID, "sub", 0, 50)
	idOne := subPage.Items[0].ID
	idTwo := rootPage.Items[1].ID

	gotSource, resolved, err := reg.ResolveSourceItems(src.ID, []string{idOne, idTwo})
	if err != nil || gotSource.ID != src.ID || len(resolved) != 2 {
		t.Fatalf("ResolveSourceItems = (%v, %d, %v)", gotSource.ID, len(resolved), err)
	}
	// Duplicate or unknown IDs rejected
	if _, _, err := reg.ResolveSourceItems(src.ID, []string{idOne, idOne}); !errors.Is(err, ErrInvalidBrowsePrefix) {
		t.Errorf("duplicate itemIds err = %v", err)
	}
	if _, _, err := reg.ResolveSourceItems(src.ID, []string{"unknown-id"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown itemId err = %v", err)
	}
}
