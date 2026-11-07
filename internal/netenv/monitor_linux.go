//go:build linux

package netenv

import (
	"context"

	"golang.org/x/sys/unix"
)

func defaultNativeNotifier(ctx context.Context, notify chan<- struct{}) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_ROUTE)
	if err != nil {
		return
	}
	defer unix.Close(fd)

	sa := &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Groups: unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR | unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE,
	}
	if err := unix.Bind(fd, sa); err != nil {
		return
	}

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

		// Wait up to 500ms for incoming kernel netlink events
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
							// Buffer overflow: kernel dropped messages, force complete reconciliation.
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
						// Notification channel already queued
					}
				}
			}
		}
	}
}
