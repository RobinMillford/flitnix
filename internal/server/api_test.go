package server

import (
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

// ---- test clock ----

// fakeClock is a deterministic clock for session idle-expiry tests: Advance
// moves time, so expiry is exercised without real sleeps (the session
// manager clamps IdleTimeout to >= 1s, making real-sleep expiry tests
// unreliable).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---- test environment ----

// testEnv is a fully wired server (page routes + authenticated API) over a
// temporary receive directory, driven through the handler directly. The
// sessions' clock is injected, so expiry tests are deterministic.
type testEnv struct {
	t     *testing.T
	sess  *session.Manager
	trans *transfer.Service
	reg   *transfer.Registry
	store *transfer.Store
	clock *fakeClock
	srv   *Server
}

// newTestEnv builds a complete environment. The idle timeout uses the
// session package's default; expiry tests Advance the fake clock instead of
// sleeping.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	clock := newFakeClock()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("cannot create receive store: %v", err)
	}
	env := &testEnv{
		t:     t,
		clock: clock,
		sess:  session.NewManager(session.Config{Now: clock.Now}),
		store: store,
	}
	env.trans = transfer.NewService(transfer.Config{Store: store})
	env.reg = transfer.NewRegistry()
	env.srv = New(Config{Services: &Services{
		Sessions:  env.sess,
		Transfers: env.trans,
		Registry:  env.reg,
	}})
	return env
}

// token creates a fresh session and returns its raw token. The token only
// lives in test memory; assertions below address it as "the bearer token".
func (e *testEnv) token() string {
	e.t.Helper()
	s, err := e.sess.Create()
	if err != nil {
		e.t.Fatalf("cannot create session: %v", err)
	}
	return s.Token()
}

// tokenKey returns the owning session's key for direct service calls
// (approve/decide from the desktop side, file registrations).
func (e *testEnv) tokenKey(tok string) string {
	e.t.Helper()
	s, err := e.sess.Validate(tok)
	if err != nil {
		e.t.Fatalf("session vanished: %v", err)
	}
	return s.Key()
}

// do issues one request against the handler with a "Bearer <token>"
// Authorization header. An empty token omits the header entirely.
func (e *testEnv) do(method, target, token string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var authValue string
	if token != "" {
		authValue = "Bearer " + token
	}
	return e.doHeader(method, target, authValue, body)
}

// doHeader issues one request with an explicit Authorization header value
// ("" omits it). It exists to exercise malformed header forms.
func (e *testEnv) doHeader(method, target, authValue string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if authValue != "" {
		req.Header.Set("Authorization", authValue)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// declare performs the metadata phase and returns the parsed record.
func (e *testEnv) declare(tok, name string, size int64) uploadJSON {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/uploads", tok, []byte(fmt.Sprintf(
		`{"name":%q,"size":%d}`, name, size)))
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("declare %q: got %d (body %s), want 201", name, rec.Code, rec.Body.String())
	}
	var got uploadJSON
	decodeBody(e.t, rec, &got)
	return got
}

// approve is the desktop side's decision, made directly on the transfer
// service (the future Approval Wails binding calls this).
func (e *testEnv) approve(tok, id string) {
	e.t.Helper()
	if _, err := e.trans.Decide(id, true); err != nil {
		e.t.Fatalf("cannot approve upload %s: %v", id, err)
	}
}

// contentPhase performs both phases: declare, approve, PUT. Returns the
// record and the parsed success body.
func (e *testEnv) contentPhase(tok, name string, content []byte) (uploadJSON, uploadResultJSON) {
	e.t.Helper()
	rec := e.do(http.MethodPost, "/api/uploads", tok, []byte(fmt.Sprintf(
		`{"name":%q,"size":%d}`, name, len(content))))
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("declare: got %d (body %s), want 201", rec.Code, rec.Body.String())
	}
	var up uploadJSON
	decodeBody(e.t, rec, &up)
	e.approve(tok, up.ID)

	uri := "/api/uploads/" + up.ID + "/content"
	rec = e.do(http.MethodPut, uri, tok, content)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("content upload: got %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	var res uploadResultJSON
	decodeBody(e.t, rec, &res)
	return up, res
}

// statusJSON is the parsed body of GET /api/uploads/{id}.
func statusJSON(t *testing.T, rec *httptest.ResponseRecorder) uploadJSON {
	t.Helper()
	var up uploadJSON
	decodeBody(t, rec, &up)
	return up
}

