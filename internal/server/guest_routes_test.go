package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func setupTestGuestServer(t *testing.T) (*httptest.Server, *guest.Manager, *guest.Room, string) {
	t.Helper()
	tmpDir := t.TempDir()

	guestMgr := guest.NewManager()
	t.Cleanup(guestMgr.Close)

	room, err := guestMgr.CreateRoom(tmpDir, 1*time.Hour)
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	downloads := transfer.NewDownloadRegistry()

	services := &Services{
		Sessions:     sessMgr,
		Transfers:    transfers,
		Coordinator:  coord,
		Registry:     reg,
		Downloads:    downloads,
		GuestManager: guestMgr,
	}

	grants := newGrantStore(downloads)
	handler := newRouter(services, 5*time.Second, 5*time.Second, grants)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts, guestMgr, room, tmpDir
}

func TestGuestRoutesFullFlow(t *testing.T) {
	ts, _, room, destDir := setupTestGuestServer(t)

	// 1. GET /guest serves HTML
	resp, err := http.Get(ts.URL + "/guest")
	if err != nil {
		t.Fatalf("GET /guest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /guest status = %d; want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("Flitnix Guest Drop Box")) {
		t.Fatalf("GET /guest body missing title: %s", string(body))
	}

	// 2. Join with invalid secret fails
	joinBad, _ := json.Marshal(map[string]string{
		"secret":   "invalid-secret",
		"nickname": "Alice",
	})
	respBad, err := http.Post(ts.URL+"/guest-api/join", "application/json", bytes.NewReader(joinBad))
	if err != nil {
		t.Fatalf("POST /guest-api/join: %v", err)
	}
	respBad.Body.Close()
	if respBad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("join with bad secret status = %d; want 401", respBad.StatusCode)
	}

	// 3. Join with valid secret succeeds
	joinGood, _ := json.Marshal(map[string]string{
		"secret":   room.InviteSecret,
		"nickname": "Alice Phone",
	})
	respGood, err := http.Post(ts.URL+"/guest-api/join", "application/json", bytes.NewReader(joinGood))
	if err != nil {
		t.Fatalf("POST /guest-api/join: %v", err)
	}
	defer respGood.Body.Close()
	if respGood.StatusCode != http.StatusCreated {
		t.Fatalf("join status = %d; want 201", respGood.StatusCode)
	}

	// Verify cookie was set
	var guestToken string
	for _, c := range respGood.Cookies() {
		if c.Name == guestCookieName {
			guestToken = c.Value
			break
		}
	}
	if guestToken == "" {
		t.Fatal("expected flitnix_guest_token cookie to be set")
	}

	var joinRes struct {
		GuestID  string `json:"guestId"`
		Token    string `json:"token"`
		Nickname string `json:"nickname"`
		State    string `json:"state"`
	}
	_ = json.NewDecoder(respGood.Body).Decode(&joinRes)
	if joinRes.State != "pending" || joinRes.Nickname != "Alice Phone" {
		t.Fatalf("unexpected join response: %+v", joinRes)
	}

	// 4. Poll status while pending
	reqStatus, _ := http.NewRequest("GET", ts.URL+"/guest-api/status", nil)
	reqStatus.Header.Set("Authorization", "Bearer "+guestToken)
	respStatus, err := http.DefaultClient.Do(reqStatus)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer respStatus.Body.Close()
	var statusRes struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(respStatus.Body).Decode(&statusRes)
	if statusRes.State != "pending" {
		t.Fatalf("status = %s; want pending", statusRes.State)
	}

	// 5. Try declaring upload before approval -> 403 Forbidden
	fileContent := []byte("hello flitnix guest drop!")
	uploadMeta, _ := json.Marshal(map[string]any{
		"name": "photo.jpg",
		"size": len(fileContent),
	})
	reqUpEarly, _ := http.NewRequest("POST", ts.URL+"/guest-api/uploads", bytes.NewReader(uploadMeta))
	reqUpEarly.Header.Set("Authorization", "Bearer "+guestToken)
	reqUpEarly.Header.Set("Content-Type", "application/json")
	respUpEarly, err := http.DefaultClient.Do(reqUpEarly)
	if err != nil {
		t.Fatalf("POST uploads early: %v", err)
	}
	respUpEarly.Body.Close()
	if respUpEarly.StatusCode != http.StatusForbidden {
		t.Fatalf("upload while unapproved status = %d; want 403", respUpEarly.StatusCode)
	}

	// 6. Host approves guest
	if err := room.ApproveGuest(joinRes.GuestID); err != nil {
		t.Fatalf("ApproveGuest: %v", err)
	}

	// 7. Poll status -> approved
	respStatus2, err := http.DefaultClient.Do(reqStatus)
	if err != nil {
		t.Fatalf("GET status 2: %v", err)
	}
	defer respStatus2.Body.Close()
	_ = json.NewDecoder(respStatus2.Body).Decode(&statusRes)
	if statusRes.State != "approved" {
		t.Fatalf("status = %s; want approved", statusRes.State)
	}

	// 8. Declare upload
	reqUp, _ := http.NewRequest("POST", ts.URL+"/guest-api/uploads", bytes.NewReader(uploadMeta))
	reqUp.Header.Set("Authorization", "Bearer "+guestToken)
	reqUp.Header.Set("Content-Type", "application/json")
	respUp, err := http.DefaultClient.Do(reqUp)
	if err != nil {
		t.Fatalf("POST uploads: %v", err)
	}
	defer respUp.Body.Close()
	if respUp.StatusCode != http.StatusCreated {
		t.Fatalf("POST uploads status = %d; want 201", respUp.StatusCode)
	}

	var upRes struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	_ = json.NewDecoder(respUp.Body).Decode(&upRes)
	if upRes.State != "pending" {
		t.Fatalf("upload state = %s; want pending", upRes.State)
	}

	// 9. Host approves upload
	if err := room.ApproveUpload(upRes.ID); err != nil {
		t.Fatalf("ApproveUpload: %v", err)
	}

	// 10. Stream content
	reqContent, _ := http.NewRequest("PUT", ts.URL+"/guest-api/uploads/"+upRes.ID+"/content", bytes.NewReader(fileContent))
	reqContent.Header.Set("Authorization", "Bearer "+guestToken)
	respContent, err := http.DefaultClient.Do(reqContent)
	if err != nil {
		t.Fatalf("PUT content: %v", err)
	}
	defer respContent.Body.Close()
	if respContent.StatusCode != http.StatusOK {
		t.Fatalf("PUT content status = %d; want 200", respContent.StatusCode)
	}

	// 11. Verify file content on disk in isolated guest folder
	guestFolder := filepath.Join(room.RoomFolder, joinRes.GuestID+"-alice-phone")
	filePath := filepath.Join(guestFolder, "photo.jpg")
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v (destDir=%s)", filePath, err, destDir)
	}
	if !bytes.Equal(data, fileContent) {
		t.Fatalf("data mismatch: %q != %q", string(data), string(fileContent))
	}

	// 12. Upload-only boundary: Guest token cannot access personal session endpoints
	reqPersonal, _ := http.NewRequest("GET", ts.URL+"/api/files", nil)
	reqPersonal.Header.Set("Authorization", "Bearer "+guestToken)
	respPersonal, err := http.DefaultClient.Do(reqPersonal)
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	respPersonal.Body.Close()
	if respPersonal.StatusCode != http.StatusUnauthorized {
		t.Fatalf("guest accessing /api/files status = %d; want 401", respPersonal.StatusCode)
	}
}

