package discovery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
)

type mockPacket struct {
	pkt   []byte
	srcIP net.IP
	kind  PacketKind
}

type mockHub struct {
	mu      sync.Mutex
	sockets []*mockSocket
	sent    [][]byte
}

func newMockHub() *mockHub {
	return &mockHub{}
}

func (h *mockHub) Open(ifaceName string, localIP net.IP) (PacketSocket, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := &mockSocket{
		hub:     h,
		localIP: append(net.IP(nil), localIP...),
		ch:      make(chan mockPacket, 64),
		closed:  make(chan struct{}),
	}
	h.sockets = append(h.sockets, s)
	return s, nil
}

func (h *mockHub) broadcast(sender *mockSocket, pkt []byte, kind PacketKind) {
	h.mu.Lock()
	cp := append([]byte(nil), pkt...)
	h.sent = append(h.sent, cp)
	targets := append([]*mockSocket(nil), h.sockets...)
	h.mu.Unlock()
	for _, dst := range targets {
		select {
		case dst.ch <- mockPacket{pkt: append([]byte(nil), cp...), srcIP: sender.localIP, kind: kind}:
		default:
		}
	}
}

type mockSocket struct {
	hub     *mockHub
	localIP net.IP
	ch      chan mockPacket
	closed  chan struct{}
	once    sync.Once
}

func (m *mockSocket) SendMDNS(pkt []byte) error {
	m.hub.broadcast(m, pkt, PacketMDNS)
	return nil
}

func (m *mockSocket) SendBeacon(pkt []byte) error {
	m.hub.broadcast(m, pkt, PacketBeacon)
	return nil
}

func (m *mockSocket) Recv(ctx context.Context) ([]byte, net.IP, PacketKind, error) {
	select {
	case <-ctx.Done():
		return nil, nil, "", ctx.Err()
	case <-m.closed:
		return nil, nil, "", context.Canceled
	case p := <-m.ch:
		return p.pkt, p.srcIP, p.kind, nil
	}
}

func (m *mockSocket) Close() error {
	m.once.Do(func() { close(m.closed) })
	return nil
}

func TestMDNSPacketRoundTripAndValidation(t *testing.T) {
	pkt, err := buildMDNSResponse(mdnsAnnouncement{
		HostID:     "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		PublicKey:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		DeviceName: "Studio Linux",
		OS:         "linux",
		Version:    "0.2.8.0",
		Port:       43821,
		IPs:        []net.IP{net.ParseIP("192.168.1.42")},
		Timestamp:  1700000000,
		Sig:        "deadbeef",
	}, 20)
	if err != nil {
		t.Fatalf("buildMDNSResponse: %v", err)
	}
	parsed, ok := parseMDNSResponse(pkt)
	if !ok {
		t.Fatal("parseMDNSResponse returned false")
	}
	if parsed.DeviceName != "Studio Linux" || parsed.OS != "linux" || parsed.Port != 43821 || len(parsed.IPs) != 1 {
		t.Fatalf("unexpected parsed mDNS announcement: %+v", parsed)
	}

	// Compression pointer loop must fail safely without infinite loop.
	loopPkt := []byte{
		0, 0, 0x84, 0x00, 0, 0, 0, 1, 0, 0, 0, 0,
		0xC0, 12, 0, 12, 0, 1, 0, 0, 0, 10, 0, 0,
	}
	if _, ok := parseMDNSResponse(loopPkt); ok {
		t.Fatal("expected malformed pointer loop packet to fail")
	}
}

