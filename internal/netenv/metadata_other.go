//go:build !linux && !windows && !darwin

package netenv

import "net"

// Unsupported systems retain generic interface/address discovery without
// inventing a network name or depending on a platform command.
func systemMetadata(iface net.Interface) Metadata {
	return Metadata{Type: typeFromInterface(iface)}
}
