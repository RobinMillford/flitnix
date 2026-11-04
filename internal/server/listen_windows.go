//go:build windows

package server

import (
	"errors"
	"syscall"
)

// wsaEADDRINUSE is Winsock's WSAEADDRINUSE. A Windows bind failure carries
// this value, not syscall.EADDRINUSE (which is a different, invented errno in
// the syscall package), so both are classified here.
const wsaEADDRINUSE = syscall.Errno(10048)

// addressInUse reports whether a bind failure means another socket already
// holds the requested address.
func addressInUse(err error) bool {
	return errors.Is(err, wsaEADDRINUSE) || errors.Is(err, syscall.EADDRINUSE)
}
