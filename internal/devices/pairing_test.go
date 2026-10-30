package devices

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestPairingCoordinatorFlow(t *testing.T) {
	hostID, err := LoadOrCreateHostIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore("")
	if err != nil {
		t.Fatal(err)
	}

	coordinator := NewPairingCoordinator(hostID, store)

	// Client Setup
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

	sessionKey := sha256.Sum256([]byte("test-session-secret"))

	// 1. Phase 1: POST /api/pair/challenge
	chal, err := coordinator.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(clientCommit[:]))
	if err != nil {
		t.Fatalf("InitChallenge failed: %v", err)
	}
	if chal.HostID != hostID.HostID {
		t.Errorf("got HostID %s, want %s", chal.HostID, hostID.HostID)
	}
	if chal.PairingNonce == "" {
		t.Errorf("empty pairing nonce")
	}

	// Test Single-Pending Invariant: concurrent request while challenge is active
	_, errConcurrent := coordinator.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(clientCommit[:]))
	if errConcurrent != ErrPairingInProgress {
		t.Errorf("expected ErrPairingInProgress, got: %v", errConcurrent)
	}
	if coordinator.FailureCount() != 0 {
		t.Errorf("concurrent rejection should NOT consume failure budget, got count = %d", coordinator.FailureCount())
	}

	// 2. Phase 2: POST /api/pair/reveal
	rev, err := coordinator.Reveal(
		sessionKey[:],
		chal.PairingNonce,
		hex.EncodeToString(clientEphemeral.PublicKey),
		hex.EncodeToString(clientSalt),
	)
	if err != nil {
		t.Fatalf("Reveal failed: %v", err)
	}
	if rev.HostEphemeralKey == "" || rev.HostSalt == "" || rev.HostProof == "" {
		t.Fatalf("Reveal response missing fields: %+v", rev)
	}

	// Client side verifies Host Commitment and computes shared secret and transcript
	hostEphemeralBytes, _ := hex.DecodeString(rev.HostEphemeralKey)
	hostSaltBytes, _ := hex.DecodeString(rev.HostSalt)
	hostCommitBytes, _ := hex.DecodeString(chal.HostCommitment)

	if !VerifyCommitment(hostCommitBytes, hostEphemeralBytes, hostSaltBytes) {
		t.Fatalf("client failed to verify host commitment")
	}

	kSharedClient, err := ComputeSharedSecret(clientEphemeral.PrivateKey, hostEphemeralBytes)
	if err != nil {
		t.Fatalf("client ComputeSharedSecret failed: %v", err)
	}

	hostPubBytes, _ := hex.DecodeString(chal.HostPublicKey)
	hostIDDigest := sha256.Sum256(hostPubBytes)
	clientIDDigest := sha256.Sum256(clientPub)
	nonceBytes, _ := hex.DecodeString(chal.PairingNonce)

	clientTranscript, err := BuildTranscript(TranscriptParams{
		HostID:     hostIDDigest[:],
		ClientID:   clientIDDigest[:],
		CommitC:    clientCommit[:],
		CommitH:    hostCommitBytes,
		Ec:         clientEphemeral.PublicKey,
		Eh:         hostEphemeralBytes,
		Nh:         nonceBytes,
		SessionKey: sessionKey[:],
	})
	if err != nil {
		t.Fatalf("client BuildTranscript failed: %v", err)
	}

	clientTranscriptHash, err := ComputeTranscriptHash(clientTranscript)
	if err != nil {
		t.Fatalf("client ComputeTranscriptHash failed: %v", err)
	}

	// Client verifies HostProof signature
	hostProofSigBytes, _ := hex.DecodeString(rev.HostProof)
	hostProofPayload := BuildHostProofPayload(clientTranscriptHash)
	if !ed25519.Verify(ed25519.PublicKey(hostPubBytes), hostProofPayload, hostProofSigBytes) {
		t.Fatalf("client failed to verify hostProof")
	}

	// Client derives SAS
	_, clientSASDisp, err := DeriveSASFromTranscript(nonceBytes, kSharedClient, clientTranscriptHash)
	if err != nil {
		t.Fatalf("client DeriveSAS failed: %v", err)
	}
	if clientSASDisp != rev.SASCode {
		t.Fatalf("SAS mismatch: client=%s, host=%s", clientSASDisp, rev.SASCode)
	}

	// 3. Phase 3: Client generates ClientProof and submits POST /api/pair/request
	clientProofSig := SignClientProof(clientPriv, clientTranscriptHash)
	enroll, err := coordinator.ConfirmAndEnroll(
		sessionKey[:],
		"Alice Phone",
		hex.EncodeToString(clientPub),
		chal.PairingNonce,
		clientProofSig,
		true, // Host user clicked "Allow & Pair"
	)
	if err != nil {
		t.Fatalf("ConfirmAndEnroll failed: %v", err)
	}
	if enroll.Status != "paired" || enroll.DeviceID != Fingerprint(clientPub) {
		t.Errorf("enroll response mismatch: %+v", enroll)
	}

	// Verify device exists in store
	dev, ok := store.Get(enroll.DeviceID)
	if !ok || dev.Name != "Alice Phone" {
		t.Errorf("device not found in store: %+v", dev)
	}
}

