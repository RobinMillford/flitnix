package session

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPinVerifierAndValidation(t *testing.T) {
	if _, err := NewPinVerifier("123"); err == nil {
		t.Fatal("expected error for too-short PIN")
	}
	vEmpty, err := NewPinVerifier("")
	if err != nil || vEmpty.Enabled() {
		t.Fatalf("empty PIN should disable verifier cleanly, got enabled=%v err=%v", vEmpty.Enabled(), err)
	}

	v1, err := NewPinVerifier("847291")
	if err != nil {
		t.Fatalf("NewPinVerifier: %v", err)
	}
	v2, err := NewPinVerifier("847291")
	if err != nil {
		t.Fatalf("NewPinVerifier v2: %v", err)
	}
	if !v1.Enabled() || !v1.Verify("847291") || !v1.Verify(" 847291 ") {
		t.Fatal("expected valid PIN to verify")
	}
	if v1.Verify("847290") || v1.Verify("") {
		t.Fatal("expected wrong PIN to fail")
	}
	// Distinct random salts must produce distinct hashes for the same PIN.
	if v1.salt == v2.salt || v1.digest == v2.digest {
		t.Fatal("expected unique salt and digest per verifier instance")
	}
}

func TestDownloadQuotaConcurrentOneTime(t *testing.T) {
	var q DownloadQuota
	q.Init(1)

	const contenders = 16
	var reservedCount atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if q.TryReserve(fmt.Sprintf("grant-%d", idx)) {
				reservedCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := reservedCount.Load(); got != 1 {
		t.Fatalf("concurrent one-time reservations = %d; want exactly 1", got)
	}

	// Interrupted/re-attempted continuation with the SAME reservation ID succeeds idempotently.
	// Find whichever reservation won by testing release/consume semantics cleanly.
	var winner string
	for i := 0; i < contenders; i++ {
		id := fmt.Sprintf("grant-%d", i)
		if q.TryReserve(id) {
			winner = id
			break
		}
	}
	if winner == "" {
		t.Fatal("could not re-acquire reservation for winning grant ID")
	}

	// If download failed cleanly and released, slot becomes available once more.
	q.Release(winner)
	if !q.TryReserve("retry-grant") {
		t.Fatal("expected slot to be reusable after failed transfer release")
	}
	completed, exhausted := q.Consume("retry-grant")
	if completed != 1 || !exhausted || !q.Exhausted() {
		t.Fatalf("after consume: completed=%d exhausted=%v", completed, exhausted)
	}
	if q.TryReserve("after-exhaust") {
		t.Fatal("expected reservation after exhaustion to fail")
	}
}

func TestSessionPolicyHardExpiryAndPINUnlock(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	mgr := NewManager(Config{
		IdleTimeout: 10 * time.Minute,
		Now:         clock,
	})
	defer mgr.Close()

	s, err := mgr.CreateWithPolicy(PolicyOptions{
		PinCode:      "2468",
		MaxDownloads: 2,
		ExpiresIn:    2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("CreateWithPolicy: %v", err)
	}
	if !s.PINRequired() || !s.VerifyPIN("2468") || s.VerifyPIN("0000") {
		t.Fatal("session PIN checks failed")
	}
	proof, err := s.IssuePINUnlock()
	if err != nil || !s.ValidatePINUnlock(proof) || s.ValidatePINUnlock("bogus-proof") {
		t.Fatal("session unlock proof validation failed")
	}

	// Pinning keeps idle alive, but MUST NOT override configured hard expiry!
	s.Begin()
	advance(3 * time.Minute)
	if st := s.Status(); st != StatusExpired {
		t.Fatalf("Status after hard expiry while pinned = %s; want expired", st)
	}
	s.End()
	if _, err := mgr.Validate(s.Token()); err != ErrExpired {
		t.Fatalf("Validate after hard expiry = %v; want ErrExpired", err)
	}
}
