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

func TestSanitizeNickname(t *testing.T) {
	cases := []struct {
		in       string
		wantDisp string
		wantSlug string
	}{
		{"", "Guest", "guest"},
		{"   ", "Guest", "guest"},
		{"Alice", "Alice", "alice"},
		{"Bob's iPhone 15 Pro!", "Bob's iPhone 15 Pro!", "bob-s-iphone-15-pro"},
		{"   Charlie & Friends   ", "Charlie & Friends", "charlie-friends"},
		{"../../../etc/passwd", "../../../etc/passwd", "etc-passwd"},
		{strings.Repeat("A", 100), strings.Repeat("A", 30), strings.Repeat("a", 24)},
	}

	for _, tc := range cases {
		disp, slug := SanitizeNickname(tc.in)
		if disp != tc.wantDisp {
			t.Errorf("SanitizeNickname(%q) display = %q; want %q", tc.in, disp, tc.wantDisp)
		}
		if slug != tc.wantSlug {
			t.Errorf("SanitizeNickname(%q) slug = %q; want %q", tc.in, slug, tc.wantSlug)
		}
	}
}

func TestRoomLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	room, err := NewRoom(tmpDir, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}

	if room.State != RoomActive {
		t.Fatalf("expected state %s, got %s", RoomActive, room.State)
	}

	// Secret validation
	if !room.ValidateInviteSecret(room.InviteSecret) {
		t.Fatal("expected invite secret to be valid")
	}
	if room.ValidateInviteSecret("wrong-secret") {
		t.Fatal("expected wrong secret to fail")
	}

	// Wait for expiry
	time.Sleep(30 * time.Millisecond)
	state := room.CheckExpiry()
	if state != RoomExpired {
		t.Fatalf("expected room to be expired, got %s", state)
	}

	// After expiry, secret validation fails
	if room.ValidateInviteSecret(room.InviteSecret) {
		t.Fatal("expected invite secret to be invalid after room expiry")
	}

	// Join fails on expired room
	_, err = room.Join("Alice")
	if err != ErrRoomInactive {
		t.Fatalf("expected ErrRoomInactive, got %v", err)
	}
}

func TestGuestApprovalAndRevocation(t *testing.T) {
	tmpDir := t.TempDir()
	room, err := NewRoom(tmpDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}

	// Join Guest A
	guestA, err := room.Join("Alice Phone")
	if err != nil {
		t.Fatalf("Join guestA: %v", err)
	}
	if guestA.State != GuestPending {
		t.Fatalf("expected guestA pending, got %s", guestA.State)
	}

	// Join Guest B
	guestB, err := room.Join("Bob Laptop")
	if err != nil {
		t.Fatalf("Join guestB: %v", err)
	}

	// Reject Guest B
	if err := room.RejectGuest(guestB.ID); err != nil {
		t.Fatalf("RejectGuest: %v", err)
	}
	gB, _ := room.GetGuest(guestB.ID)
	if gB.State != GuestRejected {
		t.Fatalf("expected guestB rejected, got %s", gB.State)
	}

	// Token validation for rejected guest
	_, err = room.ValidateGuestToken(guestB.Token)
	if err != ErrGuestRejected {
		t.Fatalf("expected ErrGuestRejected, got %v", err)
	}

	// Approve Guest A
	if err := room.ApproveGuest(guestA.ID); err != nil {
		t.Fatalf("ApproveGuest: %v", err)
	}
	gA, _ := room.GetGuest(guestA.ID)
	if gA.State != GuestApproved {
		t.Fatalf("expected guestA approved, got %s", gA.State)
	}

	// Revoke Guest A
	if err := room.RevokeGuest(guestA.ID); err != nil {
		t.Fatalf("RevokeGuest: %v", err)
	}
	gA, _ = room.GetGuest(guestA.ID)
	if gA.State != GuestRevoked {
		t.Fatalf("expected guestA revoked, got %s", gA.State)
	}

	// Token validation for revoked guest
	_, err = room.ValidateGuestToken(guestA.Token)
	if err != ErrGuestRevoked {
		t.Fatalf("expected ErrGuestRevoked, got %v", err)
	}
}

func TestGuestUploadWorkflowAndIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	room, err := NewRoom(tmpDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}

	// Create and approve two guests
	guestA, err := room.Join("Alice")
	if err != nil {
		t.Fatalf("Join A: %v", err)
	}
	if err := room.ApproveGuest(guestA.ID); err != nil {
		t.Fatalf("Approve A: %v", err)
	}

	guestB, err := room.Join("Bob")
	if err != nil {
		t.Fatalf("Join B: %v", err)
	}
	if err := room.ApproveGuest(guestB.ID); err != nil {
		t.Fatalf("Approve B: %v", err)
	}

	// Guest A declares upload
	contentA := []byte("hello from alice's camera")
	upA, err := room.DeclareUpload(guestA.ID, "photo.jpg", int64(len(contentA)))
	if err != nil {
		t.Fatalf("DeclareUpload A: %v", err)
	}
	if upA.State != UploadPendingApproval {
		t.Fatalf("expected pending approval, got %s", upA.State)
	}

	// Cannot stream before approval
	_, err = room.StreamUpload(upA.ID, bytes.NewReader(contentA))
	if err != ErrUploadNotApproved {
		t.Fatalf("expected ErrUploadNotApproved, got %v", err)
	}

	// Host approves upload
	if err := room.ApproveUpload(upA.ID); err != nil {
		t.Fatalf("ApproveUpload A: %v", err)
	}

	// Stream upload
	res, err := room.StreamUpload(upA.ID, bytes.NewReader(contentA))
	if err != nil {
		t.Fatalf("StreamUpload A: %v", err)
	}
	if res.Written != int64(len(contentA)) {
		t.Fatalf("written %d; want %d", res.Written, len(contentA))
	}

	// Verify file landed in guest A's isolated folder
	guestAFolder := filepath.Join(room.RoomFolder, guestA.ID+"-"+guestA.SafeSlug)
	receivedA := filepath.Join(guestAFolder, "photo.jpg")
	data, err := os.ReadFile(receivedA)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", receivedA, err)
	}
	if !bytes.Equal(data, contentA) {
		t.Fatalf("data mismatch in received file: %q != %q", string(data), string(contentA))
	}

	// Now Guest B uploads a file with the SAME name "photo.jpg"
	contentB := []byte("different photo from bob")
	upB, err := room.DeclareUpload(guestB.ID, "photo.jpg", int64(len(contentB)))
	if err != nil {
		t.Fatalf("DeclareUpload B: %v", err)
	}
	if err := room.ApproveUpload(upB.ID); err != nil {
		t.Fatalf("ApproveUpload B: %v", err)
	}
	_, err = room.StreamUpload(upB.ID, bytes.NewReader(contentB))
	if err != nil {
		t.Fatalf("StreamUpload B: %v", err)
	}

	// Verify file landed in guest B's isolated folder, not affecting A
	guestBFolder := filepath.Join(room.RoomFolder, guestB.ID+"-"+guestB.SafeSlug)
	receivedB := filepath.Join(guestBFolder, "photo.jpg")
	dataB, err := os.ReadFile(receivedB)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", receivedB, err)
	}
	if !bytes.Equal(dataB, contentB) {
		t.Fatalf("data mismatch in Bob's file")
	}

	// Ensure Alice's file was unchanged
	dataA2, err := os.ReadFile(receivedA)
	if err != nil || !bytes.Equal(dataA2, contentA) {
		t.Fatalf("Alice's file was altered or overwritten!")
	}
}

func TestUnapprovedGuestCannotUpload(t *testing.T) {
	tmpDir := t.TempDir()
	room, err := NewRoom(tmpDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}

	guest, err := room.Join("Mallory")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	// Try declaring upload while still pending approval
	_, err = room.DeclareUpload(guest.ID, "malicious.sh", 100)
	if err != ErrGuestNotApproved {
		t.Fatalf("expected ErrGuestNotApproved, got %v", err)
	}

	// Reject guest
	_ = room.RejectGuest(guest.ID)
	_, err = room.DeclareUpload(guest.ID, "malicious.sh", 100)
	if err != ErrGuestNotApproved {
		t.Fatalf("expected ErrGuestNotApproved after rejection, got %v", err)
	}
}

