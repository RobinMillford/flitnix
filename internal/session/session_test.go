package session

import (
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a mutex-protected clock tests can advance at will.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	return &fakeClock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func mustCreate(t *testing.T, m *Manager) *Session {
	t.Helper()
	s, err := m.Create()
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	return s
}

func TestTokenShape(t *testing.T) {
	s := mustCreate(t, NewManager(Config{}))

	if s.Token() == "" {
		t.Fatal("token is empty")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s.Token())
	if err != nil {
		t.Fatalf("token is not strict base64url: %v", err)
	}
	if len(raw) != TokenLen {
		t.Fatalf("token entropy is %d bytes, want %d (256-bit)", len(raw), TokenLen)
	}
	// URL-safe charset: strict decoding into a URL query would still be
	// checked implicitly, but assert no characters that need escaping.
	if strings.ContainsAny(s.Token(), "+/= ?#&") {
		t.Errorf("token %q is not URL-safe", s.Token())
	}
}

func TestTokenUniqueness(t *testing.T) {
	m := NewManager(Config{MaxSessions: 64})
	tokens := make(map[string]bool)
	for i := 0; i < 64; i++ {
		s := mustCreate(t, m)
		if tokens[s.Token()] {
			t.Fatalf("duplicate token generated at iteration %d", i)
		}
		tokens[s.Token()] = true
	}
}

func TestValidateAcceptsOwnToken(t *testing.T) {
	m := NewManager(Config{})
	s := mustCreate(t, m)

	got, err := m.Validate(s.Token())
	if err != nil {
		t.Fatalf("Validate(own token) failed: %v", err)
	}
	if got != s {
		t.Fatal("Validate returned a different session than the one created")
	}
}

func TestValidateRejectsMalformed(t *testing.T) {
	m := NewManager(Config{})
	cases := []string{
		"",
		"short",
		strings.Repeat("a", tokenEncodedLen), // wrong charset ('a' is fine actually... base64 size)
		"a+bC123wYcGxhY2Vob2xkZXJ2YWx1ZQ",    // contains '+'
		strings.Repeat("A", tokenEncodedLen) + "x",
		"ΩΩΩΩΩ", // not base64url at all
	}
	for _, tok := range cases {
		if _, err := m.Validate(tok); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate(%q) err = %v, want ErrInvalid", tok, err)
		}
	}
}

func TestValidateRejectsUnknownTokens(t *testing.T) {
	m := NewManager(Config{})
	// A well-formed token nobody ever created.
	unknown := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	if _, err := m.Validate(unknown); !errors.Is(err, ErrInvalid) {
		t.Errorf("Validate(unknown) err = %v, want ErrInvalid", err)
	}
}

func TestIdleExpiry(t *testing.T) {
	clock := newFakeClock(t)
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now})
	s := mustCreate(t, m)

	if _, err := m.Validate(s.Token()); err != nil {
		t.Fatalf("Validate before expiry failed: %v", err)
	}

	clock.advance(30 * time.Second)
	// Activity refreshes the sliding idle window.
	if _, err := m.Validate(s.Token()); err != nil {
		t.Fatalf("Validate mid-window failed: %v", err)
	}
	clock.advance(30 * time.Second)
	if _, err := m.Validate(s.Token()); err != nil {
		t.Fatalf("session expired before the refreshed deadline elapsed: %v", err)
	}

	clock.advance(61 * time.Second)
	if _, err := m.Validate(s.Token()); !errors.Is(err, ErrExpired) {
		t.Errorf("Validate past idle deadline err = %v, want ErrExpired", err)
	}
	// The precise error persists for subsequent attempts.
	if _, err := m.Validate(s.Token()); !errors.Is(err, ErrExpired) {
		t.Errorf("second validate past expiry err = %v, want ErrExpired", err)
	}
}

