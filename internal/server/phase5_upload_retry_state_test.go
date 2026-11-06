package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// errorAfterReader reads up to limit bytes from data, then returns err.
type errorAfterReader struct {
	data   []byte
	offset int
	limit  int
	err    error
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if r.offset >= r.limit {
		return 0, r.err
	}
	toRead := len(p)
	if rem := r.limit - r.offset; toRead > rem {
		toRead = rem
	}
	copy(p, r.data[r.offset:r.offset+toRead])
	r.offset += toRead
	if r.offset >= r.limit {
		return toRead, r.err
	}
	return toRead, nil
}

// blockingReader reads up to blockAt bytes, then blocks on unblock channel before finishing.
type blockingReader struct {
	data    []byte
	offset  int
	blockAt int
	unblock <-chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
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

// -----------------------------------------------------------------------------
// Scenario 1: Disconnect while receiving data (premature EOF / truncated PATCH)
// leaves offset committed and state resumable.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario01_DisconnectPrematureEOFLeavesOffsetCommitted(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 2 * 1024 * 1024 // 2 MB
	payload := deterministicData(payloadSize, 0x41)

	bID, itID := createSimTestBatch(t, env, "premature_eof.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)
	transferID := personalTransferID(bID, itID)

	const dropAt = 512 * 1024 // Drop after 512 KB
	errReader := &errorAfterReader{
		data:  payload,
		limit: dropAt,
		err:   io.ErrUnexpectedEOF,
	}

	req, _ := http.NewRequest(http.MethodPatch, tusURL, errReader)
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.ContentLength = payloadSize

	// The client request will encounter an error or EOF during transmission
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	// Wait for server-side upload handler to commit partial bytes and settle to ResumableInterrupted
	waitForUploadSettled(t, env, transferID, transfer.ResumableInterrupted, 5*time.Second)

	// Authoritative HEAD request
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+env.token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD request failed: %v", err)
	}
	defer headResp.Body.Close()

	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from HEAD, got %d", headResp.StatusCode)
	}

	committedStr := headResp.Header.Get("Upload-Offset")
	committedOffset, err := strconv.ParseInt(committedStr, 10, 64)
	if err != nil || committedOffset != dropAt {
		t.Fatalf("expected committed offset %d, got %d (err=%v)", dropAt, committedOffset, err)
	}

	state := headResp.Header.Get("Upload-State")
	if state != string(transfer.ResumableInterrupted) {
		t.Fatalf("expected upload state %q, got %q", transfer.ResumableInterrupted, state)
	}

	prefixDigest := headResp.Header.Get("Upload-Prefix-Digest")
	expectedPrefixHash := sha256.Sum256(payload[:committedOffset])
	expectedPrefixHex := hex.EncodeToString(expectedPrefixHash[:])
	if prefixDigest != expectedPrefixHex {
		t.Fatalf("prefix digest mismatch: expected %s, got %s", expectedPrefixHex, prefixDigest)
	}
}

