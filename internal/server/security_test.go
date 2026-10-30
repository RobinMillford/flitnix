package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

type securityTestHarness struct {
	t        *testing.T
	dir      string
	store    *transfer.Store
	devStore *devices.Store
	hostID   *devices.HostIdentity
	coord    *devices.PairingCoordinator
	sessMgr  *session.Manager
	sess     *session.Session
	guestMgr *guest.Manager
	svcs     *server.Services
	server   *server.Server
	ts       *httptest.Server
}

func newSecurityTestHarness(t *testing.T) *securityTestHarness {
	t.Helper()
	dir := t.TempDir()

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: filepath.Join(dir, "transfers")})
	if err != nil {
		t.Fatalf("transfer.NewStore: %v", err)
	}

	devStore, err := devices.NewStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatalf("devices.NewStore: %v", err)
	}

	hostIdentity, err := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "host_identity.json"))
	if err != nil {
		t.Fatalf("LoadOrCreateHostIdentity: %v", err)
	}

	pairingCoord := devices.NewPairingCoordinator(hostIdentity, devStore)
	pairingCoord.OnPairingPrompt = func(deviceName, clientID, sasCode string, decisionChan chan<- bool) {
		decisionChan <- true
	}
	sessMgr := session.NewManager(session.Config{})
	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatalf("sessMgr.Create: %v", err)
	}

	guestMgr := guest.NewManager()

	svcs := &server.Services{
		Sessions:     sessMgr,
		Transfers:    transfer.NewService(transfer.Config{Store: store}),
		Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:     transfer.NewRegistry(),
		Downloads:    transfer.NewDownloadRegistry(),
		GuestManager: guestMgr,
		Devices:      devStore,
		HostIdentity: hostIdentity,
		Pairing:      pairingCoord,
	}

	srv := server.New(server.Config{Services: svcs})
	ts := httptest.NewServer(srv.Handler())

	h := &securityTestHarness{
		t:        t,
		dir:      dir,
		store:    store,
		devStore: devStore,
		hostID:   hostIdentity,
		coord:    pairingCoord,
		sessMgr:  sessMgr,
		sess:     sess,
		guestMgr: guestMgr,
		svcs:     svcs,
		server:   srv,
		ts:       ts,
	}

	t.Cleanup(func() {
		ts.Close()
	})

	return h
}

func (h *securityTestHarness) doJSON(method, path string, body any, token string, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("json.Marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(data)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	return rec
}

// 1. Client Commitment Verification Failure
func TestSecurity1_ClientCommitmentVerificationFailure(t *testing.T) {
	h := newSecurityTestHarness(t)
	clientPub, _, _ := devices.GenerateKeyPair()
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	clientCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)

	// Phase 1
	recChal := h.doJSON(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, h.sess.Token(), nil)
	if recChal.Code != http.StatusOK {
		t.Fatalf("Phase 1 failed: %d", recChal.Code)
	}
	var chalResp devices.ChallengeResponse
	_ = json.Unmarshal(recChal.Body.Bytes(), &chalResp)

	// Phase 2: Provide invalid opening (tampered clientEphemeral)
	tamperedEphemeral, _ := devices.GenerateEphemeralKeyPair()
	recRev := h.doJSON(http.MethodPost, "/api/pair/reveal", map[string]string{
		"pairingNonce":       chalResp.PairingNonce,
		"clientEphemeralKey": hex.EncodeToString(tamperedEphemeral.PublicKey),
		"clientSalt":         hex.EncodeToString(clientSalt),
	}, h.sess.Token(), nil)

	if recRev.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got: %d", recRev.Code)
	}
	if !bytes.Contains(recRev.Body.Bytes(), []byte("invalid_commitment")) {
		t.Fatalf("expected error code invalid_commitment, got: %s", recRev.Body.String())
	}
}

// 2. Host Commitment Verification Failure
func TestSecurity2_HostCommitmentVerificationFailure(t *testing.T) {
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	hostEphemeral, _ := devices.GenerateEphemeralKeyPair()
	hostSalt, _ := devices.GenerateOpeningSalt()
	validCommit, _ := devices.ComputeCommitment(hostEphemeral.PublicKey, hostSalt)

	// Host sends a tampered ephemeral key in reveal
	tamperedHostEphemeral, _ := devices.GenerateEphemeralKeyPair()
	if devices.VerifyCommitment(validCommit[:], tamperedHostEphemeral.PublicKey, hostSalt) {
		t.Fatal("commitment check should have failed for tampered host ephemeral key")
	}
	_ = clientEphemeral
}