func TestDiscoveryVisibilityModesAndSpoofingDefense(t *testing.T) {
	dir := t.TempDir()
	aliceID, err := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "alice.json"))
	if err != nil {
		t.Fatalf("alice identity: %v", err)
	}
	bobID, err := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "bob.json"))
	if err != nil {
		t.Fatalf("bob identity: %v", err)
	}

	hub := newMockHub()
	var visMu sync.Mutex
	aliceVis := devices.VisibilityHidden
	bobVis := devices.VisibilityEveryone

	var nowMu sync.Mutex
	now := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}

	alice := NewService(Config{
		HostIdentity:   aliceID,
		VisibilityMode: func() devices.VisibilityMode { visMu.Lock(); defer visMu.Unlock(); return aliceVis },
		LocalEndpoint: func() LocalEndpoint {
			return LocalEndpoint{
				DeviceName:    "Alice-\x00Workstation",
				OS:            "windows",
				Version:       "0.2.8.0",
				Port:          43821,
				InterfaceName: "Ethernet",
				LocalIP:       "192.168.1.20",
			}
		},
		Transport:        hub,
		AnnounceInterval: 50 * time.Millisecond,
		PeerTTL:          5 * time.Second,
		Now:              clock,
	})
	bob := NewService(Config{
		HostIdentity:   bobID,
		VisibilityMode: func() devices.VisibilityMode { visMu.Lock(); defer visMu.Unlock(); return bobVis },
		LocalEndpoint: func() LocalEndpoint {
			return LocalEndpoint{
				DeviceName:    "Bob MacBook",
				OS:            "darwin",
				Version:       "0.2.8.0",
				Port:          43821,
				InterfaceName: "en0",
				LocalIP:       "192.168.1.30",
			}
		},
		Transport:        hub,
		AnnounceInterval: 50 * time.Millisecond,
		PeerTTL:          5 * time.Second,
		Now:              clock,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Alice in Hidden emits ZERO packets and ignores unsolicited packets.
	alice.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	hub.mu.Lock()
	sentWhileHidden := len(hub.sent)
	hub.mu.Unlock()
	if sentWhileHidden != 0 {
		t.Fatalf("Hidden mode sent %d packets, want 0", sentWhileHidden)
	}

	// 2. Start Bob in Everyone; Alice discovers Bob ONLY when Alice switches away from Hidden.
	bob.Start(ctx)
	time.Sleep(120 * time.Millisecond)
	if peers := alice.Peers(); len(peers) != 0 {
		t.Fatalf("Hidden Alice unexpectedly stored peers: %+v", peers)
	}

	visMu.Lock()
	aliceVis = devices.VisibilityEveryone
	visMu.Unlock()
	alice.TriggerScan()
	bob.TriggerScan()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(alice.Peers()) == 1 && len(bob.Peers()) == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	alicePeers := alice.Peers()
	bobPeers := bob.Peers()
	if len(alicePeers) != 1 || alicePeers[0].DeviceName != "Bob MacBook" || alicePeers[0].OS != "darwin" {
		t.Fatalf("Alice peers = %+v", alicePeers)
	}
	if len(bobPeers) != 1 || strings.Contains(bobPeers[0].DeviceName, "\x00") || bobPeers[0].OS != "windows" {
		t.Fatalf("Bob peers = %+v", bobPeers)
	}

	// 3. HostID spoofing attempt (different PublicKey claiming Bob's HostID) must be dropped.
	spoofID, _ := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "spoof.json"))
	spoofPkt, _ := json.Marshal(beaconEnvelope{
		Kind:       "announce",
		HostID:     bobID.HostID,
		PublicKey:  spoofID.PublicKey,
		DeviceName: "Evil Spoofer",
		OS:         "linux",
		Port:       43821,
		IPs:        []string{"192.168.1.99"},
		Timestamp:  clock().Unix(),
		Signature:  spoofID.Sign(signAnnouncementPayload(bobID.HostID, spoofID.PublicKey, "Evil Spoofer", "linux", "0.2.8.0", 43821, []string{"192.168.1.99"}, clock().Unix())),
	})
	alice.handleBeaconPacket(nil, spoofPkt, net.ParseIP("192.168.1.99"))
	if p := alice.Peers(); len(p) != 1 || p[0].DeviceName != "Bob MacBook" {
		t.Fatalf("spoof packet corrupted peer table: %+v", p)
	}

	// 3b. Replay of Bob's genuine signed announce from a DIFFERENT attacker IP (192.168.1.99) MUST be dropped!
	bobTS := clock().Unix()
	bobSignedPayload := signAnnouncementPayload(bobID.HostID, bobID.PublicKey, "Bob MacBook", "darwin", "0.2.8.0", 43821, []string{"192.168.1.30"}, bobTS)
	replayedPkt, _ := json.Marshal(beaconEnvelope{
		Kind:       "announce",
		HostID:     bobID.HostID,
		PublicKey:  bobID.PublicKey,
		DeviceName: "Bob MacBook",
		OS:         "darwin",
		Version:    "0.2.8.0",
		Port:       43821,
		IPs:        []string{"192.168.1.30"},
		Timestamp:  bobTS,
		Signature:  bobID.Sign(bobSignedPayload),
	})
	alice.handleBeaconPacket(nil, replayedPkt, net.ParseIP("192.168.1.99"))
	if p := alice.Peers(); len(p) != 1 || p[0].Address != "192.168.1.30" {
		t.Fatalf("attacker IP replay hijacked peer address: %+v", p)
	}

	// 4. Stale peer eviction clears expired entries.
	nowMu.Lock()
	now = now.Add(6 * time.Second)
	nowMu.Unlock()
	visMu.Lock()
	bobVis = devices.VisibilityHidden
	visMu.Unlock()
	if p := alice.Peers(); len(p) != 0 {
		t.Fatalf("expected stale peer eviction after TTL, got %+v", p)
	}
}

