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
	var list uintptr
	r1, _, _ = enum.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&list)))
	if r1 != 0 || list == 0 {
		return "", "", false
	}
	defer free.Call(list)
	header := (*wlanInterfaceInfoListHeader)(unsafe.Pointer(list))
	itemSize := unsafe.Sizeof(wlanInterfaceInfo{})
	base := list + unsafe.Sizeof(*header)
	var connected *wlanInterfaceInfo
	for i := uint32(0); i < header.NumberOfItems; i++ {
		info := (*wlanInterfaceInfo)(unsafe.Pointer(base + uintptr(i)*itemSize))
		if info.State != wlanInterfaceStateConnected {
			continue
		}
		description := windows.UTF16ToString(info.Description[:])
		if description != iface.Name {
			continue
		}
		connected = info
		break
	}
	if connected == nil {
		return "", "", false
	}
	{
		var data uintptr
		var size uint32
		var opcode uint32
		r1, _, _ = query.Call(uintptr(handle), uintptr(unsafe.Pointer(&connected.InterfaceGUID)), wlanIntfOpcodeCurrentConnection, 0, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&opcode)))
		if r1 != 0 || data == 0 {
			return "", "", false
		}
		defer free.Call(data)
		attr := (*wlanConnectionAttributes)(unsafe.Pointer(data))
		if attr.Ssid.SSIDLength == 0 {
			return "", windows.UTF16ToString(attr.ProfileName[:]), true
		}
		return string(attr.Ssid.SSID[:attr.Ssid.SSIDLength]), windows.UTF16ToString(attr.ProfileName[:]), true
	}
}
