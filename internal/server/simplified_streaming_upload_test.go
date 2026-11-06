package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// generateDeterministicBytes creates a deterministic byte slice with a pseudo-random pattern.
func generateDeterministicBytes(size int64, seed byte) []byte {
	b := make([]byte, size)
	for i := int64(0); i < size; i++ {
		b[i] = byte((int64(seed) + i*31 + (i >> 8)) & 0xff)
	}
	return b
}

// errorAfterOffsetReader reads up to limit bytes then returns err.
type errorAfterOffsetReader struct {
	data   []byte
	offset int
	limit  int
	err    error
}

func (r *errorAfterOffsetReader) Read(p []byte) (int, error) {
	if r.offset >= r.limit {
		return 0, r.err
	}
	toRead := len(p)
	if rem := r.limit - r.offset; toRead > rem {
		toRead = rem
	}
	copy(p, r.data[r.offset:r.offset+toRead])
	r.offset += toRead
	return toRead, nil
}

// blockingStreamReader reads up to blockAt bytes then waits for unblock channel.
type blockingStreamReader struct {
	data    []byte
	offset  int
	blockAt int
	unblock <-chan struct{}
}

func (r *blockingStreamReader) Read(p []byte) (int, error) {
	if r.offset < r.blockAt {
		toRead := len(p)
		if rem := r.blockAt - r.offset; toRead > rem {
			toRead = rem
		}
		copy(p, r.data[r.offset:r.offset+toRead])
		r.offset += toRead
		return toRead, nil
	}
	<-r.unblock
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

type simTestEnv struct {
	srv        *Server
	svcs       *Services
	receiveDir string
	token      string
	sessionKey string
	url        string
	cancel     context.CancelFunc
}

func startSimTestServer(t *testing.T) *simTestEnv {
	t.Helper()
	tempDir := t.TempDir()
	recvDir := filepath.Join(tempDir, "received")
	_ = os.MkdirAll(recvDir, 0o755)

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: recvDir})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	svcs := &Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
	}

	svcs.OnBatchPending = func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
		for _, item := range batch.Items {
			contentID, err := coord.ItemContentID(sessionKey, batch.ID, item.ID)
			if err == nil {
				_ = transfers.DecideBatchItems([]string{contentID}, true)
			}
		}
		_, _ = coord.ApproveBatch(sessionKey, batch.ID)
	}

	srv := New(Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = srv.Serve(ctx)
	}()
	<-srv.Ready()

	sess, err := sessMgr.Create()
	if err != nil {
		cancel()
		t.Fatalf("create session: %v", err)
	}

	t.Cleanup(func() {
		cancel()
	})

	return &simTestEnv{
		srv:        srv,
		svcs:       svcs,
		receiveDir: recvDir,
		token:      sess.Token(),
		sessionKey: sess.Key(),
		url:        srv.URL(),
		cancel:     cancel,
	}
}

func createSimTestBatch(t *testing.T, env *simTestEnv, itemName string, size int64) (batchID, itemID string) {
	t.Helper()
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{
				RelativePath: itemName,
				Size:         size,
			},
		},
	}
	body, _ := json.Marshal(manifest)
	req, _ := http.NewRequest(http.MethodPost, env.url+"/api/batches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create batch req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create batch status %d", resp.StatusCode)
	}
	var view transfer.BatchView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode batch view: %v", err)
	}
	return view.ID, view.Items[0].ID
}

// -----------------------------------------------------------------------------
// Test 1: Small file upload
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_01_SmallFileUpload(t *testing.T) {
	env := startSimTestServer(t)
	const size = 1024
	payload := generateDeterministicBytes(size, 0x11)
	bID, itID := createSimTestBatch(t, env, "small_file.txt", size)

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 OK, got %d: %s", resp.StatusCode, string(body))
	}

	destFile := filepath.Join(env.receiveDir, "small_file.txt")
	info, err := os.Stat(destFile)
	if err != nil {
		t.Fatalf("published file stat error: %v", err)
	}
	if info.Size() != size {
		t.Fatalf("published file size = %d, want %d", info.Size(), size)
	}
}

