package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// ---- synthetic payloads and in-flight request helpers ----

// patternReader yields n deterministic bytes without ever holding them in
// memory, so "large" uploads cost no RAM in the test either.
type patternReader struct {
	remaining int64
	block     [64 << 10]byte
}

func newPatternReader(n int64, seed byte) *patternReader {
	p := &patternReader{remaining: n}
	for i := range p.block {
		p.block[i] = seed + byte(i*7)
	}
	return p
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}
	n := len(b)
	if int64(n) > p.remaining {
		n = int(p.remaining)
	}
	if n > len(p.block) {
		n = len(p.block)
	}
	copy(b[:n], p.block[:n])
	p.remaining -= int64(n)
	return n, nil
}

type putResult struct {
	code int
	body []byte
	err  error
}

// startPut begins a PUT whose body the test feeds through the returned pipe,
// so a transfer can be held open at any byte offset.
func startPut(url, tok string, size int64) (*io.PipeWriter, <-chan putResult) {
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPut, url, pr)
	out := make(chan putResult, 1)
	if err != nil {
		out <- putResult{err: err}
		return pw, out
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/octet-stream")
	go func() {
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			out <- putResult{err: err}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		out <- putResult{code: resp.StatusCode, body: body}
	}()
	return pw, out
}

func awaitPut(t *testing.T, ch <-chan putResult) putResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the PUT to finish")
		return putResult{}
	}
}

func (e *testEnv) createBatchOver(tok string, manifest transfer.BatchManifest) transfer.BatchView {
	e.t.Helper()
	body, _ := json.Marshal(manifest)
	rec := e.do(http.MethodPost, "/api/batches", tok, body)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("create batch = %d %s", rec.Code, rec.Body.String())
	}
	var v transfer.BatchView
	decodeBody(e.t, rec, &v)
	return v
}

func (e *testEnv) filesBatch(tok string, sizes ...int64) transfer.BatchView {
	e.t.Helper()
	items := make([]transfer.ManifestItem, len(sizes))
	for i, size := range sizes {
		items[i] = transfer.ManifestItem{RelativePath: fmt.Sprintf("file-%d.bin", i), Size: size}
	}
	return e.createBatchOver(tok, transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop, Kind: transfer.BatchKindFiles, Items: items,
	})
}

// approveBatch performs the desktop decision exactly like the app does:
// every upload record is decided atomically, then the coordinator releases
// the batch.
func (e *testEnv) approveBatch(key string, b transfer.BatchView) {
	e.t.Helper()
	if err := e.trans.DecideBatchItems(e.contentIDs(key, b), true); err != nil {
		e.t.Fatalf("decide batch items: %v", err)
	}
	if _, err := e.coord.ApproveBatch(key, b.ID); err != nil {
		e.t.Fatalf("approve batch: %v", err)
	}
}

func (e *testEnv) contentIDs(key string, b transfer.BatchView) []string {
	e.t.Helper()
	ids := make([]string, len(b.Items))
	for i, item := range b.Items {
		id, err := e.coord.ItemContentID(key, b.ID, item.ID)
		if err != nil {
			e.t.Fatal(err)
		}
		ids[i] = id
	}
	return ids
}

func contentURL(base string, b transfer.BatchView, item int) string {
	return base + "/api/batches/" + b.ID + "/items/" + b.Items[item].ID + "/content"
}

func waitPinned(t *testing.T, s *session.Session, n int32) {
	t.Helper()
	waitFor(t, fmt.Sprintf("session pinned by %d stream(s)", n), func() bool { return s.PinnedTransfers() == n })
}

func partFiles(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(d.Name(), ".part.") {
			found = append(found, path)
		}
		return nil
	})
	return found
}

func itemStates(v transfer.BatchView) []transfer.ItemState {
	out := make([]transfer.ItemState, len(v.Items))
	for i, item := range v.Items {
		out[i] = item.State
	}
	return out
}

func decodeCode(t *testing.T, rec *httptest.ResponseRecorder) (int, string, string) {
	t.Helper()
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body["error"], body["code"]
}

// ---- large transfers ----

