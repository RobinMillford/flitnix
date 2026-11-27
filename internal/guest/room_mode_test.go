package guest

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoomModesAndApprovalIsolation(t *testing.T) {
	t.Run("default upload_only preserves existing gates and disables downloads", func(t *testing.T) {
		dir := t.TempDir()
		sharedFile := filepath.Join(dir, "handout.txt")
		if err := os.WriteFile(sharedFile, []byte("secret handout"), 0644); err != nil {
			t.Fatal(err)
		}

		room, err := NewRoom(dir, time.Hour)
		if err != nil {
			t.Fatalf("NewRoom: %v", err)
		}
		if room.Mode != RoomModeUploadOnly {
			t.Fatalf("default Mode = %q; want upload_only", room.Mode)
		}
		if !room.AllowsUpload() || room.AllowsDownload() {
			t.Fatalf("AllowsUpload=%v AllowsDownload=%v; want true/false", room.AllowsUpload(), room.AllowsDownload())
		}
		if _, err := room.AddSharedPath(sharedFile); !errors.Is(err, ErrDownloadDisabled) {
			t.Fatalf("AddSharedPath in upload_only got %v; want ErrDownloadDisabled", err)
		}
	})

	t.Run("download_only allows shared paths, disables uploads, keeps guest join approval authoritative", func(t *testing.T) {
		dir := t.TempDir()
		handoutDir := filepath.Join(dir, "handouts")
		if err := os.MkdirAll(handoutDir, 0755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(handoutDir, "syllabus.pdf")
		if err := os.WriteFile(f, []byte("pdf-bytes"), 0644); err != nil {
			t.Fatal(err)
		}

		room, err := NewRoomWithConfig(RoomConfig{
			DestinationRoot: dir,
			Duration:        time.Hour,
			Mode:            RoomModeDownloadOnly,
			SharedPaths:     []string{handoutDir},
		})
		if err != nil {
			t.Fatalf("NewRoomWithConfig: %v", err)
		}
		if room.AllowsUpload() || !room.AllowsDownload() {
			t.Fatalf("AllowsUpload=%v AllowsDownload=%v; want false/true", room.AllowsUpload(), room.AllowsDownload())
		}
		sources := room.ListSharedSources()
		if len(sources) != 1 || sources[0].Name != "handouts" {
			t.Fatalf("ListSharedSources = %+v", sources)
		}

		g, err := room.Join("Student 1")
		if err != nil {
			t.Fatalf("Join: %v", err)
		}
		if g.State != GuestPending {
			t.Fatalf("new guest state = %s; want pending (join approval preserved)", g.State)
		}
		if room.ValidateGuestApproved(g.ID) {
			t.Fatal("pending guest should not pass ValidateGuestApproved")
		}
		if err := room.ApproveGuest(g.ID); err != nil {
			t.Fatalf("ApproveGuest: %v", err)
		}
		if !room.ValidateGuestApproved(g.ID) {
			t.Fatal("approved guest should pass ValidateGuestApproved")
		}
		if _, err := room.DeclareUpload(g.ID, "rogue.txt", 10); !errors.Is(err, ErrUploadDisabled) {
			t.Fatalf("DeclareUpload in download_only = %v; want ErrUploadDisabled", err)
		}
	})

	t.Run("two_way enforces BOTH independent approvals and private folder isolation", func(t *testing.T) {
		dir := t.TempDir()
		hostFile := filepath.Join(dir, "prompt.txt")
		if err := os.WriteFile(hostFile, []byte("lab instructions"), 0644); err != nil {
			t.Fatal(err)
		}

		room, err := NewRoomWithConfig(RoomConfig{
			DestinationRoot:   dir,
			Duration:          time.Hour,
			Mode:              RoomModeTwoWay,
			AutoApproveGuests: true, // Even if open join is enabled, incoming files MUST still need approval!
			SharedPaths:       []string{hostFile},
		})
		if err != nil {
			t.Fatalf("NewRoomWithConfig: %v", err)
		}
		if !room.AllowsUpload() || !room.AllowsDownload() {
			t.Fatalf("expected both upload and download allowed in two_way")
		}

		alice, err := room.Join("Alice")
		if err != nil {
			t.Fatalf("Join Alice: %v", err)
		}
		bob, err := room.Join("Bob")
		if err != nil {
			t.Fatalf("Join Bob: %v", err)
		}

		// File upload from approved guest must STILL start in UploadPendingApproval (never bypassed)
		up, err := room.DeclareUpload(alice.ID, "alice_work.txt", 11)
		if err != nil {
			t.Fatalf("DeclareUpload Alice: %v", err)
		}
		if up.State != UploadPendingApproval {
			t.Fatalf("upload initial state = %s; want pending approval (gates must not combine)", up.State)
		}
		if _, err := room.StreamUpload(up.ID, strings.NewReader("alice-bytes")); !errors.Is(err, ErrUploadNotApproved) {
			t.Fatalf("StreamUpload before ApproveUpload = %v; want ErrUploadNotApproved", err)
		}
		if err := room.ApproveUpload(up.ID); err != nil {
			t.Fatalf("ApproveUpload: %v", err)
		}
		res, err := room.StreamUpload(up.ID, strings.NewReader("alice-bytes"))
		if err != nil {
			t.Fatalf("StreamUpload after approval: %v", err)
		}
		if res.Written != 11 {
			t.Fatalf("Written = %d; want 11", res.Written)
		}

		// Verify Alice and Bob receive separate private subfolders (Bob cannot touch Alice's folder)
		upBob, err := room.DeclareUpload(bob.ID, "bob_work.txt", 9)
		if err != nil {
			t.Fatalf("DeclareUpload Bob: %v", err)
		}
		if err := room.ApproveUpload(upBob.ID); err != nil {
			t.Fatalf("ApproveUpload Bob: %v", err)
		}
		if _, err := room.StreamUpload(upBob.ID, bytes.NewReader([]byte("bob-bytes"))); err != nil {
			t.Fatalf("StreamUpload Bob: %v", err)
		}

		aliceFolder := filepath.Join(room.RoomFolder, alice.ID+"-"+alice.SafeSlug)
		bobFolder := filepath.Join(room.RoomFolder, bob.ID+"-"+bob.SafeSlug)
		if aliceFolder == bobFolder {
			t.Fatalf("expected distinct guest folders, got same %q", aliceFolder)
		}
		if _, err := os.Stat(filepath.Join(aliceFolder, "alice_work.txt")); err != nil {
			t.Fatalf("expected alice file in alice private folder: %v", err)
		}
		if _, err := os.Stat(filepath.Join(bobFolder, "alice_work.txt")); err == nil {
			t.Fatal("alice file leaked into bob folder")
		}
		// Shared files list contains ONLY hostFile, not uploaded guest files
		for _, s := range room.ListSharedSources() {
			if strings.Contains(s.Name, "alice_work") || strings.Contains(s.Name, "bob_work") {
				t.Fatalf("guest private upload %q leaked into shared sources", s.Name)
			}
		}
	})
}
