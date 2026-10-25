//go:build linux

package netenv

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"time"
)

// systemMetadata uses NetworkManager when available. Generic interface and
// address discovery remains usable on Linux systems using another manager.
func systemMetadata(iface net.Interface) Metadata {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nmcli", "-t", "-g", "GENERAL.TYPE,GENERAL.CONNECTION", "device", "show", iface.Name)
	out, err := cmd.Output()
	if err != nil || ctx.Err() != nil {
		return Metadata{Type: typeFromInterface(iface)}
	}
	metadata := parseNMCLIOutput(string(out), iface)
	if metadata.Type != "Wi-Fi" {
		return metadata
	}
	wifi := exec.CommandContext(ctx, "nmcli", "-t", "-f", "IN-USE,SSID,DEVICE", "device", "wifi", "list", "ifname", iface.Name)
	wifiOut, err := wifi.Output()
	if err == nil && ctx.Err() == nil {
		metadata.SSID = activeSSID(string(wifiOut), iface.Name)
	}
	return metadata
}

func parseNMCLIOutput(output string, iface net.Interface) Metadata {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	var rawType, rawProfile string
	if len(lines) > 0 {
		rawType = unescapeNMCLI(lines[0])
	}
	if len(lines) > 1 {
		rawProfile = unescapeNMCLI(lines[1])
	}
	return Metadata{Type: friendlyType(rawType, iface), ConnectionName: rawProfile}
}

func activeSSID(output, interfaceName string) string {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := splitNMCLIFields(line)
		if len(fields) < 3 || strings.TrimSpace(fields[0]) != "*" || strings.TrimSpace(fields[2]) != interfaceName {
			continue
		}
		if fields[1] == "" || fields[1] == "--" {
			return ""
		}
		return fields[1]
	}
	return ""
}

func splitNMCLIFields(line string) []string {
	var fields []string
	var field strings.Builder
	escaped := false
	for _, r := range line {
		if escaped {
			field.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
		} else if r == ':' {
			fields = append(fields, field.String())
			field.Reset()
		} else {
			field.WriteRune(r)
		}
	}
	if escaped {
		field.WriteByte('\\')
	}
	return append(fields, field.String())
}

func unescapeNMCLI(value string) string {
	var b strings.Builder
	escaped := false
	for _, r := range value {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteByte('\\')
	}
	return strings.TrimSpace(b.String())
}

func friendlyType(raw string, iface net.Interface) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "wifi", "802-11-wireless":
		return "Wi-Fi"
	case "ethernet":
		return "Ethernet"
	case "wireguard", "vpn":
		return "VPN"
	}
	return typeFromInterface(iface)
}
