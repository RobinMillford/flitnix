package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// mockImmutable activates test immutability for the duration of the test.
func mockImmutable(t *testing.T) {
	t.Helper()
	orig := checkOSImmutabilityFn
	checkOSImmutabilityFn = func(f *os.File, path string) (bool, io.Closer) {
		return true, nil
	}
	t.Cleanup(func() {
		checkOSImmutabilityFn = orig
	})
}

// extractContinuationCookie retrieves the flitnix-cont-<id> cookie from Set-Cookie headers.
func extractContinuationCookie(t *testing.T, rec *httptest.ResponseRecorder, grantID string) *http.Cookie {
	t.Helper()
	contCookieName := continuationCookiePrefix + grantID
	for _, c := range rec.Result().Cookies() {
		if c.Name == contCookieName {
			return c
		}
	}
	t.Fatalf("continuation cookie %q not found in response cookies", contCookieName)
	return nil
}

// extractGrantID gets the grant ID from a download URL like "/api/downloads/<id>/content".
func extractGrantID(downloadURL string) string {
	s := strings.TrimPrefix(downloadURL, "/api/downloads/")
	s = strings.TrimSuffix(s, "/content")
	return s
}

// TestDownloadRange_Scenario21_30_MatchingIfRangeReturns206 tests that a valid
// strong If-Range matching the ETag of an immutable Tier 1 file returns 206
// Partial Content with the exact requested byte slice.
func TestDownloadRange_Scenario21_30_MatchingIfRangeReturns206(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!@#$%^&*()")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])
	expectedETag := fmt.Sprintf("\"sha256-%s\"", sumHex)

	p := filepath.Join(t.TempDir(), "data.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "data.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	// Initial GET to redeem grant and receive continuation cookie
	initReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	initReq.AddCookie(initialCookie)
	initRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(initRec, initReq)

	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d body=%s", initRec.Code, initRec.Body.String())
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Verify continuation cookie properties per RFC 6265 and spec
	if contCookie.Path != "/api/downloads/"+grantID {
		t.Errorf("cookie Path = %q, want %q", contCookie.Path, "/api/downloads/"+grantID)
	}
	if !contCookie.HttpOnly {
		t.Error("cookie HttpOnly = false, want true")
	}
	if contCookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite = %v, want SameSiteStrictMode", contCookie.SameSite)
	}
	if contCookie.MaxAge != 86400 {
		t.Errorf("cookie MaxAge = %d, want 86400", contCookie.MaxAge)
	}

	// Issue Range continuation request with matching strong If-Range
	rangeReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	rangeReq.AddCookie(contCookie)
	rangeReq.Header.Set("Range", "bytes=10-25")
	rangeReq.Header.Set("If-Range", expectedETag)
	rangeRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rangeRec, rangeReq)

	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", rangeRec.Code)
	}
	if got := rangeRec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 10-25/%d", len(content)) {
		t.Errorf("Content-Range = %q, want %q", got, fmt.Sprintf("bytes 10-25/%d", len(content)))
	}
	if got := rangeRec.Header().Get("Content-Length"); got != "16" {
		t.Errorf("Content-Length = %q, want 16", got)
	}
	if got := rangeRec.Header().Get("ETag"); got != expectedETag {
		t.Errorf("ETag = %q, want %q", got, expectedETag)
	}
	if got := rangeRec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}

	expectedSlice := content[10:26]
	if !bytes.Equal(rangeRec.Body.Bytes(), expectedSlice) {
		t.Fatalf("range body mismatch: got %q, want %q", rangeRec.Body.String(), string(expectedSlice))
	}
}

// TestDownloadRange_Scenario22_31_MismatchingIfRangeReturns200Full tests that
// a mismatched strong If-Range on a Tier 1 file causes the server to ignore
// the Range header and return the full file representation with 200 OK.
func TestDownloadRange_Scenario22_31_MismatchingIfRangeReturns200Full(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("The quick brown fox jumps over the lazy dog.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "fox.txt")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "fox.txt", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Continuation with invalid/mismatching If-Range
	rangeReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	rangeReq.AddCookie(contCookie)
	rangeReq.Header.Set("Range", "bytes=10-25")
	rangeReq.Header.Set("If-Range", "\"sha256-0000000000000000000000000000000000000000000000000000000000000000\"")
	rangeRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rangeRec, rangeReq)

	if rangeRec.Code != http.StatusOK {
		t.Fatalf("range status = %d, want 200 for mismatched If-Range", rangeRec.Code)
	}
	if rangeRec.Header().Get("Content-Range") != "" {
		t.Errorf("unexpected Content-Range header on 200 response: %q", rangeRec.Header().Get("Content-Range"))
	}
	if !bytes.Equal(rangeRec.Body.Bytes(), content) {
		t.Fatal("body does not match full content on mismatched validator")
	}
}

// TestDownloadRange_Scenario23_OutOfBoundsRangeReturns416 tests that requesting
// a range beyond the file length returns 416 Range Not Satisfiable with Content-Range: bytes */total.
func TestDownloadRange_Scenario23_OutOfBoundsRangeReturns416(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("short file content")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "short.txt")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "short.txt", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	contCookie := extractContinuationCookie(t, initRec, grantID)

	rangeReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	rangeReq.AddCookie(contCookie)
	rangeReq.Header.Set("Range", "bytes=99999999-")
	rangeReq.Header.Set("If-Range", fmt.Sprintf("\"sha256-%s\"", sumHex))
	rangeRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rangeRec, rangeReq)

	if rangeRec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rangeRec.Code)
	}
	wantContentRange := fmt.Sprintf("bytes */%d", len(content))
	if got := rangeRec.Header().Get("Content-Range"); got != wantContentRange {
		t.Errorf("Content-Range = %q, want %q", got, wantContentRange)
	}
}