// pollUploadState repeatedly requests the record until it reaches the
// wanted state, failing on timeouts and wrong terminal states. Used by the
// real-server tests (their handlers run outside recorder calls, so polling
// over the record service keeps them observation-driven).
func pollUploadState(t *testing.T, svc *transfer.Service, id string, want transfer.State, tokenKey string) transfer.Upload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		u, ok := svc.Get(id)
		if !ok {
			t.Fatalf("upload %s vanished", id)
		}
		if u.State == want && u.Session == tokenKey {
			return u
		}
		if isTerminalState(u.State) && u.State != want {
			// Preserve: the failing assert names the actual end state.
			t.Fatalf("upload %s ended in %q (reason=%q); want %q",
				id, u.State, u.Reason, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for upload %s to reach %q", id, want)
	return transfer.Upload{} // unreachable
}

// isTerminalState admits every end state of the transfer machine.
func isTerminalState(s transfer.State) bool {
	switch s {
	case transfer.StateCompleted, transfer.StateFailed, transfer.StateCanceled, transfer.StateRejected:
		return true
	default:
		return false
	}
}

// receiveDirNames lists the receive directory's entries, tolerating a
// directory that was never created.
func receiveDirNames(t *testing.T, st *transfer.Store) []string {
	t.Helper()
	entries, err := os.ReadDir(st.Dir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read receive directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, en := range entries {
		names = append(names, en.Name())
	}
	return names
}

// writeSharedFile creates a source file in a temporary directory and
// registers it with the environment's registry, returning the registry
// record.
func (e *testEnv) shareFile(dir, name string, content []byte) transfer.SharedFile {
	e.t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		e.t.Fatalf("cannot create shared file: %v", err)
	}
	sf, err := e.reg.Add(path, name, "")
	if err != nil {
		e.t.Fatalf("registry.Add failed: %v", err)
	}
	if sf.ID == "" {
		e.t.Fatal("registry.Add produced an empty id")
	}
	return sf
}

// ---- authentication ----

func TestAPITokenRequired(t *testing.T) {
	env := newTestEnv(t)

	tests := []struct {
		name string
		auth string // Authorization header value to attempt
	}{
		{"no header", ""},
		{"garbage scheme", "Token abc"},
		{"bearer with no token", "Bearer "},
		{"bearer with junk token", "Bearer definitely-not-a-real-token"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.doHeader(http.MethodGet, "/api/files", tc.auth, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("GET /api/files with %q header returned %d (body %s), want 401",
					tc.auth, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAPIExpiredToken(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	// Within the window the request succeeds.
	if rec := env.do(http.MethodGet, "/api/files", tok, nil); rec.Code != http.StatusOK {
		t.Fatalf("fresh token got %d (body %s), want 200", rec.Code, rec.Body.String())
	}

	// Beyond the idle window it is rejected.
	env.clock.Advance(session.DefaultIdleTimeout + time.Minute)
	rec := env.do(http.MethodGet, "/api/files", tok, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token got %d (body %s), want 401", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"unauthorized"`) {
		t.Errorf("expired-token response %q does not carry the generic body", rec.Body.String())
	}
}

func TestAPIRevokedToken(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	if rec := env.do(http.MethodGet, "/api/files", tok, nil); rec.Code != http.StatusOK {
		t.Fatalf("fresh token got %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	if err := env.sess.Revoke(tok); err != nil {
		t.Fatalf("revoke failed: %v", err)
	}
	if rec := env.do(http.MethodGet, "/api/files", tok, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token got %d (body %s), want 401", rec.Code, rec.Body.String())
	}
}

func TestAPIClosedManager(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	env.sess.Close()

	if rec := env.do(http.MethodGet, "/api/files", tok, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("token after manager close got %d (body %s), want 401", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"a.txt","size":1}`)); rec.Code != http.StatusUnauthorized {
		t.Errorf("declare after manager close got %d, want 401", rec.Code)
	}
}

// ---- upload metadata ----

func TestAPIMetadataValidation(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	t.Run("malformed JSON", func(t *testing.T) {
		rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("malformed body got %d (body %s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown field", func(t *testing.T) {
		rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"a.txt","size":1,"extra":true}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("unknown-field body got %d (body %s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty body", func(t *testing.T) {
		rec := env.do(http.MethodPost, "/api/uploads", tok, []byte{})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("empty body got %d (body %s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing name", func(t *testing.T) {
		rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"size":1}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("missing name got %d (body %s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("negative size", func(t *testing.T) {
		rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"a.txt","size":-1}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("negative size got %d (body %s), want 400", rec.Code, rec.Body.String())
		}
	})

	t.Run("path traversal names", func(t *testing.T) {
		for _, name := range []string{
			"../secret.txt",
			"/etc/passwd",
			"sub/dir.txt",
			".",
			"..",
		} {
			rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(fmt.Sprintf(`{"name":%q,"size":1}`, name)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("traversal %q got %d (body %s), want 400", name, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("oversize metadata body", func(t *testing.T) {
		long := strings.Repeat("a", maxMetadataBytes+64)
		rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(fmt.Sprintf(`{"name":%q,"size":1}`, long)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("oversize body got %d (len %d), want 413", rec.Code, len(long))
		}
	})
}

// ---- upload happy path ----

func TestAPIUploadTwoPhaseHappyPath(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	key := env.tokenKey(tok)
	if tok == "" {
		t.Fatal("empty token")
	}

	// Phase 1: metadata.
	rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"photo.jpg","size":11}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("declare got %d (body %s), want 201", rec.Code, rec.Body.String())
	}
	up := statusJSON(t, rec)
	if up.ID == "" {
		t.Fatal("declared record has an empty id")
	}
	if up.Name != "photo.jpg" || up.Size != 11 {
		t.Errorf("declared record returned name=%q size=%d, want photo.jpg/11", up.Name, up.Size)
	}
	if up.State != "pending" {
		t.Errorf("declared record returned state %q, want pending", up.State)
	}

	// Status while pending (phone polling).
	rec = env.do(http.MethodGet, "/api/uploads/"+up.ID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status poll got %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	if u := statusJSON(t, rec); u.State != "pending" || u.Uploaded != 0 {
		t.Errorf("status poll returned state=%q uploaded=%d; want pending/0", u.State, u.Uploaded)
	}

	// Phase 2 is broken without the desktop's decision.
	rec = env.do(http.MethodPut, "/api/uploads/"+up.ID+"/content", tok, []byte("early data\n"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("un-approved content upload got %d (body %s), want 409", rec.Code, rec.Body.String())
	}

	// Desktop approves.
	env.approve(tok, up.ID)

	// Content goes through.
	body := []byte("hello world")
	rec = env.do(http.MethodPut, "/api/uploads/"+up.ID+"/content", tok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("content upload got %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	var res uploadResultJSON
	decodeBody(t, rec, &res)
	if res.Written != int64(len(body)) {
		t.Errorf("content upload reported written=%d, want %d", res.Written, len(body))
	}
	sum := sha256.Sum256(body)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("content upload digest %q does not match the body", res.SHA256)
	}

	// On disk, exactly where the record says (the path comes from the
	// service record; the JSON view deliberately omits it).
	su, ok := env.trans.Get(up.ID)
	if !ok {
		t.Fatal("record vanished before the disk check")
	}
	if su.Published == "" {
		t.Fatal("completed record did not publish a path")
	}
	got, err := os.ReadFile(su.Published)
	if err != nil {
		t.Fatalf("published file unread: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("published content %q, want %q", got, body)
	}

	// Status flips to completed.
	rec = env.do(http.MethodGet, "/api/uploads/"+up.ID, tok, nil)
	if u := statusJSON(t, rec); u.State != "completed" || u.Uploaded != int64(len(body)) {
		t.Errorf("after upload the record shows state=%q uploaded=%d; want completed/%d",
			u.State, u.Uploaded, len(body))
	}

	// No leftover part files.
	if left := receiveDirNames(t, env.store); len(left) != 1 {
		t.Errorf("receive directory holds %v, want exactly the published file", left)
	}

	// The owning session key was recorded server-side (never visible in JSON).
	if su.Session != key {
		t.Errorf("record session %q, want the owning key %q", su.Session, key)
	}
}

func TestAPIUploadLargeStream(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	// 8 MiB is comfortably beyond the 1 MiB progress stride and large
	// enough to prove stream-loop correctness without being slow.
	content := make([]byte, 8<<20)
	for i := range content {
		content[i] = byte(i % 251)
	}

	up, res := env.contentPhase(tok, "large.bin", content)
	if res.Written != int64(len(content)) {
		t.Fatalf("written=%d, want %d", res.Written, len(content))
	}
	su, ok := env.trans.Get(up.ID)
	if !ok || su.Published == "" {
		t.Fatalf("completed record lacked a published path (state=%q)", su.State)
	}
	got, err := os.ReadFile(su.Published)
	if err != nil {
		t.Fatalf("published file unread: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("on-disk content differs from what was uploaded")
	}
	sum := sha256.Sum256(content)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 %q does not match the supplied body", res.SHA256)
	}
}

// ---- upload failure paths ----

func TestAPIUploadOverrun(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"big.bin","size":4}`))
	up := statusJSON(t, rec)
	env.approve(tok, up.ID)

	// Five bytes into a four-byte allocation.
	rec = env.do(http.MethodPut, "/api/uploads/"+up.ID+"/content", tok, []byte("12345"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overrunning body got %d (body %s), want 400", rec.Code, rec.Body.String())
	}

	// The record failed, and the receive directory was cleaned.
	u, ok := env.trans.Get(up.ID)
	if !ok {
		t.Fatal("record vanished before its state could be checked")
	}
	if u.State != transfer.StateFailed {
		t.Fatalf("record state %q, want failed", u.State)
	}
	if names := receiveDirNames(t, env.store); len(names) != 0 {
		t.Errorf("receive directory is not empty after a failed upload: %v", names)
	}
}

func TestAPIUploadTruncated(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"cut.bin","size":10}`))
	up := statusJSON(t, rec)
	env.approve(tok, up.ID)

	// Fewer bytes than promised, then a clean EOF.
	rec = env.do(http.MethodPut, "/api/uploads/"+up.ID+"/content", tok, []byte("short"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("truncated body got %d (body %s), want 400", rec.Code, rec.Body.String())
	}

	u, ok := env.trans.Get(up.ID)
	if !ok {
		t.Fatal("record vanished before its state could be checked")
	}
	if u.State != transfer.StateFailed {
		t.Fatalf("record state %q, want failed", u.State)
	}
	if names := receiveDirNames(t, env.store); len(names) != 0 {
		t.Errorf("receive directory is not empty after a failed upload: %v", names)
	}
}

func TestAPISessionIsolation(t *testing.T) {
	env := newTestEnv(t)
	tokA := env.token()
	tokB := env.token()

	up := env.declare(tokA, "private.bin", 4)

	// Session B cannot see A's record.
	rec := env.do(http.MethodGet, "/api/uploads/"+up.ID, tokB, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("other session's status read got %d (body %s), want 404", rec.Code, rec.Body.String())
	}

	// Session B cannot put content to A's upload.
	if _, err := env.trans.Decide(up.ID, true); err != nil {
		t.Fatalf("approval failed: %v", err)
	}
	rec = env.do(http.MethodPut, "/api/uploads/"+up.ID+"/content", tokB, []byte("nope"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("other session's content upload got %d (body %s), want 404", rec.Code, rec.Body.String())
	}

	// Nothing landed.
	if names := receiveDirNames(t, env.store); len(names) != 0 {
		t.Errorf("foreign content landed in the receive directory: %v", names)
	}
}

func TestAPIUploadCanceledByDesktop(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	up := env.declare(tok, "victim.bin", 8)
	env.approve(tok, up.ID)

	if err := env.trans.Cancel(up.ID); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}

	rec := env.do(http.MethodPut, "/api/uploads/"+up.ID+"/content", tok, []byte("deadbytes"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("content to a canceled upload got %d (body %s), want 409", rec.Code, rec.Body.String())
	}
	if names := receiveDirNames(t, env.store); len(names) != 0 {
		t.Errorf("canceled upload left files: %v", names)
	}
}

func TestAPIUploadBadContentRoutes(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	t.Run("unknown id status", func(t *testing.T) {
		rec := env.do(http.MethodGet, "/api/uploads/no-such-id", tok, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("unknown-id status got %d, want 404", rec.Code)
		}
	})

	t.Run("unknown id content", func(t *testing.T) {
		rec := env.do(http.MethodPut, "/api/uploads/no-such-id/content", tok, []byte("x"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("unknown-id content got %d, want 404", rec.Code)
		}
	})

	t.Run("second stream refused", func(t *testing.T) {
		up, _ := env.contentPhase(tok, "once.txt", []byte("first"))
		if u, _ := env.trans.Get(up.ID); u.State != transfer.StateCompleted {
			t.Fatalf("first upload did not complete: %s", u.State)
		}
		rec := env.do(http.MethodPut, "/api/uploads/"+up.ID+"/content", tok, []byte("second"))
		if rec.Code != http.StatusConflict {
			t.Errorf("second content stream got %d (body %s), want 409", rec.Code, rec.Body.String())
		}
	})
}

// ---- desktop-shared files ----

func TestAPIFilesListAndDownload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	dir := t.TempDir()
	content := []byte("shared file body")
	sf := env.shareFile(dir, "handoff.txt", content)

	// List.
	rec := env.do(http.MethodGet, "/api/files", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list got %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	var files []fileJSON
	decodeBody(t, rec, &files)
	if len(files) != 1 {
		t.Fatalf("list returned %d files, want 1", len(files))
	}
	if files[0].Name != "handoff.txt" || files[0].Size != int64(len(content)) {
		t.Errorf("listed file %+v, want name=handoff.txt size=%d", files[0], len(content))
	}
	if files[0].ID != sf.ID {
		t.Errorf("listed id %q, want the registered %q", files[0].ID, sf.ID)
	}

	// Download carries the full headers and exact body.
	rec = env.do(http.MethodGet, "/api/files/"+sf.ID+"/download", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download got %d, want 200", rec.Code)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len(content)) {
		t.Errorf("Content-Length %q, want %d", cl, len(content))
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "handoff.txt") {
		t.Errorf("Content-Disposition %q, want attachment with the filename", cd)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q, want octet-stream", ct)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, content) {
		t.Errorf("downloaded %q, want %q", got, content)
	}

	// Unknown ID on both endpoints.
	rec = env.do(http.MethodGet, "/api/files/no-such-id/download", tok, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown-id download got %d, want 404", rec.Code)
	}

	// After the file vanishes from disk, the download is honest.
	if err := os.Remove(filepath.Join(dir, "handoff.txt")); err != nil {
		t.Fatalf("remove test file: %v", err)
	}
	rec = env.do(http.MethodGet, "/api/files/"+sf.ID+"/download", tok, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("vanished-file download got %d, want 404", rec.Code)
	}
}

// ---- routing ----

func TestAPIUnknownPathsAndMethods(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	t.Run("unknown path with token", func(t *testing.T) {
		rec := env.do(http.MethodGet, "/api/unknown", tok, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/unknown got %d, want 404", rec.Code)
		}
	})

	t.Run("unknown path without token", func(t *testing.T) {
		// Authentication runs before the API router, so even unknown
		// /api/* paths demand a token.
		rec := env.do(http.MethodGet, "/api/unknown", "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET /api/unknown without token got %d, want 401", rec.Code)
		}
	})

	t.Run("method mismatch on registered path", func(t *testing.T) {
		rec := env.do(http.MethodDelete, "/api/uploads/a-id/content", tok, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("DELETE on a GET path got %d (body %s), want 405", rec.Code, rec.Body.String())
		}
		rec = env.do(http.MethodPost, "/api/files", tok, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST /api/files got %d, want 405", rec.Code)
		}
	})

	t.Run("healthz stays unauthenticated", func(t *testing.T) {
		rec := env.do(http.MethodGet, "/healthz", "", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /healthz got %d (body %s), want 200", rec.Code, rec.Body.String())
		}
	})
}

func TestPageOnlyServerHasNoAPI(t *testing.T) {
	srv := New(Config{}) // nil Services
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/uploads", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("page-only server's /api path got %d (body %s), want 404", rec.Code, rec.Body.String())
	}

	// The page routes still work.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("page-only server's /healthz got %d, want 200", rec.Code)
	}
}

// TestAPILargeHeaderTokenGoesThrough confirms a real 43-char token round
// trips with realistic download/list flows (fixture integration of the
// pieces above that only used raw tokens).
func TestAPIFullRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	tokB := env.token()

	// Session A receives a file.
	up, res := env.contentPhase(tok, "round.txt", []byte("round-trip"))
	if res.Written == 0 {
		t.Fatal("empty content result")
	}

	// Desktop shares the received file back out. The file path comes from
	// the record (which itself never leaves the server as JSON).
	su, ok := env.trans.Get(up.ID)
	if !ok {
		t.Fatal("record vanished")
	}
	sf, err := env.reg.Add(su.Published, su.Name, su.Session)
	if err != nil {
		t.Fatalf("share-back failed: %v", err)
	}

	// Session B lists and downloads it independently.
	rec := env.do(http.MethodGet, "/api/files", tokB, nil)
	var files []fileJSON
	decodeBody(t, rec, &files)
	if len(files) != 1 || files[0].ID != sf.ID {
		t.Fatalf("session B sees %v, want the shared file", files)
	}
	rec = env.do(http.MethodGet, "/api/files/"+sf.ID+"/download", tokB, nil)
	if got := rec.Body.String(); got != "round-trip" {
		t.Errorf("session B downloaded %q, want %q", got, "round-trip")
	}

	// The phone-published file's name kept its extension.
	if stem := strings.TrimSuffix(up.Name, filepath.Ext(up.Name)); stem == "" {
		t.Errorf("sanitized name lost its stem: %q", up.Name)
	}
}
