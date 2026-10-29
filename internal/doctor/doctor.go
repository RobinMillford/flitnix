package doctor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/netenv"
)

type CheckStatus string

const (
	StatusPass CheckStatus = "PASS"
	StatusWarn CheckStatus = "WARNING"
	StatusFail CheckStatus = "FAIL"
)

type DiagnosticItem struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Status      CheckStatus `json:"status"`
	Message     string      `json:"message"`
	Remediation string      `json:"remediation,omitempty"`
}

type DiagnosticReport struct {
	OverallStatus CheckStatus      `json:"overallStatus"`
	Timestamp     time.Time        `json:"timestamp"`
	Summary       string           `json:"summary"`
	Items         []DiagnosticItem `json:"items"`
}

type Params struct {
	NetSource     netenv.Provider
	ServerRunning bool
	ServerAddr    string // e.g. "192.168.1.50:43821"
	ActiveIP      string // e.g. "192.168.1.50"
	ActiveIface   string // e.g. "wlo1"
}

func RunDiagnostics(p Params) DiagnosticReport {
	report := DiagnosticReport{
		Timestamp: time.Now(),
		Items:     make([]DiagnosticItem, 0, 7),
	}

	if p.NetSource == nil {
		p.NetSource = netenv.DefaultSource()
	}

	// 1. Check network interfaces & adapter (adapter_presence)
	views, err := netenv.Enumerate(p.NetSource)
	if err != nil || len(views) == 0 {
		report.Items = append(report.Items, DiagnosticItem{
			ID:          "adapter_presence",
			Title:       "Network Adapter Status",
			Status:      StatusFail,
			Message:     "No active network adapter with a local IPv4 address was found.",
			Remediation: "Connect your computer to a local Wi-Fi network, Ethernet, or mobile hotspot.",
		})
	} else {
		foundActive := false
		for _, v := range views {
			if (p.ActiveIP != "" && v.IP == p.ActiveIP) || (p.ActiveIface != "" && v.Name == p.ActiveIface) {
				foundActive = true
				break
			}
		}
		if p.ActiveIP != "" && !foundActive {
			report.Items = append(report.Items, DiagnosticItem{
				ID:          "adapter_presence",
				Title:       "Network Adapter Status",
				Status:      StatusFail,
				Message:     fmt.Sprintf("The previously selected interface address (%s) is no longer available.", p.ActiveIP),
				Remediation: "Your network connection may have changed. Click 'Regenerate QR' to bind to an active network.",
			})
		} else {
			report.Items = append(report.Items, DiagnosticItem{
				ID:      "adapter_presence",
				Title:   "Network Adapter Status",
				Status:  StatusPass,
				Message: fmt.Sprintf("Active network adapter detected (%d usable local network interface(s)).", len(views)),
			})
		}
	}

	// 2. Check local IPv4 validity (ipv4_address)
	targetIP := p.ActiveIP
	if targetIP == "" && len(views) > 0 {
		targetIP = views[0].IP
	}

	if targetIP == "" {
		report.Items = append(report.Items, DiagnosticItem{
			ID:          "ipv4_address",
			Title:       "Local IPv4 Address",
			Status:      StatusFail,
			Message:     "No local IPv4 address assigned to your machine.",
			Remediation: "Ensure your computer is assigned a local IP address by your router or hotspot.",
		})
	} else {
		parsedIP, err := netip.ParseAddr(targetIP)
		if err != nil || !parsedIP.Is4() || parsedIP.IsLoopback() || parsedIP.IsUnspecified() {
			report.Items = append(report.Items, DiagnosticItem{
				ID:          "ipv4_address",
				Title:       "Local IPv4 Address",
				Status:      StatusFail,
				Message:     fmt.Sprintf("Current address %s is not a valid local network IPv4 address.", targetIP),
				Remediation: "Switch to a private LAN or Wi-Fi network.",
			})
		} else {
			report.Items = append(report.Items, DiagnosticItem{
				ID:      "ipv4_address",
				Title:   "Local IPv4 Address",
				Status:  StatusPass,
				Message: fmt.Sprintf("Using reachable local IPv4 address: %s", targetIP),
			})
		}
	}

	// Dynamic target port resolution from ServerAddr or fallback to 43821
	port := 43821
	if p.ServerAddr != "" {
		if _, portStr, err := net.SplitHostPort(p.ServerAddr); err == nil {
			if parsedPort, err := strconv.Atoi(portStr); err == nil && parsedPort > 0 {
				port = parsedPort
			}
		}
	}

	// 3. Check server listening status & port conflict (port_available)
	if !p.ServerRunning {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			report.Items = append(report.Items, DiagnosticItem{
				ID:          "port_available",
				Title:       "Server Port Availability",
				Status:      StatusFail,
				Message:     fmt.Sprintf("Port %d is already in use by another application.", port),
				Remediation: fmt.Sprintf("Close any conflicting software using port %d before starting Flitnix.", port),
			})
		} else {
			_ = ln.Close()
			report.Items = append(report.Items, DiagnosticItem{
				ID:      "port_available",
				Title:   "Server Port Availability",
				Status:  StatusPass,
				Message: fmt.Sprintf("Port %d is available for transfer sessions.", port),
			})
		}
	} else {
		report.Items = append(report.Items, DiagnosticItem{
			ID:      "port_available",
			Title:   "Server Port Availability",
			Status:  StatusPass,
			Message: fmt.Sprintf("Server is actively listening on %s.", p.ServerAddr),
		})
	}

	// 4. Local endpoint health check (server_health)
	if p.ServerRunning && p.ServerAddr != "" {
		healthURL := fmt.Sprintf("http://%s/healthz", p.ServerAddr)
		client := http.Client{Timeout: 2 * time.Second}
		req, _ := http.NewRequestWithContext(context.Background(), "GET", healthURL, nil)
		resp, err := client.Do(req)
		if err != nil {
			report.Items = append(report.Items, DiagnosticItem{
				ID:          "server_health",
				Title:       "HTTP Health Probe",
				Status:      StatusFail,
				Message:     fmt.Sprintf("Failed to communicate with local HTTP server at %s: %v", healthURL, err),
				Remediation: "A desktop firewall or security software may be blocking local connections.",
			})
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				report.Items = append(report.Items, DiagnosticItem{
					ID:      "server_health",
					Title:   "HTTP Health Probe",
					Status:  StatusPass,
					Message: "HTTP server responds promptly to local health requests.",
				})
			} else {
				report.Items = append(report.Items, DiagnosticItem{
					ID:          "server_health",
					Title:       "HTTP Health Probe",
					Status:      StatusWarn,
					Message:     fmt.Sprintf("HTTP health probe returned status %d.", resp.StatusCode),
					Remediation: "Restart the session if transfers fail to connect.",
				})
			}
		}
	} else {
		report.Items = append(report.Items, DiagnosticItem{
			ID:      "server_health",
			Title:   "HTTP Health Probe",
			Status:  StatusPass,
			Message: "Server is idle; start a session or guest room to begin listening.",
		})
	}

	// 5. Network Context & Wi-Fi Client Isolation Notice (connection_medium)
	isWiFi := false
	wifiName := ""
	if len(views) > 0 {
		for _, v := range views {
			if strings.EqualFold(v.Type, "Wi-Fi") {
				isWiFi = true
				if v.FriendlyName != "" && !strings.EqualFold(v.FriendlyName, "Wi-Fi") {
					wifiName = v.FriendlyName
				}
				break
			}
		}
	}

	if isWiFi {
		ssidNote := ""
		if wifiName != "" {
			ssidNote = fmt.Sprintf(" (%s)", wifiName)
		}
		report.Items = append(report.Items, DiagnosticItem{
			ID:          "connection_medium",
			Title:       "Wi-Fi & Network Medium",
			Status:      StatusPass,
			Message:     fmt.Sprintf("Connected via Wi-Fi%s. Advisory: Public or guest Wi-Fi networks may enforce AP/Client Isolation.", ssidNote),
			Remediation: "If your phone cannot load the QR page while connected to the same Wi-Fi, your router may be isolating devices. Connect both devices to a private home Wi-Fi or turn on your phone's personal hotspot.",
		})
	} else {
		report.Items = append(report.Items, DiagnosticItem{
			ID:      "connection_medium",
			Title:   "Network Medium",
			Status:  StatusPass,
			Message: "Connected via Ethernet or wired interface.",
		})
	}

	// 6. Host address binding reachability (host_reachability)
	if p.ServerRunning && p.ServerAddr != "" {
		host, _, err := net.SplitHostPort(p.ServerAddr)
		if err != nil {
			host = p.ServerAddr
		}
		matches := false
		for _, v := range views {
			if v.IP == host {
				matches = true
				break
			}
		}
		if matches || host == "127.0.0.1" || host == "localhost" {
			report.Items = append(report.Items, DiagnosticItem{
				ID:      "host_reachability",
				Title:   "Host Address Binding",
				Status:  StatusPass,
				Message: fmt.Sprintf("Server is bound to reachable local address %s.", host),
			})
		} else {
			report.Items = append(report.Items, DiagnosticItem{
				ID:          "host_reachability",
				Title:       "Host Address Binding",
				Status:      StatusWarn,
				Message:     fmt.Sprintf("Server bound to %s, which does not match active interface IP %s.", host, p.ActiveIP),
				Remediation: "Click 'Regenerate QR' to rebind to the active interface.",
			})
		}
	} else {
		report.Items = append(report.Items, DiagnosticItem{
			ID:      "host_reachability",
			Title:   "Host Address Binding",
			Status:  StatusPass,
			Message: "Host binding ready for session startup.",
		})
	}

	// 7. Multi-homed routing guidance (multihomed_routing)
	if len(views) > 1 {
		report.Items = append(report.Items, DiagnosticItem{
			ID:          "multihomed_routing",
			Title:       "Multi-Homed Routing",
			Status:      StatusWarn,
			Message:     fmt.Sprintf("%d active network interfaces detected. Multiple active adapters (e.g. Wi-Fi and VPN) can cause phones to route to the wrong interface.", len(views)),
			Remediation: "Ensure the phone connects to the same network interface shown on the desktop, or disconnect unnecessary VPN/secondary adapters.",
		})
	} else {
		report.Items = append(report.Items, DiagnosticItem{
			ID:      "multihomed_routing",
			Title:   "Multi-Homed Routing",
			Status:  StatusPass,
			Message: "Single active network adapter detected; no routing ambiguity.",
		})
	}

	// Calculate overall status
	report.OverallStatus = StatusPass
	failCount := 0
	warnCount := 0
	for _, it := range report.Items {
		if it.Status == StatusFail {
			failCount++
		} else if it.Status == StatusWarn {
			warnCount++
		}
	}

	if failCount > 0 {
		report.OverallStatus = StatusFail
		report.Summary = fmt.Sprintf("Found %d issue(s) that may prevent phones from connecting.", failCount)
	} else if warnCount > 0 {
		report.OverallStatus = StatusWarn
		report.Summary = "Connection is operational, but review network notices below if issues occur."
	} else {
		report.OverallStatus = StatusPass
		report.Summary = "All network and server diagnostics passed."
	}

	return report
}
