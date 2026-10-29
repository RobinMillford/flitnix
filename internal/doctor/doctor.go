package doctor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
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

	// 1. Check network interfaces & adapter
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

	// 2. Check local IPv4 validity
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

	// 3. Check server listening status & port conflict
	if !p.ServerRunning {
		// Server not running currently
		// Test if port 43821 is occupied by an external process
		ln, err := net.Listen("tcp", "127.0.0.1:43821")
		if err != nil {
			report.Items = append(report.Items, DiagnosticItem{
				ID:          "port_status",
				Title:       "Server Port Availability",
				Status:      StatusFail,
				Message:     "Port 43821 is already in use by another application.",
				Remediation: "Close any conflicting software using port 43821 before starting Flitnix.",
			})
		} else {
			_ = ln.Close()
			report.Items = append(report.Items, DiagnosticItem{
				ID:      "port_status",
				Title:   "Server Port Availability",
				Status:  StatusPass,
				Message: "Port 43821 is available for transfer sessions.",
			})
		}
	} else {
		report.Items = append(report.Items, DiagnosticItem{
			ID:      "port_status",
			Title:   "Server Port Availability",
			Status:  StatusPass,
			Message: fmt.Sprintf("Server is actively listening on %s.", p.ServerAddr),
		})
	}

	// 4. Local endpoint health check (HTTP probe to /healthz)
	if p.ServerRunning && p.ServerAddr != "" {
		healthURL := fmt.Sprintf("http://%s/healthz", p.ServerAddr)
		client := http.Client{Timeout: 2 * time.Second}
		req, _ := http.NewRequestWithContext(context.Background(), "GET", healthURL, nil)
		resp, err := client.Do(req)
		if err != nil {
			report.Items = append(report.Items, DiagnosticItem{
				ID:          "http_probe",
				Title:       "HTTP Health Probe",
				Status:      StatusFail,
				Message:     fmt.Sprintf("Failed to communicate with local HTTP server at %s: %v", healthURL, err),
				Remediation: "A desktop firewall or security software may be blocking local connections.",
			})
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				report.Items = append(report.Items, DiagnosticItem{
					ID:      "http_probe",
					Title:   "HTTP Health Probe",
					Status:  StatusPass,
					Message: "HTTP server responds promptly to local health requests.",
				})
			} else {
				report.Items = append(report.Items, DiagnosticItem{
					ID:          "http_probe",
					Title:       "HTTP Health Probe",
					Status:      StatusWarn,
					Message:     fmt.Sprintf("HTTP health probe returned status %d.", resp.StatusCode),
					Remediation: "Restart the session if transfers fail to connect.",
				})
			}
		}
	}

	// 5. Network Context & Wi-Fi Client Isolation Warning
	isWiFi := false
	if len(views) > 0 {
		for _, v := range views {
			if strings.EqualFold(v.Type, "Wi-Fi") {
				isWiFi = true
				break
			}
		}
	}

	if isWiFi {
		report.Items = append(report.Items, DiagnosticItem{
			ID:          "wifi_isolation",
			Title:       "Wi-Fi Client Isolation Notice",
			Status:      StatusWarn,
			Message:     "Connected via Wi-Fi. Some public, university, or corporate routers enable 'Client Isolation' (AP Isolation).",
			Remediation: "If your phone cannot load the QR page while connected to the same Wi-Fi, your router may be isolating devices. Connect both devices to a private home Wi-Fi or turn on your phone's personal hotspot.",
		})
	} else {
		report.Items = append(report.Items, DiagnosticItem{
			ID:      "wifi_isolation",
			Title:   "Network Medium",
			Status:  StatusPass,
			Message: "Connected via Ethernet or local interface.",
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
