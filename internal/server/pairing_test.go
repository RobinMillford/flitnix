package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func sha256Hash(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

type pairingTestEnv struct {
	t        *testing.T
	sess     *session.Manager
	token    string
	devices  *devices.Store
	hostID   *devices.HostIdentity
	coord    *devices.PairingCoordinator
	server   *Server
	services *Services
}

func newPairingTestEnv(t *testing.T) *pairingTestEnv {
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

	svcs := &Services{
		Sessions:     sessMgr,
		Transfers:    transfer.NewService(transfer.Config{Store: store}),
		Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:     transfer.NewRegistry(),
		Downloads:    transfer.NewDownloadRegistry(),
		Devices:      devStore,
		HostIdentity: hostIdentity,
		Pairing:      pairingCoord,
	}

	srv := New(Config{Services: svcs})

	return &pairingTestEnv{
		t:        t,
		sess:     sessMgr,
		token:    sess.Token(),
		devices:  devStore,
		hostID:   hostIdentity,
		coord:    pairingCoord,
		server:   srv,
		services: svcs,
	}
}

func (e *pairingTestEnv) do(method, path string, body any, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("json.Marshal body: %v", err)
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
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestServerPairingFullHandshakeAndDeviceManagement(t *testing.T) {
	env := newPairingTestEnv(t)

	// Client generates identity key and ephemeral keys
	clientPub, clientPriv, err := devices.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientEphemeral, err := devices.GenerateEphemeralKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	clientSalt, err := devices.GenerateOpeningSalt()
	if err != nil {
		t.Fatal(err)
	}
	clientCommit, err := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	if err != nil {
		t.Fatal(err)
	}

	// 0. Verify unauthenticated request fails with 401
	rec401 := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, "")
	if rec401.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without token, got %d", rec401.Code)
	}

	// 1. Phase 1: POST /api/pair/challenge
	recChal := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, env.token)
	if recChal.Code != http.StatusOK {
		t.Fatalf("POST /api/pair/challenge failed (%d): %s", recChal.Code, recChal.Body.String())
	}

	var chalResp devices.ChallengeResponse
	if err := json.Unmarshal(recChal.Body.Bytes(), &chalResp); err != nil {
		t.Fatalf("unmarshal challenge response: %v", err)
	}
	if chalResp.HostID != env.hostID.HostID {
		t.Fatalf("HostID mismatch: got %s, want %s", chalResp.HostID, env.hostID.HostID)
	}
	if chalResp.PairingNonce == "" {
		t.Fatal("empty pairing nonce")
	}

	// Test Single-Pending Invariant: concurrent request while challenge active
	recConcurrent := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, env.token)
	if recConcurrent.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for concurrent challenge, got %d", recConcurrent.Code)
	}

	// 2. Phase 2: POST /api/pair/reveal
	recRev := env.do(http.MethodPost, "/api/pair/reveal", map[string]string{
		"pairingNonce":       chalResp.PairingNonce,
		"clientEphemeralKey": hex.EncodeToString(clientEphemeral.PublicKey),
		"clientSalt":         hex.EncodeToString(clientSalt),
	}, env.token)
	if recRev.Code != http.StatusOK {
		t.Fatalf("POST /api/pair/reveal failed (%d): %s", recRev.Code, recRev.Body.String())
	}

	var revResp devices.RevealResponse
	if err := json.Unmarshal(recRev.Body.Bytes(), &revResp); err != nil {
		t.Fatalf("unmarshal reveal response: %v", err)
	}
	if revResp.HostEphemeralKey == "" || revResp.HostProof == "" {
		t.Fatalf("reveal response missing fields: %+v", revResp)
	}

	// Client validates Host commitment and proof
	hostEphemeralBytes, _ := hex.DecodeString(revResp.HostEphemeralKey)
	hostSaltBytes, _ := hex.DecodeString(revResp.HostSalt)
	hostCommitBytes, _ := hex.DecodeString(chalResp.HostCommitment)
	if !devices.VerifyCommitment(hostCommitBytes, hostEphemeralBytes, hostSaltBytes) {
		t.Fatal("client failed to verify host commitment opening")
	}

	kSharedClient, err := devices.ComputeSharedSecret(clientEphemeral.PrivateKey, hostEphemeralBytes)
	if err != nil {
		t.Fatal(err)
	}

	hostPubBytes, _ := hex.DecodeString(chalResp.HostPublicKey)
	hostIDDigest := sha256.Sum256(hostPubBytes)
	clientIDDigest := sha256.Sum256(clientPub)
	nonceBytes, _ := hex.DecodeString(chalResp.PairingNonce)

	clientSession, err := env.sess.Validate(env.token)
	if err != nil {
		t.Fatal(err)
	}
	clientSessionDigest := clientSession.Digest()

	clientTranscript, err := devices.BuildTranscript(devices.TranscriptParams{
		HostID:     hostIDDigest[:],
		ClientID:   clientIDDigest[:],
		CommitC:    clientCommit[:],
		CommitH:    hostCommitBytes,
		Ec:         clientEphemeral.PublicKey,
		Eh:         hostEphemeralBytes,
		Nh:         nonceBytes,
		SessionKey: clientSessionDigest[:],
	})
	if err != nil {
		t.Fatal(err)
	}
	clientTranscriptHash, err := devices.ComputeTranscriptHash(clientTranscript)
	if err != nil {
		t.Fatal(err)
	}

	// Verify SAS derivation
	_, clientSASDisp, err := devices.DeriveSASFromTranscript(nonceBytes, kSharedClient, clientTranscriptHash)
	if err != nil {
		t.Fatal(err)
	}
	if clientSASDisp == "" {
		t.Fatal("empty client SAS display")
	}

	// Verify HostProof signature
	hostProofPayload := devices.BuildHostProofPayload(clientTranscriptHash)
	hostProofSigBytes, _ := hex.DecodeString(revResp.HostProof)
	if !ed25519.Verify(ed25519.PublicKey(hostPubBytes), hostProofPayload, hostProofSigBytes) {
		t.Fatal("client failed to verify HostProof signature")
	}

	// 3. Phase 3: POST /api/pair/request with ClientProof
	clientProofSig := devices.SignClientProof(clientPriv, clientTranscriptHash)
	recReq := env.do(http.MethodPost, "/api/pair/request", map[string]string{
		"deviceName":      "Test Client Device",
		"clientPublicKey": hex.EncodeToString(clientPub),
		"pairingNonce":    chalResp.PairingNonce,
		"clientProof":     clientProofSig,
	}, env.token)
	if recReq.Code != http.StatusOK {
		t.Fatalf("POST /api/pair/request failed (%d): %s", recReq.Code, recReq.Body.String())
	}

	var reqResp devices.EnrollResponse
	if err := json.Unmarshal(recReq.Body.Bytes(), &reqResp); err != nil {
		t.Fatalf("unmarshal enroll response: %v", err)
	}
	if reqResp.Status != "paired" {
		t.Fatalf("expected status 'paired', got '%s'", reqResp.Status)
	}
	expectedDeviceID := hex.EncodeToString(clientIDDigest[:])
	if reqResp.DeviceID != expectedDeviceID {
		t.Fatalf("DeviceID mismatch: got %s, want %s", reqResp.DeviceID, expectedDeviceID)
	}

	// 4. Verify GET /api/devices returns the enrolled device
	recDevs := env.do(http.MethodGet, "/api/devices", nil, env.token)
	if recDevs.Code != http.StatusOK {
		t.Fatalf("GET /api/devices failed: %d", recDevs.Code)
	}
	var devList []devices.Device
	if err := json.Unmarshal(recDevs.Body.Bytes(), &devList); err != nil {
		t.Fatal(err)
	}
	if len(devList) != 1 || devList[0].ID != expectedDeviceID {
		t.Fatalf("expected 1 enrolled device %s, got: %+v", expectedDeviceID, devList)
	}
	if devList[0].AutoApprove != false {
		t.Fatal("default autoApprove must be false")
	}

	// 5. Test PUT /api/devices/{id}/auto-approve
	recAuto := env.do(http.MethodPut, fmt.Sprintf("/api/devices/%s/auto-approve", expectedDeviceID), map[string]bool{
		"autoApprove": true,
	}, env.token)
	if recAuto.Code != http.StatusOK {
		t.Fatalf("PUT auto-approve failed: %d", recAuto.Code)
	}
	devAfterAuto, ok := env.devices.Get(expectedDeviceID)
	if !ok || !devAfterAuto.AutoApprove {
		t.Fatal("autoApprove was not updated in store")
	}

	// 6. Test POST /api/auth/challenge
	recAuthChal := env.do(http.MethodPost, "/api/auth/challenge", map[string]string{
		"deviceId": expectedDeviceID,
	}, env.token)
	if recAuthChal.Code != http.StatusOK {
		t.Fatalf("POST /api/auth/challenge failed (%d): %s", recAuthChal.Code, recAuthChal.Body.String())
	}
	var authChalResp struct {
		HostID        string `json:"hostId"`
		DeviceID      string `json:"deviceId"`
		TransferNonce string `json:"transferNonce"`
		Timestamp     int64  `json:"timestamp"`
		HostSignature string `json:"hostSignature"`
	}
	if err := json.Unmarshal(recAuthChal.Body.Bytes(), &authChalResp); err != nil {
		t.Fatal(err)
	}
	if authChalResp.HostID != env.hostID.HostID || authChalResp.TransferNonce == "" {
		t.Fatalf("invalid auth chal response: %+v", authChalResp)
	}
	// Verify host signature on srv chal
	srvChalPayload := devices.BuildSrvChalPayload(authChalResp.HostID, expectedDeviceID, authChalResp.TransferNonce, authChalResp.Timestamp)
	hostSigBytes, _ := hex.DecodeString(authChalResp.HostSignature)
	if !ed25519.Verify(ed25519.PublicKey(hostPubBytes), srvChalPayload, hostSigBytes) {
		t.Fatal("failed to verify server challenge host signature")
	}

	// 7. Test DELETE /api/devices/{id}
	recDel := env.do(http.MethodDelete, fmt.Sprintf("/api/devices/%s", expectedDeviceID), nil, env.token)
	if recDel.Code != http.StatusOK {
		t.Fatalf("DELETE device failed: %d", recDel.Code)
	}
	if _, ok := env.devices.Get(expectedDeviceID); ok {
		t.Fatal("device was not removed")
	}
}

