package discovery

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"golang.org/x/net/ipv4"
)

const (
	// BeaconMulticastV4 is the Flitnix LAN multicast group used alongside mDNS.
	BeaconMulticastV4 = "239.255.43.82"
	// BeaconPort is the UDP port for LAN peer discovery beacons.
	BeaconPort = 43822
	// MDNSMulticastV4 is the standard IPv4 mDNS multicast address.
	MDNSMulticastV4 = "224.0.0.251"
	// MDNSPort is the standard mDNS UDP port.
	MDNSPort = 5353

	DefaultAnnounceInterval = 4 * time.Second
	DefaultPeerTTL          = 18 * time.Second
	MaxDiscoveredPeers      = 64
	maxAnnouncementAge      = 2 * time.Minute
)

// PacketKind distinguishes mDNS DNS-SD packets from JSON LAN beacons.
type PacketKind string

const (
	PacketMDNS   PacketKind = "mdns"
	PacketBeacon PacketKind = "beacon"
)

// Peer represents one sanitized, cryptographically bound desktop instance on the LAN.
type Peer struct {
	HostID              string    `json:"hostId"`
	PublicKey           string    `json:"publicKey"`
	DeviceName          string    `json:"deviceName"`
	OS                  string    `json:"os"`
	Version             string    `json:"version"`
	Address             string    `json:"address"`
	Port                int       `json:"port"`
	Addresses           []string  `json:"addresses,omitempty"`
	Trusted             bool      `json:"trusted"`
	AutoApprove         bool      `json:"autoApprove"`
	NameConflictWarning bool      `json:"nameConflictWarning,omitempty"`
	InterfaceName       string    `json:"interfaceName,omitempty"`
	LastSeen            time.Time `json:"lastSeen"`
}

// LocalEndpoint describes the currently selected local LAN interface and service port.
type LocalEndpoint struct {
	DeviceName    string
	OS            string
	Version       string
	Port          int
	InterfaceName string
	InterfaceType string
	LocalIP       string
}

// Config configures a Service.
type Config struct {
	HostIdentity     *devices.HostIdentity
	TrustedDevices   func() []devices.Device
	VisibilityMode   func() devices.VisibilityMode
	LocalEndpoint    func() LocalEndpoint
	Transport        NetworkTransport
	AnnounceInterval time.Duration
	PeerTTL          time.Duration
	Now              func() time.Time
	OnPeersChanged   func([]Peer)
}

// NetworkTransport abstracts UDP/multicast socket lifecycle for deterministic testing.
type NetworkTransport interface {
	Open(ifaceName string, localIP net.IP) (PacketSocket, error)
}

// PacketSocket sends and receives LAN discovery packets on a single bound interface.
type PacketSocket interface {
	SendMDNS(pkt []byte) error
	SendBeacon(pkt []byte) error
	Recv(ctx context.Context) (pkt []byte, srcIP net.IP, kind PacketKind, err error)
	Close() error
}

type beaconEnvelope struct {
	Kind       string         `json:"kind"`
	HostID     string         `json:"hostId,omitempty"`
	PublicKey  string         `json:"publicKey,omitempty"`
	DeviceName string         `json:"deviceName,omitempty"`
	OS         string         `json:"os,omitempty"`
	Version    string         `json:"version,omitempty"`
	Port       int            `json:"port,omitempty"`
	IPs        []string       `json:"ips,omitempty"`
	Timestamp  int64          `json:"ts,omitempty"`
	Signature  string         `json:"sig,omitempty"`
	Epoch      int64          `json:"epoch,omitempty"`
	Salt       string         `json:"salt,omitempty"`
	Probes     []trustedProbe `json:"probes,omitempty"`
}

type trustedProbe struct {
	Tag      string `json:"tag"`
	MetaMask string `json:"m"`
}

// Service advertises the local Flitnix instance (when visibility permits) and discovers nearby peers.
type Service struct {
	cfg Config
	now func() time.Time

	mu             sync.RWMutex
	peers          map[string]Peer
	keyByHostID    map[string]string
	lastAnnounceTS map[string]int64
	running        bool
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	scanCh         chan struct{}
	rebindCh       chan struct{}
	lastIface      string
	lastIP         string
}