// -----------------------------------------------------------------------------
// Scenario 2: Client timeout while server handler is active returns 423 Locked with Retry-After: 1.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario02_ClientTimeoutWhileHandlerActiveReturns423Locked(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 1024 * 1024 // 1 MB
	payload := deterministicData(payloadSize, 0x42)

	bID, itID := createSimTestBatch(t, env, "timeout_locked.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)
	transferID := personalTransferID(bID, itID)

	unblock := make(chan struct{})
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()

	blockReader := &blockingReader{
		data:    payload,
		blockAt: 1024,
		unblock: unblock,
	}

	activeStarted := make(chan struct{})
	go func() {
		close(activeStarted)
		req, _ := http.NewRequest(http.MethodPatch, tusURL, blockReader)
		req.Header.Set("Authorization", "Bearer "+env.token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Offset", "0")
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.ContentLength = payloadSize
		r, _ := http.DefaultClient.Do(req)
		if r != nil {
			r.Body.Close()
		}
	}()

	<-activeStarted

	// Wait until handler acquires the upload lifecycle lock
	var upload *transfer.ResumableUpload
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		up, ok := env.svcs.Resumable.GetUpload(transferID)
		if ok && !up.TryLock() {
			upload = up
			break
		}
		if ok {
			up.Unlock()
		}
		time.Sleep(10 * time.Millisecond)
	}
	if upload == nil {
		t.Fatal("handler did not acquire upload lifecycle lock in time")
	}

	// Concurrent request while handler is active MUST receive HTTP 423 Locked
	retryReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	retryReq.Header.Set("Authorization", "Bearer "+env.token)
	retryReq.Header.Set("Tus-Resumable", "1.0.0")
	retryReq.Header.Set("Upload-Offset", "0")
	retryReq.Header.Set("Content-Type", "application/offset+octet-stream")
	retryReq.ContentLength = payloadSize

	retryResp, err := http.DefaultClient.Do(retryReq)
	if err != nil {
		t.Fatalf("concurrent request failed: %v", err)
	}
	defer retryResp.Body.Close()

	if retryResp.StatusCode != statusLocked {
		t.Fatalf("expected status %d Locked, got %d", statusLocked, retryResp.StatusCode)
	}

	retryAfter := retryResp.Header.Get("Retry-After")
	if retryAfter != "1" {
		t.Fatalf("expected Retry-After: 1 header, got %q", retryAfter)
	}

	// Unblock active handler so test can clean up gracefully
	close(unblock)
	waitForUploadSettled(t, env, transferID, transfer.ResumableAwaitingDigest, 5*time.Second)
}

// -----------------------------------------------------------------------------
// Scenario 3: Reproduce exact error "transfer: invalid upload state: current state is active"
// by attempting /complete when CommittedOffset < TotalSize.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario03_ReproduceExactInvalidStateCurrentStateIsActive(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 2 * 1024 * 1024 // 2 MB
	payload := deterministicData(payloadSize, 0x43)

	bID, itID := createSimTestBatch(t, env, "incomplete_complete.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)
	transferID := personalTransferID(bID, itID)

	// Step 1: Send partial PATCH for 512 KB
	const partialBytes = 512 * 1024
	partialReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[:partialBytes]))
	partialReq.Header.Set("Authorization", "Bearer "+env.token)
	partialReq.Header.Set("Tus-Resumable", "1.0.0")
	partialReq.Header.Set("Upload-Offset", "0")
	partialReq.Header.Set("Content-Type", "application/offset+octet-stream")
	partialReq.ContentLength = partialBytes

	pResp, err := http.DefaultClient.Do(partialReq)
	if err != nil {
		t.Fatalf("partial PATCH failed: %v", err)
	}
	pResp.Body.Close()
	if pResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d", pResp.StatusCode)
	}

	// Verify the upload currently has CommittedOffset < TotalSize
	up, ok := env.svcs.Resumable.GetUpload(transferID)
	if !ok {
		t.Fatal("upload not found")
	}
	meta := up.Meta()
	if meta.CommittedOffset != partialBytes || meta.TotalSize != payloadSize {
		t.Fatalf("expected offset %d of %d, got %d", partialBytes, payloadSize, meta.CommittedOffset)
	}

	// Step 2: Directly verify unit behavior of FinalizeComplete when state is active and CommittedOffset < TotalSize
	// This directly tests the error formatting that produces "transfer: invalid upload state: current state is active"
	directErr := func() error {
		up.UpdateMeta(func(m *transfer.ResumableMeta) {
			m.State = transfer.ResumableActive
		})
		_, err := env.svcs.Resumable.FinalizeComplete(transferID, hex.EncodeToString(sha256.New().Sum(nil)))
		return err
	}()
	if directErr == nil {
		t.Fatal("expected FinalizeComplete to fail for incomplete active upload")
	}
	const expectedErrText = "transfer: invalid upload state: current state is active"
	if directErr.Error() != expectedErrText {
		t.Fatalf("expected exact error %q, got %q", expectedErrText, directErr.Error())
	}
	if !errors.Is(directErr, transfer.ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState sentinel, got %v", directErr)
	}

	// Step 3: Now attempt /complete via HTTP handler
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
	compBody, _ := json.Marshal(map[string]string{
		"expectedSHA256": hex.EncodeToString(sha256.New().Sum(nil)),
	})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")

	compResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatalf("complete request failed: %v", err)
	}
	defer compResp.Body.Close()

	if compResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for incomplete upload complete, got %d", compResp.StatusCode)
	}

	var errPayload map[string]string
	if err := json.NewDecoder(compResp.Body).Decode(&errPayload); err != nil {
		t.Fatalf("decode error JSON: %v", err)
	}
	errMsg := errPayload["error"]
	if !strings.Contains(errMsg, "transfer: invalid upload state:") {
		t.Fatalf("expected error message to contain 'transfer: invalid upload state:', got %q", errMsg)
	}
}