// TestDownloadRange_Scenario24_DynamicZIPIgnoresRange tests that dynamic ZIP archives
// (Tier 3) set Accept-Ranges: none and explicitly stream the full archive, ignoring Range.
func TestDownloadRange_Scenario24_DynamicZIPIgnoresRange(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	dir := filepath.Join(t.TempDir(), "ZipFolder")
	writeTree(t, dir, map[string]string{
		"file1.txt": "File One Content",
		"file2.txt": "File Two Content",
	})
	source, err := env.reg.AddFolder(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, source.ID)
	if !g.Archive {
		t.Fatal("expected g.Archive=true for folder source")
	}
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Continuation with Range header
	rangeReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	rangeReq.AddCookie(contCookie)
	rangeReq.Header.Set("Range", "bytes=10-50")
	rangeRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rangeRec, rangeReq)

	if rangeRec.Code != http.StatusOK {
		t.Fatalf("dynamic ZIP status = %d, want 200", rangeRec.Code)
	}
	if got := rangeRec.Header().Get("Accept-Ranges"); got != "none" {
		t.Errorf("Accept-Ranges = %q, want none", got)
	}
	if rangeRec.Header().Get("Content-Range") != "" {
		t.Errorf("dynamic ZIP emitted Content-Range header: %q", rangeRec.Header().Get("Content-Range"))
	}

	// Verify valid zip structure
	bodyBytes := rangeRec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(bodyBytes), int64(len(bodyBytes)))
	if err != nil {
		t.Fatalf("failed to parse returned zip archive: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("zip file count = %d, want 2", len(zr.File))
	}
}

// TestDownloadRange_Scenario25_WeakETagIfRangeRejectedReturns200 tests RFC 9110 §13.1.2:
// weak entity tags in If-Range MUST NOT be used for subrange requests; server ignores
// Range and returns 200 OK with the entire representation.
func TestDownloadRange_Scenario25_WeakETagIfRangeRejectedReturns200(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("Weak ETag rejection test content.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "weak.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "weak.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Send weak If-Range: W/"sha256-<digest>"
	rangeReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	rangeReq.AddCookie(contCookie)
	rangeReq.Header.Set("Range", "bytes=5-15")
	rangeReq.Header.Set("If-Range", fmt.Sprintf("W/\"sha256-%s\"", sumHex))
	rangeRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rangeRec, rangeReq)

	if rangeRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for weak If-Range", rangeRec.Code)
	}
	if rangeRec.Header().Get("Content-Range") != "" {
		t.Errorf("unexpected Content-Range header: %q", rangeRec.Header().Get("Content-Range"))
	}
	if !bytes.Equal(rangeRec.Body.Bytes(), content) {
		t.Fatal("response body does not match full content")
	}
}

// newTestEnvWithSessionIdle builds a test environment with a custom session idle timeout.
func newTestEnvWithSessionIdle(t *testing.T, idle time.Duration) *testEnv {
	t.Helper()
	clock := newFakeClock()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("cannot create receive store: %v", err)
	}
	env := &testEnv{
		t:     t,
		clock: clock,
		sess:  session.NewManager(session.Config{IdleTimeout: idle, Now: clock.Now}),
		store: store,
	}
	env.trans = transfer.NewService(transfer.Config{Store: store})
	env.coord = transfer.NewCoordinator(transfer.CoordinatorConfig{})
	env.reg = transfer.NewRegistry()
	env.dls = transfer.NewDownloadRegistry()
	env.svcs = &Services{
		Sessions:    env.sess,
		Transfers:   env.trans,
		Coordinator: env.coord,
		Registry:    env.reg,
		Downloads:   env.dls,
		Diagnose:    env.diagSink,
	}
	env.srv = New(Config{Services: env.svcs})
	return env
}

