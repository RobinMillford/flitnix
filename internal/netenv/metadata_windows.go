//go:build windows

package netenv

import (
	"net"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wlanInterfaceStateConnected     = 1
	wlanIntfOpcodeCurrentConnection = 7
	// WLAN_INTERFACE_MAX_NAME_LENGTH bounds the number of interfaces the OS
	// can enumerate. The bound keeps unsafe.Slice below a sane size even if a
	// driver ever returned a corrupt NumberOfItems header.
	maxWlanEnumeratedInterfaces = 256
)

type wlanInterfaceInfo struct {
	InterfaceGUID windows.GUID
	Description   [256]uint16
	State         uint32
}

type wlanInterfaceInfoListHeader struct {
	NumberOfItems uint32
	Index         uint32
}

type wlanConnectionAttributes struct {
	InterfaceState uint32
	ConnectionMode uint32
	ProfileName    [256]uint16
	Ssid           struct {
		SSIDLength uint32
		SSID       [32]byte
	}
	BssType uint32
}

// InvalidateMetadataCache is a no-op on Windows since wlanapi queries are in-process Win32 calls.
func InvalidateMetadataCache() {}

// systemMetadata queries the Native Wi-Fi API. Windows interface/address
// enumeration remains in the shared net package; this adapter only supplies
// connection identity and SSID metadata.
func systemMetadata(iface net.Interface) Metadata {
	ssid, profile, ok := wlanSSID(iface)
	if ok {
		return Metadata{SSID: ssid, ConnectionName: profile, Type: "Wi-Fi"}
	}
	if strings.Contains(strings.ToLower(iface.Name), "ethernet") {
		return Metadata{Type: "Ethernet"}
	}
	return Metadata{Type: typeFromInterface(iface)}
}

func wlanSSID(iface net.Interface) (string, string, bool) {
	mod := windows.NewLazySystemDLL("wlanapi.dll")
	open := mod.NewProc("WlanOpenHandle")
	enum := mod.NewProc("WlanEnumInterfaces")
	query := mod.NewProc("WlanQueryInterface")
	free := mod.NewProc("WlanFreeMemory")
	var handle windows.Handle
	var version uint32
	r1, _, _ := open.Call(2, 0, uintptr(unsafe.Pointer(&version)), uintptr(unsafe.Pointer(&handle)))
	if r1 != 0 {
		return "", "", false
	}
	defer mod.NewProc("WlanCloseHandle").Call(uintptr(handle), 0)
	// The Native Wi-Fi API returns pointers into memory it allocates with
	// LocalAlloc. They are received through *unsafe.Pointer output variables
	// (never through uintptr lvalues), so every cast below converts a real
	// unsafe.Pointer to a typed pointer — the pattern `go vet` accepts and
	// the only one that keeps the pointer visible to the GC while in use.
	var list unsafe.Pointer
	r1, _, _ = enum.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&list)))
	if r1 != 0 || list == nil {
		return "", "", false
	}
	// list is Windows-allocated memory freed by WlanFreeMemory, not by the Go
	// GC, so no runtime.KeepAlive is needed; every use happens before this
	// deferred free runs.
	defer free.Call(uintptr(list))
	header := (*wlanInterfaceInfoListHeader)(list)
	if header.NumberOfItems > maxWlanEnumeratedInterfaces {
		return "", "", false
	}
	items := unsafe.Slice(
		(*wlanInterfaceInfo)(unsafe.Add(list, unsafe.Sizeof(*header))),
		header.NumberOfItems,
	)
	var connected *wlanInterfaceInfo
	for i := range items {
		info := &items[i]
		if info.State != wlanInterfaceStateConnected {
			continue
		}
		description := windows.UTF16ToString(info.Description[:])
		if !strings.EqualFold(description, iface.Name) &&
			!strings.EqualFold(info.InterfaceGUID.String(), iface.Name) {
			continue
		}
		connected = info
		break
	}
	if connected == nil {
		return "", "", false
	}
	{
		var data unsafe.Pointer
		var size uint32
		var opcode uint32
		r1, _, _ = query.Call(uintptr(handle), uintptr(unsafe.Pointer(&connected.InterfaceGUID)), wlanIntfOpcodeCurrentConnection, 0, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&opcode)))
		if r1 != 0 || data == nil {
			return "", "", false
		}
		defer free.Call(uintptr(data))
		attr := (*wlanConnectionAttributes)(data)
		return wlanConnectionMetadata(attr)
	}
}

// wlanConnectionMetadata extracts (ssid, profile, ok) from a
// wlanConnectionAttributes buffer. Pure parsing, no syscalls, so it is unit
// tested in metadata_windows_test.go without Wi-Fi hardware.
func wlanConnectionMetadata(attr *wlanConnectionAttributes) (string, string, bool) {
	profile := windows.UTF16ToString(attr.ProfileName[:])
	if attr.Ssid.SSIDLength == 0 {
		return "", profile, true
	}
	if attr.Ssid.SSIDLength > uint32(len(attr.Ssid.SSID)) {
		// DOT11_SSID caps the length at 32; a larger driver-reported length
		// means corrupt data. Fail closed instead of slicing out of bounds.
		return "", "", false
	}
	return string(attr.Ssid.SSID[:attr.Ssid.SSIDLength]), profile, true
}
