// Package netenv answers one question for the desktop application: which
// local IPv4 addresses of this machine are suitable for hosting a
// phone-facing transfer session, and which one should be used.
//
// The package is deliberately independent of Wails and of the HTTP server:
// it reads interface state through an injectable [Source] (defaulting to
// the operating system) and every function is a pure decision over that
// state, so unit tests can feed synthetic interfaces.
//
// Suitability rules for a candidate address:
//
//   - The owning interface must be up and be neither loopback nor a
//     point-to-point link; VPN and dial-up links are thus excluded.
//   - The address must be a real IPv4 unicast address on that interface,
//     never in the unspecified (0.0.0.0/8), loopback (127.0.0.0/8),
//     link-local unicast (169.254.0.0/16), multicast (224.0.0.0/4),
//     benchmark (198.18.0.0/15), documentation (192.0.2.0/24,
//     198.51.100.0/24, 203.0.113.0/24), carrier-grade NAT (100.64.0.0/10,
//     where VPN overlays also live), IETF-protocol (192.0.0.0/24),
//     reserved (240.0.0.0/4) or broadcast (255.255.255.255) range.
//
// Selection rules:
//
//   - Exactly one candidate: that one is the sensible default and is used
//     without asking.
//   - Zero candidates: ErrNoCandidate — the user must connect to a network
//     first.
//   - More than one: ErrAmbiguous — the desktop must show the list and
//     make the user choose; this package never guesses.
//
// Nothing in the package hardcodes any interface name or address observed
// on the development machine; every choice comes from the live rules.
package netenv

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

// View is one usable local interface address as presented to the desktop.
// The QR code for a session uses IP, which is the machine's own address on
// that network, never a gateway address.
type View struct {
	// Name is the interface name as the OS reports it, e.g. "wlo1".
	Name string `json:"name"`
	// IP is the dotted-quad IPv4 address, e.g. "192.168.1.42".
	IP string `json:"ip"`
	// CIDR is the address with its prefix length, e.g. "192.168.1.42/24".
	CIDR string `json:"cidr"`
	// FriendlyName is safe to show as the primary connection identity.
	FriendlyName string `json:"friendlyName"`
	// Type is a broad connection type such as Wi-Fi or Ethernet.
	Type string `json:"type"`
	// MetadataAvailable reports whether the OS supplied connection metadata.
	MetadataAvailable bool `json:"metadataAvailable"`
	// ConnectionName is the saved OS connection profile name, when known.
	// It is intentionally separate from FriendlyName/SSID.
	ConnectionName string `json:"connectionName"`
	// FriendlyNameSource identifies whether FriendlyName came from the
	// verified SSID, the active profile fallback, or a generic type label.
	FriendlyNameSource string `json:"friendlyNameSource"`
	// Active is true because this view represents an interface with a
	// currently usable local address.
	Active bool `json:"active"`
}

// Metadata is optional connection identity supplied by a platform adapter.
type Metadata struct {
	SSID           string
	ConnectionName string
	Type           string
}

// Provider is the platform-neutral discovery contract used by the common
// candidate and selection logic. Source is the default implementation and
// remains injectable so platform adapters and tests do not touch the
// transfer engine.
type Provider interface {
	InterfacesFor() ([]net.Interface, error)
	AddressesFor(net.Interface) ([]net.Addr, error)
	MetadataFor(net.Interface) Metadata
}

// Errors surfaced for the three decision failures callers must handle.
var (
	// ErrNoCandidate means no interface address passed the filters.
	ErrNoCandidate = errors.New("netenv: no usable local IPv4 interface found; connect to a local network first")

	// ErrAmbiguous means several interfaces passed; the user must choose.
	ErrAmbiguous = errors.New("netenv: several usable network interfaces found; choose one")

	// ErrGone means a previously selected address is no longer usable on
	// any suitable interface (disconnected or address changed).
	ErrGone = errors.New("netenv: the selected interface address is no longer available; restart the session")
)

