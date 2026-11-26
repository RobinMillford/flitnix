//go:build darwin

package main

import (
	"context"
	"testing"
)

// newTestApp builds a fully wired App whose runtime context is ready and
// whose network source is injectable. tests re-point a.source at fakes.
//
// macOS only: os.UserConfigDir resolves to $HOME/Library/Application Support
// and ignores XDG_CONFIG_HOME. Redirect HOME as well so each test gets its own
// config dir; otherwise recovered sessions leak between tests and between the
// `go test` and `go test -race` CI steps, eventually hitting the 8-session
// limit. Linux and Windows keep their original helper in app_testapp_other_test.go.
func newTestApp(t *testing.T) *App {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("AppData", cfgDir)
	t.Setenv("HOME", cfgDir)
	a, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	a.startup(context.Background())
	t.Cleanup(func() { a.shutdown(context.Background()) })
	return a
}