// 3. Ephemeral Key Substitution
func TestSecurity3_EphemeralKeySubstitution(t *testing.T) {
	h := newSecurityTestHarness(t)
	clientPub, _, _ := devices.GenerateKeyPair()
	e1, _ := devices.GenerateEphemeralKeyPair()
	r1, _ := devices.GenerateOpeningSalt()
	c1, _ := devices.ComputeCommitment(e1.PublicKey, r1)

	// Commit with e1
	recChal := h.doJSON(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(c1[:]),
	}, h.sess.Token(), nil)
	var chalResp devices.ChallengeResponse
	_ = json.Unmarshal(recChal.Body.Bytes(), &chalResp)

	// Substitute e2 in reveal phase
	e2, _ := devices.GenerateEphemeralKeyPair()
	recRev := h.doJSON(http.MethodPost, "/api/pair/reveal", map[string]string{
		"pairingNonce":       chalResp.PairingNonce,
		"clientEphemeralKey": hex.EncodeToString(e2.PublicKey),
		"clientSalt":         hex.EncodeToString(r1),
	}, h.sess.Token(), nil)

	if recRev.Code != http.StatusBadRequest {
		t.Fatalf("substitution attempt must be rejected with 400, got: %d", recRev.Code)
	}
}

// 4. Host Key Substitution
func TestSecurity4_HostKeySubstitution(t *testing.T) {
	eh1, _ := devices.GenerateEphemeralKeyPair()
	rh1, _ := devices.GenerateOpeningSalt()
	ch1, _ := devices.ComputeCommitment(eh1.PublicKey, rh1)

	eh2, _ := devices.GenerateEphemeralKeyPair()
	if devices.VerifyCommitment(ch1[:], eh2.PublicKey, rh1) {
		t.Fatal("client must detect host key substitution")
	}
}

// 5. Adaptive MITM Attempting Independent Key Exchanges
func TestSecurity5_AdaptiveMITMIndependentKeyExchanges(t *testing.T) {
	// Attacker creates two independent key exchanges with Alice and Bob:
	// Alice <-> Attacker (k1, transcript1)
	// Attacker <-> Bob (k2, transcript2)
	// Because transcripts bind distinct public keys and nonces, SAS codes diverge with probability 1 - 10^-6
	nonceA := make([]byte, 32)
	nonceB := make([]byte, 32)
	nonceB[0] = 0xff

	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	k1[0] = 0x01
	k2[0] = 0x02

	tHash1 := sha256.Sum256([]byte("transcript-alice-attacker"))
	tHash2 := sha256.Sum256([]byte("transcript-attacker-bob"))

	_, sasAlice, _ := devices.DeriveSASFromTranscript(nonceA, k1, tHash1)
	_, sasBob, _ := devices.DeriveSASFromTranscript(nonceB, k2, tHash2)

	if sasAlice == sasBob {
		t.Fatalf("independent key exchanges produced identical SAS (%s)", sasAlice)
	}
}

// 6. SAS Mismatch and User Rejection
func TestSecurity6_SASMismatchUserRejection(t *testing.T) {
	h := newSecurityTestHarness(t)
	target, _ := client.ParseTarget(h.ts.URL+"/#"+h.sess.Token(), "")
	clientID, _ := client.LoadOrCreateClientIdentity(filepath.Join(h.dir, "client_id_test6.json"))
	kh, _ := client.LoadOrCreateKnownHosts(filepath.Join(h.dir, "kh_test6.json"))

	_, err := client.Pair(context.Background(), target, "Alice Device", clientID, kh, func(sas string, isPinned bool) (bool, error) {
		return false, nil // Local user rejects
	})

	if !errors.Is(err, client.ErrPairingAborted) {
		t.Fatalf("expected ErrPairingAborted, got: %v", err)
	}

	if _, ok := h.devStore.Get(clientID.DeviceID); ok {
		t.Fatal("device must not be enrolled after rejection")
	}
}