func TestDiscoveryTrustedOnlyNonIdentifyingProbes(t *testing.T) {
	dir := t.TempDir()
	aliceID, _ := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "alice.json"))
	bobID, _ := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "bob.json"))

	aliceStore, _ := devices.NewStore("")
	bobStore, _ := devices.NewStore("")
	bobPub, _ := hex.DecodeString(bobID.PublicKey)
	alicePub, _ := hex.DecodeString(aliceID.PublicKey)
	_, _ = aliceStore.Add("Bob Desktop", bobPub, false)
	_, _ = bobStore.Add("Alice Desktop", alicePub, true)

	hub := newMockHub()
	alice := NewService(Config{
		HostIdentity:   aliceID,
		TrustedDevices: func() []devices.Device { return aliceStore.List() },
		VisibilityMode: func() devices.VisibilityMode { return devices.VisibilityTrustedOnly },
		LocalEndpoint: func() LocalEndpoint {
			return LocalEndpoint{DeviceName: "SECRET_ALICE_HOST", OS: "linux", Port: 44444, InterfaceName: "wlan0", LocalIP: "192.168.2.10"}
		},
		Transport:        hub,
		AnnounceInterval: 40 * time.Millisecond,
	})
	bob := NewService(Config{
		HostIdentity:   bobID,
		TrustedDevices: func() []devices.Device { return bobStore.List() },
		VisibilityMode: func() devices.VisibilityMode { return devices.VisibilityHidden },
		LocalEndpoint: func() LocalEndpoint {
			return LocalEndpoint{DeviceName: "Bob", OS: "windows", Port: 43821, InterfaceName: "Ethernet", LocalIP: "192.168.2.20"}
		},
		Transport:        hub,
		AnnounceInterval: 40 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bob.Start(ctx)
	// Let Bob receive while Hidden? Wait: VisibilityHidden ignores unsolicited packets, so switch Bob to TrustedOnly!
	cancel()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	bob2 := NewService(Config{
		HostIdentity:   bobID,
		TrustedDevices: func() []devices.Device { return bobStore.List() },
		VisibilityMode: func() devices.VisibilityMode { return devices.VisibilityTrustedOnly },
		LocalEndpoint: func() LocalEndpoint {
			return LocalEndpoint{DeviceName: "Bob", OS: "windows", Port: 43821, InterfaceName: "Ethernet", LocalIP: "192.168.2.20"}
		},
		Transport:        hub,
		AnnounceInterval: 40 * time.Millisecond,
	})
	bob2.Start(ctx2)
	alice.Start(ctx2)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bob2.Peers()) == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	peers := bob2.Peers()
	if len(peers) != 1 || !peers[0].Trusted || peers[0].Port != 44444 || !peers[0].AutoApprove {
		t.Fatalf("Bob failed to resolve trusted probe from Alice: %+v", peers)
	}

	// Verify wire packets contain ZERO identifiable plaintext (no SECRET_ALICE_HOST, no Alice HostID, no PublicKey).
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.sent) == 0 {
		t.Fatal("expected probe packets on hub")
	}
	for _, raw := range hub.sent {
		s := string(raw)
		if strings.Contains(s, "SECRET_ALICE_HOST") || strings.Contains(s, aliceID.HostID) || strings.Contains(s, aliceID.PublicKey) {
			t.Fatalf("trusted probe leaked identifiable metadata on wire: %s", s)
		}
	}

	// Verify an attacker knowing BOTH public keys (pubA, pubB) CANNOT forge a trusted probe!
	fakeSalt := make([]byte, 16)
	epoch := time.Now().UTC().Unix() / 60
	fakeTag, fakeMask := computeProbeTagAndMask([]byte("01234567890123456789012345678901"), epoch, fakeSalt, aliceID.HostID, "192.168.2.99", 43821)
	forgedPkt, _ := json.Marshal(beaconEnvelope{
		Kind:   "probe",
		Epoch:  epoch,
		Salt:   hex.EncodeToString(fakeSalt),
		Probes: []trustedProbe{{Tag: fakeTag, MetaMask: fakeMask}},
	})
	bob2.handleBeaconPacket(nil, forgedPkt, net.ParseIP("192.168.2.99"))
	for _, p := range bob2.Peers() {
		if p.Address == "192.168.2.99" {
			t.Fatalf("forged trusted probe hijacked Alice's IP: %+v", p)
		}
	}

	// Verify TrustedOnly ignores unsolicited announcements from unknown strangers.
	strangerID, _ := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "stranger.json"))
	ts := time.Now().UTC().Unix()
	strangerSig := strangerID.Sign(signAnnouncementPayload(strangerID.HostID, strangerID.PublicKey, "Bob Desktop", "linux", "0.2.8.0", 43821, []string{"192.168.2.88"}, ts))
	strangerPkt, _ := json.Marshal(beaconEnvelope{
		Kind:       "announce",
		HostID:     strangerID.HostID,
		PublicKey:  strangerID.PublicKey,
		DeviceName: "Bob Desktop",
		OS:         "linux",
		Version:    "0.2.8.0",
		Port:       43821,
		IPs:        []string{"192.168.2.88"},
		Timestamp:  ts,
		Signature:  strangerSig,
	})
	alice.handleBeaconPacket(nil, strangerPkt, net.ParseIP("192.168.2.88"))
	for _, peer := range alice.Peers() {
		if peer.HostID == strangerID.HostID {
			t.Fatalf("TrustedOnly mode accepted stranger announcement: %+v", peer)
		}
	}
}