func TestServerPairingCommitmentMismatch(t *testing.T) {
	env := newPairingTestEnv(t)
	clientPub, _, _ := devices.GenerateKeyPair()
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	clientCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)

	// Phase 1
	recChal := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, env.token)
	if recChal.Code != http.StatusOK {
		t.Fatalf("Phase 1 failed: %d", recChal.Code)
	}
	var chalResp devices.ChallengeResponse
	_ = json.Unmarshal(recChal.Body.Bytes(), &chalResp)

	// Phase 2 with bad clientEphemeral
	badEphemeral, _ := devices.GenerateEphemeralKeyPair()
	recRev := env.do(http.MethodPost, "/api/pair/reveal", map[string]string{
		"pairingNonce":       chalResp.PairingNonce,
		"clientEphemeralKey": hex.EncodeToString(badEphemeral.PublicKey),
		"clientSalt":         hex.EncodeToString(clientSalt),
	}, env.token)

	if recRev.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad commitment opening, got %d", recRev.Code)
	}
}

func TestServerPairingPrematureRequest(t *testing.T) {
	env := newPairingTestEnv(t)
	clientPub, _, _ := devices.GenerateKeyPair()
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	clientCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)

	// Phase 1
	recChal := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, env.token)
	var chalResp devices.ChallengeResponse
	_ = json.Unmarshal(recChal.Body.Bytes(), &chalResp)

	// Directly call Phase 3 without Phase 2
	recReq := env.do(http.MethodPost, "/api/pair/request", map[string]string{
		"deviceName":      "Premature Client",
		"clientPublicKey": hex.EncodeToString(clientPub),
		"pairingNonce":    chalResp.PairingNonce,
		"clientProof":     "00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
	}, env.token)

	if recReq.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for premature request, got %d", recReq.Code)
	}
}

