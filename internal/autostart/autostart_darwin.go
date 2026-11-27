//go:build darwin

package autostart

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

type darwinBackend struct {
	cfg       Config
	plistPath string
}

func newPlatformBackend(cfg Config) backend {
	plistPath := ""
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		plistPath = filepath.Join(home, "Library", "LaunchAgents", cfg.AppID+".plist")
	}
	return &darwinBackend{
		cfg:       cfg,
		plistPath: plistPath,
	}
}

func (b *darwinBackend) Supported() bool {
	return b.plistPath != ""
}

func (b *darwinBackend) IsEnabled() (bool, error) {
	if b.plistPath == "" {
		return false, nil
	}
	data, err := os.ReadFile(b.plistPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(string(data), "<key>RunAtLoad</key>"), nil
}

func (b *darwinBackend) Enable() error {
	if b.plistPath == "" {
		return ErrUnsupported
	}
	content, err := buildLaunchAgentPlist(b.cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.plistPath), 0o755); err != nil {
		return fmt.Errorf("autostart: create LaunchAgents dir: %w", err)
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", b.plistPath, os.Getpid())
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("autostart: write temp plist: %w", err)
	}
	if err := os.Rename(tmpPath, b.plistPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("autostart: install LaunchAgent plist: %w", err)
	}
	return nil
}

func (b *darwinBackend) Disable() error {
	if b.plistPath == "" {
		return nil
	}
	if err := os.Remove(b.plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func buildLaunchAgentPlist(cfg Config) (string, error) {
	exe, err := validateExecPath(cfg.ExecPath)
	if err != nil {
		return "", err
	}
	args, err := validateArgs(cfg.Args)
	if err != nil {
		return "", err
	}
	label := strings.TrimSpace(cfg.AppID)
	if label == "" || containsControlChars(label) {
		label = DefaultAppID
	}

	var items strings.Builder
	fmt.Fprintf(&items, "\t\t<string>%s</string>\n", html.EscapeString(exe))
	for _, a := range args {
		fmt.Fprintf(&items, "\t\t<string>%s</string>\n", html.EscapeString(a))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<false/>
</dict>
</plist>
`, html.EscapeString(label), items.String()), nil
}
