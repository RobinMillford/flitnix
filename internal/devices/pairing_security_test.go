package devices

// Tests for durable pairing lockout persistence (F-05, pairing_security.go).
//
// The policy under test is unchanged: 5 failed attempts inside a 10-minute
// window trigger a 15-minute lockout. What is new is durability — the failure
// counter and an active lockout must survive a restart, an expired lockout
// must re-enable pairing, and every corruption or write failure must fail
// closed (pairing refused) rather than silently disabling the lockout.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// sha256Sum is a small convenience so test bodies can derive session-key
// hashes inline.
func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// newDurableIdentity loads (or creates) a file-backed host identity so two
// coordinators constructed from the same directory share the same HMAC key —
// the test's stand-in for a process restart.
func newDurableIdentity(t *testing.T, dir string) *HostIdentity {
	t.Helper()
	hostID, err := LoadOrCreateHostIdentity(filepath.Join(dir, "host_identity.json"))
	if err != nil {
		t.Fatalf("LoadOrCreateHostIdentity: %v", err)
	}
	return hostID
}

// newDurableCoordinator builds a coordinator wired to secPath. Use for happy
// paths; corrupt-record tests construct the coordinator manually so they can
// assert on the EnableDurableLockout error itself.
func newDurableCoordinator(t *testing.T, dir string) (*PairingCoordinator, string) {
	t.Helper()
	hostID := newDurableIdentity(t, dir)
	coord := NewPairingCoordinator(hostID, mustDeviceStore(t))
	secPath := filepath.Join(dir, "pairing_security.json")
	if err := coord.EnableDurableLockout(secPath); err != nil {
		t.Fatalf("EnableDurableLockout: %v", err)
	}
	return coord, secPath
}

func mustDeviceStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// failOnce drives one real failure through the public handshake surface:
// a valid challenge followed by a reveal whose commitment does not verify.
func failOnce(t *testing.T, c *PairingCoordinator, sessionKey []byte, clientPub string) {
	t.Helper()
	chal, err := c.InitChallenge(sessionKey, clientPub, hex.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("InitChallenge: %v", err)
	}
	if _, err := c.Reveal(sessionKey, chal.PairingNonce, hex.EncodeToString(make([]byte, 32)), hex.EncodeToString(make([]byte, 16))); err == nil {
		t.Fatal("expected Reveal to fail and record an attempt")
	}
}

// successfulPairing drives a full valid handshake to enrollment, mirroring
// TestPairingCoordinatorFlow.
func successfulPairing(t *testing.T, c *PairingCoordinator, sessionKey []byte, deviceName string) {
	t.Helper()
	clientPub, clientPriv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientEphemeral, err := GenerateEphemeralKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientSalt, err := GenerateOpeningSalt()
	if err != nil {
		t.Fatal(err)
	}
	clientCommit, err := ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	if err != nil {
		t.Fatal(err)
	}

	chal, err := c.InitChallenge(sessionKey, hex.EncodeToString(clientPub), hex.EncodeToString(clientCommit[:]))
	if err != nil {
		t.Fatalf("InitChallenge: %v", err)
	}
	rev, err := c.Reveal(sessionKey, chal.PairingNonce, hex.EncodeToString(clientEphemeral.PublicKey), hex.EncodeToString(clientSalt))
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}

	hostEphemeralBytes, _ := hex.DecodeString(rev.HostEphemeralKey)
	hostSaltBytes, _ := hex.DecodeString(rev.HostSalt)
	hostCommitBytes, _ := hex.DecodeString(chal.HostCommitment)
	if !VerifyCommitment(hostCommitBytes, hostEphemeralBytes, hostSaltBytes) {
		t.Fatal("client failed to verify host commitment")
	}
	hostPubBytes, _ := hex.DecodeString(chal.HostPublicKey)
	hostIDDigest := sha256Sum(hostPubBytes)
	clientIDDigest := sha256Sum(clientPub)
	nonceBytes, _ := hex.DecodeString(chal.PairingNonce)
	transcript, err := BuildTranscript(TranscriptParams{
		HostID:     hostIDDigest[:],
		ClientID:   clientIDDigest[:],
		CommitC:    clientCommit[:],
		CommitH:    hostCommitBytes,
		Ec:         clientEphemeral.PublicKey,
		Eh:         hostEphemeralBytes,
		Nh:         nonceBytes,
		SessionKey: sessionKey,
	})
	if err != nil {
		t.Fatalf("BuildTranscript: %v", err)
	}
	transcriptHash, err := ComputeTranscriptHash(transcript)
	if err != nil {
		t.Fatalf("ComputeTranscriptHash: %v", err)
	}
	clientProof := SignClientProof(clientPriv, transcriptHash)

	enroll, err := c.ConfirmAndEnroll(sessionKey, deviceName, hex.EncodeToString(clientPub), chal.PairingNonce, clientProof, true)
	if err != nil {
		t.Fatalf("ConfirmAndEnroll: %v", err)
	}
	if enroll.Status != "paired" {
		t.Fatalf("unexpected enroll status: %+v", enroll)
	}
}