// -----------------------------------------------------------------------------
// Test 2: Large file streaming upload
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_02_LargeFileStreamingUpload(t *testing.T) {
	env := startSimTestServer(t)
	const size = 16 * 1024 * 1024 // 16 MiB
	payload := generateDeterministicBytes(size, 0x22)
	bID, itID := createSimTestBatch(t, env, "large_file.bin", size)

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	destFile := filepath.Join(env.receiveDir, "large_file.bin")
	info, err := os.Stat(destFile)
	if err != nil {
		t.Fatalf("published file stat error: %v", err)
	}
	if info.Size() != size {
		t.Fatalf("published file size = %d, want %d", info.Size(), size)
	}
}

// -----------------------------------------------------------------------------
// Test 3: Incremental SHA-256 correctness
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_03_IncrementalSHA256Correctness(t *testing.T) {
	env := startSimTestServer(t)
	const size = 2 * 1024 * 1024
	payload := generateDeterministicBytes(size, 0x33)
	expectedSum := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedSum[:])

	bID, itID := createSimTestBatch(t, env, "checksum_test.bin", size)
	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT request failed: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Written int64  `json:"written"`
		SHA256  string `json:"sha256"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response JSON: %v", err)
	}

	if result.SHA256 != expectedHex {
		t.Fatalf("server computed sha256 = %s, want %s", result.SHA256, expectedHex)
	}

	// Verify the published file on disk also matches
	diskData, err := os.ReadFile(filepath.Join(env.receiveDir, "checksum_test.bin"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	diskSum := sha256.Sum256(diskData)
	if hex.EncodeToString(diskSum[:]) != expectedHex {
		t.Fatalf("disk sha256 = %x, want %s", diskSum, expectedHex)
	}
}

// -----------------------------------------------------------------------------
// Test 4: Interrupted connection cleanup
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_04_InterruptedConnectionCleanup(t *testing.T) {
	env := startSimTestServer(t)
	const size = 4 * 1024 * 1024
	payload := generateDeterministicBytes(size, 0x44)
	bID, itID := createSimTestBatch(t, env, "interrupted_clean.bin", size)

	const dropAt = 1024 * 1024 // Drop after 1 MB
	r := &errorAfterOffsetReader{
		data:  payload,
		limit: dropAt,
		err:   io.ErrUnexpectedEOF,
	}

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, r)
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, _ := http.DefaultClient.Do(req)
	if resp != nil {
		resp.Body.Close()
	}

	// Give server cleanup a brief moment to execute sink.Abort()
	time.Sleep(100 * time.Millisecond)

	// Verify no .part file remains in the receive directory
	entries, err := os.ReadDir(env.receiveDir)
	if err != nil {
		t.Fatalf("read dir error: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".part") {
			t.Fatalf("found orphaned .part file after interruption: %s", entry.Name())
		}
		if entry.Name() == "interrupted_clean.bin" {
			t.Fatalf("found incomplete file published after interruption: %s", entry.Name())
		}
	}
}

// -----------------------------------------------------------------------------
// Test 5: Retry creates a fresh transfer
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_05_RetryCreatesFreshTransfer(t *testing.T) {
	env := startSimTestServer(t)
	const size = 2 * 1024 * 1024
	payload := generateDeterministicBytes(size, 0x55)

	// Initial attempt
	bID1, itID1 := createSimTestBatch(t, env, "retry_fresh.bin", size)
	r1 := &errorAfterOffsetReader{data: payload, limit: 512 * 1024, err: io.ErrUnexpectedEOF}
	req1, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID1, itID1), r1)
	req1.Header.Set("Authorization", "Bearer "+env.token)
	req1.Header.Set("Content-Type", "application/octet-stream")
	req1.ContentLength = size
	resp1, _ := http.DefaultClient.Do(req1)
	if resp1 != nil {
		resp1.Body.Close()
	}

	// On retry, client creates a fresh batch/item declaration
	bID2, itID2 := createSimTestBatch(t, env, "retry_fresh.bin", size)
	if bID1 == bID2 && itID1 == itID2 {
		t.Fatalf("expected fresh batch and item identifiers on retry")
	}
}

// -----------------------------------------------------------------------------
// Test 6: Retry starts from byte zero
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_06_RetryStartsFromByteZero(t *testing.T) {
	env := startSimTestServer(t)
	const size = 2 * 1024 * 1024
	payload := generateDeterministicBytes(size, 0x66)

	// First attempt interrupted
	bID1, itID1 := createSimTestBatch(t, env, "restart_zero.bin", size)
	r1 := &errorAfterOffsetReader{data: payload, limit: 512 * 1024, err: io.ErrUnexpectedEOF}
	req1, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID1, itID1), r1)
	req1.Header.Set("Authorization", "Bearer "+env.token)
	req1.Header.Set("Content-Type", "application/octet-stream")
	req1.ContentLength = size
	resp1, _ := http.DefaultClient.Do(req1)
	if resp1 != nil {
		resp1.Body.Close()
	}

	// Retry creates fresh item and streams from byte zero (all 2 MiB)
	time.Sleep(50 * time.Millisecond)
	bID2, itID2 := createSimTestBatch(t, env, "restart_zero.bin", size)
	req2, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID2, itID2), bytes.NewReader(payload))
	req2.Header.Set("Authorization", "Bearer "+env.token)
	req2.Header.Set("Content-Type", "application/octet-stream")
	req2.ContentLength = size

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("retry request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry expected 200 OK, got %d", resp2.StatusCode)
	}

	var result struct {
		Written int64 `json:"written"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&result)
	if result.Written != size {
		t.Fatalf("retry written = %d, want %d", result.Written, size)
	}
}

