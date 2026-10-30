//go:build darwin && cgo

package netenv

/*
#cgo darwin LDFLAGS: -framework CoreWLAN -framework Foundation
#include <stdlib.h>
#include <stddef.h>

int flitnix_corewlan_metadata(const char *interface_name, char *ssid, size_t ssid_size);
*/
import "C"

import (
	"net"
	"strings"
	"unsafe"
)

func systemMetadata(iface net.Interface) Metadata {
	var ssid [256]byte
	interfaceName := C.CString(iface.Name)
	defer C.free(unsafe.Pointer(interfaceName))
	if C.flitnix_corewlan_metadata(
		interfaceName,
		(*C.char)(unsafe.Pointer(&ssid[0])),
		C.size_t(len(ssid)),
	) != 0 {
		return Metadata{
			SSID: strings.TrimSpace(string(ssid[:len(ssid)-1])),
			Type: "Wi-Fi",
		}
	}
	return Metadata{Type: typeFromInterface(iface)}
}
