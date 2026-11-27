package main

import (
	"path/filepath"
	"testing"
)

// isolateAppStorage redirects every location that can influence where an App
// stores received files into a fresh per-test directory.
//
// The receive directory is NOT derived from XDG_CONFIG_HOME: an empty
// settings.downloadDir makes transfer.NewStore fall back to
// os.UserHomeDir()/Downloads/Flitnix. On Linux/macOS that reads HOME, on
// Windows it reads USERPROFILE, so both must be redirected. Keeping the
// setting itself empty preserves the default-settings behavior tests assert.
func isolateAppStorage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// assertIsolatedReceiveDir fails loudly if an App silently fell back to the
// real user's receive directory. This keeps future tests from polluting it.
func assertIsolatedReceiveDir(t *testing.T, a *App, dir string) {
	t.Helper()
	want := filepath.Join(dir, filepath.FromSlash("Downloads/Flitnix"))
	if got := a.store.Dir(); got != want {
		t.Fatalf("test receive dir = %q, want %q: automated tests must never write into the real receive directory", got, want)
	}
}

// newIsolatedAppIn constructs an App using storage already isolated by
// isolateAppStorage. It does not change the environment, so tests that restart
// an App can reuse the same directory.
func newIsolatedAppIn(t *testing.T, dir string) *App {
	t.Helper()
	a, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	assertIsolatedReceiveDir(t, a, dir)
	return a
}

// newIsolatedApp isolates storage and constructs an App in one step.
func newIsolatedApp(t *testing.T) *App {
	t.Helper()
	return newIsolatedAppIn(t, isolateAppStorage(t))
}
