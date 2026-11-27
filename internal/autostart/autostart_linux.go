//go:build linux

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type linuxBackend struct {
	cfg         Config
	desktopPath string
}

func newPlatformBackend(cfg Config) backend {
	configDir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(configDir) == "" {
		if home, hErr := os.UserHomeDir(); hErr == nil {
			configDir = filepath.Join(home, ".config")
		}
	}
	desktopPath := ""
	if configDir != "" {
		desktopPath = filepath.Join(configDir, "autostart", "flitnix.desktop")
	}
	return &linuxBackend{
		cfg:         cfg,
		desktopPath: desktopPath,
	}
}

func (b *linuxBackend) Supported() bool {
	return b.desktopPath != ""
}

func (b *linuxBackend) IsEnabled() (bool, error) {
	if b.desktopPath == "" {
		return false, nil
	}
	data, err := os.ReadFile(b.desktopPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	text := string(data)
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, "Hidden=true") || strings.EqualFold(trimmed, "X-GNOME-Autostart-enabled=false") {
			return false, nil
		}
	}
	return strings.Contains(text, "[Desktop Entry]"), nil
}

func (b *linuxBackend) Enable() error {
	if b.desktopPath == "" {
		return ErrUnsupported
	}
	content, err := buildDesktopEntry(b.cfg)
	if err != nil {
		return err
	}
	dir := filepath.Dir(b.desktopPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("autostart: create dir: %w", err)
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d", b.desktopPath, os.Getpid())
	if err := os.WriteFile(tmpPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("autostart: write temp desktop entry: %w", err)
	}
	if err := os.Rename(tmpPath, b.desktopPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("autostart: install desktop entry: %w", err)
	}
	return nil
}

func (b *linuxBackend) Disable() error {
	if b.desktopPath == "" {
		return nil
	}
	if err := os.Remove(b.desktopPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func buildDesktopEntry(cfg Config) (string, error) {
	exe, err := validateExecPath(cfg.ExecPath)
	if err != nil {
		return "", err
	}
	args, err := validateArgs(cfg.Args)
	if err != nil {
		return "", err
	}

	var execParts []string
	execParts = append(execParts, quoteDesktopExecArg(exe))
	for _, a := range args {
		execParts = append(execParts, quoteDesktopExecArg(a))
	}

	name := sanitizeDesktopText(cfg.AppName, DefaultAppName)
	comment := sanitizeDesktopText(cfg.Comment, DefaultComment)

	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Version=1.0
Name=%s
Comment=%s
Exec=%s
Icon=flitnix
Terminal=false
Categories=Network;FileTransfer;Utility;
StartupNotify=false
X-GNOME-Autostart-enabled=true
`, name, comment, strings.Join(execParts, " ")), nil
}

func sanitizeDesktopText(v, fallback string) string {
	clean := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(v))
	if clean == "" {
		return fallback
	}
	return clean
}

// quoteDesktopExecArg quotes arguments per the XDG Desktop Entry Specification.
func quoteDesktopExecArg(arg string) string {
	// Always quote executable paths or args containing whitespace/reserved symbols.
	needsQuote := arg == "" || strings.ContainsAny(arg, " \t\n\"'\\><~|&;$*?#()`")
	if !needsQuote {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range arg {
		switch r {
		case '"', '`', '$', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
