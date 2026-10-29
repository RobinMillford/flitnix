package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/settings"
)

func TestAppSettings_Lifecycle(t *testing.T) {
	tempConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempConfig)

	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	// Initial settings should be default
	initial := app.GetSettings()
	if initial.Theme != settings.ThemeSystem {
		t.Errorf("initial Theme = %q, want %q", initial.Theme, settings.ThemeSystem)
	}
	if initial.DownloadDir != "" {
		t.Errorf("initial DownloadDir = %q, want empty", initial.DownloadDir)
	}
	if initial.EffectiveDir == "" {
		t.Errorf("initial EffectiveDir is empty")
	}

	// Update settings with a custom download directory
	customDir := filepath.Join(t.TempDir(), "custom_received")
	if err := os.MkdirAll(customDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	newSettings := initial
	newSettings.Theme = settings.ThemeDark
	newSettings.DownloadDir = customDir
	newSettings.AutoStartSession = true

	if err := app.SaveSettings(newSettings); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// Verify retrieved settings
	updated := app.GetSettings()
	if updated.Theme != settings.ThemeDark {
		t.Errorf("updated Theme = %q, want %q", updated.Theme, settings.ThemeDark)
	}
	if updated.DownloadDir != customDir {
		t.Errorf("updated DownloadDir = %q, want %q", updated.DownloadDir, customDir)
	}
	if updated.EffectiveDir != customDir {
		t.Errorf("updated EffectiveDir = %q, want %q", updated.EffectiveDir, customDir)
	}
	if !updated.AutoStartSession {
		t.Errorf("updated AutoStartSession is false, want true")
	}

	// Verify ReceiveDirectory reflects the change dynamically
	if app.ReceiveDirectory() != customDir {
		t.Errorf("ReceiveDirectory() = %q, want %q", app.ReceiveDirectory(), customDir)
	}

	// Reset settings
	reset, err := app.ResetSettings()
	if err != nil {
		t.Fatalf("ResetSettings: %v", err)
	}
	if reset.Theme != settings.ThemeSystem {
		t.Errorf("reset Theme = %q, want %q", reset.Theme, settings.ThemeSystem)
	}
	if reset.DownloadDir != "" {
		t.Errorf("reset DownloadDir = %q, want empty", reset.DownloadDir)
	}
	if reset.EffectiveDir == customDir {
		t.Errorf("reset EffectiveDir should have reverted from customDir, got %q", reset.EffectiveDir)
	}
}

func TestAppSettings_Validation(t *testing.T) {
	tempConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempConfig)

	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	// Invalid theme
	invalidTheme := app.GetSettings()
	invalidTheme.Theme = "neon-purple"
	if err := app.SaveSettings(invalidTheme); err == nil {
		t.Errorf("SaveSettings with invalid theme should fail")
	}

	// Non-directory path (regular file)
	regularFile := filepath.Join(t.TempDir(), "regular-file.txt")
	if err := os.WriteFile(regularFile, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	invalidDir := app.GetSettings()
	invalidDir.DownloadDir = regularFile
	if err := app.SaveSettings(invalidDir); err == nil {
		t.Errorf("SaveSettings with regular file as download directory should fail")
	}
}

func TestAppSettings_PreferredInterface(t *testing.T) {
	tempConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempConfig)

	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	// Set up fake network source with two interfaces: eth0 and wlx01
	app.source = fakeSource(
		[]net.Interface{upIf("eth0"), upIf("wlx01")},
		map[string][]net.Addr{
			"eth0":  {ipnet24(10, 0, 0, 5)},
			"wlx01": {ipnet24(192, 168, 1, 9)},
		},
	)

	// Without preference, resolveInterface should be ambiguous
	_, err = app.resolveInterface("")
	if err == nil {
		t.Fatalf("resolveInterface without preference should fail due to ambiguity")
	}

	// Save preference for eth0
	s := app.GetSettings()
	s.PreferredInterface = "eth0"
	if err := app.SaveSettings(s); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// Verify preference was saved and restored
	loaded := app.GetSettings()
	if loaded.PreferredInterface != "eth0" {
		t.Fatalf("PreferredInterface = %q, want eth0", loaded.PreferredInterface)
	}

	// resolveInterface should now select eth0
	v, err := app.resolveInterface("")
	if err != nil {
		t.Fatalf("resolveInterface with preference failed: %v", err)
	}
	if v.Name != "eth0" || v.IP != "10.0.0.5" {
		t.Fatalf("resolveInterface = %+v, want eth0 (10.0.0.5)", v)
	}

	// Now simulate eth0 becoming unavailable (e.g. unplugged), only wlx01 remains
	app.source = fakeSource(
		[]net.Interface{upIf("wlx01")},
		map[string][]net.Addr{
			"wlx01": {ipnet24(192, 168, 1, 9)},
		},
	)

	// resolveInterface should safely fall back to wlx01
	vFall, err := app.resolveInterface("")
	if err != nil {
		t.Fatalf("resolveInterface fallback failed: %v", err)
	}
	if vFall.Name != "wlx01" || vFall.IP != "192.168.1.9" {
		t.Fatalf("resolveInterface fallback = %+v, want wlx01 (192.168.1.9)", vFall)
	}

	// Reset should restore automatic preference
	reset, err := app.ResetSettings()
	if err != nil {
		t.Fatalf("ResetSettings: %v", err)
	}
	if reset.PreferredInterface != "" {
		t.Fatalf("reset PreferredInterface = %q, want empty", reset.PreferredInterface)
	}
}