// loadRecord reads and authenticates the durable record the way a restart
// would, for forensic assertions.
func loadRecord(t *testing.T, hostID *HostIdentity, secPath string) *PairingSecurityRecord {
	t.Helper()
	sec := &pairingSecurityStore{path: secPath, macKey: hostID.pairingMACKey()}
	rec, err := sec.load()
	if err != nil {
		t.Fatalf("load security record: %v", err)
	}
	return rec
}

// 1. The failure counter survives a restart.
func TestPairingSecurity_CounterSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	coord1, _ := newDurableCoordinator(t, dir)
	sessionKey := sha256Sum([]byte("session"))
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(clientPub)

	for i := 0; i < 3; i++ {
		failOnce(t, coord1, sessionKey[:], pubHex)
	}
	if got := coord1.FailureCount(); got != 3 {
		t.Fatalf("pre-restart failure count = %d, want 3", got)
	}

	coord2, _ := newDurableCoordinator(t, dir) // simulated restart
	if got := coord2.FailureCount(); got != 3 {
		t.Fatalf("post-restart failure count = %d, want 3 (counter must survive restart)", got)
	}
	if locked, _ := coord2.IsLocked(); locked {
		t.Fatal("three failures must not lock pairing")
	}
}

// 2. An active lockout survives a restart and keeps its ORIGINAL expiry
// instead of being extended or reset.
func TestPairingSecurity_ActiveLockoutSurvivesRestartWithOriginalExpiry(t *testing.T) {
	dir := t.TempDir()
	coord1, _ := newDurableCoordinator(t, dir)
	sessionKey := sha256Sum([]byte("session"))
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(clientPub)

	for i := 0; i < MaxFailedAttempts; i++ {
		failOnce(t, coord1, sessionKey[:], pubHex)
	}
	coord1.mu.Lock()
	originalExpiry := coord1.lockoutUntil
	coord1.mu.Unlock()
	if originalExpiry.IsZero() {
		t.Fatal("expected active lockout after 5 failures")
	}

	coord2, _ := newDurableCoordinator(t, dir) // simulated restart
	coord2.mu.Lock()
	restoredExpiry := coord2.lockoutUntil
	coord2.mu.Unlock()
	if !restoredExpiry.Equal(originalExpiry) {
		t.Fatalf("restored lockout expiry = %v, want original %v", restoredExpiry, originalExpiry)
	}
	if locked, dur := coord2.IsLocked(); !locked || dur <= 0 {
		t.Fatalf("expected active lockout after restart, got locked=%v dur=%v", locked, dur)
	}
}

// 3. Exactly the fifth failure triggers the lockout — no sooner, no later.
func TestPairingSecurity_FifthFailureTriggersLockout(t *testing.T) {
	dir := t.TempDir()
	coord, _ := newDurableCoordinator(t, dir)
	sessionKey := sha256Sum([]byte("session"))
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(clientPub)

	for i := 0; i < MaxFailedAttempts-1; i++ {
		failOnce(t, coord, sessionKey[:], pubHex)
		if locked, _ := coord.IsLocked(); locked {
			t.Fatalf("lockout engaged after only %d failures", i+1)
		}
	}
	failOnce(t, coord, sessionKey[:], pubHex)
	if locked, dur := coord.IsLocked(); !locked || dur <= 0 {
		t.Fatalf("expected lockout after %d failures, got locked=%v dur=%v", MaxFailedAttempts, locked, dur)
	}
}