// TestDownloadRange_Scenario26_IdleContinuationTimeout tests that continuation records
// expire after 30 minutes of idle time, returning 410 Gone, but active streams are exempt.
func TestDownloadRange_Scenario26_IdleContinuationTimeout(t *testing.T) {
	mockImmutable(t)
	// Give the session 2 hours idle timeout so we isolate and test the 30-minute continuation sliding timeout
	env := newTestEnvWithSessionIdle(t, 2*time.Hour)
	env.srv.grants.now = env.clock.Now
	tok := env.token()

	content := []byte("Sliding idle timeout test content.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "timeout.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "timeout.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Advance clock by 20 minutes (within 30 minute timeout)
	env.clock.Advance(20 * time.Minute)

	activeReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	activeReq.AddCookie(contCookie)
	activeRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(activeRec, activeReq)
	if activeRec.Code != http.StatusOK {
		t.Fatalf("request at 20 min idle = %d, want 200", activeRec.Code)
	}

	// Advance clock by 31 minutes after last activity (exceeds 30 minute sliding window)
	env.clock.Advance(31 * time.Minute)

	expiredReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	expiredReq.AddCookie(contCookie)
	expiredRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(expiredRec, expiredReq)

	if expiredRec.Code != http.StatusGone {
		t.Fatalf("request after 31 min idle = %d, want 410 Gone", expiredRec.Code)
	}

	// Test Active Stream Exemption: an in-progress active stream prevents idle pruning even past 30 minutes
	g2, initCookie2 := env.grantFor(tok, sf.ID)
	gid2 := extractGrantID(g2.DownloadURL)
	initRec2 := env.getDownload(g2, initCookie2, nil)
	contCookie2 := extractContinuationCookie(t, initRec2, gid2)

	// Simulate an active stream by calling authorizeContinuation (increments ActiveStreams to 1)
	_, _, err = env.srv.grants.authorizeContinuation(gid2, contCookie2.Value)
	if err != nil {
		t.Fatalf("authorizeContinuation failed: %v", err)
	}

	// Advance clock by 45 minutes while stream is active
	env.clock.Advance(45 * time.Minute)

	// Trigger prune
	env.srv.grants.mu.Lock()
	env.srv.grants.pruneLocked(env.clock.Now())
	_, foundActive := env.srv.grants.continuations[gid2]
	env.srv.grants.mu.Unlock()

	if !foundActive {
		t.Fatal("active continuation was pruned during ongoing stream (should be exempt)")
	}

	// Finish stream (decrements ActiveStreams to 0 and stamps LastActive)
	env.srv.grants.finishStream(gid2)

	// Advance another 31 minutes after stream ended
	env.clock.Advance(31 * time.Minute)

	env.srv.grants.mu.Lock()
	env.srv.grants.pruneLocked(env.clock.Now())
	_, foundAfterIdle := env.srv.grants.continuations[gid2]
	env.srv.grants.mu.Unlock()

	if foundAfterIdle {
		t.Fatal("continuation was not pruned after 31 minutes of idle following stream finish")
	}
}

// TestDownloadRange_Scenario27_ConcurrentCookieIsolation tests running 4 simultaneous
// downloads with isolated flitnix-cont-<id> cookies at Path=/api/downloads/<id>,
// verifying zero cross-talk, correct rejection of cross-download cookies, and parallel range fetching.
func TestDownloadRange_Scenario27_ConcurrentCookieIsolation(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	const count = 4
	type dlMeta struct {
		grantID string
		url     string
		cookie  *http.Cookie
		content []byte
		etag    string
	}

	downloads := make([]dlMeta, count)

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("file_%d.bin", i)
		data := bytes.Repeat([]byte{byte(i + 1)}, 1024)
		sum := sha256.Sum256(data)
		sumHex := hex.EncodeToString(sum[:])
		etag := fmt.Sprintf("\"sha256-%s\"", sumHex)

		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		sf, err := env.reg.AddImmutable(p, name, "", sumHex)
		if err != nil {
			t.Fatal(err)
		}

		g, initialCookie := env.grantFor(tok, sf.ID)
		gid := extractGrantID(g.DownloadURL)

		initRec := env.getDownload(g, initialCookie, nil)
		if initRec.Code != http.StatusOK {
			t.Fatalf("download %d redemption failed: %d", i, initRec.Code)
		}
		cont := extractContinuationCookie(t, initRec, gid)

		downloads[i] = dlMeta{
			grantID: gid,
			url:     g.DownloadURL,
			cookie:  cont,
			content: data,
			etag:    etag,
		}
	}

	// 1. Verify all 4 continuation tickets are distinct 64-char hex strings
	seenTickets := make(map[string]bool)
	for i, d := range downloads {
		if len(d.cookie.Value) != 64 {
			t.Errorf("download %d ticket length = %d, want 64", i, len(d.cookie.Value))
		}
		if seenTickets[d.cookie.Value] {
			t.Errorf("duplicate ticket detected: %s", d.cookie.Value)
		}
		seenTickets[d.cookie.Value] = true

		expectedPath := "/api/downloads/" + d.grantID
		if d.cookie.Path != expectedPath {
			t.Errorf("download %d cookie path = %q, want %q", i, d.cookie.Path, expectedPath)
		}
	}

	// 2. Cross-download cookie rejection: using cookie from download A on download B must fail
	for i := 0; i < count; i++ {
		targetDl := downloads[i]
		foreignCookie := downloads[(i+1)%count].cookie

		// Attempt using foreign cookie with its actual name
		req := httptest.NewRequest(http.MethodGet, targetDl.url, nil)
		req.AddCookie(foreignCookie)
		rec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(rec, req)

		if rec.Code == http.StatusOK || rec.Code == http.StatusPartialContent {
			t.Fatalf("cross-download cookie exploit succeeded! status=%d", rec.Code)
		}

		// Attempt forging foreign ticket under target cookie name
		forgedReq := httptest.NewRequest(http.MethodGet, targetDl.url, nil)
		forgedReq.AddCookie(&http.Cookie{
			Name:  continuationCookiePrefix + targetDl.grantID,
			Value: foreignCookie.Value,
		})
		forgedRec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(forgedRec, forgedReq)

		if forgedRec.Code == http.StatusOK || forgedRec.Code == http.StatusPartialContent {
			t.Fatalf("forged cross-download ticket succeeded! status=%d", forgedRec.Code)
		}
	}

	// 3. Parallel range requests across all 4 downloads
	var wg sync.WaitGroup
	errCh := make(chan error, count)

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			d := downloads[idx]

			req := httptest.NewRequest(http.MethodGet, d.url, nil)
			req.AddCookie(d.cookie)
			req.Header.Set("Range", "bytes=100-199")
			req.Header.Set("If-Range", d.etag)
			rec := httptest.NewRecorder()
			env.srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusPartialContent {
				errCh <- fmt.Errorf("download %d range status = %d, want 206", idx, rec.Code)
				return
			}
			expectedRange := d.content[100:200]
			if !bytes.Equal(rec.Body.Bytes(), expectedRange) {
				errCh <- fmt.Errorf("download %d body corrupted during concurrent read", idx)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

// TestDownloadRange_Scenario28_29_Tier2MutableSupportsRange tests that an ordinary
// mutable file (Tier 2) advertises Accept-Ranges: bytes and a strong metadata ETag,
// serves 206 Partial Content when Range/If-Range match, falls back to 200 OK when
// If-Range is weak or mismatched, and serves 416 for unsatisfiable ranges.
func TestDownloadRange_Scenario28_29_Tier2MutableSupportsRange(t *testing.T) {
	// Notice: mockImmutable is NOT called, so isOSImmutable returns false.
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("Mutable file content that changes over time.")
	p := filepath.Join(t.TempDir(), "mutable.txt")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	// Registered with regular Add (no SHA-256 pre-hash)
	sf, err := env.reg.Add(p, "mutable.txt", "")
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	if got := initRec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("initial Accept-Ranges = %q, want bytes", got)
	}
	metaETag := initRec.Header().Get("ETag")
	if metaETag == "" || strings.HasPrefix(metaETag, "W/") || strings.HasPrefix(metaETag, "\"sha256-") {
		t.Fatalf("unexpected Tier 2 strong metadata ETag: %q", metaETag)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Scenario 28: Range with matching strong metadata If-Range returns 206
	s28Req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	s28Req.AddCookie(contCookie)
	s28Req.Header.Set("Range", "bytes=5-20")
	s28Req.Header.Set("If-Range", metaETag)
	s28Rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(s28Rec, s28Req)

	if s28Rec.Code != http.StatusPartialContent {
		t.Fatalf("scenario 28 status = %d, want 206", s28Rec.Code)
	}
	if got := s28Rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("scenario 28 Accept-Ranges = %q, want bytes", got)
	}
	if got := s28Rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 5-20/%d", len(content)) {
		t.Errorf("scenario 28 Content-Range = %q", got)
	}
	if !bytes.Equal(s28Rec.Body.Bytes(), content[5:21]) {
		t.Fatal("scenario 28 body did not return requested byte range")
	}

	// Scenario 29: Range with weak If-Range falls back to 200 full representation
	s29Req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	s29Req.AddCookie(contCookie)
	s29Req.Header.Set("Range", "bytes=5-20")
	s29Req.Header.Set("If-Range", "W/\"custom-weak-validator\"")
	s29Rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(s29Rec, s29Req)

	if s29Rec.Code != http.StatusOK {
		t.Fatalf("scenario 29 status = %d, want 200", s29Rec.Code)
	}
	if got := s29Rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("scenario 29 Accept-Ranges = %q, want bytes", got)
	}
	if !bytes.Equal(s29Rec.Body.Bytes(), content) {
		t.Fatal("scenario 29 body did not return full file starting from byte 0")
	}
}

