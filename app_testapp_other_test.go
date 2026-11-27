//go:build !darwin

package main

import (
	"context"
	"testing"
)

// newTestApp builds a fully wired App whose runtime context is ready and
// whose network source is injectable. tests re-point a.source at fakes.
//
// Linux/Windows entry point. On these platforms the default receive directory
// also comes from the home directory (HOME on Linux, USERPROFILE on Windows),
// so construction goes through the shared isolation helper rather than the
// original XDG_CONFIG_HOME/AppData-only setup.
func newTestApp(t *testing.T) *App {
	t.Helper()
	a := newIsolatedApp(t)
	a.startup(context.Background())
	return a
}