// Source supplies the interface data the package reasons about. The
// default implementation calls the operating system; tests inject fakes
// (net.Interface.Addrs cannot be stubbed in place, hence the seam).
type Source struct {
	// Interfaces lists the system's interfaces. nil uses net.Interfaces.
	Interfaces func() ([]net.Interface, error)
	// Addrs lists the addresses of one interface. nil uses the
	// interface's own Addrs method.
	Addrs func(net.Interface) ([]net.Addr, error)
	// Metadata returns optional user-facing connection metadata. A nil
	// function uses the operating-system adapter and never blocks enumeration
	// for more than its bounded command timeout.
	Metadata func(net.Interface) Metadata
}

var _ Provider = Source{}

// DefaultSource builds the operating-system-backed source.
func DefaultSource() Source { return Source{} }

func (s Source) InterfacesFor() ([]net.Interface, error) {
	if s.Interfaces != nil {
		return s.Interfaces()
	}
	return net.Interfaces()
}

func (s Source) AddressesFor(iface net.Interface) ([]net.Addr, error) {
	if s.Addrs != nil {
		return s.Addrs(iface)
	}
	return iface.Addrs()
}

func (s Source) MetadataFor(iface net.Interface) Metadata {
	if s.Metadata != nil {
		return s.Metadata(iface)
	}
	return systemMetadata(iface)
}

func withMetadata(view View, metadata Metadata) View {
	view.Type = strings.TrimSpace(metadata.Type)
	if view.Type == "" {
		view.Type = "Network"
	}
	ssid := strings.TrimSpace(metadata.SSID)
	profile := strings.TrimSpace(metadata.ConnectionName)
	view.FriendlyName = ssid
	switch {
	case ssid != "":
		view.FriendlyNameSource = "SSID"
	case profile != "":
		view.FriendlyName = profile
		view.FriendlyNameSource = "Connection profile"
	default:
		switch view.Type {
		case "Wi-Fi":
			view.FriendlyName = "Wi-Fi network · Name unavailable"
		case "Ethernet":
			view.FriendlyName = "Ethernet connection"
		default:
			view.FriendlyName = view.Type + " connection"
		}
		view.FriendlyNameSource = "Unavailable"
	}
	view.MetadataAvailable = ssid != "" || profile != ""
	view.ConnectionName = profile
	view.Active = true
	return view
}

func typeFromInterface(iface net.Interface) string {
	if iface.Flags&net.FlagPointToPoint != 0 {
		return "VPN"
	}
	return "Network"
}

// Enumerate returns every usable local IPv4 address, ordered
// deterministically: by interface name, then by address value. Unusable
// interfaces and addresses are filtered out; an address-listing failure
// for one interface skips that interface rather than failing the whole
// enumeration, because the remaining interfaces still matter.
func Enumerate(src Provider) ([]View, error) {
	ifaces, err := src.InterfacesFor()
	if err != nil {
		return nil, fmt.Errorf("netenv: listing interfaces: %w", err)
	}

	var views []View
	for _, iface := range ifaces {
		if !interfaceSuitable(iface) {
			continue
		}
		addrs, err := src.AddressesFor(iface)
		if err != nil {
			continue
		}
		metadata := src.MetadataFor(iface)
		var ifaceViews []View
		for _, addr := range addrs {
			if v, ok := addressView(iface, addr); ok {
				v = withMetadata(v, metadata)
				ifaceViews = append(ifaceViews, v)
			}
		}
		sort.Slice(ifaceViews, func(i, j int) bool { return ifaceViews[i].IP < ifaceViews[j].IP })
		views = append(views, ifaceViews...)
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].IP < views[j].IP
	})
	return views, nil
}

// interfaceSuitable reports whether an interface may host the phone
// server: running, not loopback, not point-to-point.
func interfaceSuitable(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 {
		return false
	}
	if iface.Flags&net.FlagLoopback != 0 {
		return false
	}
	if iface.Flags&net.FlagPointToPoint != 0 {
		return false
	}
	return true
}

