package transfer

import (
	"encoding/json"
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