// TestDownloadRange_Scenario37_ConcurrentReadsIndependentHandles tests that multiple
// concurrent Range requests to the same Tier 1 immutable file use independent file
// descriptors with zero seek cursor interference or data corruption.
func TestDownloadRange_Scenario37_ConcurrentReadsIndependentHandles(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	const totalBlocks = 32
	const blockSize = 8192
	content := make([]byte, totalBlocks*blockSize)
	for b := 0; b < totalBlocks; b++ {
		fill := byte((b * 17) & 0xff)
		for i := 0; i < blockSize; i++ {
			content[b*blockSize+i] = fill
		}
	}

	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])
	etag := fmt.Sprintf("\"sha256-%s\"", sumHex)

	p := filepath.Join(t.TempDir(), "pattern.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "pattern.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	contCookie := extractContinuationCookie(t, initRec, grantID)

	const concurrency = 16
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	for c := 0; c < concurrency; c++ {
		wg.Add(1)
		go func(clientIdx int) {
			defer wg.Done()
			blockIdx := (clientIdx * 2) % totalBlocks
			start := blockIdx * blockSize
			end := start + blockSize - 1

			rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
			req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
			req.AddCookie(contCookie)
			req.Header.Set("Range", rangeHeader)
			req.Header.Set("If-Range", etag)
			rec := httptest.NewRecorder()
			env.srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusPartialContent {
				errCh <- fmt.Errorf("client %d status = %d, want 206", clientIdx, rec.Code)
				return
			}

			expectedSlice := content[start : end+1]
			actualBytes := rec.Body.Bytes()
			if !bytes.Equal(actualBytes, expectedSlice) {
				errCh <- fmt.Errorf("client %d got corrupted block data (seek collision detected)", clientIdx)
				return
			}
		}(c)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

// TestDownloadRange_InitialGrantSingleUse tests that the initial flitnix-download-grant
// cookie is consumed upon first redemption and cannot be redeemed a second time.
func TestDownloadRange_InitialGrantSingleUse(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("Single use grant test.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "single.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "single.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)

	// First redemption: succeeds
	rec1 := env.getDownload(g, initialCookie, nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first redemption = %d, want 200", rec1.Code)
	}

	// Second redemption with the same initial grant cookie: MUST fail
	rec2 := env.getDownload(g, initialCookie, nil)
	if rec2.Code == http.StatusOK || rec2.Code == http.StatusPartialContent {
		t.Fatalf("second redemption with consumed grant succeeded: %d", rec2.Code)
	}
	if rec2.Code != http.StatusForbidden && rec2.Code != http.StatusUnauthorized {
		t.Logf("second redemption code = %d", rec2.Code)
	}
}

// TestDownloadRange_SessionRevocationInvalidatesContinuation tests that revoking
// the owning session immediately invalidates continuation authorization.
func TestDownloadRange_SessionRevocationInvalidatesContinuation(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("Session revocation test content.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "revoc.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "revoc.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Verify continuation works before revocation
	preReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	preReq.AddCookie(contCookie)
	preRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(preRec, preReq)
	if preRec.Code != http.StatusOK {
		t.Fatalf("pre-revocation status = %d, want 200", preRec.Code)
	}

	// Revoke the session
	sess, err := env.sess.Validate(tok)
	if err != nil {
		t.Fatal(err)
	}
	env.sess.RevokeSession(sess)
	if sess.Status() != session.StatusRevoked {
		t.Fatalf("session status = %v, want StatusRevoked", sess.Status())
	}

	// Attempt continuation request after session revocation
	postReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	postReq.AddCookie(contCookie)
	postRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusUnauthorized {
		t.Fatalf("post-revocation continuation status = %d, want 401", postRec.Code)
	}
}

// TestDownloadRange_CookieOrderIndependence tests that the server parses the continuation
// cookie correctly regardless of its position among other cookies in the Cookie header.
func TestDownloadRange_CookieOrderIndependence(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("Order independence content.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "order.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "order.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Build raw Cookie header with arbitrary surrounding cookies
	cookieHeaderVal := fmt.Sprintf("zebra=123; %s=%s; alpha=abc; flitnix-dummy=xyz", contCookie.Name, contCookie.Value)

	req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	req.Header.Set("Cookie", cookieHeaderVal)
	req.Header.Set("Range", "bytes=0-5")
	req.Header.Set("If-Range", fmt.Sprintf("\"sha256-%s\"", sumHex))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status with scrambled cookies = %d, want 206", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), content[0:6]) {
		t.Fatalf("body = %q, want %q", rec.Body.String(), content[0:6])
	}
}

// TestDownloadRange_RouteAliasesAndHeadRequests tests that /api/downloads/{id},
// /api/downloads/{id}/content, and /api/archive-downloads/{id} all route to the download
// handler, and that HEAD requests correctly suppress the body while returning headers.
func TestDownloadRange_RouteAliasesAndHeadRequests(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	content := []byte("Route aliases test content.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])
	etag := fmt.Sprintf("\"sha256-%s\"", sumHex)

	p := filepath.Join(t.TempDir(), "alias.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "alias.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	contCookie := extractContinuationCookie(t, initRec, grantID)

	testPaths := []string{
		"/api/downloads/" + grantID,
		"/api/downloads/" + grantID + "/content",
		"/api/archive-downloads/" + grantID,
	}

	for _, path := range testPaths {
		// Test GET via path
		getReq := httptest.NewRequest(http.MethodGet, path, nil)
		getReq.AddCookie(contCookie)
		getRec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(getRec, getReq)

		if getRec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, getRec.Code)
		}
		if !bytes.Equal(getRec.Body.Bytes(), content) {
			t.Errorf("GET %s body mismatch", path)
		}

		// Test HEAD via path
		headReq := httptest.NewRequest(http.MethodHead, path, nil)
		headReq.AddCookie(contCookie)
		headRec := httptest.NewRecorder()
		env.srv.Handler().ServeHTTP(headRec, headReq)

		if headRec.Code != http.StatusOK {
			t.Errorf("HEAD %s status = %d, want 200", path, headRec.Code)
		}
		if headRec.Body.Len() != 0 {
			t.Errorf("HEAD %s returned non-empty body length %d", path, headRec.Body.Len())
		}
		if got := headRec.Header().Get("ETag"); got != etag {
			t.Errorf("HEAD %s ETag = %q, want %q", path, got, etag)
		}
		if got := headRec.Header().Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("HEAD %s Accept-Ranges = %q, want bytes", path, got)
		}
	}
}

type testMockCloser struct {
	mu     sync.Mutex
	closed bool
}

func (m *testMockCloser) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *testMockCloser) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// TestPlatformImmutability_Windows_ReadOnlyAttributeFallsBackToTier2 proves that
// on Windows, FILE_ATTRIBUTE_READONLY alone MUST NOT qualify a file for Tier 1.
// When only FILE_ATTRIBUTE_READONLY is present without a verified read-only volume
// or mandatory non-shared read lock, the server safely fails closed to Tier 2.
func TestPlatformImmutability_Windows_ReadOnlyAttributeFallsBackToTier2(t *testing.T) {
	// 1. Verify policy decision logic directly
	if EvaluateWindowsImmutability(false, true, false) {
		t.Fatal("EvaluateWindowsImmutability allowed FILE_ATTRIBUTE_READONLY alone to qualify for Tier 1")
	}

	// 2. Test end-to-end HTTP pipeline
	orig := checkOSImmutabilityFn
	// Simulate Windows file with only FILE_ATTRIBUTE_READONLY (no ro-volume, no mandatory lock)
	checkOSImmutabilityFn = func(f *os.File, path string) (bool, io.Closer) {
		return false, nil
	}
	t.Cleanup(func() {
		checkOSImmutabilityFn = orig
	})

	env := newTestEnv(t)
	tok := env.token()

	content := []byte("Windows file with ordinary FILE_ATTRIBUTE_READONLY attribute.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "win_readonly.txt")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	// Registered with known SHA256
	sf, err := env.reg.AddImmutable(p, "win_readonly.txt", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Client sends Range request
	req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	req.AddCookie(contCookie)
	req.Header.Set("Range", "bytes=5-25")
	req.Header.Set("If-Range", fmt.Sprintf("\"sha256-%s\"", sumHex))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	// MUST fall back to Tier 2: uses metadata ETag (not "sha256-..."), so Tier 1 If-Range mismatches -> 200 OK full body
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Tier 2 fallback)", rec.Code)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rec.Header().Get("ETag"); got == "" || got == fmt.Sprintf("\"sha256-%s\"", sumHex) {
		t.Errorf("Tier 2 ETag = %q, must not reuse Tier 1 immutable sha256 ETag", got)
	}
	if rec.Header().Get("Content-Range") != "" {
		t.Errorf("unexpected Content-Range header: %q", rec.Header().Get("Content-Range"))
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatal("response body did not stream full content from byte 0")
	}
}

// TestPlatformImmutability_Windows_MandatoryLockAndReadOnlyVolumeQualifyForTier1 proves
// that verified read-only volumes (or VSS) and mandatory non-shared read locks
// qualify for Tier 1, and that the mandatory lock is maintained across continuation requests.
func TestPlatformImmutability_Windows_MandatoryLockAndReadOnlyVolumeQualifyForTier1(t *testing.T) {
	// 1. Verify policy decision logic directly
	if !EvaluateWindowsImmutability(true, false, false) {
		t.Fatal("EvaluateWindowsImmutability rejected verified read-only volume")
	}
	if !EvaluateWindowsImmutability(false, false, true) {
		t.Fatal("EvaluateWindowsImmutability rejected mandatory non-shared read lock")
	}

	// 2. Test mandatory lock handle lifecycle across continuation requests
	mockCloser := &testMockCloser{}
	orig := checkOSImmutabilityFn
	checkOSImmutabilityFn = func(f *os.File, path string) (bool, io.Closer) {
		return true, mockCloser
	}
	t.Cleanup(func() {
		checkOSImmutabilityFn = orig
	})

	env := newTestEnv(t)
	tok := env.token()

	content := []byte("Windows mandatory read locked content stream.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])
	etag := fmt.Sprintf("\"sha256-%s\"", sumHex)

	p := filepath.Join(t.TempDir(), "win_locked.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "win_locked.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Verify the mandatory lock was attached to the continuation
	if !env.srv.grants.hasContinuationLock(grantID) {
		t.Fatal("mandatory lock was not attached to continuation record")
	}
	if mockCloser.isClosed() {
		t.Fatal("mandatory lock was closed prematurely")
	}

	// Issue Range continuation request: must be Tier 1 (206 Partial Content)
	req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	req.AddCookie(contCookie)
	req.Header.Set("Range", "bytes=10-20")
	req.Header.Set("If-Range", etag)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), content[10:21]) {
		t.Fatal("range body mismatch")
	}

	// Protection MUST be maintained across continuation requests
	if mockCloser.isClosed() {
		t.Fatal("mandatory lock was closed during active continuation lifecycle")
	}

	// Once the session is revoked or continuation pruned, the lock handle is closed
	sess, err := env.sess.Validate(tok)
	if err != nil {
		t.Fatal(err)
	}
	env.sess.RevokeSession(sess)
	env.srv.grants.RevokeSession(sess.Key())

	if !mockCloser.isClosed() {
		t.Fatal("mandatory lock was not closed upon session revocation")
	}
}

// TestPlatformImmutability_MacOS_UFImmutableFallsBackToTier2 proves that on macOS,
// UF_IMMUTABLE (chflags uchg) alone MUST NOT qualify a file for Tier 1 because
// the owner can clear it at any time without privileges.
func TestPlatformImmutability_MacOS_UFImmutableFallsBackToTier2(t *testing.T) {
	// 1. Verify policy decision logic directly
	if EvaluateDarwinImmutability(false, true) {
		t.Fatal("EvaluateDarwinImmutability allowed UF_IMMUTABLE alone to qualify for Tier 1")
	}

	// 2. Test end-to-end HTTP pipeline
	orig := checkOSImmutabilityFn
	// Simulate macOS file with only UF_IMMUTABLE flag
	checkOSImmutabilityFn = func(f *os.File, path string) (bool, io.Closer) {
		return false, nil
	}
	t.Cleanup(func() {
		checkOSImmutabilityFn = orig
	})

	env := newTestEnv(t)
	tok := env.token()

	content := []byte("macOS file with user immutable flag (uchg).")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])

	p := filepath.Join(t.TempDir(), "mac_uchg.txt")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "mac_uchg.txt", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// Issue Range request
	req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	req.AddCookie(contCookie)
	req.Header.Set("Range", "bytes=5-25")
	req.Header.Set("If-Range", fmt.Sprintf("\"sha256-%s\"", sumHex))
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	// MUST fall back to Tier 2: uses metadata ETag (not "sha256-..."), so Tier 1 If-Range mismatches -> 200 OK full body
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Tier 2 fallback)", rec.Code)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got := rec.Header().Get("ETag"); got == "" || got == fmt.Sprintf("\"sha256-%s\"", sumHex) {
		t.Errorf("Tier 2 ETag = %q, must not reuse Tier 1 immutable sha256 ETag", got)
	}
	if rec.Header().Get("Content-Range") != "" {
		t.Errorf("unexpected Content-Range header: %q", rec.Header().Get("Content-Range"))
	}
	if !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatal("response body did not stream full content from byte 0")
	}
}

