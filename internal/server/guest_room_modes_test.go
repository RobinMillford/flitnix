package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func setupTestGuestModeServer(t *testing.T, cfg guest.RoomConfig) (*httptest.Server, *guest.Manager, *guest.Room, *transfer.Registry) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg.DestinationRoot = tmpDir
	if cfg.Duration == 0 {
		cfg.Duration = time.Hour
	}

	guestMgr := guest.NewManager()
	t.Cleanup(guestMgr.Close)

	room, err := guestMgr.CreateRoomWithConfig(cfg)
	if err != nil {
		t.Fatalf("CreateRoomWithConfig: %v", err)
	}

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	personalReg := transfer.NewRegistry()
	// Add a strictly personal host file that MUST NOT leak to guest rooms.
	personalSecret := filepath.Join(tmpDir, "personal-only-secret.txt")
	if err := os.WriteFile(personalSecret, []byte("top-secret-personal"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := personalReg.AddFileSource(personalSecret, "personal-only-secret.txt", ""); err != nil {
		t.Fatal(err)
	}

	downloads := transfer.NewDownloadRegistry()
	services := &Services{
		Sessions:     session.NewManager(session.Config{}),
		Transfers:    transfer.NewService(transfer.Config{Store: store}),
		Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:     personalReg,
		Downloads:    downloads,
		GuestManager: guestMgr,
	}

	grants := newGrantStore(downloads)
	handler := newRouter(services, 5*time.Second, 5*time.Second, grants)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, guestMgr, room, personalReg
}

func joinTestGuest(t *testing.T, baseURL, secret, nickname string) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	cli := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"secret": secret, "nickname": nickname})
	resp, err := cli.Post(baseURL+"/guest-api/join", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("join %s: %v", nickname, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("join %s status = %d: %s", nickname, resp.StatusCode, string(raw))
	}
	var res guestJoinResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	return cli, res.GuestID
}

func TestGuestModeUploadOnlyBlocksDownloadRoutes(t *testing.T) {
	ts, _, room, _ := setupTestGuestModeServer(t, guest.RoomConfig{
		Mode: guest.RoomModeUploadOnly,
	})
	cli, guestID := joinTestGuest(t, ts.URL, room.InviteSecret, "Guest1")
	if err := room.ApproveGuest(guestID); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/guest-api/files", "/guest-api/sources"} {
		resp, err := cli.Get(ts.URL + route)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s in upload_only = %d; want 403", route, resp.StatusCode)
		}
	}
}