func TestServerPairingLockoutAndRearm(t *testing.T) {
	env := newPairingTestEnv(t)
	clientPub, _, _ := devices.GenerateKeyPair()

	// Cause 5 consecutive failures
	for i := 0; i < 5; i++ {
		fakeCommit := make([]byte, 32)
		recChal := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
			"clientPublicKey":  hex.EncodeToString(clientPub),
			"clientCommitment": hex.EncodeToString(fakeCommit),
		}, env.token)
		if recChal.Code != http.StatusOK {
			t.Fatalf("attempt %d failed to create challenge: %d", i, recChal.Code)
		}
		var chalResp devices.ChallengeResponse
		_ = json.Unmarshal(recChal.Body.Bytes(), &chalResp)

		// Fail reveal
		env.do(http.MethodPost, "/api/pair/reveal", map[string]string{
			"pairingNonce":       chalResp.PairingNonce,
			"clientEphemeralKey": hex.EncodeToString(fakeCommit),
			"clientSalt":         "00000000000000000000000000000000",
		}, env.token)
	}

	// 6th attempt must be rejected with 429 pairing_locked
	fakeCommit := make([]byte, 32)
	recLocked := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(fakeCommit),
	}, env.token)
	if recLocked.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests after 5 failures, got %d", recLocked.Code)
	}

	// POST /api/pair/rearm to clear lockout
	recRearm := env.do(http.MethodPost, "/api/pair/rearm", nil, env.token)
	if recRearm.Code != http.StatusOK {
		t.Fatalf("POST /api/pair/rearm failed: %d", recRearm.Code)
	}

	// Subsequent challenge must now succeed!
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	validCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	recAfterRearm := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(validCommit[:]),
	}, env.token)
	if recAfterRearm.Code != http.StatusOK {
		t.Fatalf("challenge after rearm failed (%d): %s", recAfterRearm.Code, recAfterRearm.Body.String())
	}
}