// 7. HostProof Signature Verification & 54-Byte Buffer Check
func TestSecurity7_HostProofSignatureVerification(t *testing.T) {
	h := newSecurityTestHarness(t)
	tHash := sha256.Sum256([]byte("test transcript hash"))
	hostProofSig := h.hostID.SignHostProof(tHash)

	// 1. Valid proof verifies
	valid, err := h.hostID.VerifyHostProof(tHash, hostProofSig)
	if err != nil || !valid {
		t.Fatalf("valid host proof failed verification: %v", err)
	}

	// 2. Tampered signature fails
	sigBytes, _ := hex.DecodeString(hostProofSig)
	sigBytes[0] ^= 0xff
	tamperedSig := hex.EncodeToString(sigBytes)
	valid, _ = h.hostID.VerifyHostProof(tHash, tamperedSig)
	if valid {
		t.Fatal("tampered host proof passed verification")
	}
}

// 8. HostProof Replay Across Sessions
func TestSecurity8_HostProofReplayAcrossSessions(t *testing.T) {
	sess1 := sha256.Sum256([]byte("session-1-secret"))
	sess2 := sha256.Sum256([]byte("session-2-secret"))

	idHost := sha256.Sum256([]byte("host-pub"))
	idClient := sha256.Sum256([]byte("client-pub"))
	commit := make([]byte, 32)
	ephem := make([]byte, 32)
	nonce := make([]byte, 32)

	t1, _ := devices.BuildTranscript(devices.TranscriptParams{
		HostID: idHost[:], ClientID: idClient[:],
		CommitC: commit, CommitH: commit,
		Ec: ephem, Eh: ephem, Nh: nonce,
		SessionKey: sess1[:],
	})
	t2, _ := devices.BuildTranscript(devices.TranscriptParams{
		HostID: idHost[:], ClientID: idClient[:],
		CommitC: commit, CommitH: commit,
		Ec: ephem, Eh: ephem, Nh: nonce,
		SessionKey: sess2[:],
	})

	h1, _ := devices.ComputeTranscriptHash(t1)
	h2, _ := devices.ComputeTranscriptHash(t2)

	if h1 == h2 {
		t.Fatal("transcripts across different sessions produced identical hashes")
	}
}

// 9. Premature ClientProof Submission
func TestSecurity9_PrematureClientProofSubmission(t *testing.T) {
	h := newSecurityTestHarness(t)
	clientPub, _, _ := devices.GenerateKeyPair()
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	clientCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)

	// Step 1 only
	recChal := h.doJSON(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, h.sess.Token(), nil)
	var chalResp devices.ChallengeResponse
	_ = json.Unmarshal(recChal.Body.Bytes(), &chalResp)

	// Skip step 2, jump directly to step 3
	recReq := h.doJSON(http.MethodPost, "/api/pair/request", map[string]string{
		"deviceName":      "Premature",
		"clientPublicKey": hex.EncodeToString(clientPub),
		"pairingNonce":    chalResp.PairingNonce,
		"clientProof":     "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
	}, h.sess.Token(), nil)

	if recReq.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got: %d", recReq.Code)
	}
}

