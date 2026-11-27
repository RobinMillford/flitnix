//go:build windows

package notify

import (
	"os"
	"sync"

	"git.sr.ht/~jackmordaunt/go-toast/v2"
)

const winAppID = "Flitnix"

type windowsBackend struct {
	mu      sync.Mutex
	handler func(string)
	inited  bool
}

func newPlatformBackend() backend {
	return &windowsBackend{}
}

func (b *windowsBackend) SetHandler(fn func(string)) {
	b.mu.Lock()
	b.handler = fn
	b.mu.Unlock()

	b.ensureAppData()
	toast.SetActivationCallback(func(args string, _ []toast.UserData) {
		b.mu.Lock()
		h := b.handler
		b.mu.Unlock()
		if h != nil && args != "" {
			go h(args)
		}
	})
}

func (b *windowsBackend) ensureAppData() {
	b.mu.Lock()
	if b.inited {
		b.mu.Unlock()
		return
	}
	b.inited = true
	b.mu.Unlock()

	exe, _ := os.Executable()
	_ = toast.SetAppData(toast.AppData{
		AppID:         winAppID,
		GUID:          "{7E650794-4A1D-4C2B-9A34-8E2C60F1D8B0}",
		ActivationExe: exe,
	})
}

func (b *windowsBackend) Supported() bool {
	return true
}

func (b *windowsBackend) Send(n dispatchedNotification) error {
	b.ensureAppData()
	actions := make([]toast.Action, 0, len(n.Actions))
	for _, a := range n.Actions {
		actions = append(actions, toast.Action{
			Type:      toast.Foreground,
			Content:   a.Label,
			Arguments: a.Token,
		})
	}
	notif := toast.Notification{
		AppID:               winAppID,
		Title:               n.Title,
		Body:                n.Body,
		ActivationType:      toast.Foreground,
		ActivationArguments: n.DefaultToken,
		Actions:             actions,
	}
	return notif.Push()
}

func (b *windowsBackend) Close() {}