func TestGuestCrossGuestIsolation(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	// Join Guest A
	gA, _ := room.Join("Alice")
	_ = room.ApproveGuest(gA.ID)

	// Join Guest B
	gB, _ := room.Join("Bob")
	_ = room.ApproveGuest(gB.ID)

	// Guest A declares upload
	upA, err := room.DeclareUpload(gA.ID, "alice-secret.pdf", 100)
	if err != nil {
		t.Fatalf("DeclareUpload A: %v", err)
	}

	// Guest B tries to poll Guest A's upload -> 404 Not Found!
	reqPoll, _ := http.NewRequest("GET", ts.URL+"/guest-api/uploads/"+upA.ID, nil)
	reqPoll.Header.Set("Authorization", "Bearer "+gB.Token)
	respPoll, err := http.DefaultClient.Do(reqPoll)
	if err != nil {
		t.Fatalf("GET upload status: %v", err)
	}
	respPoll.Body.Close()
	if respPoll.StatusCode != http.StatusNotFound {
		t.Fatalf("Guest B probing Guest A upload status = %d; want 404", respPoll.StatusCode)
	}

	// Guest B tries to stream content into Guest A's upload -> 404 Not Found!
	reqStream, _ := http.NewRequest("PUT", ts.URL+"/guest-api/uploads/"+upA.ID+"/content", bytes.NewReader([]byte("hack")))
	reqStream.Header.Set("Authorization", "Bearer "+gB.Token)
	respStream, err := http.DefaultClient.Do(reqStream)
	if err != nil {
		t.Fatalf("PUT content: %v", err)
	}
	respStream.Body.Close()
	if respStream.StatusCode != http.StatusNotFound {
		t.Fatalf("Guest B streaming into Guest A upload status = %d; want 404", respStream.StatusCode)
	}
}

