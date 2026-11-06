package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestStreamingProgress_MonotonicAndThrottled verifies that streaming upload
// progress reports monotonically, throttles callback frequency to the defined
// step (1 MiB), reports the final 100% byte count, and avoids excessive callback overhead.
func TestStreamingProgress_MonotonicAndThrottled(t *testing.T) {
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

	var progressMu sync.Mutex
	var reportedBytes []int64

	coord.SetProgressHook(func(sessionKey string, lease transfer.TransferLease, transferred int64) {
		progressMu.Lock()
		defer progressMu.Unlock()
		if len(reportedBytes) > 0 {
			prev := reportedBytes[len(reportedBytes)-1]
			if transferred < prev {
				t.Errorf("progress decreased: prev=%d, got=%d", prev, transferred)
			}
		}
		reportedBytes = append(reportedBytes, transferred)
	})

	svcs := &Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
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
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	<-srv.Ready()

	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 5.5 MiB payload: with 1 MiB progressStep, expect 5 or 6 updates (1, 2, 3, 4, 5, 5.5)
	const fileSize = int64(5.5 * 1024 * 1024)
	payload := make([]byte, fileSize)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	expectedHash := sha256.Sum256(payload)

	// Create batch
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{
				RelativePath: "progress_test.bin",
				Size:         fileSize,
			},
		},
	}
	mBody, _ := json.Marshal(manifest)
	bReq, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/batches", bytes.NewReader(mBody))
	bReq.Header.Set("Authorization", "Bearer "+sess.Token())
	bReq.Header.Set("Content-Type", "application/json")
	bRes, err := http.DefaultClient.Do(bReq)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	defer bRes.Body.Close()
	var bView transfer.BatchView
	if err := json.NewDecoder(bRes.Body).Decode(&bView); err != nil {
		t.Fatalf("decode batch view: %v", err)
	}

	// Stream upload
	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", srv.URL(), bView.ID, bView.Items[0].ID)
	putReq, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	putReq.Header.Set("Authorization", "Bearer "+sess.Token())
	putReq.Header.Set("Content-Type", "application/octet-stream")

	putRes, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT content: %v", err)
	}
	defer putRes.Body.Close()

	if putRes.StatusCode != http.StatusOK {
		t.Fatalf("PUT content status = %d, want 200", putRes.StatusCode)
	}

	var res struct {
		Written int64  `json:"written"`
		SHA256  string `json:"sha256"`
	}
	if err := json.NewDecoder(putRes.Body).Decode(&res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Written != fileSize {
		t.Fatalf("written = %d, want %d", res.Written, fileSize)
	}
	if res.SHA256 != hex.EncodeToString(expectedHash[:]) {
		t.Fatalf("SHA256 = %s, want %x", res.SHA256, expectedHash)
	}

	progressMu.Lock()
	defer progressMu.Unlock()

	// 1. Verify monotonic updates
	for i := 1; i < len(reportedBytes); i++ {
		if reportedBytes[i] < reportedBytes[i-1] {
			t.Fatalf("progress not monotonic: %v", reportedBytes)
		}
	}

	// 2. Verify final update equals exact declared file size
	if len(reportedBytes) == 0 {
		t.Fatal("no progress was reported")
	}
	last := reportedBytes[len(reportedBytes)-1]
	if last != fileSize {
		t.Fatalf("final progress reported %d, want total %d", last, fileSize)
	}

	// 3. Verify throttling: 5.5 MiB should produce ~5-6 callbacks, definitely < 15
	// (Proves no excessive callback overhead like 88 callbacks for 64 KiB chunks)
	if len(reportedBytes) < 5 || len(reportedBytes) > 10 {
		t.Fatalf("callback count = %d, expected between 5 and 10 callbacks for 5.5 MiB", len(reportedBytes))
	}
}

// TestStreamingProgress_ConcurrentUploads verifies that concurrent streaming uploads
// maintain isolated, monotonic progress sequences without cross-talk.
func TestStreamingProgress_ConcurrentUploads(t *testing.T) {
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

	var progressMu sync.Mutex
	itemProgress := make(map[string][]int64)

	coord.SetProgressHook(func(sessionKey string, lease transfer.TransferLease, transferred int64) {
		progressMu.Lock()
		defer progressMu.Unlock()
		itemProgress[lease.ItemID] = append(itemProgress[lease.ItemID], transferred)
	})

	svcs := &Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
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
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	<-srv.Ready()

	// 2 sessions transferring concurrently
	const numSessions = 2
	const sizePerFile = int64(3 * 1024 * 1024) // 3 MiB each
	var wg sync.WaitGroup

	for i := 0; i < numSessions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sess, err := sessMgr.Create()
			if err != nil {
				t.Errorf("create session %d: %v", idx, err)
				return
			}
			data := make([]byte, sizePerFile)
			data[0] = byte(idx + 1)

			manifest := transfer.BatchManifest{
				Direction: transfer.DirectionPhoneToDesktop,
				Kind:      transfer.BatchKindFiles,
				Items:     []transfer.ManifestItem{{RelativePath: fmt.Sprintf("file_%d.bin", idx), Size: sizePerFile}},
			}
			mBody, _ := json.Marshal(manifest)
			bReq, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/batches", bytes.NewReader(mBody))
			bReq.Header.Set("Authorization", "Bearer "+sess.Token())
			bReq.Header.Set("Content-Type", "application/json")
			bRes, err := http.DefaultClient.Do(bReq)
			if err != nil {
				t.Errorf("batch req %d: %v", idx, err)
				return
			}
			defer bRes.Body.Close()
			var bView transfer.BatchView
			_ = json.NewDecoder(bRes.Body).Decode(&bView)

			putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", srv.URL(), bView.ID, bView.Items[0].ID)
			putReq, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(data))
			putReq.Header.Set("Authorization", "Bearer "+sess.Token())
			putRes, err := http.DefaultClient.Do(putReq)
			if err != nil {
				t.Errorf("put req %d: %v", idx, err)
				return
			}
			defer putRes.Body.Close()
			if putRes.StatusCode != http.StatusOK {
				t.Errorf("put status %d = %d", idx, putRes.StatusCode)
			}
		}(i)
	}

	wg.Wait()

	progressMu.Lock()
	defer progressMu.Unlock()

	if len(itemProgress) != numSessions {
		t.Fatalf("expected progress for %d items, got %d", numSessions, len(itemProgress))
	}

	for id, reports := range itemProgress {
		for i := 1; i < len(reports); i++ {
			if reports[i] < reports[i-1] {
				t.Errorf("item %s progress not monotonic: %v", id, reports)
			}
		}
		if len(reports) == 0 || reports[len(reports)-1] != sizePerFile {
			t.Errorf("item %s final progress mismatch: %v", id, reports)
		}
	}
}