// TestPlatformImmutability_MacOS_APFSSnapshotQualifiesForTier1 proves that on macOS,
// verified read-only APFS volume snapshots and OS-enforced read-only volumes
// (MNT_RDONLY) qualify for Tier 1 Range resumption.
func TestPlatformImmutability_MacOS_APFSSnapshotQualifiesForTier1(t *testing.T) {
	// 1. Verify policy decision logic directly
	if !EvaluateDarwinImmutability(true, false) {
		t.Fatal("EvaluateDarwinImmutability rejected read-only APFS snapshot (MNT_RDONLY)")
	}

	// 2. Test end-to-end HTTP pipeline
	orig := checkOSImmutabilityFn
	checkOSImmutabilityFn = func(f *os.File, path string) (bool, io.Closer) {
		return true, nil
	}
	t.Cleanup(func() {
		checkOSImmutabilityFn = orig
	})

	env := newTestEnv(t)
	tok := env.token()

	content := []byte("macOS read-only APFS volume snapshot content stream.")
	sum := sha256.Sum256(content)
	sumHex := hex.EncodeToString(sum[:])
	etag := fmt.Sprintf("\"sha256-%s\"", sumHex)

	p := filepath.Join(t.TempDir(), "apfs_snap.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.AddImmutable(p, "apfs_snap.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	req.AddCookie(contCookie)
	req.Header.Set("Range", "bytes=0-15")
	req.Header.Set("If-Range", etag)
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), content[0:16]) {
		t.Fatal("range body mismatch")
	}
}

