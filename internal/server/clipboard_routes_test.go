package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/clipboard"
	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/events"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/textmsg"
	"github.com/itsyamin/flitnix/internal/transfer"
)

type testClipBackend struct {
	mu     sync.Mutex
	text   string
	writes []string
}

func (b *testClipBackend) Capabilities() clipboard.Capabilities {
	return clipboard.Capabilities{
		Supported:           true,
		BackendName:         "test-clipboard",
		ConcealedFilterSafe: true,
	}
}

func (b *testClipBackend) Read(ctx context.Context) (clipboard.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return clipboard.Snapshot{Text: b.text}, nil
}

func (b *testClipBackend) WriteText(ctx context.Context, text string) error {
	b.mu.Lock()
	b.text = text
	b.writes = append(b.writes, text)
	b.mu.Unlock()
	return nil
}

func setupClipboardTestServer(t *testing.T) (*httptest.Server, *Services, *session.Manager, *devices.Store, *clipboard.Coordinator, *testClipBackend) {
	t.Helper()
	dir := t.TempDir()
	devStore, err := devices.NewStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	sessMgr := session.NewManager(session.Config{})
	t.Cleanup(sessMgr.Close)
	hub := events.NewHub()
	t.Cleanup(hub.Close)
	guestMgr := guest.NewManager()
	t.Cleanup(guestMgr.Close)
	textStore := textmsg.NewStore()
	clipBack := &testClipBackend{}

	var svcs *Services
	clipCoord := clipboard.NewCoordinator(clipboard.Config{
		Backend:      clipBack,
		HostDeviceID: "host-1",
		HostName:     "HostPC",
		OnLocalSync: func(item clipboard.SyncItem) {
			publishClipboardItemToTrusted(svcs, "", item)
		},
		OnRemoteSync: func(item clipboard.SyncItem) {
			publishClipboardItemToTrusted(svcs, item.OriginDeviceID, item)
		},
	})
	t.Cleanup(clipCoord.Close)

	svcs = &Services{
		Sessions:     sessMgr,
		Registry:     transfer.NewRegistry(),
		Downloads:    transfer.NewDownloadRegistry(),
		Devices:      devStore,
		TextMessages: textStore,
		Clipboard:    clipCoord,
		GuestManager: guestMgr,
		Events:       hub,
	}
	handler := newRouter(svcs, 5*time.Second, 5*time.Second, newGrantStore(svcs.Downloads))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, svcs, sessMgr, devStore, clipCoord, clipBack
}

