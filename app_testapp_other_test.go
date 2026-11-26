//go:build !darwin

package main

import (
	"context"
	"testing"
)

// newTestApp builds a fully wired App whose runtime context is ready and
// whose network source is injectable. tests re-point a.source at fakes.
// This is the unchanged Linux/Windows implementation.
func newTestApp(t *testing.T) *App {
	t.Helper()
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("AppData", cfgDir)
	a, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	a.startup(context.Background())
	t.Cleanup(func() { a.shutdown(context.Background()) })
	return a
}