// NewService creates a nearby LAN discovery service.
func NewService(cfg Config) *Service {
	if cfg.AnnounceInterval <= 0 {
		cfg.AnnounceInterval = DefaultAnnounceInterval
	}
	if cfg.PeerTTL <= 0 {
		cfg.PeerTTL = DefaultPeerTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Transport == nil {
		cfg.Transport = udpMulticastTransport{}
	}
	return &Service{
		cfg:            cfg,
		now:            cfg.Now,
		peers:          make(map[string]Peer),
		keyByHostID:    make(map[string]string),
		lastAnnounceTS: make(map[string]int64),
		scanCh:         make(chan struct{}, 1),
		rebindCh:       make(chan struct{}, 1),
	}
}

// Start begins background LAN discovery and visibility-aware announcements.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.running = true
	s.wg.Add(1)
	s.mu.Unlock()

	go s.run(runCtx)
}

// Stop shuts down sockets and background discovery loops cleanly.
func (s *Service) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	cancel := s.cancel
	s.running = false
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// TriggerScan requests an immediate discovery query / announcement cycle.
func (s *Service) TriggerScan() {
	select {
	case s.scanCh <- struct{}{}:
	default:
	}
}

// NotifyEndpointChanged prompts discovery to rebind sockets if interface/IP changed or visibility updated.
func (s *Service) NotifyEndpointChanged() {
	select {
	case s.rebindCh <- struct{}{}:
	default:
	}
}

// Peers returns a snapshot of currently visible, non-expired nearby computers, updating trust status live.
func (s *Service) Peers() []Peer {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(now)
	return s.snapshotLocked()
}

func (s *Service) snapshotLocked() []Peer {
	trustedByPub := make(map[string]devices.Device)
	trustedByName := make(map[string]string)
	if s.cfg.TrustedDevices != nil {
		for _, dev := range s.cfg.TrustedDevices() {
			pk := strings.ToLower(dev.PublicKey)
			trustedByPub[pk] = dev
			if clean := strings.ToLower(devices.SanitizeDeviceName(dev.Name)); clean != "" {
				trustedByName[clean] = pk
			}
		}
	}
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		pkLower := strings.ToLower(p.PublicKey)
		if dev, ok := trustedByPub[pkLower]; ok {
			p.Trusted = true
			p.AutoApprove = dev.AutoApprove
			p.NameConflictWarning = false
			if dev.Name != "" && p.DeviceName == "" {
				p.DeviceName = dev.Name
			}
		} else {
			p.Trusted = false
			p.AutoApprove = false
			cleanName := strings.ToLower(devices.SanitizeDeviceName(p.DeviceName))
			if expectedPK, collides := trustedByName[cleanName]; collides && expectedPK != pkLower {
				p.NameConflictWarning = true
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Trusted != out[j].Trusted {
			return out[i].Trusted
		}
		if out[i].DeviceName != out[j].DeviceName {
			return out[i].DeviceName < out[j].DeviceName
		}
		return out[i].HostID < out[j].HostID
	})
	return out
}

func (s *Service) pruneExpiredLocked(now time.Time) bool {
	changed := false
	for id, p := range s.peers {
		if now.Sub(p.LastSeen) > s.cfg.PeerTTL {
			delete(s.peers, id)
			changed = true
		}
	}
	return changed
}

func (s *Service) visibilityMode() devices.VisibilityMode {
	if s.cfg.VisibilityMode != nil {
		return s.cfg.VisibilityMode()
	}
	return devices.VisibilityTrustedOnly
}

func (s *Service) localEndpoint() LocalEndpoint {
	if s.cfg.LocalEndpoint != nil {
		ep := s.cfg.LocalEndpoint()
		if ep.OS == "" {
			ep.OS = runtime.GOOS
		}
		return ep
	}
	return LocalEndpoint{OS: runtime.GOOS}
}

