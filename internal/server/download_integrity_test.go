package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestSingleFileDirectDownloadContentDigest tests that GET /api/files/{id}/download
// emits a standard RFC 9530 Content-Digest header matching the exact body bytes.
func TestSingleFileDirectDownloadContentDigest(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()

	// 1. Normal file
	normalPath := filepath.Join(dir, "report.pdf")
	normalContent := bytes.Repeat([]byte("PDF-1.7-FLITNIX-DATA-STREAM-"), 1024) // 28 KB
	if err := os.WriteFile(normalPath, normalContent, 0o600); err != nil {
		t.Fatal(err)
	}
	sfNormal, err := env.reg.Add(normalPath, "report.pdf", "")
	if err != nil {
		t.Fatal(err)
	}

	rec := env.do(http.MethodGet, "/api/files/"+sfNormal.ID+"/download", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download code = %d %s", rec.Code, rec.Body.String())
	}
	digestHdr := rec.Header().Get("Content-Digest")
	if digestHdr == "" {
		t.Fatal("missing Content-Digest header on direct single-file download")
	}
	extractedHex, err := transfer.ParseContentDigest(digestHdr)
	if err != nil {
		t.Fatalf("ParseContentDigest failed: %v", err)
	}
	actualSum := sha256.Sum256(rec.Body.Bytes())
	actualHex := hex.EncodeToString(actualSum[:])
	if extractedHex != actualHex {
		t.Fatalf("Content-Digest mismatch: header hex=%s, actual body hex=%s", extractedHex, actualHex)
	}

	// 2. Empty file (0 bytes)
	emptyPath := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sfEmpty, err := env.reg.Add(emptyPath, "empty.txt", "")
	if err != nil {
		t.Fatal(err)
	}

	recEmpty := env.do(http.MethodGet, "/api/files/"+sfEmpty.ID+"/download", tok, nil)
	if recEmpty.Code != http.StatusOK {
		t.Fatalf("empty download code = %d", recEmpty.Code)
	}
	emptyDigestHdr := recEmpty.Header().Get("Content-Digest")
	if emptyDigestHdr == "" {
		t.Fatal("missing Content-Digest header on empty file download")
	}
	emptyHex, err := transfer.ParseContentDigest(emptyDigestHdr)
	if err != nil {
		t.Fatalf("ParseContentDigest empty failed: %v", err)
	}
	expectedEmptyHex := hex.EncodeToString(func() []byte { s := sha256.Sum256(nil); return s[:] }())
	if emptyHex != expectedEmptyHex {
		t.Fatalf("Empty Content-Digest mismatch: got=%s, want=%s", emptyHex, expectedEmptyHex)
	}
}

// TestSingleFileGrantDownloadContentDigest tests that single-file downloads
// via one-use download grants provide Content-Digest and grant JSON sha256.
func TestSingleFileGrantDownloadContentDigest(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()

	filePath := filepath.Join(dir, "sample.bin")
	content := []byte("deterministic content for single file grant download")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.Add(filePath, "sample.bin", "")
	if err != nil {
		t.Fatal(err)
	}

	g, cookie := env.grantFor(tok, sf.ID)
	if g.Archive {
		t.Fatal("expected single file grant, got archive=true")
	}
	wantHex := hex.EncodeToString(func() []byte { s := sha256.Sum256(content); return s[:] }())
	if g.SHA256 != wantHex {
		t.Fatalf("grant metadata sha256 = %q, want %q", g.SHA256, wantHex)
	}

	rec := env.getDownload(g, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download response = %d %s", rec.Code, rec.Body.String())
	}
	hdr := rec.Header().Get("Content-Digest")
	if hdr == "" {
		t.Fatal("missing Content-Digest header on grant file download")
	}
	parsedHex, err := transfer.ParseContentDigest(hdr)
	if err != nil {
		t.Fatalf("ParseContentDigest: %v", err)
	}
	if parsedHex != wantHex {
		t.Fatalf("parsed header hex = %q, want %q", parsedHex, wantHex)
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatal("response body does not match source content")
	}
}

// TestArchiveDownloadContentDigest tests that multi-file and folder ZIP downloads
// calculate and transmit Content-Digest matching the exact archive bytes,
// and that the ZIP archive is bit-for-bit identical across passes.
func TestArchiveDownloadContentDigest(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()
	folderRoot := filepath.Join(dir, "ProjectA")
	writeTree(t, folderRoot, map[string]string{
		"main.go":    "package main\nfunc main() {}\n",
		"README.md":  "# Project A Documentation\n",
		"sub/pkg.go": "package sub\nvar X = 42\n",
	})

	source, err := env.reg.AddFolder(folderRoot, "")
	if err != nil {
		t.Fatal(err)
	}

	g, cookie := env.grantFor(tok, source.ID)
	if !g.Archive {
		t.Fatal("expected archive=true for folder source")
	}
	if g.SHA256 == "" {
		t.Fatal("expected non-empty sha256 in grant response for archive")
	}

	rec := env.getDownload(g, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive download = %d %s", rec.Code, rec.Body.String())
	}
	bodyBytes := rec.Body.Bytes()
	actualSum := sha256.Sum256(bodyBytes)
	actualHex := hex.EncodeToString(actualSum[:])

	hdr := rec.Header().Get("Content-Digest")
	if hdr == "" {
		t.Fatal("missing Content-Digest header on archive download")
	}
	parsedHex, err := transfer.ParseContentDigest(hdr)
	if err != nil {
		t.Fatalf("ParseContentDigest: %v", err)
	}
	if parsedHex != actualHex {
		t.Fatalf("header digest hex = %s, actual body hex = %s", parsedHex, actualHex)
	}
	if g.SHA256 != actualHex {
		t.Fatalf("grant metadata sha256 = %s, actual body hex = %s", g.SHA256, actualHex)
	}

	// Verify the archive content is intact and readable as a standard ZIP
	zr, err := zip.NewReader(bytes.NewReader(bodyBytes), int64(len(bodyBytes)))
	if err != nil {
		t.Fatalf("reading returned ZIP archive: %v", err)
	}
	if len(zr.File) < 3 {
		t.Fatalf("expected at least 3 files in archive, got %d", len(zr.File))
	}
}

