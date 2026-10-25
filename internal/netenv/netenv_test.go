package netenv

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
)

// fakeSource builds a Source over synthetic interfaces. The addresses'
// prefix length is /24 unless overridden.
func fakeSource(ifaces []net.Interface, addrs map[string][]net.Addr) Source {
	return Source{
		Interfaces: func() ([]net.Interface, error) { return ifaces, nil },
		Addrs: func(ic net.Interface) ([]net.Addr, error) {
			return addrs[ic.Name], nil
		},
	}
}

func fakeMetadata(values map[string]Metadata) func(net.Interface) Metadata {
	return func(iface net.Interface) Metadata { return values[iface.Name] }
}

// ipnetNet returns a /24 net.IPNet-style address for the dotted quad.
func ipnetNet(a, b, c, d byte) net.Addr {
	ip := net.IPv4(a, b, c, d)
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(24, 32)}
}

func ipaddrNet(a, b, c, d byte) net.Addr {
	return &net.IPAddr{IP: net.IPv4(a, b, c, d)}
}

func upIf(name string) net.Interface {
	return net.Interface{Index: 1, MTU: 1500, Name: name, Flags: net.FlagUp | net.FlagBroadcast | net.FlagMulticast, HardwareAddr: []byte{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}}
}

func TestEnumerateFiltersUnsuitableAddresses(t *testing.T) {
	src := fakeSource(
		[]net.Interface{upIf("br0"), upIf("lo"), upIf("down0"), upIf("ppp0")},
		map[string][]net.Addr{
			"br0": {ipnetNet(192, 168, 4, 10)},
			// lo has FlagLoopback; its 127.x address must not surface.
			"lo":    {ipnetNet(127, 0, 0, 1)},
			"down0": {ipnetNet(192, 168, 9, 9)},
			// point-to-point (VPN/dial-up): excluded even when up with a
			// plausible address.
			"ppp0": {ipnetNet(192, 168, 77, 2)},
		},
	)
	lo, _ := src.Interfaces()
	lo[1].Flags |= net.FlagLoopback
	lo[2].Flags &^= net.FlagUp
	lo[3].Flags |= net.FlagPointToPoint
	src = fakeSource(lo, map[string][]net.Addr{
		"br0":   {ipnetNet(192, 168, 4, 10)},
		"lo":    {ipnetNet(127, 0, 0, 1)},
		"down0": {ipnetNet(192, 168, 9, 9)},
		"ppp0":  {ipnetNet(192, 168, 77, 2)},
	})

	views, err := Enumerate(src)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("got %d views %+v, want exactly 1", len(views), views)
	}
	want := View{Name: "br0", IP: "192.168.4.10", CIDR: "192.168.4.10/24", FriendlyName: "Network connection", Type: "Network", FriendlyNameSource: "Unavailable", Active: true}
	if views[0] != want {
		t.Fatalf("got %+v, want %+v", views[0], want)
	}
}

func TestEnumerateFiltersUnsuitableRanges(t *testing.T) {
	ranges := []struct {
		name string
		addr net.Addr
	}{
		{"unspecified", ipnetNet(0, 0, 0, 0)},
		{"link-local", ipnetNet(169, 254, 3, 4)},
		{"carrier-nat", ipnetNet(100, 64, 5, 9)},
		{"multicast", ipaddrNet(239, 1, 2, 3)},
		{"benchmark", ipnetNet(198, 19, 0, 5)},
		{"test-net1", ipnetNet(192, 0, 2, 7)},
		{"test-net2", ipnetNet(198, 51, 100, 7)},
		{"test-net3", ipnetNet(203, 0, 113, 7)},
		{"protocol", ipnetNet(192, 0, 0, 8)},
		{"reserved", ipnetNet(240, 1, 2, 3)},
		{"broadcast", ipaddrNet(255, 255, 255, 255)},
	}
	for _, tc := range ranges {
		src := fakeSource(
			[]net.Interface{upIf("eth9")},
			map[string][]net.Addr{"eth9": {tc.addr}},
		)
		views, err := Enumerate(src)
		if err != nil {
			t.Fatalf("%s: Enumerate: %v", tc.name, err)
		}
		if len(views) != 0 {
			t.Errorf("%s: address %v was not filtered out: %+v", tc.name, tc.addr, views)
		}
	}
}

func TestEnumerateSkipsIPv6AndKeepsUsable(t *testing.T) {
	v6 := &net.IPNet{
		IP:   net.ParseIP("fe80::dead:beef"),
		Mask: net.CIDRMask(64, 128),
	}
	src := fakeSource(
		[]net.Interface{upIf("wlo1")},
		map[string][]net.Addr{"wlo1": {v6, ipnetNet(10, 42, 0, 8)}},
	)
	views, err := Enumerate(src)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(views) != 1 || views[0].IP != "10.42.0.8" {
		t.Fatalf("got %+v, want only the IPv4 address", views)
	}
}

