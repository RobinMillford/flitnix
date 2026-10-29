package doctor

import (
	"net"
	"testing"

	"github.com/itsyamin/flitnix/internal/netenv"
)

type mockProvider struct {
	ifaces []net.Interface
	addrs  map[string][]net.Addr
}

func (m *mockProvider) InterfacesFor() ([]net.Interface, error) {
	return m.ifaces, nil
}

func (m *mockProvider) AddressesFor(iface net.Interface) ([]net.Addr, error) {
	return m.addrs[iface.Name], nil
}

func (m *mockProvider) MetadataFor(iface net.Interface) netenv.Metadata {
	return netenv.Metadata{Type: "Wi-Fi", SSID: "FlitnixNet"}
}

func TestRunDiagnosticsPassAndWarn(t *testing.T) {
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

	if len(report.Items) == 0 {
		t.Fatal("expected diagnostic items")
	}

	// Because Wi-Fi is used, overall status should be WARNING (due to Wi-Fi isolation informational notice)
	if report.OverallStatus != StatusWarn {
		t.Fatalf("expected status %s, got %s", StatusWarn, report.OverallStatus)
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
}