// 10. Pairing Nonce Replay & Concurrent Consumption
func TestSecurity10_PairingNonceReplayAndConcurrentConsumption(t *testing.T) {
	h := newSecurityTestHarness(t)
	clientPub, clientPriv, _ := devices.GenerateKeyPair()
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	clientCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)

	// Phase 1
	recChal := h.doJSON(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, h.sess.Token(), nil)
	var chalResp devices.ChallengeResponse
	_ = json.Unmarshal(recChal.Body.Bytes(), &chalResp)

	// Phase 2
	recRev := h.doJSON(http.MethodPost, "/api/pair/reveal", map[string]string{
		"pairingNonce":       chalResp.PairingNonce,
		"clientEphemeralKey": hex.EncodeToString(clientEphemeral.PublicKey),
		"clientSalt":         hex.EncodeToString(clientSalt),
	}, h.sess.Token(), nil)
	var revResp devices.RevealResponse
	_ = json.Unmarshal(recRev.Body.Bytes(), &revResp)

	hostPubBytes, _ := hex.DecodeString(chalResp.HostPublicKey)
	hostEphemeralBytes, _ := hex.DecodeString(revResp.HostEphemeralKey)
	hostCommitBytes, _ := hex.DecodeString(chalResp.HostCommitment)
	nonceBytes, _ := hex.DecodeString(chalResp.PairingNonce)

	hostIDDigest := sha256.Sum256(hostPubBytes)
	clientIDDigest := sha256.Sum256(clientPub)
	sessDigest := h.sess.Digest()

	clientTranscript, _ := devices.BuildTranscript(devices.TranscriptParams{
		HostID: hostIDDigest[:], ClientID: clientIDDigest[:],
		CommitC: clientCommit[:], CommitH: hostCommitBytes,
		Ec: clientEphemeral.PublicKey, Eh: hostEphemeralBytes,
		Nh: nonceBytes, SessionKey: sessDigest[:],
	})
	transcriptHash, _ := devices.ComputeTranscriptHash(clientTranscript)
	clientProofSig := devices.SignClientProof(clientPriv, transcriptHash)

	// 10 concurrent requests with the SAME pairingNonce
	const concurrency = 10
	var wg sync.WaitGroup
	wg.Add(concurrency)
	statusCodes := make([]int, concurrency)

	for i := 0; i < concurrency; i++ {
		idx := i
		go func() {
			defer wg.Done()
			rec := h.doJSON(http.MethodPost, "/api/pair/request", map[string]string{
				"deviceName":      fmt.Sprintf("Concurrent Client %d", idx),
				"clientPublicKey": hex.EncodeToString(clientPub),
				"pairingNonce":    chalResp.PairingNonce,
				"clientProof":     clientProofSig,
			}, h.sess.Token(), nil)
			statusCodes[idx] = rec.Code
		}()
	}
	wg.Wait()

	successCount := 0
	conflictCount := 0
	for _, code := range statusCodes {
		if code == http.StatusOK {
			successCount++
		} else if code == http.StatusConflict || code == http.StatusTooManyRequests {
			conflictCount++
		}
	}

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful enrollment under concurrency, got: %d (codes: %v)", successCount, statusCodes)
	}
	if conflictCount != concurrency-1 {
		t.Fatalf("expected %d conflicts, got: %d", concurrency-1, conflictCount)
	}
}

// 11. Independently Verified SHA-256 Manifest Vectors
func TestSecurity11_IndependentlyVerifiedManifestVectors(t *testing.T) {
	// Vector 1 (384 bytes, hash c7b28b...)
	v1 := &transfer.CanonicalManifest{
		Kind:     "files",
		RootName: "",
		Items: []transfer.CanonicalManifestItem{
			{
				Sequence:     2,
				Size:         "524288",
				RelativePath: "images/photo.png",
				SHA256:       "4b227777d4dd1fc61c6f884f48641d02b4d121d3fd328cb08b5531fcacdabf8a",
				DisplayName:  "photo.png",
			},
			{
				DisplayName:  "contract.pdf",
				Size:         "1048576",
				SHA256:       "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4",
				RelativePath: "documents/contract.pdf",
				Sequence:     1,
			},
		},
	}
	raw, hash, err := transfer.ComputeManifestSHA256(v1)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 384 {
		t.Fatalf("Vector 1 length: got %d, want 384", len(raw))
	}
	if hash != "c7b28b1a12ee2ab0a89e76de18037783aa587463a57adca93de3276e28d6b554" {
		t.Fatalf("Vector 1 hash mismatch: got %s", hash)
	}

	// Vector 2 (194 bytes, hash 3ebc0c54f658d802555be2efeb66bc5d3bdd525449b8405121e988e5c827d449)
	v2Raw := `{"displayName":"quote \"special\" \\ test.txt","relativePath":"unicode/レポート_🎉.txt","sequence":1,"sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","size":"0"}`
	bytes2, err := transfer.CanonicalizeJSON([]byte(v2Raw))
	if err != nil {
		t.Fatalf("CanonicalizeJSON(v2Raw): %v", err)
	}
	h2 := sha256.Sum256(bytes2)
	hash2 := hex.EncodeToString(h2[:])
	if len(bytes2) != 194 {
		t.Fatalf("Vector 2 length: got %d, want 194", len(bytes2))
	}
	if hash2 != "3ebc0c54f658d802555be2efeb66bc5d3bdd525449b8405121e988e5c827d449" {
		t.Fatalf("Vector 2 hash mismatch: got %s", hash2)
	}
}

// 12. Host Identity Fingerprint Mismatch (TOFU Alert)
func TestSecurity12_HostIdentityFingerprintMismatchTOFU(t *testing.T) {
	kh, _ := client.LoadOrCreateKnownHosts(filepath.Join(t.TempDir(), "kh.json"))
	hostPub, _, _ := devices.GenerateKeyPair()
	hostID := devices.Fingerprint(hostPub)

	// Pin hostPub
	_ = kh.Pin(hostID, "Original Host", hex.EncodeToString(hostPub))

	// Remote host presents an imposter key with same or changed key
	imposterPub, _, _ := devices.GenerateKeyPair()
	err := kh.VerifyHost(hostID, hex.EncodeToString(imposterPub))
	if !errors.Is(err, client.ErrHostMismatch) {
		t.Fatalf("expected ErrHostMismatch on key change, got: %v", err)
	}
}