func TestEnumerateDeterministicOrderMultipleIfaces(t *testing.T) {
	src := fakeSource(
		[]net.Interface{upIf("wlan1"), upIf("eth0")},
		map[string][]net.Addr{
			"wlan1": {ipnetNet(192, 168, 8, 1), ipnetNet(10, 0, 0, 9)},
			"eth0":  {ipnetNet(192, 168, 8, 2)},
		},
	)
	views, err := Enumerate(src)
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	var got []string
	for _, v := range views {
		got = append(got, v.Name+"/"+v.IP)
	}
	want := []string{"eth0/192.168.8.2", "wlan1/10.0.0.9", "wlan1/192.168.8.1"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("ordering got %v, want %v", got, want)
		}
	}
}

func TestEnumerateSourceError(t *testing.T) {
	src := Source{Interfaces: func() ([]net.Interface, error) {
		return nil, errors.New("driver boom")
	}}
	if _, err := Enumerate(src); err == nil {
		t.Fatal("expected the interfaces error to surface")
	}
}

func TestPickDefault(t *testing.T) {
	one := View{Name: "eth0", IP: "10.0.0.2", CIDR: "10.0.0.2/24"}
	two := View{Name: "wlan0", IP: "192.168.1.5", CIDR: "192.168.1.5/24"}

	if _, err := PickDefault(nil); !errors.Is(err, ErrNoCandidate) {
		t.Errorf("empty list: got %v, want ErrNoCandidate", err)
	}
	got, err := PickDefault([]View{one})
	if err != nil || got != one {
		t.Errorf("single list: got %+v err %v, want %v", got, err, one)
	}
	if _, err := PickDefault([]View{one, two}); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("two lists: got %v, want ErrAmbiguous", err)
	} else if msg := err.Error(); !strings.Contains(msg, "eth0") || !strings.Contains(msg, "wlan0") {
		t.Errorf("ambiguous error should name the candidate interfaces: %q", msg)
	}
}

func TestInterfaceForRevalidation(t *testing.T) {
	src := fakeSource(
		[]net.Interface{upIf("wlo1")},
		map[string][]net.Addr{"wlo1": {ipnetNet(10, 42, 0, 8)}},
	)

	if v, err := InterfaceFor(src, "10.42.0.8"); err != nil || v.Name != "wlo1" {
		t.Fatalf("valid IP: got %+v err %v", v, err)
	}
	_, err := InterfaceFor(src, "10.42.0.200")
	if !errors.Is(err, ErrGone) {
		t.Fatalf("absent IP: got %v, want ErrGone", err)
	}
	if _, err := InterfaceFor(src, "not-an-ip"); err == nil {
		t.Fatal("invalid IP must error")
	}
}

func TestInterfaceForDetectsChangedAddress(t *testing.T) {
	src := fakeSource(
		[]net.Interface{upIf("wlo1")},
		map[string][]net.Addr{"wlo1": {ipnetNet(10, 43, 0, 8)}},
	)
	_, err := InterfaceFor(src, "10.42.0.8")
	if !errors.Is(err, ErrGone) {
		t.Fatalf("stale IP: got %v, want ErrGone (address changed)", err)
	}
}

func TestListenAddr(t *testing.T) {
	got, err := ListenAddr("10.42.0.8", 43821)
	if err != nil || got != "10.42.0.8:43821" {
		t.Fatalf("got %q err %v", got, err)
	}
	for _, bad := range []struct {
		ip   string
		port int
	}{
		{"fe80::1", 43821},
		{"::ffff:10.0.0.1", 43821},
		{"10.0.0.1", 0},
		{"10.0.0.1", 70000},
		{"10.0.0.1", -3},
	} {
		if _, err := ListenAddr(bad.ip, bad.port); err == nil {
			t.Errorf("ListenAddr(%q, %d) should fail", bad.ip, bad.port)
		}
	}
}

func TestTransferURL(t *testing.T) {
	got, err := TransferURL("192.168.1.42", 43821, "abc-_43chars-token-value")
	if err != nil {
		t.Fatalf("TransferURL: %v", err)
	}
	want := "http://192.168.1.42:43821/#abc-_43chars-token-value"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	badTokens := []string{"", " ", "#tok", "a#b", "a b", "x\ny"}
	for _, tok := range badTokens {
		if _, err := TransferURL("10.0.0.1", 43821, tok); err == nil {
			t.Errorf("token %q should be rejected", tok)
		}
	}
	if _, err := TransferURL("10.0.0.1", 70000, "tok"); err == nil {
		t.Error("bad port must surface from URL construction")
	}
}