func TestPairingCoordinatorLockoutAndReArm(t *testing.T) {
	hostID, _ := LoadOrCreateHostIdentity("")
	store, _ := NewStore("")
	coordinator := NewPairingCoordinator(hostID, store)

	clientPub, _, _ := GenerateKeyPair()
	sessionKey := sha256.Sum256([]byte("session"))

	// Trigger 5 failed attempts
	for i := 0; i < 5; i++ {
		chal, err := coordinator.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(make([]byte, 32)))
		if err != nil {
			t.Fatalf("attempt %d InitChallenge failed: %v", i, err)
		}
		// Submit invalid reveal to trigger a failure
		_, errRev := coordinator.Reveal(sessionKey[:], chal.PairingNonce, hex.EncodeToString(make([]byte, 32)), hex.EncodeToString(make([]byte, 16)))
		if errRev == nil {
			t.Fatalf("attempt %d expected reveal failure", i)
		}
	}

	// 6th attempt should be locked out
	locked, dur := coordinator.IsLocked()
	if !locked || dur <= 0 {
		t.Fatalf("expected lockout, got locked=%v, dur=%v", locked, dur)
	}

	_, errLocked := coordinator.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(make([]byte, 32)))
	if errLocked != ErrPairingLocked {
		t.Fatalf("expected ErrPairingLocked, got: %v", errLocked)
	}

	// Re-arm pairing
	coordinator.ReArmPairing()

	lockedAfter, _ := coordinator.IsLocked()
	if lockedAfter {
		t.Errorf("expected not locked after ReArmPairing")
	}
	if coordinator.FailureCount() != 0 {
		t.Errorf("expected failure count 0 after ReArmPairing, got %d", coordinator.FailureCount())
	}
}

func TestPairingCoordinatorHostApprovalRequiredBeforePersistence(t *testing.T) {
	hostID, _ := LoadOrCreateHostIdentity("")
	store, _ := NewStore("")
	coordinator := NewPairingCoordinator(hostID, store)

	clientPub, clientPriv, _ := GenerateKeyPair()
	clientEphemeral, _ := GenerateEphemeralKeyPair()
	clientSalt, _ := GenerateOpeningSalt()
	clientCommit, _ := ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	sessionKey := sha256.Sum256([]byte("approval-session-secret"))

	chal, err := coordinator.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(clientCommit[:]))
	if err != nil {
		t.Fatal(err)
	}

	rev, err := coordinator.Reveal(sessionKey[:], chal.PairingNonce, hex.EncodeToString(clientEphemeral.PublicKey), hex.EncodeToString(clientSalt))
	if err != nil {
		t.Fatal(err)
	}

	hostPubBytes, _ := hex.DecodeString(chal.HostPublicKey)
	hostEphemeralBytes, _ := hex.DecodeString(rev.HostEphemeralKey)
	hostCommitBytes, _ := hex.DecodeString(chal.HostCommitment)
	nonceBytes, _ := hex.DecodeString(chal.PairingNonce)

	clientTranscript, _ := BuildTranscript(TranscriptParams{
		HostID:     sha256Hash(hostPubBytes),
		ClientID:   sha256Hash(clientPub),
		CommitC:    clientCommit[:],
		CommitH:    hostCommitBytes,
		Ec:         clientEphemeral.PublicKey,
		Eh:         hostEphemeralBytes,
		Nh:         nonceBytes,
		SessionKey: sessionKey[:],
	})
	transcriptHash, _ := ComputeTranscriptHash(clientTranscript)
	clientProofSig := SignClientProof(clientPriv, transcriptHash)

	// 1. Prepare approval: validates ClientProof and exposes to desktop
	reqID, _, _, err := coordinator.PrepareApproval(sessionKey[:], "Alice Tablet", hex.EncodeToString(clientPub), chal.PairingNonce, clientProofSig)
	if err != nil {
		t.Fatalf("PrepareApproval failed: %v", err)
	}
	if reqID == "" {
		t.Fatal("empty request ID returned")
	}

	// 2. Verify device is NOT yet persisted in store!
	devID := Fingerprint(clientPub)
	if _, ok := store.Get(devID); ok {
		t.Fatal("CRITICAL: device was persisted before host approval!")
	}

	// 3. Status must report RequiresApproval == true
	status := coordinator.GetPairingStatus()
	if !status.RequiresApproval || status.RequestID != reqID || status.DeviceName != "Alice Tablet" {
		t.Fatalf("unexpected pairing status before approval: %+v", status)
	}

	// 4. Stale request ID is rejected
	if err := coordinator.ApprovePairing("bogus-request-id"); err != ErrStaleRequest {
		t.Fatalf("expected ErrStaleRequest on bogus ID, got: %v", err)
	}

	// 5. Host operator approves valid request
	if err := coordinator.ApprovePairing(reqID); err != nil {
		t.Fatalf("ApprovePairing failed: %v", err)
	}

	// 6. Duplicate approval is rejected (single-use)
	if err := coordinator.ApprovePairing(reqID); err != ErrDecisionAlreadyMade {
		t.Fatalf("expected ErrDecisionAlreadyMade on second approval, got: %v", err)
	}

	// 7. Complete enrollment persists device with AutoApprove = false
	enroll, err := coordinator.CompleteEnrollment(reqID, true)
	if err != nil {
		t.Fatalf("CompleteEnrollment failed: %v", err)
	}
	if enroll.Status != "paired" || enroll.DeviceID != devID {
		t.Fatalf("unexpected enroll response: %+v", enroll)
	}

	dev, ok := store.Get(devID)
	if !ok {
		t.Fatal("device not found in store after complete enrollment")
	}
	if dev.AutoApprove {
		t.Fatal("CRITICAL: AutoApprove must default to false on enrollment")
	}
}

