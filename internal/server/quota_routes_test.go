package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestHTTP507InsufficientStorageBatchAndUpload(t *testing.T) {
	dir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var freeBytes atomic.Uint64
	freeBytes.Store(1000)

	qm := transfer.NewQuotaManager(transfer.QuotaConfig{
		DefaultDir: dir,
		Probe: func(d string) transfer.DiskSpaceInfo {
			return transfer.DiskSpaceInfo{
				TargetDir:    d,
				ProbeDir:     d,
				TargetExists: true,
				Available:    true,
				FilesystemID: "vol:http507",
				TotalBytes:   2000,
				FreeBytes:    freeBytes.Load(),
			}
		},
	})

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store, Quota: qm})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	svcs := &Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
	}
	srv := New(Config{Services: svcs})
	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// 1. Oversized single upload returns 507.
	reqBody := bytes.NewBufferString(`{"name":"too-big.bin","size":1001}`)
	req := httptest.NewRequest(http.MethodPost, "/api/uploads", reqBody)
	req.Header.Set("Authorization", "Bearer "+sess.Token())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("POST /api/uploads oversized status = %d (%s), want 507", rec.Code, rec.Body.String())
	}

	// 2. Concurrent batch approvals totaling > 1000 cannot overcommit; one approves & reserves (600), second is refused.
	createBatch := func() transfer.BatchView {
		payload := bytes.NewBufferString(`{"direction":"phone_to_desktop","kind":"files","items":[{"relativePath":"part1.bin","size":300},{"relativePath":"part2.bin","size":300}]}`)
		r := httptest.NewRequest(http.MethodPost, "/api/batches", payload)
		r.Header.Set("Authorization", "Bearer "+sess.Token())
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("createBatch status=%d (%s)", w.Code, w.Body.String())
		}
		var bv transfer.BatchView
		if err := json.Unmarshal(w.Body.Bytes(), &bv); err != nil {
			t.Fatalf("unmarshal batch: %v", err)
		}
		return bv
	}

	b1 := createBatch()
	b2 := createBatch()
	b3 := createBatch()
	b4 := createBatch()
	allBatches := []transfer.BatchView{b1, b2, b3, b4}

	var okCount, errQuotaCount atomic.Int32
	var wg sync.WaitGroup
	var approvedBatchID string
	var batchMu sync.Mutex
	for _, bv := range allBatches {
		wg.Add(1)
		go func(b transfer.BatchView) {
			defer wg.Done()
			var cids []string
			for _, it := range b.Items {
				cid, err := coord.ItemContentID(sess.Key(), b.ID, it.ID)
				if err != nil {
					t.Errorf("ItemContentID: %v", err)
					return
				}
				cids = append(cids, cid)
			}
			err := transfers.DecideBatchItems(cids, true)
			if err == nil {
				if _, appErr := coord.ApproveBatch(sess.Key(), b.ID); appErr == nil {
					okCount.Add(1)
					batchMu.Lock()
					approvedBatchID = b.ID
					batchMu.Unlock()
				}
			} else if strings.Contains(err.Error(), "insufficient storage") {
				errQuotaCount.Add(1)
			} else {
				t.Errorf("unexpected DecideBatchItems error: %v", err)
			}
		}(bv)
	}
	wg.Wait()

	if okCount.Load() != 1 || errQuotaCount.Load() != 3 {
		t.Fatalf("concurrent batch approvals ok=%d quotaRefused=%d, want ok=1 quotaRefused=3", okCount.Load(), errQuotaCount.Load())
	}
	if approvedBatchID == "" {
		t.Fatal("missing approved batch ID")
	}

	// While approved batch holds 600 of 1000 bytes, new oversized declaration returns 507.
	rOver := httptest.NewRequest(http.MethodPost, "/api/batches", bytes.NewBufferString(`{"direction":"phone_to_desktop","kind":"files","items":[{"relativePath":"fresh.bin","size":600}]}`))
	rOver.Header.Set("Authorization", "Bearer "+sess.Token())
	rOver.Header.Set("Content-Type", "application/json")
	wOver := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wOver, rOver)
	if wOver.Code != http.StatusInsufficientStorage {
		t.Fatalf("POST /api/batches with insufficient headroom status = %d (%s), want 507", wOver.Code, wOver.Body.String())
	}

	// Canceling the approved batch releases the 600-byte reservation so a new 600-byte batch can proceed.
	if bv, err := coord.GetBatch(sess.Key(), approvedBatchID); err == nil {
		for _, it := range bv.Items {
			if cid, err := coord.ItemContentID(sess.Key(), approvedBatchID, it.ID); err == nil {
				_ = transfers.Cancel(cid)
			}
		}
	}
	_, _ = coord.CancelBatch(sess.Key(), approvedBatchID)

	r2 := httptest.NewRequest(http.MethodPost, "/api/batches", bytes.NewBufferString(`{"direction":"phone_to_desktop","kind":"files","items":[{"relativePath":"fresh.bin","size":600}]}`))
	r2.Header.Set("Authorization", "Bearer "+sess.Token())
	r2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("POST /api/batches after cancellation status = %d (%s), want 201", w2.Code, w2.Body.String())
	}
}

func TestHTTP507GuestUploadDeclare(t *testing.T) {
	dir := t.TempDir()
	qm := transfer.NewQuotaManager(transfer.QuotaConfig{
		DefaultDir: dir,
		Probe: func(d string) transfer.DiskSpaceInfo {
			return transfer.DiskSpaceInfo{
				TargetDir:    d,
				ProbeDir:     d,
				TargetExists: true,
				Available:    true,
				FilesystemID: "vol:guest507",
				TotalBytes:   1000,
				FreeBytes:    200,
			}
		},
	})
	gm := guest.NewManager()
	defer gm.Close()
	gm.SetQuotaManager(qm)

	room, err := gm.CreateRoom(dir, time.Hour)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	g, err := room.Join("Alice")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if err := room.ApproveGuest(g.ID); err != nil {
		t.Fatalf("ApproveGuest: %v", err)
	}

	svcs := &Services{GuestManager: gm}
	srv := New(Config{Services: svcs})

	req := httptest.NewRequest(http.MethodPost, "/guest-api/uploads", bytes.NewBufferString(`{"name":"huge.zip","size":250}`))
	req.AddCookie(&http.Cookie{Name: guestCookieName, Value: g.Token})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("POST /guest-api/uploads status = %d (%s), want 507", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "insufficient_storage") {
		t.Fatalf("expected insufficient_storage code in body, got %s", rec.Body.String())
	}
}
