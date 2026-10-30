package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func setupTestServer(t *testing.T) (*server.Server, *session.Manager, *transfer.Coordinator, *transfer.Service, string) {
	t.Helper()
	receiveDir := t.TempDir()

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: receiveDir})
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	svcs := &server.Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
	}

	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0", // ephemeral port
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		sessMgr.Close()
	})

	go func() { _ = srv.Serve(ctx) }()

	select {
	case <-srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("server timed out becoming ready")
	}

	return srv, sessMgr, coord, transfers, receiveDir
}

func TestCLISendEndToEndSingleFile(t *testing.T) {
	srv, sessMgr, coord, transfers, receiveDir := setupTestServer(t)

	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatalf("Create session failed: %v", err)
	}

	pairingURL := srv.URL() + "/#" + sess.Token()
	target, err := client.ParseTarget(pairingURL, "")
	if err != nil {
		t.Fatalf("ParseTarget failed: %v", err)
	}

	cli := client.NewClient(target)
	ctx := context.Background()

	// 1. Verify ping
	if err := cli.Ping(ctx); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}

	// 2. Prepare test file
	tmpDir := t.TempDir()
	testPath := filepath.Join(tmpDir, "document.txt")
	testData := []byte("Flitnix end-to-end CLI transfer payload verification test.")
	if err := os.WriteFile(testPath, testData, 0o644); err != nil {
		t.Fatal(err)
	}

	expectedSum := sha256.Sum256(testData)
	expectedSHA := hex.EncodeToString(expectedSum[:])

	lm, err := client.BuildManifest([]string{testPath}, "")
	if err != nil {
		t.Fatalf("BuildManifest failed: %v", err)
	}

	// 3. Declare batch
	batchView, err := cli.DeclareBatch(ctx, lm.Manifest)
	if err != nil {
		t.Fatalf("DeclareBatch failed: %v", err)
	}

	// 4. Approve batch on server (simulating recipient approval)
	for _, item := range batchView.Items {
		contentID, err := coord.ItemContentID(sess.Key(), batchView.ID, item.ID)
		if err != nil {
			t.Fatalf("ItemContentID failed: %v", err)
		}
		if _, err := transfers.Decide(contentID, true); err != nil {
			t.Fatalf("Transfers.Decide failed: %v", err)
		}
	}
	if _, err := coord.ApproveBatch(sess.Key(), batchView.ID); err != nil {
		t.Fatalf("ApproveBatch failed: %v", err)
	}

	// 5. Wait for approval on client
	approvedView, err := cli.WaitForApproval(ctx, batchView.ID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForApproval failed: %v", err)
	}
	if approvedView.ApprovalState != transfer.ApprovalApproved {
		t.Fatalf("batch state = %s, want approved", approvedView.ApprovalState)
	}

	// 6. Stream payload
	item := approvedView.Items[0]
	res, err := cli.StreamItem(ctx, approvedView.ID, item.ID, testPath, nil)
	if err != nil {
		t.Fatalf("StreamItem failed: %v", err)
	}

	if res.Written != int64(len(testData)) {
		t.Errorf("res.Written = %d, want %d", res.Written, len(testData))
	}
	if res.SHA256 != expectedSHA {
		t.Errorf("res.SHA256 = %s, want %s", res.SHA256, expectedSHA)
	}
	if !res.Verified {
		t.Errorf("res.Verified = false, want true")
	}

	// 7. Verify file on disk in receiveDir
	receivedPath := filepath.Join(receiveDir, "document.txt")
	diskBytes, err := os.ReadFile(receivedPath)
	if err != nil {
		t.Fatalf("failed to read received file from disk: %v", err)
	}
	if !bytes.Equal(diskBytes, testData) {
		t.Fatalf("received bytes on disk mismatch!")
	}
}