func TestRevocationByToken(t *testing.T) {
	m := NewManager(Config{})
	s := mustCreate(t, m)

	if err := m.Revoke(s.Token()); err != nil {
		t.Fatalf("Revoke failed: %v", err)
	}
	if _, err := m.Validate(s.Token()); !errors.Is(err, ErrRevoked) {
		t.Errorf("Validate of revoked token err = %v, want ErrRevoked", err)
	}
	if err := m.Revoke(s.Token()); !errors.Is(err, ErrInvalid) {
		t.Errorf("double Revoke err = %v, want ErrInvalid", err)
	}
	if got := s.Status(); got != StatusRevoked {
		t.Errorf("Status after revoke = %q, want %q", got, StatusRevoked)
	}
}

func TestRevocationBySessionPointer(t *testing.T) {
	m := NewManager(Config{})
	s := mustCreate(t, m)
	m.RevokeSession(s)

	if _, err := m.Validate(s.Token()); !errors.Is(err, ErrRevoked) {
		t.Errorf("Validate of revoked session err = %v, want ErrRevoked", err)
	}
	if m.Len() != 0 {
		t.Errorf("Len after revocation = %d, want 0", m.Len())
	}
}

func TestClosedManagerRevokesEverything(t *testing.T) {
	m := NewManager(Config{})
	s1 := mustCreate(t, m)
	s2 := mustCreate(t, m)

	m.Close()
	for _, s := range []*Session{s1, s2} {
		if _, err := m.Validate(s.Token()); !errors.Is(err, ErrRevoked) {
			t.Errorf("Validate after Close err = %v, want ErrRevoked", err)
		}
	}
	if _, err := m.Create(); !errors.Is(err, ErrClosed) {
		t.Errorf("Create after Close err = %v, want ErrClosed", err)
	}
	if m.Len() != 0 {
		t.Errorf("Len after Close = %d, want 0", m.Len())
	}
}

func TestSessionCap(t *testing.T) {
	m := NewManager(Config{MaxSessions: 3})
	for i := 0; i < 3; i++ {
		mustCreate(t, m)
	}
	if _, err := m.Create(); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("Create beyond cap err = %v, want ErrTooManySessions", err)
	}
}

func TestPinnedSessionSurvivesIdleDeadline(t *testing.T) {
	clock := newFakeClock(t)
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now})
	s := mustCreate(t, m)

	// A transfer starts and pins the session, then the transfer runs far
	// beyond the idle window.
	s.Begin()
	clock.advance(10 * time.Minute)
	if got := s.Status(); got != StatusActive {
		t.Errorf("pinned session Status = %q, want %q", got, StatusActive)
	}
	if _, err := m.Validate(s.Token()); err != nil {
		t.Fatalf("Validate of pinned session failed: %v", err)
	}

	// The transfer finishes; the session is now idle-dead and must be
	// expired deterministically by the next validation pass.
	s.End()
	clock.advance(61 * time.Second)
	if _, err := m.Validate(s.Token()); !errors.Is(err, ErrExpired) {
		t.Errorf("Validate of unpinned staled session err = %v, want ErrExpired", err)
	}
}

func TestNewActivityWhilePinnedRefreshesDeadline(t *testing.T) {
	// A phone polling the status during a transfer should keep the session
	// alive too; the sliding window applies during pins with any request.
	clock := newFakeClock(t)
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now})
	s := mustCreate(t, m)
	s.Begin()

	clock.advance(30 * time.Second)
	if _, err := m.Validate(s.Token()); err != nil {
		t.Fatalf("mid-transfer validate failed: %v", err)
	}
	clock.advance(30 * time.Second)
	if _, err := m.Validate(s.Token()); err != nil {
		t.Fatalf("validate at the old deadline failed although activity refreshed it: %v", err)
	}
}

func TestValidatorIsConcurrencySafe(t *testing.T) {
	m := NewManager(Config{MaxSessions: 16})
	sessions := make([]*Session, 16)
	for i := range sessions {
		sessions[i] = mustCreate(t, m)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range sessions {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := m.Validate(token); err != nil {
					errs <- err
					return
				}
			}
		}(sessions[i].Token())
	}
	m.Len() // exercise the locked paths while the goroutines are running
	m.Close()
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrRevoked) {
			t.Errorf("unexpected error during concurrent validation: %v", err)
		}
	}
}