// 4. An expired lockout with window-expired failure timestamps restores to a
// clean, unlocked state: pairing is re-eligible without a re-arm.
func TestPairingSecurity_ExpiredLockoutReenablesPairing(t *testing.T) {
	dir := t.TempDir()
	hostID := newDurableIdentity(t, dir)
	secPath := filepath.Join(dir, "pairing_security.json")

	// Persist a stale record: all failures and the lockout are in the past.
	coord1 := NewPairingCoordinator(hostID, mustDeviceStore(t))
	if err := coord1.EnableDurableLockout(secPath); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stale := now.Add(-2 * FailureWindow)
	coord1.mu.Lock()
	for i := 0; i < MaxFailedAttempts; i++ {
		coord1.failureTimestamps = append(coord1.failureTimestamps, stale)
	}
	coord1.lockoutUntil = now.Add(-time.Minute)
	coord1.persistSecurityLocked(now)
	coord1.mu.Unlock()

	coord2 := NewPairingCoordinator(hostID, mustDeviceStore(t))
	if err := coord2.EnableDurableLockout(secPath); err != nil {
		t.Fatal(err)
	}
	if locked, _ := coord2.IsLocked(); locked {
		t.Fatal("expired lockout must not lock pairing after restart")
	}
	if got := coord2.FailureCount(); got != 0 {
		t.Fatalf("window-expired failures must be pruned on restore, got %d", got)
	}
	// Pairing is genuinely re-eligible: a fresh challenge goes through.
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord2.InitChallenge(sha256Sum([]byte("s"))[:], hex.EncodeToString(clientPub), hex.EncodeToString(make([]byte, 32))); err != nil {
		t.Fatalf("pairing must be re-eligible after the lockout expired: %v", err)
	}
}

// 5. A successful pairing follows the existing reset policy: it does NOT
// clear the failure window (only ReArmPairing does).
func TestPairingSecurity_SuccessfulPairingKeepsExistingResetPolicy(t *testing.T) {
	dir := t.TempDir()
	coord, secPath := newDurableCoordinator(t, dir)
	sessionKey := sha256Sum([]byte("session"))
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(clientPub)

	failOnce(t, coord, sessionKey[:], pubHex)
	failOnce(t, coord, sessionKey[:], pubHex)
	if got := coord.FailureCount(); got != 2 {
		t.Fatalf("failure count = %d, want 2", got)
	}

	successfulPairing(t, coord, sessionKey[:], "Alice Phone")

	if got := coord.FailureCount(); got != 2 {
		t.Fatalf("failure count after successful pairing = %d, want 2 (success must not reset the window)", got)
	}
	rec := loadRecord(t, coord.identity, secPath)
	if len(rec.FailureTimestamps) != 2 {
		t.Fatalf("persisted failure timestamps = %d, want 2", len(rec.FailureTimestamps))
	}
	if !rec.LockoutUntil.IsZero() {
		t.Fatalf("persisted lockout = %v, want zero", rec.LockoutUntil)
	}
}

// 6. Concurrent failures through the real handshake paths cannot bypass the
// threshold: once MaxFailedAttempts failures land inside the window the
// lockout engages, and the durable record matches.
func TestPairingSecurity_ConcurrentFailuresCannotBypassThreshold(t *testing.T) {
	dir := t.TempDir()
	coord1, secPath := newDurableCoordinator(t, dir)
	sessionKey := sha256Sum([]byte("session"))
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(clientPub)

	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				chal, err := coord1.InitChallenge(sessionKey[:], pubHex, hex.EncodeToString(make([]byte, 32)))
				if errors.Is(err, ErrPairingInProgress) {
					time.Sleep(time.Millisecond) // single-pending invariant: retry
					continue
				}
				if errors.Is(err, ErrPairingLocked) {
					return // locked by peers: nothing left to probe
				}
				if err != nil {
					return
				}
				_, _ = coord1.Reveal(sessionKey[:], chal.PairingNonce, hex.EncodeToString(make([]byte, 32)), hex.EncodeToString(make([]byte, 16)))
				return // this worker's one real failure attempt is spent
			}
		}()
	}
	wg.Wait()

	count := coord1.FailureCount()
	if count < MaxFailedAttempts || count > workers {
		t.Fatalf("failure count = %d, want between %d and %d", count, MaxFailedAttempts, workers)
	}
	if locked, _ := coord1.IsLocked(); !locked {
		t.Fatal("threshold reached but pairing is not locked: concurrency bypassed the lockout")
	}

	coord2, _ := newDurableCoordinator(t, dir) // restart
	if locked, _ := coord2.IsLocked(); !locked {
		t.Fatal("lockout from concurrent failures did not survive restart")
	}
	rec := loadRecord(t, coord1.identity, secPath)
	if len(rec.FailureTimestamps) != count {
		t.Fatalf("persisted failures = %d, want %d", len(rec.FailureTimestamps), count)
	}
}

