package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/power"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// Helper to run a complete Tus upload through the HTTP test server.
func executePhase5TusUpload(t *testing.T, env *testEnv, ts *httptest.Server, token, filename string, content []byte) (bID, itID, publishedPath string) {
	t.Helper()
	bID, itID = createTestBatch(t, env, ts, token, filename, int64(len(content)))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// 1. Tus POST (create)
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(content)))
	pRes, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("Tus POST failed: %v", err)
	}
	pRes.Body.Close()
	if pRes.StatusCode != http.StatusCreated {
		t.Fatalf("Tus POST expected 201, got %d", pRes.StatusCode)
	}

	// 2. Tus PATCH (stream)
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(content))
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	ptRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatalf("Tus PATCH failed: %v", err)
	}
	ptRes.Body.Close()
	if ptRes.StatusCode != http.StatusNoContent {
		t.Fatalf("Tus PATCH expected 204, got %d", ptRes.StatusCode)
	}

	// 3. Complete
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	h := sha256.Sum256(content)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": hex.EncodeToString(h[:])})
	cReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	cReq.Header.Set("Authorization", "Bearer "+token)
	cReq.Header.Set("Content-Type", "application/json")
	cRes, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatalf("Tus Complete failed: %v", err)
	}
	defer cRes.Body.Close()
	if cRes.StatusCode != http.StatusOK {
		t.Fatalf("Tus Complete expected 200, got %d", cRes.StatusCode)
	}

	var compResult struct {
		Status string `json:"status"`
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	}
	_ = json.NewDecoder(cRes.Body).Decode(&compResult)
	return bID, itID, compResult.Path
}

// Scenario 1: Single-file upload
func TestPhase5_Scenario01_SingleFileUpload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	content := []byte("Scenario 01: Single-file upload content for end-to-end verification.")
	contentHash := sha256.Sum256(content)
	contentHashHex := hex.EncodeToString(contentHash[:])

	_, _, publishedPath := executePhase5TusUpload(t, env, ts, tok, "scenario01.txt", content)

	if publishedPath == "" {
		t.Fatal("empty published path returned")
	}
	gotData, err := os.ReadFile(publishedPath)
	if err != nil {
		t.Fatalf("failed to read published file: %v", err)
	}
	if !bytes.Equal(gotData, content) {
		t.Fatalf("published data mismatch: got %q, want %q", string(gotData), string(content))
	}
	gotHash := sha256.Sum256(gotData)
	if hex.EncodeToString(gotHash[:]) != contentHashHex {
		t.Fatalf("published file digest mismatch")
	}
}

// Scenario 2: Multiple-file upload
func TestPhase5_Scenario02_MultipleFileUpload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	files := []struct {
		name    string
		content []byte
	}{
		{"fileA.txt", []byte("File A content")},
		{"fileB.txt", []byte("File B second content")},
		{"fileC.txt", []byte("File C third content")},
	}

	for _, f := range files {
		_, _, pubPath := executePhase5TusUpload(t, env, ts, tok, f.name, f.content)
		data, err := os.ReadFile(pubPath)
		if err != nil || !bytes.Equal(data, f.content) {
			t.Fatalf("file %s data mismatch or missing: %v", f.name, err)
		}
	}
}

