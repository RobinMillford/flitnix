package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestAppResumablePauseResumeAndDiscard(t *testing.T) {
	a := newTestApp(t)
	s, err := a.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}

	payload := make([]byte, 2*1024*1024+512)
	for i := range payload {
		payload[i] = byte(i * 73)
	}

	manifestBytes, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{DisplayName: "pause-test.bin", Size: int64(len(payload))}},
	})
	decReq, _ := http.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(manifestBytes))
	decReq.Host = "127.0.0.1"
	decReq.Header.Set("Authorization", "Bearer "+s.Token())
	decRec := httptest.NewRecorder()
	a.srv.Handler().ServeHTTP(decRec, decReq)
	if decRec.Code != http.StatusCreated {
		t.Fatalf("declare batch = %d %s", decRec.Code, decRec.Body.String())
	}
	var b BatchView
	_ = json.Unmarshal(decRec.Body.Bytes(), &b)

	if _, err := a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}

	patchURL := fmt.Sprintf("/api/batches/%s/items/%s/content", b.ID, b.Items[0].ID)
	split := int64(1024 * 1024)
	pReq, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(payload[:split]))
	pReq.Host = "127.0.0.1"
	pReq.Header.Set("Authorization", "Bearer "+s.Token())
	pReq.Header.Set("Upload-Offset", "0")
	pReq.ContentLength = split
	pRec := httptest.NewRecorder()
	a.srv.Handler().ServeHTTP(pRec, pReq)
	if pRec.Code != http.StatusNoContent {
		t.Fatalf("chunk 1 = %d %s", pRec.Code, pRec.Body.String())
	}

	// Pause batch via Wails adapter -> PATCH must be refused with 409 paused.
	pausedView, err := a.PauseBatch(b.ID)
	if err != nil || pausedView.State != "paused" {
		t.Fatalf("PauseBatch = %+v, err=%v", pausedView, err)
	}
	pRefuse, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(payload[split:split+1024]))
	pRefuse.Host = "127.0.0.1"
	pRefuse.Header.Set("Authorization", "Bearer "+s.Token())
	pRefuse.Header.Set("Upload-Offset", fmt.Sprintf("%d", split))
	pRefuse.ContentLength = 1024
	rRefuse := httptest.NewRecorder()
	a.srv.Handler().ServeHTTP(rRefuse, pRefuse)
	if rRefuse.Code != http.StatusConflict {
		t.Fatalf("PATCH while paused = %d, want 409", rRefuse.Code)
	}

	// Resume batch via Wails adapter -> Go client finishes remaining bytes!
	resumedView, err := a.ResumeBatch(b.ID)
	if err != nil || (resumedView.State != "interrupted" && resumedView.State != "queued") {
		t.Fatalf("ResumeBatch = %+v, err=%v", resumedView, err)
	}

	srcFile := filepath.Join(t.TempDir(), "pause-test.bin")
	_ = os.WriteFile(srcFile, payload, 0o600)
	target, err := client.ParseTarget(a.srv.URL()+"/#"+s.Token(), "")
	if err != nil {
		t.Fatal(err)
	}
	cli := client.NewClient(target)
	res, err := cli.StreamItem(t.Context(), b.ID, b.Items[0].ID, srcFile, nil)
	if err != nil || !res.Verified {
		t.Fatalf("StreamItem after ResumeBatch = %+v, err=%v", res, err)
	}

	// Now test DiscardBatch on a second partial batch -> releases quota & cleans .part/.meta.
	b2Req, _ := http.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(manifestBytes))
	b2Req.Host = "127.0.0.1"
	b2Req.Header.Set("Authorization", "Bearer "+s.Token())
	b2Rec := httptest.NewRecorder()
	a.srv.Handler().ServeHTTP(b2Rec, b2Req)
	var b2 BatchView
	_ = json.Unmarshal(b2Rec.Body.Bytes(), &b2)
	_, _ = a.ApproveBatch(b2.ID)

	p2Req, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("/api/batches/%s/items/%s/content", b2.ID, b2.Items[0].ID), bytes.NewReader(payload[:512*1024]))
	p2Req.Host = "127.0.0.1"
	p2Req.Header.Set("Authorization", "Bearer "+s.Token())
	p2Req.Header.Set("Upload-Offset", "0")
	p2Req.ContentLength = 512 * 1024
	p2Rec := httptest.NewRecorder()
	a.srv.Handler().ServeHTTP(p2Rec, p2Req)
	if p2Rec.Code != http.StatusNoContent {
		t.Fatalf("b2 chunk 1 = %d", p2Rec.Code)
	}

	if err := a.DiscardBatch(b2.ID); err != nil {
		t.Fatalf("DiscardBatch: %v", err)
	}
	if st := a.GetStorageStatus(); st.ReservedBytes != 0 {
		t.Fatalf("ReservedBytes after DiscardBatch = %d, want 0", st.ReservedBytes)
	}
}
