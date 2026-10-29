package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/audit"
	"github.com/itsyamin/flitnix/internal/doctor"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/netenv"
)

type mockAppNetSource struct {
	ifaces []net.Interface
	addrs  map[string][]net.Addr
}

func (m *mockAppNetSource) InterfacesFor() ([]net.Interface, error) {
	return m.ifaces, nil
}

func (m *mockAppNetSource) AddressesFor(iface net.Interface) ([]net.Addr, error) {
	return m.addrs[iface.Name], nil
}

func (m *mockAppNetSource) MetadataFor(iface net.Interface) netenv.Metadata {
	return netenv.Metadata{Type: "Wi-Fi", SSID: "OfficeGuest"}
}

func TestAppGuestDropBoxWorkflow(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	defer app.shutdown(t.Context())

	// Configure mock network source
	iface := net.Interface{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast}
	_, ipnet, _ := net.ParseCIDR("192.168.1.50/24")
	app.source = &mockAppNetSource{
		ifaces: []net.Interface{iface},
		addrs:  map[string][]net.Addr{"wlo1": {ipnet}},
	}

	tmpDir := t.TempDir()

	listen := "127.0.0.77:43821"
	if err := app.ensureServer(listen); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}

	// 1. Start guest room
	roomView, err := app.StartGuestRoom(tmpDir, 60)
	if err != nil {
		t.Fatalf("StartGuestRoom: %v", err)
	}
	if roomView.State != "active" || roomView.InviteSecret == "" {
		t.Fatalf("unexpected room view: %+v", roomView)
	}

	// 2. Query initial room state
	state, err := app.GetGuestRoomState()
	if err != nil {
		t.Fatalf("GetGuestRoomState: %v", err)
	}
	if state.Room == nil || state.Room.ID != roomView.ID {
		t.Fatalf("state room mismatch")
	}

	// 3. Guest joins
	activeRoom := app.guestMgr.ActiveRoom()
	guest, err := activeRoom.Join("Dan")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	state, _ = app.GetGuestRoomState()
	if len(state.PendingGuests) != 1 || state.PendingGuests[0].ID != guest.ID {
		t.Fatalf("expected 1 pending guest, got %+v", state.PendingGuests)
	}

	// 4. Host approves guest
	if err := app.ApproveGuest(guest.ID); err != nil {
		t.Fatalf("ApproveGuest: %v", err)
	}

	state, _ = app.GetGuestRoomState()
	if len(state.ApprovedGuests) != 1 || state.ApprovedGuests[0].ID != guest.ID {
		t.Fatalf("expected 1 approved guest")
	}

	// 5. Guest declares upload
	upReq, err := activeRoom.DeclareUpload(guest.ID, "report.pdf", 500)
	if err != nil {
		t.Fatalf("DeclareUpload: %v", err)
	}

	state, _ = app.GetGuestRoomState()
	if len(state.PendingUploads) != 1 || state.PendingUploads[0].ID != upReq.ID {
		t.Fatalf("expected 1 pending upload, got %+v", state.PendingUploads)
	}

	// 6. Host approves upload
	if err := app.ApproveGuestUpload(upReq.ID); err != nil {
		t.Fatalf("ApproveGuestUpload: %v", err)
	}

	// 7. Revoke guest
	if err := app.RevokeGuest(guest.ID); err != nil {
		t.Fatalf("RevokeGuest: %v", err)
	}

	// 8. End room
	if err := app.EndGuestRoom(); err != nil {
		t.Fatalf("EndGuestRoom: %v", err)
	}

	state, _ = app.GetGuestRoomState()
	if state.Room.State != "ended" {
		t.Fatalf("expected room state ended, got %s", state.Room.State)
	}
}

func TestAppConnectionDoctor(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	defer app.shutdown(t.Context())

	report := app.RunConnectionDiagnostics()
	if len(report.Items) == 0 {
		t.Fatal("expected diagnostic report items")
	}
	if report.OverallStatus != doctor.StatusPass && report.OverallStatus != doctor.StatusWarn && report.OverallStatus != doctor.StatusFail {
		t.Fatalf("invalid report status: %s", report.OverallStatus)
	}
}

func TestAppQuickTextTransfer(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	defer app.shutdown(t.Context())

	// Need active session for text messages
	sess, err := app.sessions.Create()
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// 1. Send desktop message
	msg1, err := app.SendTextMessage(sess.Key(), "Hello from desktop!")
	if err != nil {
		t.Fatalf("SendTextMessage: %v", err)
	}
	if msg1.Sender != "desktop" || msg1.Content != "Hello from desktop!" {
		t.Fatalf("unexpected message: %+v", msg1)
	}

	// 2. Send link
	msg2, err := app.SendTextMessage(sess.Key(), "https://flitnix.org")
	if err != nil {
		t.Fatalf("Send link: %v", err)
	}
	if !msg2.IsLink || msg2.URL != "https://flitnix.org" {
		t.Fatalf("expected link detection: %+v", msg2)
	}

	// 3. List messages
	msgs := app.ListTextMessages(sess.Key())
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// 4. Clear messages
	if err := app.ClearTextMessages(sess.Key()); err != nil {
		t.Fatalf("ClearTextMessages: %v", err)
	}
	if len(app.ListTextMessages(sess.Key())) != 0 {
		t.Fatal("expected empty messages after clear")
	}
}