// addressView converts one interface address into a View if it is a
// suitable IPv4 unicast address; ok reports whether it was.
func addressView(iface net.Interface, addr net.Addr) (View, bool) {
	var ip net.IP
	switch a := addr.(type) {
	case *net.IPNet:
		if a != nil {
			ip = a.IP
		}
	case *net.IPAddr:
		if a != nil {
			ip = a.IP
		}
	default:
		return View{}, false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return View{}, false // an IPv6 (or malformed) address
	}
	parsed := netip.AddrFrom4([4]byte(ip4))
	if !suitableAddress(parsed) {
		return View{}, false
	}
	cidrBits := 32
	if ipn, isNet := addr.(*net.IPNet); isNet && ipn != nil {
		if ones, total := ipn.Mask.Size(); total == 32 && ones >= 0 {
			cidrBits = ones
		}
	}
	quad := ip4.String()
	return View{Name: iface.Name, IP: quad, CIDR: fmt.Sprintf("%s/%d", quad, cidrBits)}, true
}

// suitableAddress reports whether the given (already IPv4) address is a
// usable unicast LAN address.
func suitableAddress(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.Is4() {
		return false
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	for _, pfx := range unsuitablePrefixes {
		if pfx.Contains(ip) {
			return false
		}
	}
	return true
}

// unsuitablePrefixes are non-loopback IPv4 ranges that never make sense
// for a phone-to-laptop LAN transfer session.
var unsuitablePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // unspecified / this network
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT (and VPN overlays)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved / broadcast block
}

// PickDefault chooses the default interface when the decision can be made
// without the user: exactly one candidate. Zero candidates yield
// ErrNoCandidate; several yield ErrAmbiguous naming the alternatives.
func PickDefault(views []View) (View, error) {
	switch len(views) {
	case 0:
		return View{}, ErrNoCandidate
	case 1:
		return views[0], nil
	default:
		summary := make([]string, 0, len(views))
		for _, v := range views {
			summary = append(summary, v.Name+" ("+v.IP+")")
		}
		return View{}, fmt.Errorf("%w: %s", ErrAmbiguous, strings.Join(summary, ", "))
	}
}

// InterfaceFor revalidates that ip is currently a suitable address of a
// suitable interface. It must be called immediately before starting a
// session: a hotspot disconnect or DHCP address change invalidates the
// previous choice and returns ErrGone.
func InterfaceFor(src Provider, ip string) (View, error) {
	target, err := netip.ParseAddr(ip)
	if err != nil {
		return View{}, fmt.Errorf("netenv: %q is not an IP address: %w", ip, err)
	}
	views, err := Enumerate(src)
	if err != nil {
		return View{}, err
	}
	for _, v := range views {
		addr, perr := netip.ParseAddr(v.IP)
		if perr != nil {
			continue
		}
		if addr == target {
			return v, nil
		}
	}
	return View{}, fmt.Errorf("%w: %s is no longer an address of a usable interface", ErrGone, ip)
}

// ListenAddr renders the host:port string the HTTP server should bind.
// The host must be an explicit IPv4 or IPv6 literal (IPv6 addresses are
// automatically formatted with brackets, e.g. [2001:db8::1]:43821).
func ListenAddr(ip string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("netenv: port %d is out of range", port)
	}
	parsed, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("netenv: %q is not an IP address: %w", ip, err)
	}
	if parsed.Is4In6() {
		return "", fmt.Errorf("netenv: %q is not a plain IPv4 address", ip)
	}
	if parsed.Is4() || parsed.Is6() {
		return net.JoinHostPort(parsed.String(), fmt.Sprint(port)), nil
	}
	return "", fmt.Errorf("netenv: %q is not an IP address", ip)
}

// TransferURL builds the QR-encoded session URL:
//
//	http://<ip>:<port>/#<token>
//
// The token rides the URL fragment only (never the query string), so it is
// read locally by the phone and repeated in Authorization headers. The
// host must be the Fedora machine's own address on that network.
func TransferURL(ip string, port int, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", errors.New("netenv: a session token is required for the QR URL")
	}
	for _, c := range token {
		if c == '#' || c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			return "", errors.New("netenv: the session token must be URL-safe with no fragment separators")
		}
	}
	parsed, err := netip.ParseAddr(ip)
	if err != nil {
		return "", fmt.Errorf("netenv: %q is not an IP address: %w", ip, err)
	}
	if parsed.IsLinkLocalUnicast() {
		return "", fmt.Errorf("netenv: IPv6 link-local address %s cannot be used for phone QR URLs (requires global or unique-local scope)", ip)
	}
	base, err := ListenAddr(ip, port)
	if err != nil {
		return "", err
	}
	return "http://" + base + "/#" + token, nil
}
