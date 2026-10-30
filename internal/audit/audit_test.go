package audit

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAuditRecordAndQuery(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "audit.jsonl")

	store, err := NewStore(logPath, 100)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	now := time.Now()
	store.Record(Entry{
		EventType: EventRoomCreated,
		RoomID:    "room-1",
		Outcome:   OutcomeSuccess,
		Timestamp: now.Add(-10 * time.Minute),
		Details:   "Room created for team assignment",
	})

	store.Record(Entry{
		EventType: EventGuestJoined,
		RoomID:    "room-1",
		GuestID:   "guest-101",
		GuestName: "Alice",
		Outcome:   OutcomeSuccess,
		Timestamp: now.Add(-5 * time.Minute),
	})

	store.Record(Entry{
		EventType: EventUploadCompleted,
		RoomID:    "room-1",
		GuestID:   "guest-101",
		GuestName: "Alice",
		FileName:  "project.pdf",
		FileSize:  1024,
		SHA256:    "abcd1234ef56",
		Outcome:   OutcomeSuccess,
		Timestamp: now,
	})

	// 1. Query all
	all, total := store.Query(Filter{})
	if total != 3 || len(all) != 3 {
		t.Fatalf("expected 3 entries, got total=%d len=%d", total, len(all))
	}
	// Verify sorted newest first
	if all[0].EventType != EventUploadCompleted {
		t.Fatalf("expected newest first, got %s", all[0].EventType)
	}

	// 2. Query by room
	roomEvents, total := store.Query(Filter{RoomID: "room-1"})
	if total != 3 {
		t.Fatalf("expected 3 room events, got %d", total)
	}
	_ = roomEvents

	// 3. Query by guest
	guestEvents, total := store.Query(Filter{GuestID: "guest-101"})
	if total != 2 {
		t.Fatalf("expected 2 guest events, got %d", total)
	}
	_ = guestEvents

	// 4. Query with pagination
	page1, total := store.Query(Filter{Limit: 2, Offset: 0})
	if total != 3 || len(page1) != 2 {
		t.Fatalf("expected 2 items in page 1, got %d (total=%d)", len(page1), total)
	}
	page2, total := store.Query(Filter{Limit: 2, Offset: 2})
	if total != 3 || len(page2) != 1 {
		t.Fatalf("expected 1 item in page 2, got %d (total=%d)", len(page2), total)
	}

	// 5. Clear audit log
	if err := store.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	cleared, total := store.Query(Filter{})
	if total != 0 || len(cleared) != 0 {
		t.Fatalf("expected 0 entries after clear, got %d", total)
	}
}

func TestAuditSanitization(t *testing.T) {
	store, err := NewStore("", 50)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	store.Record(Entry{
		EventType: EventGuestJoined,
		GuestName: "Bad\x00Name\x07WithControlChars",
		FileName:  "file\r\n\t.txt",
		Outcome:   OutcomeInfo,
	})

	items, total := store.Query(Filter{})
	if total != 1 {
		t.Fatalf("expected 1 item, got %d", total)
	}
	if items[0].GuestName != "BadNameWithControlChars" {
		t.Fatalf("unexpected sanitized name: %q", items[0].GuestName)
	}
	if items[0].FileName != "file.txt" {
		t.Fatalf("unexpected sanitized filename: %q", items[0].FileName)
	}
}
