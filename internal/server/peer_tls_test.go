package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestSinglePortHTTPAndTLS13Multiplexing(t *testing.T) {
	dir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("transfer.NewStore: %v", err)
	}
	devStore, err := devices.NewStore("")
	if err != nil {
		t.Fatalf("devices.NewStore: %v", err)
	}
	hostID, err := devices.LoadOrCreateHostIdentity(dir + "/host.json")
	if err != nil {
		t.Fatalf("HostIdentity: %v", err)
	}
	sessMgr := session.NewManager(session.Config{})
	vis := devices.NewVisibilityManager(devices.VisibilityState{
		Mode:                    devices.VisibilityEveryone,
		PreviousRestrictiveMode: devices.VisibilityTrustedOnly,
	}, time.Now)

	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services: &server.Services{
			Sessions:     sessMgr,
			ShortCodes:   session.NewShortCodeManager(session.ShortCodeConfig{}),
			Transfers:    transfer.NewService(transfer.Config{Store: store}),
			Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
			Registry:     transfer.NewRegistry(),
			Downloads:    transfer.NewDownloadRegistry(),
			Devices:      devStore,
			HostIdentity: hostID,
			Visibility:   vis,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("server failed to start")
	}

	addr := srv.Addr().String()

	// 1. Plain HTTP mobile browser requests succeed on the same listener port.
	httpResp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("plain HTTP /healthz failed: %v", err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("plain HTTP status = %d, want 200", httpResp.StatusCode)
	}

	// 2. TLS 1.3 desktop peer probe succeeds and returns exact verified Ed25519 HostID/PublicKey.
	probe, err := client.ProbePeerTLS(ctx, addr)
	if err != nil {
		t.Fatalf("ProbePeerTLS: %v", err)
	}
	if !probe.TLSActive || probe.HostID != hostID.HostID || probe.PublicKey != hostID.PublicKey {
		t.Fatalf("unexpected TLS probe: %+v", probe)
	}

	// 3. Mismatched pinned HostID fails closed immediately during TLS handshake.
	badClient := client.NewPeerHTTPClient("0000000000000000000000000000000000000000000000000000000000000000", "", nil)
	if _, err := badClient.Get("https://" + addr + "/healthz"); err == nil {
		t.Fatal("expected pinned TLS handshake to fail closed on hostId mismatch")
	}
}