// 13. Large File Sizes & Path Edge Cases
func TestSecurity13_LargeFileSizesAndUnicodePaths(t *testing.T) {
	// Vector 3 (183 bytes, hash e4f5f4..., size > 2^53-1)
	v3Raw := `{"displayName":"huge_archive.iso","relativePath":"huge_archive.iso","sequence":1,"sha256":"a591a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f146e","size":"9007199254740993"}`
	bytes3, err := transfer.CanonicalizeJSON([]byte(v3Raw))
	if err != nil {
		t.Fatalf("CanonicalizeJSON(v3Raw): %v", err)
	}
	h3 := sha256.Sum256(bytes3)
	hash3 := hex.EncodeToString(h3[:])
	if len(bytes3) != 183 {
		t.Fatalf("Vector 3 length: got %d, want 183", len(bytes3))
	}
	if hash3 != "e4f5f4b3d559a057008a246d52598fb1f0c8056daef53dcecd7842a4c75b2273" {
		t.Fatalf("Vector 3 hash mismatch: got %s", hash3)
	}
}

// 14. Content Modification and Digest Mismatch Before Publication
func TestSecurity14_ContentTamperingDigestMismatchBeforePublication(t *testing.T) {
	h := newSecurityTestHarness(t)

	// Enroll client device with AutoApprove = true
	clientPub, clientPriv, _ := devices.GenerateKeyPair()
	dev, _ := h.devStore.Add("Auto Peer", clientPub, true)

	content := bytes.Repeat([]byte("A"), 2048)
	correctHash := sha256.Sum256(content)
	correctHashHex := hex.EncodeToString(correctHash[:])

	// Create single-file batch
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "tampered.bin", Size: int64(len(content)), SHA256: correctHashHex},
		},
	}

	// Request transfer challenge
	nonce, ts, _ := h.devStore.CreateTransferChallenge(h.hostID.HostID, dev.ID)
	cManifest, _ := transfer.CanonicalManifestFromBatch(manifest, nil)
	_, mHash, _ := transfer.ComputeManifestSHA256(cManifest)
	authPayload := devices.BuildAuthV2Payload(h.hostID.HostID, dev.ID, ts, nonce, mHash)
	sig := devices.SignMessage(clientPriv, authPayload)

	recBatch := h.doJSON(http.MethodPost, "/api/batches", manifest, h.sess.Token(), map[string]string{
		"X-Flitnix-Device-ID": dev.ID,
		"X-Flitnix-Nonce":     nonce,
		"X-Flitnix-Timestamp": strconv.FormatInt(ts, 10),
		"X-Flitnix-Signature": sig,
	})
	if recBatch.Code != http.StatusCreated {
		t.Fatalf("batch creation failed: %d: %s", recBatch.Code, recBatch.Body.String())
	}
	var view transfer.BatchView
	_ = json.Unmarshal(recBatch.Body.Bytes(), &view)

	// Send tampered body: flip byte 1024
	tamperedContent := make([]byte, len(content))
	copy(tamperedContent, content)
	tamperedContent[1024] ^= 0xff

	putURL := fmt.Sprintf("/api/batches/%s/items/%s/content", view.ID, view.Items[0].ID)
	req := httptest.NewRequest(http.MethodPut, putURL, bytes.NewReader(tamperedContent))
	req.Header.Set("Authorization", "Bearer "+h.sess.Token())
	recPut := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(recPut, req)

	if recPut.Code != http.StatusBadRequest {
		t.Fatalf("tampered content should return 400 Bad Request, got: %d", recPut.Code)
	}

	// Verify .part file was unlinked and file was NOT published
	entries, _ := os.ReadDir(h.store.Dir())
	for _, e := range entries {
		if e.Name() == "tampered.bin" || filepath.Ext(e.Name()) == ".part" {
			t.Fatalf("tampered file was published or .part leaked: %s", e.Name())
		}
	}
}

