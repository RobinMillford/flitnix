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
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func createTestBatch(t *testing.T, env *testEnv, ts *httptest.Server, token string, itemName string, itemSize int64) (batchID, itemID string) {
	t.Helper()
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{
				RelativePath: itemName,
				Size:         itemSize,
			},
		},
	}
	body, _ := json.Marshal(manifest)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/batches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create batch failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", res.StatusCode)
	}

	var view transfer.BatchView
	if err := json.NewDecoder(res.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}

	key := env.tokenKey(token)
	for _, item := range view.Items {
		contentID, err := env.coord.ItemContentID(key, view.ID, item.ID)
		if err == nil {
			_, _ = env.trans.Decide(contentID, true)
		}
	}
	// Approve batch in coordinator
	_, err = env.coord.ApproveBatch(key, view.ID)
	if err != nil {
		t.Fatalf("ApproveBatch failed: %v", err)
	}

	return view.ID, view.Items[0].ID
}

// TestTusOptions_NoResumableHeaderRequired tests Section 4.1 & 4.2:
// OPTIONS discovery returns 204 with capabilities, and MUST NOT reject missing Tus-Resumable.
func TestTusOptions_NoResumableHeaderRequired(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	bID, itID := createTestBatch(t, env, ts, token, "options.bin", 1024)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	req, _ := http.NewRequest(http.MethodOptions, tusURL, nil)
	// Deliberately omit Tus-Resumable header
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for OPTIONS, got %d", res.StatusCode)
	}
	if v := res.Header.Get("Tus-Resumable"); v != "1.0.0" {
		t.Errorf("expected Tus-Resumable 1.0.0, got %s", v)
	}
	if v := res.Header.Get("Tus-Version"); v != "1.0.0" {
		t.Errorf("expected Tus-Version 1.0.0, got %s", v)
	}
	if ext := res.Header.Get("Tus-Extension"); !strings.Contains(ext, "creation") || !strings.Contains(ext, "termination") {
		t.Errorf("unexpected Tus-Extension: %s", ext)
	}
}

// TestTusPrecondition_RequiresVersion100 tests Section 4.2:
// Data requests without Tus-Resumable or with unsupported version yield 412 Precondition Failed.
func TestTusPrecondition_RequiresVersion100(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	bID, itID := createTestBatch(t, env, ts, token, "precond.bin", 1024)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodPatch, http.MethodDelete} {
		// Test 1: Missing Tus-Resumable header
		req, _ := http.NewRequest(method, tusURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusPreconditionFailed {
			t.Errorf("method %s: expected 412 for missing Tus-Resumable, got %d", method, res.StatusCode)
		}
		if res.Header.Get("Tus-Version") != "1.0.0" {
			t.Errorf("method %s: expected Tus-Version 1.0.0 in 412 response, got %s", method, res.Header.Get("Tus-Version"))
		}

		// Test 2: Unsupported version
		req2, _ := http.NewRequest(method, tusURL, nil)
		req2.Header.Set("Authorization", "Bearer "+token)
		req2.Header.Set("Tus-Resumable", "0.2.2")
		res2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		res2.Body.Close()
		if res2.StatusCode != http.StatusPreconditionFailed {
			t.Errorf("method %s: expected 412 for unsupported Tus-Resumable 0.2.2, got %d", method, res2.StatusCode)
		}
	}
}

// TestTusCreate_PersonalAndIdempotency tests Section 4.3 & Scenario 4:
// Creation returns 201 with Location and Upload-Offset 0; repeated POST returns 201 idempotently.
func TestTusCreate_PersonalAndIdempotency(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	bID, itID := createTestBatch(t, env, ts, token, "create.bin", 2048)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	req, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "2048")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc == "" {
		t.Error("expected Location header in 201 response")
	}
	if off := res.Header.Get("Upload-Offset"); off != "0" {
		t.Errorf("expected Upload-Offset 0, got %s", off)
	}

	// Repeated POST (Scenario 4) must return 201 Created idempotently
	req2, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Tus-Resumable", "1.0.0")
	req2.Header.Set("Upload-Length", "2048")

	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()

	if res2.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created on repeated POST, got %d", res2.StatusCode)
	}
}

// TestTusCreate_LengthMismatch tests rejection of length mismatch:
func TestTusCreate_LengthMismatch(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	bID, itID := createTestBatch(t, env, ts, token, "mismatch.bin", 1024)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	req, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "9999") // Mismatched

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request on length mismatch, got %d", res.StatusCode)
	}
}

// TestTusHead_DiscoveryAndNoStore tests Section 4.4:
func TestTusHead_DiscoveryAndNoStore(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	bID, itID := createTestBatch(t, env, ts, token, "head.bin", 4096)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// Create first
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", "4096")
	postRes, _ := http.DefaultClient.Do(postReq)
	postRes.Body.Close()

	// Query HEAD
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")

	headRes, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	defer headRes.Body.Close()

	if headRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", headRes.StatusCode)
	}
	if off := headRes.Header.Get("Upload-Offset"); off != "0" {
		t.Errorf("expected Upload-Offset 0, got %s", off)
	}
	if length := headRes.Header.Get("Upload-Length"); length != "4096" {
		t.Errorf("expected Upload-Length 4096, got %s", length)
	}
	if state := headRes.Header.Get("Upload-State"); state != "active" {
		t.Errorf("expected Upload-State active, got %s", state)
	}
	if cc := headRes.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control no-store, got %s", cc)
	}
}

// TestTusPatch_FullStreamAndCompletion tests complete upload flow through PATCH & POST /complete:
func TestTusPatch_FullStreamAndCompletion(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	payload := bytes.Repeat([]byte("F"), 10*1024) // 10 KB
	expectedDigest := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedDigest[:])

	bID, itID := createTestBatch(t, env, ts, token, "patch_full.bin", int64(len(payload)))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// 1. POST
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	pRes, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()
	if pRes.StatusCode != http.StatusCreated {
		t.Fatalf("POST expected 201, got %d", pRes.StatusCode)
	}

	// 2. PATCH wrong content-type -> 415
	badTypeReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	badTypeReq.Header.Set("Authorization", "Bearer "+token)
	badTypeReq.Header.Set("Tus-Resumable", "1.0.0")
	badTypeReq.Header.Set("Upload-Offset", "0")
	badTypeReq.Header.Set("Content-Type", "text/plain")
	btRes, _ := http.DefaultClient.Do(badTypeReq)
	btRes.Body.Close()
	if btRes.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for bad Content-Type, got %d", btRes.StatusCode)
	}

	// 3. PATCH wrong offset -> 409 Conflict
	badOffReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	badOffReq.Header.Set("Authorization", "Bearer "+token)
	badOffReq.Header.Set("Tus-Resumable", "1.0.0")
	badOffReq.Header.Set("Upload-Offset", "500")
	badOffReq.Header.Set("Content-Type", "application/offset+octet-stream")
	boRes, _ := http.DefaultClient.Do(badOffReq)
	boRes.Body.Close()
	if boRes.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on offset mismatch, got %d", boRes.StatusCode)
	}

	// 4. Valid PATCH
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()

	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for PATCH, got %d", patchRes.StatusCode)
	}
	if off := patchRes.Header.Get("Upload-Offset"); off != strconv.Itoa(len(payload)) {
		t.Errorf("expected Upload-Offset %d, got %s", len(payload), off)
	}
	if state := patchRes.Header.Get("Upload-State"); state != "awaiting_digest" {
		t.Errorf("expected Upload-State awaiting_digest, got %s", state)
	}

	// 5. Query status endpoint -> awaiting_digest
	statusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/status", ts.URL, bID, itID)
	stReq, _ := http.NewRequest(http.MethodGet, statusURL, nil)
	stReq.Header.Set("Authorization", "Bearer "+token)
	stRes, err := http.DefaultClient.Do(stReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stRes.Body.Close()
	var stMap map[string]any
	_ = json.NewDecoder(stRes.Body).Decode(&stMap)
	if stMap["status"] != "awaiting_digest" {
		t.Fatalf("expected status awaiting_digest, got %v", stMap["status"])
	}

	// 6. POST /complete with WRONG digest -> 460 Checksum Mismatch
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	wrongBody, _ := json.Marshal(map[string]string{"expectedSHA256": hex.EncodeToString(bytes.Repeat([]byte{0}, 32))})
	compBadReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(wrongBody))
	compBadReq.Header.Set("Authorization", "Bearer "+token)
	compBadReq.Header.Set("Content-Type", "application/json")
	compBadRes, _ := http.DefaultClient.Do(compBadReq)
	compBadRes.Body.Close()
	if compBadRes.StatusCode != 460 {
		t.Fatalf("expected 460 Checksum Mismatch, got %d", compBadRes.StatusCode)
	}

	// 6b. Recover as the client does: a 460 checksum mismatch is terminal
	// for the staged upload (the digest cannot be trusted), so the client
	// deletes it and retries the file in a fresh batch. Completing the
	// retry also settles the session queue: a later batch's completion is
	// refused until the head item is terminal (same discipline as the
	// legacy PUT path).
	goodBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	delReq, _ := http.NewRequest(http.MethodDelete, tusURL, nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delReq.Header.Set("Tus-Resumable", "1.0.0")
	delRes, _ := http.DefaultClient.Do(delReq)
	delRes.Body.Close()
	if delRes.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content deleting failed upload, got %d", delRes.StatusCode)
	}
	bIDr, itIDr := createTestBatch(t, env, ts, token, "patch_full_retry.bin", int64(len(payload)))
	tusURLr := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bIDr, itIDr)
	reReq, _ := http.NewRequest(http.MethodPost, tusURLr, nil)
	reReq.Header.Set("Authorization", "Bearer "+token)
	reReq.Header.Set("Tus-Resumable", "1.0.0")
	reReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	reRes, _ := http.DefaultClient.Do(reReq)
	reRes.Body.Close()
	if reRes.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created re-creating upload after mismatch, got %d", reRes.StatusCode)
	}
	rePatchReq, _ := http.NewRequest(http.MethodPatch, tusURLr, bytes.NewReader(payload))
	rePatchReq.Header.Set("Authorization", "Bearer "+token)
	rePatchReq.Header.Set("Tus-Resumable", "1.0.0")
	rePatchReq.Header.Set("Upload-Offset", "0")
	rePatchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	rePatchRes, _ := http.DefaultClient.Do(rePatchReq)
	rePatchRes.Body.Close()
	if rePatchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content re-uploading after mismatch, got %d", rePatchRes.StatusCode)
	}
	completeURLr := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bIDr, itIDr)
	compFixReq, _ := http.NewRequest(http.MethodPost, completeURLr, bytes.NewReader(goodBody))
	compFixReq.Header.Set("Authorization", "Bearer "+token)
	compFixReq.Header.Set("Content-Type", "application/json")
	compFixRes, _ := http.DefaultClient.Do(compFixReq)
	compFixRes.Body.Close()
	if compFixRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK completing after checksum mismatch, got %d", compFixRes.StatusCode)
	}

	// 7. Test valid complete with matching digest
	bID2, itID2 := createTestBatch(t, env, ts, token, "valid_comp.bin", int64(len(payload)))
	tusURL2 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID2, itID2)
	pReq2, _ := http.NewRequest(http.MethodPost, tusURL2, nil)
	pReq2.Header.Set("Authorization", "Bearer "+token)
	pReq2.Header.Set("Tus-Resumable", "1.0.0")
	pReq2.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	pR2, _ := http.DefaultClient.Do(pReq2)
	pR2.Body.Close()

	paReq2, _ := http.NewRequest(http.MethodPatch, tusURL2, bytes.NewReader(payload))
	paReq2.Header.Set("Authorization", "Bearer "+token)
	paReq2.Header.Set("Tus-Resumable", "1.0.0")
	paReq2.Header.Set("Upload-Offset", "0")
	paReq2.Header.Set("Content-Type", "application/offset+octet-stream")
	paR2, _ := http.DefaultClient.Do(paReq2)
	paR2.Body.Close()

	completeURL2 := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID2, itID2)
	compGoodReq, _ := http.NewRequest(http.MethodPost, completeURL2, bytes.NewReader(goodBody))
	compGoodReq.Header.Set("Authorization", "Bearer "+token)
	compGoodReq.Header.Set("Content-Type", "application/json")
	compGoodRes, err := http.DefaultClient.Do(compGoodReq)
	if err != nil {
		t.Fatal(err)
	}
	defer compGoodRes.Body.Close()

	if compGoodRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on valid complete, got %d", compGoodRes.StatusCode)
	}

	// 8. Idempotent repeated complete (Scenario 44)
	compRepeatReq, _ := http.NewRequest(http.MethodPost, completeURL2, bytes.NewReader(goodBody))
	compRepeatReq.Header.Set("Authorization", "Bearer "+token)
	compRepeatReq.Header.Set("Content-Type", "application/json")
	compRepeatRes, err := http.DefaultClient.Do(compRepeatReq)
	if err != nil {
		t.Fatal(err)
	}
	compRepeatRes.Body.Close()
	if compRepeatRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on idempotent repeat complete, got %d", compRepeatRes.StatusCode)
	}
}

// TestTusPatch_LifecycleMu423Locked tests Scenario 34 & 46:
// Concurrent PATCH requests to the same upload yield 423 Locked with Retry-After: 1.
func TestTusPatch_LifecycleMu423Locked(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	bID, itID := createTestBatch(t, env, ts, token, "locked.bin", 100*1024*1024)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(100*1024*1024))
	pR, _ := http.DefaultClient.Do(postReq)
	pR.Body.Close()

	// Acquire lifecycle lock artificially via ResumableManager
	resumable := env.srv.Services().EnsureResumable()
	upload, ok := resumable.GetUpload(personalTransferID(bID, itID))
	if !ok {
		t.Fatal("upload not found")
	}

	if !upload.TryLock() {
		t.Fatal("initial TryLock failed")
	}

	// Concurrent PATCH must return 423 Locked
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader([]byte("foo")))
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	res, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if res.StatusCode != 423 {
		t.Fatalf("expected 423 Locked for concurrent PATCH, got %d", res.StatusCode)
	}
	if retry := res.Header.Get("Retry-After"); retry != "1" {
		t.Errorf("expected Retry-After: 1, got %s", retry)
	}

	// Release lock
	upload.Unlock()
}

