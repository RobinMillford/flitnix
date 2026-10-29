package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestRunSendCLIJSONOutput(t *testing.T) {
	// Set up server
	receiveDir := t.TempDir()
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: receiveDir})
	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	pendingBatches := make(chan pendingServeBatch, 16)
	svcs := &server.Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
		OnBatchPending: func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
			pendingBatches <- pendingServeBatch{sessionKey: sessionKey, batch: batch, remoteAddr: remoteAddr}
		},
	}

	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("server timed out")
	}

	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}
	pairingURL := srv.URL() + "/#" + sess.Token()

	// Automatically approve any incoming batch in background
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-pendingBatches:
				time.Sleep(20 * time.Millisecond)
				for _, it := range ev.batch.Items {
					if cid, err := coord.ItemContentID(ev.sessionKey, ev.batch.ID, it.ID); err == nil && cid != "" {
						_, _ = transfers.Decide(cid, true)
					}
				}
				_, _ = coord.ApproveBatch(ev.sessionKey, ev.batch.ID)
			}
		}
	}()

	// Prepare file
	tmpFile := filepath.Join(t.TempDir(), "sample.txt")
	testData := []byte("Machine-readable JSON CLI transfer test content.")
	if err := os.WriteFile(tmpFile, testData, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{
		"--to", pairingURL,
		"--json",
		"--yes",
		tmpFile,
	}

	code := runSendCLI(args, &stdout, &stderr)
	if code != ExitSuccess {
		t.Fatalf("runSendCLI returned code %d, stderr: %s", code, stderr.String())
	}

	// Verify stdout is valid JSON
	var res struct {
		Status         string  `json:"status"`
		BatchID        string  `json:"batchId"`
		TotalItems     int     `json:"totalItems"`
		TotalBytes     int64   `json:"totalBytes"`
		ElapsedSeconds float64 `json:"elapsedSeconds"`
		Verified       bool    `json:"verified"`
		Items          []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			SHA256   string `json:"sha256"`
			Verified bool   `json:"verified"`
		} `json:"items"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nOutput:\n%s", err, stdout.String())
	}

	if res.Status != "success" || !res.Verified || res.TotalItems != 1 || res.TotalBytes != int64(len(testData)) {
		t.Fatalf("unexpected JSON result content: %+v", res)
	}

	if len(res.Items) != 1 || !res.Items[0].Verified || res.Items[0].SHA256 == "" {
		t.Fatalf("invalid item result in JSON: %+v", res.Items)
	}

	// Verify secret token is NOT leaked in stdout or stderr
	if strings.Contains(stdout.String(), sess.Token()) {
		t.Fatalf("secret token leaked in JSON stdout!")
	}
	if strings.Contains(stderr.String(), sess.Token()) {
		t.Fatalf("secret token leaked in stderr!")
	}
}

func TestRunSendCLIApprovalTimeoutCancelsServerBatch(t *testing.T) {
	receiveDir := t.TempDir()
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: receiveDir})
	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	var pendingBatchID string
	svcs := &server.Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
		OnBatchPending: func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
			pendingBatchID = batch.ID
		},
	}

	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("server timed out")
	}

	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}
	pairingURL := srv.URL() + "/#" + sess.Token()

	tmpFile := filepath.Join(t.TempDir(), "timeout_test.txt")
	_ = os.WriteFile(tmpFile, []byte("timeout test file"), 0o644)

	var stdout, stderr bytes.Buffer
	args := []string{
		"--to", pairingURL,
		"--timeout", "120ms",
		"--yes",
		tmpFile,
	}

	code := runSendCLI(args, &stdout, &stderr)
	if code != ExitApprovalRefused {
		t.Fatalf("expected code %d (ExitApprovalRefused), got %d, stderr: %s", ExitApprovalRefused, code, stderr.String())
	}

	// Verify the batch on server was canceled and is not left pending
	if pendingBatchID == "" {
		t.Fatal("OnBatchPending was not invoked")
	}
	batchView, err := coord.GetBatch(sess.Key(), pendingBatchID)
	if err != nil {
		t.Fatal(err)
	}
	if batchView.State != transfer.BatchStateCanceled {
		t.Fatalf("expected server batch state to be BatchStateCanceled after client timeout cleanup, got %s", batchView.State)
	}
}

func TestRunSendCLITokenFromStdin(t *testing.T) {
	receiveDir := t.TempDir()
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: receiveDir})
	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	pendingBatches := make(chan pendingServeBatch, 16)
	svcs := &server.Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
		OnBatchPending: func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
			pendingBatches <- pendingServeBatch{sessionKey: sessionKey, batch: batch, remoteAddr: remoteAddr}
		},
	}

	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("server timed out")
	}

	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}

	// Auto approve
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-pendingBatches:
				time.Sleep(20 * time.Millisecond)
				for _, it := range ev.batch.Items {
					if cid, err := coord.ItemContentID(ev.sessionKey, ev.batch.ID, it.ID); err == nil && cid != "" {
						_, _ = transfers.Decide(cid, true)
					}
				}
				_, _ = coord.ApproveBatch(ev.sessionKey, ev.batch.ID)
			}
		}
	}()

	tmpFile := filepath.Join(t.TempDir(), "stdin_token.txt")
	_ = os.WriteFile(tmpFile, []byte("token from stdin"), 0o644)

	// Create OS pipe for stdin delivering token
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer w.Close()
		_, _ = w.WriteString(sess.Token() + "\n")
	}()

	var stdout, stderr bytes.Buffer
	args := []string{
		"--to", srv.URL(), // Base URL only (no fragment token)
		"--token", "-",    // Read token from stdin
		"--yes",
		tmpFile,
	}

	code := runSendCLIWithStdin(args, r, &stdout, &stderr)
	_ = r.Close()

	if code != ExitSuccess {
		t.Fatalf("expected ExitSuccess, got %d, stderr: %s", code, stderr.String())
	}
}

func TestRunSendCLISecurityNotice(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "dummy.txt")
	_ = os.WriteFile(tmpFile, []byte("dummy"), 0o644)

	var stdout, stderr bytes.Buffer
	args := []string{
		"--to", "http://127.0.0.1:49999/#dummy-token",
		"--yes",
		tmpFile,
	}

	_ = runSendCLI(args, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "Notice: Pairing token passed via command-line arguments") {
		t.Fatalf("expected security notice in stderr, got: %s", stderr.String())
	}

	// Quiet mode suppresses notice
	stderr.Reset()
	argsQuiet := []string{
		"--to", "http://127.0.0.1:49999/#dummy-token",
		"--quiet",
		"--yes",
		tmpFile,
	}
	_ = runSendCLI(argsQuiet, &stdout, &stderr)
	if strings.Contains(stderr.String(), "Notice: Pairing token passed via command-line arguments") {
		t.Fatalf("expected security notice to be suppressed by --quiet, got: %s", stderr.String())
	}
}
