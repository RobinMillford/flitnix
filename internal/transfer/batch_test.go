package transfer

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeRelativePath(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"nested", "2026/photos/image.jpg", "2026/photos/image.jpg"},
		{"backslash canonicalization", `2026\photos\image.jpg`, "2026/photos/image.jpg"},
		{"unicode", "旅行/写真.jpg", "旅行/写真.jpg"},
	}
	for _, tt := range tests {
		got, err := SanitizeRelativePath(tt.raw)
		if err != nil || got != tt.want {
			t.Errorf("%s: got %q, err %v; want %q", tt.name, got, err, tt.want)
		}
	}
	for _, raw := range []string{
		"", "/absolute", `C:\absolute`, `\\server\share`, ".", "..",
		"a/../b", "a//b", "a///b", "/a", "a/", "a/./b", "a:b",
		"CON/file", "folder/NUL.txt", "folder/name. ", "folder/name.",
		"bad\x00name", "bad\nname",
	} {
		if _, err := SanitizeRelativePath(raw); err == nil {
			t.Errorf("SanitizeRelativePath(%q) unexpectedly succeeded", raw)
		}
	}
	if _, err := SanitizeRelativePath(string([]byte{0xff})); err == nil || utf8.ValidString(string([]byte{0xff})) {
		t.Fatal("invalid UTF-8 path was accepted")
	}
}

func TestSanitizeRelativePathLimits(t *testing.T) {
	if _, err := SanitizeRelativePath(strings.Repeat("a", MaxPathComponentBytes+1)); err == nil {
		t.Fatal("overlong component was accepted")
	}
	deep := strings.Repeat("a/", MaxRelativePathDepth) + "file"
	if _, err := SanitizeRelativePath(deep); err == nil {
		t.Fatal("overdeep path was accepted")
	}
	if _, err := SanitizeRelativePath(strings.Repeat("a/", 2) + strings.Repeat("b", MaxRelativePathBytes)); err == nil {
		t.Fatal("overlong path was accepted")
	}
}

func TestValidateManifestFolderAndFlatSemantics(t *testing.T) {
	folder, err := ValidateManifest(BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFolder,
		RootName:  "Photos",
		Items: []ManifestItem{
			{RelativePath: `2026\image.jpg`, Size: 4},
			{RelativePath: "readme.txt", Size: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if folder.RootName != "Photos" || folder.Items[0].RelativePath != "2026/image.jpg" || folder.TotalBytes != 6 {
		t.Fatalf("unexpected validated folder: %+v", folder)
	}

	if _, err := ValidateManifest(BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFiles,
		Items: []ManifestItem{
			{DisplayName: "same.txt", Size: 1},
			{DisplayName: "same.txt", Size: 2},
		},
	}); err != nil {
		t.Fatalf("duplicate flat basenames should be valid: %v", err)
	}
}

func TestValidateManifestRejectsInvalidMetadata(t *testing.T) {
	base := BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFolder,
		RootName:  "Photos",
		Items:     []ManifestItem{{RelativePath: "a.txt", Size: 1}},
	}
	for name, manifest := range map[string]BatchManifest{
		"bad direction":  {Direction: "unknown", Kind: base.Kind, RootName: base.RootName, Items: base.Items},
		"bad kind":       {Direction: base.Direction, Kind: "archive", RootName: base.RootName, Items: base.Items},
		"missing root":   {Direction: base.Direction, Kind: base.Kind, Items: base.Items},
		"negative size":  {Direction: base.Direction, Kind: base.Kind, RootName: base.RootName, Items: []ManifestItem{{RelativePath: "a.txt", Size: -1}}},
		"duplicate path": {Direction: base.Direction, Kind: base.Kind, RootName: base.RootName, Items: []ManifestItem{{RelativePath: `a\a.txt`}, {RelativePath: "a/a.txt"}}},
		"bad flat path":  {Direction: base.Direction, Kind: BatchKindFiles, Items: []ManifestItem{{DisplayName: "a.txt", RelativePath: "a.txt"}}},
	} {
		if _, err := ValidateManifest(manifest); err == nil {
			t.Errorf("%s: manifest unexpectedly succeeded", name)
		}
	}
}

func TestValidateManifestRejectsTotalOverflowAndAssignedOrder(t *testing.T) {
	_, err := ValidateManifest(BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFiles,
		Items: []ManifestItem{
			{ID: "a", Sequence: 0, DisplayName: "a", Size: math.MaxInt64},
			{ID: "b", Sequence: 1, DisplayName: "b", Size: 1},
		},
	})
	if err == nil {
		t.Fatal("overflowing total was accepted")
	}
	_, err = ValidateManifest(BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFiles,
		Items: []ManifestItem{
			{ID: "a", Sequence: 1, DisplayName: "a"},
			{ID: "b", Sequence: 0, DisplayName: "b"},
		},
	})
	if err == nil {
		t.Fatal("out-of-order assigned items were accepted")
	}
}