// TestTusDelete_ActiveCancellationSequence tests Scenario 49:
// DELETE while lock is held returns 423. After lock is released, DELETE succeeds with 204
// and batch coordinator does not cancel other items in batch.
func TestTusDelete_ActiveCancellationSequence(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	// Create batch with 2 items
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{RelativePath: "file1.bin", Size: 1000},
			{RelativePath: "file2.bin", Size: 2000},
		},
	}
	body, _ := json.Marshal(manifest)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/batches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var view transfer.BatchView
	_ = json.NewDecoder(res.Body).Decode(&view)
	bID := view.ID

	key := env.tokenKey(token)
	_, _ = env.coord.ApproveBatch(key, bID)

	it1ID := view.Items[0].ID
	tusURL1 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, it1ID)

	// Create upload 1
	pReq, _ := http.NewRequest(http.MethodPost, tusURL1, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", "1000")
	pR, _ := http.DefaultClient.Do(pReq)
	pR.Body.Close()

	resumable := env.srv.Services().EnsureResumable()
	upload1, _ := resumable.GetUpload(personalTransferID(bID, it1ID))

	// Hold lock
	upload1.Lock()

	// DELETE while lock held -> 423
	delReq, _ := http.NewRequest(http.MethodDelete, tusURL1, nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delReq.Header.Set("Tus-Resumable", "1.0.0")
	delRes, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delRes.Body.Close()
	if delRes.StatusCode != 423 {
		t.Fatalf("expected 423 Locked for DELETE while in flight, got %d", delRes.StatusCode)
	}

	// Client aborts in-flight PATCH -> handler unlocks lifecycleMu
	upload1.Unlock()

	// Client sends DELETE again -> succeeds with 204
	delReq2, _ := http.NewRequest(http.MethodDelete, tusURL1, nil)
	delReq2.Header.Set("Authorization", "Bearer "+token)
	delReq2.Header.Set("Tus-Resumable", "1.0.0")
	delRes2, err := http.DefaultClient.Do(delReq2)
	if err != nil {
		t.Fatal(err)
	}
	delRes2.Body.Close()
	if delRes2.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for DELETE, got %d", delRes2.StatusCode)
	}

	// Verify other item (file2.bin) remains in batch and was not canceled
	bView, err := env.coord.GetBatch(key, bID)
	if err != nil {
		t.Fatal(err)
	}
	if bView.Items[1].State == transfer.ItemStateCanceled {
		t.Fatalf("sibling item file2.bin was unexpectedly canceled: %+v", bView.Items[1])
	}
}

// TestTusGuestDropBox tests Guest Drop Box resumable endpoints and cross-guest isolation:
func TestTusGuestDropBox(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	// Guest 1 joins and is approved
	g1, err := room.Join("Alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := room.ApproveGuest(g1.ID); err != nil {
		t.Fatal(err)
	}

	// Guest 2 joins and is approved
	g2, err := room.Join("Bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := room.ApproveGuest(g2.ID); err != nil {
		t.Fatal(err)
	}

	// Guest 1 declares upload
	payload := []byte("Guest Resumable Drop Box Content")
	up, err := room.DeclareUpload(g1.ID, "report.pdf", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := room.ApproveUpload(up.ID); err != nil {
		t.Fatal(err)
	}

	guestTusURL := fmt.Sprintf("%s/guest-api/uploads/%s/tus", ts.URL, up.ID)

	// 1. Cross-guest isolation: Guest 2 attempts HEAD on Guest 1's upload -> 404
	headReqG2, _ := http.NewRequest(http.MethodHead, guestTusURL, nil)
	headReqG2.Header.Set("Authorization", "Bearer "+g2.Token)
	headReqG2.Header.Set("Tus-Resumable", "1.0.0")
	hResG2, _ := http.DefaultClient.Do(headReqG2)
	hResG2.Body.Close()
	if hResG2.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-guest isolation: expected 404 for Guest 2, got %d", hResG2.StatusCode)
	}

	// 2. Guest 1 sends POST /tus
	postReq, _ := http.NewRequest(http.MethodPost, guestTusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+g1.Token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	postRes, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postRes.Body.Close()
	if postRes.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for guest POST, got %d", postRes.StatusCode)
	}

	// 3. Guest 1 streams PATCH
	patchReq, _ := http.NewRequest(http.MethodPatch, guestTusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+g1.Token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()
	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for guest PATCH, got %d", patchRes.StatusCode)
	}

	// 4. Guest 1 sends POST /complete
	expectedHash := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedHash[:])

	completeURL := fmt.Sprintf("%s/guest-api/uploads/%s/complete", ts.URL, up.ID)
	cBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(cBody))
	compReq.Header.Set("Authorization", "Bearer "+g1.Token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	defer compRes.Body.Close()
	if compRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for guest complete, got %d", compRes.StatusCode)
	}

	// Verify room reflects completed upload
	upAfter, _ := room.GetUpload(up.ID)
	if upAfter.State != guest.UploadCompleted {
		t.Fatalf("expected room upload state %s, got %s", guest.UploadCompleted, upAfter.State)
	}
	if upAfter.SHA256 != expectedHex {
		t.Fatalf("expected room sha256 %s, got %s", expectedHex, upAfter.SHA256)
	}
}

// TestTusLegacy_PreservationOfExistingPUT verifies that legacy continuous PUT paths remain untouched:
func TestTusLegacy_PreservationOfExistingPUT(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	payload := []byte("Legacy PUT Upload Content Preservation")
	bID, itID := createTestBatch(t, env, ts, token, "legacy.bin", int64(len(payload)))

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", ts.URL, bID, itID)
	putReq, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	putReq.Header.Set("Authorization", "Bearer "+token)

	res, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("legacy PUT expected 200 OK, got %d", res.StatusCode)
	}
}

type errAfterReader struct {
	data   []byte
	err    error
	offset int
}

func (r *errAfterReader) Read(p []byte) (n int, err error) {
	if r.offset < len(r.data) {
		copied := copy(p, r.data[r.offset:])
		r.offset += copied
		return copied, nil
	}
	return 0, r.err
}

// waitUploadSettled waits until an interrupted PATCH has persisted its committed
// offset and interrupted state and has released the upload's lifecycle lock.
//
// The committed offset becomes visible while the handler still holds the lock
// (the checkpoint is written, then the state is stored, then the deferred unlock
// runs), so polling the offset alone races the unlock. Polling the lock itself
// synchronizes on handler completion instead of guessing at a delay; the state
// guard keeps the wait from returning during the window before the handler has
// acquired the lock at all.
func waitUploadSettled(t *testing.T, upload *transfer.ResumableUpload, wantOffset int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		meta := upload.Meta()
		if meta.State == transfer.ResumableInterrupted && meta.CommittedOffset == wantOffset {
			if upload.TryLock() {
				upload.Unlock()
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("upload did not settle after the interrupted PATCH: state=%q committed=%d", meta.State, meta.CommittedOffset)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestTusScenarios_1_2_3_InterruptionAndResume tests Scenarios 1, 2, 3:
// Wi-Fi cut mid-stream preserves committed bytes in .part & .meta; client discovers offset via HEAD and resumes.
func TestTusScenarios_1_2_3_InterruptionAndResume(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	payload := bytes.Repeat([]byte("X"), 20*1024) // 20 KB
	expectedDigest := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedDigest[:])

	bID, itID := createTestBatch(t, env, ts, token, "interrupted.bin", int64(len(payload)))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// POST /tus
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	pR, _ := http.DefaultClient.Do(postReq)
	pR.Body.Close()

	// PATCH 1: Simulates Wi-Fi drop at 10 KB (50%) with io.ErrUnexpectedEOF
	dropReader := &errAfterReader{
		data: payload[:10*1024],
		err:  io.ErrUnexpectedEOF,
	}
	patchReq1, _ := http.NewRequest(http.MethodPatch, tusURL, dropReader)
	patchReq1.Header.Set("Authorization", "Bearer "+token)
	patchReq1.Header.Set("Tus-Resumable", "1.0.0")
	patchReq1.Header.Set("Upload-Offset", "0")
	patchReq1.Header.Set("Content-Type", "application/offset+octet-stream")

	// The handler should exit when reading err without returning 204
	pRes1, _ := http.DefaultClient.Do(patchReq1)
	if pRes1 != nil {
		pRes1.Body.Close()
		// Status must NOT be 204
		if pRes1.StatusCode == http.StatusNoContent {
			t.Fatalf("unexpected 204 No Content on interrupted stream")
		}
	}

	// Wait for the interrupted handler to commit its checkpoint AND release the
	// upload lock. The resume below must see the released lock, not just the
	// committed offset, or it is answered with 423 on a slow filesystem.
	interrupted, ok := env.srv.Services().EnsureResumable().GetUpload(personalTransferID(bID, itID))
	if !ok {
		t.Fatal("upload not found after interrupted PATCH")
	}
	waitUploadSettled(t, interrupted, 10*1024)

	// Client re-authenticates and queries HEAD (Discovery)
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	hRes, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	hRes.Body.Close()
	if hRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on HEAD, got %d", hRes.StatusCode)
	}
	// Exactly 10 KB committed
	if off := hRes.Header.Get("Upload-Offset"); off != strconv.Itoa(10*1024) {
		t.Fatalf("expected committed offset 10 KB, got %s", off)
	}

	// Client resumes remaining 10 KB from offset 10240
	patchReq2, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[10*1024:]))
	patchReq2.Header.Set("Authorization", "Bearer "+token)
	patchReq2.Header.Set("Tus-Resumable", "1.0.0")
	patchReq2.Header.Set("Upload-Offset", strconv.Itoa(10*1024))
	patchReq2.Header.Set("Content-Type", "application/offset+octet-stream")
	pRes2, err := http.DefaultClient.Do(patchReq2)
	if err != nil {
		t.Fatal(err)
	}
	pRes2.Body.Close()

	if pRes2.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 on resume completion, got %d", pRes2.StatusCode)
	}
	if pRes2.Header.Get("Upload-Offset") != strconv.Itoa(len(payload)) {
		t.Fatalf("expected offset %d, got %s", len(payload), pRes2.Header.Get("Upload-Offset"))
	}
	if pRes2.Header.Get("Upload-State") != "awaiting_digest" {
		t.Fatalf("expected state awaiting_digest, got %s", pRes2.Header.Get("Upload-State"))
	}

	// Complete upload
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	cBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(cBody))
	compReq.Header.Set("Authorization", "Bearer "+token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	compRes.Body.Close()
	if compRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on complete, got %d", compRes.StatusCode)
	}
}

// TestTusScenario_InterruptedPatchReleasesLock is the regression test for the
// interruption/resume lock lifecycle: an interrupted PATCH must release its
// upload lock once the handler finishes, a later resume must be accepted and
// complete with 204, and a genuinely concurrent request must still get 423.
func TestTusScenario_InterruptedPatchReleasesLock(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	payload := bytes.Repeat([]byte("Y"), 16*1024)
	expectedDigest := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedDigest[:])

	bID, itID := createTestBatch(t, env, ts, token, "resume-lock.bin", int64(len(payload)))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	pR, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	pR.Body.Close()

	// Interrupt the stream after 8 KB.
	dropReader := &errAfterReader{data: payload[:8*1024], err: io.ErrUnexpectedEOF}
	patchReq1, _ := http.NewRequest(http.MethodPatch, tusURL, dropReader)
	patchReq1.Header.Set("Authorization", "Bearer "+token)
	patchReq1.Header.Set("Tus-Resumable", "1.0.0")
	patchReq1.Header.Set("Upload-Offset", "0")
	patchReq1.Header.Set("Content-Type", "application/offset+octet-stream")
	// The aborted body can surface as a transport error on the client side; the
	// handler still ran, which is what the assertions below rely on.
	pRes1, _ := http.DefaultClient.Do(patchReq1)
	if pRes1 != nil {
		pRes1.Body.Close()
		if pRes1.StatusCode == http.StatusNoContent {
			t.Fatalf("unexpected 204 No Content on interrupted stream")
		}
	}

	interrupted, ok := env.srv.Services().EnsureResumable().GetUpload(personalTransferID(bID, itID))
	if !ok {
		t.Fatal("upload not found after interrupted PATCH")
	}
	waitUploadSettled(t, interrupted, 8*1024)

	// The lock is free, so a resume from the committed offset is accepted.
	patchReq2, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[8*1024:]))
	patchReq2.Header.Set("Authorization", "Bearer "+token)
	patchReq2.Header.Set("Tus-Resumable", "1.0.0")
	patchReq2.Header.Set("Upload-Offset", strconv.Itoa(8*1024))
	patchReq2.Header.Set("Content-Type", "application/offset+octet-stream")
	pRes2, err := http.DefaultClient.Do(patchReq2)
	if err != nil {
		t.Fatal(err)
	}
	pRes2.Body.Close()
	if pRes2.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 on resumed PATCH, got %d", pRes2.StatusCode)
	}
	if off := pRes2.Header.Get("Upload-Offset"); off != strconv.Itoa(len(payload)) {
		t.Fatalf("expected offset %d after resume, got %s", len(payload), off)
	}
	if state := pRes2.Header.Get("Upload-State"); state != "awaiting_digest" {
		t.Fatalf("expected state awaiting_digest after resume, got %s", state)
	}

	// Concurrency protection is intact: while the lock is held, a PATCH is 423.
	interrupted.Lock()
	holdReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader([]byte("x")))
	holdReq.Header.Set("Authorization", "Bearer "+token)
	holdReq.Header.Set("Tus-Resumable", "1.0.0")
	holdReq.Header.Set("Upload-Offset", strconv.Itoa(len(payload)))
	holdReq.Header.Set("Content-Type", "application/offset+octet-stream")
	holdRes, err := http.DefaultClient.Do(holdReq)
	if err != nil {
		interrupted.Unlock()
		t.Fatal(err)
	}
	holdRes.Body.Close()
	interrupted.Unlock()
	if holdRes.StatusCode != 423 {
		t.Fatalf("expected 423 Locked for concurrent PATCH, got %d", holdRes.StatusCode)
	}
	if retry := holdRes.Header.Get("Retry-After"); retry != "1" {
		t.Fatalf("expected Retry-After: 1 on 423, got %q", retry)
	}

	// The resumed upload still verifies and publishes.
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	cBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(cBody))
	compReq.Header.Set("Authorization", "Bearer "+token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	compRes.Body.Close()
	if compRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on complete, got %d", compRes.StatusCode)
	}
}

// TestTusScenario_16_17_StorageQuotaRejection tests Scenarios 16 & 17:
// Quota limits reject excessive requests with 507 Insufficient Storage or 429 Too Many Requests.
func TestTusScenario_16_17_StorageQuotaRejection(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()

	// Configure small quota manager: 100 KB total simulated free space, max 2 concurrent
	resumable := env.srv.Services().EnsureResumable()
	qm := transfer.NewQuotaManager(env.store.Dir(), 1024, 2)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024, nil
	})
	// Replace quota in resumable manager
	resumable = transfer.NewResumableManager(env.store, qm)
	env.srv.Services().Resumable = resumable

	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	// Attempt 1: Upload requesting 200 KB (> 100 KB free) -> 507 Insufficient Storage
	bID1, itID1 := createTestBatch(t, env, ts, token, "toobig.bin", 200*1024)
	tusURL1 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID1, itID1)

	req1, _ := http.NewRequest(http.MethodPost, tusURL1, nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	req1.Header.Set("Tus-Resumable", "1.0.0")
	req1.Header.Set("Upload-Length", strconv.Itoa(200*1024))
	res1, _ := http.DefaultClient.Do(req1)
	res1.Body.Close()
	if res1.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 Insufficient Storage, got %d", res1.StatusCode)
	}

	// Attempt 2 & 3: Uploads within capacity (20 KB each)
	bID2, itID2 := createTestBatch(t, env, ts, token, "ok1.bin", 20*1024)
	tusURL2 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID2, itID2)
	req2, _ := http.NewRequest(http.MethodPost, tusURL2, nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Tus-Resumable", "1.0.0")
	req2.Header.Set("Upload-Length", strconv.Itoa(20*1024))
	res2, _ := http.DefaultClient.Do(req2)
	res2.Body.Close()
	if res2.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", res2.StatusCode)
	}

	bID3, itID3 := createTestBatch(t, env, ts, token, "ok2.bin", 20*1024)
	tusURL3 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID3, itID3)
	req3, _ := http.NewRequest(http.MethodPost, tusURL3, nil)
	req3.Header.Set("Authorization", "Bearer "+token)
	req3.Header.Set("Tus-Resumable", "1.0.0")
	req3.Header.Set("Upload-Length", strconv.Itoa(20*1024))
	res3, _ := http.DefaultClient.Do(req3)
	res3.Body.Close()
	if res3.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", res3.StatusCode)
	}

	// Attempt 4: 3rd concurrent upload exceeds maxConcurrent (2) -> 429 Too Many Requests
	bID4, itID4 := createTestBatch(t, env, ts, token, "overflow.bin", 20*1024)
	tusURL4 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID4, itID4)
	req4, _ := http.NewRequest(http.MethodPost, tusURL4, nil)
	req4.Header.Set("Authorization", "Bearer "+token)
	req4.Header.Set("Tus-Resumable", "1.0.0")
	req4.Header.Set("Upload-Length", strconv.Itoa(20*1024))
	res4, _ := http.DefaultClient.Do(req4)
	res4.Body.Close()
	if res4.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on concurrent cap, got %d", res4.StatusCode)
	}
}

