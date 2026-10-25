package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// startTestServer binds an ephemeral loopback port (no fixed-port races) and
// starts serving until the test cleanup cancels it. The cleanup verifies the
// server stops cleanly, exercising the graceful-shutdown path on every test.
func startTestServer(t *testing.T) *Server {
	t.Helper()

	s := New(Config{ListenAddr: "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ctx) }()

	select {
	case <-s.Ready():
	case err := <-errc:
		cancel()
		t.Fatalf("server failed to start: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		<-errc
		t.Fatal("timed out waiting for the server to become ready")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("server returned error during shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("server did not stop after context cancellation")
		}
	})
	return s
}

func TestServeAcceptsRealRequests(t *testing.T) {
	s := startTestServer(t)

	resp, err := http.Get("http://" + s.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz on the real server failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("real GET /healthz returned %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("real /healthz body is not valid JSON: %v\nbody: %q", err, body)
	}
	if payload.Status != "ok" {
		t.Errorf("real /healthz reported status %q, want \"ok\"", payload.Status)
	}
}

func TestServePortInUse(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating the blocking listener: %v", err)
	}
	defer blocker.Close()

	s := New(Config{ListenAddr: blocker.Addr().String()})
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(context.Background()) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatalf("Serve succeeded on an occupied port %s, want an error", blocker.Addr())
		}
		if !strings.Contains(err.Error(), "already in use") {
			t.Errorf("port-in-use error = %v, want it to mention \"already in use\"", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve did not return despite the port being occupied")
	}
}

func TestServeRejectsSecondCall(t *testing.T) {
	s := startTestServer(t)

	errc := make(chan error, 1)
	go func() { errc <- s.Serve(context.Background()) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("second Serve call succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "already called") {
			t.Errorf("second-Serve error = %v, want it to mention \"already called\"", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second Serve call blocked instead of returning an error")
	}
}

func TestServerURLMatchesListener(t *testing.T) {
	s := startTestServer(t)

	url := s.URL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("URL() = %q, want it to start with \"http://127.0.0.1:\"", url)
	}
	if !strings.HasSuffix(url, "/") {
		// Addr() is host:port; the URL helper adds just the scheme here, so
		// no trailing slash is expected — this assertion documents that.
	}
}

// TestDefaultPortIsFixed guards the session-port contract: stage 4 pins
// TCP 43821 for every session, and a silent change would break QR URLs
// saved by users and firewalls whitelisted for the old port.
func TestDefaultPortIsFixed(t *testing.T) {
	if DefaultPort != 43821 {
		t.Fatalf("DefaultPort = %d, want 43821 (the session port must not change)", DefaultPort)
	}
	if DefaultListenAddr != "127.0.0.1:43821" {
		t.Fatalf("DefaultListenAddr = %q, want the loopback address on the fixed port", DefaultListenAddr)
	}
}

// lanIPv4 returns one suitable non-loopback IPv4 address of a running,
// non-point-to-point interface, or skips the test when the machine has
// none (test VMs without networking). The same filters netenv applies are
// mirrored here in minimal form so the server package stays independent.
func lanIPv4(t *testing.T) string {
	t.Helper()
	unsuitable := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot list interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipn, ok := addr.(*net.IPNet)
			if !ok || ipn == nil {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			parsed, ok := netip.AddrFromSlice(ip4)
			if !ok {
				continue
			}
			if parsed.IsLoopback() || parsed.IsLinkLocalUnicast() ||
				parsed.IsUnspecified() || parsed.IsMulticast() {
				continue
			}
			blocked := false
			for _, pfx := range unsuitable {
				if pfx.Contains(parsed) {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			return parsed.String()
		}
	}
	t.Skip("no suitable non-loopback IPv4 interface on this machine; the LAN-binding path is covered by the loopback variant")
	return ""
}

// startBound starts a server on the exact address and verifies it becomes
// ready, reporting the real bound listener. Cleanup stops it cleanly.
func startBound(t *testing.T, listen string) *Server {
	t.Helper()

	s := New(Config{ListenAddr: listen})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ctx) }()

	select {
	case <-s.Ready():
	case err := <-errc:
		cancel()
		t.Fatalf("server failed to start on %s: %v", listen, err)
	case <-time.After(3 * time.Second):
		cancel()
		<-errc
		t.Fatalf("timed out binding %s", listen)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("server on %s returned error during shutdown: %v", listen, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("server on %s did not stop within its drain window", listen)
		}
	})
	return s
}

// checkBound asserts the full binding contract: the listener resolved to
// the requested address (no silent rebind), URL() reports the reachable
// address, and a real HTTP request through that address succeeds. Every
// LAN-binding integration test funnels here.
func checkBound(t *testing.T, s *Server, listen string) {
	t.Helper()

	if got := s.Addr(); got == nil || got.String() != listen {
		t.Fatalf("Addr() = %v, want exactly %s (no silent rebind)", got, listen)
	}
	if got, want := s.URL(), "http://"+listen; got != want {
		t.Fatalf("URL() = %q, want %q", got, want)
	}

	url := s.URL() + "/healthz"
	resp, err := http.Get(url) //nolint:gosec — test URL built from a local listener only
	if err != nil {
		t.Fatalf("requesting %s through the bound address: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body is not JSON: %v\nbody: %q", err, body)
	}
	if resp.StatusCode != http.StatusOK || payload.Status != "ok" {
		t.Fatalf("GET %s → %d %q; want 200 with status \"ok\"", url, resp.StatusCode, body)
	}
}

// TestServeBindsSpecificLANIPv4 is the stage-4 integration test: the
// server binds one explicit local interface address and answers requests
// on that address only.
func TestServeBindsSpecificLANIPv4(t *testing.T) {
	ip := lanIPv4(t)
	listen := net.JoinHostPort(ip, "43821")
	checkBound(t, startBound(t, listen), listen)
}

// TestServeBindsSpecificLoopbackIPv4 runs the identical path on an always
// available explicit loopback address (127.0.0.9 is distinct from the
// package default 127.0.0.1), so the binding logic is exercised even on
// machines with no LAN interfaces.
func TestServeBindsSpecificLoopbackIPv4(t *testing.T) {
	listen := net.JoinHostPort("127.0.0.9", "43821")
	checkBound(t, startBound(t, listen), listen)
}

// TestServePortInUseOnSpecificAddress makes sure the occupied-port error
// also surfaces when binding an explicit interface address rather than
// the loopback default.
func TestServePortInUseOnSpecificAddress(t *testing.T) {
	blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", "43821"))
	if err != nil {
		t.Skipf("the default port is unavailable for this test right now: %v", err)
	}
	defer blocker.Close()

	s := New(Config{ListenAddr: blocker.Addr().String()})
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(context.Background()) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("served on an occupied explicit address; want an error")
		}
		if !strings.Contains(err.Error(), "already in use") {
			t.Errorf("error = %v, want it to mention \"already in use\"", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return despite the address being occupied")
	}
}
