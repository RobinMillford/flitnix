package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// ---- synthetic-network fakes (mirrors of the netenv test helpers) ----

func fakeSource(ifaces []net.Interface, addrs map[string][]net.Addr) netenv.Source {
	return netenv.Source{
		Interfaces: func() ([]net.Interface, error) { return ifaces, nil },
		Addrs: func(ic net.Interface) ([]net.Addr, error) {
			return addrs[ic.Name], nil
		},
	}
}

func ipnet24(a, b, c, d byte) net.Addr {
	ip := net.IPv4(a, b, c, d)
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(24, 32)}
}

func upIf(name string) net.Interface {
	return net.Interface{
		Index:        1,
		MTU:          1500,
		Name:         name,
		Flags:        net.FlagUp | net.FlagBroadcast | net.FlagMulticast,
		HardwareAddr: []byte{0x02, 0x11, 0x22, 0x33, 0x44, 0x55},
	}
}

// newTestApp builds a fully wired App whose runtime context is ready and
// whose network source is injectable. tests re-point a.source at fakes.
func newTestApp(t *testing.T) *App {
	t.Helper()
	a, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	a.startup(context.Background())
	t.Cleanup(a.stopServer)
	return a
}

// tempSharedFile creates one real regular file for registry.Add, which
// lstats its argument.
func tempSharedFile(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := dir + "/" + name
	if err := os.WriteFile(p, []byte("hello flitnix stage 4"), 0o600); err != nil {
		t.Fatalf("temp file: %v", err)
	}
	return p
}

// ---- resolveInterface decision table ----

func TestResolveInterfaceAutoPicksSingleInterface(t *testing.T) {
	a := newTestApp(t)
	a.source = fakeSource(
		[]net.Interface{upIf("flitbr0")},
		map[string][]net.Addr{"flitbr0": {ipnet24(192, 168, 44, 10)}},
	)
	v, err := a.resolveInterface("")
	if err != nil {
		t.Fatalf("resolveInterface: %v", err)
	}
	if v.Name != "flitbr0" || v.IP != "192.168.44.10" {
		t.Fatalf("got %+v, want flitbr0/192.168.44.10", v)
	}
}

