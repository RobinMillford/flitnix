//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	winRunRegPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	winRunValueName = "Flitnix"
)

type windowsBackend struct {
	cfg Config
}

func newPlatformBackend(cfg Config) backend {
	return &windowsBackend{cfg: cfg}
}

func (b *windowsBackend) Supported() bool {
	return true
}

func (b *windowsBackend) IsEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunRegPath, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer k.Close()

	val, _, err := k.GetStringValue(winRunValueName)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(val) != "", nil
}

func (b *windowsBackend) Enable() error {
	cmdLine, err := buildWindowsRunCommand(b.cfg)
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, winRunRegPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("autostart: open HKCU Run key: %w", err)
	}
	defer k.Close()
	if err := k.SetStringValue(winRunValueName, cmdLine); err != nil {
		return fmt.Errorf("autostart: set Run value: %w", err)
	}
	return nil
}

func (b *windowsBackend) Disable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, winRunRegPath, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer k.Close()
	err = k.DeleteValue(winRunValueName)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func buildWindowsRunCommand(cfg Config) (string, error) {
	exe, err := validateExecPath(cfg.ExecPath)
	if err != nil {
		return "", err
	}
	args, err := validateArgs(cfg.Args)
	if err != nil {
		return "", err
	}
	quoted := `"` + strings.ReplaceAll(exe, `"`, `\"`) + `"`
	if len(args) == 0 {
		return quoted, nil
	}
	return quoted + " " + strings.Join(args, " "), nil
}
