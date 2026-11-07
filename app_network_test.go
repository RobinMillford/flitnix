package main

import (
	"net"
	"testing"

	"github.com/itsyamin/flitnix/internal/netenv"
)

func TestAppNetworkStateAndManualRefresh(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp failed: %v", err)
	}

	app.source = fakeSource(
		[]net.Interface{{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast}},
		map[string][]net.Addr{"wlo1": {makeIPNet("192.168.1.50", 24)}},
	)

	st, err := app.RefreshNetwork()
	if err != nil {
		t.Fatalf("RefreshNetwork failed: %v", err)
	}

	if st.Status != netenv.StatusConnected {
		t.Fatalf("expected StatusConnected, got %s", st.Status)
	}
	if st.IP != "192.168.1.50" || st.InterfaceName != "wlo1" {
		t.Fatalf("unexpected fields in NetworkState: %+v", st)
	}

	cached := app.GetNetworkState()
	if cached.IP != "192.168.1.50" {
		t.Fatalf("GetNetworkState returned stale data: %+v", cached)
	}
}

func TestAppActiveSessionSurvivesNetworkSwitchAndRebinds(t *testing.T) {
	a := newTestApp(t)
	lip := lanIPv4(t)
	probeLANPort(t, lip)

	a.source = fakeSource(
		[]net.Interface{upIf("flitbr0")},
		map[string][]net.Addr{"flitbr0": {&net.IPNet{
			IP:   net.ParseIP(lip),
			Mask: net.CIDRMask(24, 32),
		}}},
	)

	// 1. Start session on Wi-Fi A (lip)
	sv, err := a.StartSession(lip)
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	sessionKey := sv.Key
	sessionToken := sv.Token

	wantListen := net.JoinHostPort(lip, "43821")
	if a.ServerAddress() != wantListen {
		t.Fatalf("expected ServerAddress %s, got %s", wantListen, a.ServerAddress())
	}

	// Verify QR code generated for Wi-Fi A
	qrA, err := a.SessionQR(sessionKey)
	if err != nil || qrA == "" {
		t.Fatalf("failed to generate QR on network A: %v", err)
	}

	// 2. Disconnect Wi-Fi A: 0 network interfaces
	a.source = fakeSource(nil, nil)
	stDisc, err := a.RefreshNetwork()
	if err != nil {
		t.Fatalf("RefreshNetwork on disconnect returned unexpected error: %v", err)
	}
	if stDisc.Status != netenv.StatusNoNetwork {
		t.Fatalf("expected StatusNoNetwork on disconnect, got %s", stDisc.Status)
	}
	if a.ServerAddress() != "" {
		t.Fatalf("expected empty ServerAddress when disconnected, got %s", a.ServerAddress())
	}

	// Session must NOT be revoked!
	if a.sessions.Len() != 1 {
		t.Fatalf("session should be preserved during disconnect, got len=%d", a.sessions.Len())
	}

	// 3. Reconnect to network: interface comes back
	a.source = fakeSource(
		[]net.Interface{upIf("flitbr0")},
		map[string][]net.Addr{"flitbr0": {&net.IPNet{
			IP:   net.ParseIP(lip),
			Mask: net.CIDRMask(24, 32),
		}}},
	)

	stConn, err := a.RefreshNetwork()
	if err != nil {
		t.Fatalf("RefreshNetwork on reconnect failed: %v", err)
	}
	if stConn.Status != netenv.StatusConnected {
		t.Fatalf("expected StatusConnected on reconnect, got %s", stConn.Status)
	}
	if stConn.IP != lip {
		t.Fatalf("expected IP %s, got %s", lip, stConn.IP)
	}

	// Server must have safely rebound to the address!
	if a.ServerAddress() != wantListen {
		t.Fatalf("expected server rebound to %s, got %s", wantListen, a.ServerAddress())
	}

	// QR code must now reflect the URL while keeping the SAME session token
	qrB, err := a.SessionQR(sessionKey)
	if err != nil || qrB == "" {
		t.Fatalf("failed to generate QR on reconnect: %v", err)
	}

	// Verify session token is STILL VALID on the newly rebound server!
	activeSessions := a.sessions.ActiveSessions()
	if len(activeSessions) != 1 {
		t.Fatalf("expected 1 active session, got %d", len(activeSessions))
	}
	if activeSessions[0].Token() != sessionToken {
		t.Fatalf("session token was unexpectedly rotated across rebind")
	}

	// Clean stop
	_ = a.EndSession(sessionKey)
}

func TestAppSecondaryInterfaceChangeDoesNotInterruptSession(t *testing.T) {
	a := newTestApp(t)
	lip := lanIPv4(t)
	probeLANPort(t, lip)

	// Start with primary Wi-Fi interface
	a.source = fakeSource(
		[]net.Interface{upIf("flitbr0")},
		map[string][]net.Addr{"flitbr0": {&net.IPNet{
			IP:   net.ParseIP(lip),
			Mask: net.CIDRMask(24, 32),
		}}},
	)

	sv, err := a.StartSession(lip)
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	defer a.EndSession(sv.Key)

	wantListen := net.JoinHostPort(lip, "43821")
	if a.ServerAddress() != wantListen {
		t.Fatalf("expected initial listen %s, got %s", wantListen, a.ServerAddress())
	}

	// Secondary interface appears (e.g. docker0 or secondary ethernet)
	a.source = fakeSource(
		[]net.Interface{
			upIf("docker0"),
			upIf("flitbr0"),
		},
		map[string][]net.Addr{
			"docker0": {makeIPNet("172.17.0.1", 16)},
			"flitbr0": {&net.IPNet{
				IP:   net.ParseIP(lip),
				Mask: net.CIDRMask(24, 32),
			}},
		},
	)

	st, err := a.RefreshNetwork()
	if err != nil {
		t.Fatalf("RefreshNetwork failed: %v", err)
	}

	// Sticky selection must keep flitbr0 and must NOT rebind the server
	if st.SelectedInterface != "flitbr0" || st.IP != lip {
		t.Fatalf("expected sticky selection to preserve flitbr0 (%s), got %s (%s)", lip, st.SelectedInterface, st.IP)
	}
	if a.ServerAddress() != wantListen {
		t.Fatalf("server address changed unexpectedly: want %s, got %s", wantListen, a.ServerAddress())
	}
	if a.sessions.Len() != 1 {
		t.Fatalf("expected active session to remain untouched, got len=%d", a.sessions.Len())
	}
}

func makeIPNet(ipStr string, maskBits int) *net.IPNet {
	ip := net.ParseIP(ipStr)
	return &net.IPNet{
		IP:   ip,
		Mask: net.CIDRMask(maskBits, 32),
	}
}
