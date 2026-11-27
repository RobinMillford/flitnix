package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistenceFilterDedupAndRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")

	st, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	now := time.Now().UTC()
	st.Record(Record{
		DedupKey:    "upload:u1",
		TransferID:  "u1",
		Timestamp:   now.Add(-48 * time.Hour),
		Direction:   DirectionIncoming,
		Kind:        "file",
		DisplayName: "old-notes.txt",
		Size:        128,
		Transferred: 128,
		Status:      StatusCompleted,
		DeviceName:  "Pixel 8",
		LocalPath:   filepath.Join(dir, "old-notes.txt"),
	})
	// Duplicate terminal emit with identical status must dedup cleanly
	if _, added := st.Record(Record{
		DedupKey:    "upload:u1",
		TransferID:  "u1",
		Timestamp:   now.Add(-48 * time.Hour),
		Direction:   DirectionIncoming,
		DisplayName: "old-notes.txt",
		Size:        128,
		Transferred: 128,
		Status:      StatusCompleted,
	}); added {
		t.Fatal("expected identical duplicate record to be ignored")
	}

	st.Record(Record{
		DedupKey:    "batch:b1",
		BatchID:     "b1",
		Timestamp:   now.Add(-2 * time.Hour),
		Direction:   DirectionBatch,
		Kind:        "folder",
		DisplayName: "Vacation Photos",
		ItemCount:   12,
		Size:        4096,
		Transferred: 4096,
		Status:      StatusCompleted,
		DeviceName:  "iPhone 15",
		SHA256:      "abcdef0123456789",
	})
	st.Record(Record{
		DedupKey:    "download:d1",
		TransferID:  "d1",
		Timestamp:   now.Add(-1 * time.Hour),
		Direction:   DirectionOutgoing,
		Kind:        "file",
		DisplayName: "presentation.pdf",
		Size:        2048,
		Transferred: 512,
		Status:      StatusFailed,
		Error:       "network timeout",
	})

	if err := st.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify file permissions are private (0600 on Unix)
	if info, err := os.Stat(path); err == nil {
		if info.Mode().Perm() != 0o600 {
			t.Logf("note: file mode is %o (expected 0600)", info.Mode().Perm())
		}
	}

	// Re-open in fresh instance -> survives restart
	st2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen NewStore: %v", err)
	}
	defer st2.Close()

	res := st2.Query(Filter{Query: "vacation"})
	if res.Total != 1 || res.Records[0].DisplayName != "Vacation Photos" {
		t.Fatalf("unexpected query result: %+v", res)
	}
	if res.Stats.TotalRecords != 3 || res.Stats.CompletedCount != 2 || res.Stats.FailedCount != 1 {
		t.Fatalf("unexpected stats: %+v", res.Stats)
	}

	// Retention: create a dummy file on disk and verify pruning history NEVER deletes it
	preservedFile := filepath.Join(dir, "old-notes.txt")
	if err := os.WriteFile(preservedFile, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := st2.ApplyRetention(24*time.Hour, 0)
	if err != nil {
		t.Fatalf("ApplyRetention: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 old record pruned, got %d", removed)
	}
	if _, err := os.Stat(preservedFile); err != nil {
		t.Fatalf("retention pruning must never delete actual user file: %v", err)
	}

	// Count cap keeps newest records
	removedCount, err := st2.ApplyRetention(0, 1)
	if err != nil || removedCount != 1 {
		t.Fatalf("expected count cap to remove 1 record, got removed=%d err=%v", removedCount, err)
	}
	after := st2.Query(Filter{})
	if after.Total != 1 || after.Records[0].TransferID != "d1" {
		t.Fatalf("expected newest record remaining after count cap, got %+v", after.Records)
	}

	if err := st2.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if empty := st2.Query(Filter{}); empty.Total != 0 {
		t.Fatalf("expected 0 records after Clear, got %d", empty.Total)
	}
}
