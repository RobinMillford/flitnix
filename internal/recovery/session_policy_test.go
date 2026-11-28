package recovery

import (
	"testing"
	"time"
)

func TestSessionPolicyFieldsSurviveJournal(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hard := time.Now().UTC().Add(time.Hour).Truncate(time.Second)

	rec := sessionRecord("k-policy")
	rec.PinEnabled = true
	rec.PinSalt = "00112233445566778899aabbccddeeff"
	rec.PinDigest = "0123456789abcdef"
	rec.DownloadLimit = 2
	rec.DownloadsDone = 1
	rec.OneTimeAutoEnd = true
	rec.HardExpiresAt = &hard
	if err := store.Append(EventSessionCreated, rec); err != nil {
		t.Fatal(err)
	}

	consumed := rec
	consumed.DownloadsDone = 2
	if err := store.Append(EventSessionUpdated, consumed); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	sessions := reopened.State().Sessions
	if len(sessions) != 1 {
		t.Fatalf("session count = %d; want 1", len(sessions))
	}
	got := sessions[0]
	if !got.PinEnabled || got.PinSalt != rec.PinSalt || got.PinDigest != rec.PinDigest {
		t.Fatalf("PIN policy lost: %+v", got)
	}
	if got.DownloadLimit != 2 || got.DownloadsDone != 2 || !got.OneTimeAutoEnd {
		t.Fatalf("download policy lost: %+v", got)
	}
	if got.HardExpiresAt == nil || !got.HardExpiresAt.Equal(hard) {
		t.Fatalf("hard expiry lost: %v", got.HardExpiresAt)
	}
}