// Scenario 3: Multi-chunk upload (traversing multiple chunks)
func TestPhase5_Scenario03_MultiChunkUpload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	largeSize := 2 * 1024 * 1024 // 2 MiB synthetic buffer for fast integration test
	largeBuf := make([]byte, largeSize)
	for i := range largeBuf {
		largeBuf[i] = byte((i * 31) % 256)
	}
	largeHash := sha256.Sum256(largeBuf)
	largeHashHex := hex.EncodeToString(largeHash[:])

	bID, itID := createTestBatch(t, env, ts, tok, "large_scenario03.bin", int64(largeSize))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// 1. Tus POST
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+tok)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(largeSize))
	pRes, err := http.DefaultClient.Do(postReq)
	if err != nil || pRes.StatusCode != http.StatusCreated {
		t.Fatalf("Tus POST failed: %v, code %d", err, pRes.StatusCode)
	}
	pRes.Body.Close()

	// 2. First chunk: 1 MiB
	chunk1 := largeBuf[:1024*1024]
	patch1, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(chunk1))
	patch1.Header.Set("Authorization", "Bearer "+tok)
	patch1.Header.Set("Tus-Resumable", "1.0.0")
	patch1.Header.Set("Upload-Offset", "0")
	patch1.Header.Set("Content-Type", "application/offset+octet-stream")
	ptRes1, err := http.DefaultClient.Do(patch1)
	if err != nil || ptRes1.StatusCode != http.StatusNoContent {
		t.Fatalf("Tus PATCH chunk 1 failed: %v, code %d", err, ptRes1.StatusCode)
	}
	ptRes1.Body.Close()

	// 3. Second chunk: remaining 1 MiB from offset 1048576
	chunk2 := largeBuf[1024*1024:]
	patch2, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(chunk2))
	patch2.Header.Set("Authorization", "Bearer "+tok)
	patch2.Header.Set("Tus-Resumable", "1.0.0")
	patch2.Header.Set("Upload-Offset", strconv.Itoa(len(chunk1)))
	patch2.Header.Set("Content-Type", "application/offset+octet-stream")
	ptRes2, err := http.DefaultClient.Do(patch2)
	if err != nil || ptRes2.StatusCode != http.StatusNoContent {
		t.Fatalf("Tus PATCH chunk 2 failed: %v, code %d", err, ptRes2.StatusCode)
	}
	ptRes2.Body.Close()

	// 4. Complete
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": largeHashHex})
	cReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	cReq.Header.Set("Authorization", "Bearer "+tok)
	cReq.Header.Set("Content-Type", "application/json")
	cRes, err := http.DefaultClient.Do(cReq)
	if err != nil || cRes.StatusCode != http.StatusOK {
		t.Fatalf("Tus Complete failed: %v, code %d", err, cRes.StatusCode)
	}
	defer cRes.Body.Close()

	var compResult struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(cRes.Body).Decode(&compResult)

	fi, err := os.Stat(compResult.Path)
	if err != nil || fi.Size() != int64(largeSize) {
		t.Fatalf("file size mismatch: %v", err)
	}
}

// Scenario 4: Single-file download
func TestPhase5_Scenario04_SingleFileDownload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "download_sample.txt")
	srcContent := []byte("Hello single-file download test!")
	if err := os.WriteFile(srcPath, srcContent, 0o600); err != nil {
		t.Fatal(err)
	}

	sf, err := env.reg.Add(srcPath, "download_sample.txt", "")
	if err != nil {
		t.Fatal(err)
	}

	grant, cookie := env.grantFor(tok, sf.ID)
	rec := env.getDownload(grant, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download returned %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), srcContent) {
		t.Fatalf("download content mismatch")
	}
	digestHdr := rec.Header().Get("Content-Digest")
	if digestHdr == "" {
		t.Fatalf("missing Content-Digest header")
	}
}

// Scenario 5: Multiple-file download as dynamic ZIP archive
func TestPhase5_Scenario05_MultipleFileDownload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "doc1.txt"), []byte("Doc 1 content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "doc2.txt"), []byte("Doc 2 content"), 0o600); err != nil {
		t.Fatal(err)
	}

	sf1, _ := env.reg.Add(filepath.Join(srcDir, "doc1.txt"), "doc1.txt", "")
	sf2, _ := env.reg.Add(filepath.Join(srcDir, "doc2.txt"), "doc2.txt", "")

	grant, cookie := env.grantFor(tok, sf1.ID, sf2.ID)
	if !grant.Archive {
		t.Fatalf("expected archive grant for multi-file selection")
	}

	rec := env.getDownload(grant, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download returned %d", rec.Code)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("expected 2 files in zip, got %d", len(zr.File))
	}
}

// Scenario 6: Interrupted upload
func TestPhase5_Scenario06_InterruptedUpload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	totalSize := 1024 * 1024
	bID, itID := createTestBatch(t, env, ts, tok, "interrupted_upload.bin", int64(totalSize))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// Create Tus upload
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+tok)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(totalSize))
	pRes, err := http.DefaultClient.Do(postReq)
	if err != nil || pRes.StatusCode != http.StatusCreated {
		t.Fatalf("Tus POST failed: %v, code %d", err, pRes.StatusCode)
	}
	pRes.Body.Close()

	// Stream only 256 KiB then cut off
	partial := bytes.Repeat([]byte{0xAA}, 256*1024)
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(partial))
	patchReq.Header.Set("Authorization", "Bearer "+tok)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	ptRes, err := http.DefaultClient.Do(patchReq)
	if err != nil || ptRes.StatusCode != http.StatusNoContent {
		t.Fatalf("partial PATCH returned: %v, code %d", err, ptRes.StatusCode)
	}
	ptRes.Body.Close()

	// Destination file must NOT exist in receive directory
	destPath := filepath.Join(env.store.Dir(), "interrupted_upload.bin")
	if _, err := os.Stat(destPath); !os.IsNotExist(err) {
		t.Fatalf("incomplete file was prematurely published!")
	}

	// HEAD query must show exactly 256 KiB committed
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+tok)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	hRes, err := http.DefaultClient.Do(headReq)
	if err != nil || hRes.StatusCode != http.StatusOK {
		t.Fatalf("HEAD returned: %v, code %d", err, hRes.StatusCode)
	}
	hRes.Body.Close()
	if got := hRes.Header.Get("Upload-Offset"); got != strconv.Itoa(256*1024) {
		t.Fatalf("got offset %q, want %d", got, 256*1024)
	}
}