func TestServerPairingHostDenialPreventsPersistence(t *testing.T) {
	env := newPairingTestEnv(t)
	// Disable automatic prompt callback so we control manual approval/denial
	env.coord.OnPairingPrompt = nil

	clientPub, clientPriv, _ := devices.GenerateKeyPair()
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	clientCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)

	// Phase 1
	recChal := env.do(http.MethodPost, "/api/pair/challenge", map[string]string{
		"clientPublicKey":  hex.EncodeToString(clientPub),
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	}, env.token)
	var chalResp devices.ChallengeResponse
	_ = json.Unmarshal(recChal.Body.Bytes(), &chalResp)

	// Phase 2
	recRev := env.do(http.MethodPost, "/api/pair/reveal", map[string]string{
		"pairingNonce":       chalResp.PairingNonce,
		"clientEphemeralKey": hex.EncodeToString(clientEphemeral.PublicKey),
		"clientSalt":         hex.EncodeToString(clientSalt),
	}, env.token)
	var revResp devices.RevealResponse
	_ = json.Unmarshal(recRev.Body.Bytes(), &revResp)

	hostPubBytes, _ := hex.DecodeString(chalResp.HostPublicKey)
	hostEphemeralBytes, _ := hex.DecodeString(revResp.HostEphemeralKey)
	hostCommitBytes, _ := hex.DecodeString(chalResp.HostCommitment)
	nonceBytes, _ := hex.DecodeString(chalResp.PairingNonce)

	clientSession, _ := env.sess.Validate(env.token)
	clientSessionDigest := clientSession.Digest()

	clientTranscript, _ := devices.BuildTranscript(devices.TranscriptParams{
		HostID:     sha256Hash(hostPubBytes),
		ClientID:   sha256Hash(clientPub),
		CommitC:    clientCommit[:],
		CommitH:    hostCommitBytes,
		Ec:         clientEphemeral.PublicKey,
		Eh:         hostEphemeralBytes,
		Nh:         nonceBytes,
		SessionKey: clientSessionDigest[:],
	})
	transcriptHash, _ := devices.ComputeTranscriptHash(clientTranscript)
	clientProofSig := devices.SignClientProof(clientPriv, transcriptHash)

	// In background, start Phase 3 request
	chDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := env.do(http.MethodPost, "/api/pair/request", map[string]string{
			"deviceName":      "Denial Candidate",
			"clientPublicKey": hex.EncodeToString(clientPub),
			"pairingNonce":    chalResp.PairingNonce,
			"clientProof":     clientProofSig,
		}, env.token)
		chDone <- rec
	}()

	// Wait until coordinator exposes verified pending approval
	var reqID string
	for i := 0; i < 50; i++ {
		status := env.coord.GetPairingStatus()
		if status.RequiresApproval && status.RequestID != "" {
			reqID = status.RequestID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if reqID == "" {
		t.Fatal("coordinator failed to expose verified pending approval")
	}

	// Verify device is NOT yet in store before host decision
	devID := hex.EncodeToString(sha256Hash(clientPub))
	if _, ok := env.devices.Get(devID); ok {
		t.Fatal("device was persisted before host decision")
	}

	// Operator clicks DENY
	if err := env.coord.DenyPairing(reqID); err != nil {
		t.Fatalf("DenyPairing failed: %v", err)
	}

	// Client response must be 403 Forbidden
	recReq := <-chDone
	if recReq.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on host denial, got: %d (%s)", recReq.Code, recReq.Body.String())
	}

	// Verify device is NEVER persisted
	if _, ok := env.devices.Get(devID); ok {
		t.Fatal("denied device was persisted in store")
	}
}