// 7. A failed durable write fails closed: the in-memory enforcement stays,
// pairing is refused, nothing reports the lockout as persisted, and a re-arm
// cannot lift the degraded state while the disk keeps failing.
func TestPairingSecurity_WriteFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	hostID := newDurableIdentity(t, dir)
	secPath := filepath.Join(dir, "pairing_security.json")

	coord := NewPairingCoordinator(hostID, mustDeviceStore(t))
	if err := coord.EnableDurableLockout(secPath); err != nil {
		t.Fatalf("EnableDurableLockout on a not-yet-existing record must start clean: %v", err)
	}

	// Make every subsequent save fail deterministically: the record path becomes
	// a non-empty directory, so the temp file's atomic rename can never install
	// it. A chmod fixture cannot deny writes on Windows (mode bits are ignored
	// there), which would leave the save succeeding and the test asserting
	// nothing.
	if err := os.MkdirAll(secPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secPath, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	sessionKey := sha256Sum([]byte("session"))
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(clientPub)

	failOnce(t, coord, sessionKey[:], pubHex)
	if got := coord.FailureCount(); got != 1 {
		t.Fatalf("in-memory enforcement lost: count = %d, want 1", got)
	}

	// The failed save must degrade the coordinator: pairing is refused even
	// though no lockout is active in memory.
	if _, err := coord.InitChallenge(sessionKey[:], pubHex, hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrPairingLocked) {
		t.Fatalf("after failed persistence want ErrPairingLocked, got: %v", err)
	}

	// Re-arm cannot lift the degraded state while writes keep failing.
	coord.ReArmPairing()
	if _, err := coord.InitChallenge(sessionKey[:], pubHex, hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrPairingLocked) {
		t.Fatalf("re-arm must not lift degraded state while saves fail, got: %v", err)
	}
}

// 8. A corrupt record (garbage bytes, or valid JSON with a bad MAC) fails
// closed: EnableDurableLockout reports the problem and pairing is refused
// until a successful re-arm rewrites the record.
func TestPairingSecurity_CorruptRecordFailsClosed(t *testing.T) {
	dir := t.TempDir()
	hostID := newDurableIdentity(t, dir)
	secPath := filepath.Join(dir, "pairing_security.json")
	sessionKey := sha256Sum([]byte("session"))

	// a) garbage bytes
	if err := os.WriteFile(secPath, []byte("\x00{not json at all}"), 0o600); err != nil {
		t.Fatal(err)
	}
	coord := NewPairingCoordinator(hostID, mustDeviceStore(t))
	if err := coord.EnableDurableLockout(secPath); err == nil {
		t.Fatal("expected EnableDurableLockout to reject a garbage record")
	}
	if _, err := coord.InitChallenge(sessionKey[:], hex.EncodeToString(make([]byte, 0)), hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrPairingLocked) {
		t.Fatalf("garbage record must fail closed, got: %v", err)
	}

	// b) valid JSON, wrong MAC (tampered record)
	tampered := PairingSecurityRecord{
		SchemaVersion:     pairingSecuritySchema,
		FailureTimestamps: []time.Time{time.Now().UTC()},
		UpdatedAt:         time.Now().UTC(),
		MAC:               hex.EncodeToString(make([]byte, 32)),
	}
	blob, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	coord2 := NewPairingCoordinator(hostID, mustDeviceStore(t))
	if err := coord2.EnableDurableLockout(secPath); err == nil {
		t.Fatal("expected EnableDurableLockout to reject a tampered record")
	}
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord2.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrPairingLocked) {
		t.Fatalf("tampered record must fail closed, got: %v", err)
	}

	// c) a successful re-arm rewrites the record and lifts the degraded state.
	coord2.ReArmPairing()
	chal, err := coord2.InitChallenge(sessionKey[:], hex.EncodeToString(make([]byte, 32)), hex.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("pairing must work again after re-arm rewrote the record: %v", err)
	}
	if chal == nil || chal.PairingNonce == "" {
		t.Fatal("expected a fresh challenge after re-arm")
	}
}