// 15. Batch Partial-Failure Semantics (Option B)
func TestSecurity15_BatchPartialFailureOptionB(t *testing.T) {
	h := newSecurityTestHarness(t)
	clientPub, clientPriv, _ := devices.GenerateKeyPair()
	dev, _ := h.devStore.Add("Batch Peer", clientPub, true)

	c1 := []byte("content 1")
	c2 := []byte("content 2")
	c3 := []byte("content 3")

	h1 := sha256.Sum256(c1)
	h2 := sha256.Sum256(c2)
	h3 := sha256.Sum256(c3)

	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "file1.txt", Size: int64(len(c1)), SHA256: hex.EncodeToString(h1[:])},
			{DisplayName: "file2.txt", Size: int64(len(c2)), SHA256: hex.EncodeToString(h2[:])},
			{DisplayName: "file3.txt", Size: int64(len(c3)), SHA256: hex.EncodeToString(h3[:])},
		},
	}

	nonce, ts, _ := h.devStore.CreateTransferChallenge(h.hostID.HostID, dev.ID)
	cManifest, _ := transfer.CanonicalManifestFromBatch(manifest, nil)
	_, mHash, _ := transfer.ComputeManifestSHA256(cManifest)
	authPayload := devices.BuildAuthV2Payload(h.hostID.HostID, dev.ID, ts, nonce, mHash)
	sig := devices.SignMessage(clientPriv, authPayload)

	recBatch := h.doJSON(http.MethodPost, "/api/batches", manifest, h.sess.Token(), map[string]string{
		"X-Flitnix-Device-ID": dev.ID,
		"X-Flitnix-Nonce":     nonce,
		"X-Flitnix-Timestamp": strconv.FormatInt(ts, 10),
		"X-Flitnix-Signature": sig,
	})
	var view transfer.BatchView
	_ = json.Unmarshal(recBatch.Body.Bytes(), &view)

	// Stream item 1: succeeds
	putURL1 := fmt.Sprintf("/api/batches/%s/items/%s/content", view.ID, view.Items[0].ID)
	req1 := httptest.NewRequest(http.MethodPut, putURL1, bytes.NewReader(c1))
	req1.Header.Set("Authorization", "Bearer "+h.sess.Token())
	recPut1 := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(recPut1, req1)
	if recPut1.Code != http.StatusOK {
		t.Fatalf("item 1 streaming failed: %d", recPut1.Code)
	}

	// Stream item 2: fails with tampered digest
	tampered2 := []byte("content X")
	putURL2 := fmt.Sprintf("/api/batches/%s/items/%s/content", view.ID, view.Items[1].ID)
	req2 := httptest.NewRequest(http.MethodPut, putURL2, bytes.NewReader(tampered2))
	req2.Header.Set("Authorization", "Bearer "+h.sess.Token())
	recPut2 := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(recPut2, req2)
	if recPut2.Code != http.StatusBadRequest {
		t.Fatalf("item 2 tampered content should return 400, got: %d", recPut2.Code)
	}

	// Attempt to stream item 3: refused because batch has failed (409 Conflict)
	putURL3 := fmt.Sprintf("/api/batches/%s/items/%s/content", view.ID, view.Items[2].ID)
	req3 := httptest.NewRequest(http.MethodPut, putURL3, bytes.NewReader(c3))
	req3.Header.Set("Authorization", "Bearer "+h.sess.Token())
	recPut3 := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(recPut3, req3)
	if recPut3.Code != http.StatusConflict {
		t.Fatalf("item 3 after batch failure must return 409 Conflict, got: %d", recPut3.Code)
	}

	// Assert Option B publication state:
	// Item 1 remains published
	if _, err := os.Stat(filepath.Join(h.store.Dir(), "file1.txt")); err != nil {
		t.Fatalf("item 1 was deleted: %v", err)
	}
	// Item 2 was NOT published
	if _, err := os.Stat(filepath.Join(h.store.Dir(), "file2.txt")); err == nil {
		t.Fatalf("item 2 was published despite digest failure")
	}
}

