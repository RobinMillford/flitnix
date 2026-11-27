package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func setupG23Server(t *testing.T, opts session.PolicyOptions, clock func() time.Time) (*httptest.Server, *session.Manager, *session.Session, transfer.SharedSource, string) {
	t.Helper()
	tmpDir := t.TempDir()
	samplePath := filepath.Join(tmpDir, "secret-bundle.bin")
	sampleBytes := bytes.Repeat([]byte("G23-Payload-"), 4096)
	if err := os.WriteFile(samplePath, sampleBytes, 0644); err != nil {
		t.Fatal(err)
	}

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	reg := transfer.NewRegistry()
	src, err := reg.AddFileSource(samplePath, "secret-bundle.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	downloads := transfer.NewDownloadRegistry()
	sessMgr := session.NewManager(session.Config{
		IdleTimeout: 10 * time.Minute,
		Now:         clock,
	})
	t.Cleanup(sessMgr.Close)

	sess, err := sessMgr.CreateWithPolicy(opts)
	if err != nil {
		t.Fatal(err)
	}

	guestMgr := guest.NewManager()
	t.Cleanup(guestMgr.Close)

	svcs := &Services{
		Sessions:     sessMgr,
		Transfers:    transfer.NewService(transfer.Config{Store: store}),
		Coordinator:  transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:     reg,
		Downloads:    downloads,
		GuestManager: guestMgr,
	}
	grants := newGrantStore(downloads)
	handler := newRouter(svcs, 5*time.Second, 5*time.Second, grants)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, sessMgr, sess, src, string(sampleBytes)
}

func doBearerReq(t *testing.T, cli *http.Client, method, url, token string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPersonalSessionPINEnforcementAndRateLimit(t *testing.T) {
	ts, _, sess, src, wantContent := setupG23Server(t, session.PolicyOptions{
		PinCode: "7391",
	}, nil)

	jar, _ := cookiejar.New(nil)
	cli := &http.Client{Jar: jar}

	// Before PIN unlock, direct API calls (files, grants, direct file download) MUST be blocked (403 pin_required).
	for _, path := range []string{
		"/api/files",
		"/api/sources",
		"/api/files/" + src.ID + "/download",
	} {
		resp := doBearerReq(t, cli, http.MethodGet, ts.URL+path, sess.Token(), nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s before PIN unlock = %d; want 403", path, resp.StatusCode)
		}
	}
	grantBlocked := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/download-grants", sess.Token(), createGrantRequest{
		SourceIDs: []string{src.ID},
	})
	grantBlocked.Body.Close()
	if grantBlocked.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /api/download-grants before PIN unlock = %d; want 403", grantBlocked.StatusCode)
	}

	// Wrong PIN repeatedly triggers 429 rate limit + Retry-After header.
	var sawRateLimit bool
	for i := 0; i < 6; i++ {
		bad := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/pin/verify", sess.Token(), map[string]string{
			"pin": "0000",
		})
		bad.Body.Close()
		if bad.StatusCode == http.StatusTooManyRequests {
			if bad.Header.Get("Retry-After") == "" {
				t.Fatal("expected Retry-After header on 429")
			}
			sawRateLimit = true
			break
		}
	}
	if !sawRateLimit {
		t.Fatal("expected repeated wrong PIN guesses to trigger 429 rate limit")
	}

	// Separate client/IP or verified flow: create fresh session with clean IP/limiter behavior by hitting distinct session?
	// Same IP stays locked out even with the right PIN until lockout window expires!
	lockedOut := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/pin/verify", sess.Token(), map[string]string{
		"pin": "7391",
	})
	lockedOut.Body.Close()
	if lockedOut.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected locked-out IP to receive 429 even on correct PIN, got %d", lockedOut.StatusCode)
	}

	// Test clean unlock & full file download on a non-locked server.
	ts2, _, sess2, src2, wantContent2 := setupG23Server(t, session.PolicyOptions{PinCode: "7391"}, nil)
	jar2, _ := cookiejar.New(nil)
	cli2 := &http.Client{Jar: jar2}
	okResp := doBearerReq(t, cli2, http.MethodPost, ts2.URL+"/api/pin/verify", sess2.Token(), map[string]string{
		"pin": "7391",
	})
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/pin/verify = %d; want 200", okResp.StatusCode)
	}

	// Separate unverified client WITH the Bearer token STILL cannot bypass PIN!
	jarEve, _ := cookiejar.New(nil)
	cliEve := &http.Client{Jar: jarEve}
	eveResp := doBearerReq(t, cliEve, http.MethodGet, ts2.URL+"/api/files", sess2.Token(), nil)
	eveResp.Body.Close()
	if eveResp.StatusCode != http.StatusForbidden {
		t.Fatalf("unverified client Eve GET /api/files = %d; want 403", eveResp.StatusCode)
	}

	// Unlocked client can create grant & download content.
	gResp := doBearerReq(t, cli2, http.MethodPost, ts2.URL+"/api/download-grants", sess2.Token(), createGrantRequest{
		SourceIDs: []string{src2.ID},
	})
	var gr createGrantResponse
	_ = json.NewDecoder(gResp.Body).Decode(&gr)
	gResp.Body.Close()
	if gResp.StatusCode != http.StatusCreated {
		t.Fatalf("grant creation after PIN unlock = %d; want 201", gResp.StatusCode)
	}
	dlResp, err := cli2.Get(ts2.URL + gr.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(dlResp.Body)
	dlResp.Body.Close()
	if string(got) != wantContent2 || wantContent2 != wantContent {
		t.Fatalf("downloaded content mismatch")
	}
}