func TestAppEventCollectionPlatform(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	defer app.shutdown(t.Context())

	iface := net.Interface{Index: 1, Name: "wlo1", Flags: net.FlagUp | net.FlagBroadcast}
	_, ipnet, _ := net.ParseCIDR("192.168.1.50/24")
	app.source = &mockAppNetSource{
		ifaces: []net.Interface{iface},
		addrs:  map[string][]net.Addr{"wlo1": {ipnet}},
	}

	tmpDir := t.TempDir()
	listen := "127.0.0.78:43821"
	if err := app.ensureServer(listen); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}

	// 1. Presets validation
	presets := app.ListGuestPresets()
	if len(presets) < 5 {
		t.Fatalf("expected at least 5 default presets, got %d", len(presets))
	}

	// 2. Templates CRUD
	tpl := guest.Template{
		ID:              "tpl-midterm",
		Name:            "Midterm Exam Template",
		Title:           "Midterm Code Drop",
		DurationMinutes: 90,
		CollisionPolicy: guest.CollisionReject,
		FormSchema: []guest.FormField{
			{ID: "student_id", Label: "Student ID", Type: "text", Required: true},
		},
	}
	if err := app.SaveGuestTemplate(tpl); err != nil {
		t.Fatalf("SaveGuestTemplate: %v", err)
	}
	tpls := app.ListGuestTemplates()
	found := false
	for _, item := range tpls {
		if item.ID == "tpl-midterm" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("saved template not found in listing")
	}

	dup, err := app.DuplicateGuestTemplate("tpl-midterm", "Midterm Exam (Copy)")
	if err != nil || dup.Name != "Midterm Exam (Copy)" {
		t.Fatalf("DuplicateGuestTemplate failed: %v", err)
	}

	// 3. Start room with config
	roomView, err := app.StartGuestRoomWithConfig(GuestRoomConfigView{
		DestinationDir:  filepath.Join(tmpDir, "submissions"),
		DurationMinutes: 60,
		Title:           "Midterm Submissions",
		CollisionPolicy: "reject",
		FormSchema:      tpl.FormSchema,
	})
	if err != nil {
		t.Fatalf("StartGuestRoomWithConfig: %v", err)
	}
	if roomView.CollisionPolicy != "reject" || roomView.Title != "Midterm Submissions" {
		t.Fatalf("unexpected room view: %+v", roomView)
	}

	// 4. Expected participants roster
	if err := app.AddExpectedParticipant("Alice Smith", "asmith@example.com", "CS-101"); err != nil {
		t.Fatalf("AddExpectedParticipant: %v", err)
	}

	state, err := app.GetGuestRoomState()
	if err != nil {
		t.Fatalf("GetGuestRoomState: %v", err)
	}
	if len(state.Roster) != 1 || state.Roster[0].Name != "Alice Smith" {
		t.Fatalf("expected roster item for Alice Smith, got %+v", state.Roster)
	}
	aliceParticipantID := state.Roster[0].ID

	// 5. Alice joins using participant ID
	activeRoom := app.guestMgr.ActiveRoom()
	g, err := activeRoom.JoinWithParticipant("Alice", aliceParticipantID)
	if err != nil {
		t.Fatalf("JoinWithParticipant: %v", err)
	}
	if err := app.ApproveGuest(g.ID); err != nil {
		t.Fatalf("ApproveGuest: %v", err)
	}

	// 6. Upload file with form values
	content := []byte("function solve() { return 42; }\n")
	up, err := activeRoom.DeclareUploadWithMeta(g.ID, "solution.js", int64(len(content)), map[string]string{
		"student_id": "CS-101",
	})
	if err != nil {
		t.Fatalf("DeclareUploadWithMeta: %v", err)
	}
	if err := app.ApproveGuestUpload(up.ID); err != nil {
		t.Fatalf("ApproveGuestUpload: %v", err)
	}

	if _, err := activeRoom.StreamUpload(up.ID, bytes.NewReader(content)); err != nil {
		t.Fatalf("StreamUpload: %v", err)
	}

	state, _ = app.GetGuestRoomState()
	if len(state.CompletedUploads) != 1 {
		t.Fatalf("expected 1 completed upload, got %d", len(state.CompletedUploads))
	}
	// Alice status should now be submitted
	for _, p := range state.Roster {
		if p.ID == aliceParticipantID && p.Status != guest.StatusSubmitted {
			t.Fatalf("expected Alice status to be submitted, got %s", p.Status)
		}
	}

	// 7. Verify collision reject policy: uploading another file named "solution.js" must fail
	_, err = activeRoom.DeclareUploadWithMeta(g.ID, "solution.js", int64(len(content)), map[string]string{
		"student_id": "CS-101",
	})
	if !errors.Is(err, guest.ErrFileConflict) {
		t.Fatalf("expected ErrFileConflict for duplicate file under CollisionReject, got %v", err)
	}

	// 8. Export collection to ZIP
	zipPath := filepath.Join(tmpDir, "final-export.zip")
	exportRes, err := app.ExportGuestCollection(zipPath)
	if err != nil {
		t.Fatalf("ExportGuestCollection: %v", err)
	}
	if exportRes.TotalFiles != 1 || exportRes.TotalBytes != int64(len(content)) {
		t.Fatalf("unexpected export result: %+v", exportRes)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("exported zip file does not exist: %v", err)
	}

	// 9. Query audit history
	auditRes, err := app.ListAuditEvents(audit.Filter{RoomID: roomView.ID})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if auditRes.Total < 3 {
		t.Fatalf("expected at least 3 audit entries for room, got %d", auditRes.Total)
	}

	// 10. End room
	if err := app.EndGuestRoom(); err != nil {
		t.Fatalf("EndGuestRoom: %v", err)
	}
}