func isVPNOrVirtualName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	for _, prefix := range []string{
		"tun", "tap", "wg", "tailscale", "utun", "ppp", "ipsec", "zt", "nord", "proton", "mullvad", "veth", "docker", "br-", "virbr",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func (s *Service) run(ctx context.Context) {
	defer s.wg.Done()

	var sock PacketSocket
	var recvCancel context.CancelFunc
	var recvWG sync.WaitGroup

	closeSocket := func() {
		if recvCancel != nil {
			recvCancel()
			recvCancel = nil
		}
		if sock != nil {
			_ = sock.Close()
			sock = nil
		}
		recvWG.Wait()
	}
	defer closeSocket()

	rebind := func() {
		ep := s.localEndpoint()
		if ep.LocalIP == "" || strings.EqualFold(ep.InterfaceType, "VPN") || isVPNOrVirtualName(ep.InterfaceName) {
			closeSocket()
			s.mu.Lock()
			s.lastIface = ""
			s.lastIP = ""
			s.mu.Unlock()
			return
		}
		ip := net.ParseIP(ep.LocalIP)
		if ip == nil || ip.IsLoopback() && !strings.HasPrefix(ep.InterfaceName, "lo") {
			// allow explicit loopback in tests if InterfaceName starts with "lo" or mock transport
			if ep.LocalIP != "127.0.0.1" && ep.LocalIP != "::1" {
				closeSocket()
				return
			}
		}

		s.mu.RLock()
		same := sock != nil && s.lastIface == ep.InterfaceName && s.lastIP == ep.LocalIP
		s.mu.RUnlock()
		if same {
			return
		}

		closeSocket()
		opened, err := s.cfg.Transport.Open(ep.InterfaceName, ip)
		if err != nil {
			return
		}
		sock = opened
		s.mu.Lock()
		s.lastIface = ep.InterfaceName
		s.lastIP = ep.LocalIP
		s.mu.Unlock()

		var rCtx context.Context
		rCtx, recvCancel = context.WithCancel(ctx)
		recvWG.Add(1)
		go func(sc PacketSocket, c context.Context) {
			defer recvWG.Done()
			for {
				pkt, srcIP, kind, rErr := sc.Recv(c)
				if rErr != nil {
					return
				}
				if srcIP == nil {
					continue
				}
				if kind == PacketMDNS {
					if isMDNSFlitnixQuery(pkt) {
						s.respondIfAllowed(sc)
					} else if ann, ok := parseMDNSResponse(pkt); ok {
						s.handleAnnouncement(ann, srcIP)
					}
				} else if kind == PacketBeacon {
					s.handleBeaconPacket(sc, pkt, srcIP)
				}
			}
		}(sock, rCtx)
	}

	rebind()
	s.broadcastCycle(sock)

	announceTick := time.NewTicker(s.cfg.AnnounceInterval)
	defer announceTick.Stop()
	pruneTick := time.NewTicker(2 * time.Second)
	defer pruneTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.rebindCh:
			rebind()
			s.broadcastCycle(sock)
		case <-s.scanCh:
			rebind()
			s.sendQueries(sock)
			s.broadcastCycle(sock)
		case <-announceTick.C:
			rebind()
			s.broadcastCycle(sock)
		case <-pruneTick.C:
			s.mu.Lock()
			changed := s.pruneExpiredLocked(s.now())
			var snap []Peer
			if changed && s.cfg.OnPeersChanged != nil {
				snap = s.snapshotLocked()
			}
			s.mu.Unlock()
			if changed && s.cfg.OnPeersChanged != nil {
				s.cfg.OnPeersChanged(snap)
			}
		}
	}
}

func (s *Service) sendQueries(sock PacketSocket) {
	if sock == nil || s.visibilityMode() == devices.VisibilityHidden {
		return
	}
	if q, err := buildMDNSQuery(); err == nil {
		_ = sock.SendMDNS(q)
	}
	if b, err := json.Marshal(beaconEnvelope{Kind: "query"}); err == nil {
		_ = sock.SendBeacon(b)
	}
}

func (s *Service) respondIfAllowed(sock PacketSocket) {
	if sock == nil {
		return
	}
	mode := s.visibilityMode()
	if mode == devices.VisibilityEveryone || mode == devices.VisibilityTemporaryEveryone {
		s.broadcastPublic(sock)
	}
}

func (s *Service) broadcastCycle(sock PacketSocket) {
	if sock == nil {
		return
	}
	mode := s.visibilityMode()
	switch mode {
	case devices.VisibilityHidden:
		// Hidden emits zero discovery packets.
		return
	case devices.VisibilityTrustedOnly:
		s.broadcastTrustedProbe(sock)
	case devices.VisibilityTemporaryEveryone, devices.VisibilityEveryone:
		s.broadcastPublic(sock)
	}
}