type truncatedPayloadReader struct {
	data  []byte
	limit int
	read  int
}

func (r *truncatedPayloadReader) Read(p []byte) (int, error) {
	if r.read >= r.limit {
		return 0, io.ErrUnexpectedEOF
	}
	toCopy := len(p)
	if r.limit-r.read < toCopy {
		toCopy = r.limit - r.read
	}
	n := copy(p, r.data[r.read:r.read+toCopy])
	r.read += n
	return n, nil
}

type byteWithErrReader struct {
	data    []byte
	hasRead bool
}

func (r *byteWithErrReader) Read(p []byte) (int, error) {
	if !r.hasRead {
		r.hasRead = true
		n := copy(p, r.data)
		return n, errors.New("simulated custom non-EOF read error")
	}
	return 0, io.EOF
}

type cancelOnReadReader struct {
	data   []byte
	limit  int
	read   int
	cancel func()
}

func (r *cancelOnReadReader) Read(p []byte) (int, error) {
	if r.read < r.limit {
		toRead := len(p)
		if r.limit-r.read < toRead {
			toRead = r.limit - r.read
		}
		n := copy(p, r.data[r.read:r.read+toRead])
		r.read += n
		return n, nil
	}
	if r.cancel != nil {
		r.cancel()
	}
	return 0, context.Canceled
}

func TestTusPatch_TruncatedBodyTests(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	totalSize := int64(1000)
	fullData := make([]byte, totalSize)
	for i := range fullData {
		fullData[i] = byte((i * 17) % 251)
	}
	expectedHash := sha256.Sum256(fullData)
	expectedHex := hex.EncodeToString(expectedHash[:])

	// --- Case A: Content-Length larger than actual transmitted body ---
	bID, itID := createTestBatch(t, env, ts, token, "trunc_cl.bin", totalSize)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// POST /tus
	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", strconv.FormatInt(totalSize, 10))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()
	if pRes.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", pRes.StatusCode)
	}

	// Send PATCH with Content-Length 500, but only 300 bytes are in body
	patchReqA, _ := http.NewRequest(http.MethodPatch, tusURL, &truncatedPayloadReader{data: fullData, limit: 300})
	patchReqA.Header.Set("Authorization", "Bearer "+token)
	patchReqA.Header.Set("Tus-Resumable", "1.0.0")
	patchReqA.Header.Set("Upload-Offset", "0")
	patchReqA.Header.Set("Content-Type", "application/offset+octet-stream")
	// Declared request content length 500
	patchReqA.ContentLength = 500

	resA, err := http.DefaultClient.Do(patchReqA)
	if err == nil {
		resA.Body.Close()
		if resA.StatusCode == http.StatusNoContent {
			t.Fatal("unexpected 204 No Content for truncated PATCH body")
		}
	}

	// Wait for server to process and commit uncommitted bytes
	for i := 0; i < 100; i++ {
		if upA, ok := env.srv.Services().EnsureResumable().GetUpload(personalTransferID(bID, itID)); ok {
			if upA.Meta().CommittedOffset == 300 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Next HEAD must return authoritative committed offset (300)
	headReqA, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReqA.Header.Set("Authorization", "Bearer "+token)
	headReqA.Header.Set("Tus-Resumable", "1.0.0")
	hResA, err := http.DefaultClient.Do(headReqA)
	if err != nil {
		t.Fatal(err)
	}
	hResA.Body.Close()
	if hResA.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for HEAD, got %d", hResA.StatusCode)
	}
	offsetA := hResA.Header.Get("Upload-Offset")
	if offsetA != "300" {
		t.Fatalf("expected HEAD Upload-Offset 300, got %s", offsetA)
	}

	// Subsequent PATCH completes remaining 700 bytes (300..1000)
	patchReqA2, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(fullData[300:]))
	patchReqA2.Header.Set("Authorization", "Bearer "+token)
	patchReqA2.Header.Set("Tus-Resumable", "1.0.0")
	patchReqA2.Header.Set("Upload-Offset", "300")
	patchReqA2.Header.Set("Content-Type", "application/offset+octet-stream")
	resA2, err := http.DefaultClient.Do(patchReqA2)
	if err != nil {
		t.Fatal(err)
	}
	resA2.Body.Close()
	if resA2.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for subsequent resume PATCH, got %d", resA2.StatusCode)
	}

	// Complete Case A
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	cBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(cBody))
	compReq.Header.Set("Authorization", "Bearer "+token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	compRes.Body.Close()
	if compRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for complete, got %d", compRes.StatusCode)
	}

	// --- Case B: io.ErrUnexpectedEOF from custom reader ---
	bID_B, itID_B := createTestBatch(t, env, ts, token, "trunc_err_eof.bin", totalSize)
	tusURL_B := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID_B, itID_B)

	pReqB, _ := http.NewRequest(http.MethodPost, tusURL_B, nil)
	pReqB.Header.Set("Authorization", "Bearer "+token)
	pReqB.Header.Set("Tus-Resumable", "1.0.0")
	pReqB.Header.Set("Upload-Length", strconv.FormatInt(totalSize, 10))
	pResB, _ := http.DefaultClient.Do(pReqB)
	pResB.Body.Close()

	readerB := &truncatedPayloadReader{data: fullData, limit: 400}
	patchReqB, _ := http.NewRequest(http.MethodPatch, tusURL_B, readerB)
	patchReqB.Header.Set("Authorization", "Bearer "+token)
	patchReqB.Header.Set("Tus-Resumable", "1.0.0")
	patchReqB.Header.Set("Upload-Offset", "0")
	patchReqB.Header.Set("Content-Type", "application/offset+octet-stream")
	resB, err := http.DefaultClient.Do(patchReqB)
	if err == nil {
		resB.Body.Close()
		if resB.StatusCode == http.StatusNoContent {
			t.Fatal("unexpected 204 No Content for io.ErrUnexpectedEOF")
		}
	}

	// Wait for server to process and commit uncommitted bytes
	for i := 0; i < 100; i++ {
		if upB, ok := env.srv.Services().EnsureResumable().GetUpload(personalTransferID(bID_B, itID_B)); ok {
			if upB.Meta().CommittedOffset == 400 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	headReqB, _ := http.NewRequest(http.MethodHead, tusURL_B, nil)
	headReqB.Header.Set("Authorization", "Bearer "+token)
	headReqB.Header.Set("Tus-Resumable", "1.0.0")
	hResB, _ := http.DefaultClient.Do(headReqB)
	hResB.Body.Close()
	if hResB.Header.Get("Upload-Offset") != "400" {
		t.Fatalf("expected HEAD offset 400 after ErrUnexpectedEOF, got %s", hResB.Header.Get("Upload-Offset"))
	}

	// --- Case C: Read returning n > 0 with non-nil error ---
	bID_C, itID_C := createTestBatch(t, env, ts, token, "trunc_n_err.bin", totalSize)
	tusURL_C := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID_C, itID_C)

	pReqC, _ := http.NewRequest(http.MethodPost, tusURL_C, nil)
	pReqC.Header.Set("Authorization", "Bearer "+token)
	pReqC.Header.Set("Tus-Resumable", "1.0.0")
	pReqC.Header.Set("Upload-Length", strconv.FormatInt(totalSize, 10))
	pResC, _ := http.DefaultClient.Do(pReqC)
	pResC.Body.Close()

	readerC := &byteWithErrReader{data: fullData[:250]}
	patchReqC, _ := http.NewRequest(http.MethodPatch, tusURL_C, readerC)
	patchReqC.Header.Set("Authorization", "Bearer "+token)
	patchReqC.Header.Set("Tus-Resumable", "1.0.0")
	patchReqC.Header.Set("Upload-Offset", "0")
	patchReqC.Header.Set("Content-Type", "application/offset+octet-stream")
	resC, err := http.DefaultClient.Do(patchReqC)
	if err == nil {
		resC.Body.Close()
		if resC.StatusCode == http.StatusNoContent {
			t.Fatal("unexpected 204 No Content for n > 0 with non-nil error")
		}
	}

	// Wait for server to process and commit uncommitted bytes
	for i := 0; i < 100; i++ {
		if upC, ok := env.srv.Services().EnsureResumable().GetUpload(personalTransferID(bID_C, itID_C)); ok {
			if upC.Meta().CommittedOffset == 250 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	headReqC, _ := http.NewRequest(http.MethodHead, tusURL_C, nil)
	headReqC.Header.Set("Authorization", "Bearer "+token)
	headReqC.Header.Set("Tus-Resumable", "1.0.0")
	hResC, _ := http.DefaultClient.Do(headReqC)
	hResC.Body.Close()
	if hResC.Header.Get("Upload-Offset") != "250" {
		t.Fatalf("expected HEAD offset 250 after n > 0 read error, got %s", hResC.Header.Get("Upload-Offset"))
	}

	// --- Case D: Request context cancellation mid-stream ---
	bID_D, itID_D := createTestBatch(t, env, ts, token, "trunc_ctx_cancel.bin", totalSize)
	tusURL_D := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID_D, itID_D)

	pReqD, _ := http.NewRequest(http.MethodPost, tusURL_D, nil)
	pReqD.Header.Set("Authorization", "Bearer "+token)
	pReqD.Header.Set("Tus-Resumable", "1.0.0")
	pReqD.Header.Set("Upload-Length", strconv.FormatInt(totalSize, 10))
	pResD, _ := http.DefaultClient.Do(pReqD)
	pResD.Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	readerD := &cancelOnReadReader{data: fullData, limit: 350, cancel: cancel}

	patchReqD, _ := http.NewRequestWithContext(ctx, http.MethodPatch, tusURL_D, readerD)
	patchReqD.Header.Set("Authorization", "Bearer "+token)
	patchReqD.Header.Set("Tus-Resumable", "1.0.0")
	patchReqD.Header.Set("Upload-Offset", "0")
	patchReqD.Header.Set("Content-Type", "application/offset+octet-stream")
	resD, _ := http.DefaultClient.Do(patchReqD)
	if resD != nil {
		resD.Body.Close()
		if resD.StatusCode == http.StatusNoContent {
			t.Fatal("unexpected 204 No Content for context cancellation mid-stream")
		}
	}

	// Wait for server to process and commit uncommitted bytes
	for i := 0; i < 100; i++ {
		if upD, ok := env.srv.Services().EnsureResumable().GetUpload(personalTransferID(bID_D, itID_D)); ok {
			if upD.Meta().CommittedOffset == 350 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	headReqD, _ := http.NewRequest(http.MethodHead, tusURL_D, nil)
	headReqD.Header.Set("Authorization", "Bearer "+token)
	headReqD.Header.Set("Tus-Resumable", "1.0.0")
	hResD, err := http.DefaultClient.Do(headReqD)
	if err != nil {
		t.Fatal(err)
	}
	hResD.Body.Close()
	if hResD.Header.Get("Upload-Offset") != "350" {
		t.Fatalf("expected HEAD offset 350 after context cancellation, got %s", hResD.Header.Get("Upload-Offset"))
	}
}

func TestTusSessionRevocationAndIsolation(t *testing.T) {
	env := newTestEnv(t)
	tokenA := env.token()
	tokenB := env.token() // Separate active session
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	// 1. Session A creates batch and tus upload
	bID, itID := createTestBatch(t, env, ts, tokenA, "sec_test.bin", 1000)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	statusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/status", ts.URL, bID, itID)

	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+tokenA)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", "1000")
	pR, _ := http.DefaultClient.Do(pReq)
	pR.Body.Close()
	if pR.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", pR.StatusCode)
	}

	// 2. Cross-Session Isolation: Session B attempts to manipulate Session A's upload -> All MUST return 404 Not Found!
	// HEAD
	hReqB, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	hReqB.Header.Set("Authorization", "Bearer "+tokenB)
	hReqB.Header.Set("Tus-Resumable", "1.0.0")
	hResB, _ := http.DefaultClient.Do(hReqB)
	hResB.Body.Close()
	if hResB.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-session: expected 404 for HEAD with Session B, got %d", hResB.StatusCode)
	}

	// PATCH
	ptReqB, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader([]byte("hack")))
	ptReqB.Header.Set("Authorization", "Bearer "+tokenB)
	ptReqB.Header.Set("Tus-Resumable", "1.0.0")
	ptReqB.Header.Set("Upload-Offset", "0")
	ptReqB.Header.Set("Content-Type", "application/offset+octet-stream")
	ptResB, _ := http.DefaultClient.Do(ptReqB)
	ptResB.Body.Close()
	if ptResB.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-session: expected 404 for PATCH with Session B, got %d", ptResB.StatusCode)
	}

	// COMPLETE
	cReqB, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader([]byte(`{"expectedSHA256":"0000000000000000000000000000000000000000000000000000000000000000"}`)))
	cReqB.Header.Set("Authorization", "Bearer "+tokenB)
	cReqB.Header.Set("Content-Type", "application/json")
	cResB, _ := http.DefaultClient.Do(cReqB)
	cResB.Body.Close()
	if cResB.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-session: expected 404 for complete with Session B, got %d", cResB.StatusCode)
	}

	// DELETE
	dReqB, _ := http.NewRequest(http.MethodDelete, tusURL, nil)
	dReqB.Header.Set("Authorization", "Bearer "+tokenB)
	dReqB.Header.Set("Tus-Resumable", "1.0.0")
	dResB, _ := http.DefaultClient.Do(dReqB)
	dResB.Body.Close()
	if dResB.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-session: expected 404 for DELETE with Session B, got %d", dResB.StatusCode)
	}

	// STATUS
	sReqB, _ := http.NewRequest(http.MethodGet, statusURL, nil)
	sReqB.Header.Set("Authorization", "Bearer "+tokenB)
	sResB, _ := http.DefaultClient.Do(sReqB)
	sResB.Body.Close()
	if sResB.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-session: expected 404 for status with Session B, got %d", sResB.StatusCode)
	}

	// 3. Session Revocation: Revoke Session A -> All requests with Token A MUST return 401 Unauthorized!
	if err := env.sess.Revoke(tokenA); err != nil {
		t.Fatalf("Revoke session failed: %v", err)
	}

	// HEAD with revoked token
	hReqRev, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	hReqRev.Header.Set("Authorization", "Bearer "+tokenA)
	hReqRev.Header.Set("Tus-Resumable", "1.0.0")
	hResRev, _ := http.DefaultClient.Do(hReqRev)
	hResRev.Body.Close()
	if hResRev.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session revocation: expected 401 for HEAD, got %d", hResRev.StatusCode)
	}

	// PATCH with revoked token
	ptReqRev, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader([]byte("data")))
	ptReqRev.Header.Set("Authorization", "Bearer "+tokenA)
	ptReqRev.Header.Set("Tus-Resumable", "1.0.0")
	ptReqRev.Header.Set("Upload-Offset", "0")
	ptReqRev.Header.Set("Content-Type", "application/offset+octet-stream")
	ptResRev, _ := http.DefaultClient.Do(ptReqRev)
	ptResRev.Body.Close()
	if ptResRev.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session revocation: expected 401 for PATCH, got %d", ptResRev.StatusCode)
	}

	// COMPLETE with revoked token
	cReqRev, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader([]byte(`{"expectedSHA256":"0000000000000000000000000000000000000000000000000000000000000000"}`)))
	cReqRev.Header.Set("Authorization", "Bearer "+tokenA)
	cReqRev.Header.Set("Content-Type", "application/json")
	cResRev, _ := http.DefaultClient.Do(cReqRev)
	cResRev.Body.Close()
	if cResRev.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session revocation: expected 401 for complete, got %d", cResRev.StatusCode)
	}

	// DELETE with revoked token
	dReqRev, _ := http.NewRequest(http.MethodDelete, tusURL, nil)
	dReqRev.Header.Set("Authorization", "Bearer "+tokenA)
	dReqRev.Header.Set("Tus-Resumable", "1.0.0")
	dResRev, _ := http.DefaultClient.Do(dReqRev)
	dResRev.Body.Close()
	if dResRev.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session revocation: expected 401 for DELETE, got %d", dResRev.StatusCode)
	}

	// STATUS with revoked token
	sReqRev, _ := http.NewRequest(http.MethodGet, statusURL, nil)
	sReqRev.Header.Set("Authorization", "Bearer "+tokenA)
	sResRev, _ := http.DefaultClient.Do(sReqRev)
	sResRev.Body.Close()
	if sResRev.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session revocation: expected 401 for status, got %d", sResRev.StatusCode)
	}

	// 4. Guest Drop Box Revocation and Cross-Guest Isolation
	gts, _, room, _ := setupTestGuestServer(t)
	gAlice, _ := room.Join("Alice")
	_ = room.ApproveGuest(gAlice.ID)
	gBob, _ := room.Join("Bob")
	_ = room.ApproveGuest(gBob.ID)

	upAlice, _ := room.DeclareUpload(gAlice.ID, "alice.pdf", 500)
	_ = room.ApproveUpload(upAlice.ID)

	gTusURL := fmt.Sprintf("%s/guest-api/uploads/%s/tus", gts.URL, upAlice.ID)
	gCompURL := fmt.Sprintf("%s/guest-api/uploads/%s/complete", gts.URL, upAlice.ID)
	gStatURL := fmt.Sprintf("%s/guest-api/uploads/%s/status", gts.URL, upAlice.ID)

	// Create upload as Alice
	gpReq, _ := http.NewRequest(http.MethodPost, gTusURL, nil)
	gpReq.Header.Set("Authorization", "Bearer "+gAlice.Token)
	gpReq.Header.Set("Tus-Resumable", "1.0.0")
	gpReq.Header.Set("Upload-Length", "500")
	gpR, _ := http.DefaultClient.Do(gpReq)
	gpR.Body.Close()
	if gpR.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for guest Alice, got %d", gpR.StatusCode)
	}

	// Guest Bob attempts on Alice's upload -> All 404
	for method, u := range map[string]string{
		http.MethodHead:   gTusURL,
		http.MethodPatch:  gTusURL,
		http.MethodDelete: gTusURL,
		http.MethodPost:   gCompURL,
		http.MethodGet:    gStatURL,
	} {
		req, _ := http.NewRequest(method, u, bytes.NewReader([]byte(`{"expectedSHA256":"0000000000000000000000000000000000000000000000000000000000000000"}`)))
		req.Header.Set("Authorization", "Bearer "+gBob.Token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Offset", "0")
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		res, _ := http.DefaultClient.Do(req)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("cross-guest isolation: expected 404 for Bob %s %s, got %d", method, u, res.StatusCode)
		}
	}

	// Revoke Alice
	room.RevokeGuest(gAlice.ID)

	// Alice attempts on her upload -> All 401
	for method, u := range map[string]string{
		http.MethodHead:   gTusURL,
		http.MethodPatch:  gTusURL,
		http.MethodDelete: gTusURL,
		http.MethodPost:   gCompURL,
		http.MethodGet:    gStatURL,
	} {
		req, _ := http.NewRequest(method, u, bytes.NewReader([]byte(`{"expectedSHA256":"0000000000000000000000000000000000000000000000000000000000000000"}`)))
		req.Header.Set("Authorization", "Bearer "+gAlice.Token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Offset", "0")
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		res, _ := http.DefaultClient.Do(req)
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("guest revocation: expected 401 for revoked Alice %s %s, got %d", method, u, res.StatusCode)
		}
	}
}