// -----------------------------------------------------------------------------
// Scenario 4: Retry attempted while upload state is active settles or waits for 423.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario04_RetryWhileActiveReturns423OrSettles(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 1024 * 1024
	payload := deterministicData(payloadSize, 0x44)

	bID, itID := createSimTestBatch(t, env, "retry_active.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)
	transferID := personalTransferID(bID, itID)

	const firstChunk = 512 * 1024
	unblock := make(chan struct{})
	blockReader := &blockingReader{
		data:    payload[:firstChunk],
		blockAt: 2048,
		unblock: unblock,
	}

	go func() {
		req, _ := http.NewRequest(http.MethodPatch, tusURL, blockReader)
		req.Header.Set("Authorization", "Bearer "+env.token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Offset", "0")
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.ContentLength = firstChunk
		r, _ := http.DefaultClient.Do(req)
		if r != nil {
			r.Body.Close()
		}
	}()

	// Wait until lock is held
	for i := 0; i < 50; i++ {
		up, ok := env.svcs.Resumable.GetUpload(transferID)
		if ok && !up.TryLock() {
			break
		}
		if ok {
			up.Unlock()
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Immediate retry during active hold receives 423
	retryReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[2048:]))
	retryReq.Header.Set("Authorization", "Bearer "+env.token)
	retryReq.Header.Set("Tus-Resumable", "1.0.0")
	retryReq.Header.Set("Upload-Offset", "2048")
	retryReq.Header.Set("Content-Type", "application/offset+octet-stream")
	retryResp, err := http.DefaultClient.Do(retryReq)
	if err != nil {
		close(unblock)
		t.Fatal(err)
	}
	retryResp.Body.Close()
	if retryResp.StatusCode != statusLocked {
		close(unblock)
		t.Fatalf("expected 423 Locked, got %d", retryResp.StatusCode)
	}

	// Release previous handler and settle
	close(unblock)
	waitForUploadSettled(t, env, transferID, transfer.ResumableActive, 5*time.Second)

	// Bounded retry succeeds after handler settles
	up, _ := env.svcs.Resumable.GetUpload(transferID)
	committed := up.Meta().CommittedOffset

	resumeReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[committed:]))
	resumeReq.Header.Set("Authorization", "Bearer "+env.token)
	resumeReq.Header.Set("Tus-Resumable", "1.0.0")
	resumeReq.Header.Set("Upload-Offset", strconv.FormatInt(committed, 10))
	resumeReq.Header.Set("Content-Type", "application/offset+octet-stream")
	resumeReq.ContentLength = payloadSize - committed

	resumeResp, err := http.DefaultClient.Do(resumeReq)
	if err != nil {
		t.Fatalf("resumed request failed: %v", err)
	}
	resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d", resumeResp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Scenario 5: Retry after original handler settles resumes from committed checkpoint.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario05_RetryAfterSettlingResumesFromCommittedCheckpoint(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 2 * 1024 * 1024 // 2 MB
	payload := deterministicData(payloadSize, 0x45)

	bID, itID := createSimTestBatch(t, env, "checkpoint_resume.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)
	transferID := personalTransferID(bID, itID)

	// Stage 1: Upload first 1 MB and drop
	const firstChunk = 1024 * 1024
	errReader := &errorAfterReader{
		data:  payload,
		limit: firstChunk,
		err:   io.ErrUnexpectedEOF,
	}

	req1, _ := http.NewRequest(http.MethodPatch, tusURL, errReader)
	req1.Header.Set("Authorization", "Bearer "+env.token)
	req1.Header.Set("Tus-Resumable", "1.0.0")
	req1.Header.Set("Upload-Offset", "0")
	req1.Header.Set("Content-Type", "application/offset+octet-stream")
	req1.ContentLength = payloadSize
	r1, _ := http.DefaultClient.Do(req1)
	if r1 != nil {
		r1.Body.Close()
	}

	// Wait for settlement
	waitForUploadSettled(t, env, transferID, transfer.ResumableInterrupted, 5*time.Second)

	// Stage 2: Query authoritative status via HEAD
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+env.token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD request failed: %v", err)
	}
	defer headResp.Body.Close()

	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status %d", headResp.StatusCode)
	}
	serverOffsetStr := headResp.Header.Get("Upload-Offset")
	serverOffset, err := strconv.ParseInt(serverOffsetStr, 10, 64)
	if err != nil || serverOffset != firstChunk {
		t.Fatalf("expected server offset %d, got %d (err=%v)", firstChunk, serverOffset, err)
	}

	// Verify prefix digest
	serverPrefix := headResp.Header.Get("Upload-Prefix-Digest")
	expectedPrefix := hex.EncodeToString(func() []byte {
		h := sha256.Sum256(payload[:serverOffset])
		return h[:]
	}())
	if serverPrefix != expectedPrefix {
		t.Fatalf("prefix mismatch: expected %s, got %s", expectedPrefix, serverPrefix)
	}

	// Stage 3: Resume remaining 1 MB from serverOffset
	remPayload := payload[serverOffset:]
	resumeReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(remPayload))
	resumeReq.Header.Set("Authorization", "Bearer "+env.token)
	resumeReq.Header.Set("Tus-Resumable", "1.0.0")
	resumeReq.Header.Set("Upload-Offset", strconv.FormatInt(serverOffset, 10))
	resumeReq.Header.Set("Content-Type", "application/offset+octet-stream")
	resumeReq.ContentLength = int64(len(remPayload))

	resumeResp, err := http.DefaultClient.Do(resumeReq)
	if err != nil {
		t.Fatalf("resume request failed: %v", err)
	}
	defer resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content from resume PATCH, got %d", resumeResp.StatusCode)
	}

	// Stage 4: Finalize complete
	fullSHA := sha256.Sum256(payload)
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
	compBody, _ := json.Marshal(map[string]string{
		"expectedSHA256": hex.EncodeToString(fullSHA[:]),
	})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")

	compResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatalf("complete request failed: %v", err)
	}
	defer compResp.Body.Close()
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from complete, got %d", compResp.StatusCode)
	}

	// Verify published content
	received, err := os.ReadFile(filepath.Join(env.receiveDir, "checkpoint_resume.bin"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("received payload does not match expected payload")
	}
}