func TestLargeSingleUploadStreamsToDiskWithBoundedMemory(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	const size = 128 << 20
	up := env.declare(tok, "movie.mkv", size)
	env.approve(tok, up.ID)

	hasher := sha256.New()
	body := io.TeeReader(newPatternReader(size, 3), hasher)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/uploads/"+up.ID+"/content", body)
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+tok)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var res uploadResultJSON
	_ = json.NewDecoder(resp.Body).Decode(&res)
	_ = resp.Body.Close()
	runtime.ReadMemStats(&after)
	if resp.StatusCode != http.StatusOK || res.Written != size {
		t.Fatalf("upload = %d %+v", resp.StatusCode, res)
	}
	if want := hex.EncodeToString(hasher.Sum(nil)); res.SHA256 != want {
		t.Fatalf("server digest %s != sent %s", res.SHA256, want)
	}
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 48<<20 {
		t.Fatalf("allocated %d MiB receiving a %d MiB file: the body is being buffered", delta>>20, size>>20)
	}
	got, _ := env.trans.Get(up.ID)
	info, err := os.Stat(got.Published)
	if err != nil || info.Size() != size {
		t.Fatalf("published file = %v, %v", info, err)
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("part files left behind: %v", left)
	}
}

func TestSequentialBatchOfLargeItemsCompletes(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	sizes := []int64{24 << 20, 0, 17<<20 + 3, 5}
	b := env.filesBatch(tok, sizes...)
	env.approveBatch(key, b)

	var total int64
	for i, size := range sizes {
		hasher := sha256.New()
		body := io.TeeReader(newPatternReader(size, byte(i)), hasher)
		req, _ := http.NewRequest(http.MethodPut, contentURL(ts.URL, b, i), body)
		req.ContentLength = size
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
		var res struct {
			Written int64              `json:"written"`
			SHA256  string             `json:"sha256"`
			Batch   transfer.BatchView `json:"batch"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || res.Written != size {
			t.Fatalf("item %d = %d %+v", i, resp.StatusCode, res)
		}
		if res.SHA256 != hex.EncodeToString(hasher.Sum(nil)) {
			t.Fatalf("item %d digest mismatch", i)
		}
		total += size
		if res.Batch.TransferredBytes != total {
			t.Fatalf("item %d: batch bytes = %d, want %d", i, res.Batch.TransferredBytes, total)
		}
		wantState := transfer.BatchStateQueued
		if i == len(sizes)-1 {
			wantState = transfer.BatchStateCompleted
		}
		if res.Batch.State != wantState {
			t.Fatalf("item %d: batch state %q, want %q", i, res.Batch.State, wantState)
		}
	}
	final, _ := env.coord.GetBatch(key, b.ID)
	for i, st := range itemStates(final) {
		if st != transfer.ItemStateCompleted {
			t.Fatalf("item %d = %q", i, st)
		}
	}
	for _, id := range env.contentIDs(key, b) {
		if u, _ := env.trans.Get(id); u.State != transfer.StateCompleted {
			t.Fatalf("upload record %s = %q", id, u.State)
		}
	}
}

func TestFolderBatchWithNestedPathsCompletes(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	files := map[string]int64{
		"top.txt":                  10,
		"2026/jan/big.bin":         9 << 20,
		"2026/jan/notes (v2).txt":  33,
		"2026/feb/deep/er/end.dat": 1,
	}
	var items []transfer.ManifestItem
	var order []string
	for path, size := range files {
		items = append(items, transfer.ManifestItem{RelativePath: path, Size: size})
		order = append(order, path)
	}
	b := env.createBatchOver(tok, transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop, Kind: transfer.BatchKindFolder, RootName: "Trip photos", Items: items,
	})
	if got := env.trans.ListPending(); len(got) != 0 {
		t.Fatalf("folder items surfaced as standalone approvals: %+v", got)
	}
	env.approveBatch(key, b)
	for i, item := range b.Items {
		pw, res := startPut(contentURL(ts.URL, b, i), tok, files[item.RelativePath])
		go func(path string) {
			_, _ = io.Copy(pw, newPatternReader(files[path], 9))
			_ = pw.Close()
		}(item.RelativePath)
		if r := awaitPut(t, res); r.err != nil || r.code != http.StatusOK {
			t.Fatalf("%s = %+v", item.RelativePath, r)
		}
	}
	_ = order
	roots, err := os.ReadDir(env.store.Dir())
	if err != nil || len(roots) != 1 {
		t.Fatalf("receive dir = %v, %v", roots, err)
	}
	root := filepath.Join(env.store.Dir(), roots[0].Name())
	if !strings.HasPrefix(roots[0].Name(), "Trip photos") {
		t.Fatalf("folder root = %q", roots[0].Name())
	}
	for path, size := range files {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || info.Size() != size {
			t.Fatalf("%s = %v, %v", path, info, err)
		}
	}
	if v, _ := env.coord.GetBatch(key, b.ID); v.State != transfer.BatchStateCompleted {
		t.Fatalf("folder batch = %q", v.State)
	}
}

func TestLargeBatchDoesNotExhaustStandalonePendingCapacity(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	sizes := make([]int64, 60)
	b := env.filesBatch(tok, sizes...)
	if len(b.Items) != 60 {
		t.Fatalf("items = %d", len(b.Items))
	}
	if pend := env.trans.ListPending(); len(pend) != 0 {
		t.Fatalf("batch items counted as standalone approvals: %d", len(pend))
	}
	env.declare(tok, "standalone.txt", 1) // still has room despite 60 batch items
}

// ---- session lifetime across long transfers ----

func TestUploadCrossingTheIdleThresholdKeepsTheSessionAndNextRequestsWork(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	sess, _ := env.sess.Validate(tok)
	b := env.filesBatch(tok, 8, 4)
	env.approveBatch(key, b)

	pw, res := startPut(contentURL(ts.URL, b, 0), tok, 8)
	if _, err := pw.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	waitPinned(t, sess, 1)

	// Three idle windows pass while the stream is open and nothing else
	// touches the session.
	env.clock.Advance(3 * session.DefaultIdleTimeout)
	if got := env.sess.Len(); got != 1 {
		t.Fatalf("session pruned under an in-flight transfer (Len=%d)", got)
	}
	if _, err := pw.Write([]byte("5678")); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	if r := awaitPut(t, res); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("long transfer = %+v", r)
	}

	// The transfer outlasted the idle window. The next batch item and the
	// status poll that follow it must not hit "session ended".
	rec := env.do(http.MethodGet, "/api/batches/"+b.ID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status poll right after a long upload = %d %s", rec.Code, rec.Body.String())
	}
	pw2, res2 := startPut(contentURL(ts.URL, b, 1), tok, 4)
	_, _ = pw2.Write([]byte("abcd"))
	_ = pw2.Close()
	if r := awaitPut(t, res2); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("second item after a long first upload = %+v", r)
	}
	if v, _ := env.coord.GetBatch(key, b.ID); v.State != transfer.BatchStateCompleted {
		t.Fatalf("batch = %q", v.State)
	}

	// Ordinary idle expiry is intact: with nothing running the session dies.
	env.clock.Advance(session.DefaultIdleTimeout + time.Second)
	rec = env.do(http.MethodGet, "/api/batches/"+b.ID, tok, nil)
	if code, _, c := decodeCode(t, rec); code != http.StatusUnauthorized || c != "session_expired" {
		t.Fatalf("idle session = %d code %q, want 401 session_expired", code, c)
	}
}

func TestUnauthorizedResponsesDistinguishExpiredRevokedAndUnknown(t *testing.T) {
	env := newTestEnv(t)
	expired := env.token()
	env.clock.Advance(session.DefaultIdleTimeout + time.Second)
	revoked := env.token()
	s, _ := env.sess.Validate(revoked)
	env.sess.RevokeSession(s)
	valid := env.token()

	for _, tc := range []struct{ name, tok, wantCode string }{
		{"expired", expired, "session_expired"},
		{"revoked", revoked, "session_revoked"},
		{"unknown", strings.Repeat("A", 43), "invalid_token"},
		{"malformed", "short", "invalid_token"},
	} {
		code, msg, c := decodeCode(t, env.do(http.MethodGet, "/api/sources", tc.tok, nil))
		if code != http.StatusUnauthorized || msg != "unauthorized" || c != tc.wantCode {
			t.Errorf("%s: %d error=%q code=%q, want 401 unauthorized %q", tc.name, code, msg, c, tc.wantCode)
		}
	}
	if rec := env.do(http.MethodGet, "/api/sources", valid, nil); rec.Code != http.StatusOK {
		t.Fatalf("live session = %d", rec.Code)
	}
}

func TestRevokedSessionStopsItsTransferWhileOthersContinue(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	a, b := env.token(), env.token()
	keyA, keyB := env.tokenKey(a), env.tokenKey(b)
	batchA := env.filesBatch(a, 8)
	batchB := env.filesBatch(b, 8)
	env.approveBatch(keyA, batchA)
	env.approveBatch(keyB, batchB)

	pwA, resA := startPut(contentURL(ts.URL, batchA, 0), a, 8)
	pwB, resB := startPut(contentURL(ts.URL, batchB, 0), b, 8)
	_, _ = pwA.Write([]byte("aaaa"))
	_, _ = pwB.Write([]byte("bbbb"))
	sessA, _ := env.sess.Validate(a)
	sessB, _ := env.sess.Validate(b)
	waitPinned(t, sessA, 1)
	waitPinned(t, sessB, 1)

	// The desktop ends A's session: its records are canceled, its stream dies.
	env.trans.CancelSession(keyA)
	_ = env.coord.CloseSession(keyA)
	env.sess.RevokeSession(sessA)
	_, _ = pwA.Write([]byte("aaaa"))
	_ = pwA.Close()
	if r := awaitPut(t, resA); r.err == nil && r.code == http.StatusOK {
		t.Fatalf("revoked session's upload still succeeded: %+v", r)
	}
	if rec := env.do(http.MethodGet, "/api/batches/"+batchA.ID, a, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token = %d", rec.Code)
	}

	_, _ = pwB.Write([]byte("bbbb"))
	_ = pwB.Close()
	if r := awaitPut(t, resB); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("independent session B = %+v", r)
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("part files left behind: %v", left)
	}
}

func TestPhoneAbortCleansUpAndTheQueueRecovers(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	sess, _ := env.sess.Validate(tok)
	b := env.filesBatch(tok, 8, 8, 8)
	env.approveBatch(key, b)
	ids := env.contentIDs(key, b)

	pw, res := startPut(contentURL(ts.URL, b, 0), tok, 8)
	_, _ = pw.Write([]byte("1234"))
	waitFor(t, "stream to start", func() bool { u, _ := env.trans.Get(ids[0]); return u.State == transfer.StateUploading })
	pw.CloseWithError(errors.New("phone aborted")) // user cancel / network drop
	if r := awaitPut(t, res); r.err == nil && r.code == http.StatusOK {
		t.Fatalf("aborted upload reported success: %+v", r)
	}
	waitFor(t, "failed state", func() bool { u, _ := env.trans.Get(ids[0]); return u.State == transfer.StateFailed })
	waitPinned(t, sess, 0)

	v, _ := env.coord.GetBatch(key, b.ID)
	want := []transfer.ItemState{transfer.ItemStateFailed, transfer.ItemStateCanceled, transfer.ItemStateCanceled}
	for i := range want {
		if v.Items[i].State != want[i] {
			t.Fatalf("items = %v, want %v", itemStates(v), want)
		}
	}
	if v.State != transfer.BatchStateFailed || v.TransferredBytes != 0 {
		t.Fatalf("batch = %q with %d bytes: partial bytes must not count", v.State, v.TransferredBytes)
	}
	for i, wantState := range []transfer.State{transfer.StateFailed, transfer.StateCanceled, transfer.StateCanceled} {
		if u, _ := env.trans.Get(ids[i]); u.State != wantState {
			t.Fatalf("upload %d = %q, want %q (no approved record may linger)", i, u.State, wantState)
		}
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("temporary file survived the abort: %v", left)
	}
	if entries, _ := os.ReadDir(env.store.Dir()); len(entries) != 0 {
		t.Fatalf("an incomplete file was published: %v", entries)
	}
	// The phone re-declares; the failed batch must not block the queue.
	b2 := env.filesBatch(tok, 4)
	env.approveBatch(key, b2)
	pw2, res2 := startPut(contentURL(ts.URL, b2, 0), tok, 4)
	_, _ = pw2.Write([]byte("done"))
	_ = pw2.Close()
	if r := awaitPut(t, res2); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("replacement batch after an abort = %+v", r)
	}
}

func TestDesktopCancelDuringStreamIsDeliberateAndClean(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	b := env.filesBatch(tok, 8, 8)
	env.approveBatch(key, b)
	ids := env.contentIDs(key, b)

	pw, res := startPut(contentURL(ts.URL, b, 0), tok, 8)
	_, _ = pw.Write([]byte("1234"))
	waitFor(t, "stream to start", func() bool { u, _ := env.trans.Get(ids[0]); return u.State == transfer.StateUploading })

	if _, err := env.coord.CancelBatch(key, b.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids { // the desktop (App.CancelBatch) withdraws every record
		_ = env.trans.Cancel(id)
	}
	go func() { _, _ = pw.Write([]byte("5678")); _ = pw.Close() }()
	r := awaitPut(t, res)
	if r.err == nil && r.code != http.StatusConflict {
		t.Fatalf("canceled stream answered %d, want 409", r.code)
	}
	waitFor(t, "canceled state", func() bool { u, _ := env.trans.Get(ids[0]); return u.State == transfer.StateCanceled })
	v, _ := env.coord.GetBatch(key, b.ID)
	if v.State != transfer.BatchStateCanceled {
		t.Fatalf("batch = %q: a deliberate cancel must stay canceled, not become failed", v.State)
	}
	for i, st := range itemStates(v) {
		if st != transfer.ItemStateCanceled {
			t.Fatalf("item %d = %q", i, st)
		}
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("temporary file survived the cancel: %v", left)
	}
	if u, _ := env.trans.Get(ids[1]); u.State != transfer.StateCanceled {
		t.Fatalf("unstarted record = %q", u.State)
	}
}

func TestStatusPollTroubleDoesNotDisturbAValidTransfer(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	b := env.filesBatch(tok, 8)
	env.approveBatch(key, b)
	sess, _ := env.sess.Validate(tok)

	pw, res := startPut(contentURL(ts.URL, b, 0), tok, 8)
	_, _ = pw.Write([]byte("1234"))
	waitPinned(t, sess, 1)

	// A flurry of failing or abandoned status polls from the same phone.
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var reqURL, auth string
			switch i % 3 {
			case 0:
				reqURL, auth = ts.URL+"/api/batches/does-not-exist", tok // 404
			case 1:
				reqURL, auth = ts.URL+"/api/batches/"+b.ID, "wrong-token" // 401 invalid token
			default:
				reqURL, auth = ts.URL+"/api/batches/"+b.ID, tok // abandoned mid-flight below
			}
			ctx, cancel := context.WithCancel(context.Background())
			if i%3 == 2 {
				cancel()
			} else {
				defer cancel()
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
			req.Header.Set("Authorization", "Bearer "+auth)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()

	if sess.Status() != session.StatusActive || sess.PinnedTransfers() != 1 {
		t.Fatalf("poll noise disturbed the session: %q pins=%d", sess.Status(), sess.PinnedTransfers())
	}
	_, _ = pw.Write([]byte("5678"))
	_ = pw.Close()
	if r := awaitPut(t, res); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("transfer after poll noise = %+v", r)
	}
	if v, _ := env.coord.GetBatch(key, b.ID); v.State != transfer.BatchStateCompleted || v.TransferredBytes != 8 {
		t.Fatalf("batch = %+v", v)
	}
}

func TestWrongItemContentRequestDoesNotStickTheQueue(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	key := env.tokenKey(tok)
	b := env.filesBatch(tok, 4, 4)
	env.approveBatch(key, b)

	rec := env.do(http.MethodPut, "/api/batches/"+b.ID+"/items/"+b.Items[1].ID+"/content", tok, []byte("2222"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("out-of-order item = %d, want 409", rec.Code)
	}
	snap, _ := env.coord.QueueStatus(key)
	if snap.ActiveLeaseID != "" || snap.Paused {
		t.Fatalf("a refused request left the queue stuck: %+v", snap)
	}
	for i, body := range []string{"1111", "2222"} {
		rec := env.do(http.MethodPut, "/api/batches/"+b.ID+"/items/"+b.Items[i].ID+"/content", tok, []byte(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("item %d after the stray request = %d %s", i, rec.Code, rec.Body.String())
		}
	}
}

// ---- cleanup racing with transfers; session independence ----

func TestSessionCleanupConcurrentWithActiveTransferCompletion(t *testing.T) {
	env := newTestEnv(t)
	var expired atomic.Int32
	env.sess = session.NewManager(session.Config{Now: env.clock.Now, OnExpire: func(key string) {
		expired.Add(1)
		env.trans.CancelSession(key)
		_ = env.coord.CloseSession(key)
	}})
	env.srv = New(Config{Services: &Services{
		Sessions: env.sess, Transfers: env.trans, Coordinator: env.coord, Registry: env.reg,
	}})
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	sess, _ := env.sess.Validate(tok)
	b := env.filesBatch(tok, 8)
	env.approveBatch(key, b)
	id := env.contentIDs(key, b)[0]

	pw, res := startPut(contentURL(ts.URL, b, 0), tok, 8)
	_, _ = pw.Write([]byte("1234"))
	waitPinned(t, sess, 1)

	env.clock.Advance(time.Hour) // six idle windows with the stream open
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = env.sess.Len()
					_ = env.sess.ActiveSessions()
					runtime.Gosched()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	_, _ = pw.Write([]byte("5678"))
	_ = pw.Close()
	r := awaitPut(t, res)
	close(stop)
	wg.Wait()
	if r.err != nil || r.code != http.StatusOK {
		t.Fatalf("transfer racing cleanup = %+v", r)
	}
	if expired.Load() != 0 {
		t.Fatalf("OnExpire fired %d times for a session with a live stream", expired.Load())
	}
	if u, _ := env.trans.Get(id); u.State != transfer.StateCompleted {
		t.Fatalf("upload = %q", u.State)
	}

	// Stale cleanup arriving later must not rewrite history.
	env.clock.Advance(2 * session.DefaultIdleTimeout)
	_ = env.sess.Len()
	if expired.Load() != 1 {
		t.Fatalf("idle session cleanup fired %d times, want 1", expired.Load())
	}
	if u, _ := env.trans.Get(id); u.State != transfer.StateCompleted {
		t.Fatalf("completed upload became %q after stale cleanup", u.State)
	}
	if v, err := env.coord.GetBatch(key, b.ID); err == nil && v.State != transfer.BatchStateCompleted {
		t.Fatalf("completed batch became %q after stale cleanup", v.State)
	}
}

func TestIndependentSessionsKeepSeparateLifetimes(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	a, b, idle := env.token(), env.token(), env.token()
	keyA, keyB := env.tokenKey(a), env.tokenKey(b)
	sessA, _ := env.sess.Validate(a)
	sessB, _ := env.sess.Validate(b)
	batchA := env.filesBatch(a, 8)
	batchB := env.filesBatch(b, 8)
	env.approveBatch(keyA, batchA)
	env.approveBatch(keyB, batchB)

	pwA, resA := startPut(contentURL(ts.URL, batchA, 0), a, 8)
	pwB, resB := startPut(contentURL(ts.URL, batchB, 0), b, 8)
	_, _ = pwA.Write([]byte("aaaa"))
	_, _ = pwB.Write([]byte("bbbb"))
	waitPinned(t, sessA, 1)
	waitPinned(t, sessB, 1)

	// Two idle windows later, the two streaming sessions are alive and the
	// session with no transfer is gone: transfers pin only their own session.
	env.clock.Advance(2 * session.DefaultIdleTimeout)
	if got := env.sess.Len(); got != 2 {
		t.Fatalf("Len = %d, want the two streaming sessions", got)
	}
	if _, err := env.sess.Validate(idle); !errors.Is(err, session.ErrExpired) {
		t.Fatalf("idle session next to busy ones: %v, want ErrExpired", err)
	}

	// A finishes; B keeps streaming. A is then idle and expires while B's
	// stream, alone, keeps B alive.
	_, _ = pwA.Write([]byte("aaaa"))
	_ = pwA.Close()
	if r := awaitPut(t, resA); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("A = %+v", r)
	}
	waitPinned(t, sessA, 0)
	env.clock.Advance(session.DefaultIdleTimeout + time.Second)
	if got := env.sess.Len(); got != 1 {
		t.Fatalf("Len = %d, want only B (still streaming)", got)
	}
	if _, err := env.sess.Validate(a); !errors.Is(err, session.ErrExpired) {
		t.Fatalf("A = %v, want ErrExpired", err)
	}
	_, _ = pwB.Write([]byte("bbbb"))
	_ = pwB.Close()
	if r := awaitPut(t, resB); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("B = %+v", r)
	}
	// Both sessions' data is intact and separate.
	for _, tc := range []struct {
		key   string
		batch transfer.BatchView
	}{{keyA, batchA}, {keyB, batchB}} {
		for _, id := range env.contentIDs(tc.key, tc.batch) {
			if u, _ := env.trans.Get(id); u.State != transfer.StateCompleted {
				t.Fatalf("record %s = %q", id, u.State)
			}
		}
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("part files: %v", left)
	}
}

func TestBatchStatusAndContentAreSessionScoped(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.token(), env.token()
	keyA := env.tokenKey(a)
	batch := env.filesBatch(a, 4)
	env.approveBatch(keyA, batch)
	if rec := env.do(http.MethodGet, "/api/batches/"+batch.ID, b, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("other session read the batch: %d", rec.Code)
	}
	rec := env.do(http.MethodPut, "/api/batches/"+batch.ID+"/items/"+batch.Items[0].ID+"/content", b, bytes.Repeat([]byte("x"), 4))
	if rec.Code == http.StatusOK {
		t.Fatal("other session uploaded into someone else's batch")
	}
	if v, _ := env.coord.GetBatch(keyA, batch.ID); v.State != transfer.BatchStateQueued {
		t.Fatalf("owner's batch was disturbed: %q", v.State)
	}
}
