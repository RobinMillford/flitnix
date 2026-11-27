package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestLocalHTTPSCoexistsWithPeerTLSAndPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("transfer.NewStore: %v", err)
	}
	hostID, err := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "host.json"))
	if err != nil {
		t.Fatalf("HostIdentity: %v", err)
	}
	localHTTPS := NewLocalHTTPSManager(filepath.Join(dir, "local_ca"))
	sessMgr := session.NewManager(session.Config{})

	srv := New(Config{
		ListenAddr:        "127.0.0.1:0",
		LocalHTTPSEnabled: true,
		Services: &Services{
			Sessions:     sessMgr,
			Transfers:    transfer.NewService(transfer.Config{Store: store}),
			Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
			Registry:     transfer.NewRegistry(),
			Downloads:    transfer.NewDownloadRegistry(),
			HostIdentity: hostID,
			LocalHTTPS:   localHTTPS,
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

	if !srv.LocalHTTPSActive() {
		t.Fatal("expected LocalHTTPSActive() to be true")
	}

	// 1. Plain HTTP remains functional on the exact same port (zero-friction QR compatibility).
	httpResp, err := http.Get(srv.URL() + "/healthz")
	if err != nil {
		t.Fatalf("plain HTTP /healthz: %v", err)
	}
	httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("plain HTTP status = %d, want 200", httpResp.StatusCode)
	}

	// 2. Public CA certificate endpoint returns valid CA PEM (never private key).
	caResp, err := http.Get(srv.URL() + "/local-ca.crt")
	if err != nil {
		t.Fatalf("GET /local-ca.crt: %v", err)
	}
	caPEM, _ := io.ReadAll(caResp.Body)
	caResp.Body.Close()
	if caResp.StatusCode != http.StatusOK {
		t.Fatalf("/local-ca.crt status = %d, want 200", caResp.StatusCode)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to parse downloaded Local Root CA PEM")
	}

	// 3. Browser HTTPS verified via Local Root CA works on 127.0.0.1 AND <host>.local Host header!
	browserClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS13,
				RootCAs:    pool,
			},
		},
	}
	httpsResp, err := browserClient.Get(srv.HTTPSURL() + "/healthz")
	if err != nil {
		t.Fatalf("browser HTTPS /healthz failed: %v", err)
	}
	httpsResp.Body.Close()
	if httpsResp.StatusCode != http.StatusOK {
		t.Fatalf("browser HTTPS status = %d, want 200", httpsResp.StatusCode)
	}

	// Test .local mDNS Host header validation (allowed for own host, rejected for rogue .local).
	mdnsHost := LocalMDNSHost(hostID.HostID)
	reqMDNS, _ := http.NewRequest(http.MethodGet, srv.HTTPSURL()+"/healthz", nil)
	reqMDNS.Host = mdnsHost + ":43821"
	respMDNS, err := browserClient.Do(reqMDNS)
	if err != nil {
		t.Fatalf("HTTPS with own .local Host failed: %v", err)
	}
	respMDNS.Body.Close()
	if respMDNS.StatusCode != http.StatusOK {
		t.Fatalf("own .local Host status = %d, want 200", respMDNS.StatusCode)
	}

	reqRogue, _ := http.NewRequest(http.MethodGet, srv.HTTPSURL()+"/healthz", nil)
	reqRogue.Host = "evil-attacker.local:43821"
	respRogue, err := browserClient.Do(reqRogue)
	if err != nil {
		t.Fatalf("HTTPS with rogue .local Host error: %v", err)
	}
	respRogue.Body.Close()
	if respRogue.StatusCode != http.StatusBadRequest {
		t.Fatalf("rogue .local Host must be rejected with 400, got %d", respRogue.StatusCode)
	}

	// 4. Desktop/CLI peer SNI still receives the Ed25519 HostIdentity leaf cert!
	peerConn, err := tls.Dial("tcp", srv.Addr().String(), &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         hostID.HostID + ".flitnix.peer",
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("peer SNI TLS dial failed: %v", err)
	}
	peerState := peerConn.ConnectionState()
	_ = peerConn.Close()
	if len(peerState.PeerCertificates) == 0 || peerState.PeerCertificates[0].Subject.CommonName != hostID.HostID {
		t.Fatalf("expected Ed25519 HostIdentity peer cert for peer SNI, got %+v", peerState.PeerCertificates)
	}

	// 5. Removing Local CA deletes CA files cleanly.
	if err := localHTTPS.RemoveCA(); err != nil {
		t.Fatalf("RemoveCA: %v", err)
	}
	if localHTTPS.HasCA() {
		t.Fatal("expected HasCA() to be false after RemoveCA")
	}
}
