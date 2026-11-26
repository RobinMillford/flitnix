package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/server"
)

func TestDeviceSessionChallengeVerifyVisibilityAndReplay(t *testing.T) {
	h := newSecurityTestHarness(t)
	vis := devices.NewVisibilityManager(devices.VisibilityState{
		Mode:                    devices.VisibilityTrustedOnly,
		PreviousRestrictiveMode: devices.VisibilityTrustedOnly,
	}, time.Now)
	h.svcs.Visibility = vis

	pub, priv, err := devices.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	dev, err := h.devStore.Add("iPhone Safari", pub, true)
	if err != nil {
		t.Fatalf("devStore.Add: %v", err)
	}

	// Hidden mode blocks unsolicited device session reconnects cleanly (403 visibility_hidden).
	if _, err := vis.SetMode(devices.VisibilityHidden, 0); err != nil {
		t.Fatalf("SetMode(hidden): %v", err)
	}
	hidRec := h.doJSON(http.MethodPost, "/api/device-sessions/challenge", map[string]string{"deviceId": dev.ID}, "", nil)
	if hidRec.Code != http.StatusForbidden {
		t.Fatalf("hidden challenge = %d, want 403 (%s)", hidRec.Code, hidRec.Body.String())
	}

	// Switch back to Trusted Only -> challenge succeeds.
	if _, err := vis.SetMode(devices.VisibilityTrustedOnly, 0); err != nil {
		t.Fatalf("SetMode(trusted): %v", err)
	}
	chalRec := h.doJSON(http.MethodPost, "/api/device-sessions/challenge", map[string]string{"deviceId": dev.ID}, "", nil)
	if chalRec.Code != http.StatusOK {
		t.Fatalf("challenge = %d, want 200 (%s)", chalRec.Code, chalRec.Body.String())
	}
	var chal struct {
		HostID    string `json:"hostId"`
		DeviceID  string `json:"deviceId"`
		Nonce     string `json:"nonce"`
		Timestamp int64  `json:"timestamp"`
	}
	if err := json.Unmarshal(chalRec.Body.Bytes(), &chal); err != nil {
		t.Fatalf("unmarshal challenge: %v", err)
	}

	// Wrong signature fails with 401 without consuming nonce or minting session.
	badRec := h.doJSON(http.MethodPost, "/api/device-sessions/verify", map[string]any{
		"deviceId":  chal.DeviceID,
		"hostId":    chal.HostID,
		"nonce":     chal.Nonce,
		"timestamp": chal.Timestamp,
		"signature": "00000000",
	}, "", nil)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", badRec.Code)
	}

	// Valid signature yields a fresh 256-bit device Bearer token.
	payload := devices.BuildDeviceSessionAuthPayload(chal.HostID, chal.DeviceID, chal.Timestamp, chal.Nonce)
	sig := devices.SignMessage(priv, payload)
	verifyRec := h.doJSON(http.MethodPost, "/api/device-sessions/verify", map[string]any{
		"deviceId":  chal.DeviceID,
		"hostId":    chal.HostID,
		"nonce":     chal.Nonce,
		"timestamp": chal.Timestamp,
		"signature": sig,
	}, "", nil)
	if verifyRec.Code != http.StatusOK {
		t.Fatalf("verify = %d, want 200 (%s)", verifyRec.Code, verifyRec.Body.String())
	}
	var verified struct {
		Token      string `json:"token"`
		DeviceID   string `json:"deviceId"`
		DeviceName string `json:"deviceName"`
	}
	if err := json.Unmarshal(verifyRec.Body.Bytes(), &verified); err != nil {
		t.Fatalf("unmarshal verify: %v", err)
	}
	if verified.Token == "" || verified.DeviceID != dev.ID {
		t.Fatalf("unexpected verify response: %+v", verified)
	}

	// Single-use nonce replay is rejected.
	replayRec := h.doJSON(http.MethodPost, "/api/device-sessions/verify", map[string]any{
		"deviceId":  chal.DeviceID,
		"hostId":    chal.HostID,
		"nonce":     chal.Nonce,
		"timestamp": chal.Timestamp,
		"signature": sig,
	}, "", nil)
	if replayRec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed verify = %d, want 401", replayRec.Code)
	}

	// Auto-approval works over authenticated KindDevice session (and revoking rejects future reconnects).
	sum := sha256.Sum256([]byte("reconnected-upload"))
	upRec := h.doJSON(http.MethodPost, "/api/uploads", map[string]any{
		"name":   "from-bookmark.jpg",
		"size":   18,
		"sha256": hex.EncodeToString(sum[:]),
	}, verified.Token, nil)
	if upRec.Code != http.StatusCreated {
		t.Fatalf("upload after reconnect = %d, want 201 (%s)", upRec.Code, upRec.Body.String())
	}
	var upOut struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(upRec.Body.Bytes(), &upOut)
	if upOut.State != "approved" {
		t.Fatalf("trusted device autoApprove state = %q, want approved", upOut.State)
	}

	// Revoking the trusted device must immediately block new reconnect challenges.
	if err := h.devStore.Remove(dev.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	revokedRec := h.doJSON(http.MethodPost, "/api/device-sessions/challenge", map[string]string{"deviceId": dev.ID}, "", nil)
	if revokedRec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked device challenge = %d, want 401", revokedRec.Code)
	}
}

