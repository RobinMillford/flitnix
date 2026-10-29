package doctor

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/itsyamin/flitnix/internal/netenv"
)

type mockProvider struct {
	ifaces []net.Interface
	addrs  map[string][]net.Addr
	meta   map[string]netenv.Metadata
}

func (m *mockProvider) InterfacesFor() ([]net.Interface, error) {
	return m.ifaces, nil
}

func (m *mockProvider) AddressesFor(iface net.Interface) ([]net.Addr, error) {
	return m.addrs[iface.Name], nil
}

func (m *mockProvider) MetadataFor(iface net.Interface) netenv.Metadata {
	if m.meta != nil {
		if meta, ok := m.meta[iface.Name]; ok {
			return meta
		}
	}
	return netenv.Metadata{Type: "Wi-Fi", SSID: "FlitnixNet"}
}

func TestRunDiagnostics7ChecksHealthyPass(t *testing.T) {
	iface := net.Interface{
		Index: 1,
		Name:  "wlo1",
		Flags: net.FlagUp | net.FlagBroadcast,
	}
	_, ipnet, _ := net.ParseCIDR("192.168.1.100/24")

	prov := &mockProvider{
		ifaces: []net.Interface{iface},
		addrs:  map[string][]net.Addr{"wlo1": {ipnet}},
	}

	report := RunDiagnostics(Params{
		NetSource:     prov,
		ServerRunning: false,
		ActiveIP:      "192.168.1.100",
		ActiveIface:   "wlo1",
	})

	if len(report.Items) != 7 {
		t.Fatalf("expected 7 diagnostic items, got %d", len(report.Items))
	}

	expectedIDs := []string{
		"adapter_presence",
		"ipv4_address",
		"port_available",
		"server_health",
		"connection_medium",
		"host_reachability",
		"multihomed_routing",
	}
	for i, expectedID := range expectedIDs {
		if report.Items[i].ID != expectedID {
			t.Errorf("item %d expected ID %q, got %q", i, expectedID, report.Items[i].ID)
		}
	}

	// Single healthy Wi-Fi network with no port conflict must report PASS
	if report.OverallStatus != StatusPass {
		t.Fatalf("expected overall status %s, got %s", StatusPass, report.OverallStatus)
	}
}

func TestRunDiagnosticsMultiHomedWarning(t *testing.T) {
	iface1 := net.Interface{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast}
	iface2 := net.Interface{Index: 2, Name: "tun0", Flags: net.FlagUp | net.FlagBroadcast}
	_, ipnet1, _ := net.ParseCIDR("192.168.1.100/24")
	_, ipnet2, _ := net.ParseCIDR("10.8.0.2/24")

	prov := &mockProvider{
		ifaces: []net.Interface{iface1, iface2},
		addrs: map[string][]net.Addr{
			"wlo1": {ipnet1},
			"tun0": {ipnet2},
		},
		meta: map[string]netenv.Metadata{
			"wlo1": {Type: "Wi-Fi", SSID: "FlitnixNet"},
			"tun0": {Type: "VPN"},
		},
	}

	report := RunDiagnostics(Params{
		NetSource:     prov,
		ServerRunning: false,
		ActiveIP:      "192.168.1.100",
		ActiveIface:   "wlo1",
	})

	if len(report.Items) != 7 {
		t.Fatalf("expected 7 diagnostic items, got %d", len(report.Items))
	}

	if report.OverallStatus != StatusWarn {
		t.Fatalf("expected overall status %s for multi-homed setup, got %s", StatusWarn, report.OverallStatus)
	}

	// Verify multihomed item is WARNING
	foundMulti := false
	for _, it := range report.Items {
		if it.ID == "multihomed_routing" {
			foundMulti = true
			if it.Status != StatusWarn {
				t.Errorf("expected multihomed_routing to be WARNING, got %s", it.Status)
			}
		}
	}
	if !foundMulti {
		t.Fatal("multihomed_routing check not found")
	}
}

func TestRunDiagnosticsFailWhenNoAdapter(t *testing.T) {
	prov := &mockProvider{
		ifaces: []net.Interface{},
		addrs:  map[string][]net.Addr{},
	}

	report := RunDiagnostics(Params{
		NetSource:     prov,
		ServerRunning: false,
	})

	if report.OverallStatus != StatusFail {
		t.Fatalf("expected StatusFail when no network adapter exists, got %s", report.OverallStatus)
	}

	if len(report.Items) != 7 {
		t.Fatalf("expected 7 diagnostic items, got %d", len(report.Items))
	}
}

func TestRunDiagnosticsDynamicPortAndHealthProbe(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// ts.Listener.Addr().String() gives e.g. "127.0.0.1:PORT"
	serverAddr := ts.Listener.Addr().String()
	_, portStr, _ := net.SplitHostPort(serverAddr)

	iface := net.Interface{Index: 1, Name: "eth0", Flags: net.FlagUp | net.FlagBroadcast}
	_, ipnet, _ := net.ParseCIDR("192.168.1.10/24")

	prov := &mockProvider{
		ifaces: []net.Interface{iface},
		addrs:  map[string][]net.Addr{"eth0": {ipnet}},
		meta:   map[string]netenv.Metadata{"eth0": {Type: "Ethernet"}},
	}

	report := RunDiagnostics(Params{
		NetSource:     prov,
		ServerRunning: true,
		ServerAddr:    serverAddr,
		ActiveIP:      "192.168.1.10",
		ActiveIface:   "eth0",
	})

	var healthItem, portItem *DiagnosticItem
	for i := range report.Items {
		if report.Items[i].ID == "server_health" {
			healthItem = &report.Items[i]
		}
		if report.Items[i].ID == "port_available" {
			portItem = &report.Items[i]
		}
	}

	if healthItem == nil || healthItem.Status != StatusPass {
		t.Fatalf("expected server_health to pass, got %+v", healthItem)
	}

	if portItem == nil || portItem.Status != StatusPass {
		t.Fatalf("expected port_available to pass, got %+v", portItem)
	}

	// Now simulate port occupied when server is reported as not running
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	busyPort := ln.Addr().(*net.TCPAddr).Port
	reportConflict := RunDiagnostics(Params{
		NetSource:     prov,
		ServerRunning: false,
		ServerAddr:    fmt.Sprintf("127.0.0.1:%d", busyPort),
	})

	var conflictPortItem *DiagnosticItem
	for i := range reportConflict.Items {
		if reportConflict.Items[i].ID == "port_available" {
			conflictPortItem = &reportConflict.Items[i]
		}
	}
	if conflictPortItem == nil || conflictPortItem.Status != StatusFail {
		t.Fatalf("expected port conflict to report FAIL on dynamic port, got %+v", conflictPortItem)
	}
	_ = portStr
}
