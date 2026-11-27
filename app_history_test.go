package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/events"
	"github.com/itsyamin/flitnix/internal/history"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestAppHistoryRecordingConfinementAndActions(t *testing.T) {
	a := newIsolatedApp(t)
	recvDir := a.store.Dir()
	if err := os.MkdirAll(recvDir, 0o700); err != nil {
		t.Fatal(err)
	}

	safeFile := filepath.Join(recvDir, "report.pdf")
	if err := os.WriteFile(safeFile, []byte("pdf-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, added := a.historyStore.Record(history.Record{
		DedupKey:    "upload:u-safe",
		TransferID:  "u-safe",
		Timestamp:   time.Now().UTC(),
		Direction:   history.DirectionIncoming,
		Kind:        "file",
		DisplayName: "report.pdf",
		Size:        9,
		Transferred: 9,
		Status:      history.StatusCompleted,
		DeviceName:  "My Phone",
		LocalPath:   safeFile,
	})
	if !added {
		t.Fatal("expected safe history record added")
	}

	// Query view must NEVER expose raw LocalPath, and must mark file actionable.
	res, err := a.QueryTransferHistory(HistoryFilterView{})
	if err != nil {
		t.Fatalf("QueryTransferHistory: %v", err)
	}
	if res.Total != 1 || !res.Records[0].CanOpen || !res.Records[0].CanReveal || !res.Records[0].CanReshare {
		t.Fatalf("unexpected history view: %+v", res)
	}

	// OpenHistoryFile runs OS opener with safe path (mocked for headless test safety).
	origExec := execHistoryCommand
	var launchedName string
	var launchedArgs []string
	execHistoryCommand = func(name string, args ...string) error {
		launchedName = name
		launchedArgs = append([]string(nil), args...)
		return nil
	}
	t.Cleanup(func() { execHistoryCommand = origExec })

	if err := a.OpenHistoryFile(rec.ID); err != nil {
		t.Fatalf("OpenHistoryFile: %v", err)
	}
	if launchedName == "" || len(launchedArgs) == 0 {
		t.Fatalf("expected OS command invoked, got %q %v", launchedName, launchedArgs)
	}

	// Reshare stages the safe file in registry cleanly.
	if err := a.ReshareHistoryFile(rec.ID); err != nil {
		t.Fatalf("ReshareHistoryFile: %v", err)
	}
	if len(a.registry.ListSources()) == 0 {
		t.Fatal("expected reshared source registered")
	}

	// Security check: incoming record pointing outside receive directory (or symlink escape) must fail closed.
	outsideFile := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	badRec, _ := a.historyStore.Record(history.Record{
		DedupKey:    "upload:u-escape",
		TransferID:  "u-escape",
		Timestamp:   time.Now().UTC(),
		Direction:   history.DirectionIncoming,
		DisplayName: "escape.txt",
		Status:      history.StatusCompleted,
		LocalPath:   outsideFile,
	})
	if err := a.OpenHistoryFile(badRec.ID); err == nil {
		t.Fatal("expected path confinement rejection for outside file")
	}

	symlinkEscape := filepath.Join(recvDir, "sym.txt")
	if err := os.Symlink(outsideFile, symlinkEscape); err == nil {
		symRec, _ := a.historyStore.Record(history.Record{
			DedupKey:    "upload:u-sym",
			TransferID:  "u-sym",
			Timestamp:   time.Now().UTC(),
			Direction:   history.DirectionIncoming,
			DisplayName: "sym.txt",
			Status:      history.StatusCompleted,
			LocalPath:   symlinkEscape,
		})
		if err := a.OpenHistoryFile(symRec.ID); err == nil {
			t.Fatal("expected symlink escape rejection outside receive dir")
		}
	}

	// Clearing history preserves files on disk.
	if err := a.ClearTransferHistory(); err != nil {
		t.Fatalf("ClearTransferHistory: %v", err)
	}
	if _, err := os.Stat(safeFile); err != nil {
		t.Fatalf("ClearTransferHistory must not delete user files: %v", err)
	}
}

func TestRecordHistoryFromEvent(t *testing.T) {
	a := newIsolatedApp(t)
	a.recordHistoryFromEvent(events.DesktopEvent{
		Event: events.Event{Type: events.TypeDownloadUpdated},
		RawPayload: DownloadView{
			ID:      "dl-1",
			FileID:  "f-1",
			Name:    "slides.pptx",
			Kind:    "file",
			Size:    1000,
			Written: 1000,
			State:   string(transfer.DownloadSent),
		},
	})
	res, err := a.QueryTransferHistory(HistoryFilterView{Direction: history.DirectionOutgoing})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Records[0].Status != history.StatusCompleted || res.Records[0].DisplayName != "slides.pptx" {
		t.Fatalf("unexpected download history record: %+v", res)
	}
}