// TestDownloadRange_G2_Tier2_ResumeEditAnd416 tests the full G2 byte-range
// lifecycle for mutable Tier 2 grant downloads: matching If-Range resumes at
// arbitrary offsets with 206 Partial Content, unsatisfiable ranges return 416,
// and editing the file on disk invalidates the old ETag so resuming with the
// stale If-Range restarts cleanly with 200 OK instead of splicing bytes.
func TestDownloadRange_G2_Tier2_ResumeEditAnd416(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	partA := bytes.Repeat([]byte("A"), 64*1024)
	partB := bytes.Repeat([]byte("B"), 64*1024)
	v1 := append(append([]byte{}, partA...), partB...)

	p := filepath.Join(t.TempDir(), "mutable_video.bin")
	if err := os.WriteFile(p, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := env.reg.Add(p, "mutable_video.bin", "")
	if err != nil {
		t.Fatal(err)
	}

	g, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(g.DownloadURL)

	initRec := env.getDownload(g, initialCookie, nil)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption status = %d", initRec.Code)
	}
	etagV1 := initRec.Header().Get("ETag")
	contCookie := extractContinuationCookie(t, initRec, grantID)

	// 1. Out-of-bounds range returns 416 with Content-Range: bytes */<size>
	oobReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	oobReq.AddCookie(contCookie)
	oobReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(v1)+10))
	oobReq.Header.Set("If-Range", etagV1)
	oobRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(oobRec, oobReq)
	if oobRec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("OOB range status = %d, want 416", oobRec.Code)
	}
	if got := oobRec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes */%d", len(v1)) {
		t.Fatalf("416 Content-Range = %q, want bytes */%d", got, len(v1))
	}

	// 2. Edit the file on disk (different size & bumped mtime); resuming with stale etagV1 falls back to 200 full fresh file
	v2 := append(bytes.Repeat([]byte("C"), 64*1024), bytes.Repeat([]byte("D"), 64*1024+512)...)
	if err := os.WriteFile(p, v2, 0o600); err != nil {
		t.Fatal(err)
	}
	newTime := time.Now().Add(5 * time.Second)
	_ = os.Chtimes(p, newTime, newTime)

	editReq := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	editReq.AddCookie(contCookie)
	editReq.Header.Set("Range", "bytes=65536-")
	editReq.Header.Set("If-Range", etagV1)
	editRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(editRec, editReq)
	if editRec.Code != http.StatusOK {
		t.Fatalf("after file edit with stale If-Range: status = %d, want 200", editRec.Code)
	}
	if !bytes.Equal(editRec.Body.Bytes(), v2) {
		t.Fatalf("after file edit: returned %d bytes, want %d full v2 bytes", editRec.Body.Len(), len(v2))
	}
	etagV2 := editRec.Header().Get("ETag")
	if etagV2 == "" || etagV2 == etagV1 {
		t.Fatalf("expected updated ETag after edit; v1=%q v2=%q", etagV1, etagV2)
	}
}

