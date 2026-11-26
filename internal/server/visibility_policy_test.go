package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestQRSharingPausedBlocksQRButNotDeviceSession(t *testing.T) {
	h := newSecurityTestHarness(t)
	vis := devices.NewVisibilityManager(devices.VisibilityState{
		Mode:                    devices.VisibilityHidden,
		PreviousRestrictiveMode: devices.VisibilityHidden,
	}, time.Now)
	h.svcs.Visibility = vis

	// Add a shared file to test /api/files, /download, and /api/download-grants.
	sharedPath := filepath.Join(h.dir, "shared.txt")
	if err := os.WriteFile(sharedPath, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write shared file: %v", err)
	}
	sf, err := h.svcs.Registry.Add(sharedPath, "shared.txt", h.sess.Key())
	if err != nil {
		t.Fatalf("Registry.Add: %v", err)
	}

	// Hidden mode must still permit explicit QR sessions until paused.
	sum := sha256.Sum256([]byte("hello"))
	digest := hex.EncodeToString(sum[:])
	rec := h.doJSON(http.MethodPost, "/api/uploads", map[string]any{
		"name":   "qr-hidden-ok.txt",
		"size":   5,
		"sha256": digest,
	}, h.sess.Token(), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("hidden mode should preserve explicit QR uploads, got %d: %s", rec.Code, rec.Body.String())
	}

	// Pausing QR sharing rejects QR upload/files/download/grants with 403 qr_sharing_paused.
	if _, err := vis.SetQRSharingPaused(true); err != nil {
		t.Fatalf("SetQRSharingPaused(true): %v", err)
	}
	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/uploads", map[string]any{"name": "paused.txt", "size": 5, "sha256": digest}},
		{http.MethodGet, "/api/files", nil},
		{http.MethodGet, "/api/files/" + sf.ID + "/download", nil},
		{http.MethodPost, "/api/download-grants", map[string]any{"fileId": sf.ID}},
	} {
		r := h.doJSON(tc.method, tc.path, tc.body, h.sess.Token(), nil)
		if r.Code != http.StatusForbidden {
			t.Fatalf("%s %s while QR paused = %d, want 403 (%s)", tc.method, tc.path, r.Code, r.Body.String())
		}
		var errBody map[string]string
		_ = json.Unmarshal(r.Body.Bytes(), &errBody)
		if errBody["code"] != "qr_sharing_paused" {
			t.Fatalf("%s %s error code = %q, want qr_sharing_paused", tc.method, tc.path, errBody["code"])
		}
	}

	// Resuming QR sharing restores access immediately.
	if _, err := vis.SetQRSharingPaused(false); err != nil {
		t.Fatalf("SetQRSharingPaused(false): %v", err)
	}
	rOK := h.doJSON(http.MethodGet, "/api/files", nil, h.sess.Token(), nil)
	if rOK.Code != http.StatusOK {
		t.Fatalf("GET /api/files after resume = %d, want 200", rOK.Code)
	}
}

func TestConditionalAutoApprovalEnforcementOverHTTP(t *testing.T) {
	h := newSecurityTestHarness(t)
	vis := devices.NewVisibilityManager(devices.VisibilityState{
		Mode:                    devices.VisibilityTrustedOnly,
		PreviousRestrictiveMode: devices.VisibilityTrustedOnly,
	}, time.Now)
	h.svcs.Visibility = vis

	clientPub, clientPriv, _ := devices.GenerateKeyPair()
	dev, err := h.devStore.Add("Trusted Phone", clientPub, true)
	if err != nil {
		t.Fatalf("devStore.Add: %v", err)
	}

	// Set tight policy: AutoApprove=true, max 1 KiB, executables/scripts disallowed.
	recPolicy := h.doJSON(http.MethodPut, "/api/devices/"+dev.ID+"/policy", devices.DevicePolicy{
		AutoApprove:         true,
		MaxAutoApproveBytes: 1024,
		AllowExecutables:    false,
		AllowScripts:        false,
	}, h.sess.Token(), nil)
	if recPolicy.Code != http.StatusOK {
		t.Fatalf("PUT policy = %d: %s", recPolicy.Code, recPolicy.Body.String())
	}

	declareSigned := func(name string, payload []byte) transfer.Upload {
		t.Helper()
		sum := sha256.Sum256(payload)
		digest := hex.EncodeToString(sum[:])
		nonce, ts, err := h.devStore.CreateTransferChallenge(h.hostID.HostID, dev.ID)
		if err != nil {
			t.Fatalf("CreateTransferChallenge: %v", err)
		}
		cm := &transfer.CanonicalManifest{
			Kind: string(transfer.BatchKindFiles),
			Items: []transfer.CanonicalManifestItem{
				{DisplayName: name, Sequence: 1, SHA256: digest, Size: strconv.FormatInt(int64(len(payload)), 10)},
			},
		}
		_, mHash, _ := transfer.ComputeManifestSHA256(cm)
		authPayload := devices.BuildAuthV2Payload(h.hostID.HostID, dev.ID, ts, nonce, mHash)
		sig := devices.SignMessage(clientPriv, authPayload)

		res := h.doJSON(http.MethodPost, "/api/uploads", map[string]any{
			"name":   name,
			"size":   len(payload),
			"sha256": digest,
		}, h.sess.Token(), map[string]string{
			"X-Flitnix-Device-ID": dev.ID,
			"X-Flitnix-Nonce":     nonce,
			"X-Flitnix-Timestamp": strconv.FormatInt(ts, 10),
			"X-Flitnix-Signature": sig,
		})
		if res.Code != http.StatusCreated {
			t.Fatalf("declare %s failed %d: %s", name, res.Code, res.Body.String())
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(res.Body.Bytes(), &out)
		up, ok := h.svcs.Transfers.Get(out.ID)
		if !ok {
			t.Fatalf("upload %s not stored", out.ID)
		}
		return up
	}

	// 1. Small safe file -> auto-approved.
	safeUp := declareSigned("notes.txt", []byte("small safe note"))
	if safeUp.State != transfer.StateApproved {
		t.Fatalf("safe file state = %s, want approved", safeUp.State)
	}

	// 2. Oversized safe file -> falls back to pending with reason.
	bigUp := declareSigned("big.txt", bytes.Repeat([]byte("X"), 2048))
	if bigUp.State != transfer.StatePending || bigUp.ApprovalReason == "" {
		t.Fatalf("oversized file state=%s reason=%q, want pending with reason", bigUp.State, bigUp.ApprovalReason)
	}

	// 3. Executable file within size limit -> falls back to pending with reason.
	exeUp := declareSigned("tool.exe", []byte("MZ"))
	if exeUp.State != transfer.StatePending || exeUp.ApprovalReason == "" {
		t.Fatalf("executable file state=%s reason=%q, want pending with reason", exeUp.State, exeUp.ApprovalReason)
	}
}
