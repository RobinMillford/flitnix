package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/server"
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
	// The audit writer keeps audit.jsonl open until shutdown. Register cleanup
	// here so the file is closed before t.TempDir() removes its directory
	// (Windows cannot delete an open file).
	t.Cleanup(func() { a.shutdown(context.Background()) })
	return a
}

// testAppServer returns the current server under srvMu. The network monitor
// can replace a.srv between test steps, so tests must never read the field
// directly.
func testAppServer(t *testing.T, a *App) *server.Server {
	t.Helper()
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	if a.srv == nil {
		t.Fatal("test app has no server")
	}
	return a.srv
}

func testAppServerOrNil(a *App) *server.Server {
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	return a.srv
}

func testAppHandler(t *testing.T, a *App) http.Handler {
	return testAppServer(t, a).Handler()
}

func testAppServerURL(t *testing.T, a *App) string {
	return testAppServer(t, a).URL()
}

// newIsolatedApp isolates storage and constructs an App in one step.
func newIsolatedApp(t *testing.T) *App {
	t.Helper()
	return newIsolatedAppIn(t, isolateAppStorage(t))
}