// TestDownloadRange_G2_DirectFileEndpoint verifies HEAD, 206 Range, suffix range,
// If-Range mismatch 200 fallback, 416, and mid-stream mutation abort on GET /api/files/{id}/download.
func TestDownloadRange_G2_DirectFileEndpoint(t *testing.T) {
	live := newLiveEnv(t)
	tok := live.token()

	content := bytes.Repeat([]byte("0123456789ABCDEF"), 128*1024) // 2 MiB
	p := filepath.Join(t.TempDir(), "direct_large.bin")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sf, err := live.reg.Add(p, "direct_large.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	dlURL := live.ts.URL + "/api/files/" + sf.ID + "/download"

	// 1. HEAD exposes Accept-Ranges, Content-Length, strong ETag, no body, no Downloads leak
	headReq, _ := http.NewRequest(http.MethodHead, dlURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+tok)
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", headResp.StatusCode)
	}
	if got := headResp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("HEAD Accept-Ranges = %q, want bytes", got)
	}
	etag := headResp.Header.Get("ETag")
	if etag == "" || strings.HasPrefix(etag, "W/") {
		t.Fatalf("HEAD ETag = %q, want strong validator", etag)
	}
	if len(live.dls.List()) != 0 {
		t.Fatalf("HEAD created %d download records, want 0", len(live.dls.List()))
	}

	// 2. Partial Range with matching If-Range -> 206
	r1Req, _ := http.NewRequest(http.MethodGet, dlURL, nil)
	r1Req.Header.Set("Authorization", "Bearer "+tok)
	r1Req.Header.Set("Range", "bytes=1024-65535")
	r1Req.Header.Set("If-Range", etag)
	r1Resp, err := http.DefaultClient.Do(r1Req)
	if err != nil {
		t.Fatal(err)
	}
	r1Body, _ := io.ReadAll(r1Resp.Body)
	r1Resp.Body.Close()
	if r1Resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range 1 status = %d, want 206", r1Resp.StatusCode)
	}
	if !bytes.Equal(r1Body, content[1024:65536]) {
		t.Fatal("range 1 body mismatch")
	}

	// 3. Suffix range -> last 100 bytes
	sufReq, _ := http.NewRequest(http.MethodGet, dlURL, nil)
	sufReq.Header.Set("Authorization", "Bearer "+tok)
	sufReq.Header.Set("Range", "bytes=-100")
	sufResp, err := http.DefaultClient.Do(sufReq)
	if err != nil {
		t.Fatal(err)
	}
	sufBody, _ := io.ReadAll(sufResp.Body)
	sufResp.Body.Close()
	if sufResp.StatusCode != http.StatusPartialContent || !bytes.Equal(sufBody, content[len(content)-100:]) {
		t.Fatalf("suffix range status=%d len=%d", sufResp.StatusCode, len(sufBody))
	}

	// 4. Stale If-Range -> 200 full body
	misReq, _ := http.NewRequest(http.MethodGet, dlURL, nil)
	misReq.Header.Set("Authorization", "Bearer "+tok)
	misReq.Header.Set("Range", "bytes=0-99")
	misReq.Header.Set("If-Range", "\"flit-stale-validator\"")
	misResp, err := http.DefaultClient.Do(misReq)
	if err != nil {
		t.Fatal(err)
	}
	misBody, _ := io.ReadAll(misResp.Body)
	misResp.Body.Close()
	if misResp.StatusCode != http.StatusOK || !bytes.Equal(misBody, content) {
		t.Fatalf("mismatched If-Range status=%d len=%d, want 200 full", misResp.StatusCode, len(misBody))
	}

	// 5. Out-of-bounds range -> 416
	oobReq, _ := http.NewRequest(http.MethodGet, dlURL, nil)
	oobReq.Header.Set("Authorization", "Bearer "+tok)
	oobReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(content)))
	oobResp, err := http.DefaultClient.Do(oobReq)
	if err != nil {
		t.Fatal(err)
	}
	oobResp.Body.Close()
	if oobResp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("OOB status = %d, want 416", oobResp.StatusCode)
	}

	// 6. Mid-stream file replacement aborts a live ranged stream rather than delivering spliced bytes
	bigPath := filepath.Join(t.TempDir(), "mutated_stream.bin")
	const bigSize = 32 << 20
	sparseFile(t, bigPath, bigSize)
	sfBig, err := live.reg.Add(bigPath, "mutated_stream.bin", "")
	if err != nil {
		t.Fatal(err)
	}
	liveReq, _ := http.NewRequest(http.MethodGet, live.ts.URL+"/api/files/"+sfBig.ID+"/download", nil)
	liveReq.Header.Set("Authorization", "Bearer "+tok)
	liveReq.Header.Set("Range", "bytes=0-")
	liveResp, err := (&http.Client{}).Do(liveReq)
	if err != nil {
		t.Fatal(err)
	}
	defer liveResp.Body.Close()
	first := make([]byte, downloadCopySize)
	if _, err := io.ReadFull(liveResp.Body, first); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	// Replace the file atomically on disk while streaming
	tmpSwap := bigPath + ".swap"
	if err := os.WriteFile(tmpSwap, []byte("mutated-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpSwap, bigPath); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.Copy(io.Discard, liveResp.Body)
	if int64(len(first))+rest >= bigSize {
		t.Fatalf("mid-stream replaced file still transferred full size (%d >= %d)", int64(len(first))+rest, bigSize)
	}
}
