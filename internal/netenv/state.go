package netenv

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// ConnectionStatus represents the explicit operational status of the host network.
type ConnectionStatus string

const (
	// StatusScanning indicates network discovery is actively in progress.
	StatusScanning ConnectionStatus = "scanning"
	// StatusConnected indicates a usable LAN interface is connected and ready.
	StatusConnected ConnectionStatus = "connected"
	// StatusNoNetwork indicates no usable local network interface was found.
	StatusNoNetwork ConnectionStatus = "no_network"
	// StatusSelectedUnavailable indicates the user's preferred interface is down or not found,
	// and Flitnix fell back to an eligible alternative if available.
	StatusSelectedUnavailable ConnectionStatus = "selected_unavailable"
	// StatusMetadataUnavailable indicates the interface is usable, but OS metadata (e.g. SSID) is missing.
	StatusMetadataUnavailable ConnectionStatus = "metadata_unavailable"
	// StatusError indicates a discovery error occurred during interface enumeration.
	StatusError ConnectionStatus = "error"
)

// NetworkState represents the complete, verified network snapshot passed to callers and UI.
type NetworkState struct {
	Status             ConnectionStatus `json:"status"`
	InterfaceName      string           `json:"interfaceName"`
	FriendlyName       string           `json:"friendlyName"`
	Type               string           `json:"type"`
	SSID               string           `json:"ssid,omitempty"`
	IP                 string           `json:"ip"`
	Family             string           `json:"family"`
	CIDR               string           `json:"cidr"`
	SelectedInterface  string           `json:"selectedInterface"`
	PreferredInterface string           `json:"preferredInterface,omitempty"`
	ServerAddress      string           `json:"serverAddress,omitempty"`
	AdvertisedURL      string           `json:"advertisedURL,omitempty"`
	LastDetected       int64            `json:"lastDetected"`
	Interfaces         []View           `json:"interfaces"`
	Error              string           `json:"error,omitempty"`
}

// EvaluateState evaluates an enumerated list of interfaces against user preferences
// and running server bindings to produce an accurate, explicit NetworkState.
func EvaluateState(views []View, preferredInterface string, serverAddr string, scanErr error) NetworkState {
	now := time.Now().Unix()

	if scanErr != nil {
		return NetworkState{
			Status:             StatusError,
			PreferredInterface: preferredInterface,
			ServerAddress:      serverAddr,
			AdvertisedURL:      advertisedURL(serverAddr),
			LastDetected:       now,
			Interfaces:         []View{},
			Error:              scanErr.Error(),
		}
	}

	if len(views) == 0 {
		return NetworkState{
			Status:             StatusNoNetwork,
			PreferredInterface: preferredInterface,
			ServerAddress:      "",
			AdvertisedURL:      "",
			LastDetected:       now,
			Interfaces:         []View{},
			Error:              "No usable local network interface found; connect to a network first",
		}
	}

	// Case: User specified a preferred interface.
	if strings.TrimSpace(preferredInterface) != "" {
		pref := strings.TrimSpace(preferredInterface)
		var match *View
		for _, v := range views {
			if v.Name == pref {
				if v.Family == "IPv4" || match == nil {
					vCopy := v
					match = &vCopy
				}
			}
		}

		if match != nil {
			status := StatusConnected
			if match.Type == "Wi-Fi" && !match.MetadataAvailable {
				status = StatusMetadataUnavailable
			}
			return NetworkState{
				Status:             status,
				InterfaceName:      match.Name,
				FriendlyName:       match.FriendlyName,
				Type:               match.Type,
				SSID:               extractSSID(*match),
				IP:                 match.IP,
				Family:             match.Family,
				CIDR:               match.CIDR,
				SelectedInterface:  match.Name,
				PreferredInterface: pref,
				ServerAddress:      serverAddr,
				AdvertisedURL:      advertisedURL(serverAddr),
				LastDetected:       now,
				Interfaces:         views,
			}
		}

		// Preferred interface not found; fall back safely using PickDefault or first eligible.
		fallback, pickErr := PickDefault(views)
		if pickErr != nil && len(views) > 0 {
			fallback = views[0]
		}

		return NetworkState{
			Status:             StatusSelectedUnavailable,
			InterfaceName:      fallback.Name,
			FriendlyName:       fallback.FriendlyName,
			Type:               fallback.Type,
			SSID:               extractSSID(fallback),
			IP:                 fallback.IP,
			Family:             fallback.Family,
			CIDR:               fallback.CIDR,
			SelectedInterface:  fallback.Name,
			PreferredInterface: pref,
			ServerAddress:      serverAddr,
			AdvertisedURL:      advertisedURL(serverAddr),
			LastDetected:       now,
			Interfaces:         views,
			Error:              fmt.Sprintf("Preferred interface %q is unavailable; using %s (%s)", pref, fallback.FriendlyName, fallback.IP),
		}
	}

	// Case: Automatic selection (preferredInterface is empty).
	// 1. Check if the currently running server address has a matching candidate in views (sticky selection).
	var selected View
	selectedFound := false

	if serverAddr != "" {
		host, _, err := net.SplitHostPort(serverAddr)
		if err == nil && host != "" {
			// Strip brackets if IPv6
			host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
			var bestCandidate View
			bestScore := -1
			for _, v := range views {
				if v.IP == host {
					score := interfaceScore(v)
					if score > bestScore {
						bestScore = score
						bestCandidate = v
						selectedFound = true
					}
				}
			}
			if selectedFound {
				selected = bestCandidate
			}
		}
	}

	// 2. If no sticky match, pick the highest-priority candidate.
	if !selectedFound {
		bestScore := -1
		for _, v := range views {
			score := interfaceScore(v)
			// Prefer IPv4 over IPv6 when scores are equal to maximize phone browser compatibility
			if v.Family == "IPv4" {
				score += 5
			}
			if score > bestScore {
				bestScore = score
				selected = v
				selectedFound = true
			}
		}
	}

	if !selectedFound && len(views) > 0 {
		selected = views[0]
	}

	status := StatusConnected
	if selected.Type == "Wi-Fi" && !selected.MetadataAvailable {
		status = StatusMetadataUnavailable
	}

	return NetworkState{
		Status:            status,
		InterfaceName:     selected.Name,
		FriendlyName:      selected.FriendlyName,
		Type:              selected.Type,
		SSID:              extractSSID(selected),
		IP:                selected.IP,
		Family:            selected.Family,
		CIDR:              selected.CIDR,
		SelectedInterface: selected.Name,
		ServerAddress:     serverAddr,
		AdvertisedURL:     advertisedURL(serverAddr),
		LastDetected:      now,
		Interfaces:        views,
	}
}

