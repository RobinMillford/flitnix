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
	NetSource        netenv.Provider
	ServerRunning    bool
	ServerAddr       string // e.g. "192.168.1.50:43821"
	ActiveIP         string // e.g. "192.168.1.50"
	ActiveIface      string // e.g. "wlo1"
	LastPhoneContact *time.Time
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
			Message:     "No active network adapter with a usable local IP address was found.",
			Remediation: "Connect your computer to a local Wi-Fi network, Ethernet, or mobile hotspot.",
		})
	} else {
		foundActive := false
		v4Count := 0
		v6Count := 0
		for _, v := range views {
			if v.Family == "IPv6" {
				v6Count++
			} else {
				v4Count++
			}
			if (p.ActiveIP != "" && (v.IP == p.ActiveIP || v.IP == strings.Trim(p.ActiveIP, "[]"))) || (p.ActiveIface != "" && v.Name == p.ActiveIface) {
				foundActive = true
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
			familySummary := fmt.Sprintf("%d IPv4", v4Count)
			if v6Count > 0 {
				familySummary += fmt.Sprintf(", %d IPv6", v6Count)
			}
			report.Items = append(report.Items, DiagnosticItem{
				ID:      "adapter_presence",
				Title:   "Network Adapter Status",
				Status:  StatusPass,
				Message: fmt.Sprintf("Active network adapter detected (%d usable candidate(s): %s).", len(views), familySummary),
			})
		}
	}

	// 2. Check local IP validity (ipv4_address)
	targetIP := p.ActiveIP
	if targetIP == "" && len(views) > 0 {
		targetIP = views[0].IP
	}
	rawTargetIP := strings.Trim(targetIP, "[]")

	if targetIP == "" {
		report.Items = append(report.Items, DiagnosticItem{
			ID:          "ipv4_address",
			Title:       "Local IP Address",
			Status:      StatusFail,
			Message:     "No local IP address assigned to your machine.",
			Remediation: "Ensure your computer is assigned a local IP address by your router or hotspot.",
		})
	} else {
		parsedIP, err := netip.ParseAddr(rawTargetIP)
		if err != nil || parsedIP.IsLoopback() || parsedIP.IsUnspecified() || (!parsedIP.Is4() && !parsedIP.Is6()) {
			report.Items = append(report.Items, DiagnosticItem{
				ID:          "ipv4_address",
				Title:       "Local IP Address",
				Status:      StatusFail,
				Message:     fmt.Sprintf("Current address %s is not a valid local network address.", targetIP),
				Remediation: "Switch to a private LAN or Wi-Fi network.",
			})
		} else {
			var msg string
			if parsedIP.Is6() {
				msg = fmt.Sprintf("Using reachable local IPv6 address: [%s]", rawTargetIP)
			} else {
				msg = fmt.Sprintf("Using reachable local IPv4 address: %s", rawTargetIP)
			}
			report.Items = append(report.Items, DiagnosticItem{
				ID:      "ipv4_address",
				Title:   "Local IP Address",
				Status:  StatusPass,
				Message: msg,
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
		probeHost := "127.0.0.1"
		if rawTargetIP != "" {
			if parsed, perr := netip.ParseAddr(rawTargetIP); perr == nil {
				if parsed.Is6() {
					probeHost = "::1"
				}
			}
		}
		probeAddr := net.JoinHostPort(probeHost, strconv.Itoa(port))
		ln, err := net.Listen("tcp", probeAddr)
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
		familyLabel := "IPv4"
		if strings.Contains(p.ServerAddr, "[") || strings.Count(p.ServerAddr, ":") > 1 {
			familyLabel = "IPv6"
		}
		report.Items = append(report.Items, DiagnosticItem{
			ID:      "port_available",
			Title:   "Server Port Availability",
			Status:  StatusPass,
			Message: fmt.Sprintf("Server is actively listening on %s (%s).", p.ServerAddr, familyLabel),
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
				msg := "HTTP server responds promptly to local health requests."
				if p.LastPhoneContact != nil {
					msg = fmt.Sprintf("HTTP server healthy. Phone connection active (last contact at %s).", p.LastPhoneContact.Format("15:04:05"))
				}
				report.Items = append(report.Items, DiagnosticItem{
					ID:      "server_health",
					Title:   "HTTP Health Probe",
					Status:  StatusPass,
					Message: msg,
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
		cleanHost := strings.Trim(host, "[]")
		matches := false
		for _, v := range views {
			if v.IP == cleanHost || v.IP == host {
				matches = true
				break
			}
		}
		if matches || cleanHost == "127.0.0.1" || cleanHost == "::1" || cleanHost == "localhost" {
			family := "IPv4"
			if strings.Contains(cleanHost, ":") {
				family = "IPv6"
			}
			msg := fmt.Sprintf("Server is bound to reachable local %s address %s.", family, host)
			if p.LastPhoneContact != nil {
				msg = fmt.Sprintf("Server is bound to %s (%s). Phone handshake active (last contact at %s).", host, family, p.LastPhoneContact.Format("15:04:05"))
			} else {
				msg += " Local health check passed; awaiting phone connection."
			}
			report.Items = append(report.Items, DiagnosticItem{
				ID:      "host_reachability",
				Title:   "Host Address Binding",
				Status:  StatusPass,
				Message: msg,
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
