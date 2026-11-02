package server

import (
	"strings"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// FuzzSafeArchivePath asserts that whatever a source name contains, an
// accepted archive entry name can never be absolute, contain a backslash, or
// carry an empty, "." or ".." component. Rejection is a valid outcome.
func FuzzSafeArchivePath(f *testing.F) {
	seeds := []string{
		"a.txt",
		"folder/a.txt",
		"folder/nested/deep/a.txt",
		"folder/",
		"/etc/passwd",
		"../escape.txt",
		"a/../../b.txt",
		`a\b.txt`,
		`..\..\escape.txt`,
		"a//b.txt",
		"a/./b.txt",
		"a/",
		"/",
		"",
		"données/été.txt",
		"日本語/ファイル.txt",
		"file:with:colons.txt",
		"trailing space .txt",
		strings.Repeat("d/", 200) + "f.txt",
		strings.Repeat("a", transfer.MaxRelativePathBytes+1),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		name, err := safeArchivePath(raw)
		if err != nil {
			return
		}
		if strings.HasPrefix(name, "/") {
			t.Fatalf("accepted absolute archive entry %q", name)
		}
		if strings.Contains(name, `\`) {
			t.Fatalf("accepted archive entry with a backslash: %q", name)
		}
		if len(name) > transfer.MaxRelativePathBytes {
			t.Fatalf("accepted %d-byte entry (limit %d)", len(name), transfer.MaxRelativePathBytes)
		}
		if err := validateArchiveName(name); err != nil {
			t.Fatalf("safeArchivePath returned %q that validation rejects: %v", name, err)
		}
		for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
			if part == "" || part == "." || part == ".." {
				t.Fatalf("accepted archive entry with component %q in %q", part, name)
			}
		}
	})
}

// FuzzUniqueArchiveName asserts the deduplication contract: the result is
// deterministic for the same occupancy set, and an already-used name never
// comes back unchanged (which would make a ZIP with two identical entries).
func FuzzUniqueArchiveName(f *testing.F) {
	f.Add("a.txt", "a.txt")
	f.Add("a.txt", "a.txt,a (1).txt")
	f.Add("a.txt", "")
	f.Add("archive.tar.gz", "archive.tar.gz")
	f.Add(".hidden", ".hidden")
	f.Add("noext", "noext")
	f.Add("données.txt", "données.txt")
	f.Fuzz(func(t *testing.T, name, usedCSV string) {
		used := make(map[string]bool)
		if usedCSV != "" {
			for _, entry := range strings.Split(usedCSV, ",") {
				if entry != "" {
					used[entry] = true
				}
			}
		}
		first := uniqueArchiveName(name, used)
		second := uniqueArchiveName(name, used)
		if first != second {
			t.Fatalf("deduplication is not deterministic: %q vs %q", first, second)
		}
		if used[name] && first == name {
			t.Fatalf("returned the already-occupied name %q", name)
		}
	})
}