func extractSSID(v View) string {
	if v.FriendlyNameSource == "SSID" {
		return v.FriendlyName
	}
	return ""
}

func advertisedURL(serverAddr string) string {
	if strings.TrimSpace(serverAddr) == "" {
		return ""
	}
	if strings.HasPrefix(serverAddr, "http://") || strings.HasPrefix(serverAddr, "https://") {
		return serverAddr
	}
	return "http://" + serverAddr
}

// Equal reports whether two NetworkState snapshots are equivalent in all user-facing
// and operational fields, ignoring the transient LastDetected timestamp.
func (s NetworkState) Equal(o NetworkState) bool {
	if s.Status != o.Status ||
		s.InterfaceName != o.InterfaceName ||
		s.FriendlyName != o.FriendlyName ||
		s.Type != o.Type ||
		s.SSID != o.SSID ||
		s.IP != o.IP ||
		s.Family != o.Family ||
		s.CIDR != o.CIDR ||
		s.SelectedInterface != o.SelectedInterface ||
		s.PreferredInterface != o.PreferredInterface ||
		s.ServerAddress != o.ServerAddress ||
		s.AdvertisedURL != o.AdvertisedURL ||
		s.Error != o.Error {
		return false
	}

	if len(s.Interfaces) != len(o.Interfaces) {
		return false
	}

	for i := range s.Interfaces {
		a, b := s.Interfaces[i], o.Interfaces[i]
		if a.Name != b.Name ||
			a.IP != b.IP ||
			a.CIDR != b.CIDR ||
			a.Family != b.Family ||
			a.FriendlyName != b.FriendlyName ||
			a.Type != b.Type ||
			a.Active != b.Active {
			return false
		}
	}

	return true
}

func isVirtualInterface(name string) bool {
	lower := strings.ToLower(name)
	prefixes := []string{"docker", "virbr", "veth", "vmnet", "vboxnet", "br-", "cni", "flannel", "dummy"}
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// interfaceScore ranks candidates for default network selection:
// Wi-Fi / Hotspot > Ethernet > USB / Tethering > Other physical > Virtual bridges / containers.
func interfaceScore(v View) int {
	if isVirtualInterface(v.Name) {
		return 10
	}
	switch v.Type {
	case "Wi-Fi":
		if v.FriendlyNameSource == "SSID" {
			return 100
		}
		return 90
	case "Ethernet":
		return 80
	default:
		lowerName := strings.ToLower(v.Name)
		if strings.Contains(lowerName, "usb") || strings.Contains(lowerName, "rndis") || strings.Contains(lowerName, "tether") {
			return 70
		}
		return 50
	}
}