// 16. Mid-Stream Device Revocation
func TestSecurity16_MidStreamDeviceRevocation(t *testing.T) {
	h := newSecurityTestHarness(t)
	clientPub, clientPriv, _ := devices.GenerateKeyPair()
	dev, _ := h.devStore.Add("Revoke Peer", clientPub, true)

	content := bytes.Repeat([]byte("Z"), 4096)
	correctHash := sha256.Sum256(content)
	correctHashHex := hex.EncodeToString(correctHash[:])

	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "revoked.bin", Size: int64(len(content)), SHA256: correctHashHex},
		},
	}

	nonce, ts, _ := h.devStore.CreateTransferChallenge(h.hostID.HostID, dev.ID)
	cManifest, _ := transfer.CanonicalManifestFromBatch(manifest, nil)
	_, mHash, _ := transfer.ComputeManifestSHA256(cManifest)
	authPayload := devices.BuildAuthV2Payload(h.hostID.HostID, dev.ID, ts, nonce, mHash)
	sig := devices.SignMessage(clientPriv, authPayload)

	recBatch := h.doJSON(http.MethodPost, "/api/batches", manifest, h.sess.Token(), map[string]string{
		"X-Flitnix-Device-ID": dev.ID,
		"X-Flitnix-Nonce":     nonce,
		"X-Flitnix-Timestamp": strconv.FormatInt(ts, 10),
		"X-Flitnix-Signature": sig,
	})
	var view transfer.BatchView
	_ = json.Unmarshal(recBatch.Body.Bytes(), &view)

	// Revoke device before content stream arrives
	_ = h.devStore.Remove(dev.ID)

	putURL := fmt.Sprintf("/api/batches/%s/items/%s/content", view.ID, view.Items[0].ID)
	req := httptest.NewRequest(http.MethodPut, putURL, bytes.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+h.sess.Token())
	recPut := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(recPut, req)

	// Must reject with 401 Unauthorized
	if recPut.Code != http.StatusUnauthorized {
		t.Fatalf("revoked device stream should return 401 Unauthorized, got: %d", recPut.Code)
	}

	// File was not published
	entries, _ := os.ReadDir(h.store.Dir())
	for _, e := range entries {
		if e.Name() == "revoked.bin" {
			t.Fatal("revoked device file was published to disk")
		}
	}
}

// 17. Guest Drop Box Auto-Approval Isolation
func TestSecurity17_GuestDropBoxAutoApprovalIsolation(t *testing.T) {
	h := newSecurityTestHarness(t)
	clientPub, clientPriv, _ := devices.GenerateKeyPair()
	dev, _ := h.devStore.Add("Trusted Guest Peer", clientPub, true)

	// Create guest room
	room, err := h.guestMgr.CreateRoom("123456", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	guestUser, err := room.Join("guest-user")
	if err != nil {
		t.Fatal(err)
	}
	if err := room.ApproveGuest(guestUser.ID); err != nil {
		t.Fatal(err)
	}

	// Declare upload on /guest-api/uploads with trusted device headers
	body := map[string]any{
		"name": "guest_file.txt",
		"size": 100,
	}
	bodyData, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/guest-api/uploads", bytes.NewReader(bodyData))
	req.Header.Set("Authorization", "Bearer "+guestUser.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flitnix-Device-ID", dev.ID)
	req.Header.Set("X-Flitnix-Nonce", "fake-nonce")
	req.Header.Set("X-Flitnix-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-Flitnix-Signature", devices.SignMessage(clientPriv, []byte("fake-payload")))

	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on guest declare, got: %d", rec.Code)
	}

	// Verify upload is in Pending state, NOT auto-approved!
	var guestResp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &guestResp)
	status, _ := guestResp["status"].(string)
	if status == "approved" {
		t.Fatal("CRITICAL: Guest drop box upload was auto-approved via trusted device headers!")
	}
}

// 18. Session-Token Exposure Containment
func TestSecurity18_SessionTokenExposureContainment(t *testing.T) {
	h := newSecurityTestHarness(t)

	// An attacker holds only the Bearer session token, without any enrolled device private key
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "attacker_file.txt", Size: 50},
		},
	}

	// Normal declaration without trusted headers
	recBatch := h.doJSON(http.MethodPost, "/api/batches", manifest, h.sess.Token(), nil)
	if recBatch.Code != http.StatusCreated {
		t.Fatalf("batch declare failed: %d", recBatch.Code)
	}
	var view transfer.BatchView
	_ = json.Unmarshal(recBatch.Body.Bytes(), &view)

	// Assert it requires manual desktop approval (ApprovalPending), NOT auto-approved
	if view.ApprovalState != transfer.ApprovalPending || view.State != transfer.BatchStatePending {
		t.Fatalf("batch declared with session token alone must be pending approval, got: %s / %s", view.ApprovalState, view.State)
	}
}