func signAnnouncementPayload(hostID, pubKeyHex, name, osName, ver string, port int, ips []string, ts int64) []byte {
	cleanIPs := normalizeSortedIPStrings(ips)
	return []byte(fmt.Sprintf("flitnix-disc-v2|%s|%s|%s|%s|%s|%d|%s|%d", hostID, pubKeyHex, name, osName, ver, port, strings.Join(cleanIPs, ","), ts))
}

func normalizeSortedIPStrings(ips []string) []string {
	seen := make(map[string]struct{}, len(ips))
	out := make([]string, 0, len(ips))
	for _, raw := range ips {
		parsed := net.ParseIP(strings.TrimSpace(raw))
		if parsed == nil || parsed.IsUnspecified() || parsed.IsMulticast() {
			continue
		}
		s := parsed.String()
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func ipStringsFromNet(ips []net.IP) []string {
	raw := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip != nil {
			raw = append(raw, ip.String())
		}
	}
	return normalizeSortedIPStrings(raw)
}

var curve25519P = func() *big.Int {
	p := new(big.Int).Lsh(big.NewInt(1), 255)
	return p.Sub(p, big.NewInt(19))
}()

func ed25519PrivateToX25519(privHex string) (*ecdh.PrivateKey, bool) {
	raw, err := hex.DecodeString(privHex)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, false
	}
	h := sha512.Sum512(raw[:32])
	h[0] &= 248
	h[31] &= 127
	h[31] |= 64
	k, err := ecdh.X25519().NewPrivateKey(h[:32])
	if err != nil {
		return nil, false
	}
	return k, true
}

func ed25519PublicToX25519(pubHex string) (*ecdh.PublicKey, bool) {
	raw, err := hex.DecodeString(pubHex)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, false
	}
	// Little-endian y coordinate in 255 bits; clear sign bit (MSB of last byte).
	var le [32]byte
	copy(le[:], raw)
	le[31] &^= 0x80
	var be [32]byte
	for i := 0; i < 32; i++ {
		be[i] = le[31-i]
	}
	y := new(big.Int).SetBytes(be[:])
	if y.Cmp(curve25519P) >= 0 {
		return nil, false
	}
	one := big.NewInt(1)
	den := new(big.Int).Sub(curve25519P, y)
	den.Add(den, one)
	den.Mod(den, curve25519P)
	if den.Sign() == 0 {
		return nil, false
	}
	denInv := new(big.Int).ModInverse(den, curve25519P)
	if denInv == nil {
		return nil, false
	}
	num := new(big.Int).Add(one, y)
	num.Mod(num, curve25519P)
	u := new(big.Int).Mul(num, denInv)
	u.Mod(u, curve25519P)

	uBE := u.Bytes()
	var uLE [32]byte
	for i := 0; i < len(uBE); i++ {
		uLE[i] = uBE[len(uBE)-1-i]
	}
	pk, err := ecdh.X25519().NewPublicKey(uLE[:])
	if err != nil {
		return nil, false
	}
	return pk, true
}

func verifyKeyAndSignature(hostID, pubKeyHex, sigHex string, payload []byte) bool {
	pubBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return false
	}
	expectedIDBytes := sha256.Sum256(pubBytes)
	expectedHostID := hex.EncodeToString(expectedIDBytes[:])
	if !strings.EqualFold(hostID, expectedHostID) {
		return false
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pubBytes), payload, sigBytes)
}