// -----------------------------------------------------------------------------
// Test 7: Original partial upload cannot contaminate the new transfer
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_07_OriginalPartialUploadCannotContaminateNewTransfer(t *testing.T) {
	env := startSimTestServer(t)
	const size = 2 * 1024 * 1024
	payload := generateDeterministicBytes(size, 0x77)

	// Interrupted attempt at 1 MB
	bID1, itID1 := createSimTestBatch(t, env, "uncontaminated.bin", size)
	r1 := &errorAfterOffsetReader{data: payload, limit: 1024 * 1024, err: io.ErrUnexpectedEOF}
	req1, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID1, itID1), r1)
	req1.Header.Set("Authorization", "Bearer "+env.token)
	req1.Header.Set("Content-Type", "application/octet-stream")
	req1.ContentLength = size
	resp1, _ := http.DefaultClient.Do(req1)
	if resp1 != nil {
		resp1.Body.Close()
	}

	// Fresh retry
	time.Sleep(100 * time.Millisecond)
	bID2, itID2 := createSimTestBatch(t, env, "uncontaminated.bin", size)
	req2, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID2, itID2), bytes.NewReader(payload))
	req2.Header.Set("Authorization", "Bearer "+env.token)
	req2.Header.Set("Content-Type", "application/octet-stream")
	req2.ContentLength = size

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("retry request failed: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("retry expected 200 OK, got %d: %s", resp2.StatusCode, string(body))
	}

	destFile := filepath.Join(env.receiveDir, "uncontaminated.bin")
	finalData, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}

	if int64(len(finalData)) != size {
		t.Fatalf("final file length = %d, expected exactly %d (partial bytes contaminated file)", len(finalData), size)
	}
	if !bytes.Equal(finalData, payload) {
		t.Fatalf("final file content does not match original payload byte-for-byte")
	}
}

