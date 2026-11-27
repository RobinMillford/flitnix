//go:build darwin

package main

import (
	"context"
	"testing"
)

// newTestApp builds a fully wired App whose runtime context is ready and
// whose network source is injectable. tests re-point a.source at fakes.
//
// macOS-specific entry point: os.UserConfigDir resolves to
// $HOME/Library/Application Support and ignores XDG_CONFIG_HOME, so HOME must
// be redirected to keep config and recovered sessions per-test. That redirect,
// the Windows/Linux equivalents, and the production-directory guard all live
// in the shared helper test_storage_isolation_test.go.
func newTestApp(t *testing.T) *App {
	t.Helper()
	a := newIsolatedApp(t)
	a.startup(context.Background())
	return a
}