// -----------------------------------------------------------------------------
// Scenario 6: Retry when upload is already complete returns existing publication idempotently.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario06_RetryWhenAlreadyCompleteReturnsPublicationIdempotently(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 512 * 1024 // 512 KB
	payload := deterministicData(payloadSize, 0x46)
	fullSHA := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(fullSHA[:])

	bID, itID := createSimTestBatch(t, env, "idempotent_complete.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)

	// Upload all bytes
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+env.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = payloadSize

	pResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatalf("patch request failed: %v", err)
	}
	pResp.Body.Close()
	if pResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d", pResp.StatusCode)
	}

	// First /complete call
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})

	compReq1, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq1.Header.Set("Authorization", "Bearer "+env.token)
	compReq1.Header.Set("Content-Type", "application/json")
	compResp1, err := http.DefaultClient.Do(compReq1)
	if err != nil {
		t.Fatalf("complete 1 failed: %v", err)
	}
	defer compResp1.Body.Close()
	if compResp1.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from complete 1, got %d", compResp1.StatusCode)
	}

	var pub1 map[string]any
	if err := json.NewDecoder(compResp1.Body).Decode(&pub1); err != nil {
		t.Fatalf("decode pub 1: %v", err)
	}

	// HEAD check: upload is marked completed and committed offset is full size
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+env.token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD failed: %v", err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from HEAD, got %d", headResp.StatusCode)
	}
	if headResp.Header.Get("Upload-Offset") != strconv.FormatInt(payloadSize, 10) {
		t.Fatalf("expected offset %d, got %s", payloadSize, headResp.Header.Get("Upload-Offset"))
	}
	if headResp.Header.Get("Upload-State") != string(transfer.ResumableCompleted) {
		t.Fatalf("expected Upload-State completed, got %s", headResp.Header.Get("Upload-State"))
	}

	// Repeat /complete call: must be idempotent and return 200 OK with the publication
	compReq2, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq2.Header.Set("Authorization", "Bearer "+env.token)
	compReq2.Header.Set("Content-Type", "application/json")
	compResp2, err := http.DefaultClient.Do(compReq2)
	if err != nil {
		t.Fatalf("complete 2 failed: %v", err)
	}
	defer compResp2.Body.Close()
	if compResp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on idempotent retry, got %d", compResp2.StatusCode)
	}

	var pub2 map[string]any
	if err := json.NewDecoder(compResp2.Body).Decode(&pub2); err != nil {
		t.Fatalf("decode pub 2: %v", err)
	}
	if pub2["sha256"] != pub1["sha256"] || pub2["status"] != "completed" {
		t.Fatalf("idempotent publication mismatch: %+v vs %+v", pub1, pub2)
	}
}