func (s *Service) broadcastPublic(sock PacketSocket) {
	if s.cfg.HostIdentity == nil {
		return
	}
	ep := s.localEndpoint()
	if ep.Port <= 0 || ep.LocalIP == "" {
		return
	}
	ip := net.ParseIP(ep.LocalIP)
	if ip == nil {
		return
	}
	name := devices.SanitizeDeviceName(ep.DeviceName)
	osName := sanitizeToken(ep.OS, "unknown")
	ver := sanitizeToken(ep.Version, "dev")
	ts := s.now().UTC().Unix()
	ips := []string{ip.String()}
	payload := signAnnouncementPayload(s.cfg.HostIdentity.HostID, s.cfg.HostIdentity.PublicKey, name, osName, ver, ep.Port, ips, ts)
	sig := s.cfg.HostIdentity.Sign(payload)

	env := beaconEnvelope{
		Kind:       "announce",
		HostID:     s.cfg.HostIdentity.HostID,
		PublicKey:  s.cfg.HostIdentity.PublicKey,
		DeviceName: name,
		OS:         osName,
		Version:    ver,
		Port:       ep.Port,
		IPs:        ips,
		Timestamp:  ts,
		Signature:  sig,
	}
	if b, err := json.Marshal(env); err == nil {
		_ = sock.SendBeacon(b)
	}
	if mdnsPkt, err := buildMDNSResponse(mdnsAnnouncement{
		HostID:     env.HostID,
		PublicKey:  env.PublicKey,
		DeviceName: env.DeviceName,
		OS:         env.OS,
		Version:    env.Version,
		Port:       uint16(ep.Port),
		IPs:        []net.IP{ip},
		Timestamp:  ts,
		Sig:        sig,
	}, 20); err == nil {
		_ = sock.SendMDNS(mdnsPkt)
	}
}

// derivePairProbeKey computes a true secret shared key via Ed25519->X25519 Diffie-Hellman
// so passive eavesdroppers who only observe public keys cannot correlate or forge probes.
func derivePairProbeKey(localHost *devices.HostIdentity, peerPubHex string) ([]byte, bool) {
	if localHost == nil {
		return nil, false
	}
	privX, ok := ed25519PrivateToX25519(localHost.PrivateKey)
	if !ok {
		return nil, false
	}
	pubX, ok := ed25519PublicToX25519(peerPubHex)
	if !ok {
		return nil, false
	}
	sharedSecret, err := privX.ECDH(pubX)
	if err != nil {
		return nil, false
	}
	h := sha256.New()
	h.Write([]byte("flitnix-probe-key-v2|"))
	h.Write(sharedSecret)
	return h.Sum(nil), true
}

func computeProbeTagAndMask(sharedKey []byte, epoch int64, salt []byte, senderHostID, senderIP string, port int) (tagHex, maskHex string) {
	saltHex := hex.EncodeToString(salt)
	mac := hmac.New(sha256.New, sharedKey)
	mac.Write([]byte("flitnix-probe-v2|"))
	mac.Write([]byte(saltHex))
	mac.Write([]byte("|"))
	mac.Write([]byte(strconv.FormatInt(epoch, 10)))
	mac.Write([]byte("|"))
	mac.Write([]byte(strings.ToLower(senderHostID)))
	mac.Write([]byte("|"))
	mac.Write([]byte(senderIP))
	mac.Write([]byte("|"))
	mac.Write([]byte(strconv.Itoa(port)))
	tag := mac.Sum(nil)[:16]

	maskMAC := hmac.New(sha256.New, sharedKey)
	maskMAC.Write([]byte("flitnix-probe-meta-v2|"))
	maskMAC.Write([]byte(saltHex))
	pad := maskMAC.Sum(nil)
	var plain [2]byte
	binary.BigEndian.PutUint16(plain[:], uint16(port))
	plain[0] ^= pad[0]
	plain[1] ^= pad[1]
	return hex.EncodeToString(tag), hex.EncodeToString(plain[:])
}

// broadcastTrustedProbe emits non-identifying unlinkable ECDH-authenticated HMAC tags for already-paired peers ONLY.
func (s *Service) broadcastTrustedProbe(sock PacketSocket) {
	if s.cfg.HostIdentity == nil || s.cfg.TrustedDevices == nil {
		return
	}
	ep := s.localEndpoint()
	if ep.Port <= 0 || ep.LocalIP == "" {
		return
	}
	devs := s.cfg.TrustedDevices()
	if len(devs) == 0 {
		return
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return
	}
	epoch := s.now().UTC().Unix() / 60
	probes := make([]trustedProbe, 0, len(devs))
	for _, dev := range devs {
		sharedKey, ok := derivePairProbeKey(s.cfg.HostIdentity, dev.PublicKey)
		if !ok {
			continue
		}
		tagHex, maskHex := computeProbeTagAndMask(sharedKey, epoch, salt[:], s.cfg.HostIdentity.HostID, ep.LocalIP, ep.Port)
		probes = append(probes, trustedProbe{Tag: tagHex, MetaMask: maskHex})
	}
	if len(probes) == 0 {
		return
	}
	env := beaconEnvelope{
		Kind:   "probe",
		Epoch:  epoch,
		Salt:   hex.EncodeToString(salt[:]),
		Probes: probes,
	}
	if b, err := json.Marshal(env); err == nil {
		_ = sock.SendBeacon(b)
	}
}