func TestClipboardRoutesTrustOptInAndIsolation(t *testing.T) {
	ts, svcs, sessMgr, devStore, clipCoord, clipBack := setupClipboardTestServer(t)

	// Enroll two trusted devices and create:
	//  - trustedSess1 (Phone A)
	//  - trustedSess2 (Phone B)
	//  - qrSess (untrusted ad-hoc QR browser)
	pub1, _, _ := ed25519.GenerateKey(nil)
	pub2, _, _ := ed25519.GenerateKey(nil)
	dev1, err := devStore.Add("Phone-A", pub1, false)
	if err != nil {
		t.Fatal(err)
	}
	dev2, err := devStore.Add("Phone-B", pub2, false)
	if err != nil {
		t.Fatal(err)
	}
	trustedSess1, err := sessMgr.CreateForDevice(dev1.ID)
	if err != nil {
		t.Fatal(err)
	}
	trustedSess2, err := sessMgr.CreateForDevice(dev2.ID)
	if err != nil {
		t.Fatal(err)
	}
	qrSess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}

	// Subscribe to personal events for Phone B and QR Session to verify isolation.
	subTrusted2, err := svcs.Events.SubscribePersonal(trustedSess2.Key())
	if err != nil {
		t.Fatal(err)
	}
	defer subTrusted2.Close()
	subQR, err := svcs.Events.SubscribePersonal(qrSess.Key())
	if err != nil {
		t.Fatal(err)
	}
	defer subQR.Close()

	// 1. Default OFF: trusted device sees enabled=false and cannot push.
	cli := &http.Client{}
	getOff := doBearerReq(t, cli, http.MethodGet, ts.URL+"/api/clipboard", trustedSess1.Token(), nil)
	var stMap map[string]any
	_ = json.NewDecoder(getOff.Body).Decode(&stMap)
	getOff.Body.Close()
	if getOff.StatusCode != http.StatusOK || stMap["enabled"] != false {
		t.Fatalf("expected enabled=false by default, got status=%d body=%+v", getOff.StatusCode, stMap)
	}
	pushOff := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/clipboard", trustedSess1.Token(), map[string]string{
		"content": "should fail while disabled",
	})
	pushOff.Body.Close()
	if pushOff.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /api/clipboard when disabled = %d; want 403", pushOff.StatusCode)
	}

	// 2. Enable opt-in clipboard sync. Untrusted QR phone STILL gets 403 trusted_device_required!
	clipCoord.SetEnabled(true)
	refreshClipboardAuthorizedPeers(svcs)

	qrGet := doBearerReq(t, cli, http.MethodGet, ts.URL+"/api/clipboard", qrSess.Token(), nil)
	qrGet.Body.Close()
	if qrGet.StatusCode != http.StatusForbidden {
		t.Fatalf("untrusted QR GET /api/clipboard = %d; want 403", qrGet.StatusCode)
	}
	qrPost := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/clipboard", qrSess.Token(), map[string]string{
		"content": "untrusted attempt",
	})
	qrPost.Body.Close()
	if qrPost.StatusCode != http.StatusForbidden {
		t.Fatalf("untrusted QR POST /api/clipboard = %d; want 403", qrPost.StatusCode)
	}

	// 3. Trusted Phone A pushes oversized (> 64 KiB) -> 413 Request Entity Too Large.
	tooLarge := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/clipboard", trustedSess1.Token(), map[string]string{
		"content": strings.Repeat("A", clipboard.MaxContentBytes+1),
	})
	tooLarge.Body.Close()
	if tooLarge.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized clipboard POST = %d; want 413", tooLarge.StatusCode)
	}

	// 4. Trusted Phone A pushes valid UTF-8 -> updates desktop backend & emits ONLY to trusted Phone B (NOT QR phone!).
	pushOK := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/clipboard", trustedSess1.Token(), map[string]string{
		"content": "trusted-clipboard-text-123",
	})
	pushOK.Body.Close()
	if pushOK.StatusCode != http.StatusOK {
		t.Fatalf("valid trusted POST /api/clipboard = %d; want 200", pushOK.StatusCode)
	}
	snap, _ := clipBack.Read(context.Background())
	if snap.Text != "trusted-clipboard-text-123" {
		t.Fatalf("desktop clipboard backend text = %q; want trusted-clipboard-text-123", snap.Text)
	}

	select {
	case ev := <-subTrusted2.C():
		if ev.Type != events.TypeClipboardUpdated {
			t.Fatalf("unexpected event type on trusted Phone B: %s", ev.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("trusted Phone B did not receive clipboard.updated event")
	}

	select {
	case ev := <-subQR.C():
		t.Fatalf("untrusted QR session unexpectedly received event: %s", ev.Type)
	case <-time.After(80 * time.Millisecond):
		// expected: zero events on untrusted QR session
	}

	// 5. Clipboard content MUST NEVER be saved into textmsg Quick Text history.
	if msgs := svcs.TextMessages.List(trustedSess1.Key()); len(msgs) != 0 {
		t.Fatalf("clipboard text leaked into Quick Text store: %+v", msgs)
	}

	// 6. Pause sync blocks push (409) & clears transient Latest.
	clipCoord.SetPaused(true)
	pushPaused := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/clipboard", trustedSess1.Token(), map[string]string{
		"content": "while paused",
	})
	pushPaused.Body.Close()
	if pushPaused.StatusCode != http.StatusConflict {
		t.Fatalf("POST while paused = %d; want 409", pushPaused.StatusCode)
	}
	if _, ok := clipCoord.Latest(); ok {
		t.Fatal("expected Latest cleared when paused")
	}

	// 7. Revoking Phone A via DELETE /api/devices/{id} revokes its token immediately.
	clipCoord.SetPaused(false)
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/devices/"+dev1.ID, nil)
	delReq.Header.Set("Authorization", "Bearer "+qrSess.Token())
	delResp, err := cli.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(delResp.Body)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE device = %d; want 200", delResp.StatusCode)
	}
	afterRevoke := doBearerReq(t, cli, http.MethodGet, ts.URL+"/api/clipboard", trustedSess1.Token(), nil)
	afterRevoke.Body.Close()
	if afterRevoke.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device GET /api/clipboard = %d; want 401", afterRevoke.StatusCode)
	}
}