func TestAddDroppedSourcesValidation(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	defer app.shutdown(t.Context())

	tmpDir := t.TempDir()

	// 1. Valid file with Unicode and spaces
	validFile := filepath.Join(tmpDir, "日本語 ドキュメント.pdf")
	if err := os.WriteFile(validFile, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 2. Symlink to valid file
	symlinkFile := filepath.Join(tmpDir, "link-to-doc.pdf")
	if err := os.Symlink(validFile, symlinkFile); err != nil {
		t.Fatal(err)
	}

	// 3. Nonexistent file
	nonexistentFile := filepath.Join(tmpDir, "does-not-exist.bin")

	// Call AddDroppedSources with all three
	views, err := app.AddDroppedSources([]string{validFile, symlinkFile, nonexistentFile})
	if err == nil {
		t.Fatal("expected AddDroppedSources to report error for invalid sources")
	}

	// Valid file must be registered successfully despite failures of other inputs
	if len(views) != 1 {
		t.Fatalf("expected 1 valid view registered, got %d", len(views))
	}
	if views[0].Name != "日本語 ドキュメント.pdf" {
		t.Fatalf("expected valid source name %q, got %q", "日本語 ドキュメント.pdf", views[0].Name)
	}
}

func TestExportDuplicateNameDeduplication(t *testing.T) {
	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	defer app.shutdown(t.Context())

	tmpDir := t.TempDir()
	roomFolder := filepath.Join(tmpDir, "export-room")
	_ = os.MkdirAll(roomFolder, 0o755)

	_, err = app.StartGuestRoomWithConfig(GuestRoomConfigView{
		Title:           "Duplicate Test Room",
		DestinationDir:  roomFolder,
		DurationMinutes: 30,
		CollisionPolicy: "preserve_both",
	})
	if err != nil {
		t.Fatalf("StartGuestRoomWithConfig: %v", err)
	}

	activeRoom := app.guestMgr.ActiveRoom()
	if activeRoom == nil {
		t.Fatal("active room is nil")
	}

	g, err := activeRoom.Join("Tester")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	_ = activeRoom.ApproveGuest(g.ID)

	// Declare and upload file 1: "report.pdf"
	content1 := []byte("first submission content")
	up1, err := activeRoom.DeclareUpload(g.ID, "report.pdf", int64(len(content1)))
	if err != nil {
		t.Fatalf("DeclareUpload 1: %v", err)
	}
	_ = activeRoom.ApproveUpload(up1.ID)
	if _, err := activeRoom.StreamUpload(up1.ID, bytes.NewReader(content1)); err != nil {
		t.Fatalf("StreamUpload 1: %v", err)
	}

	// Declare and upload file 2: identical filename "report.pdf" under preserve_both
	content2 := []byte("second submission content revised")
	up2, err := activeRoom.DeclareUpload(g.ID, "report.pdf", int64(len(content2)))
	if err != nil {
		t.Fatalf("DeclareUpload 2: %v", err)
	}
	_ = activeRoom.ApproveUpload(up2.ID)
	if _, err := activeRoom.StreamUpload(up2.ID, bytes.NewReader(content2)); err != nil {
		t.Fatalf("StreamUpload 2: %v", err)
	}

	// Export to zip
	zipPath := filepath.Join(tmpDir, "dedup-test.zip")
	res, err := guest.ExportRoomToZip(activeRoom, zipPath)
	if err != nil {
		t.Fatalf("ExportRoomToZip failed: %v", err)
	}
	if res.TotalFiles != 2 {
		t.Fatalf("expected 2 exported files, got %d", res.TotalFiles)
	}

	// Read and verify zip entries
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer zr.Close()

	entryNames := make(map[string]bool)
	for _, f := range zr.File {
		if f.Name != "manifest.json" && f.Name != "manifest.csv" {
			if entryNames[f.Name] {
				t.Fatalf("duplicate entry path in zip archive: %q", f.Name)
			}
			entryNames[f.Name] = true
		}
	}
	if len(entryNames) != 2 {
		t.Fatalf("expected 2 distinct file entries in zip, got %d (%v)", len(entryNames), entryNames)
	}
}