func (s *Service) handleBeaconPacket(sock PacketSocket, pkt []byte, srcIP net.IP) {
	if len(pkt) > 16384 {
		return
	}
	var env beaconEnvelope
	if err := json.Unmarshal(pkt, &env); err != nil {
		return
	}
	switch env.Kind {
	case "query":
		s.respondIfAllowed(sock)
	case "announce":
		ips := make([]net.IP, 0, len(env.IPs))
		for _, sIP := range env.IPs {
			if parsed := net.ParseIP(strings.TrimSpace(sIP)); parsed != nil {
				ips = append(ips, parsed)
			}
		}
		s.handleAnnouncement(mdnsAnnouncement{
			HostID:     env.HostID,
			PublicKey:  env.PublicKey,
			DeviceName: env.DeviceName,
			OS:         env.OS,
			Version:    env.Version,
			Port:       uint16(env.Port),
			IPs:        ips,
			Timestamp:  env.Timestamp,
			Sig:        env.Signature,
		}, srcIP)
	case "probe":
		s.handleTrustedProbe(env, srcIP)
	}
}

func (s *Service) handleTrustedProbe(env beaconEnvelope, srcIP net.IP) {
	if s.cfg.HostIdentity == nil || s.cfg.TrustedDevices == nil || srcIP == nil {
		return
	}
	if s.visibilityMode() == devices.VisibilityHidden {
		return
	}
	salt, err := hex.DecodeString(env.Salt)
	if err != nil || len(salt) != 16 || len(env.Probes) == 0 {
		return
	}
	curEpoch := s.now().UTC().Unix() / 60
	if env.Epoch < curEpoch-1 || env.Epoch > curEpoch+1 {
		return
	}
	saltHex := hex.EncodeToString(salt)
	srcIPStr := srcIP.String()
	devs := s.cfg.TrustedDevices()
	for _, dev := range devs {
		if dev.PublicKey == "" || strings.EqualFold(dev.PublicKey, s.cfg.HostIdentity.PublicKey) {
			continue
		}
		pubBytes, err := hex.DecodeString(dev.PublicKey)
		if err != nil || len(pubBytes) != ed25519.PublicKeySize {
			continue
		}
		peerHostSum := sha256.Sum256(pubBytes)
		peerHostID := hex.EncodeToString(peerHostSum[:])

		sharedKey, ok := derivePairProbeKey(s.cfg.HostIdentity, dev.PublicKey)
		if !ok {
			continue
		}
		maskMAC := hmac.New(sha256.New, sharedKey)
		maskMAC.Write([]byte("flitnix-probe-meta-v2|"))
		maskMAC.Write([]byte(saltHex))
		pad := maskMAC.Sum(nil)

		for _, pr := range env.Probes {
			maskBytes, mErr := hex.DecodeString(pr.MetaMask)
			if mErr != nil || len(maskBytes) != 2 {
				continue
			}
			port := int(binary.BigEndian.Uint16([]byte{maskBytes[0] ^ pad[0], maskBytes[1] ^ pad[1]}))
			if port <= 0 {
				continue
			}
			expectedTag, _ := computeProbeTagAndMask(sharedKey, env.Epoch, salt, peerHostID, srcIPStr, port)
			if len(pr.Tag) == len(expectedTag) && hmac.Equal([]byte(pr.Tag), []byte(expectedTag)) {
				s.upsertPeer(Peer{
					HostID:        peerHostID,
					PublicKey:     strings.ToLower(dev.PublicKey),
					DeviceName:    devices.SanitizeDeviceName(dev.Name),
					OS:            "desktop",
					Address:       srcIPStr,
					Port:          port,
					Addresses:     []string{srcIPStr},
					Trusted:       true,
					AutoApprove:   dev.AutoApprove,
					InterfaceName: s.localEndpoint().InterfaceName,
					LastSeen:      s.now(),
				}, env.Epoch*60)
				return
			}
		}
	}
}

