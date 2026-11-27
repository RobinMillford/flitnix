package main

import (
	"context"
	"testing"
)

func TestBeforeCloseRespectsMinimizeToTrayAndExplicitQuit(t *testing.T) {
	a := newIsolatedApp(t)

	// Without tray active, beforeClose never intercepts shutdown.
	if a.beforeClose(context.Background()) {
		t.Fatal("expected beforeClose=false when tray is inactive")
	}

	a.HideWindow()
	if !a.windowHidden.Load() {
		t.Fatal("expected windowHidden=true after HideWindow")
	}
	a.ToggleWindow()
	if a.windowHidden.Load() {
		t.Fatal("expected windowHidden=false after ToggleWindow")
	}

	// Explicit quit always allows close even if tray were supported.
	a.quitRequested.Store(true)
	if a.beforeClose(context.Background()) {
		t.Fatal("expected beforeClose=false after QuitApp / quitRequested")
	}
}

func TestSettingsLaunchOnLoginAndMinimizeToTray(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a := newIsolatedApp(t)

	v := a.GetSettings()
	if !v.MinimizeToTray {
		t.Fatal("expected default MinimizeToTray=true")
	}
	v.MinimizeToTray = false
	v.LaunchOnLogin = true
	if err := a.SaveSettings(v); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got := a.GetSettings()
	if got.MinimizeToTray {
		t.Fatal("expected MinimizeToTray=false persisted")
	}
	if a.autostart.Supported() && !got.LaunchOnLogin {
		t.Fatal("expected LaunchOnLogin=true persisted on supported platform")
	}
}