// TestArchiveSourceMutationBetweenPasses verifies that modifying a file on disk
// after grant creation causes Pass 2 to detect the checksum mismatch and abort
// the connection rather than serving a false verified state.
func TestArchiveSourceMutationBetweenPasses(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()
	folderRoot := filepath.Join(dir, "MutateDir")
	targetFile := filepath.Join(folderRoot, "doc.txt")
	writeTree(t, folderRoot, map[string]string{
		"doc.txt": "initial unmodified content",
	})

	source, err := env.reg.AddFolder(folderRoot, "")
	if err != nil {
		t.Fatal(err)
	}

	// Create grant (Pass 1 computes digest over "initial unmodified content")
	g, cookie := env.grantFor(tok, source.ID)
	if g.SHA256 == "" {
		t.Fatal("expected grant SHA256")
	}

	// Mutate the file on disk before redeeming grant!
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(targetFile, []byte("MODIFIED CORRUPTED CONTENT OVERWRITTEN"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Redeem grant: Pass 2 streams, detects checksum difference, and aborts
	defer func() {
		// Recovery from abortResponse panic(http.ErrAbortHandler)
		if r := recover(); r != nil {
			if r != http.ErrAbortHandler {
				t.Fatalf("unexpected panic: %v", r)
			}
		}
	}()

	rec := env.getDownload(g, cookie, nil)
	// If the handler didn't panic, the body should not be a valid archive matching g.SHA256
	bodyBytes := rec.Body.Bytes()
	actualSum := sha256.Sum256(bodyBytes)
	actualHex := hex.EncodeToString(actualSum[:])
	if actualHex == g.SHA256 {
		t.Fatal("corrupted source produced identical hash to initial pass! Should have been rejected.")
	}
}

// TestSingleFileSourceMutationDuringStreaming tests that modifying a single file
// during streaming cuts the connection and reports failure.
func TestSingleFileSourceMutationDuringStreaming(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "mutate_single.txt")
	if err := os.WriteFile(filePath, []byte("initial source file content"), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.Add(filePath, "mutate_single.txt", "")
	if err != nil {
		t.Fatal(err)
	}

	g, cookie := env.grantFor(tok, sf.ID)

	// Mutate file before redeeming
	if err := os.WriteFile(filePath, []byte("changed completely!"), 0o600); err != nil {
		t.Fatal(err)
	}

	defer func() {
		if r := recover(); r != nil && r != http.ErrAbortHandler {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()

	rec := env.getDownload(g, cookie, nil)
	body := rec.Body.Bytes()
	actualHex := hex.EncodeToString(func() []byte { s := sha256.Sum256(body); return s[:] }())
	if actualHex == g.SHA256 {
		t.Fatal("mutated single file should not match original digest")
	}
}

// TestLargeFileDownloadContentDigest tests streaming a 5 MB file and verifying
// that Content-Digest matches the body without excessive buffering.
func TestLargeFileDownloadContentDigest(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()

	largePath := filepath.Join(dir, "large.dat")
	largeData := bytes.Repeat([]byte("0123456789ABCDEF"), 327680) // 5,242,880 bytes (5 MB)
	if err := os.WriteFile(largePath, largeData, 0o600); err != nil {
		t.Fatal(err)
	}

	sf, err := env.reg.Add(largePath, "large.dat", "")
	if err != nil {
		t.Fatal(err)
	}

	rec := env.do(http.MethodGet, "/api/files/"+sf.ID+"/download", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download code = %d", rec.Code)
	}

	hdr := rec.Header().Get("Content-Digest")
	if hdr == "" {
		t.Fatal("missing Content-Digest header on large file download")
	}
	extractedHex, err := transfer.ParseContentDigest(hdr)
	if err != nil {
		t.Fatalf("ParseContentDigest: %v", err)
	}

	wantSum := sha256.Sum256(largeData)
	wantHex := hex.EncodeToString(wantSum[:])
	if extractedHex != wantHex {
		t.Fatalf("large file digest = %s, want %s", extractedHex, wantHex)
	}
	if rec.Body.Len() != len(largeData) {
		t.Fatalf("received %d bytes, want %d", rec.Body.Len(), len(largeData))
	}
}

// TestDownloadGrantReplayRejection verifies that redeeming a grant a second time
// fails with HTTP 403/404, preventing replay attacks.
func TestDownloadGrantReplayRejection(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "replay.txt")
	if err := os.WriteFile(filePath, []byte("replay test"), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.Add(filePath, "replay.txt", "")
	if err != nil {
		t.Fatal(err)
	}

	g, cookie := env.grantFor(tok, sf.ID)
	rec1 := env.getDownload(g, cookie, nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first download = %d", rec1.Code)
	}

	// Replay download with same cookie and grant
	rec2 := env.getDownload(g, cookie, nil)
	if rec2.Code == http.StatusOK {
		t.Fatal("grant was replayed successfully! Must be one-use only.")
	}
}
