package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatContentDigest(t *testing.T) {
	// Vector 1: Empty input SHA-256
	emptySum := sha256.Sum256(nil)
	emptyHex := hex.EncodeToString(emptySum[:])
	wantEmptyHeader := "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:"
	if got := FormatContentDigest(emptyHex); got != wantEmptyHeader {
		t.Fatalf("FormatContentDigest(emptyHex) = %q, want %q", got, wantEmptyHeader)
	}

	// Vector 2: "hello world"
	helloSum := sha256.Sum256([]byte("hello world"))
	helloHex := hex.EncodeToString(helloSum[:])
	wantHelloHeader := "sha-256=:" + base64.StdEncoding.EncodeToString(helloSum[:]) + ":"
	if got := FormatContentDigest(helloHex); got != wantHelloHeader {
		t.Fatalf("FormatContentDigest(helloHex) = %q, want %q", got, wantHelloHeader)
	}

	// Invalid hex strings should return ""
	invalidInputs := []string{
		"",
		"abc",
		"xyz",
		strings.Repeat("a", 63), // 63 chars
		strings.Repeat("a", 65), // 65 chars
		strings.Repeat("g", 64), // invalid hex chars
	}
	for _, inv := range invalidInputs {
		if got := FormatContentDigest(inv); got != "" {
			t.Errorf("FormatContentDigest(%q) = %q, want empty string", inv, got)
		}
	}
}

func TestParseContentDigest(t *testing.T) {
	emptySum := sha256.Sum256(nil)
	emptyHex := hex.EncodeToString(emptySum[:])

	tests := []struct {
		name    string
		header  string
		wantHex string
		wantErr bool
	}{
		{
			name:    "RFC 9530 standard empty",
			header:  "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:",
			wantHex: emptyHex,
		},
		{
			name:    "RFC 9530 with leading/trailing spaces",
			header:  "  sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:  ",
			wantHex: emptyHex,
		},
		{
			name:    "Legacy Digest header format",
			header:  "SHA-256=47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=",
			wantHex: emptyHex,
		},
		{
			name:    "Missing closing colon",
			header:  "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=",
			wantErr: true,
		},
		{
			name:    "Invalid base64 payload",
			header:  "sha-256=:???invalid-base64???=:",
			wantErr: true,
		},
		{
			name:    "Unsupported digest algorithm",
			header:  "md5=:Q2hlY2sgSW50ZWdyaXR5IQ==:",
			wantErr: true,
		},
		{
			name:    "Empty header",
			header:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHex, err := ParseContentDigest(tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseContentDigest() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && gotHex != tt.wantHex {
				t.Fatalf("ParseContentDigest() gotHex = %q, want %q", gotHex, tt.wantHex)
			}
		})
	}
}

func TestComputeReaderAndFileSHA256(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Empty file
	emptyFile := filepath.Join(tempDir, "empty.bin")
	if err := os.WriteFile(emptyFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	emptyDigest, emptySize, err := ComputeFileSHA256(emptyFile)
	if err != nil {
		t.Fatalf("ComputeFileSHA256(empty) error: %v", err)
	}
	if emptySize != 0 {
		t.Errorf("emptySize = %d, want 0", emptySize)
	}
	wantEmptyHex := hex.EncodeToString(func() []byte { s := sha256.Sum256(nil); return s[:] }())
	if emptyDigest != wantEmptyHex {
		t.Errorf("emptyDigest = %q, want %q", emptyDigest, wantEmptyHex)
	}

	// 2. Multi-chunk file (128 KB = 2x 64KB chunks)
	data := bytes.Repeat([]byte("abcdef0123456789"), 8192) // 131072 bytes
	chunkFile := filepath.Join(tempDir, "chunked.bin")
	if err := os.WriteFile(chunkFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	digest, size, err := ComputeFileSHA256(chunkFile)
	if err != nil {
		t.Fatalf("ComputeFileSHA256(chunked) error: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}
	wantDigest := hex.EncodeToString(func() []byte { s := sha256.Sum256(data); return s[:] }())
	if digest != wantDigest {
		t.Errorf("digest = %q, want %q", digest, wantDigest)
	}

	// 3. Non-existent file error
	_, _, err = ComputeFileSHA256(filepath.Join(tempDir, "nonexistent.bin"))
	if err == nil {
		t.Error("ComputeFileSHA256(nonexistent) expected error, got nil")
	}

	// 4. Directory should fail as non-regular file
	_, _, err = ComputeFileSHA256(tempDir)
	if err == nil {
		t.Error("ComputeFileSHA256(dir) expected error, got nil")
	}
}
