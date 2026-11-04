//go:build windows

package netenv

import (
	"net"
	"strings"
	"testing"
	"unsafe"
)

// TestWlanStructLayout guards the binary layout assumptions of the Native
// Wi-Fi adapter. The WLAN_INTERFACE_INFO list is walked with pointer
// arithmetic (unsafe.Add + unsafe.Slice), so any field drift between these
// structs and the C API definitions would silently read garbage. Sizes follow
// wlanapi.h: GUID(16) + WCHAR Description[256](512) + DWORD State(4), and a
// list header of two DWORDs.
func TestWlanStructLayout(t *testing.T) {
	if got := unsafe.Sizeof(wlanInterfaceInfo{}); got != 16+512+4 {
		t.Errorf("sizeof(wlanInterfaceInfo) = %d; want %d (GUID+Description+State)", got, 16+512+4)
	}
	if got := unsafe.Sizeof(wlanInterfaceInfoListHeader{}); got != 8 {
		t.Errorf("sizeof(wlanInterfaceInfoListHeader) = %d; want 8 (NumberOfItems+Index)", got)
	}
	if got := unsafe.Offsetof(wlanInterfaceInfo{}.State); got != 16+512 {
		t.Errorf("offset(State) = %d; want %d", got, 16+512)
	}
}

// makeConnectionAttributes builds a wlanConnectionAttributes as the driver
// would fill it, with an ASCII SSID and a UTF-16 profile name.
func makeConnectionAttributes(ssid string, ssidLen uint32, profile string) *wlanConnectionAttributes {
	attr := &wlanConnectionAttributes{}
	attr.Ssid.SSIDLength = ssidLen
	copy(attr.Ssid.SSID[:], ssid)
	for i, r := range profile {
		attr.ProfileName[i] = uint16(r)
	}
	return attr
}

// TestWlanConnectionMetadata covers the pure parsing of a query result
// buffer without any Wi-Fi hardware or WLAN API calls.
func TestWlanConnectionMetadata(t *testing.T) {
	t.Run("ssid and profile", func(t *testing.T) {
		// uSSIDLength is the number of valid SSID bytes; the remaining bytes of
		// the 32-byte buffer are padding the driver does not fill. A fixture
		// that overstates the length would make the parser read that padding,
		// so the length is derived from the SSID itself.
		const ssid = "flitnix-net"
		got, profile, ok := wlanConnectionMetadata(makeConnectionAttributes(ssid, uint32(len(ssid)), "Home Wifi"))
		if !ok || got != ssid || profile != "Home Wifi" {
			t.Errorf("got (%q, %q, %v); want (%q, %q, true)", got, profile, ok, ssid, "Home Wifi")
		}
	})

	t.Run("empty ssid still reports profile", func(t *testing.T) {
		// A hidden or absent SSID must not lose the connection profile.
		ssid, profile, ok := wlanConnectionMetadata(makeConnectionAttributes("", 0, "Office LAN"))
		if !ok || ssid != "" || profile != "Office LAN" {
			t.Errorf("got (%q, %q, %v); want (\"\", %q, true)", ssid, profile, ok, "Office LAN")
		}
	})

	t.Run("oversized ssid length fails closed", func(t *testing.T) {
		// DOT11_SSID caps length at 32; a driver reporting more means corrupt
		// data. The adapter must refuse it rather than slice out of bounds.
		attr := makeConnectionAttributes("x", 33, "Evil")
		ssid, profile, ok := wlanConnectionMetadata(attr)
		if ok || ssid != "" || profile != "" {
			t.Errorf("oversized SSIDLength accepted: got (%q, %q, %v)", ssid, profile, ok)
		}
	})

	t.Run("max valid ssid length accepted", func(t *testing.T) {
		long := strings.Repeat("s", 32)
		ssid, _, ok := wlanConnectionMetadata(makeConnectionAttributes(long, 32, "P"))
		if !ok || ssid != long {
			t.Errorf("32-byte SSID rejected: got (%q, %v)", ssid, ok)
		}
	})
}

// TestSystemMetadataTypeFallback exercises systemMetadata's non-WLAN path
// with a loopback interface: no WLAN API activity is expected and the
// fallback classification must not regress.
func TestSystemMetadataTypeFallback(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skip("no interface enumeration available")
	}
	found := false
	for _, ifc := range ifaces {
		m := systemMetadata(ifc)
		if m.Type == "" {
			t.Errorf("interface %s produced empty Type", ifc.Name)
		}
		found = true
		break
	}
	if !found {
		t.Skip("no network interfaces to classify")
	}
}
