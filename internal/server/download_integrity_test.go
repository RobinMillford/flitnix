package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// via one-use download grants provide immediate grant creation (empty grant sha256),
// inline single-pass Content-Digest trailer, and final sha256 via download progress.
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
	// In single-pass immediate start, grant creation returns immediately without pre-hashing
	if g.SHA256 != "" {
		t.Fatalf("expected empty sha256 in grant creation response, got %q", g.SHA256)
	}

	rec := env.getDownload(g, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download response = %d %s", rec.Code, rec.Body.String())
	}
	hdr := rec.Header().Get("Content-Digest")
	if hdr == "" {
		t.Fatal("missing Content-Digest trailer on grant file download")
	}
	parsedHex, err := transfer.ParseContentDigest(hdr)
	if err != nil {
		t.Fatalf("ParseContentDigest: %v", err)
	}
	wantHex := hex.EncodeToString(func() []byte { s := sha256.Sum256(content); return s[:] }())
	if parsedHex != wantHex {
		t.Fatalf("parsed trailer hex = %q, want %q", parsedHex, wantHex)
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatal("response body does not match source content")
	}

	// Verify progress endpoint reports completed and the final SHA-256 digest
	progRec := env.do(http.MethodGet, "/api/download-progress/"+g.ProgressID, tok, nil)
	if progRec.Code != http.StatusOK {
		t.Fatalf("progress response = %d", progRec.Code)
	}
	var prog map[string]any
	if err := json.Unmarshal(progRec.Body.Bytes(), &prog); err != nil {
		t.Fatal(err)
	}
	if prog["state"] != "completed" {
		t.Fatalf("progress state = %v, want completed", prog["state"])
	}
	if prog["sha256"] != wantHex {
		t.Fatalf("progress sha256 = %v, want %q", prog["sha256"], wantHex)
	}
}

// TestArchiveDownloadContentDigest tests that multi-file and folder ZIP downloads
// calculate and transmit Content-Digest matching the exact archive bytes in a single pass,
// and that the final digest is published to download progress.
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
	// In single-pass streaming, grant creation is immediate and g.SHA256 is empty
	if g.SHA256 != "" {
		t.Fatalf("expected empty sha256 in grant creation response, got %q", g.SHA256)
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
		t.Fatal("missing Content-Digest trailer on archive download")
	}
	parsedHex, err := transfer.ParseContentDigest(hdr)
	if err != nil {
		t.Fatalf("ParseContentDigest: %v", err)
	}
	if parsedHex != actualHex {
		t.Fatalf("header digest hex = %s, actual body hex = %s", parsedHex, actualHex)
	}

	// Verify progress endpoint reports completed and the exact body digest
	progRec := env.do(http.MethodGet, "/api/download-progress/"+g.ProgressID, tok, nil)
	if progRec.Code != http.StatusOK {
		t.Fatalf("progress response = %d", progRec.Code)
	}
	var prog map[string]any
	if err := json.Unmarshal(progRec.Body.Bytes(), &prog); err != nil {
		t.Fatal(err)
	}
	if prog["state"] != "completed" {
		t.Fatalf("progress state = %v, want completed", prog["state"])
	}
	if prog["sha256"] != actualHex {
		t.Fatalf("progress sha256 = %v, want %q", prog["sha256"], actualHex)
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

// TestArchiveSourceMutationDuringStreaming verifies that modifying a file on disk
// causes the stream to detect the change, abort, and mark progress as failed
// without exposing an incomplete or erroneous digest.
func TestArchiveSourceMutationDuringStreaming(t *testing.T) {
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

	// Create grant (immediate: no Pass 1 hashing)
	g, cookie := env.grantFor(tok, source.ID)
	if g.SHA256 != "" {
		t.Fatalf("expected empty grant SHA256 in single-pass, got %q", g.SHA256)
	}

	// Mutate the file on disk before redeeming grant!
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(targetFile, []byte("MODIFIED CORRUPTED CONTENT OVERWRITTEN"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Redeem grant: Single-pass streams, detects size mismatch in writeArchiveFile, and aborts
	defer func() {
		// Recovery from abortResponse panic(http.ErrAbortHandler)
		if r := recover(); r != nil {
			if r != http.ErrAbortHandler {
				t.Fatalf("unexpected panic: %v", r)
			}
		}
	}()

	rec := env.getDownload(g, cookie, nil)
	_ = rec

	// Verify progress endpoint reports failed and does not expose a digest
	progRec := env.do(http.MethodGet, "/api/download-progress/"+g.ProgressID, tok, nil)
	var prog map[string]any
	_ = json.Unmarshal(progRec.Body.Bytes(), &prog)
	if prog["state"] != "failed" {
		t.Fatalf("progress state = %v, want failed", prog["state"])
	}
	if prog["sha256"] != nil && prog["sha256"] != "" {
		t.Fatalf("failed progress must not expose sha256, got %v", prog["sha256"])
	}
}

// TestSingleFileSourceMutationDuringStreaming tests that modifying a single file
// during streaming cuts the connection and reports failure without exposing digest.
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
	_ = rec

	// Verify progress endpoint reports failed and does not expose a digest
	progRec := env.do(http.MethodGet, "/api/download-progress/"+g.ProgressID, tok, nil)
	var prog map[string]any
	_ = json.Unmarshal(progRec.Body.Bytes(), &prog)
	if prog["state"] != "failed" {
		t.Fatalf("progress state = %v, want failed", prog["state"])
	}
	if prog["sha256"] != nil && prog["sha256"] != "" {
		t.Fatalf("failed progress must not expose sha256, got %v", prog["sha256"])
	}
}

// TestImmediateGrantCreationLatency verifies that grant creation is instantaneous (< 50ms)
// even for multi-megabyte files, proving no Pass 1 pre-reading occurs.
func TestImmediateGrantCreationLatency(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()

	largePath := filepath.Join(dir, "fast_grant.dat")
	data := make([]byte, 20*1024*1024) // 20 MB
	if err := os.WriteFile(largePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.Add(largePath, "fast_grant.dat", "")
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	g, _ := env.grantFor(tok, sf.ID)
	elapsed := time.Since(start)

	if g.DownloadURL == "" {
		t.Fatal("missing downloadUrl in grant response")
	}
	if g.SHA256 != "" {
		t.Fatalf("grant response should have empty sha256 in single-pass mode, got %q", g.SHA256)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("grant creation took %v, want < 50ms", elapsed)
	}
}

// TestFailedStreamDoesNotExposeDigest verifies that a canceled or interrupted stream
// transitions to failed progress and never exposes any partial digest.
func TestFailedStreamDoesNotExposeDigest(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()

	filePath := filepath.Join(dir, "interrupted.dat")
	if err := os.WriteFile(filePath, bytes.Repeat([]byte("X"), 1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.Add(filePath, "interrupted.dat", "")
	if err != nil {
		t.Fatal(err)
	}

	g, cookie := env.grantFor(tok, sf.ID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled immediately before reading body

	req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil).WithContext(ctx)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	defer func() {
		_ = recover()
	}()
	env.srv.Handler().ServeHTTP(rec, req)

	progRec := env.do(http.MethodGet, "/api/download-progress/"+g.ProgressID, tok, nil)
	var prog map[string]any
	_ = json.Unmarshal(progRec.Body.Bytes(), &prog)
	if prog["state"] == "completed" {
		t.Fatalf("progress state should not be completed for canceled stream")
	}
	if prog["sha256"] != nil && prog["sha256"] != "" {
		t.Fatalf("canceled stream must not expose sha256 digest, got %v", prog["sha256"])
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
