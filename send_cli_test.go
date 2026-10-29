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