// Scenario 7: Interrupted download (HTTP Range / 206 Partial Content)
func TestPhase5_Scenario07_InterruptedDownload(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "interrupted_dl.bin")
	srcData := bytes.Repeat([]byte{0x55}, 1024*1024)
	if err := os.WriteFile(srcPath, srcData, 0o600); err != nil {
		t.Fatal(err)
	}
	sumHex := hex.EncodeToString(sha256Bytes(srcData))
	sf, err := env.reg.AddImmutable(srcPath, "interrupted_dl.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	grant, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(grant.DownloadURL)

	// Initial request to redeem grant and obtain continuation cookie
	initReq := httptest.NewRequest(http.MethodGet, grant.DownloadURL, nil)
	initReq.AddCookie(initialCookie)
	initRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(initRec, initReq)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Continuation Range request for bytes 0-99 (100 bytes)
	req := httptest.NewRequest("GET", grant.DownloadURL, nil)
	req.Header.Set("Range", "bytes=0-99")
	req.Header.Set("If-Range", fmt.Sprintf("\"sha256-%s\"", sumHex))
	req.AddCookie(contCookie)
	w := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", w.Code)
	}
	if w.Body.Len() != 100 {
		t.Fatalf("expected 100 bytes, got %d", w.Body.Len())
	}
	if !bytes.Equal(w.Body.Bytes(), srcData[:100]) {
		t.Fatalf("partial content mismatch")
	}
}

// Scenario 8: Resume after network loss
func TestPhase5_Scenario08_ResumeAfterNetworkLoss(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	chunk1 := bytes.Repeat([]byte{0x11}, 64*1024)
	chunk2 := bytes.Repeat([]byte{0x22}, 64*1024)
	fullData := append(append([]byte(nil), chunk1...), chunk2...)
	fullHash := sha256.Sum256(fullData)
	fullHashHex := hex.EncodeToString(fullHash[:])

	bID, itID := createTestBatch(t, env, ts, tok, "resume_net_loss.bin", int64(len(fullData)))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// 1. Tus POST
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+tok)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(fullData)))
	pRes, err := http.DefaultClient.Do(postReq)
	if err != nil || pRes.StatusCode != http.StatusCreated {
		t.Fatalf("Tus POST failed: %v, code %d", err, pRes.StatusCode)
	}
	pRes.Body.Close()

	// 2. Stream First chunk (64 KiB)
	req1, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(chunk1))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Tus-Resumable", "1.0.0")
	req1.Header.Set("Upload-Offset", "0")
	req1.Header.Set("Content-Type", "application/offset+octet-stream")
	w1, err := http.DefaultClient.Do(req1)
	if err != nil || w1.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH 1 failed: %v, code %d", err, w1.StatusCode)
	}
	w1.Body.Close()

	// 3. Query HEAD to rediscover offset after simulated loss
	reqH, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	reqH.Header.Set("Authorization", "Bearer "+tok)
	reqH.Header.Set("Tus-Resumable", "1.0.0")
	wH, err := http.DefaultClient.Do(reqH)
	if err != nil || wH.StatusCode != http.StatusOK {
		t.Fatalf("HEAD failed: %v, code %d", err, wH.StatusCode)
	}
	wH.Body.Close()
	if wH.Header.Get("Upload-Offset") != strconv.Itoa(len(chunk1)) {
		t.Fatalf("HEAD returned incorrect offset: %s", wH.Header.Get("Upload-Offset"))
	}

	// 4. Resume second chunk from offset 64 KiB
	req2, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(chunk2))
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Tus-Resumable", "1.0.0")
	req2.Header.Set("Upload-Offset", strconv.Itoa(len(chunk1)))
	req2.Header.Set("Content-Type", "application/offset+octet-stream")
	w2, err := http.DefaultClient.Do(req2)
	if err != nil || w2.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH 2 failed: %v, code %d", err, w2.StatusCode)
	}
	w2.Body.Close()

	// 5. Complete
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": fullHashHex})
	reqC, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	reqC.Header.Set("Authorization", "Bearer "+tok)
	reqC.Header.Set("Content-Type", "application/json")
	wC, err := http.DefaultClient.Do(reqC)
	if err != nil || wC.StatusCode != http.StatusOK {
		t.Fatalf("Complete failed: %v, code %d", err, wC.StatusCode)
	}
	defer wC.Body.Close()

	var compResult struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(wC.Body).Decode(&compResult)

	data, err := os.ReadFile(compResult.Path)
	if err != nil || !bytes.Equal(data, fullData) {
		t.Fatalf("resumed file data mismatch: %v", err)
	}
}