func (s *Service) isTrustedPublicKey(pubKeyHex string) bool {
	if s.cfg.TrustedDevices == nil {
		return false
	}
	for _, dev := range s.cfg.TrustedDevices() {
		if strings.EqualFold(dev.PublicKey, pubKeyHex) {
			return true
		}
	}
	return false
}

func (s *Service) handleAnnouncement(ann mdnsAnnouncement, srcIP net.IP) {
	if s.cfg.HostIdentity != nil && strings.EqualFold(ann.HostID, s.cfg.HostIdentity.HostID) {
		return
	}
	mode := s.visibilityMode()
	if mode == devices.VisibilityHidden {
		return
	}
	if mode == devices.VisibilityTrustedOnly && !s.isTrustedPublicKey(ann.PublicKey) {
		// Suppress unverified stranger announcements while in Trusted Devices Only mode.
		return
	}
	if ann.Port == 0 || ann.HostID == "" || ann.PublicKey == "" || ann.Sig == "" || srcIP == nil {
		return
	}
	now := s.now().UTC()
	diff := now.Unix() - ann.Timestamp
	if diff < 0 {
		diff = -diff
	}
	if time.Duration(diff)*time.Second > maxAnnouncementAge {
		return
	}

	signedIPs := ipStringsFromNet(ann.IPs)
	if len(signedIPs) == 0 {
		return
	}
	// Source IP MUST match one of the Ed25519-signed IPs inside the announcement.
	primaryIP := srcIP.String()
	srcMatchedSigned := false
	for _, sIP := range signedIPs {
		if sIP == primaryIP {
			srcMatchedSigned = true
			break
		}
	}
	if !srcMatchedSigned {
		return
	}

	cleanName := devices.SanitizeDeviceName(ann.DeviceName)
	osName := sanitizeToken(ann.OS, "unknown")
	ver := sanitizeToken(ann.Version, "dev")
	payload := signAnnouncementPayload(ann.HostID, ann.PublicKey, cleanName, osName, ver, int(ann.Port), signedIPs, ann.Timestamp)
	if !verifyKeyAndSignature(ann.HostID, ann.PublicKey, ann.Sig, payload) {
		return
	}

	s.upsertPeer(Peer{
		HostID:        strings.ToLower(ann.HostID),
		PublicKey:     strings.ToLower(ann.PublicKey),
		DeviceName:    cleanName,
		OS:            osName,
		Version:       ver,
		Address:       primaryIP,
		Port:          int(ann.Port),
		Addresses:     []string{primaryIP},
		InterfaceName: s.localEndpoint().InterfaceName,
		LastSeen:      s.now(),
	}, ann.Timestamp)
}

func (s *Service) upsertPeer(p Peer, msgTS int64) {
	now := s.now()
	s.mu.Lock()
	if existingKey, known := s.keyByHostID[p.HostID]; known && !strings.EqualFold(existingKey, p.PublicKey) {
		// Reject public-key spoofing attempts against a seen HostID.
		s.mu.Unlock()
		return
	}
	if prev, active := s.peers[p.HostID]; active && now.Sub(prev.LastSeen) <= s.cfg.PeerTTL {
		if prevTS, hasTS := s.lastAnnounceTS[p.HostID]; hasTS && msgTS < prevTS {
			// Reject stale replay / timestamp rollback for an already-active peer.
			s.mu.Unlock()
			return
		}
	}
	s.keyByHostID[p.HostID] = p.PublicKey
	s.lastAnnounceTS[p.HostID] = msgTS

	if _, exists := s.peers[p.HostID]; !exists && len(s.peers) >= MaxDiscoveredPeers {
		var oldestID string
		var oldestTime time.Time
		for k, v := range s.peers {
			if oldestID == "" || v.LastSeen.Before(oldestTime) {
				oldestID = k
				oldestTime = v.LastSeen
			}
		}
		if oldestID != "" {
			delete(s.peers, oldestID)
		}
	}
	s.peers[p.HostID] = p
	var snap []Peer
	if s.cfg.OnPeersChanged != nil {
		snap = s.snapshotLocked()
	}
	s.mu.Unlock()

	if s.cfg.OnPeersChanged != nil {
		s.cfg.OnPeersChanged(snap)
	}
}

