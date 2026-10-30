package guest

import (
	"testing"
	"time"
)

func TestManagerOperations(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager()
	defer mgr.Close()

	if mgr.ActiveRoom() != nil {
		t.Fatal("expected no active room initially")
	}

	room, err := mgr.CreateRoom(tmpDir, 30*time.Minute)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	if mgr.ActiveRoom() == nil || mgr.ActiveRoom().ID != room.ID {
		t.Fatal("expected active room to match")
	}

	guest, err := room.Join("Charlie")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	// Validate before approval
	r, g, err := mgr.ValidateGuest(guest.Token)
	if err != nil || r.ID != room.ID || g.ID != guest.ID {
		t.Fatalf("ValidateGuest: %v", err)
	}

	// End room
	if err := mgr.EndRoom(); err != nil {
		t.Fatalf("EndRoom: %v", err)
	}

	// Guest validation on ended room returns ErrRoomEnded
	_, _, err = mgr.ValidateGuest(guest.Token)
	if err != ErrRoomEnded {
		t.Fatalf("expected ErrRoomEnded, got %v", err)
	}
}