func TestOneTimeDownloadConcurrencyAndSelfDestruct(t *testing.T) {
	ts, sessMgr, sess, src, wantContent := setupG23Server(t, session.PolicyOptions{
		MaxDownloads:   1,
		OneTimeAutoEnd: true,
	}, nil)

	const racers = 5
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			jar, _ := cookiejar.New(nil)
			cli := &http.Client{Jar: jar}
			gResp := doBearerReq(t, cli, http.MethodPost, ts.URL+"/api/download-grants", sess.Token(), createGrantRequest{
				SourceIDs: []string{src.ID},
			})
			defer gResp.Body.Close()
			if gResp.StatusCode != http.StatusCreated {
				return
			}
			var gr createGrantResponse
			if err := json.NewDecoder(gResp.Body).Decode(&gr); err != nil {
				return
			}
			dlResp, err := cli.Get(ts.URL + gr.DownloadURL)
			if err != nil {
				return
			}
			raw, _ := io.ReadAll(dlResp.Body)
			dlResp.Body.Close()
			if dlResp.StatusCode == http.StatusOK && string(raw) == wantContent {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	if wins.Load() != 1 {
		t.Fatalf("one-time concurrent wins = %d; want exactly 1", wins.Load())
	}

	// Wait briefly for auto-end goroutine & verify session self-destructed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := sessMgr.Validate(sess.Token()); err != nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if _, err := sessMgr.Validate(sess.Token()); err == nil {
		t.Fatal("expected 1-time session to self-destruct after completion")
	}
}

