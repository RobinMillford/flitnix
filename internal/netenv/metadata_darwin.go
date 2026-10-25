//go:build darwin

package netenv

import "net"

// CoreWLAN requires an Objective-C bridge and a native macOS build. Keep the
// common candidate discovery usable until that adapter is built and tested on
// supported Intel and Apple Silicon runners.
func systemMetadata(iface net.Interface) Metadata {
	return Metadata{Type: typeFromInterface(iface)}
}