func TestAppSettings_ActiveTransferGuards(t *testing.T) {
	tempConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempConfig)

	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.startup(context.Background())
	defer app.shutdown(context.Background())

	// Create an active session and approved upload
	sess, err := app.sessions.Create()
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	u, err := app.transfers.Declare(sess.Key(), "document.pdf", 500)
	if err != nil {
		t.Fatalf("Declare upload: %v", err)
	}
	if _, err := app.transfers.Decide(u.ID, true); err != nil {
		t.Fatalf("Decide upload: %v", err)
	}

	if !app.hasActiveTransfers() {
		t.Fatalf("hasActiveTransfers() returned false while upload is approved")
	}

	// Attempting to change DownloadDir while transfer is active must fail
	customDir := filepath.Join(t.TempDir(), "blocked_dir")
	_ = os.MkdirAll(customDir, 0o755)

	s := app.GetSettings()
	s.DownloadDir = customDir
	if err := app.SaveSettings(s); err == nil {
		t.Fatalf("SaveSettings changing download dir during active transfer must fail")
	}

	// But updating non-directory settings (e.g. Theme) during active transfer must succeed
	nonDirSettings := app.GetSettings()
	nonDirSettings.Theme = settings.ThemeDark
	if err := app.SaveSettings(nonDirSettings); err != nil {
		t.Fatalf("SaveSettings updating theme during active transfer failed: %v", err)
	}
	if app.GetSettings().Theme != settings.ThemeDark {
		t.Fatalf("theme was not updated")
	}

	// Attempting to reset settings while transfer is active (with non-default dir) must fail
	// First cancel the transfer, change the directory, then declare another active transfer
	if err := app.transfers.Cancel(u.ID); err != nil {
		t.Fatalf("Cancel transfer: %v", err)
	}
	if app.hasActiveTransfers() {
		t.Fatalf("hasActiveTransfers() true after cancel")
	}

	s.DownloadDir = customDir
	if err := app.SaveSettings(s); err != nil {
		t.Fatalf("SaveSettings after cancel: %v", err)
	}

	// Start a second active transfer
	u2, err := app.transfers.Declare(sess.Key(), "photo.png", 200)
	if err != nil {
		t.Fatalf("Declare u2: %v", err)
	}
	if _, err := app.transfers.Decide(u2.ID, true); err != nil {
		t.Fatalf("Decide u2: %v", err)
	}

	// ResetSettings must be blocked while u2 is active because it would change customDir back to default
	if _, err := app.ResetSettings(); err == nil {
		t.Fatalf("ResetSettings changing download dir during active transfer must fail")
	}

	// Cancel second transfer and ResetSettings must now succeed
	_ = app.transfers.Cancel(u2.ID)
	if _, err := app.ResetSettings(); err != nil {
		t.Fatalf("ResetSettings failed after transfer completion: %v", err)
	}
}

func TestAppSettings_LocalePersistenceAndRestart(t *testing.T) {
	tempConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempConfig)

	// Phase 1: First application run
	app1, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp (instance 1): %v", err)
	}
	app1.startup(context.Background())

	initial := app1.GetSettings()
	if initial.Locale != "system" {
		t.Fatalf("initial Locale = %q, want 'system'", initial.Locale)
	}

	// Change locale to "en" and save preferences
	toSave := initial
	toSave.Locale = "en"
	if err := app1.SaveSettings(toSave); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	saved := app1.GetSettings()
	if saved.Locale != "en" {
		t.Fatalf("saved Locale = %q, want 'en'", saved.Locale)
	}

	// Shutdown first instance (simulates application exit)
	app1.shutdown(context.Background())

	// Phase 2: Second application run (simulates restart)
	app2, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp (instance 2 / restart): %v", err)
	}
	app2.startup(context.Background())
	defer app2.shutdown(context.Background())

	restored := app2.GetSettings()
	if restored.Locale != "en" {
		t.Fatalf("restored Locale after restart = %q, want 'en'", restored.Locale)
	}

	// Resetting preferences restores default locale "system"
	reset, err := app2.ResetSettings()
	if err != nil {
		t.Fatalf("ResetSettings: %v", err)
	}
	if reset.Locale != "system" {
		t.Fatalf("reset Locale = %q, want 'system'", reset.Locale)
	}
}