func TestCustomExpiryAndHeadSafety(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}

	ts, _, sess, src, wantContent := setupG23Server(t, session.PolicyOptions{
		MaxDownloads:   1,
		ExpiresIn:      1 * time.Minute,
		OneTimeAutoEnd: false,
	}, clock)

	cli := &http.Client{}
	// HEAD on /api/files/{id}/download MUST NOT consume the 1-time download quota!
	headResp := doBearerReq(t, cli, http.MethodHead, ts.URL+"/api/files/"+src.ID+"/download", sess.Token(), nil)
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD download status = %d; want 200", headResp.StatusCode)
	}

	// Now actual GET succeeds and consumes the 1-time slot.
	getResp := doBearerReq(t, cli, http.MethodGet, ts.URL+"/api/files/"+src.ID+"/download", sess.Token(), nil)
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK || string(body) != wantContent {
		t.Fatalf("GET after HEAD failed: status=%d len=%d", getResp.StatusCode, len(body))
	}

	// Second direct download MUST fail with 410 Gone (not bypass limit!).
	secondResp := doBearerReq(t, cli, http.MethodGet, ts.URL+"/api/files/"+src.ID+"/download", sess.Token(), nil)
	secondResp.Body.Close()
	if secondResp.StatusCode != http.StatusGone {
		t.Fatalf("second direct file download = %d; want 410", secondResp.StatusCode)
	}

	// Advancing past hard expiry rejects requests with 401.
	advance(2 * time.Minute)
	expResp := doBearerReq(t, cli, http.MethodGet, ts.URL+"/api/files", sess.Token(), nil)
	expResp.Body.Close()
	if expResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET after hard expiry = %d; want 401", expResp.StatusCode)
	}
}

func TestGuestRoomPINAndDownloadCap(t *testing.T) {
	shareDir := t.TempDir()
	handout := filepath.Join(shareDir, "exam.pdf")
	if err := os.WriteFile(handout, []byte("exam-pdf-data"), 0644); err != nil {
		t.Fatal(err)
	}
	ts, _, room, _ := setupTestGuestModeServer(t, guest.RoomConfig{
		Mode:              guest.RoomModeDownloadOnly,
		AutoApproveGuests: true,
		PinCode:           "4321",
		MaxDownloads:      1,
		SharedPaths:       []string{handout},
	})
	srcID := room.ListSharedSources()[0].ID

	// Wrong PIN rejected with 403.
	badBody, _ := json.Marshal(map[string]string{"secret": room.InviteSecret, "nickname": "Bad", "pin": "9999"})
	badResp, err := http.Post(ts.URL+"/guest-api/join", "application/json", bytes.NewReader(badBody))
	if err != nil {
		t.Fatal(err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("join with wrong PIN = %d; want 403", badResp.StatusCode)
	}

	// Correct PIN joins cleanly.
	jar, _ := cookiejar.New(nil)
	cli := &http.Client{Jar: jar}
	goodBody, _ := json.Marshal(map[string]string{"secret": room.InviteSecret, "nickname": "Good", "pin": "4321"})
	goodResp, err := cli.Post(ts.URL+"/guest-api/join", "application/json", bytes.NewReader(goodBody))
	if err != nil {
		t.Fatal(err)
	}
	goodResp.Body.Close()
	if goodResp.StatusCode != http.StatusCreated {
		t.Fatalf("join with valid PIN = %d; want 201", goodResp.StatusCode)
	}

	// 1st download succeeds; 2nd exceeds MaxDownloads=1 -> 410.
	gReq, _ := json.Marshal(createGrantRequest{SourceIDs: []string{srcID}})
	g1, _ := cli.Post(ts.URL+"/guest-api/download-grants", "application/json", bytes.NewReader(gReq))
	var gr1 createGrantResponse
	_ = json.NewDecoder(g1.Body).Decode(&gr1)
	g1.Body.Close()
	if g1.StatusCode != http.StatusCreated {
		t.Fatalf("1st guest grant = %d; want 201", g1.StatusCode)
	}
	dl1, _ := cli.Get(ts.URL + gr1.DownloadURL)
	_, _ = io.ReadAll(dl1.Body)
	dl1.Body.Close()

	g2, _ := cli.Post(ts.URL+"/guest-api/download-grants", "application/json", bytes.NewReader(gReq))
	g2.Body.Close()
	if g2.StatusCode != http.StatusGone {
		t.Fatalf("2nd guest grant beyond MaxDownloads=1 = %d; want 410 (fmt check %v)", g2.StatusCode, fmt.Sprintf("rem=%d", room.View().RemainingDownloads))
	}
}
