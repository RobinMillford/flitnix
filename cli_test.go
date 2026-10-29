package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
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

func TestHandleCLISendHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code, _ := handleCLI([]string{"flitnix", "send", "--help"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("expected handled=true code=0, got %v, %d", handled, code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "--to") || !strings.Contains(out, "Exit Codes:") {
		t.Fatalf("send help missing expected content: %s", out)
	}
}

func TestHandleCLISendMissingArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code, _ := handleCLI([]string{"flitnix", "send"}, &stdout, &stderr)
	if !handled || code != ExitUsageError {
		t.Fatalf("expected handled=true code=1, got %v, %d", handled, code)
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "no files or directories specified") {
		t.Fatalf("expected missing files error, got: %s", errOut)
	}
}

func TestHandleCLISendMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handled, code, _ := handleCLI([]string{
		"flitnix", "send",
		"--to", "http://127.0.0.1:43821/#dummy-token",
		"/path/to/nonexistent/file.bin",
	}, &stdout, &stderr)

	if !handled || code != ExitLocalPathError {
		t.Fatalf("expected handled=true code=2 (local path error), got %v, %d", handled, code)
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "cannot access") {
		t.Fatalf("expected cannot access error, got: %s", errOut)
	}
}

func TestRunServeApprovalLoopApprove(t *testing.T) {
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: t.TempDir()})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	sessionKey := "test-session-key"

	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{DisplayName: "hello.txt", Size: 5}},
	}
	view, err := coord.CreateBatch(sessionKey, manifest)
	if err != nil {
		t.Fatal(err)
	}
	up, err := transfers.Declare(sessionKey, "hello.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.SetItemContentID(sessionKey, view.ID, view.Items[0].ID, up.ID); err != nil {
		t.Fatal(err)
	}

	pendingChan := make(chan pendingServeBatch, 1)
	pendingChan <- pendingServeBatch{sessionKey: sessionKey, batch: view, remoteAddr: "192.168.1.50:54321"}
	close(pendingChan)

	var stdout bytes.Buffer
	in := strings.NewReader("y\n")

	ctx := context.Background()
	runServeApprovalLoop(ctx, pendingChan, transfers, coord, in, &stdout)

	// Verify batch is approved
	updated, err := coord.GetBatch(sessionKey, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ApprovalState != transfer.ApprovalApproved {
		t.Fatalf("expected ApprovalApproved, got %s", updated.ApprovalState)
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "192.168.1.50") || !strings.Contains(outStr, "approved") {
		t.Fatalf("stdout missing peer or approval text: %s", outStr)
	}
}

func TestRunServeApprovalLoopDecline(t *testing.T) {
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: t.TempDir()})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	sessionKey := "test-session-key-2"

	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{DisplayName: "declined.txt", Size: 8}},
	}
	view, err := coord.CreateBatch(sessionKey, manifest)
	if err != nil {
		t.Fatal(err)
	}
	up, err := transfers.Declare(sessionKey, "declined.txt", 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.SetItemContentID(sessionKey, view.ID, view.Items[0].ID, up.ID); err != nil {
		t.Fatal(err)
	}

	pendingChan := make(chan pendingServeBatch, 1)
	pendingChan <- pendingServeBatch{sessionKey: sessionKey, batch: view, remoteAddr: "10.0.0.42:12345"}
	close(pendingChan)

	var stdout bytes.Buffer
	// Empty response / Enter defaults to decline
	in := strings.NewReader("\n")

	ctx := context.Background()
	runServeApprovalLoop(ctx, pendingChan, transfers, coord, in, &stdout)

	// Verify batch is rejected
	updated, err := coord.GetBatch(sessionKey, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ApprovalState != transfer.ApprovalRejected {
		t.Fatalf("expected ApprovalRejected, got %s", updated.ApprovalState)
	}

	outStr := stdout.String()
	if !strings.Contains(outStr, "10.0.0.42") || !strings.Contains(outStr, "declined") {
		t.Fatalf("stdout missing peer or decline text: %s", outStr)
	}
}