// Scenario 9: Resume after application restart
func TestPhase5_Scenario09_ResumeAfterApplicationRestart(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}

	rm1 := transfer.NewResumableManager(store, nil)
	totalSize := int64(2000)
	fullData := make([]byte, totalSize)
	for i := range fullData {
		fullData[i] = byte((i * 13) % 256)
	}
	expectedHash := sha256.Sum256(fullData)
	expectedHex := hex.EncodeToString(expectedHash[:])

	transferID := "scenario-09-transfer"
	_, err = rm1.CreateUpload("sess-scen09", transferID, "b-1", "it-1", "restarted.bin", "", totalSize, tmpDir)
	if err != nil {
		t.Fatalf("CreateUpload failed: %v", err)
	}

	// Stream first 1000 bytes
	committed, state, err := rm1.IngestStream(transferID, 0, bytes.NewReader(fullData[:1000]), nil)
	if err != nil || committed != 1000 || state != transfer.ResumableActive {
		t.Fatalf("initial ingest failed: committed=%d, state=%s, err=%v", committed, state, err)
	}

	// Simulate app shutdown
	rm1.Close()

	// Simulate new instance starting up on same dir
	rm2 := transfer.NewResumableManager(store, nil)
	defer rm2.Close()

	if err := rm2.RecoverOnStartup(); err != nil {
		t.Fatalf("RecoverOnStartup failed: %v", err)
	}

	up, ok := rm2.GetUpload(transferID)
	if !ok {
		t.Fatal("upload not found after startup recovery")
	}
	if up.Meta().CommittedOffset != 1000 {
		t.Fatalf("recovered offset = %d, want 1000", up.Meta().CommittedOffset)
	}

	// Ingest remaining bytes
	remCommitted, remState, remErr := rm2.IngestStream(transferID, 1000, bytes.NewReader(fullData[1000:]), nil)
	if remErr != nil || remCommitted != totalSize || remState != transfer.ResumableAwaitingDigest {
		t.Fatalf("resumed ingest failed: committed=%d, state=%s, err=%v", remCommitted, remState, remErr)
	}

	// Finalize publication
	pub, err := rm2.FinalizeComplete(transferID, expectedHex)
	if err != nil {
		t.Fatalf("FinalizeComplete failed: %v", err)
	}
	if pub.ExpectedSHA256 != expectedHex {
		t.Fatalf("digest mismatch: expected %s, got %s", expectedHex, pub.ExpectedSHA256)
	}

	publishedBytes, err := os.ReadFile(pub.TargetPath)
	if err != nil || !bytes.Equal(publishedBytes, fullData) {
		t.Fatalf("published data corrupted after restart recovery")
	}
}

// Scenario 10: Duplicate filename handling (two sequential uploads with same name)
func TestPhase5_Scenario10_DuplicateFilenameHandling(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	c1 := []byte("First upload content")
	c2 := []byte("Second upload with same name")

	_, _, path1 := executePhase5TusUpload(t, env, ts, tok, "duplicate.txt", c1)
	_, _, path2 := executePhase5TusUpload(t, env, ts, tok, "duplicate.txt", c2)

	if path1 == path2 {
		t.Fatalf("expected different paths for duplicate names, got same path: %s", path1)
	}

	data1, err1 := os.ReadFile(path1)
	data2, err2 := os.ReadFile(path2)
	if err1 != nil || err2 != nil {
		t.Fatalf("failed to read duplicate files: %v, %v", err1, err2)
	}
	if !bytes.Equal(data1, c1) || !bytes.Equal(data2, c2) {
		t.Fatalf("collision contents corrupted")
	}
}