func TestTusHttp_CheckpointFailures_Scenarios32_33_47(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	totalSize := int64(2000)
	fullData := make([]byte, totalSize)
	for i := range fullData {
		fullData[i] = byte((i * 19) % 251)
	}

	resumable := env.srv.Services().EnsureResumable()

	// --- Scenario 32 via HTTP: partFile.Sync failure ---
	bID32, itID32 := createTestBatch(t, env, ts, token, "http_s32.bin", totalSize)
	tusURL32 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID32, itID32)

	pReq, _ := http.NewRequest(http.MethodPost, tusURL32, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", strconv.FormatInt(totalSize, 10))
	pR, _ := http.DefaultClient.Do(pReq)
	pR.Body.Close()

	// Commit 500 bytes successfully
	pt1, _ := http.NewRequest(http.MethodPatch, tusURL32, bytes.NewReader(fullData[:500]))
	pt1.Header.Set("Authorization", "Bearer "+token)
	pt1.Header.Set("Tus-Resumable", "1.0.0")
	pt1.Header.Set("Upload-Offset", "0")
	pt1.Header.Set("Content-Type", "application/offset+octet-stream")
	res1, _ := http.DefaultClient.Do(pt1)
	res1.Body.Close()
	if res1.StatusCode != http.StatusNoContent {
		t.Fatalf("first 500 bytes PATCH failed: %d", res1.StatusCode)
	}

	// Inject partFile.Sync failure
	resumable.SetPartFileSyncHook(func(f *os.File) error {
		return errors.New("injected http sync failure")
	})

	// Send next 500 bytes (500..1000)
	pt2, _ := http.NewRequest(http.MethodPatch, tusURL32, bytes.NewReader(fullData[500:1000]))
	pt2.Header.Set("Authorization", "Bearer "+token)
	pt2.Header.Set("Tus-Resumable", "1.0.0")
	pt2.Header.Set("Upload-Offset", "500")
	pt2.Header.Set("Content-Type", "application/offset+octet-stream")
	res2, _ := http.DefaultClient.Do(pt2)
	res2.Body.Close()
	// Must NOT be 204
	if res2.StatusCode == http.StatusNoContent {
		t.Fatal("unexpected 204 No Content when partFile.Sync fails")
	}
	if res2.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 Internal Server Error, got %d", res2.StatusCode)
	}

	// HEAD must return previous committed offset (500)
	headReq, _ := http.NewRequest(http.MethodHead, tusURL32, nil)
	headReq.Header.Set("Authorization", "Bearer "+token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headRes, _ := http.DefaultClient.Do(headReq)
	headRes.Body.Close()
	if headRes.Header.Get("Upload-Offset") != "500" {
		t.Fatalf("expected HEAD offset 500, got %s", headRes.Header.Get("Upload-Offset"))
	}

	resumable.SetPartFileSyncHook(nil)

	// --- Scenario 33 via HTTP: meta rename failure ---
	bID33, itID33 := createTestBatch(t, env, ts, token, "http_s33.bin", totalSize)
	tusURL33 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID33, itID33)

	pReq33, _ := http.NewRequest(http.MethodPost, tusURL33, nil)
	pReq33.Header.Set("Authorization", "Bearer "+token)
	pReq33.Header.Set("Tus-Resumable", "1.0.0")
	pReq33.Header.Set("Upload-Length", strconv.FormatInt(totalSize, 10))
	pR33, _ := http.DefaultClient.Do(pReq33)
	pR33.Body.Close()

	// Commit 600 bytes
	pt33_1, _ := http.NewRequest(http.MethodPatch, tusURL33, bytes.NewReader(fullData[:600]))
	pt33_1.Header.Set("Authorization", "Bearer "+token)
	pt33_1.Header.Set("Tus-Resumable", "1.0.0")
	pt33_1.Header.Set("Upload-Offset", "0")
	pt33_1.Header.Set("Content-Type", "application/offset+octet-stream")
	res33_1, _ := http.DefaultClient.Do(pt33_1)
	res33_1.Body.Close()
	if res33_1.StatusCode != http.StatusNoContent {
		t.Fatalf("pt33_1 failed: %d", res33_1.StatusCode)
	}

	// Inject meta rename failure
	resumable.SetMetaRenameHook(func(oldPath, newPath string) error {
		return errors.New("injected http meta rename failure")
	})

	pt33_2, _ := http.NewRequest(http.MethodPatch, tusURL33, bytes.NewReader(fullData[600:1000]))
	pt33_2.Header.Set("Authorization", "Bearer "+token)
	pt33_2.Header.Set("Tus-Resumable", "1.0.0")
	pt33_2.Header.Set("Upload-Offset", "600")
	pt33_2.Header.Set("Content-Type", "application/offset+octet-stream")
	res33_2, _ := http.DefaultClient.Do(pt33_2)
	res33_2.Body.Close()
	if res33_2.StatusCode == http.StatusNoContent {
		t.Fatal("unexpected 204 No Content when meta rename fails")
	}

	// HEAD must return 600
	headReq33, _ := http.NewRequest(http.MethodHead, tusURL33, nil)
	headReq33.Header.Set("Authorization", "Bearer "+token)
	headReq33.Header.Set("Tus-Resumable", "1.0.0")
	headRes33, _ := http.DefaultClient.Do(headReq33)
	headRes33.Body.Close()
	if headRes33.Header.Get("Upload-Offset") != "600" {
		t.Fatalf("expected HEAD offset 600, got %s", headRes33.Header.Get("Upload-Offset"))
	}

	resumable.SetMetaRenameHook(nil)

	// --- Scenario 47 via HTTP: final EOF checkpoint failure ---
	bID47, itID47 := createTestBatch(t, env, ts, token, "http_s47.bin", 1000)
	tusURL47 := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID47, itID47)

	pReq47, _ := http.NewRequest(http.MethodPost, tusURL47, nil)
	pReq47.Header.Set("Authorization", "Bearer "+token)
	pReq47.Header.Set("Tus-Resumable", "1.0.0")
	pReq47.Header.Set("Upload-Length", "1000")
	pR47, _ := http.DefaultClient.Do(pReq47)
	pR47.Body.Close()

	// Commit 500 bytes
	pt47_1, _ := http.NewRequest(http.MethodPatch, tusURL47, bytes.NewReader(fullData[:500]))
	pt47_1.Header.Set("Authorization", "Bearer "+token)
	pt47_1.Header.Set("Tus-Resumable", "1.0.0")
	pt47_1.Header.Set("Upload-Offset", "0")
	pt47_1.Header.Set("Content-Type", "application/offset+octet-stream")
	res47_1, _ := http.DefaultClient.Do(pt47_1)
	res47_1.Body.Close()
	if res47_1.StatusCode != http.StatusNoContent {
		t.Fatalf("pt47_1 failed: %d", res47_1.StatusCode)
	}

	// Inject partFile.Sync hook on EOF
	resumable.SetPartFileSyncHook(func(f *os.File) error {
		return errors.New("injected EOF sync failure")
	})

	pt47_2, _ := http.NewRequest(http.MethodPatch, tusURL47, bytes.NewReader(fullData[500:1000]))
	pt47_2.Header.Set("Authorization", "Bearer "+token)
	pt47_2.Header.Set("Tus-Resumable", "1.0.0")
	pt47_2.Header.Set("Upload-Offset", "500")
	pt47_2.Header.Set("Content-Type", "application/offset+octet-stream")
	res47_2, _ := http.DefaultClient.Do(pt47_2)
	res47_2.Body.Close()
	if res47_2.StatusCode == http.StatusNoContent {
		t.Fatal("unexpected 204 No Content when final EOF checkpoint fails")
	}

	// HEAD must return 500, state must not be awaiting_digest
	headReq47, _ := http.NewRequest(http.MethodHead, tusURL47, nil)
	headReq47.Header.Set("Authorization", "Bearer "+token)
	headReq47.Header.Set("Tus-Resumable", "1.0.0")
	headRes47, _ := http.DefaultClient.Do(headReq47)
	headRes47.Body.Close()
	if headRes47.Header.Get("Upload-Offset") != "500" {
		t.Fatalf("expected HEAD offset 500, got %s", headRes47.Header.Get("Upload-Offset"))
	}
	if headRes47.Header.Get("Upload-State") == "awaiting_digest" {
		t.Fatal("state must NOT be awaiting_digest after EOF checkpoint failure")
	}

	resumable.SetPartFileSyncHook(nil)

	// Retry final 500 bytes cleanly
	pt47_3, _ := http.NewRequest(http.MethodPatch, tusURL47, bytes.NewReader(fullData[500:1000]))
	pt47_3.Header.Set("Authorization", "Bearer "+token)
	pt47_3.Header.Set("Tus-Resumable", "1.0.0")
	pt47_3.Header.Set("Upload-Offset", "500")
	pt47_3.Header.Set("Content-Type", "application/offset+octet-stream")
	res47_3, _ := http.DefaultClient.Do(pt47_3)
	res47_3.Body.Close()
	if res47_3.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for clean retry PATCH, got %d", res47_3.StatusCode)
	}

	// --- Cross-Filesystem Destination Rejection via HTTP ---
	resumable.SetSameFsCheck(func(p1, p2 string) (bool, error) {
		return false, nil
	})
	bID_X, itID_X := createTestBatch(t, env, ts, token, "xfs.bin", 100)
	tusURL_X := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID_X, itID_X)

	pReqX, _ := http.NewRequest(http.MethodPost, tusURL_X, nil)
	pReqX.Header.Set("Authorization", "Bearer "+token)
	pReqX.Header.Set("Tus-Resumable", "1.0.0")
	pReqX.Header.Set("Upload-Length", "100")
	resX, _ := http.DefaultClient.Do(pReqX)
	resX.Body.Close()
	if resX.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for cross-filesystem upload, got %d", resX.StatusCode)
	}
}