func TestHistoryBounded(t *testing.T) {
	clock := newFakeClock(t)
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now, MaxSessions: 64})
	for i := 0; i < 200; i++ {
		s := mustCreate(t, m)
		clock.advance(2 * time.Minute) // each expires before the next is created
		_ = s
	}
	// Force a validation pass so pruning happens.
	mustCreate(t, m)

	m.mu.Lock()
	n := len(m.hist)
	m.mu.Unlock()
	if n > maxHistory {
		t.Fatalf("history grew to %d entries, want at most %d", n, maxHistory)
	}
}

func TestSessionKeyIsStableAndNotTheToken(t *testing.T) {
	m := NewManager(Config{})
	s := mustCreate(t, m)

	key := s.Key()
	if key == s.Token() {
		t.Fatal("Key() must not equal the raw token")
	}
	again := s.Key()
	if again != key {
		t.Fatal("Key() is not stable across calls")
	}
	if strings.Contains(strings.ToLower(key), strings.ToLower(s.Token())) {
		t.Fatal("key appears to embed the raw token material")
	}
}

func TestEndRestartsIdleWindowAfterLongTransfer(t *testing.T) {
	// The reported bug: a transfer that outlasts the idle window used to
	// leave the session expired the instant it was unpinned, killing the very
	// next request (the following batch item or status poll).
	clock := newFakeClock(t)
	m := NewManager(Config{IdleTimeout: 10 * time.Minute, Now: clock.Now})
	s := mustCreate(t, m)
	token := s.Token()

	s.Begin()
	clock.advance(25 * time.Minute) // far beyond the idle window, no other traffic
	s.End()

	if _, err := m.Validate(token); err != nil {
		t.Fatalf("session died the moment a long transfer finished: %v", err)
	}
	// Ordinary idle expiry still applies afterwards.
	clock.advance(10*time.Minute + time.Second)
	if _, err := m.Validate(token); !errors.Is(err, ErrExpired) {
		t.Fatalf("idle session err = %v, want ErrExpired", err)
	}
}

func TestBeginRefreshesIdleWindow(t *testing.T) {
	clock := newFakeClock(t)
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now})
	s := mustCreate(t, m)
	clock.advance(50 * time.Second)
	s.Begin()
	s.End()
	clock.advance(50 * time.Second) // 100s after creation, 50s after the transfer
	if _, err := m.Validate(s.Token()); err != nil {
		t.Fatalf("recent transfer did not count as activity: %v", err)
	}
}

func TestOnExpireFiresOnlyForIdleUnpinnedSessions(t *testing.T) {
	clock := newFakeClock(t)
	var mu sync.Mutex
	var expired []string
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now, OnExpire: func(key string) {
		mu.Lock()
		expired = append(expired, key)
		mu.Unlock()
	}})
	idle := mustCreate(t, m)
	busy := mustCreate(t, m)
	revoked := mustCreate(t, m)
	m.RevokeSession(revoked)
	busy.Begin() // a live transfer stream pins its session

	clock.advance(5 * time.Minute)
	if got := m.Len(); got != 1 {
		t.Fatalf("Len = %d, want only the pinned session", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(expired) != 1 || expired[0] != idle.Key() {
		t.Fatalf("expired = %v, want exactly the idle session %s", expired, idle.Key())
	}
	if busy.Status() != StatusActive {
		t.Fatal("pinned session must stay active")
	}
}

func TestOnExpireCallbackMayReenterManager(t *testing.T) {
	clock := newFakeClock(t)
	var m *Manager
	called := make(chan struct{}, 4)
	m = NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now, OnExpire: func(string) {
		_ = m.Len() // would deadlock if the hook ran under the Manager lock
		called <- struct{}{}
	}})
	mustCreate(t, m)
	clock.advance(2 * time.Minute)
	m.ActiveSessions()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("OnExpire was not delivered (or deadlocked)")
	}
}