// -----------------------------------------------------------------------------
// Scenario 7: Retry when upload has expired or part file is missing returns 404/410.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario07_RetryWhenUploadExpiredOrMissingReturns404Or410(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 256 * 1024
	payload := deterministicData(payloadSize, 0x47)

	bID, itID := createSimTestBatch(t, env, "expired.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)
	transferID := personalTransferID(bID, itID)

	// Partial upload
	pReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[:1024]))
	pReq.Header.Set("Authorization", "Bearer "+env.token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Offset", "0")
	pReq.Header.Set("Content-Type", "application/offset+octet-stream")
	pReq.ContentLength = 1024
	pResp, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pResp.Body.Close()

	waitForUploadSettled(t, env, transferID, transfer.ResumableActive, 5*time.Second)

	// Explicitly cancel / discard the upload
	_ = env.svcs.Resumable.CancelUpload(transferID)

	// Attempting HEAD now returns 404 (upload not found / expired)
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+env.token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusNotFound && headResp.StatusCode != http.StatusGone {
		t.Fatalf("expected 404 or 410 for discarded upload, got %d", headResp.StatusCode)
	}

	// Attempting resumed PATCH returns 404 or 410
	resumeReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[1024:]))
	resumeReq.Header.Set("Authorization", "Bearer "+env.token)
	resumeReq.Header.Set("Tus-Resumable", "1.0.0")
	resumeReq.Header.Set("Upload-Offset", "1024")
	resumeReq.Header.Set("Content-Type", "application/offset+octet-stream")
	resumeReq.ContentLength = payloadSize - 1024

	resumeResp, err := http.DefaultClient.Do(resumeReq)
	if err != nil {
		t.Fatal(err)
	}
	resumeResp.Body.Close()
	if resumeResp.StatusCode != http.StatusNotFound && resumeResp.StatusCode != http.StatusGone {
		t.Fatalf("expected 404 or 410 for discarded resumed PATCH, got %d", resumeResp.StatusCode)
	}
}