func TestTusUploadPrefixDigestExposureAndIntegrity(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	// 1. Create an upload of 2000 bytes
	totalBytes := int64(2000)
	fullData := bytes.Repeat([]byte("PrefixDigestTestData1234567890"), 100)[:totalBytes]
	h1000 := sha256.Sum256(fullData[:1000])
	prefix1000Digest := hex.EncodeToString(h1000[:])
	hFull := sha256.Sum256(fullData)
	fullFileDigest := hex.EncodeToString(hFull[:])

	bID, itID := createTestBatch(t, env, ts, token, "prefix_test.bin", totalBytes)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// Check OPTIONS exposes Upload-Prefix-Digest in Access-Control-Expose-Headers
	optReq, _ := http.NewRequest(http.MethodOptions, tusURL, nil)
	optReq.Header.Set("Authorization", "Bearer "+token)
	optRes, err := http.DefaultClient.Do(optReq)
	if err != nil {
		t.Fatal(err)
	}
	optRes.Body.Close()
	if optRes.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for OPTIONS, got %d", optRes.StatusCode)
	}
	exposeHeaders := optRes.Header.Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposeHeaders, "Upload-Prefix-Digest") {
		t.Fatalf("expected Upload-Prefix-Digest in Access-Control-Expose-Headers, got: %s", exposeHeaders)
	}

	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", strconv.FormatInt(totalBytes, 10))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()
	if pRes.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", pRes.StatusCode)
	}
	if pRes.Header.Get("Upload-Prefix-Digest") != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("expected initial empty digest on Create, got %s", pRes.Header.Get("Upload-Prefix-Digest"))
	}

	// 3. HEAD at offset 0 must report empty sha256
	hReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	hReq.Header.Set("Authorization", "Bearer "+token)
	hReq.Header.Set("Tus-Resumable", "1.0.0")
	hRes, err := http.DefaultClient.Do(hReq)
	if err != nil {
		t.Fatal(err)
	}
	hRes.Body.Close()
	if hRes.Header.Get("Upload-Prefix-Digest") != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("expected empty sha256 at offset 0 HEAD, got: %s", hRes.Header.Get("Upload-Prefix-Digest"))
	}

	// 4. Send first 1000 bytes
	ptReq1, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(fullData[:1000]))
	ptReq1.Header.Set("Authorization", "Bearer "+token)
	ptReq1.Header.Set("Tus-Resumable", "1.0.0")
	ptReq1.Header.Set("Upload-Offset", "0")
	ptReq1.Header.Set("Content-Type", "application/offset+octet-stream")
	ptRes1, err := http.DefaultClient.Do(ptReq1)
	if err != nil {
		t.Fatal(err)
	}
	ptRes1.Body.Close()
	if ptRes1.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for first PATCH, got %d", ptRes1.StatusCode)
	}
	if ptRes1.Header.Get("Upload-Prefix-Digest") != prefix1000Digest {
		t.Fatalf("expected PATCH 204 Upload-Prefix-Digest %s, got %s", prefix1000Digest, ptRes1.Header.Get("Upload-Prefix-Digest"))
	}

	// 5. Query HEAD: must expose Upload-Prefix-Digest matching prefix1000Digest
	hReq2, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	hReq2.Header.Set("Authorization", "Bearer "+token)
	hReq2.Header.Set("Tus-Resumable", "1.0.0")
	hRes2, err := http.DefaultClient.Do(hReq2)
	if err != nil {
		t.Fatal(err)
	}
	hRes2.Body.Close()
	if hRes2.Header.Get("Upload-Offset") != "1000" {
		t.Fatalf("expected offset 1000, got %s", hRes2.Header.Get("Upload-Offset"))
	}
	if hRes2.Header.Get("Upload-Prefix-Digest") != prefix1000Digest {
		t.Fatalf("expected HEAD Upload-Prefix-Digest %s, got %s", prefix1000Digest, hRes2.Header.Get("Upload-Prefix-Digest"))
	}

	// 6. Inject sync failure on subsequent chunk; verify Upload-Prefix-Digest is NOT advanced
	resumable := env.srv.Services().EnsureResumable()
	resumable.SetPartFileSyncHook(func(f *os.File) error {
		return errors.New("simulated fsync failure")
	})

	ptReqFail, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(fullData[1000:1500]))
	ptReqFail.Header.Set("Authorization", "Bearer "+token)
	ptReqFail.Header.Set("Tus-Resumable", "1.0.0")
	ptReqFail.Header.Set("Upload-Offset", "1000")
	ptReqFail.Header.Set("Content-Type", "application/offset+octet-stream")
	ptResFail, err := http.DefaultClient.Do(ptReqFail)
	if err != nil {
		t.Fatal(err)
	}
	ptResFail.Body.Close()
	if ptResFail.StatusCode == http.StatusNoContent {
		t.Fatal("expected failure on fsync error, got 204")
	}

	// Query HEAD: offset and digest must remain at 1000 and prefix1000Digest
	hReq3, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	hReq3.Header.Set("Authorization", "Bearer "+token)
	hReq3.Header.Set("Tus-Resumable", "1.0.0")
	hRes3, err := http.DefaultClient.Do(hReq3)
	if err != nil {
		t.Fatal(err)
	}
	hRes3.Body.Close()
	if hRes3.Header.Get("Upload-Offset") != "1000" {
		t.Fatalf("expected offset to stay 1000 after failed checkpoint, got %s", hRes3.Header.Get("Upload-Offset"))
	}
	if hRes3.Header.Get("Upload-Prefix-Digest") != prefix1000Digest {
		t.Fatalf("expected prefix digest not advanced after failure, got %s", hRes3.Header.Get("Upload-Prefix-Digest"))
	}

	resumable.SetPartFileSyncHook(nil)

	// 7. Finish the rest of the stream (1000 to 2000)
	ptReqClean, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(fullData[1000:2000]))
	ptReqClean.Header.Set("Authorization", "Bearer "+token)
	ptReqClean.Header.Set("Tus-Resumable", "1.0.0")
	ptReqClean.Header.Set("Upload-Offset", "1000")
	ptReqClean.Header.Set("Content-Type", "application/offset+octet-stream")
	ptResClean, err := http.DefaultClient.Do(ptReqClean)
	if err != nil {
		t.Fatal(err)
	}
	ptResClean.Body.Close()
	if ptResClean.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 on clean finish, got %d", ptResClean.StatusCode)
	}
	if ptResClean.Header.Get("Upload-Prefix-Digest") != fullFileDigest {
		t.Fatalf("expected full file digest on EOF, got %s", ptResClean.Header.Get("Upload-Prefix-Digest"))
	}

	// 8. Query HEAD at full completion: must return fullFileDigest
	hReq4, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	hReq4.Header.Set("Authorization", "Bearer "+token)
	hReq4.Header.Set("Tus-Resumable", "1.0.0")
	hRes4, err := http.DefaultClient.Do(hReq4)
	if err != nil {
		t.Fatal(err)
	}
	hRes4.Body.Close()
	if hRes4.Header.Get("Upload-Offset") != "2000" {
		t.Fatalf("expected offset 2000, got %s", hRes4.Header.Get("Upload-Offset"))
	}
	if hRes4.Header.Get("Upload-Prefix-Digest") != fullFileDigest {
		t.Fatalf("expected HEAD full file digest %s, got %s", fullFileDigest, hRes4.Header.Get("Upload-Prefix-Digest"))
	}

	// 9. Guest Drop Box verification of Upload-Prefix-Digest
	gTs, _, room, _ := setupTestGuestServer(t)
	g1, _ := room.Join("Alice")
	_ = room.ApproveGuest(g1.ID)

	gPayload := []byte("Guest prefix digest test data 1234567890")
	gUp, _ := room.DeclareUpload(g1.ID, "guest_prefix.dat", int64(len(gPayload)))
	_ = room.ApproveUpload(gUp.ID)

	guestTusURL := fmt.Sprintf("%s/guest-api/uploads/%s/tus", gTs.URL, gUp.ID)
	gPostReq, _ := http.NewRequest(http.MethodPost, guestTusURL, nil)
	gPostReq.Header.Set("Authorization", "Bearer "+g1.Token)
	gPostReq.Header.Set("Tus-Resumable", "1.0.0")
	gPostReq.Header.Set("Upload-Length", strconv.Itoa(len(gPayload)))
	gPostRes, err := http.DefaultClient.Do(gPostReq)
	if err != nil {
		t.Fatal(err)
	}
	gPostRes.Body.Close()
	if gPostRes.Header.Get("Upload-Prefix-Digest") != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("expected guest initial empty digest on Create, got %s", gPostRes.Header.Get("Upload-Prefix-Digest"))
	}

	// PATCH half bytes
	gHalf := len(gPayload) / 2
	gPtReq, _ := http.NewRequest(http.MethodPatch, guestTusURL, bytes.NewReader(gPayload[:gHalf]))
	gPtReq.Header.Set("Authorization", "Bearer "+g1.Token)
	gPtReq.Header.Set("Tus-Resumable", "1.0.0")
	gPtReq.Header.Set("Upload-Offset", "0")
	gPtReq.Header.Set("Content-Type", "application/offset+octet-stream")
	gPtRes, err := http.DefaultClient.Do(gPtReq)
	if err != nil {
		t.Fatal(err)
	}
	gPtRes.Body.Close()
	gHalfDigest := sha256.Sum256(gPayload[:gHalf])
	expectedGDigest := hex.EncodeToString(gHalfDigest[:])
	if gPtRes.Header.Get("Upload-Prefix-Digest") != expectedGDigest {
		t.Fatalf("expected guest PATCH Upload-Prefix-Digest %s, got %s", expectedGDigest, gPtRes.Header.Get("Upload-Prefix-Digest"))
	}

	// HEAD query on guest upload
	gHeadReq, _ := http.NewRequest(http.MethodHead, guestTusURL, nil)
	gHeadReq.Header.Set("Authorization", "Bearer "+g1.Token)
	gHeadReq.Header.Set("Tus-Resumable", "1.0.0")
	gHeadRes, err := http.DefaultClient.Do(gHeadReq)
	if err != nil {
		t.Fatal(err)
	}
	gHeadRes.Body.Close()
	if gHeadRes.Header.Get("Upload-Prefix-Digest") != expectedGDigest {
		t.Fatalf("expected guest HEAD Upload-Prefix-Digest %s, got %s", expectedGDigest, gHeadRes.Header.Get("Upload-Prefix-Digest"))
	}
}

// declareBatchWithoutApproval posts a batch manifest and returns its view
// WITHOUT approving it, so tests can exercise the desktop-consent gate on
// the resumable path.
func declareBatchWithoutApproval(t *testing.T, ts *httptest.Server, token string, itemName string, itemSize int64) (batchID, itemID string) {
	t.Helper()
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{
				RelativePath: itemName,
				Size:         itemSize,
			},
		},
	}
	body, _ := json.Marshal(manifest)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/batches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create batch failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", res.StatusCode)
	}
	var view transfer.BatchView
	if err := json.NewDecoder(res.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.ApprovalState != transfer.ApprovalPending {
		t.Fatalf("expected pending approval, got %q", view.ApprovalState)
	}
	return view.ID, view.Items[0].ID
}

// TestTusCreate_RejectsUnapprovedBatch proves the resumable path cannot
// bypass desktop consent: tus create on a pending batch is refused with
// 409, nothing lands in the receive directory, and approving the batch
// unblocks the same request (the legitimate client waits for approval
// before creating uploads, so the happy path is unchanged).
func TestTusCreate_RejectsUnapprovedBatch(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	bID, itID := declareBatchWithoutApproval(t, ts, token, "unapproved.bin", 2048)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	req, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "2048")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for unapproved batch, got %d", res.StatusCode)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errBody.Error, "approval") {
		t.Fatalf("expected approval-related error, got %q", errBody.Error)
	}
	for _, name := range receiveDirNames(t, env.store) {
		if name == transfer.StagingDirName {
			continue // staging infrastructure, not published content
		}
		t.Fatalf("unapproved batch published files: %v", name)
	}

	// Approval must unblock the identical request.
	key := env.tokenKey(token)
	if _, err := env.coord.ApproveBatch(key, bID); err != nil {
		t.Fatalf("ApproveBatch failed: %v", err)
	}
	req2, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Tus-Resumable", "1.0.0")
	req2.Header.Set("Upload-Length", "2048")
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created after approval, got %d", res2.StatusCode)
	}
}

// TestTusComplete_RejectsUnapprovedBatch proves completion cannot publish
// content for a batch whose approval was withdrawn: the file must stay in
// staging and the receive directory must remain empty.
func TestTusComplete_RejectsUnapprovedBatch(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	key := env.tokenKey(token)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	bID, itID := declareBatchWithoutApproval(t, ts, token, "withdrawn.bin", 2048)
	if _, err := env.coord.ApproveBatch(key, bID); err != nil {
		t.Fatalf("ApproveBatch failed: %v", err)
	}
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)

	// Create the upload while approved.
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
	req, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", "2048")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", res.StatusCode)
	}

	// Withdraw approval by rejecting the batch, then attempt completion.
	if _, err := env.coord.RejectBatch(key, bID); err != nil {
		t.Fatalf("RejectBatch failed: %v", err)
	}
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, strings.NewReader(`{"expectedSHA256":"`+strings.Repeat("a", 64)+`"}`))
	compReq.Header.Set("Authorization", "Bearer "+token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	defer compRes.Body.Close()
	if compRes.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for rejected batch completion, got %d", compRes.StatusCode)
	}
	for _, name := range receiveDirNames(t, env.store) {
		if name == transfer.StagingDirName {
			continue // staging infrastructure, not published content
		}
		t.Fatalf("rejected batch published files: %v", name)
	}
}

// ---------------------------------------------------------------------------
// WP4 dedicated scenario tests: 5, 7, 9, 16, 17, 18, 20
//
// Every test in this block is a SOFTWARE-LEVEL simulation over loopback HTTP.
// None of them validate physical Wi-Fi interruption, physical Android Chrome,
// or physical iOS Safari behaviour; those acceptance dimensions remain NOT
// TESTED pending hardware validation.
// ---------------------------------------------------------------------------

// patternedPayload generates deterministic pseudo-random-looking test bytes.
func patternedPayload(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i * 17) % 251)
	}
	return data
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// tusPartPath returns the on-disk staging part file path for a transfer ID.
func tusPartPath(storeDir, transferID string) string {
	return filepath.Join(storeDir, transfer.StagingDirName, fmt.Sprintf(".flitnix-part-%s.part", transferID))
}