func TestPruneExpiresEveryStaleSessionAndKeepsTheLive(t *testing.T) {
	// Several stale sessions around a live one: none may be skipped, lost or
	// recorded twice.
	clock := newFakeClock(t)
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now})
	a := mustCreate(t, m)
	b := mustCreate(t, m)
	c := mustCreate(t, m)
	d := mustCreate(t, m)
	clock.advance(30 * time.Second)
	if _, err := m.Validate(b.Token()); err != nil { // keep b alive
		t.Fatal(err)
	}
	clock.advance(45 * time.Second) // a, c, d stale; b alive
	if got := m.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
	if _, err := m.Validate(b.Token()); err != nil {
		t.Fatalf("live session was dropped by pruning: %v", err)
	}
	for _, s := range []*Session{a, c, d} {
		if _, err := m.Validate(s.Token()); !errors.Is(err, ErrExpired) {
			t.Fatalf("stale session err = %v, want ErrExpired", err)
		}
	}
}

func TestCloseMarksSessionsRevoked(t *testing.T) {
	m := NewManager(Config{})
	s := mustCreate(t, m)
	m.Close()
	if got := s.Status(); got != StatusRevoked {
		t.Fatalf("Status after Close = %q, want revoked (streams watch it)", got)
	}
}

func TestEndNeverExposesAnUnpinnedSessionWithAStaleDeadline(t *testing.T) {
	// A prune running concurrently with End must see either "still pinned" or
	// "fresh deadline", never "unpinned + deadline from the transfer's start".
	clock := newFakeClock(t)
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now})
	s := mustCreate(t, m)
	for i := 0; i < 300; i++ {
		s.Begin()
		clock.advance(time.Hour) // the transfer outlasts the idle window
		done := make(chan struct{})
		go func() {
			defer close(done)
			for j := 0; j < 50; j++ {
				_ = m.Len()
			}
		}()
		s.End()
		<-done
		if m.Len() != 1 {
			t.Fatalf("iteration %d: session expired at the end of a long transfer", i)
		}
	}
}

// ---- OnEnd end-reason diagnostics ----

// endRecorder collects OnEnd events for assertions.
type endRecorder struct {
	mu     sync.Mutex
	events []EndEvent
}

func (r *endRecorder) callback(ev EndEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *endRecorder) snapshot() []EndEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]EndEvent(nil), r.events...)
}

func TestOnEndFiresOncePerSessionWithTheRightReason(t *testing.T) {
	// One manager, three endings, every category distinct and exactly once:
	// idle expiry, revocation, manager close. Each session is created while
	// the clock still has headroom for its own ending.
	clock := newFakeClock(t)
	rec := &endRecorder{}
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now, OnEnd: rec.callback})
	idle := mustCreate(t, m)

	clock.advance(2 * time.Minute) // idle expires lazily
	if _, err := m.Validate(idle.Token()); !errors.Is(err, ErrExpired) {
		t.Fatalf("idle token err = %v, want ErrExpired", err)
	}

	revoked := mustCreate(t, m)
	m.RevokeSession(revoked)

	last := mustCreate(t, m)
	m.Close()

	want := []struct {
		key    string
		reason EndReason
	}{
		{idle.Key(), EndIdleExpiry},
		{revoked.Key(), EndRevoked},
		{last.Key(), EndShutdown},
	}
	got := rec.snapshot()
	if len(got) != len(want) {
		t.Fatalf("OnEnd events = %v, want %d events", got, len(want))
	}
	for i, w := range want {
		if got[i].Key != w.key || got[i].Reason != w.reason {
			t.Fatalf("event %d = {%.8s %s}, want {%.8s %s}", i, got[i].Key, got[i].Reason, w.key, w.reason)
		}
	}
	// Keys are non-secret digests, never raw tokens.
	for _, ev := range got {
		if ev.Key == idle.Token() || ev.Key == revoked.Token() || ev.Key == last.Token() {
			t.Fatalf("OnEnd leaked a raw token in %q", ev.Key)
		}
	}
}

