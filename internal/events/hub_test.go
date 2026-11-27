package events

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHubPersonalIsolationAndLifecycle(t *testing.T) {
	h := NewHub()
	defer h.Close()

	s1, err := h.SubscribePersonal("sess-1")
	if err != nil {
		t.Fatalf("SubscribePersonal s1: %v", err)
	}
	s2, err := h.SubscribePersonal("sess-2")
	if err != nil {
		t.Fatalf("SubscribePersonal s2: %v", err)
	}
	desk, err := h.SubscribeDesktop()
	if err != nil {
		t.Fatalf("SubscribeDesktop: %v", err)
	}

	h.PublishPersonal("sess-1", TypeBatchUpdated, map[string]string{"batchId": "b1", "state": "approved"})

	select {
	case ev := <-s1.C():
		if ev.Type != TypeBatchUpdated || !strings.Contains(string(ev.Payload), "b1") {
			t.Fatalf("unexpected s1 event: %+v", ev)
		}
		if strings.Contains(string(ev.Payload), "sess-1") {
			t.Fatalf("session key leaked in payload: %s", string(ev.Payload))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for s1 event")
	}

	select {
	case ev := <-s2.C():
		t.Fatalf("sess-2 received sess-1 event: %+v", ev)
	case <-time.After(30 * time.Millisecond):
	}

	select {
	case dev := <-desk.C():
		if dev.Type != TypeBatchUpdated || dev.SessionKey != "sess-1" {
			t.Fatalf("unexpected desktop event: %+v", dev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for desktop event")
	}

	h.CloseSession("sess-1", "session_revoked")
	select {
	case <-s1.Done():
		if s1.CloseReason() != "session_revoked" {
			t.Fatalf("CloseReason = %q, want session_revoked", s1.CloseReason())
		}
	case <-time.After(time.Second):
		t.Fatal("s1 did not close on CloseSession")
	}

	p, g, d := h.Stats()
	if p != 1 || g != 0 || d != 1 {
		t.Fatalf("Stats = (%d, %d, %d), want (1, 0, 1)", p, g, d)
	}
}

func TestHubGuestIsolationAndRoomClose(t *testing.T) {
	h := NewHub()
	defer h.Close()

	g1, err := h.SubscribeGuest("room-a", "guest-1")
	if err != nil {
		t.Fatalf("SubscribeGuest g1: %v", err)
	}
	g2, err := h.SubscribeGuest("room-a", "guest-2")
	if err != nil {
		t.Fatalf("SubscribeGuest g2: %v", err)
	}

	h.PublishGuest("room-a", "guest-1", TypeGuestUploadUpdated, map[string]string{"id": "up-1", "state": "approved"})

	select {
	case ev := <-g1.C():
		if ev.Type != TypeGuestUploadUpdated {
			t.Fatalf("unexpected g1 event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("g1 timed out")
	}

	select {
	case ev := <-g2.C():
		t.Fatalf("g2 received g1 private event: %+v", ev)
	case <-time.After(30 * time.Millisecond):
	}

	h.CloseRoom("room-a", "room_ended")
	for _, sub := range []*Subscription{g1, g2} {
		select {
		case <-sub.Done():
		case <-time.After(time.Second):
			t.Fatal("guest sub did not close on CloseRoom")
		}
	}
}

func TestHubBackpressureAndProgressShedding(t *testing.T) {
	h := NewHub()
	defer h.Close()

	sub, err := h.SubscribePersonal("sess-bp")
	if err != nil {
		t.Fatalf("SubscribePersonal: %v", err)
	}

	// Fill past ProgressBackpressure (48) with progress events, then send critical event.
	for i := 0; i < DefaultQueueSize; i++ {
		h.PublishPersonal("sess-bp", TypeBatchProgress, map[string]int{"i": i})
	}
	if got := len(sub.C()); got != ProgressBackpressure {
		t.Fatalf("queued progress events = %d, want %d (shedding threshold)", got, ProgressBackpressure)
	}

	h.PublishPersonal("sess-bp", TypeBatchUpdated, map[string]string{"state": "completed"})
	if got := len(sub.C()); got != ProgressBackpressure+1 {
		t.Fatalf("critical event dropped despite reserved queue headroom: len=%d", got)
	}

	// Fill remaining headroom with critical events to trigger Resync.
	for i := 0; i < DefaultQueueSize; i++ {
		h.PublishPersonal("sess-bp", TypeBatchUpdated, map[string]int{"c": i})
	}
	select {
	case <-sub.Resync():
	case <-time.After(time.Second):
		t.Fatal("expected Resync signal on queue overflow")
	}
}

func TestHubConnectionCapsAndPayloadLimits(t *testing.T) {
	h := NewHub()
	defer h.Close()

	for i := 0; i < MaxPerSessionSubs; i++ {
		if _, err := h.SubscribePersonal("sess-cap"); err != nil {
			t.Fatalf("sub %d failed unexpectedly: %v", i, err)
		}
	}
	if _, err := h.SubscribePersonal("sess-cap"); err != ErrTooManyStreams {
		t.Fatalf("expected ErrTooManyStreams, got %v", err)
	}

	// Oversized payload must be dropped silently without panicking or delivering.
	sub, err := h.SubscribePersonal("sess-big")
	if err != nil {
		t.Fatalf("SubscribePersonal: %v", err)
	}
	h.PublishPersonal("sess-big", TypeBatchUpdated, map[string]string{
		"blob": strings.Repeat("A", MaxPayloadBytes+100),
	})
	select {
	case ev := <-sub.C():
		t.Fatalf("oversized event should have been dropped, got %+v", ev)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestHubConcurrentPublishAndClose(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sub, err := h.SubscribePersonal("sess-race")
			if err != nil {
				return
			}
			for j := 0; j < 20; j++ {
				h.PublishPersonal("sess-race", TypeBatchProgress, map[string]int{"j": j})
				h.PublishDesktop(TypePairingPrompt, map[string]int{"j": j})
			}
			sub.Close()
		}(i)
	}
	wg.Wait()
	h.Close()
}

func TestHubAllowProgressThrottle(t *testing.T) {
	h := NewHub()
	defer h.Close()

	now := time.Unix(1700000000, 0)
	h.now = func() time.Time { return now }

	if !h.AllowProgress("k1", 200*time.Millisecond) {
		t.Fatal("first progress should be allowed")
	}
	if h.AllowProgress("k1", 200*time.Millisecond) {
		t.Fatal("immediate second progress should be throttled")
	}
	now = now.Add(250 * time.Millisecond)
	if !h.AllowProgress("k1", 200*time.Millisecond) {
		t.Fatal("progress after interval should be allowed")
	}
}