// setupGuestServerWithServices mirrors setupTestGuestServer but also returns
// the *Services so tests can reach the guest chain's ResumableManager (the
// guest handlers create it lazily via EnsureResumable, so a handle on Services
// must be primed before any handler runs to share the same instance).
func setupGuestServerWithServices(t *testing.T) (*httptest.Server, *guest.Manager, *guest.Room, *Services) {
	t.Helper()
	tmpDir := t.TempDir()

	guestMgr := guest.NewManager()
	t.Cleanup(guestMgr.Close)

	room, err := guestMgr.CreateRoom(tmpDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	downloads := transfer.NewDownloadRegistry()

	services := &Services{
		Sessions:     sessMgr,
		Transfers:    transfers,
		Coordinator:  coord,
		Registry:     reg,
		Downloads:    downloads,
		GuestManager: guestMgr,
	}

	grants := newGrantStore(downloads)
	handler := newRouter(services, 5*time.Second, 5*time.Second, grants)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts, guestMgr, room, services
}

// lostPatchResponseTransport implements Scenario 5 at the software level: it
// lets the inner round trip complete (the server has fully processed the PATCH
// and synced the final chunk), then records the successful response, discards
// it, and returns a synthetic transport error to the client — as if the TCP
// connection dropped right after the server sync. Only PATCH requests with the
// gate armed are dropped; HEAD and complete pass through untouched.
type lostPatchResponseTransport struct {
	base http.RoundTripper

	mu          sync.Mutex
	dropPatches bool
	patchCount  int
	dropped     int
	lastStatus  int
	lastOffset  string
	lastState   string
}

func (lt *lostPatchResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	lt.mu.Lock()
	drop := lt.dropPatches && req.Method == http.MethodPatch
	if req.Method == http.MethodPatch {
		lt.patchCount++
	}
	lt.mu.Unlock()

	resp, err := lt.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if !drop {
		return resp, nil
	}

	lt.mu.Lock()
	lt.dropped++
	lt.lastStatus = resp.StatusCode
	lt.lastOffset = resp.Header.Get("Upload-Offset")
	lt.lastState = resp.Header.Get("Upload-State")
	lt.mu.Unlock()

	resp.Body.Close()
	return nil, errors.New("simulated Scenario 5: connection lost after server synced final chunk")
}

func (lt *lostPatchResponseTransport) snapshot() (patchCount, dropped, lastStatus int, lastOffset, lastState string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.patchCount, lt.dropped, lt.lastStatus, lt.lastOffset, lt.lastState
}

func (lt *lostPatchResponseTransport) setDrop(v bool) {
	lt.mu.Lock()
	lt.dropPatches = v
	lt.mu.Unlock()
}

// TestTusScenario_5_LostPatchResponseAfterServerSync: the server processed the
// PATCH to 100% (awaiting_digest) but the client lost the response. The client
// recovers with HEAD, discovers offset == size, and submits the digest — with
// zero PATCH retransmission.
func TestTusScenario_5_LostPatchResponseAfterServerSync(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	payload := patternedPayload(16 * 1024)
	expectedHex := sha256Hex(payload)

	lt := &lostPatchResponseTransport{base: http.DefaultTransport}
	client := &http.Client{Transport: lt}

	bID, itID := createTestBatch(t, env, ts, token, "scenario5.bin", int64(len(payload)))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)

	// POST -> 201
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	postRes, err := client.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postRes.Body.Close()
	if postRes.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", postRes.StatusCode)
	}

	// PATCH with the drop gate armed: the server fully processes the request
	// (the inner round trip succeeds), but the client observes a transport error.
	lt.setDrop(true)

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchRes, err := client.Do(patchReq)
	if err == nil {
		patchRes.Body.Close()
		t.Fatalf("expected synthetic transport error on gated PATCH, got response %d", patchRes.StatusCode)
	}

	patchCount, dropped, dropStatus, dropOffset, dropState := lt.snapshot()
	if dropped != 1 {
		t.Fatalf("expected exactly 1 discarded response, got %d", dropped)
	}
	if patchCount != 1 {
		t.Fatalf("server processed %d PATCH round trips; want exactly 1 (zero retransmission)", patchCount)
	}
	if dropStatus != http.StatusNoContent {
		t.Fatalf("discarded response status = %d, want 204 (server processed the PATCH fully)", dropStatus)
	}
	if dropOffset != strconv.Itoa(len(payload)) {
		t.Fatalf("discarded Upload-Offset = %q, want full size %d", dropOffset, len(payload))
	}
	if dropState != string(transfer.ResumableAwaitingDigest) {
		t.Fatalf("discarded Upload-State = %q, want %q", dropState, transfer.ResumableAwaitingDigest)
	}

	// Disarm the gate; HEAD must pass through and reveal full server progress.
	lt.setDrop(false)

	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headRes, err := client.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	headRes.Body.Close()
	if headRes.StatusCode != http.StatusOK {
		t.Fatalf("HEAD recovery: expected 200, got %d", headRes.StatusCode)
	}
	if got := headRes.Header.Get("Upload-Offset"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("HEAD Upload-Offset = %q, want %d", got, len(payload))
	}
	if got := headRes.Header.Get("Upload-State"); got != string(transfer.ResumableAwaitingDigest) {
		t.Fatalf("HEAD Upload-State = %q, want %q", got, transfer.ResumableAwaitingDigest)
	}
	// After the full-body commit the EOF checkpoint sets meta.PrefixDigest, so
	// the exposed prefix digest equals sha256 of the FULL payload.
	if got := headRes.Header.Get("Upload-Prefix-Digest"); got != expectedHex {
		t.Fatalf("HEAD Upload-Prefix-Digest = %q, want digest of full payload %q", got, expectedHex)
	}

	// The client submits the digest directly; no bytes are retransmitted.
	cBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(cBody))
	compReq.Header.Set("Authorization", "Bearer "+token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := client.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	compRes.Body.Close()
	if compRes.StatusCode != http.StatusOK {
		t.Fatalf("complete after HEAD recovery: expected 200, got %d", compRes.StatusCode)
	}

	if finalPatches, _, _, _, _ := lt.snapshot(); finalPatches != 1 {
		t.Fatalf("PATCH count after full recovery = %d, want 1 (zero retransmission)", finalPatches)
	}
}

// TestTusScenario_7_OffendingOffsetAfterTruncation: a 20 MiB upload dies at
// 10 MiB (truncated body under a 20 MiB Content-Length); the offending client
// then PATCHes claiming offset 20 MiB. The server must reject with 409 Conflict
// and echo its own committed offset 10485760 — no offset advance, no corruption
// (the resumed upload completes with the digest of the full payload).
func TestTusScenario_7_OffendingOffsetAfterTruncation(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	const half = int64(10 * 1024 * 1024) // 10 MiB committed before the drop
	totalSize := 2 * half
	fullData := patternedPayload(int(totalSize))
	expectedHex := sha256Hex(fullData)

	bID, itID := createTestBatch(t, env, ts, token, "scenario7.bin", totalSize)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)

	// POST -> 201
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.FormatInt(totalSize, 10))
	postRes, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postRes.Body.Close()
	if postRes.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", postRes.StatusCode)
	}

	// Commit a valid prefix, then deliberately send a wrong offset.
	truncReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(fullData[:half]))
	truncReq.Header.Set("Authorization", "Bearer "+token)
	truncReq.Header.Set("Tus-Resumable", "1.0.0")
	truncReq.Header.Set("Upload-Offset", "0")
	truncReq.Header.Set("Content-Type", "application/offset+octet-stream")
	truncReq.ContentLength = int64(half)
	truncRes, err := http.DefaultClient.Do(truncReq)
	if err != nil {
		t.Fatal(err)
	}
	truncRes.Body.Close()
	if truncRes.StatusCode != http.StatusNoContent {
		t.Fatalf("prefix PATCH: expected 204, got %d", truncRes.StatusCode)
	}

	// HEAD confirms the interruption: exactly 10 MiB, state interrupted.
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headRes, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	headRes.Body.Close()
	if headRes.StatusCode != http.StatusOK {
		t.Fatalf("HEAD after truncation: expected 200, got %d", headRes.StatusCode)
	}
	if got := headRes.Header.Get("Upload-Offset"); got != strconv.FormatInt(half, 10) {
		t.Fatalf("HEAD Upload-Offset = %q, want %d", got, half)
	}
	if got := headRes.Header.Get("Upload-State"); got != string(transfer.ResumableActive) {
		t.Fatalf("HEAD Upload-State = %q, want %q", got, transfer.ResumableActive)
	}

	// Offending PATCH: claims offset 20 MiB with an EMPTY body. Offset
	// verification happens before any body read, so the server rejects with
	// 409 Conflict and echoes its own committed offset (no broken pipe risk).
	badReq, _ := http.NewRequest(http.MethodPatch, tusURL, nil)
	badReq.Header.Set("Authorization", "Bearer "+token)
	badReq.Header.Set("Tus-Resumable", "1.0.0")
	badReq.Header.Set("Upload-Offset", strconv.FormatInt(totalSize, 10))
	badReq.Header.Set("Content-Type", "application/offset+octet-stream")
	badRes, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badRes.Body.Close()
	if badRes.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for offending offset, got %d", badRes.StatusCode)
	}
	if got := badRes.Header.Get("Upload-Offset"); got != strconv.FormatInt(half, 10) {
		t.Fatalf("409 Upload-Offset = %q, want server's committed %d (no advance)", got, half)
	}

	// HEAD confirms no offset advance.
	headReq2, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq2.Header.Set("Authorization", "Bearer "+token)
	headReq2.Header.Set("Tus-Resumable", "1.0.0")
	headRes2, err := http.DefaultClient.Do(headReq2)
	if err != nil {
		t.Fatal(err)
	}
	headRes2.Body.Close()
	if got := headRes2.Header.Get("Upload-Offset"); got != strconv.FormatInt(half, 10) {
		t.Fatalf("HEAD after offending PATCH = %q, want unchanged %d", got, half)
	}

	// Correct resume from 10 MiB completes the transfer.
	resumeReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(fullData[half:]))
	resumeReq.Header.Set("Authorization", "Bearer "+token)
	resumeReq.Header.Set("Tus-Resumable", "1.0.0")
	resumeReq.Header.Set("Upload-Offset", strconv.FormatInt(half, 10))
	resumeReq.Header.Set("Content-Type", "application/offset+octet-stream")
	resumeRes, err := http.DefaultClient.Do(resumeReq)
	if err != nil {
		t.Fatal(err)
	}
	resumeRes.Body.Close()
	if resumeRes.StatusCode != http.StatusNoContent {
		t.Fatalf("resume PATCH: expected 204, got %d", resumeRes.StatusCode)
	}
	if got := resumeRes.Header.Get("Upload-Offset"); got != strconv.FormatInt(totalSize, 10) {
		t.Fatalf("resume Upload-Offset = %q, want %d", got, totalSize)
	}
	if got := resumeRes.Header.Get("Upload-State"); got != string(transfer.ResumableAwaitingDigest) {
		t.Fatalf("resume Upload-State = %q, want %q", got, transfer.ResumableAwaitingDigest)
	}

	// Complete with the digest of the FULL payload: proves no corruption.
	cBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(cBody))
	compReq.Header.Set("Authorization", "Bearer "+token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	compRes.Body.Close()
	if compRes.StatusCode != http.StatusOK {
		t.Fatalf("complete after resume: expected 200 with full-payload digest, got %d", compRes.StatusCode)
	}
}