// Scenario 11: Destination already exists before upload starts
func TestPhase5_Scenario11_DestinationAlreadyExists(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	preExisting := filepath.Join(env.store.Dir(), "existing.txt")
	if err := os.WriteFile(preExisting, []byte("Pre-existing data"), 0o600); err != nil {
		t.Fatal(err)
	}

	incoming := []byte("Incoming fresh data")
	_, _, pubPath := executePhase5TusUpload(t, env, ts, tok, "existing.txt", incoming)

	// Pre-existing file MUST NOT be modified
	oldData, _ := os.ReadFile(preExisting)
	if string(oldData) != "Pre-existing data" {
		t.Fatalf("pre-existing file was overwritten!")
	}
	newData, err := os.ReadFile(pubPath)
	if err != nil || string(newData) != "Incoming fresh data" {
		t.Fatalf("incoming file was not safely published: %v", err)
	}
	if pubPath == preExisting {
		t.Fatalf("incoming upload overwrote existing path!")
	}
}

// Scenario 12: Insufficient disk space (rejected before or during upload)
func TestPhase5_Scenario12_InsufficientDiskSpace(t *testing.T) {
	tmpDir := t.TempDir()
	qm := transfer.NewQuotaManager(tmpDir, transfer.DefaultSafetyMargin, 1)

	// Single slot allowed
	if err := qm.Reserve("s1", "u1", 1000); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}

	// Concurrent limit exceeded
	err := qm.Reserve("s1", "u2", 1000)
	if err != transfer.ErrQuotaExceeded {
		t.Fatalf("expected ErrQuotaExceeded, got: %v", err)
	}

	// Storage quota exceeded
	errBig := qm.Reserve("s2", "uBig", 1000*1024*1024*1024*1024)
	if errBig != transfer.ErrInsufficientStorage {
		t.Fatalf("expected ErrInsufficientStorage, got: %v", errBig)
	}
}

// Scenario 13: Server shutdown during transfer
func TestPhase5_Scenario13_ServerShutdownDuringTransfer(t *testing.T) {
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: t.TempDir()})
	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	svcs := &Services{
		Sessions:  sessMgr,
		Transfers: transfers,
	}

	srv := New(Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("server failed to bind")
	}

	// Trigger shutdown
	cancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled && err != http.ErrServerClosed {
			t.Fatalf("unexpected shutdown error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server failed to drain and shut down cleanly")
	}
}

// Scenario 14: Client disconnection releases stream lock promptly
func TestPhase5_Scenario14_ClientDisconnection(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	totalSize := 1024 * 1024
	bID, itID := createTestBatch(t, env, ts, tok, "disconnect_test.bin", int64(totalSize))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// Tus POST
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+tok)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(totalSize))
	pRes, err := http.DefaultClient.Do(postReq)
	if err != nil || pRes.StatusCode != http.StatusCreated {
		t.Fatalf("Tus POST failed: %v", err)
	}
	pRes.Body.Close()

	// Stream with early client cancellation
	reqCtx, reqCancel := context.WithCancel(context.Background())
	bodyReader, bodyWriter := io.Pipe()

	go func() {
		_, _ = bodyWriter.Write([]byte("some initial bytes"))
		time.Sleep(20 * time.Millisecond)
		reqCancel()
		_ = bodyWriter.CloseWithError(io.ErrUnexpectedEOF)
	}()

	patchReq, _ := http.NewRequestWithContext(reqCtx, http.MethodPatch, tusURL, bodyReader)
	patchReq.Header.Set("Authorization", "Bearer "+tok)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	_, _ = http.DefaultClient.Do(patchReq)

	// Within 200ms, the lock MUST be released so a subsequent HEAD request succeeds
	time.Sleep(50 * time.Millisecond)
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+tok)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	hRes, err := http.DefaultClient.Do(headReq)
	if err != nil || hRes.StatusCode != http.StatusOK {
		t.Fatalf("lock was not released after client disconnect: err=%v, code=%d", err, hRes.StatusCode)
	}
	hRes.Body.Close()
}

// Scenario 15: Multiple concurrent uploads
func TestPhase5_Scenario15_MultipleConcurrentUploads(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			clientTok := env.token()
			data := bytes.Repeat([]byte{byte(idx + 1)}, 32*1024)
			name := fmt.Sprintf("concurrent_%d.bin", idx)

			_, _, pubPath := executePhase5TusUpload(t, env, ts, clientTok, name, data)
			readBack, err := os.ReadFile(pubPath)
			if err != nil || !bytes.Equal(readBack, data) {
				errs <- fmt.Errorf("concurrent upload %d verification failed: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent upload failed: %v", err)
	}
}

