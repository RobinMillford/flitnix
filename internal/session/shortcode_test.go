package session

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestShortCodeLifecycleAndReplayProtection(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	mgr := NewShortCodeManager(ShortCodeConfig{
		TTL: 3 * time.Minute,
		Now: func() time.Time { return now },
	})

	view, err := mgr.GetOrCreate("sess-key-1")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if len(view.Code) != ShortCodeLength || view.FormattedCode == "" {
		t.Fatalf("unexpected short code view: %+v", view)
	}
	for i := 0; i < len(view.Code); i++ {
		if view.Code[i] < '0' || view.Code[i] > '9' {
			t.Fatalf("code contains non-digit: %q", view.Code)
		}
	}

	// Idempotent until used, expired, or regenerated.
	again, err := mgr.GetOrCreate("sess-key-1")
	if err != nil || again.Code != view.Code {
		t.Fatalf("GetOrCreate not idempotent: %+v err=%v", again, err)
	}

	// Redeem with formatted space/dash succeeds once and binds session key.
	spaced := view.Code[:3] + " " + view.Code[3:]
	red, err := mgr.Redeem("192.168.1.50", spaced)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if red.SessionKey != "sess-key-1" {
		t.Fatalf("redeemed SessionKey = %q, want sess-key-1", red.SessionKey)
	}

	// Single-use: second attempt fails cleanly as already used.
	if _, err := mgr.Redeem("192.168.1.51", view.Code); !errors.Is(err, ErrShortCodeUsed) {
		t.Fatalf("replay err = %v, want ErrShortCodeUsed", err)
	}

	// Expiration enforces TTL strictly.
	reg, err := mgr.Regenerate("sess-key-1")
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	now = now.Add(3*time.Minute + time.Second)
	if _, err := mgr.Redeem("192.168.1.51", reg.Code); !errors.Is(err, ErrShortCodeExpired) {
		t.Fatalf("expired err = %v, want ErrShortCodeExpired", err)
	}
}

func TestShortCodeConcurrentSingleWinner(t *testing.T) {
	mgr := NewShortCodeManager(ShortCodeConfig{})
	view, err := mgr.GetOrCreate("sess-race")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	wins := make(chan string, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			ip := "192.168.1." + string(rune('1'+idx%9))
			if res, rErr := mgr.Redeem(ip, view.Code); rErr == nil {
				wins <- res.SessionKey
			}
		}(i)
	}
	wg.Wait()
	close(wins)

	count := 0
	for range wins {
		count++
	}
	if count != 1 {
		t.Fatalf("concurrent wins = %d, want exactly 1", count)
	}
}

func TestShortCodePerIPRateLimitAndNoGlobalDoS(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	mgr := NewShortCodeManager(ShortCodeConfig{
		TTL:             3 * time.Minute,
		MaxFailures:     5,
		LockoutDuration: 2 * time.Minute,
		Now:             func() time.Time { return now },
	})
	view, err := mgr.GetOrCreate("sess-limit")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	attackerIP := "192.168.1.99"
	legitIP := "192.168.1.42"

	for i := 0; i < 4; i++ {
		if _, err := mgr.Redeem(attackerIP, "000000"); !errors.Is(err, ErrShortCodeInvalid) {
			t.Fatalf("attempt %d err = %v, want ErrShortCodeInvalid", i+1, err)
		}
	}
	if _, err := mgr.Redeem(attackerIP, "000000"); !errors.Is(err, ErrShortCodeRateLimited) {
		t.Fatalf("5th failure err = %v, want ErrShortCodeRateLimited", err)
	}
	// Locked out IP cannot even redeem valid code during lockout.
	if _, err := mgr.Redeem(attackerIP, view.Code); !errors.Is(err, ErrShortCodeRateLimited) {
		t.Fatalf("during lockout err = %v, want ErrShortCodeRateLimited", err)
	}

	// Legitimate client IP must NOT be locked out (no global DoS).
	red, err := mgr.Redeem(legitIP, view.Code)
	if err != nil || red.SessionKey != "sess-limit" {
		t.Fatalf("legit IP redeem failed during attacker lockout: %+v err=%v", red, err)
	}

	// Lockout clears after cooldown.
	now = now.Add(2*time.Minute + time.Second)
	reg, _ := mgr.Regenerate("sess-limit")
	if _, err := mgr.Redeem(attackerIP, reg.Code); err != nil {
		t.Fatalf("after lockout expiry redeem failed: %v", err)
	}
}
