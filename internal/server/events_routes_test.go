package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/events"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/textmsg"
	"github.com/itsyamin/flitnix/internal/transfer"
)

type sseFrame struct {
	ID    string
	Event string
	Data  string
}

type sseTestReader struct {
	ch    chan sseFrame
	errCh chan error
}

func newSSETestReader(r *bufio.Reader) *sseTestReader {
	sr := &sseTestReader{
		ch:    make(chan sseFrame, 32),
		errCh: make(chan error, 1),
	}
	go func() {
		var cur sseFrame
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				sr.errCh <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if cur.Event != "" || cur.Data != "" || cur.ID != "" {
					sr.ch <- cur
					cur = sseFrame{}
				}
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "id:") {
				cur.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			} else if strings.HasPrefix(line, "event:") {
				cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if cur.Data != "" {
					cur.Data += "\n" + val
				} else {
					cur.Data = val
				}
			}
		}
	}()
	return sr
}

func (sr *sseTestReader) read(t *testing.T, wantCount int, timeout time.Duration) []sseFrame {
	t.Helper()
	var frames []sseFrame
	deadline := time.After(timeout)
	for len(frames) < wantCount {
		select {
		case f := <-sr.ch:
			frames = append(frames, f)
		case err := <-sr.errCh:
			if len(frames) >= wantCount {
				return frames
			}
			t.Fatalf("SSE stream ended early after %d/%d frames: %v", len(frames), wantCount, err)
		case <-deadline:
			t.Fatalf("timed out waiting for SSE frames (%d/%d received): %+v", len(frames), wantCount, frames)
		}
	}
	return frames
}

func startEventsTestServer(t *testing.T) (*Server, *Services, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	hub := events.NewHub()
	sessMgr := session.NewManager(session.Config{
		IdleTimeout: 30 * time.Second,
		OnExpire: func(key string) {
			hub.CloseSession(key, "session_expired")
		},
		OnEnd: func(ev session.EndEvent) {
			reason := "session_revoked"
			if ev.Reason == session.EndIdleExpiry {
				reason = "session_expired"
			}
			hub.CloseSession(ev.Key, reason)
		},
	})
	devStore, err := devices.NewStore("")
	if err != nil {
		t.Fatalf("devices.NewStore: %v", err)
	}
	svcs := &Services{
		Sessions:     sessMgr,
		Transfers:    transfer.NewService(transfer.Config{Store: store}),
		Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:     transfer.NewRegistry(),
		Downloads:    transfer.NewDownloadRegistry(),
		GuestManager: guest.NewManager(),
		TextMessages: textmsg.NewStore(),
		Devices:      devStore,
		Events:       hub,
	}
	srv := New(Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("server did not start in time")
	}
	t.Cleanup(func() {
		cancel()
		hub.Close()
		svcs.GuestManager.Close()
		sessMgr.Close()
	})
	return srv, svcs, cancel
}

func TestPersonalEventsSecurityAndDelivery(t *testing.T) {
	srv, svcs, _ := startEventsTestServer(t)
	client := &http.Client{}

	// 1. Unauthenticated request rejected
	resp, err := client.Get(srv.URL() + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	sess, err := svcs.Sessions.Create()
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	otherSess, err := svcs.Sessions.Create()
	if err != nil {
		t.Fatalf("Create other session: %v", err)
	}

	// 2. Origin spoofing & Sec-Fetch-Site cross-site rejected
	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{"evil_origin", "Origin", "http://evil.example.com"},
		{"null_origin", "Origin", "null"},
		{"cross_site", "Sec-Fetch-Site", "cross-site"},
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/events", nil)
		req.Header.Set("Authorization", "Bearer "+sess.Token())
		req.Header.Set(tc.header, tc.value)
		r, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s request: %v", tc.name, err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403", tc.name, r.StatusCode)
		}
	}

	// 3. Valid authenticated SSE stream receives ready + live events, isolated from other sessions
	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL()+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+sess.Token())
	req.Header.Set("Origin", srv.URL())
	streamResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("connect /api/events: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, want 200", streamResp.StatusCode)
	}

	sse := newSSETestReader(bufio.NewReader(streamResp.Body))
	frames := sse.read(t, 1, 2*time.Second)
	if frames[0].Event != "ready" {
		t.Fatalf("first event = %q, want ready", frames[0].Event)
	}

	// Publish to other session -> must not arrive on sess
	svcs.Events.PublishPersonal(otherSess.Key(), events.TypeTextMessage, map[string]string{"id": "other"})

	// Publish Quick Text + Batch Update to sess -> must arrive without leaking tokens/keys
	svcs.Events.PublishPersonal(sess.Key(), events.TypeTextMessage, map[string]string{"id": "m1", "content": "hello"})
	svcs.Events.PublishPersonal(sess.Key(), events.TypeBatchUpdated, map[string]string{"id": "b1", "state": "queued"})

	got := sse.read(t, 2, 2*time.Second)
	if got[0].Event != events.TypeTextMessage || !strings.Contains(got[0].Data, `"m1"`) {
		t.Fatalf("unexpected frame[0]: %+v", got[0])
	}
	if got[1].Event != events.TypeBatchUpdated || !strings.Contains(got[1].Data, `"b1"`) {
		t.Fatalf("unexpected frame[1]: %+v", got[1])
	}
	for _, f := range got {
		if strings.Contains(f.Data, sess.Token()) || strings.Contains(f.Data, sess.Key()) {
			t.Fatalf("sensitive session material leaked in SSE frame: %s", f.Data)
		}
	}

	// 4. Revoking the session closes the SSE stream with session.ended
	svcs.Sessions.RevokeSession(sess)
	endFrames := sse.read(t, 1, 2*time.Second)
	if endFrames[0].Event != events.TypeSessionEnded {
		t.Fatalf("expected %s on revocation, got %+v", events.TypeSessionEnded, endFrames[0])
	}
}

