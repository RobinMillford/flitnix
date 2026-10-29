package settings

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDefaultSettings(t *testing.T) {
	d := DefaultSettings()
	if d.Version != CurrentSchemaVersion {
		t.Fatalf("expected version %d, got %d", CurrentSchemaVersion, d.Version)
	}
	if d.Theme != ThemeSystem {
		t.Fatalf("expected default theme %q, got %q", ThemeSystem, d.Theme)
	}
	if d.Locale != "system" {
		t.Fatalf("expected default locale 'system', got %q", d.Locale)
	}
	if d.DownloadDir != "" {
		t.Fatalf("expected default downloadDir to be empty (home default), got %q", d.DownloadDir)
	}
	if d.AutoStartSession {
		t.Fatalf("expected default autoStartSession false")
	}
}

func TestStore_SaveAndGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	initial := store.Get()
	if initial.Theme != ThemeSystem {
		t.Fatalf("expected initial theme %q, got %q", ThemeSystem, initial.Theme)
	}

	// Update settings
	customDir := filepath.Join(dir, "custom_downloads")
	update := Settings{
		Theme:              ThemeDark,
		Locale:             "en",
		DownloadDir:        customDir,
		AutoStartSession:   true,
		PreferredInterface: "wlan0",
	}

	if err := store.Save(update); err != nil {
		t.Fatalf("failed to save settings: %v", err)
	}

	current := store.Get()
	if current.Theme != ThemeDark {
		t.Fatalf("expected theme %q, got %q", ThemeDark, current.Theme)
	}
	if current.Locale != "en" {
		t.Fatalf("expected locale %q, got %q", "en", current.Locale)
	}
	if current.DownloadDir != customDir {
		t.Fatalf("expected downloadDir %q, got %q", customDir, current.DownloadDir)
	}
	if !current.AutoStartSession {
		t.Fatalf("expected autoStartSession true")
	}
	if current.PreferredInterface != "wlan0" {
		t.Fatalf("expected interface 'wlan0', got %q", current.PreferredInterface)
	}

	// Verify persistence on disk by reloading in a separate store
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	fromDisk := reloaded.Get()
	if fromDisk.Theme != ThemeDark || fromDisk.Locale != "en" || fromDisk.DownloadDir != customDir || !fromDisk.AutoStartSession {
		t.Fatalf("reloaded settings do not match saved state: %+v", fromDisk)
	}

	// Verify file permissions on Linux/Unix (should be 0600)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Logf("note: file permissions are %o (expected 0600 on unix)", perm)
	}
}

func TestStore_ValidationAndNormalization(t *testing.T) {
	s := Settings{
		Theme:       "INVALID_THEME",
		Locale:      "  ",
		DownloadDir: "  /my/path/../path/  ",
	}

	norm := ValidateAndNormalize(s)
	if norm.Theme != ThemeSystem {
		t.Fatalf("expected invalid theme to normalize to 'system', got %q", norm.Theme)
	}
	if norm.Locale != "system" {
		t.Fatalf("expected empty locale to normalize to 'system', got %q", norm.Locale)
	}
	if norm.DownloadDir != "/my/path" {
		t.Fatalf("expected cleaned downloadDir '/my/path', got %q", norm.DownloadDir)
	}
}

func TestStore_CorruptedFileRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt_settings.json")

	// Write malformed JSON
	if err := os.WriteFile(path, []byte("{ this is not valid json :"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("expected corrupted file to recover gracefully with defaults, got error: %v", err)
	}

	// Verify defaults restored
	current := store.Get()
	if current.Theme != ThemeSystem {
		t.Fatalf("expected default theme %q, got %q", ThemeSystem, current.Theme)
	}

	// Verify a backup file was created
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var backupFound bool
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" && len(e.Name()) > len("corrupt_settings.json.corrupt") {
			backupFound = true
			break
		}
	}
	if !backupFound {
		t.Fatalf("expected backup of corrupted settings file in %s, found: %v", dir, entries)
	}
}

func TestStore_Reset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reset_settings.json")

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	_ = store.Save(Settings{Theme: ThemeLight, Locale: "es", AutoStartSession: true})
	if store.Get().Theme != ThemeLight {
		t.Fatal("failed to set theme light")
	}

	resetted, err := store.Reset()
	if err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if resetted.Theme != ThemeSystem || resetted.Locale != "system" || resetted.AutoStartSession {
		t.Fatalf("reset did not restore defaults: %+v", resetted)
	}
}

func TestStore_Concurrency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrency_settings.json")

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	workers := 10
	iterations := 20

	for i := 0; i < workers; i++ {
		wg.Add(2)
		// Reader goroutine
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_ = store.Get()
				time.Sleep(1 * time.Millisecond)
			}
		}()

		// Writer goroutine
		go func(id int) {
			defer wg.Done()
			theme := ThemeDark
			if id%2 == 0 {
				theme = ThemeLight
			}
			for j := 0; j < iterations; j++ {
				_ = store.Save(Settings{Theme: theme, AutoStartSession: id%2 == 0})
				time.Sleep(1 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()
	final := store.Get()
	if final.Theme != ThemeDark && final.Theme != ThemeLight {
		t.Fatalf("unexpected final theme: %q", final.Theme)
	}
}