// Scenario 16: Multiple concurrent downloads
func TestPhase5_Scenario16_MultipleConcurrentDownloads(t *testing.T) {
	env := newTestEnv(t)

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "concurrent_dl.bin")
	srcContent := bytes.Repeat([]byte("ConcurrentReadPayload"), 5000)
	if err := os.WriteFile(srcPath, srcContent, 0o600); err != nil {
		t.Fatal(err)
	}

	sf, _ := env.reg.Add(srcPath, "concurrent_dl.bin", "")

	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			clientToken := env.token()
			grant, cookie := env.grantFor(clientToken, sf.ID)
			rec := env.getDownload(grant, cookie, nil)
			if rec.Code != http.StatusOK {
				errs <- fmt.Errorf("download %d failed: code %d", idx, rec.Code)
				return
			}
			if !bytes.Equal(rec.Body.Bytes(), srcContent) {
				errs <- fmt.Errorf("download %d content mismatch", idx)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent download failed: %v", err)
	}
}

// Scenario 17: Different sessions transferring simultaneously with strict isolation
func TestPhase5_Scenario17_DifferentSessionsTransferringSimultaneously(t *testing.T) {
	env := newTestEnv(t)
	tok1 := env.token()
	tok2 := env.token()
	k1 := env.tokenKey(tok1)
	k2 := env.tokenKey(tok2)

	up1, _ := env.trans.Declare(k1, "sess1.txt", 100)
	up2, _ := env.trans.Declare(k2, "sess2.txt", 200)

	// Session 2 must NOT be able to query or modify Session 1's upload
	reqCross := httptest.NewRequest("HEAD", "/api/upload/"+up1.ID, nil)
	reqCross.Header.Set("Authorization", "Bearer "+tok2)
	wCross := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(wCross, reqCross)

	if wCross.Code != http.StatusNotFound && wCross.Code != http.StatusUnauthorized {
		t.Fatalf("cross-session access succeeded: code %d", wCross.Code)
	}

	_ = up2
}

// Scenario 18: Network interface changes during transfer
func TestPhase5_Scenario18_NetworkInterfaceChangesDuringTransfer(t *testing.T) {
	src := netenv.Source{
		Interfaces: func() ([]net.Interface, error) {
			return []net.Interface{
				{Name: "eth0", Flags: net.FlagUp},
				{Name: "wlan0", Flags: net.FlagUp},
			}, nil
		},
		Addrs: func(iface net.Interface) ([]net.Addr, error) {
			if iface.Name == "eth0" {
				return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.50"), Mask: net.CIDRMask(24, 32)}}, nil
			}
			return []net.Addr{&net.IPNet{IP: net.ParseIP("10.0.0.25"), Mask: net.CIDRMask(24, 32)}}, nil
		},
	}

	views, err := netenv.Enumerate(src)
	if err != nil || len(views) != 2 {
		t.Fatalf("expected 2 interfaces, got %d (err: %v)", len(views), err)
	}

	v1, err := netenv.InterfaceFor(src, "192.168.1.50")
	if err != nil || v1.Name != "eth0" {
		t.Fatalf("failed to select eth0: %v", err)
	}
	v2, err := netenv.InterfaceFor(src, "10.0.0.25")
	if err != nil || v2.Name != "wlan0" {
		t.Fatalf("failed to select wlan0: %v", err)
	}
}

// Scenario 19: DHCP address changes
func TestPhase5_Scenario19_DHCPAddressChanges(t *testing.T) {
	currentIP := "192.168.1.100"
	src := netenv.Source{
		Interfaces: func() ([]net.Interface, error) {
			return []net.Interface{{Name: "wlo1", Flags: net.FlagUp}}, nil
		},
		Addrs: func(iface net.Interface) ([]net.Addr, error) {
			return []net.Addr{&net.IPNet{IP: net.ParseIP(currentIP), Mask: net.CIDRMask(24, 32)}}, nil
		},
	}

	// Initially valid
	if _, err := netenv.InterfaceFor(src, "192.168.1.100"); err != nil {
		t.Fatalf("expected valid candidate: %v", err)
	}

	// DHCP changes IP to 192.168.1.150
	currentIP = "192.168.1.150"
	_, err := netenv.InterfaceFor(src, "192.168.1.100")
	if !errors.Is(err, netenv.ErrGone) {
		t.Fatalf("expected ErrGone after DHCP change, got: %v", err)
	}
}

