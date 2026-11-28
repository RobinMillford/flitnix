package session

import (
	"testing"
	"time"
)

func TestSessionPolicySurvivesRestore(t *testing.T) {
	now := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	m1 := NewManager(Config{Now: clock})
	var updates []SessionRecord
	m1.SetUpdateHook(func(rec SessionRecord) error {
		updates = append(updates, rec)
		return nil
	})

	s, err := m1.CreateWithPolicy(PolicyOptions{
		PinCode:        "1234",
		MaxDownloads:   2,
		ExpiresIn:      time.Hour,
		OneTimeAutoEnd: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := m1.recordFor(s, StatusActive)
	if !rec.PIN.Enabled {
		t.Fatal("PIN policy missing from durable record")
	}
	if rec.DownloadLimit.Limit != 2 {
		t.Fatalf("download limit = %d; want 2", rec.DownloadLimit.Limit)
	}
	if !rec.OneTimeAutoEnd {
		t.Fatal("one-time auto-end missing from durable record")
	}
	if want := now.Add(time.Hour); !rec.HardExpiresAt.Equal(want) {
		t.Fatalf("hard expiry = %v; want %v", rec.HardExpiresAt, want)
	}

	exhausted, autoEnd := s.ConsumeDownload("grant-1")
	if exhausted || autoEnd {
		t.Fatalf("first consume: exhausted=%v autoEnd=%v", exhausted, autoEnd)
	}
	if len(updates) != 1 {
		t.Fatalf("update hook called %d times; want 1", len(updates))
	}
	rec = updates[0]
	if rec.DownloadLimit.Completed != 1 {
		t.Fatalf("persisted completed = %d; want 1", rec.DownloadLimit.Completed)
	}

	m2 := NewManager(Config{Now: clock})
	if err := m2.Restore(rec); err != nil {
		t.Fatal(err)
	}
	restored, err := m2.Validate(s.token)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.pinVerifier.Verify("1234") || restored.pinVerifier.Verify("9999") {
		t.Fatal("PIN verifier did not survive restore")
	}
	if limit, completed, remaining := restored.DownloadQuotaStats(); limit != 2 || completed != 1 || remaining != 1 {
		t.Fatalf("quota after restore = (%d,%d,%d); want (2,1,1)", limit, completed, remaining)
	}
	if want := now.Add(time.Hour); !restored.HardExpiresAt().Equal(want) {
		t.Fatalf("restored hard expiry = %v; want %v", restored.HardExpiresAt(), want)
	}
	exhausted, autoEnd = restored.ConsumeDownload("grant-2")
	if !exhausted || !autoEnd {
		t.Fatalf("after restore second consume: exhausted=%v autoEnd=%v; want true,true", exhausted, autoEnd)
	}
}