func TestViewJSONShape(t *testing.T) {
	// Views cross the Wails bridge as JSON; the field names are part of
	// that contract, so this decodes and verifies them precisely.
	data, err := json.Marshal(View{Name: "wlo1", IP: "10.42.0.8", CIDR: "10.42.0.8/24", FriendlyName: "Synthetic hotspot", Type: "Wi-Fi", MetadataAvailable: true, ConnectionName: "saved-profile", FriendlyNameSource: "SSID", Active: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"name", "ip", "cidr", "friendlyName", "type", "connectionName", "friendlyNameSource"} {
		if got[key] == nil || got[key] == "" {
			t.Errorf("field %q missing or empty in %s", key, data)
		}
	}
	if available, ok := got["metadataAvailable"].(bool); !ok || !available {
		t.Errorf("metadataAvailable = %#v, want true", got["metadataAvailable"])
	}
	if active, ok := got["active"].(bool); !ok || !active {
		t.Errorf("active = %#v, want true", got["active"])
	}
}

func TestEnumerateFriendlyMetadataAndFallbacks(t *testing.T) {
	ifaces := []net.Interface{upIf("wlo1"), upIf("eth0"), upIf("mystery0")}
	src := fakeSource(ifaces, map[string][]net.Addr{
		"wlo1":     {ipnetNet(192, 168, 0, 106)},
		"eth0":     {ipnetNet(10, 0, 0, 2)},
		"mystery0": {ipnetNet(172, 16, 0, 2)},
	})
	src.Metadata = fakeMetadata(map[string]Metadata{
		"wlo1": {SSID: "Synthetic hotspot", ConnectionName: "phone-profile", Type: "Wi-Fi"},
		"eth0": {Type: "Ethernet"},
	})
	views, err := Enumerate(src)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]View, len(views))
	for _, view := range views {
		byName[view.Name] = view
	}
	if byName["mystery0"].FriendlyName != "Network connection" || byName["mystery0"].Type != "Network" {
		t.Fatalf("missing metadata fallback = %+v", byName["mystery0"])
	}
	if byName["eth0"].FriendlyName != "Ethernet connection" {
		t.Fatalf("ethernet fallback = %+v", byName["eth0"])
	}
	if byName["mystery0"].MetadataAvailable {
		t.Fatal("missing metadata reported as available")
	}
	if byName["wlo1"].FriendlyName != "Synthetic hotspot" ||
		byName["wlo1"].ConnectionName != "phone-profile" ||
		byName["wlo1"].FriendlyNameSource != "SSID" ||
		!byName["wlo1"].Active {
		t.Fatalf("SSID/profile metadata = %+v", byName["wlo1"])
	}
}

func TestEnumeratePreservesUniversalConnectionLabels(t *testing.T) {
	cases := []struct {
		name string
		typ  string
	}{
		{name: "Samsung test network", typ: "Wi-Fi"},
		{name: "Pixel test network", typ: "Wi-Fi"},
		{name: "iPhone test network", typ: "Wi-Fi"},
		{name: "Generic access point", typ: "Wi-Fi"},
	}
	for i, tc := range cases {
		ifaceName := "wifi" + string(rune('0'+i))
		src := fakeSource(
			[]net.Interface{upIf(ifaceName)},
			map[string][]net.Addr{ifaceName: {ipnetNet(10, byte(i+1), 0, 2)}},
		)
		src.Metadata = fakeMetadata(map[string]Metadata{
			ifaceName: {SSID: tc.name, ConnectionName: "profile-" + ifaceName, Type: tc.typ},
		})
		views, err := Enumerate(src)
		if err != nil || len(views) != 1 {
			t.Fatalf("%s: views=%+v err=%v", tc.name, views, err)
		}
		if views[0].FriendlyName != tc.name || views[0].ConnectionName == views[0].FriendlyName {
			t.Fatalf("%s: did not preserve separate SSID/profile metadata: %+v", tc.name, views[0])
		}
	}
}

func TestMissingSSIDUsesExplicitFallback(t *testing.T) {
	view := withMetadata(View{}, Metadata{Type: "Wi-Fi"})
	if view.FriendlyName != "Wi-Fi network · Name unavailable" {
		t.Fatalf("fallback = %q", view.FriendlyName)
	}
	if view.FriendlyNameSource != "Unavailable" {
		t.Fatalf("fallback source = %q, want Unavailable", view.FriendlyNameSource)
	}
}

func TestProfileNameIsFriendlyFallbackWhenSSIDUnavailable(t *testing.T) {
	view := withMetadata(View{}, Metadata{
		Type:           "Wi-Fi",
		ConnectionName: "moto edge 60 pro_3360",
	})
	if view.FriendlyName != "moto edge 60 pro_3360" {
		t.Fatalf("friendly fallback = %q, want profile name", view.FriendlyName)
	}
	if view.FriendlyNameSource != "Connection profile" {
		t.Fatalf("fallback source = %q, want Connection profile", view.FriendlyNameSource)
	}
	if !view.MetadataAvailable {
		t.Fatal("profile fallback should report metadata available")
	}
}

func TestSSIDRemainsPrimaryWhenProfileDiffers(t *testing.T) {
	view := withMetadata(View{}, Metadata{
		Type:           "Wi-Fi",
		SSID:           "actual hotspot",
		ConnectionName: "saved profile",
	})
	if view.FriendlyName != "actual hotspot" || view.FriendlyNameSource != "SSID" {
		t.Fatalf("SSID did not remain primary: %+v", view)
	}
}