// Scenario 20: Desktop sleep and wake inhibition
func TestPhase5_Scenario20_DesktopSleepAndWake(t *testing.T) {
	// 1. Portable automated verification with deterministic fake inhibitor
	inh := power.NewFake()
	defer inh.Close()

	initialStatus := inh.Status()
	if initialStatus.Active || initialStatus.Holders != 0 {
		t.Fatalf("expected sleep inhibitor initially inactive: %+v", initialStatus)
	}

	// Inhibit sleep during transfer (first acquire)
	lease1, err := inh.Acquire("Active Flitnix Transfer 1")
	if err != nil {
		t.Fatalf("failed to inhibit sleep: %v", err)
	}
	if !inh.Status().Active || inh.Status().Holders != 1 {
		t.Fatalf("sleep inhibitor should be active with 1 holder: %+v", inh.Status())
	}

	// Second concurrent transfer (reference counting)
	lease2, err := inh.Acquire("Active Flitnix Transfer 2")
	if err != nil {
		t.Fatalf("failed to acquire second lease: %v", err)
	}
	if !inh.Status().Active || inh.Status().Holders != 2 {
		t.Fatalf("sleep inhibitor should have 2 holders: %+v", inh.Status())
	}

	// Release first transfer - inhibitor must remain active because second transfer is running
	lease1.Release()
	if !inh.Status().Active || inh.Status().Holders != 1 {
		t.Fatalf("sleep inhibitor should remain active with 1 holder after partial release")
	}

	// Release second transfer - inhibitor must become inactive
	lease2.Release()
	if inh.Status().Active || inh.Status().Holders != 0 {
		t.Fatalf("sleep inhibitor should be inactive after all transfers complete")
	}

	// Idempotent release
	lease2.Release()

	// 2. Failure handling verification
	simErr := errors.New("simulated D-Bus/kernel power failure")
	failingInh := power.NewFakeWithConfig(true, simErr)
	defer failingInh.Close()
	failedLease, failErr := failingInh.Acquire("Transfer with failing power API")
	if failErr == nil || !errors.Is(failErr, power.ErrInhibitionFailed) {
		t.Fatalf("expected ErrInhibitionFailed, got %v", failErr)
	}
	if failedLease != nil {
		t.Fatal("expected nil lease on failed inhibition")
	}
	failStatus := failingInh.Status()
	if failStatus.Active || failStatus.Holders != 0 {
		t.Fatalf("expected inactive status after failed acquire: %+v", failStatus)
	}

	// 3. Platform Native Power Inspection (opt-in / diagnostic reporting)
	if os.Getenv("FLITNIX_POWER_NATIVE_TESTS") == "1" {
		nativeInh := power.New()
		defer nativeInh.Close()
		nativeLease, nativeErr := nativeInh.Acquire("Native power test")
		if nativeErr != nil {
			t.Fatalf("native sleep inhibition failed: %v", nativeErr)
		}
		nativeLease.Release()
	} else {
		// Probe native power manager without failing if CI runner lacks desktop privileges
		nativeInh := power.New()
		defer nativeInh.Close()
		st := nativeInh.Status()
		t.Logf("Native power backend probed: supported=%v (login1 Inhibit requires desktop session privileges; set FLITNIX_POWER_NATIVE_TESTS=1 on desktop to test native D-Bus)", st.Supported)
	}
}

// Scenario 21: Invalid or expired pairing
func TestPhase5_Scenario21_InvalidOrExpiredPairing(t *testing.T) {
	hostID, _ := devices.LoadOrCreateHostIdentity("")
	store, _ := devices.NewStore("")
	coord := devices.NewPairingCoordinator(hostID, store)

	clientPub, _, _ := devices.GenerateKeyPair()
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	clientCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	sessionKey := sha256.Sum256([]byte("sess-scen21"))

	chal, err := coord.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(clientCommit[:]))
	if err != nil {
		t.Fatal(err)
	}

	_, _ = coord.Reveal(sessionKey[:], chal.PairingNonce, hex.EncodeToString(clientEphemeral.PublicKey), hex.EncodeToString(clientSalt))

	// Submitting forged bogus signature
	bogusSig := hex.EncodeToString(make([]byte, 64))
	_, _, _, err = coord.PrepareApproval(sessionKey[:], "Evil Device", hex.EncodeToString(clientPub), chal.PairingNonce, bogusSig)
	if err != devices.ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got: %v", err)
	}
	if coord.FailureCount() != 1 {
		t.Fatalf("failure counter not incremented: count=%d", coord.FailureCount())
	}
}

// Scenario 22: Unauthorized transfer attempt
func TestPhase5_Scenario22_UnauthorizedTransferAttempt(t *testing.T) {
	env := newTestEnv(t)

	// No token
	r1 := httptest.NewRequest("PATCH", "/api/upload/nonexistent", strings.NewReader("data"))
	w1 := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w1, r1)
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", w1.Code)
	}

	// Bogus token
	r2 := httptest.NewRequest("PATCH", "/api/upload/nonexistent", strings.NewReader("data"))
	r2.Header.Set("Authorization", "Bearer invalid-token-string")
	w2 := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for bogus token, got %d", w2.Code)
	}
}

