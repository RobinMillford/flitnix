package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
)

func waitForPeerJobDone(t *testing.T, app *App, jobID string, timeout time.Duration) PeerTransferJob {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, j := range app.ListPeerTransfers() {
			if j.ID == jobID {
				switch j.State {
				case "completed", "failed", "canceled":
					return j
				}
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for peer transfer job %s", jobID)
	return PeerTransferJob{}
}

func TestTwoDesktopInstancesBidirectionalAndTrustedTransfer(t *testing.T) {
	cfgA := isolateAppStorage(t)
	appA := newIsolatedAppIn(t, cfgA)
	t.Cleanup(func() { appA.shutdown(context.Background()) })

	cfgB := isolateAppStorage(t)
	appB := newIsolatedAppIn(t, cfgB)
	t.Cleanup(func() { appB.shutdown(context.Background()) })

	// Bind both desktop servers cleanly on loopback ephemeral ports.
	if err := appA.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatalf("appA.ensureServer: %v", err)
	}
	if err := appB.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatalf("appB.ensureServer: %v", err)
	}
	addrA := appA.ServerAddress()
	addrB := appB.ServerAddress()

	// Prepare test files & nested folder on Computer A.
	srcDir := t.TempDir()
	srcFolder := filepath.Join(srcDir, "ProjectBundle")
	if err := os.MkdirAll(filepath.Join(srcFolder, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	payload1 := bytes.Repeat([]byte("desktop-to-desktop-"), 512)
	file1 := filepath.Join(srcFolder, "sub", "report.txt")
	if err := os.WriteFile(file1, payload1, 0o600); err != nil {
		t.Fatalf("write file1: %v", err)
	}

	// 1. VisibilityHidden on B blocks unsolicited send from A.
	if _, err := appB.SetVisibilityMode(string(devices.VisibilityHidden)); err != nil {
		t.Fatalf("B SetVisibilityMode(hidden): %v", err)
	}
	jobHidden, err := appA.ConnectAndSendToPeer(PeerSendRequest{
		Address: addrB,
		Paths:   []string{file1},
	})
	if err != nil {
		t.Fatalf("ConnectAndSendToPeer kickoff failed: %v", err)
	}
	doneHidden := waitForPeerJobDone(t, appA, jobHidden.ID, 5*time.Second)
	if doneHidden.State != "failed" {
		t.Fatalf("expected failed job while B is Hidden, got %+v", doneHidden)
	}

	// 2. B opens Everyone mode -> A inspects TLS identity and sends folder with manual approval on B.
	if _, err := appB.SetVisibilityMode(string(devices.VisibilityEveryone)); err != nil {
		t.Fatalf("B SetVisibilityMode(everyone): %v", err)
	}
	probeB, err := appA.InspectPeerTarget(addrB, 0)
	if err != nil {
		t.Fatalf("InspectPeerTarget B: %v", err)
	}
	if probeB.HostID != appB.hostIdentity.HostID || !probeB.TLSActive {
		t.Fatalf("unexpected probe B: %+v", probeB)
	}

	// Background approver on B approves the incoming folder batch once it arrives in Approval Inbox.
	approvedFolder := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			batches := appB.ListPendingBatches()
			if len(batches) > 0 {
				_, _ = appB.ApproveBatch(batches[0].ID)
				close(approvedFolder)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	jobFolder, err := appA.ConnectAndSendToPeer(PeerSendRequest{
		Address:        addrB,
		ExpectedHostID: probeB.HostID,
		ExpectedPubKey: probeB.PublicKey,
		PeerName:       "Desktop B",
		Paths:          []string{srcFolder},
	})
	if err != nil {
		t.Fatalf("ConnectAndSendToPeer folder: %v", err)
	}
	<-approvedFolder
	doneFolder := waitForPeerJobDone(t, appA, jobFolder.ID, 10*time.Second)
	if doneFolder.State != "completed" || !doneFolder.Verified {
		t.Fatalf("folder peer job failed: %+v", doneFolder)
	}
	receivedPath := filepath.Join(appB.ReceiveDirectory(), "ProjectBundle", "sub", "report.txt")
	gotBytes, err := os.ReadFile(receivedPath)
	if err != nil || !bytes.Equal(gotBytes, payload1) {
		t.Fatalf("received folder payload mismatch: err=%v len=%d", err, len(gotBytes))
	}

	// 3. Pair Computer A with Computer B over TLS 1.3 (B auto-approves pairing prompt in test).
	appB.pairing.OnPairingPrompt = func(_, _, _ string, decision chan<- bool) {
		decision <- true
	}
	pairRes, err := appA.PairWithPeer(PeerPairRequest{
		Address:        addrB,
		ExpectedHostID: probeB.HostID,
		ExpectedPubKey: probeB.PublicKey,
		PeerName:       "Desktop B",
	})
	if err != nil {
		t.Fatalf("PairWithPeer: %v", err)
	}

	// B grants A AutoApprove (with G15 safe policy), then restricts visibility to TrustedOnly.
	if err := appB.SetDevicePolicy(pairRes.DeviceID, devices.DevicePolicy{
		AutoApprove:         true,
		MaxAutoApproveBytes: 1024 * 1024,
		AllowExecutables:    false,
		AllowScripts:        false,
	}); err != nil {
		t.Fatalf("B SetDevicePolicy: %v", err)
	}
	if _, err := appB.SetVisibilityMode(string(devices.VisibilityTrustedOnly)); err != nil {
		t.Fatalf("B SetVisibilityMode(trusted): %v", err)
	}

	// A sends a safe file to B while B is TrustedOnly -> must auto-approve WITHOUT manual inbox click!
	safeFile := filepath.Join(srcDir, "trusted-note.txt")
	if err := os.WriteFile(safeFile, []byte("trusted hello"), 0o600); err != nil {
		t.Fatalf("write safeFile: %v", err)
	}
	jobTrusted, err := appA.ConnectAndSendToPeer(PeerSendRequest{
		Address:        addrB,
		ExpectedHostID: probeB.HostID,
		ExpectedPubKey: probeB.PublicKey,
		PeerName:       "Desktop B",
		Paths:          []string{safeFile},
	})
	if err != nil {
		t.Fatalf("trusted ConnectAndSendToPeer: %v", err)
	}
	doneTrusted := waitForPeerJobDone(t, appA, jobTrusted.ID, 10*time.Second)
	if doneTrusted.State != "completed" || !doneTrusted.TrustedPeer || !doneTrusted.Verified {
		t.Fatalf("trusted peer job unexpected: %+v", doneTrusted)
	}

	// 4. Reverse transfer B -> A using 6-digit connection code while A is Hidden!
	if _, err := appA.SetVisibilityMode(string(devices.VisibilityHidden)); err != nil {
		t.Fatalf("A SetVisibilityMode(hidden): %v", err)
	}
	sessA, err := appA.sessions.Create()
	if err != nil {
		t.Fatalf("A sessions.Create: %v", err)
	}
	scA := appA.GetSessionShortCode(sessA.Key())

	doneRevApprove := make(chan struct{})
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			batches := appA.ListPendingBatches()
			if len(batches) > 0 {
				_, _ = appA.ApproveBatch(batches[0].ID)
				close(doneRevApprove)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	revFile := filepath.Join(t.TempDir(), "reply.txt")
	if err := os.WriteFile(revFile, []byte("reply from B"), 0o600); err != nil {
		t.Fatalf("write revFile: %v", err)
	}
	jobRev, err := appB.ConnectAndSendToPeer(PeerSendRequest{
		Address:   addrA,
		ShortCode: scA.Code,
		PeerName:  "Desktop A",
		Paths:     []string{revFile},
	})
	if err != nil {
		t.Fatalf("reverse ConnectAndSendToPeer: %v", err)
	}
	<-doneRevApprove
	doneRev := waitForPeerJobDone(t, appB, jobRev.ID, 10*time.Second)
	if doneRev.State != "completed" || !doneRev.Verified {
		t.Fatalf("reverse peer job failed: %+v", doneRev)
	}

	// 5. TOFU violation: if addrB suddenly serves a rotated/swapped HostID, ConnectAndSendToPeer fails closed!
	cfgC := isolateAppStorage(t)
	appC := newIsolatedAppIn(t, cfgC)
	t.Cleanup(func() { appC.shutdown(context.Background()) })
	if err := appC.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatalf("appC.ensureServer: %v", err)
	}
	_, _ = appC.SetVisibilityMode(string(devices.VisibilityEveryone))
	jobTofuMismatch, err := appA.ConnectAndSendToPeer(PeerSendRequest{
		Address:        appC.ServerAddress(),
		ExpectedHostID: probeB.HostID,
		ExpectedPubKey: probeB.PublicKey,
		Paths:          []string{safeFile},
	})
	if err != nil {
		t.Fatalf("jobTofuMismatch kickoff: %v", err)
	}
	doneTofu := waitForPeerJobDone(t, appA, jobTofuMismatch.ID, 5*time.Second)
	if doneTofu.State != "failed" {
		t.Fatalf("expected TOFU hostId mismatch to fail closed, got %+v", doneTofu)
	}

	// 6. Authority key swap without ExpectedHostID against previously paired LastAddress (addrB) vs appC?
	// Even if A removes B's trust on B side, A must surface revocation clearly instead of falling back silently.
	if err := appB.RemoveTrustedDevice(pairRes.DeviceID); err != nil {
		t.Fatalf("B RemoveTrustedDevice: %v", err)
	}
	_, _ = appB.SetVisibilityMode(string(devices.VisibilityEveryone))
	jobRevoked, err := appA.ConnectAndSendToPeer(PeerSendRequest{
		Address:        addrB,
		ExpectedHostID: probeB.HostID,
		ExpectedPubKey: probeB.PublicKey,
		Paths:          []string{safeFile},
	})
	if err != nil {
		t.Fatalf("jobRevoked kickoff: %v", err)
	}
	doneRevoked := waitForPeerJobDone(t, appA, jobRevoked.ID, 5*time.Second)
	if doneRevoked.State != "failed" {
		t.Fatalf("expected revoked peer to fail explicitly instead of silently downgrading, got %+v", doneRevoked)
	}
}
