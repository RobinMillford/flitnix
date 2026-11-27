// Package autostart provides cross-platform login autostart management for
// Flitnix without third-party dependencies.
package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	DefaultAppName = "Flitnix"
	DefaultComment = "Always-ready local file sharing between devices"
	DefaultAppID   = "io.github.robinmillford.flitnix"
)

var (
	ErrUnsupported     = errors.New("autostart: unsupported on this platform")
	ErrInvalidExecPath = errors.New("autostart: invalid executable path")
)

// Config describes the desktop application entry registered for login autostart.
type Config struct {
	AppName  string
	Comment  string
	AppID    string
	ExecPath string
	Args     []string
}

type backend interface {
	Supported() bool
	IsEnabled() (bool, error)
	Enable() error
	Disable() error
}

// Manager wraps OS-specific autostart configuration cleanly and safely.
type Manager struct {
	mu      sync.Mutex
	backend backend
}

// NewManager creates an autostart Manager using the current executable and platform backend.
func NewManager(cfg Config) *Manager {
	normalized := normalizeConfig(cfg)
	return &Manager{
		backend: newPlatformBackend(normalized),
	}
}

func newManagerWithBackend(b backend) *Manager {
	return &Manager{backend: b}
}

func normalizeConfig(cfg Config) Config {
	if strings.TrimSpace(cfg.AppName) == "" {
		cfg.AppName = DefaultAppName
	}
	if strings.TrimSpace(cfg.Comment) == "" {
		cfg.Comment = DefaultComment
	}
	if strings.TrimSpace(cfg.AppID) == "" {
		cfg.AppID = DefaultAppID
	}
	if strings.TrimSpace(cfg.ExecPath) == "" {
		if exe, err := os.Executable(); err == nil {
			if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
				exe = resolved
			}
			cfg.ExecPath = exe
		}
	}
	if len(cfg.Args) == 0 {
		cfg.Args = []string{"--background"}
	}
	return cfg
}

// Supported reports whether login autostart is supported on the current host.
func (m *Manager) Supported() bool {
	if m == nil || m.backend == nil {
		return false
	}
	return m.backend.Supported()
}

// IsEnabled checks whether login autostart is currently configured.
func (m *Manager) IsEnabled() (bool, error) {
	if m == nil || m.backend == nil || !m.backend.Supported() {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.backend.IsEnabled()
}

// SetEnabled enables or disables login autostart idempotently.
func (m *Manager) SetEnabled(enabled bool) error {
	if m == nil || m.backend == nil || !m.backend.Supported() {
		if !enabled {
			return nil
		}
		return ErrUnsupported
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if enabled {
		return m.backend.Enable()
	}
	return m.backend.Disable()
}

func validateExecPath(execPath string) (string, error) {
	p := strings.TrimSpace(execPath)
	if p == "" || containsControlChars(p) {
		return "", ErrInvalidExecPath
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: must be absolute", ErrInvalidExecPath)
	}
	return filepath.Clean(p), nil
}

func validateArgs(args []string) ([]string, error) {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if containsControlChars(a) {
			return nil, fmt.Errorf("%w: control characters in argument", ErrInvalidExecPath)
		}
		trimmed := strings.TrimSpace(a)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out, nil
}

func containsControlChars(s string) bool {
	for _, r := range s {
		if r == 0 || r == '\r' || r == '\n' || (r < 0x20 && r != '\t') {
			return true
		}
	}
	return false
}

// PlatformName exposes the normalized platform identifier for diagnostics.
func PlatformName() string {
	return runtime.GOOS
}