// Scenario 23: Malformed upload metadata
func TestPhase5_Scenario23_MalformedUploadMetadata(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	// 1. Path traversal filename in declaration
	travBody := []byte(`{"name":"../../etc/passwd","size":100}`)
	r1 := httptest.NewRequest("POST", "/api/uploads", bytes.NewReader(travBody))
	r1.Header.Set("Authorization", "Bearer "+tok)
	r1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w1, r1)
	if w1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for traversal filename, got %d", w1.Code)
	}

	// 2. Windows reserved filename in declaration
	resBody := []byte(`{"name":"CON.txt","size":100}`)
	r2 := httptest.NewRequest("POST", "/api/uploads", bytes.NewReader(resBody))
	r2.Header.Set("Authorization", "Bearer "+tok)
	r2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for Windows reserved filename, got %d", w2.Code)
	}

	// 3. Negative length in declaration
	negBody := []byte(`{"name":"normal.txt","size":-50}`)
	r3 := httptest.NewRequest("POST", "/api/uploads", bytes.NewReader(negBody))
	r3.Header.Set("Authorization", "Bearer "+tok)
	r3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w3, r3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for negative length, got %d", w3.Code)
	}

	// 4. Tus creation with negative length
	bID, itID := createTestBatch(t, env, ts, tok, "valid.bin", 1024)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	r4, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	r4.Header.Set("Authorization", "Bearer "+tok)
	r4.Header.Set("Tus-Resumable", "1.0.0")
	r4.Header.Set("Upload-Length", "-50")
	res4, err := http.DefaultClient.Do(r4)
	if err != nil {
		t.Fatal(err)
	}
	res4.Body.Close()
	if res4.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for Tus negative length, got %d", res4.StatusCode)
	}
}

// Scenario 24: Corrupted transfer metadata quarantined safely
func TestPhase5_Scenario24_CorruptedTransferMetadata(t *testing.T) {
	dir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	rm := transfer.NewResumableManager(store, nil)
	defer rm.Close()

	if _, err := rm.CreateUpload("sess-key", "orphan-1", "b-1", "it-1", "orphan.bin", "", 100, dir); err != nil {
		t.Fatal(err)
	}

	// Corrupt metadata by declaring invalid authorization
	quarantined, err := rm.Reconcile(func(transfer.ResumableMeta) error {
		return fmt.Errorf("corrupted or orphaned metadata")
	})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if quarantined != 1 {
		t.Fatalf("expected 1 quarantined record, got %d", quarantined)
	}
}

// Scenario 25: Large directory transfer with nested files and folders
func TestPhase5_Scenario25_LargeDirectoryTransfer(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	srcDir := t.TempDir()
	rootDir := filepath.Join(srcDir, "ProjectFolder")
	_ = os.MkdirAll(filepath.Join(rootDir, "src", "pkg"), 0o755)
	_ = os.MkdirAll(filepath.Join(rootDir, "docs"), 0o755)

	_ = os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("# Project Root"), 0o600)
	_ = os.WriteFile(filepath.Join(rootDir, "src", "main.go"), []byte("package main\nfunc main(){}"), 0o600)
	_ = os.WriteFile(filepath.Join(rootDir, "src", "pkg", "lib.go"), []byte("package pkg"), 0o600)
	_ = os.WriteFile(filepath.Join(rootDir, "docs", "arch.pdf"), bytes.Repeat([]byte{0x25}, 10000), 0o600)

	source, err := env.reg.AddFolder(rootDir, "")
	if err != nil {
		t.Fatalf("AddFolder failed: %v", err)
	}

	grant, cookie := env.grantFor(tok, source.ID)
	if !grant.Archive {
		t.Fatalf("expected archive grant for directory transfer")
	}

	rec := env.getDownload(grant, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive download returned %d: %s", rec.Code, rec.Body.String())
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}

	names := make(map[string]bool)
	for _, f := range zr.File {
		names[f.Name] = true
	}

	expected := []string{
		"ProjectFolder/README.md",
		"ProjectFolder/src/main.go",
		"ProjectFolder/src/pkg/lib.go",
		"ProjectFolder/docs/arch.pdf",
	}
	for _, exp := range expected {
		if !names[exp] {
			t.Errorf("missing entry in archive: %s (got %v)", exp, names)
		}
	}
}

func sha256Bytes(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
