package main

import (
	"net"
	"testing"

	"github.com/itsyamin/flitnix/internal/doctor"
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