func TestDeviceCryptoJSInteroperabilityWithGoServer(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed; skipping JS crypto interoperability test")
	}
	h := newSecurityTestHarness(t)
	h.ts.Close()
	svcs := *h.svcs
	svcs.Visibility = devices.NewVisibilityManager(devices.VisibilityState{
		Mode:                    devices.VisibilityTrustedOnly,
		PreviousRestrictiveMode: devices.VisibilityTrustedOnly,
	}, time.Now)
	srvWithVis := server.New(server.Config{Services: &svcs})
	ts := httptest.NewServer(srvWithVis.Handler())
	defer ts.Close()

	assetsDir, err := filepath.Abs("assets")
	if err != nil {
		t.Fatalf("abs assets dir: %v", err)
	}

	script := `
const assert = require("assert");
globalThis.sha256 = require(process.env.ASSETS_DIR + "/sha256.min.js");
globalThis.nacl = require(process.env.ASSETS_DIR + "/nacl-fast.min.js");
const cryptoApi = require(process.env.ASSETS_DIR + "/device-crypto.js");

(async () => {
  const baseUrl = process.env.BASE_URL;
  const qrToken = process.env.QR_TOKEN;

  const id = cryptoApi.generateDeviceIdentity();
  const eph = cryptoApi.createEphemeralPairingData();

  const chalRes = await fetch(baseUrl + "/api/pair/challenge", {
    method: "POST",
    headers: { "Authorization": "Bearer " + qrToken, "Content-Type": "application/json" },
    body: JSON.stringify({ clientPublicKey: id.publicKeyHex, clientCommitment: eph.commitmentHex })
  });
  assert.strictEqual(chalRes.status, 200, "challenge failed");
  const chal = await chalRes.json();

  const revRes = await fetch(baseUrl + "/api/pair/reveal", {
    method: "POST",
    headers: { "Authorization": "Bearer " + qrToken, "Content-Type": "application/json" },
    body: JSON.stringify({
      pairingNonce: chal.pairingNonce,
      clientEphemeralKey: eph.ephemeralPublicKeyHex,
      clientSalt: eph.saltHex
    })
  });
  assert.strictEqual(revRes.status, 200, "reveal failed");
  const rev = await revRes.json();

  const verified = cryptoApi.completeHostReveal({
    token: qrToken,
    hostPublicKeyHex: chal.hostPublicKey,
    hostCommitmentHex: chal.hostCommitment,
    hostEphemeralHex: rev.hostEphemeralKey,
    hostSaltHex: rev.hostSalt,
    hostProofHex: rev.hostProof,
    pairingNonceHex: chal.pairingNonce,
    clientPublicKeyHex: id.publicKeyHex,
    clientCommitmentHex: eph.commitmentHex,
    clientEphemeralPublicKey: eph.ephemeralPublicKey,
    clientEphemeralSecretKey: eph.ephemeralSecretKey,
    clientSecretKeyHex: id.secretKeyHex
  });
  assert.strictEqual(verified.sas, rev.sasCode, "SAS mismatch between JS and Go");

  const enrollRes = await fetch(baseUrl + "/api/pair/request", {
    method: "POST",
    headers: { "Authorization": "Bearer " + qrToken, "Content-Type": "application/json" },
    body: JSON.stringify({
      deviceName: "Node Test Phone",
      clientPublicKey: id.publicKeyHex,
      pairingNonce: chal.pairingNonce,
      clientProof: verified.clientProofHex
    })
  });
  assert.strictEqual(enrollRes.status, 200, "enroll failed");
  const enrolled = await enrollRes.json();
  assert.ok(enrolled.deviceId, "missing deviceId");

  // Reconnect without QR token via device-sessions endpoints
  const rcChalRes = await fetch(baseUrl + "/api/device-sessions/challenge", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ deviceId: enrolled.deviceId })
  });
  assert.strictEqual(rcChalRes.status, 200, "reconnect challenge failed");
  const rcChal = await rcChalRes.json();

  const sig = cryptoApi.signDeviceSessionChallenge({
    hostId: rcChal.hostId,
    deviceId: rcChal.deviceId,
    nonce: rcChal.nonce,
    timestamp: rcChal.timestamp,
    secretKeyHex: id.secretKeyHex
  });
  const rcVerRes = await fetch(baseUrl + "/api/device-sessions/verify", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      deviceId: rcChal.deviceId,
      hostId: rcChal.hostId,
      nonce: rcChal.nonce,
      timestamp: rcChal.timestamp,
      signature: sig
    })
  });
  assert.strictEqual(rcVerRes.status, 200, "reconnect verify failed");
  const rcSession = await rcVerRes.json();
  assert.ok(rcSession.token && rcSession.token !== qrToken, "expected fresh reconnect session token");

  const filesRes = await fetch(baseUrl + "/api/files", {
    headers: { "Authorization": "Bearer " + rcSession.token }
  });
  assert.strictEqual(filesRes.status, 200, "reconnected token failed /api/files");
})();
`
	cmd := exec.Command("node", "-e", script)
	cmd.Env = append(cmd.Environ(),
		"ASSETS_DIR="+assetsDir,
		"BASE_URL="+ts.URL,
		"QR_TOKEN="+h.sess.Token(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("JS crypto interop failed: %v\nOutput:\n%s", err, string(out))
	}
}
