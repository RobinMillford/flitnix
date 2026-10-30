package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestCanonicalManifestTestVectors(t *testing.T) {
	// Vector 1: Standard multi-item ordering & minification
	m1 := &CanonicalManifest{
		Kind:     "files",
		RootName: "",
		Items: []CanonicalManifestItem{
			{
				Sequence:     2,
				Size:         "524288",
				RelativePath: "images/photo.png",
				SHA256:       "4b227777d4dd1fc61c6f884f48641d02b4d121d3fd328cb08b5531fcacdabf8a",
				DisplayName:  "photo.png",
			},
			{
				DisplayName:  "contract.pdf",
				Size:         "1048576",
				SHA256:       "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4",
				RelativePath: "documents/contract.pdf",
				Sequence:     1,
			},
		},
	}

	bytes1, hash1, err := ComputeManifestSHA256(m1)
	if err != nil {
		t.Fatalf("ComputeManifestSHA256(m1) failed: %v", err)
	}

	expectedString1 := `{"items":[{"displayName":"contract.pdf","relativePath":"documents/contract.pdf","sequence":1,"sha256":"8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4","size":"1048576"},{"displayName":"photo.png","relativePath":"images/photo.png","sequence":2,"sha256":"4b227777d4dd1fc61c6f884f48641d02b4d121d3fd328cb08b5531fcacdabf8a","size":"524288"}],"kind":"files","rootName":""}`
	if string(bytes1) != expectedString1 {
		t.Errorf("m1 string mismatch:\ngot:  %s\nwant: %s", string(bytes1), expectedString1)
	}
	if len(bytes1) != 384 {
		t.Errorf("m1 length = %d, want 384", len(bytes1))
	}
	if hash1 != "c7b28b1a12ee2ab0a89e76de18037783aa587463a57adca93de3276e28d6b554" {
		t.Errorf("m1 hash = %s, want c7b28b1a12ee2ab0a89e76de18037783aa587463a57adca93de3276e28d6b554", hash1)
	}

	// Vector 2: Unicode filenames and escape sequences
	v2Raw := `{"displayName":"quote \"special\" \\ test.txt","relativePath":"unicode/レポート_🎉.txt","sequence":1,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","size":"0"}`
	bytes2, err := CanonicalizeJSON([]byte(v2Raw))
	if err != nil {
		t.Fatalf("CanonicalizeJSON(v2Raw) failed: %v", err)
	}
	h2 := sha256.Sum256(bytes2)
	hash2 := hex.EncodeToString(h2[:])
	if len(bytes2) != 194 {
		t.Errorf("v2 length = %d, want 194", len(bytes2))
	}
	if hash2 != "3ebc0c54f658d802555be2efeb66bc5d3bdd525449b8405121e988e5c827d449" {
		t.Errorf("v2 hash = %s, want 3ebc0c54f658d802555be2efeb66bc5d3bdd525449b8405121e988e5c827d449", hash2)
	}

	// Vector 3: Large file size (> 2^53-1)
	v3Raw := `{"displayName":"huge_archive.iso","relativePath":"huge_archive.iso","sequence":1,"sha256":"a591a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f146e","size":"9007199254740993"}`
	bytes3, err := CanonicalizeJSON([]byte(v3Raw))
	if err != nil {
		t.Fatalf("CanonicalizeJSON(v3Raw) failed: %v", err)
	}
	h3 := sha256.Sum256(bytes3)
	hash3 := hex.EncodeToString(h3[:])
	if len(bytes3) != 183 {
		t.Errorf("v3 length = %d, want 183", len(bytes3))
	}
	if hash3 != "e4f5f4b3d559a057008a246d52598fb1f0c8056daef53dcecd7842a4c75b2273" {
		t.Errorf("v3 hash = %s, want e4f5f4b3d559a057008a246d52598fb1f0c8056daef53dcecd7842a4c75b2273", hash3)
	}
}

func TestValidateDecimalSize(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{"0", true},
		{"1024", true},
		{"9007199254740993", true},
		{"0123", false}, // leading zero
		{"-5", false},   // negative
		{"12.34", false},
		{"", false},
		{"abc", false},
	}

	for _, tc := range tests {
		err := ValidateDecimalSize(tc.input)
		if (err == nil) != tc.valid {
			t.Errorf("ValidateDecimalSize(%q): got err=%v, want valid=%v", tc.input, err, tc.valid)
		}
	}
}
