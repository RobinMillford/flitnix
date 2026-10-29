package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wailsapp/wails/v2/pkg/options"
)

func TestSecondInstanceLaunch_Handoff(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatal(err)
	}

	secondDir := t.TempDir()
	relFile := "second_instance_doc.txt"
	absFile := filepath.Join(secondDir, relFile)
	if err := os.WriteFile(absFile, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	relFolder := "second_instance_folder"
	absFolder := filepath.Join(secondDir, relFolder)
	if err := os.Mkdir(absFolder, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(absFolder, "inside.txt"), []byte("inner"), 0o644)

	// Simulate app startup
	app.startup(context.Background())

	// Simulate a second instance launch providing relative paths with working directory
	data := options.SecondInstanceData{
		Args:             []string{relFile, relFolder},
		WorkingDirectory: secondDir,
	}

	app.onSecondInstanceLaunch(data)

	// Verify sources were registered in the app registry
	sources := app.ListSharedSources()
	if len(sources) != 2 {
		t.Fatalf("expected 2 sources registered, got %d", len(sources))
	}

	var hasFile, hasFolder bool
	for _, s := range sources {
		if s.Name == relFile && s.Kind == "file" {
			hasFile = true
		}
		if s.Name == relFolder && s.Kind == "folder" {
			hasFolder = true
		}
	}
	if !hasFile || !hasFolder {
		t.Fatalf("expected both file and folder in registry: %+v", sources)
	}
}

func TestSecondInstanceLaunch_PreStartupQueue(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "pre_startup.txt")
	_ = os.WriteFile(f, []byte("pre"), 0o644)

	// Launch event before startup
	data := options.SecondInstanceData{
		Args:             []string{f},
		WorkingDirectory: dir,
	}
	app.onSecondInstanceLaunch(data)

	// Verify initialShares accumulated
	app.startupMu.Lock()
	if len(app.initialShares) != 1 {
		t.Fatalf("expected 1 initial share queued, got %d", len(app.initialShares))
	}
	app.startupMu.Unlock()

	// Complete startup
	app.startup(context.Background())

	// Verify sources registered after startup runs
	sources := app.ListSharedSources()
	if len(sources) != 1 {
		t.Fatalf("expected 1 source registered after startup, got %d", len(sources))
	}
}

func TestSecondInstanceLaunch_Deduplication(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatal(err)
	}
	app.startup(context.Background())

	dir := t.TempDir()
	f := filepath.Join(dir, "dedup_test.txt")
	_ = os.WriteFile(f, []byte("dedup"), 0o644)

	// Launch with duplicate entries in Args
	data := options.SecondInstanceData{
		Args:             []string{f, f, "dedup_test.txt"},
		WorkingDirectory: dir,
	}
	app.onSecondInstanceLaunch(data)

	sources := app.ListSharedSources()
	if len(sources) != 1 {
		t.Fatalf("expected exactly 1 source due to deduplication, got %d", len(sources))
	}

	// Repeated launch with the exact same file
	app.onSecondInstanceLaunch(data)
	sources2 := app.ListSharedSources()
	if len(sources2) != 1 {
		t.Fatalf("expected still exactly 1 source after second launch, got %d", len(sources2))
	}
}

func TestEnsureLinuxNautilusScript(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("XDG_CURRENT_DESKTOP", "GNOME")

	ensureLinuxNautilusScript()

	scriptPath := filepath.Join(tempHome, ".local", "share", "nautilus", "scripts", "Share with Flitnix")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("expected nautilus script to be created at %s: %v", scriptPath, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("expected script to have executable permissions, got %v", info.Mode())
	}
	content, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Fatal("expected non-empty script content")
	}

	// Idempotency: calling again should succeed without error or file corruption
	ensureLinuxNautilusScript()
}
