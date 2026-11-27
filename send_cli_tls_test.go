package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestSendCLITLS13RequiredAndPinning(t *testing.T) {
	setClientConfigDir(t, t.TempDir())

	// 1. Start TLS-capable server with HostIdentity.
	receiveDir := t.TempDir()
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: receiveDir})
	hostID, err := devices.LoadOrCreateHostIdentity(filepath.Join(t.TempDir(), "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	pendingBatches := make(chan pendingServeBatch, 8)
	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services: &server.Services{
			Sessions:     sessMgr,
			Transfers:    transfers,
			Coordinator:  coord,
			Registry:     transfer.NewRegistry(),
			Downloads:    transfer.NewDownloadRegistry(),
			HostIdentity: hostID,
			OnBatchPending: func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
				pendingBatches <- pendingServeBatch{sessionKey: sessionKey, batch: batch, remoteAddr: remoteAddr}
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	<-srv.Ready()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-pendingBatches:
				for _, it := range ev.batch.Items {
					if cid, err := coord.ItemContentID(ev.sessionKey, ev.batch.ID, it.ID); err == nil && cid != "" {
						_, _ = transfers.Decide(cid, true)
					}
				}
				_, _ = coord.ApproveBatch(ev.sessionKey, ev.batch.ID)
			}
		}
	}()

	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(t.TempDir(), "tls_send.txt")
	if err := os.WriteFile(srcFile, []byte("verified tls13 cli transfer"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Send with --require-tls and --host-id over http:// URL -> upgrades to verified TLS 1.3 and succeeds!
	var stdout, stderr bytes.Buffer
	code := runSendCLI([]string{
		"--to", srv.URL() + "/#" + sess.Token(),
		"--host-id", hostID.HostID,
		"--require-tls",
		"--yes",
		srcFile,
	}, &stdout, &stderr)
	if code != ExitSuccess {
		t.Fatalf("expected ExitSuccess for TLS 1.3 send, got %d (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "TLS 1.3 Pinned") {
		t.Fatalf("expected TLS 1.3 Pinned transport line, got: %s", stdout.String())
	}

	// Mismatched --host-id must fail closed with ExitAuthError!
	stdout.Reset()
	stderr.Reset()
	badCode := runSendCLI([]string{
		"--to", srv.URL() + "/#" + sess.Token(),
		"--host-id", strings.Repeat("0", 64),
		"--require-tls",
		"--yes",
		srcFile,
	}, &stdout, &stderr)
	if badCode != ExitAuthError {
		t.Fatalf("expected ExitAuthError on host-id mismatch, got %d (stderr: %s)", badCode, stderr.String())
	}
}

func TestSendAndPairCLIRefuseUnencryptedWhenRequireTLS(t *testing.T) {
	setClientConfigDir(t, t.TempDir())

	// Plain HTTP-only server (no HostIdentity)
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: t.TempDir()})
	sessMgr := session.NewManager(session.Config{})
	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services: &server.Services{
			Sessions:    sessMgr,
			Transfers:   transfer.NewService(transfer.Config{Store: store}),
			Coordinator: transfer.NewCoordinator(transfer.CoordinatorConfig{}),
			Registry:    transfer.NewRegistry(),
			Downloads:   transfer.NewDownloadRegistry(),
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("server start timeout")
	}
	sess, _ := sessMgr.Create()
	srcFile := filepath.Join(t.TempDir(), "plain.txt")
	_ = os.WriteFile(srcFile, []byte("hello"), 0o600)

	var stdout, stderr bytes.Buffer
	code := runSendCLI([]string{
		"--to", srv.URL() + "/#" + sess.Token(),
		"--require-tls",
		"--yes",
		srcFile,
	}, &stdout, &stderr)
	if code != ExitAuthError {
		t.Fatalf("expected ExitAuthError when --require-tls hits plain HTTP peer, got %d", code)
	}

	stdout.Reset()
	stderr.Reset()
	pairCode := runPairCLIWithStdin([]string{
		"--to", srv.URL() + "/#" + sess.Token(),
		"--require-tls",
		"--json",
	}, nil, &stdout, &stderr)
	if pairCode != ExitAuthError {
		t.Fatalf("expected ExitAuthError when pair --require-tls hits plain HTTP peer, got %d", pairCode)
	}
	var errRes map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &errRes); err != nil || errRes["error"] != "tls_verification_failed" {
		t.Fatalf("expected tls_verification_failed JSON error, got %s", stdout.String())
	}
}

func TestKnownHostsCLIRecovery(t *testing.T) {
	setClientConfigDir(t, t.TempDir())
	kh, err := client.LoadOrCreateKnownHosts("")
	if err != nil {
		t.Fatal(err)
	}
	hid := strings.Repeat("a", 64)
	if err := kh.PinWithAddress(hid, "Peer One", strings.Repeat("b", 64), "192.168.1.10:43821"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runKnownHostsCLI([]string{"list"}, &stdout, &stderr); code != ExitSuccess {
		t.Fatalf("known-hosts list failed: %d %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), hid) {
		t.Fatalf("expected hostId in known-hosts list, got: %s", stdout.String())
	}

	stdout.Reset()
	if code := runKnownHostsCLI([]string{"remove", hid}, &stdout, &stderr); code != ExitSuccess {
		t.Fatalf("known-hosts remove failed: %d %s", code, stderr.String())
	}
	reloaded, _ := client.LoadOrCreateKnownHosts("")
	if _, exists := reloaded.Get(hid); exists {
		t.Fatal("expected hostId to be removed from known_hosts")
	}
}
