package client_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func startCLITestServer(t *testing.T, withTLS bool) (*server.Server, *devices.HostIdentity, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	sessMgr := session.NewManager(session.Config{})
	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}
	var hostID *devices.HostIdentity
	if withTLS {
		hostID, err = devices.LoadOrCreateHostIdentity(filepath.Join(dir, "host.json"))
		if err != nil {
			t.Fatal(err)
		}
	}
	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services: &server.Services{
			Sessions:     sessMgr,
			Transfers:    transfer.NewService(transfer.Config{Store: store}),
			Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
			Registry:     transfer.NewRegistry(),
			Downloads:    transfer.NewDownloadRegistry(),
			HostIdentity: hostID,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("server start timeout")
	}
	return srv, hostID, sess.Token()
}

func TestResolveCLITargetAndClient_UpgradesToPinnedTLS13AndDetectsMITM(t *testing.T) {
	srv, hostID, tok := startCLITestServer(t, true)
	kh, err := client.LoadOrCreateKnownHosts(filepath.Join(t.TempDir(), "known_hosts.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Passing an http:// URL to a TLS-capable server automatically upgrades to pinned https://.
	target, err := client.ParseTarget(srv.URL()+"/#"+tok, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.ResolveCLITargetAndClient(context.Background(), target, client.CLITLSOptions{
		KnownHosts: kh,
		RequireTLS: true,
	})
	if err != nil {
		t.Fatalf("ResolveCLITargetAndClient: %v", err)
	}
	if !res.TLSActive || res.Target.Scheme() != "https" || res.Probe == nil || res.Probe.HostID != hostID.HostID {
		t.Fatalf("expected upgraded TLS 1.3 resolution, got %+v", res)
	}

	// Wrong ExpectedHostID must fail closed (MITM simulation).
	_, err = client.ResolveCLITargetAndClient(context.Background(), target, client.CLITLSOptions{
		KnownHosts:     kh,
		ExpectedHostID: "0000000000000000000000000000000000000000000000000000000000000000",
		RequireTLS:     true,
	})
	if !errors.Is(err, client.ErrPeerHostKeyMismatch) {
		t.Fatalf("expected ErrPeerHostKeyMismatch, got %v", err)
	}

	// Pinning a forged key for this HostID in known_hosts must fail closed with ErrHostMismatch.
	if err := kh.PinWithAddress(hostID.HostID, "Host", "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", target.Host); err != nil {
		t.Fatal(err)
	}
	_, err = client.ResolveCLITargetAndClient(context.Background(), target, client.CLITLSOptions{
		KnownHosts: kh,
		RequireTLS: true,
	})
	if !errors.Is(err, client.ErrHostMismatch) {
		t.Fatalf("expected ErrHostMismatch, got %v", err)
	}
}

func TestResolveCLITargetAndClient_RefusesDowngradeOnPlainHTTPServer(t *testing.T) {
	srv, _, tok := startCLITestServer(t, false)
	kh, err := client.LoadOrCreateKnownHosts(filepath.Join(t.TempDir(), "known_hosts.json"))
	if err != nil {
		t.Fatal(err)
	}

	// 1. RequireTLS=true against unencrypted peer must fail closed.
	httpTarget, _ := client.ParseTarget(srv.URL()+"/#"+tok, "")
	_, err = client.ResolveCLITargetAndClient(context.Background(), httpTarget, client.CLITLSOptions{
		KnownHosts: kh,
		RequireTLS: true,
	})
	if !errors.Is(err, client.ErrTLSRequired) {
		t.Fatalf("expected ErrTLSRequired, got %v", err)
	}

	// 2. Explicit https:// URL against unencrypted peer must fail closed (never downgrade to http://).
	httpsTarget, _ := client.ParseTarget("https://"+srv.Addr().String()+"/#"+tok, "")
	_, err = client.ResolveCLITargetAndClient(context.Background(), httpsTarget, client.CLITLSOptions{
		KnownHosts: kh,
		RequireTLS: false,
	})
	if !errors.Is(err, client.ErrTLSRequired) {
		t.Fatalf("expected ErrTLSRequired on https:// downgrade attempt, got %v", err)
	}

	// 3. Previously TLS-pinned address suddenly serving plaintext HTTP must fail closed (anti-downgrade).
	if err := kh.PinWithAddress("abcd", "Prior TLS Host", "abcd", httpTarget.Host); err != nil {
		t.Fatal(err)
	}
	_, err = client.ResolveCLITargetAndClient(context.Background(), httpTarget, client.CLITLSOptions{
		KnownHosts: kh,
		RequireTLS: false,
	})
	if !errors.Is(err, client.ErrTLSRequired) {
		t.Fatalf("expected ErrTLSRequired for previously pinned address downgrade, got %v", err)
	}
}
