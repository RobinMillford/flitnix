//go:build linux

package netenv

import (
	"context"
	"testing"
)

func TestParseNMCLIOutputSeparatesProfileAndSSID(t *testing.T) {
	metadata := parseNMCLIOutput("802-11-wireless\nHome\\: 5G\n", upIf("wlo1"))
	if metadata.Type != "Wi-Fi" {
		t.Fatalf("type = %q, want Wi-Fi", metadata.Type)
	}
	if metadata.ConnectionName != "Home: 5G" {
		t.Fatalf("profile = %q, want escaped profile value", metadata.ConnectionName)
	}
	if metadata.SSID != "" {
		t.Fatalf("SSID = %q, want empty until active AP lookup", metadata.SSID)
	}
}

func TestActiveSSIDMatchesInterfaceAndUnescapesFields(t *testing.T) {
	output := "*:synthetic\\: hotspot:wlo1\n:other:wlan0\n"
	if got := activeSSID(output, "wlo1"); got != "synthetic: hotspot" {
		t.Fatalf("activeSSID = %q, want synthetic: hotspot", got)
	}
	if got := activeSSID(output, "wlan0"); got != "" {
		t.Fatalf("inactive interface SSID = %q, want empty", got)
	}
}

func TestActiveSSIDWithRetryHandlesNetworkManagerWarmup(t *testing.T) {
	attempts := 0
	got := activeSSIDWithRetry(context.Background(), "wlo1", func() (string, error) {
		attempts++
		if attempts == 1 {
			return "*:--:wlo1\n", nil
		}
		return "*:moto\\: hotspot:wlo1\n", nil
	})
	if got != "moto: hotspot" {
		t.Fatalf("activeSSIDWithRetry = %q, want the active SSID", got)
	}
	if attempts != 2 {
		t.Fatalf("query attempts = %d, want bounded retry count 2", attempts)
	}
}

func TestActiveSSIDWithRetryDoesNotRetryAfterSuccess(t *testing.T) {
	attempts := 0
	got := activeSSIDWithRetry(context.Background(), "wlo1", func() (string, error) {
		attempts++
		return "*:connected:wlo1\n", nil
	})
	if got != "connected" || attempts != 1 {
		t.Fatalf("got %q after %d attempts, want one successful lookup", got, attempts)
	}
}
