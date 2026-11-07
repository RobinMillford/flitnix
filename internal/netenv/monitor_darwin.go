//go:build darwin

package netenv

import (
	"context"

	"golang.org/x/sys/unix"
)

func defaultNativeNotifier(ctx context.Context, notify chan<- struct{}) {
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return
	}
	defer unix.Close(fd)

	_ = unix.SetNonblock(fd, true)

	buf := make([]byte, 4096)
	pollFds := []unix.PollFd{
		{
			Fd:     int32(fd),
			Events: unix.POLLIN,
		},
	}

	for {
		if ctx.Err() != nil {
			return
		}

		n, err := unix.Poll(pollFds, 500)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}

		if n > 0 {
			rev := pollFds[0].Revents
			if rev&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return
			}
			if rev&unix.POLLIN != 0 {
				gotEvent := false
				for {
					nr, readErr := unix.Read(fd, buf)
					if readErr != nil {
						if readErr == unix.EAGAIN || readErr == unix.EWOULDBLOCK {
							break
						}
						if readErr == unix.EINTR {
							continue
						}
						if readErr == unix.ENOBUFS {
							// Buffer overflow: force complete network-state reconciliation.
							gotEvent = true
							break
						}
						return
					}
					if nr > 0 {
						gotEvent = true
					}
				}

				if gotEvent {
					select {
					case <-ctx.Done():
						return
					case notify <- struct{}{}:
					default:
					}
				}
			}
		}
	}
}