func sanitizeToken(raw, fallback string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
		if b.Len() >= 24 {
			break
		}
	}
	if b.Len() == 0 {
		return fallback
	}
	return b.String()
}

// udpMulticastTransport implements NetworkTransport over standard mDNS + UDP multicast bound to one interface.
type udpMulticastTransport struct{}

type udpPacketSocket struct {
	beaconConn *net.UDPConn
	mdnsConn   *net.UDPConn
	beaconAddr *net.UDPAddr
	mdnsAddr   *net.UDPAddr
	iface      *net.Interface
	packets    chan incomingPacket
	closeOnce  sync.Once
	done       chan struct{}
}

type incomingPacket struct {
	pkt   []byte
	srcIP net.IP
	kind  PacketKind
}

func (udpMulticastTransport) Open(ifaceName string, localIP net.IP) (PacketSocket, error) {
	if localIP == nil || localIP.To4() == nil {
		return nil, errors.New("discovery: valid IPv4 local interface required")
	}
	var iface *net.Interface
	if ifaceName != "" {
		if resolved, err := net.InterfaceByName(ifaceName); err == nil {
			iface = resolved
		}
	}
	beaconGroup := &net.UDPAddr{IP: net.ParseIP(BeaconMulticastV4), Port: BeaconPort}
	beaconConn, err := net.ListenMulticastUDP("udp4", iface, beaconGroup)
	if err != nil {
		return nil, fmt.Errorf("discovery: listen beacon multicast: %w", err)
	}
	if iface != nil {
		_ = ipv4.NewPacketConn(beaconConn).SetMulticastInterface(iface)
	}

	mdnsGroup := &net.UDPAddr{IP: net.ParseIP(MDNSMulticastV4), Port: MDNSPort}
	mdnsConn, _ := net.ListenMulticastUDP("udp4", iface, mdnsGroup)
	if mdnsConn != nil && iface != nil {
		_ = ipv4.NewPacketConn(mdnsConn).SetMulticastInterface(iface)
	}

	sock := &udpPacketSocket{
		beaconConn: beaconConn,
		mdnsConn:   mdnsConn,
		beaconAddr: beaconGroup,
		mdnsAddr:   mdnsGroup,
		iface:      iface,
		packets:    make(chan incomingPacket, 64),
		done:       make(chan struct{}),
	}
	sock.startReaders()
	return sock, nil
}

func (u *udpPacketSocket) startReaders() {
	readLoop := func(conn *net.UDPConn, kind PacketKind) {
		buf := make([]byte, 4096)
		for {
			n, src, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			cp := make([]byte, n)
			copy(cp, buf[:n])
			var ip net.IP
			if src != nil {
				ip = src.IP
			}
			select {
			case u.packets <- incomingPacket{pkt: cp, srcIP: ip, kind: kind}:
			case <-u.done:
				return
			default:
			}
		}
	}
	go readLoop(u.beaconConn, PacketBeacon)
	if u.mdnsConn != nil {
		go readLoop(u.mdnsConn, PacketMDNS)
	}
}

func (u *udpPacketSocket) SendMDNS(pkt []byte) error {
	if u.mdnsConn != nil {
		_, err := u.mdnsConn.WriteToUDP(pkt, u.mdnsAddr)
		return err
	}
	return nil
}

func (u *udpPacketSocket) SendBeacon(pkt []byte) error {
	_, err := u.beaconConn.WriteToUDP(pkt, u.beaconAddr)
	return err
}

func (u *udpPacketSocket) Recv(ctx context.Context) ([]byte, net.IP, PacketKind, error) {
	select {
	case <-ctx.Done():
		return nil, nil, "", ctx.Err()
	case <-u.done:
		return nil, nil, "", errors.New("socket closed")
	case p := <-u.packets:
		return p.pkt, p.srcIP, p.kind, nil
	}
}

func (u *udpPacketSocket) Close() error {
	u.closeOnce.Do(func() {
		close(u.done)
		if u.beaconConn != nil {
			_ = u.beaconConn.Close()
		}
		if u.mdnsConn != nil {
			_ = u.mdnsConn.Close()
		}
	})
	return nil
}