// 9. A structurally out-of-bounds record (more failure timestamps than the
// record bound) is treated as corrupt and fails closed.
func TestPairingSecurity_OutOfBoundsRecordFailsClosed(t *testing.T) {
	dir := t.TempDir()
	hostID := newDurableIdentity(t, dir)
	secPath := filepath.Join(dir, "pairing_security.json")

	// Craft and seal a record that exceeds maxPersistedFailures. The save path
	// does not validate (it trusts the coordinator), so this is exactly what a
	// future bug or hand-tampered file would look like.
	sec := &pairingSecurityStore{path: secPath, macKey: hostID.pairingMACKey()}
	now := time.Now().UTC()
	bloated := PairingSecurityRecord{SchemaVersion: pairingSecuritySchema, UpdatedAt: now}
	for i := 0; i <= maxPersistedFailures; i++ {
		bloated.FailureTimestamps = append(bloated.FailureTimestamps, now.Add(-time.Duration(i)*time.Minute))
	}
	if err := sec.save(bloated); err != nil {
		t.Fatalf("crafting out-of-bounds record: %v", err)
	}

	coord := NewPairingCoordinator(hostID, mustDeviceStore(t))
	if err := coord.EnableDurableLockout(secPath); err == nil {
		t.Fatal("expected EnableDurableLockout to reject an out-of-bounds record")
	}
	if _, err := coord.InitChallenge(sha256Sum([]byte("s"))[:], hex.EncodeToString(make([]byte, 32)), hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrPairingLocked) {
		t.Fatalf("out-of-bounds record must fail closed, got: %v", err)
	}
}

// 9b. A replaced host identity derives a different MAC key, so a record
// sealed under the old identity fails authentication and the coordinator
// fails closed instead of trusting foreign state.
func TestPairingSecurity_IdentityRotationFailsClosed(t *testing.T) {
	dir1 := t.TempDir()
	coord1, _ := newDurableCoordinator(t, dir1)
	sessionKey := sha256Sum([]byte("session"))
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(clientPub)
	for i := 0; i < MaxFailedAttempts; i++ {
		failOnce(t, coord1, sessionKey[:], pubHex)
	}
	rec := loadRecord(t, coord1.identity, filepath.Join(dir1, "pairing_security.json"))
	if rec.LockoutUntil.IsZero() {
		t.Fatal("expected persisted lockout")
	}

	// A "new machine" (fresh identity) pointing at the same record file must
	// not honour a lockout it cannot authenticate — it must refuse pairing
	// until its own re-arm.
	dir2 := t.TempDir()
	hostID2 := newDurableIdentity(t, dir2)
	secPath2 := filepath.Join(dir2, "pairing_security.json")
	if err := copyFile(filepath.Join(dir1, "pairing_security.json"), secPath2); err != nil {
		t.Fatal(err)
	}
	coord2 := NewPairingCoordinator(hostID2, mustDeviceStore(t))
	if err := coord2.EnableDurableLockout(secPath2); err == nil {
		t.Fatal("expected EnableDurableLockout to reject a record sealed by another identity")
	}
	if _, err := coord2.InitChallenge(sessionKey[:], pubHex, hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrPairingLocked) {
		t.Fatalf("foreign-identity record must fail closed, got: %v", err)
	}
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o600)
}

// 10. A normal shutdown at any moment preserves an active lockout: because
// every state change is written through immediately, the on-disk record is
// always current, and a fresh process refuses pairing at the entry point.
func TestPairingSecurity_NormalShutdownPreservesActiveLockout(t *testing.T) {
	dir := t.TempDir()
	coord1, secPath := newDurableCoordinator(t, dir)
	sessionKey := sha256Sum([]byte("session"))
	clientPub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(clientPub)

	for i := 0; i < MaxFailedAttempts; i++ {
		failOnce(t, coord1, sessionKey[:], pubHex)
	}

	// Forensics: the record on disk is authenticated and carries the lockout.
	rec := loadRecord(t, coord1.identity, secPath)
	if rec.LockoutUntil.IsZero() {
		t.Fatal("durable record does not carry the active lockout")
	}

	// Simulated restart: a fresh process loads the same file and refuses
	// pairing at the public entry point without any explicit flush.
	coord2 := NewPairingCoordinator(coord1.identity, mustDeviceStore(t))
	if err := coord2.EnableDurableLockout(secPath); err != nil {
		t.Fatalf("restart over a healthy record must not fail: %v", err)
	}
	if _, err := coord2.InitChallenge(sessionKey[:], pubHex, hex.EncodeToString(make([]byte, 32))); !errors.Is(err, ErrPairingLocked) {
		t.Fatalf("restart must refuse pairing while the lockout is active, got: %v", err)
	}
}