// -----------------------------------------------------------------------------
// Scenario 8: Multiple concurrent retry requests synchronize safely via lifecycleMu.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario08_ConcurrentRetriesSynchronizeViaLifecycleMu(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 2 * 1024 * 1024 // 2 MB
	payload := deterministicData(payloadSize, 0x48)

	bID, itID := createSimTestBatch(t, env, "concurrent_sync.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)
	transferID := personalTransferID(bID, itID)

	// Upload first 512 KB and interrupt
	const initialBytes = 512 * 1024
	pReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[:initialBytes]))
	pReq.Header.Set("Authorization", "Bearer "+env.token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Offset", "0")
	pReq.Header.Set("Content-Type", "application/offset+octet-stream")
	pReq.ContentLength = initialBytes
	pResp, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pResp.Body.Close()

	waitForUploadSettled(t, env, transferID, transfer.ResumableActive, 5*time.Second)

	// Launch 5 concurrent goroutines attempting to resume remaining bytes from initialBytes
	const concurrency = 5
	var wg sync.WaitGroup
	var successCount atomic.Int32
	var lockedCount atomic.Int32
	var conflictCount atomic.Int32

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			remData := payload[initialBytes:]
			req, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(remData))
			req.Header.Set("Authorization", "Bearer "+env.token)
			req.Header.Set("Tus-Resumable", "1.0.0")
			req.Header.Set("Upload-Offset", strconv.FormatInt(initialBytes, 10))
			req.Header.Set("Content-Type", "application/offset+octet-stream")
			req.ContentLength = int64(len(remData))

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			switch resp.StatusCode {
			case http.StatusNoContent:
				successCount.Add(1)
			case statusLocked:
				lockedCount.Add(1)
			case http.StatusConflict:
				conflictCount.Add(1)
			}
		}()
	}
	wg.Wait()

	// Exactly one request should succeed in streaming the remaining bytes,
	// while others are safely rejected with 423 Locked or 409 Conflict (since offset shifted).
	if successCount.Load() != 1 {
		t.Fatalf("expected exactly 1 successful resume, got %d (locked=%d, conflict=%d)",
			successCount.Load(), lockedCount.Load(), conflictCount.Load())
	}

	// Verify all bytes are committed on server
	waitForUploadSettled(t, env, transferID, transfer.ResumableAwaitingDigest, 5*time.Second)
	up, _ := env.svcs.Resumable.GetUpload(transferID)
	if up.Meta().CommittedOffset != payloadSize {
		t.Fatalf("expected all %d bytes committed, got %d", payloadSize, up.Meta().CommittedOffset)
	}
}