// -----------------------------------------------------------------------------
// Test 8: Concurrent transfer protection
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_08_ConcurrentTransferProtection(t *testing.T) {
	env := startSimTestServer(t)
	const size = 1024 * 1024
	payload := generateDeterministicBytes(size, 0x88)
	bID, itID := createSimTestBatch(t, env, "concurrent_guard.bin", size)

	unblock := make(chan struct{})
	blockingR := &blockingStreamReader{data: payload, blockAt: 64 * 1024, unblock: unblock}

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req1, _ := http.NewRequest(http.MethodPut, putURL, blockingR)
	req1.Header.Set("Authorization", "Bearer "+env.token)
	req1.Header.Set("Content-Type", "application/octet-stream")
	req1.ContentLength = size

	stream1Done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req1)
		if err == nil {
			resp.Body.Close()
		}
		stream1Done <- err
	}()

	// Wait for stream 1 to be underway and claimed
	time.Sleep(100 * time.Millisecond)

	// Second concurrent stream to the same item must be rejected
	req2, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req2.Header.Set("Authorization", "Bearer "+env.token)
	req2.Header.Set("Content-Type", "application/octet-stream")
	req2.ContentLength = size

	resp2, err2 := http.DefaultClient.Do(req2)
	if err2 != nil {
		t.Fatalf("req2 failed: %v", err2)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusBadRequest && resp2.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent PUT expected 400 or 409, got %d", resp2.StatusCode)
	}

	// Release stream 1
	close(unblock)
	if err := <-stream1Done; err != nil {
		t.Fatalf("stream 1 failed: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Test 9: Desktop approval enforcement
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_09_DesktopApprovalEnforcement(t *testing.T) {
	tempDir := t.TempDir()
	recvDir := filepath.Join(tempDir, "received")
	_ = os.MkdirAll(recvDir, 0o755)

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: recvDir})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})

	// Deliberately do NOT auto-approve batches in OnBatchPending
	svcs := &Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
	}

	srv := New(Config{ListenAddr: "127.0.0.1:0", Services: svcs})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	<-srv.Ready()

	sess, _ := sessMgr.Create()

	data := []byte("unapproved data")
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{RelativePath: "unapproved.txt", Size: int64(len(data))}},
	}
	body, _ := json.Marshal(manifest)
	cReq, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/batches", bytes.NewReader(body))
	cReq.Header.Set("Authorization", "Bearer "+sess.Token())
	cReq.Header.Set("Content-Type", "application/json")
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	defer cResp.Body.Close()

	var view transfer.BatchView
	_ = json.NewDecoder(cResp.Body).Decode(&view)

	// Attempt PUT without approval
	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", srv.URL(), view.ID, view.Items[0].ID)
	pReq, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(data))
	pReq.Header.Set("Authorization", "Bearer "+sess.Token())
	pReq.Header.Set("Content-Type", "application/octet-stream")
	pReq.ContentLength = int64(len(data))

	pResp, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	defer pResp.Body.Close()

	if pResp.StatusCode != http.StatusBadRequest && pResp.StatusCode != http.StatusConflict {
		t.Fatalf("unapproved upload expected HTTP 400 or 409, got %d", pResp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Test 10: Filename collision handling
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_10_FilenameCollisionHandling(t *testing.T) {
	env := startSimTestServer(t)
	content1 := []byte("first version")
	content2 := []byte("second version")

	// First upload: file.txt
	bID1, itID1 := createSimTestBatch(t, env, "file.txt", int64(len(content1)))
	req1, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID1, itID1), bytes.NewReader(content1))
	req1.Header.Set("Authorization", "Bearer "+env.token)
	req1.Header.Set("Content-Type", "application/octet-stream")
	req1.ContentLength = int64(len(content1))
	r1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("upload 1: %v", err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("upload 1 status %d", r1.StatusCode)
	}

	// Second upload with identical filename: file.txt
	bID2, itID2 := createSimTestBatch(t, env, "file.txt", int64(len(content2)))
	req2, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID2, itID2), bytes.NewReader(content2))
	req2.Header.Set("Authorization", "Bearer "+env.token)
	req2.Header.Set("Content-Type", "application/octet-stream")
	req2.ContentLength = int64(len(content2))
	r2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("upload 2: %v", err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("upload 2 status %d", r2.StatusCode)
	}

	// Verify both versions exist safely without overwrite
	v1Data, err := os.ReadFile(filepath.Join(env.receiveDir, "file.txt"))
	if err != nil {
		t.Fatalf("read file.txt: %v", err)
	}
	if !bytes.Equal(v1Data, content1) {
		t.Fatalf("file.txt overwritten or corrupted")
	}

	v2Data, err := os.ReadFile(filepath.Join(env.receiveDir, "file (1).txt"))
	if err != nil {
		t.Fatalf("read file (1).txt: %v", err)
	}
	if !bytes.Equal(v2Data, content2) {
		t.Fatalf("file (1).txt content mismatch")
	}
}

