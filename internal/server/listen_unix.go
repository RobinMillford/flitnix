//go:build !windows

package server

import (
	"errors"
	"syscall"
)

// addressInUse reports whether a bind failure means another socket already
// holds the requested address.
func addressInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