// -----------------------------------------------------------------------------
// Scenario 9: Browser refresh followed by upload-state reconciliation (HEAD -> awaiting_digest / partial offset).
// -----------------------------------------------------------------------------
func TestPhase5_Scenario09_BrowserRefreshReconciliation(t *testing.T) {
	// Case A: 100% of bytes committed, browser refreshes before calling /complete
	t.Run("AwaitingDigestReconciliation", func(t *testing.T) {
		env := startSimTestServer(t)
		const payloadSize = 1024 * 1024
		payload := deterministicData(payloadSize, 0x49)
		fullDigest := sha256.Sum256(payload)
		fullDigestHex := hex.EncodeToString(fullDigest[:])

		bID, itID := createSimTestBatch(t, env, "awaiting_refresh.bin", payloadSize)
		tusURL := createSimTus(t, env, bID, itID, payloadSize)
		transferID := personalTransferID(bID, itID)

		// Upload full 1 MB
		req, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+env.token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Offset", "0")
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.ContentLength = payloadSize

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 204 No Content, got %d", resp.StatusCode)
		}

		waitForUploadSettled(t, env, transferID, transfer.ResumableAwaitingDigest, 5*time.Second)

		// Simulate browser reload: client queries HEAD to reconcile
		headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
		headReq.Header.Set("Authorization", "Bearer "+env.token)
		headReq.Header.Set("Tus-Resumable", "1.0.0")
		headResp, err := http.DefaultClient.Do(headReq)
		if err != nil {
			t.Fatal(err)
		}
		headResp.Body.Close()

		if headResp.StatusCode != http.StatusOK {
			t.Fatalf("HEAD expected 200 OK, got %d", headResp.StatusCode)
		}
		if headResp.Header.Get("Upload-Offset") != strconv.FormatInt(payloadSize, 10) {
			t.Fatalf("expected offset %d, got %s", payloadSize, headResp.Header.Get("Upload-Offset"))
		}
		// Notice: even if internal state settled to interrupted, HEAD reports awaiting_digest
		// because CommittedOffset == TotalSize!
		stateHeader := headResp.Header.Get("Upload-State")
		if stateHeader != string(transfer.ResumableAwaitingDigest) {
			t.Fatalf("expected Upload-State %q, got %q", transfer.ResumableAwaitingDigest, stateHeader)
		}

		// Client computes local digest and calls /complete directly without transferring bytes
		compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
		compBody, _ := json.Marshal(map[string]string{"expectedSHA256": fullDigestHex})
		compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
		compReq.Header.Set("Authorization", "Bearer "+env.token)
		compReq.Header.Set("Content-Type", "application/json")

		compResp, err := http.DefaultClient.Do(compReq)
		if err != nil {
			t.Fatal(err)
		}
		defer compResp.Body.Close()
		if compResp.StatusCode != http.StatusOK {
			t.Fatalf("complete expected 200 OK, got %d", compResp.StatusCode)
		}

		// Verify file published
		received, err := os.ReadFile(filepath.Join(env.receiveDir, "awaiting_refresh.bin"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(received, payload) {
			t.Fatal("file corruption")
		}
	})

	// Case B: Partial bytes committed, browser refreshes mid-transfer
	t.Run("PartialOffsetReconciliation", func(t *testing.T) {
		env := startSimTestServer(t)
		const payloadSize = 1024 * 1024
		payload := deterministicData(payloadSize, 0x4A)

		bID, itID := createSimTestBatch(t, env, "partial_refresh.bin", payloadSize)
		tusURL := createSimTus(t, env, bID, itID, payloadSize)
		transferID := personalTransferID(bID, itID)

		const partial = 400 * 1024
		pReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[:partial]))
		pReq.Header.Set("Authorization", "Bearer "+env.token)
		pReq.Header.Set("Tus-Resumable", "1.0.0")
		pReq.Header.Set("Upload-Offset", "0")
		pReq.Header.Set("Content-Type", "application/offset+octet-stream")
		pReq.ContentLength = partial
		pResp, err := http.DefaultClient.Do(pReq)
		if err != nil {
			t.Fatal(err)
		}
		pResp.Body.Close()

		waitForUploadSettled(t, env, transferID, transfer.ResumableActive, 5*time.Second)

		// Browser reloads: issues HEAD
		headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
		headReq.Header.Set("Authorization", "Bearer "+env.token)
		headReq.Header.Set("Tus-Resumable", "1.0.0")
		headResp, err := http.DefaultClient.Do(headReq)
		if err != nil {
			t.Fatal(err)
		}
		headResp.Body.Close()

		if headResp.StatusCode != http.StatusOK {
			t.Fatalf("HEAD expected 200 OK, got %d", headResp.StatusCode)
		}
		if headResp.Header.Get("Upload-Offset") != strconv.FormatInt(partial, 10) {
			t.Fatalf("expected offset %d, got %s", partial, headResp.Header.Get("Upload-Offset"))
		}
		if headResp.Header.Get("Upload-State") != string(transfer.ResumableActive) {
			t.Fatalf("expected active state, got %s", headResp.Header.Get("Upload-State"))
		}
	})
}

