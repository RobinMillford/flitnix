//go:build darwin

package notify

import (
	"os/exec"
)

type darwinBackend struct {
	osaPath string
}

func newPlatformBackend() backend {
	p, _ := exec.LookPath("osascript")
	return &darwinBackend{osaPath: p}
}

func (b *darwinBackend) Supported() bool {
	return b.osaPath != ""
}

func (b *darwinBackend) Send(n dispatchedNotification) error {
	if b.osaPath == "" {
		return nil
	}
	script := "on run argv\ndisplay notification (item 1 of argv) with title (item 2 of argv)\nend run"
	cmd := exec.Command(b.osaPath, "-e", script, n.Body, n.Title)
	return cmd.Run()
}

func (b *darwinBackend) Close() {}