func TestGuestRevocationBlocksOperations(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	g, _ := room.Join("Mallory")
	_ = room.ApproveGuest(g.ID)

	// Revoke Mallory
	_ = room.RevokeGuest(g.ID)

	// Mallory tries to declare upload
	uploadMeta, _ := json.Marshal(map[string]any{"name": "file.txt", "size": 10})
	req, _ := http.NewRequest("POST", ts.URL+"/guest-api/uploads", bytes.NewReader(uploadMeta))
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST uploads: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked guest declare status = %d; want 401", resp.StatusCode)
	}
}

func TestDNSRebindingProtection(t *testing.T) {
	ts, _, _, _ := setupTestGuestServer(t)

	req, err := http.NewRequest("GET", ts.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.attacker.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for rebinding Host header, got %d", resp.StatusCode)
	}
}

func TestCrossSiteCSRFProtection(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	// Test Sec-Fetch-Site: cross-site
	joinData, _ := json.Marshal(map[string]string{
		"secret":   room.InviteSecret,
		"nickname": "CSRF Victim",
	})
	req, err := http.NewRequest("POST", ts.URL+"/guest-api/join", bytes.NewReader(joinData))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for cross-site Sec-Fetch-Site, got %d", resp.StatusCode)
	}

	// Test untrusted Origin
	req2, err := http.NewRequest("POST", ts.URL+"/guest-api/join", bytes.NewReader(joinData))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Origin", "https://attacker.com")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for untrusted Origin, got %d", resp2.StatusCode)
	}
}

func TestZeroByteUpload(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	g, err := room.Join("ZeroByteUser")
	if err != nil {
		t.Fatal(err)
	}
	if err := room.ApproveGuest(g.ID); err != nil {
		t.Fatal(err)
	}

	up, err := room.DeclareUpload(g.ID, "empty.txt", 0)
	if err != nil {
		t.Fatalf("failed to declare 0-byte upload: %v", err)
	}
	if err := room.ApproveUpload(up.ID); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("PUT", ts.URL+"/guest-api/uploads/"+up.ID+"/content", bytes.NewReader([]byte{}))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for 0-byte upload stream, got %d", resp.StatusCode)
	}
}

func TestUnicodeNicknameSanitization(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	// Nickname containing multi-byte UTF-8 emojis and non-Latin runes exceeding 30 runes
	multiByteNick := "🚀✨🎉🔥🌟日本語テストユーザー名長いテキスト超過分確認"
	joinData, _ := json.Marshal(map[string]string{
		"secret":   room.InviteSecret,
		"nickname": multiByteNick,
	})
	req, err := http.NewRequest("POST", ts.URL+"/guest-api/join", bytes.NewReader(joinData))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("join status = %d; want 201", resp.StatusCode)
	}

	var res struct {
		GuestID  string `json:"guestId"`
		Nickname string `json:"nickname"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if len([]rune(res.Nickname)) > 30 {
		t.Fatalf("expected max 30 runes, got %d runes: %q", len([]rune(res.Nickname)), res.Nickname)
	}
	// Verify valid UTF-8
	if !bytes.Equal([]byte(res.Nickname), []byte(string([]rune(res.Nickname)))) {
		t.Fatalf("nickname is not valid UTF-8: %q", res.Nickname)
	}
}

func TestGuestRoomCapacityLimit(t *testing.T) {
	ts, _, room, _ := setupTestGuestServer(t)

	for i := 0; i < guest.MaxGuestsPerRoom; i++ {
		_, err := room.Join("Guest")
		if err != nil {
			t.Fatalf("Join guest %d failed: %v", i, err)
		}
	}

	// 257th guest should hit capacity limit -> 429 Too Many Requests
	joinData, _ := json.Marshal(map[string]string{
		"secret":   room.InviteSecret,
		"nickname": "OverflowGuest",
	})
	req, _ := http.NewRequest("POST", ts.URL+"/guest-api/join", bytes.NewReader(joinData))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests on full room, got %d", resp.StatusCode)
	}
}
