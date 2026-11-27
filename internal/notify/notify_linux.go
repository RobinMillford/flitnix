//go:build linux

package notify

import (
	"errors"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	fdoNotifyService   = "org.freedesktop.Notifications"
	fdoNotifyPath      = dbus.ObjectPath("/org/freedesktop/Notifications")
	fdoNotifyInterface = "org.freedesktop.Notifications"
)

type linuxBackend struct {
	mu           sync.Mutex
	conn         *dbus.Conn
	supported    bool
	supportsActs bool
	supportsMark bool
	handler      func(string)

	idByKey map[string]uint32
	pending map[uint32]map[string]string // notifID -> actionKey -> opaque token
	closed  bool
}

func newPlatformBackend() backend {
	b := &linuxBackend{
		idByKey: make(map[string]uint32),
		pending: make(map[uint32]map[string]string),
	}
	b.init()
	return b
}

func (b *linuxBackend) SetHandler(fn func(string)) {
	b.mu.Lock()
	b.handler = fn
	b.mu.Unlock()
}

func (b *linuxBackend) init() {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return
	}
	var hasOwner bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, fdoNotifyService).Store(&hasOwner); err != nil || !hasOwner {
		_ = conn.Close()
		return
	}

	var caps []string
	if err := conn.Object(fdoNotifyService, fdoNotifyPath).Call(fdoNotifyInterface+".GetCapabilities", 0).Store(&caps); err == nil {
		for _, c := range caps {
			if c == "actions" {
				b.supportsActs = true
			}
			if c == "body-markup" {
				b.supportsMark = true
			}
		}
	}

	_ = conn.AddMatchSignal(
		dbus.WithMatchObjectPath(fdoNotifyPath),
		dbus.WithMatchInterface(fdoNotifyInterface),
		dbus.WithMatchMember("ActionInvoked"),
	)
	_ = conn.AddMatchSignal(
		dbus.WithMatchObjectPath(fdoNotifyPath),
		dbus.WithMatchInterface(fdoNotifyInterface),
		dbus.WithMatchMember("NotificationClosed"),
	)

	sigCh := make(chan *dbus.Signal, 32)
	conn.Signal(sigCh)

	b.conn = conn
	b.supported = true

	go b.readSignals(sigCh)
}

func (b *linuxBackend) readSignals(ch <-chan *dbus.Signal) {
	for sig := range ch {
		if sig == nil {
			continue
		}
		switch sig.Name {
		case fdoNotifyInterface + ".ActionInvoked":
			if len(sig.Body) >= 2 {
				id, ok1 := sig.Body[0].(uint32)
				actKey, ok2 := sig.Body[1].(string)
				if ok1 && ok2 {
					b.mu.Lock()
					tokMap := b.pending[id]
					tok := tokMap[actKey]
					fn := b.handler
					b.mu.Unlock()
					if tok != "" && fn != nil {
						go fn(tok)
					}
				}
			}
		case fdoNotifyInterface + ".NotificationClosed":
			if len(sig.Body) >= 1 {
				if id, ok := sig.Body[0].(uint32); ok {
					b.mu.Lock()
					delete(b.pending, id)
					for k, v := range b.idByKey {
						if v == id {
							delete(b.idByKey, k)
						}
					}
					b.mu.Unlock()
				}
			}
		}
	}
}

func (b *linuxBackend) Supported() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.supported && !b.closed
}

func (b *linuxBackend) Send(n dispatchedNotification) error {
	b.mu.Lock()
	if !b.supported || b.closed || b.conn == nil {
		b.mu.Unlock()
		return errors.New("notify: linux notifications unavailable")
	}
	conn := b.conn
	replacesID := b.idByKey[n.Key]
	acts := make([]string, 0, len(n.Actions)*2+2)
	tokMap := make(map[string]string)
	if b.supportsActs {
		if n.DefaultToken != "" {
			acts = append(acts, "default", "Open")
			tokMap["default"] = n.DefaultToken
		}
		for i, a := range n.Actions {
			key := "act_" + string(rune('a'+i))
			acts = append(acts, key, a.Label)
			tokMap[key] = a.Token
		}
	}
	body := n.Body
	if b.supportsMark {
		body = EscapeMarkup(body)
	}
	b.mu.Unlock()

	hints := map[string]dbus.Variant{
		"desktop-entry": dbus.MakeVariant("flitnix"),
		"category":      dbus.MakeVariant("transfer"),
	}
	var outID uint32
	call := conn.Object(fdoNotifyService, fdoNotifyPath).Call(
		fdoNotifyInterface+".Notify",
		0,
		"Flitnix",
		replacesID,
		"flitnix",
		n.Title,
		body,
		acts,
		hints,
		int32(5000),
	)
	if err := call.Store(&outID); err != nil {
		return err
	}

	b.mu.Lock()
	if outID != 0 {
		if n.Key != "" {
			if len(b.idByKey) >= 256 {
				clear(b.idByKey)
			}
			b.idByKey[n.Key] = outID
		}
		if len(tokMap) > 0 {
			if len(b.pending) >= 256 {
				clear(b.pending)
			}
			b.pending[outID] = tokMap
		}
	}
	b.mu.Unlock()
	return nil
}

func (b *linuxBackend) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	conn := b.conn
	b.conn = nil
	b.supported = false
	b.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}