func TestGuestModeDownloadOnlyBroadcastAndIsolation(t *testing.T) {
	shareDir := t.TempDir()
	handout := filepath.Join(shareDir, "lecture-notes.bin")
	payload := bytes.Repeat([]byte("FlitnixRoomPayload-"), 12000)
	if err := os.WriteFile(handout, payload, 0644); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(shareDir, "slides")
	if err := os.MkdirAll(filepath.Join(folder, "week1"), 0755); err != nil {
		t.Fatal(err)
	}
	slideBytes := []byte("slide-deck-content-v1")
	if err := os.WriteFile(filepath.Join(folder, "week1", "deck.txt"), slideBytes, 0644); err != nil {
		t.Fatal(err)
	}

	ts, _, room, _ := setupTestGuestModeServer(t, guest.RoomConfig{
		Mode:        guest.RoomModeDownloadOnly,
		SharedPaths: []string{handout, folder},
	})

	// Unapproved guest is denied access to host shared files.
	cliAlice, aliceID := joinTestGuest(t, ts.URL, room.InviteSecret, "Alice")
	unauthResp, err := cliAlice.Get(ts.URL + "/guest-api/sources")
	if err != nil {
		t.Fatal(err)
	}
	unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusForbidden {
		t.Fatalf("pending guest GET /guest-api/sources = %d; want 403", unauthResp.StatusCode)
	}

	// Host approves Alice.
	if err := room.ApproveGuest(aliceID); err != nil {
		t.Fatal(err)
	}

	// Upload attempts in download_only mode must return 403.
	upBody, _ := json.Marshal(map[string]any{"name": "rogue.txt", "size": 5})
	upResp, err := cliAlice.Post(ts.URL+"/guest-api/uploads", "application/json", bytes.NewReader(upBody))
	if err != nil {
		t.Fatal(err)
	}
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /guest-api/uploads in download_only = %d; want 403", upResp.StatusCode)
	}

	// Shared sources list contains handout & folder ONLY (never personal-only-secret.txt).
	srcResp, err := cliAlice.Get(ts.URL + "/guest-api/sources")
	if err != nil {
		t.Fatal(err)
	}
	defer srcResp.Body.Close()
	if srcResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /guest-api/sources = %d; want 200", srcResp.StatusCode)
	}
	var sources []phoneSourceJSON
	if err := json.NewDecoder(srcResp.Body).Decode(&sources); err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("len(sources) = %d; want 2 (%+v)", len(sources), sources)
	}
	var handoutSrcID, folderSrcID string
	for _, s := range sources {
		if s.Name == "personal-only-secret.txt" {
			t.Fatal("personal registry item leaked to guest room")
		}
		if s.Name == "lecture-notes.bin" {
			handoutSrcID = s.ID
		} else if s.Name == "slides" {
			folderSrcID = s.ID
		}
	}
	if handoutSrcID == "" || folderSrcID == "" {
		t.Fatalf("missing expected sources in %+v", sources)
	}

	// Single-file download grant + stream + SHA-256 verification.
	grantReq, _ := json.Marshal(createGrantRequest{SourceIDs: []string{handoutSrcID}})
	gResp, err := cliAlice.Post(ts.URL+"/guest-api/download-grants", "application/json", bytes.NewReader(grantReq))
	if err != nil {
		t.Fatal(err)
	}
	defer gResp.Body.Close()
	if gResp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(gResp.Body)
		t.Fatalf("create guest download grant = %d: %s", gResp.StatusCode, string(raw))
	}
	var grantRes createGrantResponse
	if err := json.NewDecoder(gResp.Body).Decode(&grantRes); err != nil {
		t.Fatal(err)
	}
	dlResp, err := cliAlice.Get(ts.URL + grantRes.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, err := io.ReadAll(dlResp.Body)
	dlResp.Body.Close()
	if err != nil || dlResp.StatusCode != http.StatusOK {
		t.Fatalf("download handout status=%d err=%v", dlResp.StatusCode, err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Fatalf("downloaded payload mismatch: got %d bytes, want %d", len(gotBytes), len(payload))
	}
	wantSum := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(wantSum[:])

	// Progress should report completed + SHA-256.
	progResp, err := cliAlice.Get(ts.URL + "/guest-api/download-progress/" + grantRes.ProgressID)
	if err != nil {
		t.Fatal(err)
	}
	var prog map[string]any
	_ = json.NewDecoder(progResp.Body).Decode(&prog)
	progResp.Body.Close()
	if prog["state"] != progressCompleted || prog["sha256"] != wantHex {
		t.Fatalf("unexpected progress %+v (want sha256 %s)", prog, wantHex)
	}

	// Folder browse + ZIP archive download.
	itemsResp, err := cliAlice.Get(ts.URL + "/guest-api/sources/" + folderSrcID + "/items?prefix=week1/")
	if err != nil {
		t.Fatal(err)
	}
	itemsRaw, _ := io.ReadAll(itemsResp.Body)
	itemsResp.Body.Close()
	if itemsResp.StatusCode != http.StatusOK || !bytes.Contains(itemsRaw, []byte("deck.txt")) {
		t.Fatalf("browse subfolder failed status=%d body=%s", itemsResp.StatusCode, string(itemsRaw))
	}

	zipGrantReq, _ := json.Marshal(createGrantRequest{SourceIDs: []string{folderSrcID}})
	zgResp, err := cliAlice.Post(ts.URL+"/guest-api/download-grants", "application/json", bytes.NewReader(zipGrantReq))
	if err != nil {
		t.Fatal(err)
	}
	var zGrant createGrantResponse
	_ = json.NewDecoder(zgResp.Body).Decode(&zGrant)
	zgResp.Body.Close()
	zDL, err := cliAlice.Get(ts.URL + zGrant.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	zipBytes, _ := io.ReadAll(zDL.Body)
	zDL.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatalf("invalid ZIP downloaded: %v", err)
	}
	if len(zr.File) == 0 {
		t.Fatal("empty ZIP downloaded from guest room")
	}

	// Cross-guest isolation: Bob (another approved guest) cannot use Alice's grant or progress.
	cliBob, bobID := joinTestGuest(t, ts.URL, room.InviteSecret, "Bob")
	if err := room.ApproveGuest(bobID); err != nil {
		t.Fatal(err)
	}
	crossProg, err := cliBob.Get(ts.URL + "/guest-api/download-progress/" + grantRes.ProgressID)
	if err != nil {
		t.Fatal(err)
	}
	crossProg.Body.Close()
	if crossProg.StatusCode != http.StatusNotFound {
		t.Fatalf("Bob accessing Alice progress returned %d; want 404", crossProg.StatusCode)
	}
}

func TestGuestRoomConcurrentBroadcastScale(t *testing.T) {
	shareDir := t.TempDir()
	broadcastFile := filepath.Join(shareDir, "workshop-kit.bin")
	payload := bytes.Repeat([]byte("B"), 512*1024)
	if err := os.WriteFile(broadcastFile, payload, 0644); err != nil {
		t.Fatal(err)
	}

	ts, _, room, _ := setupTestGuestModeServer(t, guest.RoomConfig{
		Mode:              guest.RoomModeDownloadOnly,
		AutoApproveGuests: true,
		SharedPaths:       []string{broadcastFile},
	})
	sources := room.ListSharedSources()
	if len(sources) != 1 {
		t.Fatalf("expected 1 shared source, got %d", len(sources))
	}
	sourceID := sources[0].ID

	const attendees = 25
	var wg sync.WaitGroup
	errCh := make(chan error, attendees)

	for i := 0; i < attendees; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cli, _ := joinTestGuest(t, ts.URL, room.InviteSecret, fmt.Sprintf("Attendee-%02d", idx))
			reqBody, _ := json.Marshal(createGrantRequest{SourceIDs: []string{sourceID}})
			gResp, err := cli.Post(ts.URL+"/guest-api/download-grants", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				errCh <- err
				return
			}
			var grantRes createGrantResponse
			_ = json.NewDecoder(gResp.Body).Decode(&grantRes)
			gResp.Body.Close()
			if gResp.StatusCode != http.StatusCreated {
				errCh <- fmt.Errorf("attendee %d grant status = %d", idx, gResp.StatusCode)
				return
			}
			dlResp, err := cli.Get(ts.URL + grantRes.DownloadURL)
			if err != nil {
				errCh <- err
				return
			}
			data, err := io.ReadAll(dlResp.Body)
			dlResp.Body.Close()
			if err != nil || dlResp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("attendee %d download status=%d err=%v", idx, dlResp.StatusCode, err)
				return
			}
			if !bytes.Equal(data, payload) {
				errCh <- fmt.Errorf("attendee %d size mismatch: got %d want %d", idx, len(data), len(payload))
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	rv := room.View()
	if rv.DownloadCount != attendees {
		t.Fatalf("DownloadCount = %d; want %d", rv.DownloadCount, attendees)
	}
	if rv.ActiveDownloads != 0 {
		t.Fatalf("ActiveDownloads = %d; want 0 after completion", rv.ActiveDownloads)
	}
}
