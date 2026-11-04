package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/itsyamin/flitnix/internal/power"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

type recordingInhibitor struct {
	mu           sync.Mutex
	acquires     []string
	releases     int
	activeLeases int
}

func (r *recordingInhibitor) Close() error {
	return nil
}

func (r *recordingInhibitor) Acquire(reason string) (power.Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acquires = append(r.acquires, reason)
	r.activeLeases++
	var once sync.Once
	return &testLease{
		release: func() {
			once.Do(func() {
				r.mu.Lock()
				defer r.mu.Unlock()
				r.releases++
				r.activeLeases--
			})
		},
	}, nil
}

func (r *recordingInhibitor) Status() power.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return power.Status{
		Supported: true,
		Active:    r.activeLeases > 0,
		Holders:   r.activeLeases,
	}
}

type testLease struct {
	release func()
}

func (l *testLease) Release() {
	if l.release != nil {
		l.release()
	}
}

func TestPowerInhibition_UploadAndDownloadLifecycle(t *testing.T) {
	storeDir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: storeDir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	clock := newFakeClock()
	sessMgr := session.NewManager(session.Config{Now: clock.Now})
	transSvc := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()
	recInhibitor := &recordingInhibitor{}

	srv := New(Config{Services: &Services{
		Sessions:    sessMgr,
		Transfers:   transSvc,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
		Inhibitor:   recInhibitor,
	}})

	// 1. Create session
	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rawToken := sess.Token()

	// 2. Perform Batch Upload
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "hello.txt", Size: 11},
		},
	}
	declBytes, _ := json.Marshal(manifest)

	req := httptest.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(declBytes))
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("create batch code = %d: %s", w.Code, w.Body.String())
	}
	var batchResp transfer.BatchView
	_ = json.Unmarshal(w.Body.Bytes(), &batchResp)

	// Inhibit should NOT be acquired for batch creation (pending)
	recInhibitor.mu.Lock()
	if len(recInhibitor.acquires) != 0 {
		t.Errorf("inhibitor acquired on batch create; want 0, got %d", len(recInhibitor.acquires))
	}
	recInhibitor.mu.Unlock()

	// Approve batch in coordinator and approve item in transfers
	itemID := batchResp.Items[0].ID
	contentID, err := coord.ItemContentID(sess.Key(), batchResp.ID, itemID)
	if err != nil {
		t.Fatalf("ItemContentID: %v", err)
	}
	if _, err := transSvc.Decide(contentID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	_, err = coord.ApproveBatch(sess.Key(), batchResp.ID)
	if err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}

	// Upload item content
	contentReq := httptest.NewRequest(http.MethodPut, "/api/batches/"+batchResp.ID+"/items/"+itemID+"/content", strings.NewReader("hello world"))
	contentReq.Header.Set("Authorization", "Bearer "+rawToken)
	contentW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(contentW, contentReq)

	if contentW.Code != http.StatusOK {
		t.Fatalf("upload content code = %d: %s", contentW.Code, contentW.Body.String())
	}

	// Inhibit MUST have been acquired and released for the upload stream
	recInhibitor.mu.Lock()
	if len(recInhibitor.acquires) != 1 {
		t.Errorf("expected 1 acquire for upload, got %d", len(recInhibitor.acquires))
	}
	if recInhibitor.releases != 1 {
		t.Errorf("expected 1 release after upload completion, got %d", recInhibitor.releases)
	}
	if recInhibitor.activeLeases != 0 {
		t.Errorf("expected 0 active leases, got %d", recInhibitor.activeLeases)
	}
	recInhibitor.mu.Unlock()

	// 3. Perform Direct File Download
	testFile := filepath.Join(t.TempDir(), "download.txt")
	if err := os.WriteFile(testFile, []byte("downloadable content 12345"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	sf, err := reg.Add(testFile, "download.txt", sess.Key())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	dlReq := httptest.NewRequest(http.MethodGet, "/api/files/"+sf.ID+"/download", nil)
	dlReq.Header.Set("Authorization", "Bearer "+rawToken)
	dlW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(dlW, dlReq)

	if dlW.Code != http.StatusOK {
		t.Fatalf("download file code = %d: %s", dlW.Code, dlW.Body.String())
	}
	body, _ := io.ReadAll(dlW.Body)
	if string(body) != "downloadable content 12345" {
		t.Errorf("download content = %q; want 'downloadable content 12345'", string(body))
	}

	// Inhibit MUST have been acquired and released for the download stream
	recInhibitor.mu.Lock()
	if len(recInhibitor.acquires) != 2 {
		t.Errorf("expected 2 total acquires (upload + download), got %d", len(recInhibitor.acquires))
	}
	if recInhibitor.releases != 2 {
		t.Errorf("expected 2 total releases, got %d", recInhibitor.releases)
	}
	if recInhibitor.activeLeases != 0 {
		t.Errorf("expected 0 active leases, got %d", recInhibitor.activeLeases)
	}
	recInhibitor.mu.Unlock()
}

func TestPowerInhibition_ArchiveGrantDownload(t *testing.T) {
	storeDir := t.TempDir()
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: storeDir})
	clock := newFakeClock()
	sessMgr := session.NewManager(session.Config{Now: clock.Now})
	transSvc := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()
	recInhibitor := &recordingInhibitor{}

	srv := New(Config{Services: &Services{
		Sessions:    sessMgr,
		Transfers:   transSvc,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
		Inhibitor:   recInhibitor,
	}})

	sess, _ := sessMgr.Create()
	rawToken := sess.Token()

	// Create test folder
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "file1.txt"), []byte("data 1"), 0644)
	source, err := reg.AddFolder(root, sess.Key())
	if err != nil {
		t.Fatalf("AddFolder: %v", err)
	}

	// Request download grant
	grantBody, _ := json.Marshal(map[string]any{"sourceIds": []string{source.ID}})
	grantReq := httptest.NewRequest(http.MethodPost, "/api/download-grants", bytes.NewReader(grantBody))
	grantReq.Header.Set("Authorization", "Bearer "+rawToken)
	grantReq.Header.Set("Content-Type", "application/json")
	grantW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(grantW, grantReq)

	if grantW.Code != http.StatusCreated {
		t.Fatalf("grant code = %d: %s", grantW.Code, grantW.Body.String())
	}

	// Grant creation must not acquire sleep inhibitor
	recInhibitor.mu.Lock()
	if len(recInhibitor.acquires) != 0 {
		t.Errorf("expected 0 acquires on grant creation, got %d", len(recInhibitor.acquires))
	}
	recInhibitor.mu.Unlock()

	var grantResp struct {
		DownloadURL string `json:"downloadUrl"`
		Filename    string `json:"filename"`
	}
	_ = json.Unmarshal(grantW.Body.Bytes(), &grantResp)

	cookie := grantW.Result().Cookies()[0]
	streamReq := httptest.NewRequest(http.MethodGet, grantResp.DownloadURL, nil)
	streamReq.AddCookie(cookie)
	streamW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(streamW, streamReq)

	if streamW.Code != http.StatusOK {
		t.Fatalf("stream archive code = %d", streamW.Code)
	}

	// Stream execution must have acquired and released sleep inhibitor
	recInhibitor.mu.Lock()
	if len(recInhibitor.acquires) != 1 {
		t.Errorf("expected 1 acquire on archive stream, got %d", len(recInhibitor.acquires))
	}
	if recInhibitor.releases != 1 {
		t.Errorf("expected 1 release after archive stream, got %d", recInhibitor.releases)
	}
	if recInhibitor.activeLeases != 0 {
		t.Errorf("expected 0 active leases, got %d", recInhibitor.activeLeases)
	}
	recInhibitor.mu.Unlock()
}