func TestOnEndDoesNotRepeatForRevalidateAndPrune(t *testing.T) {
	// A session ends exactly once: the expired error is served from
	// bounded history, and revalidating that token must never emit a
	// further OnEnd event, no matter how many passes run.
	clock := newFakeClock(t)
	rec := &endRecorder{}
	m := NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now, OnEnd: rec.callback})
	s := mustCreate(t, m)
	key := s.Key()

	clock.advance(2 * time.Minute)
	if _, err := m.Validate(s.Token()); !errors.Is(err, ErrExpired) {
		t.Fatalf("dead token err = %v, want ErrExpired", err)
	}
	live := mustCreate(t, m)
	for i := 0; i < 3; i++ {
		_, _ = m.Validate(s.Token()) // served from history: no event
		if _, err := m.Validate(live.Token()); err != nil {
			t.Fatal(err) // keep the live session alive through every prune pass
		}
	}
	events := rec.snapshot()
	if len(events) != 1 || events[0].Key != key || events[0].Reason != EndIdleExpiry {
		t.Fatalf("OnEnd events = %v, want exactly one idle_expiry for %s", events, key)
	}
}

func TestOnEndCallbackMayReenterManager(t *testing.T) {
	// Same reentrancy contract as OnExpire: no Manager lock while delivering.
	clock := newFakeClock(t)
	var m *Manager
	called := make(chan struct{}, 4)
	m = NewManager(Config{IdleTimeout: time.Minute, Now: clock.Now, OnEnd: func(EndEvent) {
		_ = m.Len()            // would deadlock if the hook ran under the lock
		_ = m.ActiveSessions() // same
		called <- struct{}{}
	}})
	mustCreate(t, m)
	clock.advance(2 * time.Minute)
	m.Len()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("OnEnd was not delivered (or deadlocked)")
	}
}

func TestOnEndQueueStaysBoundedWithoutAConsumerAndOnExpireStillFires(t *testing.T) {
	// Without OnEnd configured, endings must not queue events at all; the
	// OnExpire release path must keep working unchanged next to it.
	clock := newFakeClock(t)
	var expired []string
	m := NewManager(Config{
		IdleTimeout: time.Minute, Now: clock.Now,
		OnExpire: func(key string) { expired = append(expired, key) },
	})
	s := mustCreate(t, m)
	clock.advance(2 * time.Minute)
	m.Len()
	if len(m.ended) != 0 {
		t.Fatalf("ended queue = %v without OnEnd, want empty (events must not pile up)", m.ended)
	}
	if _, err := m.Validate(s.Token()); !errors.Is(err, ErrExpired) {
		t.Fatalf("token err = %v, want ErrExpired", err)
	}
	if len(expired) != 1 || expired[0] != s.Key() {
		t.Fatalf("OnExpire = %v, want exactly %s", expired, s.Key())
	}
}

func TestRevokeEmitsRevokedEndEvent(t *testing.T) {
	// Revoke by token (the phone-facing or explicit path) reports the same
	// revoked category as RevokeSession, delivered before Revoke returns.
	rec := &endRecorder{}
	m := NewManager(Config{OnEnd: rec.callback})
	s := mustCreate(t, m)
	if err := m.Revoke(s.Token()); err != nil {
		t.Fatal(err)
	}
	events := rec.snapshot()
	if len(events) != 1 || events[0].Key != s.Key() || events[0].Reason != EndRevoked {
		t.Fatalf("OnEnd events = %v, want one revoked for %s", events, s.Key())
	}
	if err := m.Revoke(s.Token()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second revoke = %v, want ErrInvalid (session already gone)", err)
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Fatalf("revoking a dead token emitted %d extra events", got-1)
	}
}
