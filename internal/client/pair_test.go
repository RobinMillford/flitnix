package client

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestClientPairEndToEnd(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Setup Server
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: filepath.Join(tempDir, "transfers")})
	if err != nil {
		t.Fatal(err)
	}
	devStore, err := devices.NewStore(filepath.Join(tempDir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	hostIdentity, err := devices.LoadOrCreateHostIdentity(filepath.Join(tempDir, "host_identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	pairingCoord := devices.NewPairingCoordinator(hostIdentity, devStore)
	pairingCoord.OnPairingPrompt = func(deviceName, clientID, sasCode string, decisionChan chan<- bool) {
		decisionChan <- true
	}
	sessMgr := session.NewManager(session.Config{})
	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}

	svcs := &server.Services{
		Sessions:     sessMgr,
		Transfers:    transfer.NewService(transfer.Config{Store: store}),
		Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:     transfer.NewRegistry(),
		Downloads:    transfer.NewDownloadRegistry(),
		Devices:      devStore,
		HostIdentity: hostIdentity,
		Pairing:      pairingCoord,
	}

	srv := server.New(server.Config{Services: svcs})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 2. Setup Client
	target, err := ParseTarget(ts.URL+"/#"+sess.Token(), "")
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}

	clientID, err := LoadOrCreateClientIdentity(filepath.Join(tempDir, "client_identity.json"))
	if err != nil {
		t.Fatalf("LoadOrCreateClientIdentity: %v", err)
	}
	kh, err := LoadOrCreateKnownHosts(filepath.Join(tempDir, "known_hosts.json"))
	if err != nil {
		t.Fatalf("LoadOrCreateKnownHosts: %v", err)
	}

	var observedSAS string
	res, err := Pair(context.Background(), target, "Alice CLI Laptop", clientID, kh, func(sas string, isPinned bool) (bool, error) {
		observedSAS = sas
		return true, nil
	})
	if err != nil {
		t.Fatalf("Pair failed: %v", err)
	}

	if res.HostID != hostIdentity.HostID {
		t.Errorf("HostID mismatch: got %s, want %s", res.HostID, hostIdentity.HostID)
	}
	if res.DeviceID != clientID.DeviceID {
		t.Errorf("DeviceID mismatch: got %s, want %s", res.DeviceID, clientID.DeviceID)
	}
	if observedSAS == "" || res.SASDisplay != observedSAS {
		t.Errorf("SASDisplay mismatch: got %s, observed %s", res.SASDisplay, observedSAS)
	}

	// Verify host was pinned in known_hosts
	pinned, ok := kh.Get(hostIdentity.HostID)
	if !ok || pinned.PublicKey != hostIdentity.PublicKey {
		t.Fatalf("host was not pinned properly: %+v", pinned)
	}

	// Verify device exists on host in devices.Store
	dev, ok := devStore.Get(clientID.DeviceID)
	if !ok || dev.Name != "Alice CLI Laptop" {
		t.Fatalf("device not found on host store: %+v", dev)
	}
}

func TestClientPairTOFUAlertOnKeyMismatch(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Setup Server
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: filepath.Join(tempDir, "transfers")})
	devStore, _ := devices.NewStore(filepath.Join(tempDir, "devices.json"))
	hostIdentity, _ := devices.LoadOrCreateHostIdentity(filepath.Join(tempDir, "host_identity.json"))
	pairingCoord := devices.NewPairingCoordinator(hostIdentity, devStore)
	pairingCoord.OnPairingPrompt = func(deviceName, clientID, sasCode string, decisionChan chan<- bool) {
		decisionChan <- true
	}
	sessMgr := session.NewManager(session.Config{})
	sess, _ := sessMgr.Create()

	svcs := &server.Services{
		Sessions:     sessMgr,
		Transfers:    transfer.NewService(transfer.Config{Store: store}),
		Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:     transfer.NewRegistry(),
		Downloads:    transfer.NewDownloadRegistry(),
		Devices:      devStore,
		HostIdentity: hostIdentity,
		Pairing:      pairingCoord,
	}

	srv := server.New(server.Config{Services: svcs})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	target, _ := ParseTarget(ts.URL+"/#"+sess.Token(), "")
	clientID, _ := LoadOrCreateClientIdentity(filepath.Join(tempDir, "client_identity.json"))
	kh, _ := LoadOrCreateKnownHosts(filepath.Join(tempDir, "known_hosts.json"))

	// Pre-pin a DIFFERENT public key for this HostID in known_hosts.json
	bogusPub, _, _ := ed25519.GenerateKey(nil)
	_ = kh.Pin(hostIdentity.HostID, "Bob Desktop", hex.EncodeToString(bogusPub))

	// Pairing attempt must fail immediately with ErrHostMismatch (TOFU alert)
	_, err := Pair(context.Background(), target, "Alice Phone", clientID, kh, func(sas string, isPinned bool) (bool, error) {
		return true, nil
	})
	if err == nil || !errors.Is(err, ErrHostMismatch) {
		t.Fatalf("expected ErrHostMismatch, got: %v", err)
	}
}

func TestClientPairUserAbortsOnSASMismatch(t *testing.T) {
	tempDir := t.TempDir()

	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: filepath.Join(tempDir, "transfers")})
	devStore, _ := devices.NewStore(filepath.Join(tempDir, "devices.json"))
	hostIdentity, _ := devices.LoadOrCreateHostIdentity(filepath.Join(tempDir, "host_identity.json"))
	pairingCoord := devices.NewPairingCoordinator(hostIdentity, devStore)
	pairingCoord.OnPairingPrompt = func(deviceName, clientID, sasCode string, decisionChan chan<- bool) {
		decisionChan <- true
	}
	sessMgr := session.NewManager(session.Config{})
	sess, _ := sessMgr.Create()

	svcs := &server.Services{
		Sessions:     sessMgr,
		Transfers:    transfer.NewService(transfer.Config{Store: store}),
		Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:     transfer.NewRegistry(),
		Downloads:    transfer.NewDownloadRegistry(),
		Devices:      devStore,
		HostIdentity: hostIdentity,
		Pairing:      pairingCoord,
	}

	srv := server.New(server.Config{Services: svcs})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	target, _ := ParseTarget(ts.URL+"/#"+sess.Token(), "")
	clientID, _ := LoadOrCreateClientIdentity(filepath.Join(tempDir, "client_identity.json"))
	kh, _ := LoadOrCreateKnownHosts(filepath.Join(tempDir, "known_hosts.json"))

	// User enters "n" (declines SAS)
	_, err := Pair(context.Background(), target, "Alice Phone", clientID, kh, func(sas string, isPinned bool) (bool, error) {
		return false, nil // Rejection!
	})
	if !errors.Is(err, ErrPairingAborted) {
		t.Fatalf("expected ErrPairingAborted, got: %v", err)
	}

	// Ensure host did NOT enroll device
	if _, ok := devStore.Get(clientID.DeviceID); ok {
		t.Fatal("device was enrolled despite user rejection")
	}

	// Ensure host was NOT pinned in client known_hosts
	if _, ok := kh.Get(hostIdentity.HostID); ok {
		t.Fatal("host was pinned despite user rejection")
	}
}