// TestTusScenario_9_PartFileLossRestart: the staged .part file is lost while
// .meta survives (software-level loss, e.g. rogue cleanup). PATCH must fail
// with 410 Gone, HEAD must still expose the recorded metadata, and the upload
// must restart fresh from zero (DELETE -> POST -> PATCH 0) on both the Personal
// and Guest Drop Box chains.
func TestTusScenario_9_PartFileLossRestart(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	// ---- Personal chain ----
	payload := patternedPayload(20 * 1024)
	expectedHex := sha256Hex(payload)
	const prefix = 5 * 1024

	bID, itID := createTestBatch(t, env, ts, token, "scenario9.bin", int64(len(payload)))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)

	// POST -> 201
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	postRes, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postRes.Body.Close()
	if postRes.StatusCode != http.StatusCreated {
		t.Fatalf("personal POST: expected 201, got %d", postRes.StatusCode)
	}

	// PATCH first 5 KB -> 204 (clean EOF partial commit, state active)
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[:prefix]))
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()
	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("personal prefix PATCH: expected 204, got %d", patchRes.StatusCode)
	}

	// Remove the .part file, keep .meta.
	transferID := personalTransferID(bID, itID)
	part := tusPartPath(env.store.Dir(), transferID)
	if _, err := os.Stat(part); err != nil {
		t.Fatalf("part file must exist before removal: %v", err)
	}
	if err := os.Remove(part); err != nil {
		t.Fatal(err)
	}

	// PATCH after part loss -> 410 Gone: upload must restart from zero.
	brokenReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[prefix:]))
	brokenReq.Header.Set("Authorization", "Bearer "+token)
	brokenReq.Header.Set("Tus-Resumable", "1.0.0")
	brokenReq.Header.Set("Upload-Offset", strconv.FormatInt(prefix, 10))
	brokenReq.Header.Set("Content-Type", "application/offset+octet-stream")
	brokenRes, err := http.DefaultClient.Do(brokenReq)
	if err != nil {
		t.Fatal(err)
	}
	brokenRes.Body.Close()
	if brokenRes.StatusCode != http.StatusGone {
		t.Fatalf("PATCH after part loss: expected 410 Gone, got %d", brokenRes.StatusCode)
	}

	// HEAD still exposes the recorded metadata (upload is registered, not 404).
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headRes, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	headRes.Body.Close()
	if headRes.StatusCode != http.StatusOK {
		t.Fatalf("HEAD after part loss: expected 200 (metadata retained), got %d", headRes.StatusCode)
	}

	// Startup recovery registers missing-part uploads so a restarted server
	// exposes the same behaviour instead of a bare 404.
	rm2 := transfer.NewResumableManager(env.store, transfer.NewQuotaManager(env.store.Dir(), transfer.DefaultSafetyMargin, transfer.DefaultMaxConcurrentPerSession))
	if err := rm2.RecoverOnStartup(); err != nil {
		t.Fatal(err)
	}
	recovered, ok := rm2.GetUpload(transferID)
	if !ok {
		t.Fatal("RecoverOnStartup did not register the missing-part upload")
	}
	if got := recovered.Meta().CommittedOffset; got != prefix {
		t.Fatalf("recovered CommittedOffset = %d, want %d", got, prefix)
	}
	if got := recovered.Meta().State; got != transfer.ResumableInterrupted {
		t.Fatalf("recovered state = %q, want %q", got, transfer.ResumableInterrupted)
	}

	// DELETE the broken transfer -> 204.
	delReq, _ := http.NewRequest(http.MethodDelete, tusURL, nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delReq.Header.Set("Tus-Resumable", "1.0.0")
	delRes, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delRes.Body.Close()
	if delRes.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", delRes.StatusCode)
	}

	// Fresh from zero requires a new approved coordinator item because DELETE
	// intentionally terminally cancels the original item.
	bID, itID = createTestBatch(t, env, ts, token, "scenario9-fresh.bin", int64(len(payload)))
	tusURL = fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
	completeURL = fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
	postReq2, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq2.Header.Set("Authorization", "Bearer "+token)
	postReq2.Header.Set("Tus-Resumable", "1.0.0")
	postReq2.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	postRes2, err := http.DefaultClient.Do(postReq2)
	if err != nil {
		t.Fatal(err)
	}
	postRes2.Body.Close()
	if postRes2.StatusCode != http.StatusCreated {
		t.Fatalf("fresh POST after DELETE: expected 201, got %d", postRes2.StatusCode)
	}

	patchReq2, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq2.Header.Set("Authorization", "Bearer "+token)
	patchReq2.Header.Set("Tus-Resumable", "1.0.0")
	patchReq2.Header.Set("Upload-Offset", "0")
	patchReq2.Header.Set("Content-Type", "application/offset+octet-stream")
	patchRes2, err := http.DefaultClient.Do(patchReq2)
	if err != nil {
		t.Fatal(err)
	}
	patchRes2.Body.Close()
	if patchRes2.StatusCode != http.StatusNoContent {
		t.Fatalf("fresh PATCH 0: expected 204, got %d", patchRes2.StatusCode)
	}

	cBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(cBody))
	compReq.Header.Set("Authorization", "Bearer "+token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	compRes.Body.Close()
	if compRes.StatusCode != http.StatusOK {
		t.Fatalf("fresh complete: expected 200, got %d", compRes.StatusCode)
	}

	// ---- Guest Drop Box chain ----
	gts, _, groom, gtmp := setupTestGuestServer(t)

	g, err := groom.Join("Cara")
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveGuest(g.ID); err != nil {
		t.Fatal(err)
	}

	gup, err := groom.DeclareUpload(g.ID, "scenario9-guest.bin", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveUpload(gup.ID); err != nil {
		t.Fatal(err)
	}

	gTusURL := fmt.Sprintf("%s/guest-api/uploads/%s/tus", gts.URL, gup.ID)

	gPostReq, _ := http.NewRequest(http.MethodPost, gTusURL, nil)
	gPostReq.Header.Set("Authorization", "Bearer "+g.Token)
	gPostReq.Header.Set("Tus-Resumable", "1.0.0")
	gPostReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	gPostRes, err := http.DefaultClient.Do(gPostReq)
	if err != nil {
		t.Fatal(err)
	}
	gPostRes.Body.Close()
	if gPostRes.StatusCode != http.StatusCreated {
		t.Fatalf("guest POST: expected 201, got %d", gPostRes.StatusCode)
	}

	gPatchReq, _ := http.NewRequest(http.MethodPatch, gTusURL, bytes.NewReader(payload[:prefix]))
	gPatchReq.Header.Set("Authorization", "Bearer "+g.Token)
	gPatchReq.Header.Set("Tus-Resumable", "1.0.0")
	gPatchReq.Header.Set("Upload-Offset", "0")
	gPatchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	gPatchRes, err := http.DefaultClient.Do(gPatchReq)
	if err != nil {
		t.Fatal(err)
	}
	gPatchRes.Body.Close()
	if gPatchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("guest prefix PATCH: expected 204, got %d", gPatchRes.StatusCode)
	}

	gPart := tusPartPath(gtmp, "guest-"+gup.ID)
	if err := os.Remove(gPart); err != nil {
		t.Fatal(err)
	}

	gBrokenReq, _ := http.NewRequest(http.MethodPatch, gTusURL, bytes.NewReader(payload[prefix:]))
	gBrokenReq.Header.Set("Authorization", "Bearer "+g.Token)
	gBrokenReq.Header.Set("Tus-Resumable", "1.0.0")
	gBrokenReq.Header.Set("Upload-Offset", strconv.FormatInt(prefix, 10))
	gBrokenReq.Header.Set("Content-Type", "application/offset+octet-stream")
	gBrokenRes, err := http.DefaultClient.Do(gBrokenReq)
	if err != nil {
		t.Fatal(err)
	}
	gBrokenRes.Body.Close()
	if gBrokenRes.StatusCode != http.StatusGone {
		t.Fatalf("guest PATCH after part loss: expected 410 Gone, got %d", gBrokenRes.StatusCode)
	}

	gDelReq, _ := http.NewRequest(http.MethodDelete, gTusURL, nil)
	gDelReq.Header.Set("Authorization", "Bearer "+g.Token)
	gDelReq.Header.Set("Tus-Resumable", "1.0.0")
	gDelRes, err := http.DefaultClient.Do(gDelReq)
	if err != nil {
		t.Fatal(err)
	}
	gDelRes.Body.Close()
	if gDelRes.StatusCode != http.StatusNoContent {
		t.Fatalf("guest DELETE: expected 204, got %d", gDelRes.StatusCode)
	}

	// Fresh guest restart requires a NEW upload declaration: guest DELETE marks
	// the room upload canceled, so the same upload ID cannot be reused.
	gup2, err := groom.DeclareUpload(g.ID, "scenario9-guest.bin", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveUpload(gup2.ID); err != nil {
		t.Fatal(err)
	}

	gTusURL2 := fmt.Sprintf("%s/guest-api/uploads/%s/tus", gts.URL, gup2.ID)
	gPostReq2, _ := http.NewRequest(http.MethodPost, gTusURL2, nil)
	gPostReq2.Header.Set("Authorization", "Bearer "+g.Token)
	gPostReq2.Header.Set("Tus-Resumable", "1.0.0")
	gPostReq2.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	gPostRes2, err := http.DefaultClient.Do(gPostReq2)
	if err != nil {
		t.Fatal(err)
	}
	gPostRes2.Body.Close()
	if gPostRes2.StatusCode != http.StatusCreated {
		t.Fatalf("guest fresh POST: expected 201, got %d", gPostRes2.StatusCode)
	}

	gPatchReq2, _ := http.NewRequest(http.MethodPatch, gTusURL2, bytes.NewReader(payload))
	gPatchReq2.Header.Set("Authorization", "Bearer "+g.Token)
	gPatchReq2.Header.Set("Tus-Resumable", "1.0.0")
	gPatchReq2.Header.Set("Upload-Offset", "0")
	gPatchReq2.Header.Set("Content-Type", "application/offset+octet-stream")
	gPatchRes2, err := http.DefaultClient.Do(gPatchReq2)
	if err != nil {
		t.Fatal(err)
	}
	gPatchRes2.Body.Close()
	if gPatchRes2.StatusCode != http.StatusNoContent {
		t.Fatalf("guest fresh PATCH 0: expected 204, got %d", gPatchRes2.StatusCode)
	}

	gCompleteURL := fmt.Sprintf("%s/guest-api/uploads/%s/complete", gts.URL, gup2.ID)
	gCBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	gCompReq, _ := http.NewRequest(http.MethodPost, gCompleteURL, bytes.NewReader(gCBody))
	gCompReq.Header.Set("Authorization", "Bearer "+g.Token)
	gCompReq.Header.Set("Content-Type", "application/json")
	gCompRes, err := http.DefaultClient.Do(gCompReq)
	if err != nil {
		t.Fatal(err)
	}
	gCompRes.Body.Close()
	if gCompRes.StatusCode != http.StatusOK {
		t.Fatalf("guest fresh complete: expected 200, got %d", gCompRes.StatusCode)
	}
}

// TestTusScenario_16_MidUploadENOSPC simulates the storage filling up DURING
// an ACTIVE upload (not at declare time): after the first half is committed,
// the staged part file is swapped for a symlink to /dev/full (writes return
// ENOSPC; stat/seek succeed). The server must return 507 with state
// paused_storage_full while preserving the committed offset, and the upload
// must recover safely once space is available again. Linux-only simulation.
func TestTusScenario_16_MidUploadENOSPC(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Scenario 16 /dev/full simulation is Linux-only")
	}
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full not available on this host")
	}

	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	payload := patternedPayload(16 * 1024)
	expectedHex := sha256Hex(payload)
	const half = 8 * 1024

	bID, itID := createTestBatch(t, env, ts, token, "scenario16.bin", int64(len(payload)))
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)

	// POST -> 201
	postReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	postReq.Header.Set("Authorization", "Bearer "+token)
	postReq.Header.Set("Tus-Resumable", "1.0.0")
	postReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	postRes, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatal(err)
	}
	postRes.Body.Close()
	if postRes.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", postRes.StatusCode)
	}

	// PATCH first half -> 204.
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[:half]))
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()
	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("first half PATCH: expected 204, got %d", patchRes.StatusCode)
	}

	// Swap the staged part file for a symlink to /dev/full between PATCHes.
	transferID := personalTransferID(bID, itID)
	part := tusPartPath(env.store.Dir(), transferID)
	orig, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(part); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/full", part); err != nil {
		t.Fatal(err)
	}

	// PATCH second half -> 507 with preserved offset and paused_storage_full.
	enospcReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[half:]))
	enospcReq.Header.Set("Authorization", "Bearer "+token)
	enospcReq.Header.Set("Tus-Resumable", "1.0.0")
	enospcReq.Header.Set("Upload-Offset", strconv.FormatInt(half, 10))
	enospcReq.Header.Set("Content-Type", "application/offset+octet-stream")
	enospcRes, err := http.DefaultClient.Do(enospcReq)
	if err != nil {
		t.Fatal(err)
	}
	enospcRes.Body.Close()
	if enospcRes.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("mid-upload ENOSPC: expected 507, got %d", enospcRes.StatusCode)
	}
	if got := enospcRes.Header.Get("Upload-Offset"); got != strconv.FormatInt(half, 10) {
		t.Fatalf("507 Upload-Offset = %q, want preserved %d", got, half)
	}
	if got := enospcRes.Header.Get("Upload-State"); got != string(transfer.ResumablePausedStorageFull) {
		t.Fatalf("507 Upload-State = %q, want %q", got, transfer.ResumablePausedStorageFull)
	}

	// Restore the original part bytes (remove the symlink first).
	if err := os.Remove(part); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, orig, 0o600); err != nil {
		t.Fatal(err)
	}

	// Recovery arc: HEAD shows the preserved offset, the resume PATCH succeeds,
	// and the upload completes with the full-payload digest.
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headRes, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	headRes.Body.Close()
	if headRes.StatusCode != http.StatusOK {
		t.Fatalf("HEAD after recovery: expected 200, got %d", headRes.StatusCode)
	}
	if got := headRes.Header.Get("Upload-Offset"); got != strconv.FormatInt(half, 10) {
		t.Fatalf("HEAD Upload-Offset after ENOSPC = %q, want preserved %d", got, half)
	}

	resumeReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload[half:]))
	resumeReq.Header.Set("Authorization", "Bearer "+token)
	resumeReq.Header.Set("Tus-Resumable", "1.0.0")
	resumeReq.Header.Set("Upload-Offset", strconv.FormatInt(half, 10))
	resumeReq.Header.Set("Content-Type", "application/offset+octet-stream")
	resumeRes, err := http.DefaultClient.Do(resumeReq)
	if err != nil {
		t.Fatal(err)
	}
	resumeRes.Body.Close()
	if resumeRes.StatusCode != http.StatusNoContent {
		t.Fatalf("resume PATCH after storage restored: expected 204, got %d", resumeRes.StatusCode)
	}
	if got := resumeRes.Header.Get("Upload-State"); got != string(transfer.ResumableAwaitingDigest) {
		t.Fatalf("resume Upload-State = %q, want %q", got, transfer.ResumableAwaitingDigest)
	}

	cBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(cBody))
	compReq.Header.Set("Authorization", "Bearer "+token)
	compReq.Header.Set("Content-Type", "application/json")
	compRes, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatal(err)
	}
	compRes.Body.Close()
	if compRes.StatusCode != http.StatusOK {
		t.Fatalf("complete after ENOSPC recovery: expected 200, got %d", compRes.StatusCode)
	}
}