func TestResolveInterfaceAmbiguousNamesCandidates(t *testing.T) {
	a := newTestApp(t)
	a.source = fakeSource(
		[]net.Interface{upIf("eth0"), upIf("wlx01")},
		map[string][]net.Addr{
			"eth0":  {ipnet24(10, 0, 0, 5)},
			"wlx01": {ipnet24(192, 168, 1, 9)},
		},
	)
	_, err := a.resolveInterface("")
	if err == nil {
		t.Fatal("two interfaces with empty ip must fail, ask the user to choose")
	}
	msg := err.Error()
	for _, want := range []string{"choose which network interface", "eth0", "wlx01"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestResolveInterfaceNone(t *testing.T) {
	a := newTestApp(t)
	a.source = fakeSource(nil, nil)
	_, err := a.resolveInterface("")
	if err == nil {
		t.Fatal("no interfaces must fail")
	}
	if !strings.Contains(err.Error(), "no usable local IPv4") {
		t.Errorf("error %q should describe the missing network", err)
	}
}

func TestResolveInterfaceNamedIPGone(t *testing.T) {
	a := newTestApp(t)
	a.source = fakeSource(
		[]net.Interface{upIf("wlo2")},
		map[string][]net.Addr{"wlo2": {ipnet24(192, 168, 0, 106)}},
	)
	// An address the phone network used to hand out but no longer does.
	_, err := a.resolveInterface("192.168.0.7")
	if err == nil {
		t.Fatal("stale address must fail")
	}
	if !strings.Contains(err.Error(), "no longer reachable") {
		t.Errorf("error %q should tell the user the network changed: %v", err, err)
	}
	// The still-present address stays valid.
	if v, lerr := a.resolveInterface("192.168.0.106"); lerr != nil || v.Name != "wlo2" {
		t.Fatalf("live address: got %+v err %v, want it to keep working", v, lerr)
	}
}

func TestResolveInterfaceSourceError(t *testing.T) {
	a := newTestApp(t)
	a.source = netenv.Source{Interfaces: func() ([]net.Interface, error) {
		return nil, errors.New("driver exploded")
	}}
	_, err := a.resolveInterface("")
	if err == nil || !strings.Contains(err.Error(), "could not read the network interfaces") {
		t.Fatalf("err = %v, want the interfaces-failure message", err)
	}
}

func TestStartSessionRequiresRuntimeContext(t *testing.T) {
	a, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	a.source = fakeSource(
		[]net.Interface{upIf("flitbr0")},
		map[string][]net.Addr{"flitbr0": {ipnet24(192, 168, 44, 10)}},
	)
	if _, err := a.StartSession(""); err == nil || !strings.Contains(err.Error(), "still starting") {
		t.Fatalf("err = %v, want the still-starting guard", err)
	}
}

// ListNetworkInterfaces must hand back exactly the enumerated usable
// addresses, unfiltered by anything product-logic-specific.
func TestListNetworkInterfaces(t *testing.T) {
	a := newTestApp(t)
	a.source = netenv.Source{
		Interfaces: func() ([]net.Interface, error) { return []net.Interface{upIf("wlo1")}, nil },
		Addrs: func(ic net.Interface) ([]net.Addr, error) {
			return []net.Addr{ipnet24(192, 168, 3, 3)}, nil
		},
		Metadata: func(ic net.Interface) netenv.Metadata {
			return netenv.Metadata{
				SSID:           "runtime hotspot",
				ConnectionName: "saved profile",
				Type:           "Wi-Fi",
			}
		},
	}
	ifaces, err := a.ListNetworkInterfaces()
	if err != nil {
		t.Fatalf("ListNetworkInterfaces: %v", err)
	}
	if len(ifaces) != 1 || ifaces[0].Name != "wlo1" || ifaces[0].IP != "192.168.3.3" ||
		ifaces[0].FriendlyName != "runtime hotspot" || ifaces[0].ConnectionName != "saved profile" {
		t.Fatalf("got %+v, want the same candidate to carry address, SSID and profile", ifaces)
	}
}

func TestAddDroppedSourcesKeepsValidSelectionWhenAnotherPathFails(t *testing.T) {
	a := newTestApp(t)
	root := t.TempDir()
	folder := filepath.Join(root, "DropFolder")
	if err := os.MkdirAll(filepath.Join(folder, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "nested", "note.txt"), []byte("note"), 0o600); err != nil {
		t.Fatal(err)
	}
	views, err := a.AddDroppedSources([]string{folder, filepath.Join(root, "missing.txt")})
	if err == nil {
		t.Fatal("mixed drop should report the missing path")
	}
	if len(views) != 1 || views[0].Kind != "folder" || views[0].RootName != "DropFolder" ||
		views[0].ItemCount != 1 {
		t.Fatalf("valid dropped folder was not retained: %+v", views)
	}
	if strings.Contains(string(mustJSON(t, views[0])), folder) {
		t.Fatal("public dropped-source view disclosed the absolute path")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// ---- server lifecycle through the adapter ----

// loopTestIP picks an uncommon loopback address: the adapter tests exercise
// real binds through addresses that no other test package's default
// (127.0.0.1 / .9) occupies, while keeping the fixed port.
const loopTestIP = "127.0.0.77"

func TestEnsureServerBindsExplicitAddress(t *testing.T) {
	a := newTestApp(t)
	listen := loopTestIP + ":43821"
	if err := a.ensureServer(listen); err != nil {
		t.Fatalf("ensureServer(%s): %v", listen, err)
	}
	if a.srv == nil {
		t.Fatal("server not running")
	}
	if got := a.srv.Addr().String(); got != listen {
		t.Fatalf("Addr() = %s, want %s (no silent rebind)", got, listen)
	}
	if got, want := a.srv.URL(), "http://"+listen; got != want {
		t.Fatalf("URL() = %q, want %q", got, want)
	}
}

func TestEnsureServerReusesSameAddress(t *testing.T) {
	a := newTestApp(t)
	listen := loopTestIP + ":43821"
	if err := a.ensureServer(listen); err != nil {
		t.Fatalf("first ensureServer: %v", err)
	}
	first := a.srv

	s, err := a.sessions.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := a.ensureServer(listen); err != nil {
		t.Fatalf("second ensureServer: %v", err)
	}
	if a.srv != first {
		t.Fatal("running server on the same address must be reused, not restarted")
	}
	// The earlier session must survive: its URL is still valid.
	if got := a.sessions.Len(); got != 1 {
		t.Fatalf("session count = %d, want the session to survive reuse", got)
	}
	if _, verr := a.sessions.Validate(s.Token()); verr != nil {
		t.Fatalf("surviving session must still validate: %v", verr)
	}
}

func TestEnsureServerDifferentAddressEndsSessions(t *testing.T) {
	a := newTestApp(t)
	first := loopTestIP + ":43821"
	second := "127.0.0.78:43821"
	if err := a.ensureServer(first); err != nil {
		t.Fatalf("first ensureServer: %v", err)
	}
	s, err := a.sessions.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sfPath := tempSharedFile(t, "note.txt")
	sfv, err := a.registry.Add(sfPath, "note.txt", s.Key())
	if err != nil {
		t.Fatalf("registry.Add: %v", err)
	}

	if err := a.ensureServer(second); err != nil {
		t.Fatalf("rebind to %s: %v", second, err)
	}
	if a.srv.Addr().String() != second {
		t.Fatalf("Addr() = %s, want the rebound %s", a.srv.Addr(), second)
	}
	if a.sessions.Len() != 0 {
		t.Fatalf("Len = %d, want 0: the old address's sessions are unreachable", a.sessions.Len())
	}
	if _, verr := a.sessions.Validate(s.Token()); verr == nil {
		t.Fatal("revoked session token must fail validation")
	}
	if _, ok := a.registry.Get(sfv.ID); ok {
		t.Fatal("per-session shared file must be unlisted after the rebind")
	}
}

func TestEnsureServerPortOccupied(t *testing.T) {
	a := newTestApp(t)
	listen := "127.0.0.79:43821"
	blocker, err := net.Listen("tcp", listen)
	if err != nil {
		t.Skipf("test address unavailable right now: %v", err)
	}
	defer blocker.Close()

	err = a.ensureServer(listen)
	if err == nil {
		t.Fatal("occupied address must fail with a clear error")
	}
	for _, want := range []string{"could not start on " + listen, "address already in use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if a.srv != nil {
		t.Fatal("failed start must not leave a server behind")
	}
}

func TestStopServerIdleSafe(t *testing.T) {
	a := newTestApp(t)
	a.stopServer() // never started: must be a no-op
	a.stopServer() // twice: still a no-op
	if a.srv != nil {
		t.Fatal("stopServer must keep the server unset")
	}
}

// ---- end-to-end adapter flow on a real interface ----

// lanIPv4 picks the machine's real usable LAN IPv4 address the same way
// netenv does (see the server package's mirror for the rule rationale), so
// the adapter test binds an address the OS actually accepts.
func lanIPv4(t *testing.T) string {
	t.Helper()
	src := netenv.DefaultSource()
	views, err := netenv.Enumerate(src)
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	if len(views) == 0 {
		t.Skip("no suitable LAN interface on this machine")
	}
	return views[0].IP
}

// probeLANPort skips the E2E when the fixed port is busy (e.g. another
// package's binding test running concurrently in go test ./...).
func probeLANPort(t *testing.T, ip string) {
	t.Helper()
	probe, err := net.Listen("tcp", net.JoinHostPort(ip, "43821"))
	if err != nil {
		t.Skipf("the session port is currently unavailable for the E2E adapter test: %v", err)
	}
	probe.Close()
}

func TestStartSessionEndToEnd(t *testing.T) {
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

	view, err := a.StartSession("")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	wantListen := net.JoinHostPort(lip, "43821")
	if got := a.srv.Addr().String(); got != wantListen {
		t.Fatalf("server bound to %s, want %s", got, wantListen)
	}
	if view.InterfaceName != "flitbr0" || view.Address != lip {
		t.Fatalf("view fields %+v/%+v, want flitbr0/%s", view.InterfaceName, view.Address, lip)
	}
	if len(view.Token) != 43 {
		t.Fatalf("token length = %d, want the 43-char base64url secret", len(view.Token))
	}
	// The listing view must never expose the token.
	for _, s := range a.ListSessions() {
		if s.Token != "" {
			t.Fatal("ListSessions leaked the raw token")
		}
		if s.Key != view.Key {
			t.Fatalf("listing key %s, want the session's %s", s.Key, view.Key)
		}
	}
	if got := a.sessions.Len(); got != 1 {
		t.Fatalf("session count = %d, want 1", got)
	}
}

func TestStartSessionSecondCallIsTheSameAddress(t *testing.T) {
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

	first, err := a.StartSession("")
	if err != nil {
		t.Fatalf("first StartSession: %v", err)
	}
	srvFirst := a.srv
	second, err := a.StartSession(lip)
	if err != nil {
		t.Fatalf("second StartSession on the live address: %v", err)
	}
	if a.srv != srvFirst {
		t.Fatal("same-address second session must not restart the server")
	}
	if a.sessions.Len() != 2 {
		t.Fatalf("session count = %d, want 2 coexisting sessions", a.sessions.Len())
	}
	if first.Key == second.Key {
		t.Fatal("two sessions must not share a key")
	}
	if second.Token == "" {
		t.Fatal("the second StartSession must surface its own token")
	}
}

// ---- Session QR ----

func TestSessionQREndToEnd(t *testing.T) {
	a := newTestApp(t)
	listen := loopTestIP + ":43821"
	if err := a.ensureServer(listen); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	s, err := a.sessions.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dataURL, err := a.SessionQR(s.Key())
	if err != nil {
		t.Fatalf("SessionQR: %v", err)
	}
	if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatalf("data URL = %q...", dataURL[:30])
	}
	raw, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, "data:image/png;base64,"))
	if derr != nil {
		t.Fatalf("decoding QR payload: %v", derr)
	}
	if !strings.Contains(string(raw[:8]), "PNG") {
		t.Fatal("QR payload is not a PNG")
	}

	// Unknown key and no-server cases give visible failures (never an
	// empty image and never a token).
	if _, err := a.SessionQR("not-a-key"); err == nil {
		t.Fatal("unknown session key must fail visibly")
	}
	a.stopServer()
	if _, err := a.SessionQR(s.Key()); err == nil || !strings.Contains(err.Error(), "no session is running") {
		t.Fatalf("err = %v, want the no-running-server message", err)
	}
}

// ---- end-of-session lifecycle coupling ----

func TestEndSessionStopsServerWhenLastLeaves(t *testing.T) {
	a := newTestApp(t)
	listen := loopTestIP + ":43821"
	if err := a.ensureServer(listen); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	s1, _ := a.sessions.Create()
	s2, err := a.sessions.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := a.EndSession(s1.Key()); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	// One session remains, so the server stays up for it.
	if a.srv == nil {
		t.Fatal("server must stay up while a session is active")
	}
	// The revoked session can no longer validate, the survivor can.
	if _, verr := a.sessions.Validate(s1.Token()); verr == nil {
		t.Fatal("ended session token must be dead")
	}

	if err := a.EndSession(s2.Key()); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if a.srv != nil {
		t.Fatal("last session gone: the server must stop")
	}
	if a.sessions.Len() != 0 {
		t.Fatalf("Len = %d, want 0", a.sessions.Len())
	}
}

func TestEndSessionUnknownKey(t *testing.T) {
	a := newTestApp(t)
	if err := a.EndSession("no-such-key"); err == nil {
		t.Fatal("unknown key must fail visibly")
	}
}

func TestEndSessionCancelsPendingDownloadRecord(t *testing.T) {
	// A granted download whose native navigation never redeemed it stays
	// in the starting state for the grant lifetime. Idle expiry and a
	// changed network cancel such a record ("session ended"); ending the
	// session at the user's request must too, or the desktop keeps a
	// phantom Active-transfers row that can never progress again.
	a := newTestApp(t)
	s, err := a.sessions.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := a.downloads.StartKind("<?>progress", "film.mkv", s.Key(), 2048, "archive"); err != nil {
		t.Fatalf("StartKind: %v", err)
	}
	if err := a.EndSession(s.Key()); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	list := a.RecentDownloads()
	if len(list) != 1 {
		t.Fatalf("RecentDownloads holds %d entries, want the one pending download", len(list))
	}
	if list[0].State != string(transfer.DownloadCanceled) {
		t.Errorf("pending download after EndSession = state %q reason %q, want %q \"session ended\"",
			list[0].State, list[0].Reason, transfer.DownloadCanceled)
	}
}

// ---- projection contracts crossing the Wails bridge ----

func TestSessionViewJSONShape(t *testing.T) {
	// The desktop holds interfaceName/address as the session identity the
	// UI stamps into the QR; the JSON names are reserved-word-safe and
	// fixed. Listing views must omit token/interface/address.
	listData, err := json.Marshal(SessionView{Key: "abc123"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{"\"token\"", "\"interfaceName\"", "\"address\""} {
		if strings.Contains(string(listData), banned) {
			t.Errorf("session listing leaks %s: %s", banned, listData)
		}
	}

	startData, err := json.Marshal(SessionView{
		Key:           "abc123",
		Token:         "tok43",
		InterfaceName: "wlo1",
		Address:       "192.168.0.10",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{"\"token\":\"tok43\"", "\"interfaceName\":\"wlo1\"", "\"address\":\"192.168.0.10\""} {
		if !strings.Contains(string(startData), want) {
			t.Errorf("StartSession view %s must contain %s", startData, want)
		}
	}
}