func TestDiscoveryFlagsSpoofedTrustedNameInEveryoneMode(t *testing.T) {
	dir := t.TempDir()
	aliceID, _ := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "alice.json"))
	bobID, _ := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "bob.json"))
	evilID, _ := devices.LoadOrCreateHostIdentity(filepath.Join(dir, "evil.json"))

	aliceStore, _ := devices.NewStore("")
	bobPub, _ := hex.DecodeString(bobID.PublicKey)
	_, _ = aliceStore.Add("Bob Workstation", bobPub, true)

	alice := NewService(Config{
		HostIdentity:   aliceID,
		TrustedDevices: func() []devices.Device { return aliceStore.List() },
		VisibilityMode: func() devices.VisibilityMode { return devices.VisibilityEveryone },
	})

	ts := time.Now().UTC().Unix()
	evilSig := evilID.Sign(signAnnouncementPayload(evilID.HostID, evilID.PublicKey, "Bob Workstation", "linux", "0.2.8.0", 43821, []string{"192.168.1.99"}, ts))
	evilPkt, _ := json.Marshal(beaconEnvelope{
		Kind:       "announce",
		HostID:     evilID.HostID,
		PublicKey:  evilID.PublicKey,
		DeviceName: "Bob Workstation",
		OS:         "linux",
		Version:    "0.2.8.0",
		Port:       43821,
		IPs:        []string{"192.168.1.99"},
		Timestamp:  ts,
		Signature:  evilSig,
	})
	alice.handleBeaconPacket(nil, evilPkt, net.ParseIP("192.168.1.99"))
	peers := alice.Peers()
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %+v", peers)
	}
	if peers[0].Trusted {
		t.Fatalf("attacker reusing trusted DeviceName marked Trusted=true!")
	}
	if !peers[0].NameConflictWarning {
		t.Fatalf("expected NameConflictWarning=true for attacker mimicking trusted name, got %+v", peers[0])
	}
}
