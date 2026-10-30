package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
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

type pairCLITestServer struct {
	srv      *server.Server
	sess     *session.Session
	coord    *devices.PairingCoordinator
	devStore *devices.Store
	hostID   *devices.HostIdentity
	url      string
}

func setupPairCLIServer(t *testing.T) *pairCLITestServer {
	t.Helper()
	dir := t.TempDir()

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: filepath.Join(dir, "transfers")})
	if err != nil {
		t.Fatal(err)
	}
	devStore, err := devices.NewStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	hostIdentity, err := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "host_identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	coord := devices.NewPairingCoordinator(hostIdentity, devStore)
	coord.OnPairingPrompt = func(deviceName, clientID, sasCode string, decisionChan chan<- bool) {
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
		Pairing:      coord,
	}

	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = srv.Serve(ctx) }()

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("server start timeout")
	}

	pairingURL := srv.URL() + "/#" + sess.Token()

	return &pairCLITestServer{
		srv:      srv,
		sess:     sess,
		coord:    coord,
		devStore: devStore,
		hostID:   hostIdentity,
		url:      pairingURL,
	}
}

func TestPairCLIFirstContactYesRefused(t *testing.T) {
	s := setupPairCLIServer(t)
	clientConfigDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", clientConfigDir)

	var stdout, stderr bytes.Buffer
	args := []string{"--to", s.url, "--yes"}
	code := runPairCLIWithStdin(args, nil, &stdout, &stderr)

	if code != ExitAuthError {
		t.Fatalf("expected ExitAuthError (%d) when --yes used on first contact, got: %d", ExitAuthError, code)
	}

	if !strings.Contains(stderr.String(), "first-contact pairing requires interactive visual SAS verification") {
		t.Fatalf("expected SAS verification requirement message in stderr, got: %s", stderr.String())
	}

	// Verify host was NOT pinned
	khPath := filepath.Join(clientConfigDir, "flitnix", "known_hosts.json")
	if data, err := os.ReadFile(khPath); err == nil && len(data) > 0 {
		var kh client.KnownHosts
		_ = json.Unmarshal(data, &kh)
		if len(kh.Hosts) > 0 {
			t.Fatalf("known_hosts must not contain any hosts after failed first contact: %+v", kh.Hosts)
		}
	}
}

func TestPairCLIFirstContactJSONInteractionRequired(t *testing.T) {
	s := setupPairCLIServer(t)
	clientConfigDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", clientConfigDir)

	var stdout, stderr bytes.Buffer
	args := []string{"--to", s.url, "--json"}
	code := runPairCLIWithStdin(args, nil, &stdout, &stderr)

	if code != ExitAuthError {
		t.Fatalf("expected ExitAuthError (%d) when --json used on first contact, got: %d", ExitAuthError, code)
	}

	var res map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("expected valid JSON error response on stdout: %v, got: %s", err, stdout.String())
	}

	if res["error"] != "interaction_required" {
		t.Fatalf("expected error='interaction_required', got: %v", res["error"])
	}
	if res["success"] != false {
		t.Fatalf("expected success=false, got: %v", res["success"])
	}
}

func TestPairCLIPinnedHostAllowsYesAndJSON(t *testing.T) {
	s := setupPairCLIServer(t)
	clientConfigDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", clientConfigDir)

	// Pre-pin the server's legitimate public key
	khPath := filepath.Join(clientConfigDir, "flitnix", "known_hosts.json")
	kh, err := client.LoadOrCreateKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := kh.Pin(s.hostID.HostID, "Legitimate Host", s.hostID.PublicKey); err != nil {
		t.Fatal(err)
	}

	// Now run pair with --yes and --json on previously pinned host -> should succeed non-interactively!
	var stdout, stderr bytes.Buffer
	args := []string{"--to", s.url, "--yes", "--json", "--name", "Pinned Laptop"}
	code := runPairCLIWithStdin(args, nil, &stdout, &stderr)

	if code != ExitSuccess {
		t.Fatalf("expected ExitSuccess (%d) for pinned host with --yes, got %d (stderr: %s)", ExitSuccess, code, stderr.String())
	}

	var res client.PairResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("expected valid JSON PairResult on stdout: %v, got: %s", err, stdout.String())
	}

	if res.HostID != s.hostID.HostID {
		t.Fatalf("host ID mismatch: got %s, want %s", res.HostID, s.hostID.HostID)
	}

	// Verify device exists on host store
	dev, ok := s.devStore.Get(res.DeviceID)
	if !ok || dev.Name != "Pinned Laptop" {
		t.Fatalf("device not found on host store: %+v", dev)
	}
}

func TestPairCLIPinnedHostRejectsKeyChange(t *testing.T) {
	s := setupPairCLIServer(t)
	clientConfigDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", clientConfigDir)

	// Pre-pin an IMPOSTER public key for this HostID
	khPath := filepath.Join(clientConfigDir, "flitnix", "known_hosts.json")
	kh, err := client.LoadOrCreateKnownHosts(khPath)
	if err != nil {
		t.Fatal(err)
	}
	imposterPub, _, _ := ed25519.GenerateKey(nil)
	if err := kh.Pin(s.hostID.HostID, "Old Host Key", hex.EncodeToString(imposterPub)); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{"--to", s.url}
	code := runPairCLIWithStdin(args, nil, &stdout, &stderr)

	if code != ExitAuthError {
		t.Fatalf("expected ExitAuthError on TOFU violation, got: %d", code)
	}

	if !strings.Contains(stderr.String(), "CRITICAL SECURITY ALERT") {
		t.Fatalf("expected security alert in stderr, got: %s", stderr.String())
	}
}