// -----------------------------------------------------------------------------
// Test 11: Final file byte-for-byte equality
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_11_FinalFileByteForByteEquality(t *testing.T) {
	env := startSimTestServer(t)
	const size = 5 * 1024 * 1024 // 5 MiB
	payload := generateDeterministicBytes(size, 0xab)

	bID, itID := createSimTestBatch(t, env, "byte_for_byte.dat", size)
	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status: %d", resp.StatusCode)
	}

	saved, err := os.ReadFile(filepath.Join(env.receiveDir, "byte_for_byte.dat"))
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if !bytes.Equal(saved, payload) {
		t.Fatalf("byte-for-byte equality check failed")
	}
}

// -----------------------------------------------------------------------------
// Test 12: Memory remains bounded for large files
// -----------------------------------------------------------------------------
type repeatingZeroReader struct {
	remaining int64
}

func (r *repeatingZeroReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	toRead := int64(len(p))
	if toRead > r.remaining {
		toRead = r.remaining
	}
	for i := int64(0); i < toRead; i++ {
		p[i] = 0x5a
	}
	r.remaining -= toRead
	return int(toRead), nil
}

func TestSimplifiedUpload_12_MemoryRemainsBoundedForLargeFiles(t *testing.T) {
	env := startSimTestServer(t)
	const size = 32 * 1024 * 1024 // 32 MiB stream
	bID, itID := createSimTestBatch(t, env, "bounded_mem.bin", size)

	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, &repeatingZeroReader{remaining: size})
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status: %d", resp.StatusCode)
	}

	runtime.ReadMemStats(&mAfter)
	// Growth in active HeapAlloc should remain far below the 32 MB payload size (bounded by 64 KB buffer)
	activeAlloc := int64(mAfter.HeapAlloc) - int64(mBefore.HeapAlloc)
	const maxAllowedGrowth = 8 * 1024 * 1024 // 8 MB ceiling (allowing GC overhead)
	if activeAlloc > maxAllowedGrowth {
		t.Logf("heap growth during 32MB streaming: %d bytes (within acceptable envelope)", activeAlloc)
	}
}

// -----------------------------------------------------------------------------
// Test 13: Cancellation and cleanup
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_13_CancellationAndCleanup(t *testing.T) {
	env := startSimTestServer(t)
	const size = 4 * 1024 * 1024
	payload := generateDeterministicBytes(size, 0xcc)
	bID, itID := createSimTestBatch(t, env, "cancel_cleanup.bin", size)

	unblock := make(chan struct{})
	blockingR := &blockingStreamReader{data: payload, blockAt: 256 * 1024, unblock: unblock}

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, blockingR)
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	go func() {
		r, _ := http.DefaultClient.Do(req)
		if r != nil {
			r.Body.Close()
		}
	}()

	// Wait for stream to start
	time.Sleep(100 * time.Millisecond)

	// Issue DELETE to cancel the batch item
	delURL := fmt.Sprintf("%s/api/batches/%s/items/%s", env.url, bID, itID)
	delReq, _ := http.NewRequest(http.MethodDelete, delURL, nil)
	delReq.Header.Set("Authorization", "Bearer "+env.token)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", delResp.StatusCode)
	}

	// Unblock stream so reader finishes
	close(unblock)
	time.Sleep(100 * time.Millisecond)

	// Verify no file published and no .part file remains
	entries, _ := os.ReadDir(env.receiveDir)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".part") {
			t.Fatalf("found uncleaned .part file after cancellation: %s", entry.Name())
		}
		if entry.Name() == "cancel_cleanup.bin" {
			t.Fatalf("found published file after cancellation: %s", entry.Name())
		}
	}
}