func TestPairingCoordinatorHostDenialPreventsPersistence(t *testing.T) {
	hostID, _ := LoadOrCreateHostIdentity("")
	store, _ := NewStore("")
	coordinator := NewPairingCoordinator(hostID, store)

	clientPub, clientPriv, _ := GenerateKeyPair()
	clientEphemeral, _ := GenerateEphemeralKeyPair()
	clientSalt, _ := GenerateOpeningSalt()
	clientCommit, _ := ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	sessionKey := sha256.Sum256([]byte("denial-session-secret"))

	chal, _ := coordinator.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(clientCommit[:]))
	rev, _ := coordinator.Reveal(sessionKey[:], chal.PairingNonce, hex.EncodeToString(clientEphemeral.PublicKey), hex.EncodeToString(clientSalt))

	hostPubBytes, _ := hex.DecodeString(chal.HostPublicKey)
	hostEphemeralBytes, _ := hex.DecodeString(rev.HostEphemeralKey)
	hostCommitBytes, _ := hex.DecodeString(chal.HostCommitment)
	nonceBytes, _ := hex.DecodeString(chal.PairingNonce)

	clientTranscript, _ := BuildTranscript(TranscriptParams{
		HostID:     sha256Hash(hostPubBytes),
		ClientID:   sha256Hash(clientPub),
		CommitC:    clientCommit[:],
		CommitH:    hostCommitBytes,
		Ec:         clientEphemeral.PublicKey,
		Eh:         hostEphemeralBytes,
		Nh:         nonceBytes,
		SessionKey: sessionKey[:],
	})
	transcriptHash, _ := ComputeTranscriptHash(clientTranscript)
	clientProofSig := SignClientProof(clientPriv, transcriptHash)

	reqID, _, _, err := coordinator.PrepareApproval(sessionKey[:], "Untrusted Phone", hex.EncodeToString(clientPub), chal.PairingNonce, clientProofSig)
	if err != nil {
		t.Fatal(err)
	}

	// Host operator denies pairing
	if err := coordinator.DenyPairing(reqID); err != nil {
		t.Fatalf("DenyPairing failed: %v", err)
	}

	// Double denial is rejected
	if err := coordinator.DenyPairing(reqID); err != ErrDecisionAlreadyMade {
		t.Fatalf("expected ErrDecisionAlreadyMade on second denial, got: %v", err)
	}

	// Complete enrollment returns ErrPairingDenied
	_, err = coordinator.CompleteEnrollment(reqID, false)
	if err != ErrPairingDenied {
		t.Fatalf("expected ErrPairingDenied, got: %v", err)
	}

	// Device must NEVER be persisted in store
	devID := Fingerprint(clientPub)
	if _, ok := store.Get(devID); ok {
		t.Fatal("CRITICAL: denied device was persisted in store!")
	}

	// Exactly 1 failure must be recorded
	if coordinator.FailureCount() != 1 {
		t.Fatalf("expected exactly 1 failure recorded, got: %d", coordinator.FailureCount())
	}
}

func TestPairingCoordinatorInvalidProofNeverCreatesApprovableRequest(t *testing.T) {
	hostID, _ := LoadOrCreateHostIdentity("")
	store, _ := NewStore("")
	coordinator := NewPairingCoordinator(hostID, store)

	clientPub, _, _ := GenerateKeyPair()
	clientEphemeral, _ := GenerateEphemeralKeyPair()
	clientSalt, _ := GenerateOpeningSalt()
	clientCommit, _ := ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	sessionKey := sha256.Sum256([]byte("invalid-proof-session"))

	chal, _ := coordinator.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(clientCommit[:]))
	_, _ = coordinator.Reveal(sessionKey[:], chal.PairingNonce, hex.EncodeToString(clientEphemeral.PublicKey), hex.EncodeToString(clientSalt))

	// Invalid forged client proof signature
	bogusSig := hex.EncodeToString(make([]byte, 64))

	_, _, _, err := coordinator.PrepareApproval(sessionKey[:], "Attacker Phone", hex.EncodeToString(clientPub), chal.PairingNonce, bogusSig)
	if err != ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got: %v", err)
	}

	// Status must NEVER show requires approval
	status := coordinator.GetPairingStatus()
	if status.RequiresApproval || status.RequestID != "" {
		t.Fatalf("invalid proof created approvable request: %+v", status)
	}
}

func sha256Hash(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
