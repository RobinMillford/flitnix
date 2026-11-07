package netenv

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeProvider struct {
	mu       sync.Mutex
	ifaces   []net.Interface
	addrs    map[string][]net.Addr
	metadata map[string]Metadata
	err      error
}

func (f *fakeProvider) InterfacesFor() ([]net.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.ifaces, nil
}

func (f *fakeProvider) AddressesFor(iface net.Interface) ([]net.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addrs[iface.Name], nil
}

func (f *fakeProvider) MetadataFor(iface net.Interface) Metadata {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metadata[iface.Name]
}

func (f *fakeProvider) setNetwork(ifaces []net.Interface, addrs map[string][]net.Addr, metadata map[string]Metadata) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ifaces = ifaces
	f.addrs = addrs
	f.metadata = metadata
	f.err = nil
}

func (f *fakeProvider) setError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func makeIPNet(ipStr string, maskBits int) *net.IPNet {
	ip := net.ParseIP(ipStr)
	return &net.IPNet{
		IP:   ip,
		Mask: net.CIDRMask(maskBits, 32),
	}
}

func TestMonitorInitialDiscoveryAndConnected(t *testing.T) {
	prov := &fakeProvider{
		ifaces: []net.Interface{
			{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast},
		},
		addrs: map[string][]net.Addr{
			"wlo1": {makeIPNet("192.168.1.50", 24)},
		},
		metadata: map[string]Metadata{
			"wlo1": {SSID: "Home-WiFi", Type: "Wi-Fi"},
		},
	}

	m := NewMonitor(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.Start(ctx)
	defer m.Stop()

	st := m.State()
	if st.Status != StatusConnected {
		t.Fatalf("expected status connected, got %s", st.Status)
	}
	if st.InterfaceName != "wlo1" || st.IP != "192.168.1.50" || st.SSID != "Home-WiFi" {
		t.Fatalf("unexpected state fields: %+v", st)
	}
}

func TestMonitorDisconnectionAndReconnection(t *testing.T) {
	prov := &fakeProvider{
		ifaces: []net.Interface{
			{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast},
		},
		addrs: map[string][]net.Addr{
			"wlo1": {makeIPNet("192.168.1.50", 24)},
		},
		metadata: map[string]Metadata{
			"wlo1": {SSID: "Home-WiFi", Type: "Wi-Fi"},
		},
	}

	notifyCh := make(chan struct{}, 10)
	notifier := func(ctx context.Context, notify chan<- struct{}) {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-notifyCh:
				select {
				case notify <- sig:
				case <-ctx.Done():
					return
				}
			}
		}
	}

	var stateHistory []NetworkState
	var historyMu sync.Mutex

	m := NewMonitor(prov,
		WithNativeNotifier(notifier),
		WithDebounceDelay(10*time.Millisecond),
		WithPollInterval(50*time.Millisecond),
	)

	m.OnChange(func(st NetworkState) {
		historyMu.Lock()
		defer historyMu.Unlock()
		stateHistory = append(stateHistory, st)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.Start(ctx)
	defer m.Stop()

	// Initial state is connected
	if m.State().Status != StatusConnected {
		t.Fatalf("expected initial connected, got %s", m.State().Status)
	}

	// 1. Disconnection: interface goes down / empty
	prov.setNetwork(nil, nil, nil)
	notifyCh <- struct{}{}

	time.Sleep(50 * time.Millisecond)

	st := m.State()
	if st.Status != StatusNoNetwork {
		t.Fatalf("expected StatusNoNetwork on disconnect, got %s", st.Status)
	}
	if st.IP != "" || st.InterfaceName != "" {
		t.Fatalf("expected cleared IP and interface, got ip=%s iface=%s", st.IP, st.InterfaceName)
	}

	// 2. Reconnection: interface comes back with phone hotspot
	prov.setNetwork(
		[]net.Interface{{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast}},
		map[string][]net.Addr{"wlo1": {makeIPNet("192.168.43.120", 24)}},
		map[string]Metadata{"wlo1": {SSID: "PixelHotspot", Type: "Wi-Fi"}},
	)
	notifyCh <- struct{}{}

	time.Sleep(50 * time.Millisecond)

	st = m.State()
	if st.Status != StatusConnected {
		t.Fatalf("expected StatusConnected on reconnect, got %s", st.Status)
	}
	if st.IP != "192.168.43.120" || st.SSID != "PixelHotspot" {
		t.Fatalf("expected reconnected hotspot fields, got ip=%s ssid=%s", st.IP, st.SSID)
	}

	historyMu.Lock()
	count := len(stateHistory)
	historyMu.Unlock()
	if count < 2 {
		t.Fatalf("expected at least 2 state transitions in history, got %d", count)
	}
}

func TestMonitorSSIDAndIPChangeOnSameInterface(t *testing.T) {
	prov := &fakeProvider{
		ifaces: []net.Interface{
			{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast},
		},
		addrs: map[string][]net.Addr{
			"wlo1": {makeIPNet("192.168.1.50", 24)},
		},
		metadata: map[string]Metadata{
			"wlo1": {SSID: "Network-A", Type: "Wi-Fi"},
		},
	}

	m := NewMonitor(prov)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.Start(ctx)
	defer m.Stop()

	// Initial
	if m.State().SSID != "Network-A" || m.State().IP != "192.168.1.50" {
		t.Fatalf("unexpected initial: %+v", m.State())
	}

	// Change SSID and IP on wlo1 (e.g. switched Wi-Fi without changing adapter)
	prov.setNetwork(
		[]net.Interface{{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast}},
		map[string][]net.Addr{"wlo1": {makeIPNet("10.0.0.25", 24)}},
		map[string]Metadata{"wlo1": {SSID: "Network-B", Type: "Wi-Fi"}},
	)

	fresh, err := m.Refresh()
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if fresh.SSID != "Network-B" || fresh.IP != "10.0.0.25" {
		t.Fatalf("expected updated SSID Network-B and IP 10.0.0.25, got ssid=%s ip=%s", fresh.SSID, fresh.IP)
	}
}

func TestMonitorPreferredInterfaceUnavailableFallback(t *testing.T) {
	prov := &fakeProvider{
		ifaces: []net.Interface{
			{Index: 1, Name: "eth0", Flags: net.FlagUp | net.FlagBroadcast},
			{Index: 2, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast},
		},
		addrs: map[string][]net.Addr{
			"eth0": {makeIPNet("192.168.1.10", 24)},
			"wlo1": {makeIPNet("192.168.1.50", 24)},
		},
		metadata: map[string]Metadata{
			"eth0": {ConnectionName: "Wired", Type: "Ethernet"},
			"wlo1": {SSID: "Home-WiFi", Type: "Wi-Fi"},
		},
	}

	pref := "eth0"
	m := NewMonitor(prov, WithPreferredInterface(func() string {
		return pref
	}))

	st, err := m.Refresh()
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if st.Status != StatusConnected || st.SelectedInterface != "eth0" {
		t.Fatalf("expected eth0 selected, got status=%s iface=%s", st.Status, st.SelectedInterface)
	}

	// Disconnect eth0 (e.g. cable unplugged). Only wlo1 remains.
	prov.setNetwork(
		[]net.Interface{{Index: 2, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast}},
		map[string][]net.Addr{"wlo1": {makeIPNet("192.168.1.50", 24)}},
		map[string]Metadata{"wlo1": {SSID: "Home-WiFi", Type: "Wi-Fi"}},
	)

	st, err = m.Refresh()
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if st.Status != StatusSelectedUnavailable {
		t.Fatalf("expected StatusSelectedUnavailable, got %s", st.Status)
	}
	if st.SelectedInterface != "wlo1" || st.IP != "192.168.1.50" {
		t.Fatalf("expected fallback to wlo1, got %s (%s)", st.SelectedInterface, st.IP)
	}
	if st.PreferredInterface != "eth0" {
		t.Fatalf("expected preferred interface setting preserved, got %s", st.PreferredInterface)
	}
	if st.Error == "" {
		t.Fatalf("expected explanatory warning in Error field for unavailable preferred interface")
	}
}

func TestMonitorCoalescesDuplicateNotifications(t *testing.T) {
	prov := &fakeProvider{
		ifaces: []net.Interface{
			{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast},
		},
		addrs: map[string][]net.Addr{
			"wlo1": {makeIPNet("192.168.1.50", 24)},
		},
		metadata: map[string]Metadata{
			"wlo1": {SSID: "Home-WiFi", Type: "Wi-Fi"},
		},
	}

	notifyCh := make(chan struct{}, 20)
	notifier := func(ctx context.Context, notify chan<- struct{}) {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-notifyCh:
				select {
				case notify <- sig:
				case <-ctx.Done():
					return
				}
			}
		}
	}

	var callCount int
	var mu sync.Mutex

	m := NewMonitor(prov,
		WithNativeNotifier(notifier),
		WithDebounceDelay(25*time.Millisecond),
		WithPollInterval(1*time.Hour), // disable periodic poll for this test
	)

	m.OnChange(func(st NetworkState) {
		mu.Lock()
		callCount++
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.Start(ctx)
	defer m.Stop()

	// Reset callCount after initial startup transition
	mu.Lock()
	callCount = 0
	mu.Unlock()

	// Now update to new state and blast 10 rapid notifications
	prov.setNetwork(
		[]net.Interface{{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast}},
		map[string][]net.Addr{"wlo1": {makeIPNet("192.168.1.99", 24)}},
		map[string]Metadata{"wlo1": {SSID: "Home-WiFi", Type: "Wi-Fi"}},
	)

	for i := 0; i < 10; i++ {
		notifyCh <- struct{}{}
		time.Sleep(1 * time.Millisecond)
	}

	// Wait for debounce window (25ms) to fire and settle
	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	count := callCount
	mu.Unlock()

	// Exactly 1 update call should have fired for the coalesced burst
	if count != 1 {
		t.Fatalf("expected exactly 1 callback for debounced burst, got %d", count)
	}
	if m.State().IP != "192.168.1.99" {
		t.Fatalf("expected updated IP 192.168.1.99, got %s", m.State().IP)
	}
}

func TestMonitorDiscoveryErrorState(t *testing.T) {
	prov := &fakeProvider{
		err: errors.New("netlink query failed: permission denied"),
	}

	m := NewMonitor(prov)
	st, err := m.Refresh()
	if err == nil {
		t.Fatalf("expected error from refresh")
	}
	if st.Status != StatusError {
		t.Fatalf("expected StatusError, got %s", st.Status)
	}
	if st.Error != "netenv: listing interfaces: netlink query failed: permission denied" {
		t.Fatalf("unexpected error string: %s", st.Error)
	}
}

func TestMonitorCleanShutdown(t *testing.T) {
	prov := &fakeProvider{
		ifaces: []net.Interface{{Index: 1, Name: "wlo1", Flags: net.FlagUp}},
		addrs:  map[string][]net.Addr{"wlo1": {makeIPNet("192.168.1.50", 24)}},
	}

	m := NewMonitor(prov, WithPollInterval(10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())

	m.Start(ctx)
	time.Sleep(25 * time.Millisecond)

	// Calling Stop must not hang or deadlock
	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		// Clean exit
	case <-time.After(1 * time.Second):
		t.Fatalf("Stop() timed out or deadlocked")
	}

	cancel()
}

func TestEvaluateStateAllStatuses(t *testing.T) {
	// 1. Error state
	stErr := EvaluateState(nil, "", "", errors.New("socket fail"))
	if stErr.Status != StatusError || stErr.Error != "socket fail" {
		t.Fatalf("unexpected stErr: %+v", stErr)
	}

	// 2. No network state
	stNoNet := EvaluateState([]View{}, "", "", nil)
	if stNoNet.Status != StatusNoNetwork || stNoNet.Error == "" {
		t.Fatalf("unexpected stNoNet: %+v", stNoNet)
	}

	// 3. Metadata unavailable state
	vNoMeta := View{
		Name:              "wlo1",
		IP:                "192.168.1.15",
		Type:              "Wi-Fi",
		MetadataAvailable: false,
		FriendlyName:      "Wi-Fi network · Name unavailable",
	}
	stNoMeta := EvaluateState([]View{vNoMeta}, "", "192.168.1.15:43821", nil)
	if stNoMeta.Status != StatusMetadataUnavailable || stNoMeta.ServerAddress != "192.168.1.15:43821" || stNoMeta.AdvertisedURL != "http://192.168.1.15:43821" {
		t.Fatalf("unexpected stNoMeta: %+v", stNoMeta)
	}

	// 4. Connected state with verified SSID
	vConnected := View{
		Name:               "wlo1",
		IP:                 "192.168.1.50",
		Type:               "Wi-Fi",
		MetadataAvailable:  true,
		FriendlyName:       "Home-Network",
		FriendlyNameSource: "SSID",
	}
	stConnected := EvaluateState([]View{vConnected}, "", "192.168.1.50:43821", nil)
	if stConnected.Status != StatusConnected || stConnected.SSID != "Home-Network" || stConnected.SelectedInterface != "wlo1" {
		t.Fatalf("unexpected stConnected: %+v", stConnected)
	}

	// 5. Selected interface unavailable fallback state
	vEthernet := View{
		Name:         "eth0",
		IP:           "10.0.0.5",
		Type:         "Ethernet",
		FriendlyName: "Ethernet connection",
	}
	stFallback := EvaluateState([]View{vEthernet}, "wlo1", "", nil)
	if stFallback.Status != StatusSelectedUnavailable || stFallback.SelectedInterface != "eth0" || stFallback.PreferredInterface != "wlo1" {
		t.Fatalf("unexpected stFallback: %+v", stFallback)
	}
	if stFallback.Error == "" {
		t.Fatalf("expected explanatory warning for unavailable preferred interface")
	}

	// 6. Equality check
	stConnectedCopy := stConnected
	stConnectedCopy.LastDetected += 100 // LastDetected should be ignored in Equal()
	if !stConnected.Equal(stConnectedCopy) {
		t.Fatalf("expected snapshots with different LastDetected timestamps to be Equal")
	}

	stConnectedCopy.IP = "192.168.1.51"
	if stConnected.Equal(stConnectedCopy) {
		t.Fatalf("expected snapshots with different IPs to not be Equal")
	}
}

func TestMonitorStickySelectionWithSecondaryInterface(t *testing.T) {
	vWiFi := View{
		Name:               "wlo1",
		IP:                 "192.168.1.50",
		Type:               "Wi-Fi",
		FriendlyName:       "Office-WiFi",
		FriendlyNameSource: "SSID",
		Family:             "IPv4",
	}
	vEth := View{
		Name:         "eth0",
		IP:           "10.0.0.100",
		Type:         "Ethernet",
		FriendlyName: "Ethernet connection",
		Family:       "IPv4",
	}
	vDocker := View{
		Name:         "docker0",
		IP:           "172.17.0.1",
		Type:         "Network",
		FriendlyName: "Network connection",
		Family:       "IPv4",
	}

	// 1. With active server running on wlo1 (192.168.1.50:43821),
	// adding eth0 and docker0 MUST NOT displace wlo1.
	views := []View{vDocker, vEth, vWiFi}
	st := EvaluateState(views, "", "192.168.1.50:43821", nil)
	if st.SelectedInterface != "wlo1" || st.IP != "192.168.1.50" {
		t.Fatalf("expected sticky selection to preserve wlo1, got interface %s with IP %s", st.SelectedInterface, st.IP)
	}

	// 2. If wlo1 drops, it cleanly falls back to the next best interface (eth0 over docker0).
	viewsNoWiFi := []View{vDocker, vEth}
	stFallback := EvaluateState(viewsNoWiFi, "", "192.168.1.50:43821", nil)
	if stFallback.SelectedInterface != "eth0" || stFallback.IP != "10.0.0.100" {
		t.Fatalf("expected fallback to eth0, got interface %s with IP %s", stFallback.SelectedInterface, stFallback.IP)
	}
}

func TestInterfacePriorityHierarchy(t *testing.T) {
	vDocker := View{Name: "docker0", IP: "172.17.0.1", Type: "Network", Family: "IPv4"}
	vVirbr := View{Name: "virbr0", IP: "192.168.122.1", Type: "Network", Family: "IPv4"}
	vEth := View{Name: "eth0", IP: "10.0.0.2", Type: "Ethernet", Family: "IPv4"}
	vWiFi := View{Name: "wlo1", IP: "192.168.1.40", Type: "Wi-Fi", FriendlyName: "FastWiFi", FriendlyNameSource: "SSID", Family: "IPv4"}

	// When no server is running and no preference is set, Wi-Fi with SSID beats Ethernet and Virtual bridges
	views := []View{vDocker, vVirbr, vEth, vWiFi}
	st := EvaluateState(views, "", "", nil)
	if st.SelectedInterface != "wlo1" {
		t.Fatalf("expected highest priority for Wi-Fi (wlo1), got %s", st.SelectedInterface)
	}

	// When Wi-Fi is absent, Ethernet beats Virtual bridges
	viewsWithoutWiFi := []View{vDocker, vVirbr, vEth}
	stEth := EvaluateState(viewsWithoutWiFi, "", "", nil)
	if stEth.SelectedInterface != "eth0" {
		t.Fatalf("expected Ethernet (eth0) to beat virtual bridges, got %s", stEth.SelectedInterface)
	}
}

func TestMonitorRepeatedStartStop(t *testing.T) {
	prov := &fakeProvider{
		ifaces: []net.Interface{{Index: 1, Name: "wlo1", Flags: net.FlagUp}},
		addrs:  map[string][]net.Addr{"wlo1": {makeIPNet("192.168.1.50", 24)}},
	}

	m := NewMonitor(prov, WithPollInterval(10*time.Millisecond))

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		m.Start(ctx)
		time.Sleep(15 * time.Millisecond)
		m.Stop()
		cancel()
	}
}