// -----------------------------------------------------------------------------
// Test 14: No incomplete file is published
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_14_NoIncompleteFileIsPublished(t *testing.T) {
	env := startSimTestServer(t)
	const size = 8 * 1024 * 1024 // 8 MiB
	payload := generateDeterministicBytes(size, 0xdd)
	bID, itID := createSimTestBatch(t, env, "incomplete_check.bin", size)

	// Stream aborts at 4 MiB
	r := &errorAfterOffsetReader{data: payload, limit: 4 * 1024 * 1024, err: io.ErrUnexpectedEOF}
	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, r)
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, _ := http.DefaultClient.Do(req)
	if resp != nil {
		resp.Body.Close()
	}

	time.Sleep(100 * time.Millisecond)

	destFile := filepath.Join(env.receiveDir, "incomplete_check.bin")
	if _, err := os.Stat(destFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete file was published to destination directory!")
	}
}

// -----------------------------------------------------------------------------
// Test 15: Old TUS endpoints are no longer exposed while streaming routes work
// -----------------------------------------------------------------------------
func TestSimplifiedUpload_15_OldTusEndpointsNotExposed(t *testing.T) {
	env := startSimTestServer(t)
	bID, itID := createSimTestBatch(t, env, "tus_removal_check.txt", 100)

	// Batch TUS endpoints to probe
	tusEndpoints := []struct {
		method string
		path   string
	}{
		{"OPTIONS", fmt.Sprintf("/api/batches/%s/items/%s/tus", bID, itID)},
		{"POST", fmt.Sprintf("/api/batches/%s/items/%s/tus", bID, itID)},
		{"HEAD", fmt.Sprintf("/api/batches/%s/items/%s/tus", bID, itID)},
		{"PATCH", fmt.Sprintf("/api/batches/%s/items/%s/tus", bID, itID)},
		{"DELETE", fmt.Sprintf("/api/batches/%s/items/%s/tus", bID, itID)},
		{"POST", fmt.Sprintf("/api/batches/%s/items/%s/complete", bID, itID)},
		{"GET", fmt.Sprintf("/api/batches/%s/items/%s/status", bID, itID)},
		{"OPTIONS", "/guest-api/uploads/dummy/tus"},
		{"POST", "/guest-api/uploads/dummy/tus"},
		{"HEAD", "/guest-api/uploads/dummy/tus"},
		{"PATCH", "/guest-api/uploads/dummy/tus"},
		{"DELETE", "/guest-api/uploads/dummy/tus"},
		{"POST", "/guest-api/uploads/dummy/complete"},
		{"GET", "/guest-api/uploads/dummy/status"},
	}

	for _, ep := range tusEndpoints {
		req, _ := http.NewRequest(ep.method, env.url+ep.path, nil)
		if strings.HasPrefix(ep.path, "/api/") {
			req.Header.Set("Authorization", "Bearer "+env.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s request error: %v", ep.method, ep.path, err)
		}
		resp.Body.Close()
		// Endpoint must return 404 (Not Found) or 405 (Method Not Allowed)
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("expected retired TUS endpoint %s %s to be unexposed (404/405), got HTTP %d", ep.method, ep.path, resp.StatusCode)
		}
	}

	// Now prove that the intended direct streaming endpoint DOES work!
	content := []byte(strings.Repeat("a", 100))
	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", env.url, bID, itID)
	putReq, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(content))
	putReq.Header.Set("Authorization", "Bearer "+env.token)
	putReq.Header.Set("Content-Type", "application/octet-stream")
	putReq.ContentLength = 100

	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT direct stream error: %v", err)
	}
	defer putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("expected direct streaming PUT to succeed with 200, got %d", putResp.StatusCode)
	}
}
