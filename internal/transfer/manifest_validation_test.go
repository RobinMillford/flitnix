package transfer

import (
	"errors"
	"strings"
	"testing"
)

func folderManifestFor(root string, paths ...string) BatchManifest {
	items := make([]ManifestItem, 0, len(paths))
	for _, path := range paths {
		items = append(items, ManifestItem{RelativePath: path, Size: 1})
	}
	return BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFolder,
		RootName:  root,
		Items:     items,
	}
}

// TestValidateManifestRejectsUnsafeFolderPaths is the server-side half of the
// folder-selection contract: a phone's webkitRelativePath is untrusted input,
// so every shape that could escape the batch root, collide on another
// platform, or smuggle a control byte must be refused before any upload
// record exists.
func TestValidateManifestRejectsUnsafeFolderPaths(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"absolute posix", "/etc/passwd"},
		{"absolute posix nested", "/var/tmp/x.txt"},
		{"windows drive", "C:/Windows/win.ini"},
		{"windows drive backslash", `C:\Windows\win.ini`},
		{"windows drive relative", "C:file.txt"},
		{"unc path", `\\server\share\file.txt`},
		{"dot component", "./file.txt"},
		{"dotdot component", "a/../b.txt"},
		{"trailing dotdot", "a/.."},
		{"leading dotdot", "../a.txt"},
		{"empty component", "a//b.txt"},
		{"trailing separator", "a/b/"},
		{"nul byte", "a\x00b.txt"},
		{"newline", "a\nb.txt"},
		{"carriage return", "a\rb.txt"},
		{"tab", "a\tb.txt"},
		{"invalid utf8", string([]byte{0x66, 0xff, 0x66})},
		{"windows device", "CON.txt"},
		{"windows device nested", "a/LPT1"},
		{"colon component", "a/b:c.txt"},
		{"trailing space", "a/b "},
		{"trailing dot", "a/b."},
		{"deep nesting", strings.Repeat("d/", MaxRelativePathDepth) + "f.txt"},
		{"overlong path", strings.Repeat("a", MaxRelativePathBytes) + "/b"},
		{"overlong component", strings.Repeat("a", MaxPathComponentBytes+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateManifest(folderManifestFor("Photos", tc.path))
			if err == nil {
				t.Fatalf("accepted unsafe relative path %q", tc.path)
			}
			if !errors.Is(err, ErrInvalidRelativePath) && !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("error %v is not classified as an invalid path or batch", err)
			}
		})
	}
}

// TestValidateManifestRejectsDuplicateNormalizedPaths proves the duplicate
// check runs on the canonical form, so two spellings of the same destination
// cannot both be accepted for one batch.
func TestValidateManifestRejectsDuplicateNormalizedPaths(t *testing.T) {
	if _, err := ValidateManifest(folderManifestFor("Photos", "a/b.txt", `a\b.txt`)); err == nil {
		t.Fatal("accepted two items that normalize to the same destination")
	}
	if _, err := ValidateManifest(folderManifestFor("Photos", "a/b.txt", "a/B.txt")); err != nil {
		t.Fatalf("rejected two distinct case-sensitive paths: %v", err)
	}
	// Configurable limits are the documented escape hatch for a tighter
	// installation; a limit of zero is invalid rather than silently ignored.
	if _, err := ValidateManifestWithLimits(folderManifestFor("Photos", "a.txt"), ManifestLimits{}); err == nil {
		t.Fatal("accepted invalid validation limits")
	}
}

func TestValidateManifestEnforcesItemPathAndByteLimits(t *testing.T) {
	limits := ManifestLimits{MaxItems: 2, MaxManifestPathBytes: 8, MaxTotalBytes: 10}

	if _, err := ValidateManifestWithLimits(folderManifestFor("Photos", "a", "b", "c"), limits); err == nil {
		t.Fatal("accepted a manifest above the item-count limit")
	}
	if _, err := ValidateManifestWithLimits(folderManifestFor("Photos", "a/verylongname.txt"), limits); err == nil {
		t.Fatal("accepted a relative path above the byte limit")
	}
	tooBig := folderManifestFor("Photos", "a", "b")
	for i := range tooBig.Items {
		tooBig.Items[i].Size = 6
	}
	if _, err := ValidateManifestWithLimits(tooBig, limits); err == nil {
		t.Fatal("accepted a manifest above the declared total-byte limit")
	}
	negative := folderManifestFor("Photos", "a")
	negative.Items[0].Size = -1
	if _, err := ValidateManifestWithLimits(negative, limits); err == nil {
		t.Fatal("accepted a negative declared size")
	}
}

// TestValidateManifestRootNameRules covers the batch root, which also becomes
// a directory on the receiver.
func TestValidateManifestRootNameRules(t *testing.T) {
	for _, root := range []string{"", "..", ".", "a/b", `a\b`, "CON", "a:c", strings.Repeat("a", MaxFilenameBytes+1)} {
		if _, err := ValidateManifest(folderManifestFor(root, "a.txt")); err == nil {
			t.Fatalf("accepted unsafe folder root %q", root)
		}
	}
	if _, err := ValidateManifest(folderManifestFor("  Photos  ", "a.txt")); err != nil {
		t.Fatalf("rejected a trimmable folder root: %v", err)
	}
	flat := folderManifestFor("", "a.txt")
	flat.Kind = BatchKindFiles
	flat.Items = []ManifestItem{{DisplayName: "a.txt", Size: 1}}
	if _, err := ValidateManifest(flat); err != nil {
		t.Fatalf("rejected a valid flat collection: %v", err)
	}
	flat.Items = []ManifestItem{{DisplayName: "a/b.txt", Size: 1}}
	if _, err := ValidateManifest(flat); err == nil {
		t.Fatal("accepted a flat item whose display name contains a separator")
	}
}

// TestValidateManifestCanonicalizesValidUnicodeFolder proves the accepted
// path is stored in one canonical spelling and that the display name is
// derived from it rather than from the client's original string.
func TestValidateManifestCanonicalizesValidUnicodeFolder(t *testing.T) {
	manifest := folderManifestFor("Photos", `données\été\photo 1.jpg`)
	validated, err := ValidateManifest(manifest)
	if err != nil {
		t.Fatalf("ValidateManifest: %v", err)
	}
	item := validated.Items[0]
	if item.RelativePath != "données/été/photo 1.jpg" {
		t.Fatalf("relative path = %q, want canonical slash form", item.RelativePath)
	}
	if item.DisplayName != "photo 1.jpg" {
		t.Fatalf("display name = %q, want the basename of the canonical path", item.DisplayName)
	}
	if validated.TotalItemCount != 1 || validated.TotalBytes != 1 {
		t.Fatalf("validated totals = %d items / %d bytes", validated.TotalItemCount, validated.TotalBytes)
	}
}
