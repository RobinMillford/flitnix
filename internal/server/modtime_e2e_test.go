package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func newRecoverableTestServer(t *testing.T, receiveDir, stateDir string) (*Server, *session.Manager, *transfer.Service, *transfer.Coordinator) {
	t.Helper()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: receiveDir})
	if err != nil {
		t.Fatal(err)
	}
	quota := transfer.NewQuotaManager(transfer.QuotaConfig{DefaultDir: receiveDir, SafetyMargin: 1})
	sess := session.NewManager(session.Config{IdleTimeout: 10 * time.Minute})
	trans := transfer.NewService(transfer.Config{Store: store, Quota: quota})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	svcs := &Services{
		Sessions:    sess,
		Transfers:   trans,
		Coordinator: coord,
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
	}
	srv := New(Config{ListenAddr: "127.0.0.1:0", RecoveryDir: stateDir, Services: svcs})
	if err := srv.Recover(); err != nil {
		t.Fatal(err)
	}
	return srv, sess, trans, coord
}

func TestModTimePreservationHTTPBatchAndRestart(t *testing.T) {
	receiveDir := t.TempDir()
	stateDir := t.TempDir()
	origTime := time.Date(2021, 11, 5, 19, 45, 30, 0, time.UTC)
	origMs := origTime.UnixMilli()

	// Phase 1: declare batch with lastModifiedMs and upload first half before restart
	srv1, sess1, trans1, coord1 := newRecoverableTestServer(t, receiveDir, stateDir)
	s, err := sess1.Create()
	if err != nil {
		t.Fatal(err)
	}
	tok := s.Token()
	sessKey := s.Key()

	data := bytes.Repeat([]byte("D"), 128*1024)
	declBody, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFolder,
		RootName:  "Memories",
		Items: []transfer.ManifestItem{{
			RelativePath:   "2021/nov.jpg",
			Size:           int64(len(data)),
			LastModifiedMs: origMs,
		}},
	})
	declReq := httptest.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(declBody))
	declReq.Header.Set("Authorization", "Bearer "+tok)
	declRec := httptest.NewRecorder()
	srv1.Handler().ServeHTTP(declRec, declReq)
	if declRec.Code != http.StatusCreated {
		t.Fatalf("declare batch = %d: %s", declRec.Code, declRec.Body.String())
	}
	var batch transfer.BatchView
	_ = json.Unmarshal(declRec.Body.Bytes(), &batch)
	if batch.Items[0].LastModifiedMs != origMs {
		t.Fatalf("BatchItemView.LastModifiedMs = %d, want %d", batch.Items[0].LastModifiedMs, origMs)
	}
	item := batch.Items[0]
	cid, err := coord1.ItemContentID(sessKey, batch.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = trans1.DecideBatchItems([]string{cid}, true)
	if _, err := coord1.ApproveBatch(sessKey, batch.ID); err != nil {
		t.Fatal(err)
	}

	split := 64 * 1024
	patchURL := fmt.Sprintf("/api/batches/%s/items/%s/content", batch.ID, item.ID)
	req1 := httptest.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(data[:split]))
	req1.Header.Set("Authorization", "Bearer "+tok)
	req1.Header.Set("Content-Type", "application/offset+octet-stream")
	req1.Header.Set("Upload-Offset", "0")
	rec1 := httptest.NewRecorder()
	srv1.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusNoContent {
		t.Fatalf("chunk 1 PATCH = %d: %s", rec1.Code, rec1.Body.String())
	}
	srv1.CloseRecovery()

	// Phase 2: restart server cleanly using the same receiveDir and stateDir, finish second half
	srv2, _, _, _ := newRecoverableTestServer(t, receiveDir, stateDir)
	defer srv2.CloseRecovery()

	req2 := httptest.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(data[split:]))
	req2.Header.Set("Authorization", "Bearer "+tok)
	req2.Header.Set("Content-Type", "application/offset+octet-stream")
	req2.Header.Set("Upload-Offset", strconv.Itoa(split))
	rec2 := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("chunk 2 PATCH = %d: %s", rec2.Code, rec2.Body.String())
	}

	finalPath := filepath.Join(receiveDir, "Memories", "2021", "nov.jpg")
	info, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("stat published file: %v", err)
	}
	if got := info.ModTime().UTC().Truncate(time.Second); !got.Equal(origTime) {
		t.Fatalf("published mtime after crash recovery = %v, want %v", got, origTime)
	}
}

func TestModTimePreservationDirectUploadValidationAndPut(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	// Reject negative timestamps on /api/uploads
	if rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"bad.txt","size":4,"lastModifiedMs":-10}`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative lastModifiedMs = %d, want 400", rec.Code)
	}

	origTime := time.Date(2019, 3, 14, 9, 26, 0, 0, time.UTC)
	origMs := origTime.UnixMilli()
	payload := []byte("direct-put-dated")
	declRec := env.do(http.MethodPost, "/api/uploads", tok, []byte(fmt.Sprintf(`{"name":"dated.txt","size":%d,"lastModifiedMs":%d}`, len(payload), origMs)))
	if declRec.Code != http.StatusCreated {
		t.Fatalf("declare direct upload = %d: %s", declRec.Code, declRec.Body.String())
	}
	var upRes struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(declRec.Body.Bytes(), &upRes)
	if _, err := env.trans.Decide(upRes.ID, true); err != nil {
		t.Fatal(err)
	}
	putRec := env.do(http.MethodPut, "/api/uploads/"+upRes.ID+"/content", tok, payload)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT content = %d: %s", putRec.Code, putRec.Body.String())
	}
	info, err := os.Stat(filepath.Join(env.store.Dir(), "dated.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ModTime().UTC().Truncate(time.Second); !got.Equal(origTime) {
		t.Fatalf("direct PUT mtime = %v, want %v", got, origTime)
	}
}