// ---- size acceptance: manifest metadata alone must not cap transfers ----

// TestValidateManifestAcceptsGigabyteScaleSizes guards the property the
// phone→desktop protocol relies on: a manifest is metadata, so declaring ~7
// GiB (24 episodes in one folder, or one huge file at all) must be accepted
// exactly, with the total derived from the items and never approximated or
// capped.
func TestValidateManifestAcceptsGigabyteScaleSizes(t *testing.T) {
	episode := int64(300 << 20) // 300 MiB: a plausible 24-episode season
	items := make([]ManifestItem, 24)
	for i := range items {
		items[i] = ManifestItem{RelativePath: fmt.Sprintf("ep%02d.mkv", i+1), Size: episode}
	}
	validated, err := ValidateManifest(BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFolder,
		RootName:  "Season",
		Items:     items,
	})
	if err != nil {
		t.Fatalf("a ~7 GiB folder manifest was rejected: %v", err)
	}
	if got := validated.TotalBytes; got != 24*episode {
		t.Fatalf("total bytes = %d, want the exact item sum %d", got, 24*episode)
	}
	if validated.TotalItemCount != 24 {
		t.Fatalf("item count = %d, want 24", validated.TotalItemCount)
	}

	// A single file as large as the machine can describe is accepted too:
	// no arbitrary ceiling exists on one file, only the int64 overflow
	// guard itself.
	const biggest = math.MaxInt64
	validated, err = ValidateManifest(BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFiles,
		Items:     []ManifestItem{{DisplayName: "disk.img", Size: biggest}},
	})
	if err != nil {
		t.Fatalf("a single math.MaxInt64-sized file was rejected: %v", err)
	}
	if got := validated.TotalBytes; got != biggest {
		t.Fatalf("total bytes = %d, want %d", got, biggest)
	}
}

// TestValidateManifestEnforcesLimitsExplicitly pins the two layered limit
// behaviors apart: the fixed metadata limits (item count, path shape) are
// always enforced, while any total-byte limit exists only as an int64
// overflow guard by default — a lowered limit must still be honored when a
// deployment sets one on purpose.
func TestValidateManifestEnforcesLimitsExplicitly(t *testing.T) {
	if DefaultManifestLimits.MaxTotalBytes != math.MaxInt64 {
		t.Fatalf("default MaxTotalBytes = %d, want math.MaxInt64: the default is an overflow guard, not a size cap", DefaultManifestLimits.MaxTotalBytes)
	}

	limits := DefaultManifestLimits
	limits.MaxItems = 3
	items := make([]ManifestItem, 4)
	for i := range items {
		items[i] = ManifestItem{DisplayName: fmt.Sprintf("f%d.bin", i)}
	}
	if _, err := ValidateManifestWithLimits(BatchManifest{
		Direction: DirectionPhoneToDesktop, Kind: BatchKindFiles, Items: items,
	}, limits); err == nil {
		t.Fatal("item-count limit was ignored")
	}

	byteLimits := DefaultManifestLimits
	byteLimits.MaxTotalBytes = 16
	if _, err := ValidateManifestWithLimits(BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFiles,
		Items:     []ManifestItem{{DisplayName: "big.bin", Size: 17}},
	}, byteLimits); err == nil {
		t.Fatal("an explicitly configured total-byte limit was ignored")
	}

	// Path-shape limits hold at the manifest level, not only in the
	// sanitizer's standalone tests.
	overlong := ManifestItem{RelativePath: strings.Repeat("a", MaxPathComponentBytes+1), Size: 1}
	if _, err := ValidateManifest(BatchManifest{
		Direction: DirectionPhoneToDesktop, Kind: BatchKindFolder, RootName: "R", Items: []ManifestItem{overlong},
	}); err == nil {
		t.Fatal("overlong relative-path component was accepted")
	}
	overdeep := ManifestItem{RelativePath: strings.Repeat("a/", MaxRelativePathDepth) + "file", Size: 1}
	if _, err := ValidateManifest(BatchManifest{
		Direction: DirectionPhoneToDesktop, Kind: BatchKindFolder, RootName: "R", Items: []ManifestItem{overdeep},
	}); err == nil {
		t.Fatal("overdeep relative path was accepted")
	}
}

func TestOpaqueIDsAndPhoneSafeViews(t *testing.T) {
	first, err := NewOpaqueID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewOpaqueID()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || first == second {
		t.Fatalf("IDs are not opaque and unique: %q %q", first, second)
	}

	batch := TransferBatch{
		ID: "batch", SessionKey: "secret-session", Items: []TransferItem{{
			ID: "item", BatchID: "batch", DisplayName: "file.txt",
			SourcePath: "/home/private/file.txt",
		}},
	}
	data, err := json.Marshal(batch.View())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"secret-session", "/home/private/file.txt", "SourcePath", "SessionKey"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("phone view exposes %q: %s", forbidden, text)
		}
	}
}