func TestPersonalEventsRevokedDeviceRejected(t *testing.T) {
	srv, svcs, _ := startEventsTestServer(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dev, err := svcs.Devices.Add("Test Phone", pub, false)
	if err != nil {
		t.Fatalf("Devices.Add: %v", err)
	}
	sess, err := svcs.Sessions.CreateForDevice(dev.ID)
	if err != nil {
		t.Fatalf("CreateForDevice: %v", err)
	}
	if err := svcs.Devices.Remove(dev.ID); err != nil {
		t.Fatalf("Devices.Remove: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/events", nil)
	req.Header.Set("Authorization", "Bearer "+sess.Token())
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device status = %d, want 401", resp.StatusCode)
	}
}

func TestGuestEventsSecurityAndLifecycle(t *testing.T) {
	srv, svcs, _ := startEventsTestServer(t)
	client := &http.Client{}

	// 1. Missing guest token rejected
	resp, err := client.Get(srv.URL() + "/guest-api/events")
	if err != nil {
		t.Fatalf("GET /guest-api/events: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated guest status = %d, want 401", resp.StatusCode)
	}

	room, err := svcs.GuestManager.CreateRoom(t.TempDir(), 10*time.Minute)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	g, err := room.Join("Alice")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	// 2. Origin spoofing rejected
	badReq, _ := http.NewRequest(http.MethodGet, srv.URL()+"/guest-api/events", nil)
	badReq.AddCookie(&http.Cookie{Name: guestCookieName, Value: g.Token})
	badReq.Header.Set("Origin", "http://attacker.example.com")
	badResp, err := client.Do(badReq)
	if err != nil {
		t.Fatalf("spoofed guest request: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("spoofed guest origin status = %d, want 403", badResp.StatusCode)
	}

	// 3. Valid guest cookie connects and receives guest status + room end
	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL()+"/guest-api/events", nil)
	req.AddCookie(&http.Cookie{Name: guestCookieName, Value: g.Token})
	streamResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("connect /guest-api/events: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("guest SSE status = %d, want 200", streamResp.StatusCode)
	}

	sse := newSSETestReader(bufio.NewReader(streamResp.Body))
	frames := sse.read(t, 1, 2*time.Second)
	if frames[0].Event != "ready" {
		t.Fatalf("guest first event = %q, want ready", frames[0].Event)
	}

	svcs.Events.PublishGuest(room.ID, g.ID, events.TypeGuestStatus, map[string]string{
		"guestId": g.ID,
		"state":   "approved",
	})
	got := sse.read(t, 1, 2*time.Second)
	if got[0].Event != events.TypeGuestStatus {
		t.Fatalf("guest event = %q, want %s", got[0].Event, events.TypeGuestStatus)
	}

	var env struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(got[0].Data), &env); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if !bytes.Contains(env.Payload, []byte(`"approved"`)) {
		t.Fatalf("unexpected guest payload: %s", string(env.Payload))
	}

	svcs.Events.CloseRoom(room.ID, "room_ended")
	endFrames := sse.read(t, 1, 2*time.Second)
	if endFrames[0].Event != events.TypeSessionEnded {
		t.Fatalf("expected %s on CloseRoom, got %+v", events.TypeSessionEnded, endFrames[0])
	}
}