func TestCollisionPolicyReject(t *testing.T) {
	tmpDir := t.TempDir()
	room, err := NewRoomWithConfig(RoomConfig{
		DestinationRoot: tmpDir,
		Duration:        1 * time.Hour,
		CollisionPolicy: CollisionReject,
	})
	if err != nil {
		t.Fatalf("NewRoomWithConfig: %v", err)
	}

	g, err := room.Join("Alice")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	_ = room.ApproveGuest(g.ID)

	content := []byte("first upload")
	up1, err := room.DeclareUpload(g.ID, "essay.pdf", int64(len(content)))
	if err != nil {
		t.Fatalf("DeclareUpload 1: %v", err)
	}
	_ = room.ApproveUpload(up1.ID)
	_, err = room.StreamUpload(up1.ID, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("StreamUpload: %v", err)
	}

	// Now try declaring another file with the exact same name under CollisionReject
	_, err = room.DeclareUpload(g.ID, "essay.pdf", int64(len(content)))
	if !errors.Is(err, ErrFileConflict) {
		t.Fatalf("expected ErrFileConflict, got %v", err)
	}
}

func TestFormValidation(t *testing.T) {
	tmpDir := t.TempDir()
	room, err := NewRoomWithConfig(RoomConfig{
		DestinationRoot: tmpDir,
		Duration:        1 * time.Hour,
		FormSchema: []FormField{
			{ID: "student_id", Label: "Student ID", Type: "text", Required: true, MaxLength: 10},
			{ID: "category", Label: "Category", Type: "select", Required: false, Options: []string{"Essay", "Code", "Slides"}},
		},
	})
	if err != nil {
		t.Fatalf("NewRoomWithConfig: %v", err)
	}

	g, _ := room.Join("Alice")
	_ = room.ApproveGuest(g.ID)

	// 1. Missing required field -> error
	_, err = room.DeclareUploadWithMeta(g.ID, "doc.pdf", 10, map[string]string{
		"student_id": "",
	})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required field error, got %v", err)
	}

	// 2. Exceeding max length -> error
	_, err = room.DeclareUploadWithMeta(g.ID, "doc.pdf", 10, map[string]string{
		"student_id": "123456789012345",
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds max length") {
		t.Fatalf("expected max length error, got %v", err)
	}

	// 3. Invalid select option -> error
	_, err = room.DeclareUploadWithMeta(g.ID, "doc.pdf", 10, map[string]string{
		"student_id": "ST123",
		"category":   "MaliciousOption",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid selection") {
		t.Fatalf("expected invalid selection error, got %v", err)
	}

	// 4. Valid values -> success
	up, err := room.DeclareUploadWithMeta(g.ID, "doc.pdf", 10, map[string]string{
		"student_id": "ST123",
		"category":   "Essay",
	})
	if err != nil {
		t.Fatalf("valid form declare failed: %v", err)
	}
	if up.FormValues["student_id"] != "ST123" {
		t.Fatalf("form values not preserved: %+v", up.FormValues)
	}
}

func TestRosterTrackingAndDeadline(t *testing.T) {
	tmpDir := t.TempDir()
	pastDeadline := time.Now().Add(-1 * time.Minute)
	room, err := NewRoomWithConfig(RoomConfig{
		DestinationRoot: tmpDir,
		Duration:        1 * time.Hour,
		Deadline:        &pastDeadline,
		ExpectedParticipants: []ExpectedParticipant{
			{Name: "Charlie Brown", Identifier: "cb_01"},
			{Name: "Diana Prince", Identifier: "dp_02"},
		},
	})
	if err != nil {
		t.Fatalf("NewRoomWithConfig: %v", err)
	}

	roster := room.GetRoster()
	if len(roster) != 2 {
		t.Fatalf("expected 2 participants, got %d", len(roster))
	}

	// CheckExpiry updates status to missing_at_deadline if past deadline and unsubmitted
	room.CheckExpiry()
	rosterAfter := room.GetRoster()
	for _, p := range rosterAfter {
		if p.Status != StatusMissingAtDeadline {
			t.Fatalf("expected StatusMissingAtDeadline, got %s for %s", p.Status, p.Name)
		}
	}
}