func TestCLISendEndToEndDirectory(t *testing.T) {
	srv, sessMgr, coord, transfers, receiveDir := setupTestServer(t)

	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatalf("Create session failed: %v", err)
	}

	pairingURL := srv.URL() + "/#" + sess.Token()
	target, err := client.ParseTarget(pairingURL, "")
	if err != nil {
		t.Fatalf("ParseTarget failed: %v", err)
	}

	cli := client.NewClient(target)
	ctx := context.Background()

	// Prepare directory tree:
	// sourceDir/
	//   fileA.txt
	//   sub/
	//     fileB.txt
	sourceDir := t.TempDir()
	fileA := filepath.Join(sourceDir, "fileA.txt")
	subDir := filepath.Join(sourceDir, "sub")
	fileB := filepath.Join(subDir, "fileB.txt")

	_ = os.WriteFile(fileA, []byte("Content of file A"), 0o644)
	_ = os.Mkdir(subDir, 0o755)
	_ = os.WriteFile(fileB, []byte("Content of nested file B in subfolder"), 0o644)

	lm, err := client.BuildManifest([]string{sourceDir}, "custom-root")
	if err != nil {
		t.Fatalf("BuildManifest failed: %v", err)
	}
	if lm.Manifest.Kind != transfer.BatchKindFolder || lm.Manifest.RootName != "custom-root" {
		t.Fatalf("invalid manifest kind or root: %+v", lm.Manifest)
	}

	batchView, err := cli.DeclareBatch(ctx, lm.Manifest)
	if err != nil {
		t.Fatalf("DeclareBatch failed: %v", err)
	}

	// Server approves
	for _, item := range batchView.Items {
		contentID, err := coord.ItemContentID(sess.Key(), batchView.ID, item.ID)
		if err != nil {
			t.Fatalf("ItemContentID failed: %v", err)
		}
		if _, err := transfers.Decide(contentID, true); err != nil {
			t.Fatalf("Transfers.Decide failed: %v", err)
		}
	}
	if _, err := coord.ApproveBatch(sess.Key(), batchView.ID); err != nil {
		t.Fatalf("ApproveBatch failed: %v", err)
	}

	approvedView, err := cli.WaitForApproval(ctx, batchView.ID, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForApproval failed: %v", err)
	}

	for i, item := range approvedView.Items {
		localItem := lm.LocalItems[i]
		res, streamErr := cli.StreamItem(ctx, approvedView.ID, item.ID, localItem.Path, nil)
		if streamErr != nil {
			t.Fatalf("StreamItem for %s failed: %v", item.DisplayName, streamErr)
		}
		if !res.Verified {
			t.Fatalf("StreamItem verification failed for %s", item.DisplayName)
		}
	}

	// Verify directory structure on disk
	expectedDir := filepath.Join(receiveDir, "custom-root")
	if _, err := os.Stat(expectedDir); err != nil {
		t.Fatalf("expected root directory not created: %v", err)
	}
	diskA, err := os.ReadFile(filepath.Join(expectedDir, "fileA.txt"))
	if err != nil || string(diskA) != "Content of file A" {
		t.Errorf("fileA mismatch: %s, %v", string(diskA), err)
	}
	diskB, err := os.ReadFile(filepath.Join(expectedDir, "sub", "fileB.txt"))
	if err != nil || string(diskB) != "Content of nested file B in subfolder" {
		t.Errorf("fileB mismatch: %s, %v", string(diskB), err)
	}
}

func TestCLISendRecipientRejection(t *testing.T) {
	srv, sessMgr, coord, transfers, _ := setupTestServer(t)

	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}

	cli := client.NewClient(mustParseTarget(t, srv.URL()+"/#"+sess.Token()))
	ctx := context.Background()

	tmpFile := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(tmpFile, []byte("xyz"), 0o644)
	lm, _ := client.BuildManifest([]string{tmpFile}, "")

	batchView, err := cli.DeclareBatch(ctx, lm.Manifest)
	if err != nil {
		t.Fatal(err)
	}

	// Server declines
	for _, item := range batchView.Items {
		contentID, err := coord.ItemContentID(sess.Key(), batchView.ID, item.ID)
		if err == nil {
			_, _ = transfers.Decide(contentID, false)
		}
	}
	_, _ = coord.RejectBatch(sess.Key(), batchView.ID)

	_, err = cli.WaitForApproval(ctx, batchView.ID, 50*time.Millisecond)
	if !errors.Is(err, client.ErrRejected) {
		t.Errorf("expected ErrRejected, got %v", err)
	}
}

func mustParseTarget(t *testing.T, s string) *client.Target {
	t.Helper()
	target, err := client.ParseTarget(s, "")
	if err != nil {
		t.Fatalf("ParseTarget failed: %v", err)
	}
	return target
}
