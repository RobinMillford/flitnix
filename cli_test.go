package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleCLIVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code, shares := handleCLI([]string{"flitnix", "--version"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("expected handled=true code=0, got %v, %d", handled, code)
	}
	if len(shares) != 0 {
		t.Fatalf("expected no initial shares, got %v", shares)
	}
	out := stdout.String()
	if !strings.Contains(out, "Flitnix") || !strings.Contains(out, AppVersion) {
		t.Fatalf("version output missing details: %q", out)
	}
}

func TestHandleCLIHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code, _ := handleCLI([]string{"flitnix", "--help"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("expected handled=true code=0, got %v, %d", handled, code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "doctor") {
		t.Fatalf("help output missing details: %q", out)
	}
}

func TestHandleCLIDoctor(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, _, _ := handleCLI([]string{"flitnix", "doctor", "--json"}, &stdout, &stderr)
	if !handled {
		t.Fatalf("expected doctor command to be handled")
	}
	out := stdout.String()
	if !strings.Contains(out, "timestamp") || !strings.Contains(out, "items") {
		t.Fatalf("expected JSON doctor output, got: %q", out)
	}
}

func TestHandleCLILaunchShares(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	_ = os.WriteFile(testFile, []byte("hello"), 0o644)

	var stdout, stderr bytes.Buffer
	handled, code, shares := handleCLI([]string{"flitnix", testFile}, &stdout, &stderr)
	if handled {
		t.Fatalf("expected handled=false for GUI launch with file args, got true")
	}
	if code != 0 {
		t.Fatalf("expected code 0, got %d", code)
	}
	if len(shares) != 1 || shares[0] != testFile {
		t.Fatalf("expected 1 share %q, got %v", testFile, shares)
	}
}
