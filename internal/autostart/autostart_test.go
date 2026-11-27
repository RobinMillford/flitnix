package autostart

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type fakeBackend struct {
	supported bool
	enabled   bool
}

func (f *fakeBackend) Supported() bool          { return f.supported }
func (f *fakeBackend) IsEnabled() (bool, error) { return f.enabled, nil }
func (f *fakeBackend) Enable() error            { f.enabled = true; return nil }
func (f *fakeBackend) Disable() error           { f.enabled = false; return nil }

func TestManagerLifecycle(t *testing.T) {
	b := &fakeBackend{supported: true}
	m := newManagerWithBackend(b)

	if !m.Supported() {
		t.Fatal("expected supported=true")
	}
	if en, err := m.IsEnabled(); err != nil || en {
		t.Fatalf("expected initially disabled, got enabled=%v err=%v", en, err)
	}
	if err := m.SetEnabled(true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if en, err := m.IsEnabled(); err != nil || !en {
		t.Fatalf("expected enabled, got enabled=%v err=%v", en, err)
	}
	if err := m.SetEnabled(false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if en, err := m.IsEnabled(); err != nil || en {
		t.Fatalf("expected disabled, got enabled=%v err=%v", en, err)
	}

	unsupported := newManagerWithBackend(&fakeBackend{supported: false})
	if err := unsupported.SetEnabled(true); err != ErrUnsupported {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	if err := unsupported.SetEnabled(false); err != nil {
		t.Fatalf("disabling unsupported backend should be safe no-op, got %v", err)
	}
}

func TestValidateExecPathRejectsUnsafeInputs(t *testing.T) {
	cases := []string{
		"",
		"relative/path/flitnix",
		"/tmp/bad\nline",
		"/tmp/bad\x00byte",
	}
	for _, tc := range cases {
		if _, err := validateExecPath(tc); err == nil {
			t.Fatalf("expected error for invalid exec path %q", tc)
		}
	}
}

func TestLinuxDesktopFileEnableDisable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux desktop entry test")
	}
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	m := NewManager(Config{
		AppName:  "Flitnix",
		ExecPath: "/opt/Flitnix App/flitnix",
		Args:     []string{"--background"},
	})
	if err := m.SetEnabled(true); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	en, err := m.IsEnabled()
	if err != nil || !en {
		t.Fatalf("expected enabled after Enable, got %v (%v)", en, err)
	}
	desktopFile := filepath.Join(tmp, "autostart", "flitnix.desktop")
	data, err := os.ReadFile(desktopFile)
	if err != nil {
		t.Fatalf("read desktop file: %v", err)
	}
	text := string(data)
	if text == "" || filepath.Base(desktopFile) != "flitnix.desktop" {
		t.Fatalf("unexpected desktop file: %s", text)
	}
	// Idempotent disable
	if err := m.SetEnabled(false); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := m.SetEnabled(false); err != nil {
		t.Fatalf("second Disable: %v", err)
	}
	if _, err := os.Stat(desktopFile); !os.IsNotExist(err) {
		t.Fatalf("expected desktop file removed, got err=%v", err)
	}
}