// TestTusScenario_17_ConcurrencyCapDefaultQuota exercises the concurrent
// upload quota independently of storage-full rejection: with the DEFAULT cap
// of 10 active resumables per session, the 10th POST succeeds, the 11th is
// rejected with 429, and cancelling one upload releases its reservation so the
// next POST succeeds. (The conflated 507-vs-429 test is
// TestTusScenario_16_17_StorageQuotaRejection; this test isolates the cap.)
func TestTusScenario_17_ConcurrencyCapDefaultQuota(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	maxConcurrent := transfer.DefaultMaxConcurrentPerSession
	if maxConcurrent != 10 {
		t.Fatalf("unexpected DefaultMaxConcurrentPerSession: %d", maxConcurrent)
	}

	// Declare maxConcurrent+1 uploads up front.
	urls := make([]string, maxConcurrent+1)
	for i := range urls {
		bID, itID := createTestBatch(t, env, ts, token, fmt.Sprintf("scenario17-%d.bin", i), 1024)
		urls[i] = fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
	}

	doPost := func(tusURL string) int {
		req, _ := http.NewRequest(http.MethodPost, tusURL, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Length", "1024")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	// The first maxConcurrent POSTs succeed with 201.
	for i := 0; i < maxConcurrent; i++ {
		if code := doPost(urls[i]); code != http.StatusCreated {
			t.Fatalf("POST %d/%d: expected 201, got %d", i+1, maxConcurrent, code)
		}
	}

	// The (maxConcurrent+1)-th POST exceeds the per-session cap -> 429.
	if code := doPost(urls[maxConcurrent]); code != http.StatusTooManyRequests {
		t.Fatalf("POST beyond cap: expected 429 Too Many Requests, got %d", code)
	}

	// Cancel one active upload: CancelUpload releases the quota reservation.
	delReq, _ := http.NewRequest(http.MethodDelete, urls[0], nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delReq.Header.Set("Tus-Resumable", "1.0.0")
	delRes, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delRes.Body.Close()
	if delRes.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", delRes.StatusCode)
	}

	// The next POST now fits under the cap again.
	if code := doPost(urls[maxConcurrent]); code != http.StatusCreated {
		t.Fatalf("POST after cancel: expected 201 (quota released), got %d", code)
	}
}

// TestTusScenario_18_ExpiryDeterministic exercises resumable-upload expiry
// without waiting the real 24h TTL: ExpiresAt is forced into the past
// deterministically. Expired uploads must fail PATCH with 410 Gone (or an
// explicitly supported equivalent), CleanupExpired must purge them safely,
// unexpired uploads must be untouched, zero ExpiresAt must disable PATCH-time
// enforcement, and the guest leg must not inherit any Personal session state.
func TestTusScenario_18_ExpiryDeterministic(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	resumable := env.srv.Services().EnsureResumable()

	const itemSize = 20 * 1024
	payload := patternedPayload(itemSize)
	const prefix = 4 * 1024

	// declare creates a batch item and POSTs the tus upload.
	declare := func(name string) (tusURL, completeURL, transferID string) {
		bID, itID := createTestBatch(t, env, ts, token, name, itemSize)
		tus := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
		comp := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", ts.URL, bID, itID)
		req, _ := http.NewRequest(http.MethodPost, tus, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Length", strconv.FormatInt(itemSize, 10))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("POST %s: expected 201, got %d", name, res.StatusCode)
		}
		return tus, comp, personalTransferID(bID, itID)
	}

	patchRange := func(tusURL string, body []byte, offset int64) int {
		req, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	complete := func(completeURL string) int {
		cBody, _ := json.Marshal(map[string]string{"expectedSHA256": sha256Hex(payload)})
		req, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(cBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	// --- Upload A: expires before the next PATCH (deterministic; no 24h wait) ---
	tusA, _, idA := declare("scenario18-expired.bin")
	if code := patchRange(tusA, payload[:prefix], 0); code != http.StatusNoContent {
		t.Fatalf("upload A prefix PATCH: expected 204, got %d", code)
	}
	upA, ok := resumable.GetUpload(idA)
	if !ok {
		t.Fatal("upload A not registered")
	}
	upA.UpdateMeta(func(m *transfer.ResumableMeta) {
		m.ExpiresAt = time.Now().Add(-time.Minute)
	})
	if code := patchRange(tusA, payload[prefix:], prefix); code != http.StatusGone {
		t.Fatalf("upload A expired PATCH: expected 410 Gone, got %d", code)
	}
	delA, _ := http.NewRequest(http.MethodDelete, tusA, nil)
	delA.Header.Set("Authorization", "Bearer "+token)
	delA.Header.Set("Tus-Resumable", "1.0.0")
	delARes, err := http.DefaultClient.Do(delA)
	if err != nil {
		t.Fatal(err)
	}
	delARes.Body.Close()
	if delARes.StatusCode != http.StatusNoContent {
		t.Fatalf("expired upload DELETE: expected 204, got %d", delARes.StatusCode)
	}

	// --- Upload B: NOT expired; must survive CleanupExpired untouched ---
	tusB, compB, idB := declare("scenario18-live.bin")
	if code := patchRange(tusB, payload[:prefix], 0); code != http.StatusNoContent {
		t.Fatalf("upload B prefix PATCH: expected 204, got %d", code)
	}

	// --- Upload C: zero ExpiresAt disables PATCH-time expiry enforcement ---
	tusC, compC, idC := declare("scenario18-zero.bin")
	upC, ok := resumable.GetUpload(idC)
	if !ok {
		t.Fatal("upload C not registered")
	}
	upC.UpdateMeta(func(m *transfer.ResumableMeta) {
		m.ExpiresAt = time.Time{}
	})

	// Cleanup purges only the expired upload.
	resumable.CleanupExpired()
	if _, ok := resumable.GetUpload(idA); ok {
		t.Fatal("CleanupExpired did not purge the expired upload")
	}
	if _, ok := resumable.GetUpload(idB); !ok {
		t.Fatal("CleanupExpired purged an unexpired upload")
	}

	// HEAD on the purged upload reports 404 (it is gone, not resumable).
	headReq, _ := http.NewRequest(http.MethodHead, tusA, nil)
	headReq.Header.Set("Authorization", "Bearer "+token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headRes, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	headRes.Body.Close()
	if headRes.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD on purged expired upload: expected 404, got %d", headRes.StatusCode)
	}

	// Unexpired upload B resumes and completes normally.
	if code := patchRange(tusB, payload[prefix:], prefix); code != http.StatusNoContent {
		t.Fatalf("upload B resume PATCH: expected 204, got %d", code)
	}
	if code := complete(compB); code != http.StatusOK {
		t.Fatalf("upload B complete: expected 200, got %d", code)
	}

	// Zero ExpiresAt does not block PATCHes or completion.
	if code := patchRange(tusC, payload, 0); code != http.StatusNoContent {
		t.Fatalf("upload C (zero ExpiresAt) PATCH: expected 204, got %d", code)
	}
	if code := complete(compC); code != http.StatusOK {
		t.Fatalf("upload C (zero ExpiresAt) complete: expected 200, got %d", code)
	}

	// --- Guest leg: expiry must be enforced on the guest chain too, with no
	// inheritance of Personal session state (fresh guest server + fresh room). ---
	gts, _, groom, gsrvcs := setupGuestServerWithServices(t)
	gGuest, err := groom.Join("Dina")
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveGuest(gGuest.ID); err != nil {
		t.Fatal(err)
	}

	gup, err := groom.DeclareUpload(gGuest.ID, "scenario18-guest.bin", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveUpload(gup.ID); err != nil {
		t.Fatal(err)
	}

	gTusURL := fmt.Sprintf("%s/guest-api/uploads/%s/tus", gts.URL, gup.ID)
	gPostReq, _ := http.NewRequest(http.MethodPost, gTusURL, nil)
	gPostReq.Header.Set("Authorization", "Bearer "+gGuest.Token)
	gPostReq.Header.Set("Tus-Resumable", "1.0.0")
	gPostReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	gPostRes, err := http.DefaultClient.Do(gPostReq)
	if err != nil {
		t.Fatal(err)
	}
	gPostRes.Body.Close()
	if gPostRes.StatusCode != http.StatusCreated {
		t.Fatalf("guest POST: expected 201, got %d", gPostRes.StatusCode)
	}

	gPatchReq, _ := http.NewRequest(http.MethodPatch, gTusURL, bytes.NewReader(payload[:prefix]))
	gPatchReq.Header.Set("Authorization", "Bearer "+gGuest.Token)
	gPatchReq.Header.Set("Tus-Resumable", "1.0.0")
	gPatchReq.Header.Set("Upload-Offset", "0")
	gPatchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	gPatchRes, err := http.DefaultClient.Do(gPatchReq)
	if err != nil {
		t.Fatal(err)
	}
	gPatchRes.Body.Close()
	if gPatchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("guest prefix PATCH: expected 204, got %d", gPatchRes.StatusCode)
	}

	gResumable := gsrvcs.EnsureResumable()
	gUp, ok := gResumable.GetUpload("guest-" + gup.ID)
	if !ok {
		t.Fatal("guest upload not registered")
	}
	gUp.UpdateMeta(func(m *transfer.ResumableMeta) {
		m.ExpiresAt = time.Now().Add(-time.Minute)
	})

	gExpiredReq, _ := http.NewRequest(http.MethodPatch, gTusURL, bytes.NewReader(payload[prefix:]))
	gExpiredReq.Header.Set("Authorization", "Bearer "+gGuest.Token)
	gExpiredReq.Header.Set("Tus-Resumable", "1.0.0")
	gExpiredReq.Header.Set("Upload-Offset", strconv.FormatInt(prefix, 10))
	gExpiredReq.Header.Set("Content-Type", "application/offset+octet-stream")
	gExpiredRes, err := http.DefaultClient.Do(gExpiredReq)
	if err != nil {
		t.Fatal(err)
	}
	gExpiredRes.Body.Close()
	if gExpiredRes.StatusCode != http.StatusGone {
		t.Fatalf("guest expired PATCH: expected 410 Gone, got %d", gExpiredRes.StatusCode)
	}
}

// TestTusScenario_20_GuestInterruptionResumeAndIsolation: on the real Guest
// Drop Box routes, a guest upload is interrupted mid-stream, recovered via
// HEAD, resumed, and completed; the published file lands inside the guest's
// own directory with byte-exact content while another guest's concurrent
// upload is unaffected. This is a software-level simulation — no physical
// Wi-Fi or device browser is involved.
func TestTusScenario_20_GuestInterruptionResumeAndIsolation(t *testing.T) {
	gts, _, groom, _ := setupTestGuestServer(t)

	alice, err := groom.Join("Alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveGuest(alice.ID); err != nil {
		t.Fatal(err)
	}
	bob, err := groom.Join("Bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveGuest(bob.ID); err != nil {
		t.Fatal(err)
	}

	payload := patternedPayload(64 * 1024)
	expectedHex := sha256Hex(payload)
	const half = 32 * 1024

	// Alice declares and starts uploading.
	aUp, err := groom.DeclareUpload(alice.ID, "scenario20-alice.bin", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveUpload(aUp.ID); err != nil {
		t.Fatal(err)
	}
	aTusURL := fmt.Sprintf("%s/guest-api/uploads/%s/tus", gts.URL, aUp.ID)

	aPostReq, _ := http.NewRequest(http.MethodPost, aTusURL, nil)
	aPostReq.Header.Set("Authorization", "Bearer "+alice.Token)
	aPostReq.Header.Set("Tus-Resumable", "1.0.0")
	aPostReq.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	aPostRes, err := http.DefaultClient.Do(aPostReq)
	if err != nil {
		t.Fatal(err)
	}
	aPostRes.Body.Close()
	if aPostRes.StatusCode != http.StatusCreated {
		t.Fatalf("Alice POST: expected 201, got %d", aPostRes.StatusCode)
	}

	// Alice streams the first half, then the connection "drops" (client stops).
	aPatchReq, _ := http.NewRequest(http.MethodPatch, aTusURL, bytes.NewReader(payload[:half]))
	aPatchReq.Header.Set("Authorization", "Bearer "+alice.Token)
	aPatchReq.Header.Set("Tus-Resumable", "1.0.0")
	aPatchReq.Header.Set("Upload-Offset", "0")
	aPatchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	aPatchRes, err := http.DefaultClient.Do(aPatchReq)
	if err != nil {
		t.Fatal(err)
	}
	aPatchRes.Body.Close()
	if aPatchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("Alice prefix PATCH: expected 204, got %d", aPatchRes.StatusCode)
	}

	// HEAD recovery discovers the committed offset.
	aHeadReq, _ := http.NewRequest(http.MethodHead, aTusURL, nil)
	aHeadReq.Header.Set("Authorization", "Bearer "+alice.Token)
	aHeadReq.Header.Set("Tus-Resumable", "1.0.0")
	aHeadRes, err := http.DefaultClient.Do(aHeadReq)
	if err != nil {
		t.Fatal(err)
	}
	aHeadRes.Body.Close()
	if aHeadRes.StatusCode != http.StatusOK {
		t.Fatalf("Alice HEAD: expected 200, got %d", aHeadRes.StatusCode)
	}
	if got := aHeadRes.Header.Get("Upload-Offset"); got != strconv.FormatInt(half, 10) {
		t.Fatalf("Alice HEAD Upload-Offset = %q, want %d", got, half)
	}

	// Bob uploads his own file concurrently and completes — unaffected.
	bobPayload := patternedPayload(8 * 1024)
	bobHex := sha256Hex(bobPayload)
	bUp, err := groom.DeclareUpload(bob.ID, "scenario20-bob.bin", int64(len(bobPayload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := groom.ApproveUpload(bUp.ID); err != nil {
		t.Fatal(err)
	}
	bTusURL := fmt.Sprintf("%s/guest-api/uploads/%s/tus", gts.URL, bUp.ID)

	bPostReq, _ := http.NewRequest(http.MethodPost, bTusURL, nil)
	bPostReq.Header.Set("Authorization", "Bearer "+bob.Token)
	bPostReq.Header.Set("Tus-Resumable", "1.0.0")
	bPostReq.Header.Set("Upload-Length", strconv.Itoa(len(bobPayload)))
	bPostRes, err := http.DefaultClient.Do(bPostReq)
	if err != nil {
		t.Fatal(err)
	}
	bPostRes.Body.Close()
	if bPostRes.StatusCode != http.StatusCreated {
		t.Fatalf("Bob POST: expected 201, got %d", bPostRes.StatusCode)
	}

	bPatchReq, _ := http.NewRequest(http.MethodPatch, bTusURL, bytes.NewReader(bobPayload))
	bPatchReq.Header.Set("Authorization", "Bearer "+bob.Token)
	bPatchReq.Header.Set("Tus-Resumable", "1.0.0")
	bPatchReq.Header.Set("Upload-Offset", "0")
	bPatchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	bPatchRes, err := http.DefaultClient.Do(bPatchReq)
	if err != nil {
		t.Fatal(err)
	}
	bPatchRes.Body.Close()
	if bPatchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("Bob PATCH: expected 204, got %d", bPatchRes.StatusCode)
	}

	bCompleteURL := fmt.Sprintf("%s/guest-api/uploads/%s/complete", gts.URL, bUp.ID)
	bCBody, _ := json.Marshal(map[string]string{"expectedSHA256": bobHex})
	bCompReq, _ := http.NewRequest(http.MethodPost, bCompleteURL, bytes.NewReader(bCBody))
	bCompReq.Header.Set("Authorization", "Bearer "+bob.Token)
	bCompReq.Header.Set("Content-Type", "application/json")
	bCompRes, err := http.DefaultClient.Do(bCompReq)
	if err != nil {
		t.Fatal(err)
	}
	bCompRes.Body.Close()
	if bCompRes.StatusCode != http.StatusOK {
		t.Fatalf("Bob complete: expected 200, got %d", bCompRes.StatusCode)
	}

	// Alice resumes from the discovered offset and completes.
	aResumeReq, _ := http.NewRequest(http.MethodPatch, aTusURL, bytes.NewReader(payload[half:]))
	aResumeReq.Header.Set("Authorization", "Bearer "+alice.Token)
	aResumeReq.Header.Set("Tus-Resumable", "1.0.0")
	aResumeReq.Header.Set("Upload-Offset", strconv.FormatInt(half, 10))
	aResumeReq.Header.Set("Content-Type", "application/offset+octet-stream")
	aResumeRes, err := http.DefaultClient.Do(aResumeReq)
	if err != nil {
		t.Fatal(err)
	}
	aResumeRes.Body.Close()
	if aResumeRes.StatusCode != http.StatusNoContent {
		t.Fatalf("Alice resume PATCH: expected 204, got %d", aResumeRes.StatusCode)
	}

	aCompleteURL := fmt.Sprintf("%s/guest-api/uploads/%s/complete", gts.URL, aUp.ID)
	aCBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	aCompReq, _ := http.NewRequest(http.MethodPost, aCompleteURL, bytes.NewReader(aCBody))
	aCompReq.Header.Set("Authorization", "Bearer "+alice.Token)
	aCompReq.Header.Set("Content-Type", "application/json")
	aCompRes, err := http.DefaultClient.Do(aCompReq)
	if err != nil {
		t.Fatal(err)
	}
	defer aCompRes.Body.Close()
	if aCompRes.StatusCode != http.StatusOK {
		t.Fatalf("Alice complete: expected 200, got %d", aCompRes.StatusCode)
	}

	var compReply struct {
		Status string `json:"status"`
		SHA256 string `json:"sha256"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
	}
	if err := json.NewDecoder(aCompRes.Body).Decode(&compReply); err != nil {
		t.Fatal(err)
	}
	if compReply.Status != "completed" {
		t.Fatalf("complete status = %q, want %q", compReply.Status, "completed")
	}
	if compReply.SHA256 != expectedHex {
		t.Fatalf("complete sha256 = %q, want %q", compReply.SHA256, expectedHex)
	}
	if compReply.Size != int64(len(payload)) {
		t.Fatalf("complete size = %d, want %d", compReply.Size, len(payload))
	}

	// Published content is byte-exact and lives inside Alice's guest directory
	// (not in any personal/desktop transfer destination).
	published, err := os.ReadFile(compReply.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, payload) {
		t.Fatal("published file content differs from the uploaded payload")
	}
	wantDir := groom.GuestDirectory(alice.ID, alice.SafeSlug)
	if filepath.Dir(compReply.Path) != wantDir {
		t.Fatalf("published path %q is outside Alice's guest directory %q", compReply.Path, wantDir)
	}

	// The room record shows completed (not failed/canceled) with matching digest.
	upAfter, err := groom.GetUpload(aUp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if upAfter.State != guest.UploadCompleted {
		t.Fatalf("room state = %q, want %q", upAfter.State, guest.UploadCompleted)
	}
	if upAfter.SHA256 != expectedHex {
		t.Fatalf("room sha256 = %q, want %q", upAfter.SHA256, expectedHex)
	}
}
