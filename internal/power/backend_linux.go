//go:build linux

package power

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type linuxBackend struct {
	mu      sync.Mutex
	conn    *dbus.Conn
	fd      dbus.UnixFD
	held    bool
	initErr error
}

func newNativeBackend() (backend, error) {
	return newLinuxBackend()
}

func newLinuxBackend() (*linuxBackend, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return &linuxBackend{initErr: fmt.Errorf("systemd-logind system bus unavailable: %w", err)}, nil
	}
	return &linuxBackend{conn: conn}, nil
}

func (b *linuxBackend) Supported() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.initErr == nil && b.conn != nil
}

func (b *linuxBackend) Inhibit(reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.held {
		return nil
	}

	if b.conn == nil {
		if b.initErr != nil {
			return b.initErr
		}
		conn, err := dbus.SystemBus()
		if err != nil {
			b.initErr = err
			return fmt.Errorf("systemd-logind system bus unavailable: %w", err)
		}
		b.conn = conn
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	obj := b.conn.Object("org.freedesktop.login1", "/org/freedesktop/login1")
	var fd dbus.UnixFD
	err := obj.CallWithContext(ctx, "org.freedesktop.login1.Manager.Inhibit", 0, "sleep", "Flitnix", reason, "block").Store(&fd)
	if err != nil {
		return fmt.Errorf("org.freedesktop.login1.Manager.Inhibit failed: %w", err)
	}

	b.fd = fd
	b.held = true
	return nil
}

func (b *linuxBackend) Uninhibit() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.held {
		return nil
	}

	fd := int(b.fd)
	b.held = false
	b.fd = 0

	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("closing inhibitor file descriptor %d failed: %w", fd, err)
	}
	return nil
}

func (b *linuxBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.held {
		_ = unix.Close(int(b.fd))
		b.held = false
		b.fd = 0
	}

	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
	return nil
}