// -----------------------------------------------------------------------------
// Scenario 10: Final file content and SHA-256 digest verified byte-for-byte after multi-stage resumption.
// -----------------------------------------------------------------------------
func TestPhase5_Scenario10_MultiStageResumptionByteForByteIntegrity(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 3 * 1024 * 1024 // 3 MB
	payload := deterministicData(payloadSize, 0x4B)
	fullDigest := sha256.Sum256(payload)
	fullDigestHex := hex.EncodeToString(fullDigest[:])

	bID, itID := createSimTestBatch(t, env, "multistage_integrity.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)
	transferID := personalTransferID(bID, itID)

	// Stage 1: Upload [0, 1 MB), then drop
	const stage1Limit = 1024 * 1024
	req1, _ := http.NewRequest(http.MethodPatch, tusURL, &errorAfterReader{
		data:  payload,
		limit: stage1Limit,
		err:   io.ErrUnexpectedEOF,
	})
	req1.Header.Set("Authorization", "Bearer "+env.token)
	req1.Header.Set("Tus-Resumable", "1.0.0")
	req1.Header.Set("Upload-Offset", "0")
	req1.Header.Set("Content-Type", "application/offset+octet-stream")
	req1.ContentLength = payloadSize
	r1, _ := http.DefaultClient.Do(req1)
	if r1 != nil {
		r1.Body.Close()
	}
	waitForUploadSettled(t, env, transferID, transfer.ResumableInterrupted, 5*time.Second)

	// Verify stage 1 offset via HEAD
	h1Req, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	h1Req.Header.Set("Authorization", "Bearer "+env.token)
	h1Req.Header.Set("Tus-Resumable", "1.0.0")
	h1Resp, err := http.DefaultClient.Do(h1Req)
	if err != nil {
		t.Fatal(err)
	}
	h1Resp.Body.Close()
	if h1Resp.Header.Get("Upload-Offset") != strconv.FormatInt(stage1Limit, 10) {
		t.Fatalf("stage 1 offset mismatch: expected %d, got %s", stage1Limit, h1Resp.Header.Get("Upload-Offset"))
	}

	// Stage 2: Resume [1 MB, 2 MB), then drop
	const stage2Limit = 2 * 1024 * 1024
	req2, _ := http.NewRequest(http.MethodPatch, tusURL, &errorAfterReader{
		data:  payload[stage1Limit:],
		limit: stage2Limit - stage1Limit,
		err:   io.ErrUnexpectedEOF,
	})
	req2.Header.Set("Authorization", "Bearer "+env.token)
	req2.Header.Set("Tus-Resumable", "1.0.0")
	req2.Header.Set("Upload-Offset", strconv.FormatInt(stage1Limit, 10))
	req2.Header.Set("Content-Type", "application/offset+octet-stream")
	req2.ContentLength = payloadSize - stage1Limit
	r2, _ := http.DefaultClient.Do(req2)
	if r2 != nil {
		r2.Body.Close()
	}
	waitForUploadSettled(t, env, transferID, transfer.ResumableInterrupted, 5*time.Second)

	// Verify stage 2 offset via HEAD
	h2Req, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	h2Req.Header.Set("Authorization", "Bearer "+env.token)
	h2Req.Header.Set("Tus-Resumable", "1.0.0")
	h2Resp, err := http.DefaultClient.Do(h2Req)
	if err != nil {
		t.Fatal(err)
	}
	h2Resp.Body.Close()
	if h2Resp.Header.Get("Upload-Offset") != strconv.FormatInt(stage2Limit, 10) {
		t.Fatalf("stage 2 offset mismatch: expected %d, got %s", stage2Limit, h2Resp.Header.Get("Upload-Offset"))
	}

	// Stage 3: Resume [2 MB, 3 MB) to completion
	stage3Payload := payload[stage2Limit:]
	req3, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(stage3Payload))
	req3.Header.Set("Authorization", "Bearer "+env.token)
	req3.Header.Set("Tus-Resumable", "1.0.0")
	req3.Header.Set("Upload-Offset", strconv.FormatInt(stage2Limit, 10))
	req3.Header.Set("Content-Type", "application/offset+octet-stream")
	req3.ContentLength = int64(len(stage3Payload))

	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("stage 3 expected 204 No Content, got %d", resp3.StatusCode)
	}

	// Finalize complete
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": fullDigestHex})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")

	compResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	defer compResp.Body.Close()
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from complete, got %d", compResp.StatusCode)
	}

	// Read published file and verify byte-for-byte equality
	received, err := os.ReadFile(filepath.Join(env.receiveDir, "multistage_integrity.bin"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("published file does not match original payload byte-for-byte")
	}
	receivedSHA := sha256.Sum256(received)
	if hex.EncodeToString(receivedSHA[:]) != fullDigestHex {
		t.Fatalf("digest mismatch: expected %s, got %s", fullDigestHex, hex.EncodeToString(receivedSHA[:]))
	}
}
