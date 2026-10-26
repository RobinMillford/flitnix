package server

// Regression tests for stable large folder uploads (the multi-file,
// multi-gigabyte phone case): strict sequential claims with exact byte
// accounting, diagnostics that stay low-volume and private, no absolute
// deadline standing between a slow-but-continuous sender and success, and
// an aborted connection that cannot publish its item, cannot cancel any
// other session's work, and never revokes a healthy session.
//
// All bodies here are small synthetic ones. A real multi-gigabyte fixture
// is not reproducible in tests and adds nothing: the streaming machinery
// is size-independent (fixed 64 KiB copy buffer, exact-size enforcement),
// so order, accounting and diagnostics are the properties these tests
// actually guard.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// syntheticBytes fills a buffer with a deterministic, per-item pattern.
func syntheticBytes(size int64, seed byte) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = seed ^ byte(i%251)
	}
	return b
}

// approveAll is the desktop side of the batch approval workflow: every
// linked upload takes the approve decision, then the batch itself does.
func approveAll(t *testing.T, env *testEnv, key string, batch transfer.BatchView) {
	t.Helper()
	for _, item := range batch.Items {
		contentID, err := env.coord.ItemContentID(key, batch.ID, item.ID)
		if err != nil {
			t.Fatalf("content id for item %s: %v", item.ID, err)
		}
		if _, err := env.trans.Decide(contentID, true); err != nil {
			t.Fatalf("approve item %s: %v", item.ID, err)
		}
	}
	if _, err := env.coord.ApproveBatch(key, batch.ID); err != nil {
		t.Fatalf("approve batch: %v", err)
	}
}

// ---- a full 24-item batch: order, accounting, diagnostics ----

func TestBatchTwentyFourItemsSequentialAccounting(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	key := env.tokenKey(token)

	const count = 24
	items := make([]transfer.ManifestItem, 0, count)
	sizes := make([]int64, count)
	for i := 0; i < count; i++ {
		sizes[i] = int64(40 + i*13) // distinct sizes, one per item
		items = append(items, transfer.ManifestItem{
			RelativePath: fmt.Sprintf("part%02d.bin", i),
			Size:         sizes[i],
		})
	}
	manifest, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     items,
	})
	rec := env.do(http.MethodPost, "/api/batches", token, manifest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var batch transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != count {
		t.Fatalf("batch has %d items, want %d", len(batch.Items), count)
	}
	approveAll(t, env, key, batch)

	// Out-of-order probe: claiming a later item while the queue's next is
	// item 0 must be refused, and the queue must then still serve item 0.
	later := batch.Items[3]
	rec = env.do(http.MethodPut, "/api/batches/"+batch.ID+"/items/"+later.ID+"/content",
		token, []byte{0x01})
	if rec.Code != http.StatusConflict {
		t.Fatalf("out-of-order claim got %d (body %s), want 409", rec.Code, rec.Body.String())
	}

	var total int64
	for i := 0; i < count; i++ {
		total += sizes[i]
	}
	for i := 0; i < count; i++ {
		item := batch.Items[i]
		body := syntheticBytes(sizes[i], byte(i+7))
		rec = env.do(http.MethodPut,
			"/api/batches/"+batch.ID+"/items/"+item.ID+"/content", token, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("item %d upload status=%d body=%s", i, rec.Code, rec.Body.String())
		}
		var reply struct {
			Written int64  `json:"written"`
			SHA256  string `json:"sha256"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatalf("item %d reply: %v", i, err)
		}
		if reply.Written != sizes[i] {
			t.Errorf("item %d written=%d, want %d", i, reply.Written, sizes[i])
		}
	}
	rec = env.do(http.MethodGet, "/api/batches/"+batch.ID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status request got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.State != transfer.BatchStateCompleted || batch.TransferredBytes != total {
		t.Fatalf("batch after 24 items = %+v (total %d), want completed with %d bytes",
			batch, batch.TransferredBytes, total)
	}

	// Every byte landed on disk under its declared name.
	for i := 0; i < count; i++ {
		path := filepath.Join(env.store.Dir(), fmt.Sprintf("part%02d.bin", i))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("item %d publish: %v", i, err)
		}
		want := syntheticBytes(sizes[i], byte(i+7))
		if !bytes.Equal(got, want) {
			t.Errorf("item %d disk content differs (len %d, want %d)", i, len(got), len(want))
		}
	}
	if names := receiveDirNames(t, env.store); len(names) != count {
		t.Errorf("receive directory holds %d files, want %d", len(names), count)
	}

	// Diagnostics: one claim + one done per item, milestone lines only
	// for large streams (none here), and the exact byte totals recorded.
	text := env.diagText()
	if got := strings.Count(text, "batch item claim"); got != count {
		t.Errorf("claim diagnostics = %d, want %d", got, count)
	}
	if got := strings.Count(text, "batch item done"); got != count {
		t.Errorf("done diagnostics = %d, want %d", got, count)
	}
	if strings.Contains(text, " at=") {
		t.Errorf("small streams produced milestone lines: %s", text)
	}
	for i := 0; i < count; i++ {
		marker := fmt.Sprintf(" bytes=%d ", sizes[i])
		if strings.Count(text, marker) != 1 {
			t.Errorf("byte count %d not recorded exactly once", sizes[i])
		}
	}
	// Privacy: no token, no filesystem path, no file name may appear.
	if strings.Contains(text, token) {
		t.Error("diagnostics leaked the session token")
	}
	if strings.Contains(text, env.store.Dir()) {
		t.Error("diagnostics leaked the receive directory path")
	}
	if strings.Contains(text, ".bin") {
		t.Error("diagnostics leaked file names")
	}
}

// ---- quarter-mark diagnostics for large streams only ----

func TestLargeStreamQuarterMarkDiagnostics(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	key := env.tokenKey(token)
	const size = 2 << 20 // 2 MiB: above the milestone threshold

	manifest, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{RelativePath: "bulk.bin", Size: size}},
	})
	rec := env.do(http.MethodPost, "/api/batches", token, manifest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var batch transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	approveAll(t, env, key, batch)

	body := syntheticBytes(size, 0x5a)
	rec = env.do(http.MethodPut,
		"/api/batches/"+batch.ID+"/items/"+batch.Items[0].ID+"/content", token, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if reply.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("published digest = %s, want %s", reply.SHA256, hex.EncodeToString(sum[:]))
	}

	// Exactly five lines: claim, three quarter marks — and never a per
	// chunk line — and one done.
	text := env.diagText()
	for _, p := range []string{"at=25%", "at=50%", "at=75%"} {
		if got := strings.Count(text, p); got != 1 {
			t.Errorf("%s milestone recorded %d times, want 1 (%s)", p, got, text)
		}
	}
	if got := strings.Count(text, "batch item done"); got != 1 {
		t.Errorf("done lines = %d, want 1", got)
	}
	if got := strings.Count(text, "batch item progress"); got != 3 {
		t.Errorf("progress lines = %d, want 3: %s", got, text)
	}
	if strings.Contains(text, token) || strings.Contains(text, env.store.Dir()) {
		t.Error("diagnostics leaked a token or a path")
	}
}

// ---- no absolute deadline: slow-but-continuous beats the stall window ----

// pacedBody supplies its content slowly but without interruption: one chunk
// every delay, until exhausted. Each inter-chunk gap stays under the stall
// window while the total run is comfortably longer than it.
type pacedBody struct {
	data   []byte
	pos    int
	chunk  int
	delay  time.Duration
	stop   chan struct{}
	closed bool
}

func newPacedBody(data []byte, chunk int, delay time.Duration) *pacedBody {
	return &pacedBody{data: data, chunk: chunk, delay: delay, stop: make(chan struct{})}
}

func (p *pacedBody) Read(buf []byte) (int, error) {
	if p.pos >= len(p.data) {
		return 0, io.EOF
	}
	select {
	case <-time.After(p.delay):
	case <-p.stop:
		return 0, io.EOF
	}
	n := len(p.data) - p.pos
	if n > p.chunk {
		n = p.chunk
	}
	if n > len(buf) {
		n = len(buf)
	}
	copy(buf, p.data[p.pos:p.pos+n])
	p.pos += n
	return n, nil
}

func (p *pacedBody) Close() error {
	select {
	case <-p.stop:
	default:
		if !p.closed {
			close(p.stop)
			p.closed = true
		}
	}
	return nil
}

func TestSlowContinuousUploadExceedsStallWindow(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	key := env.tokenKey(token)
	const stallWindow = 300 * time.Millisecond
	const size = 6144
	const chunk = 512 // 12 reads at 50ms each: ~600ms total run

	srv, errc, stop := startRealServer(t, Config{
		TransferStall: stallWindow,
		Services: &Services{
			Sessions:    env.sess,
			Transfers:   env.trans,
			Coordinator: env.coord,
			Registry:    env.reg,
		},
	})
	start := time.Now()

	// A five-item batch so the in-flight batch polls race the stream.
	manifest, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{RelativePath: "trickle-00.bin", Size: size},
			{RelativePath: "trickle-01.bin", Size: size},
			{RelativePath: "trickle-02.bin", Size: size},
			{RelativePath: "trickle-03.bin", Size: size},
			{RelativePath: "trickle-04.bin", Size: size},
		},
	})
	rec := env.do(http.MethodPost, "/api/batches", token, manifest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var batch transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	// Pre-decide each item on the transfer service, then approve the batch,
	// matching the desktop workflow.
	for _, item := range batch.Items {
		contentID, err := env.coord.ItemContentID(key, batch.ID, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.trans.Decide(contentID, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.coord.ApproveBatch(key, batch.ID); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		// Background batch polls while the first item streams.
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				req, err := http.NewRequest(http.MethodGet, srv.URL()+"/api/batches/"+batch.ID, nil)
				if err != nil {
					t.Error(err)
					return
				}
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						t.Errorf("batch poll failed: %v", err)
					}
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("batch poll got %d", resp.StatusCode)
					return
				}
			}
		}
	}()

	for i := 0; i < 5; i++ {
		item := batch.Items[i]
		body := newPacedBody(syntheticBytes(size, byte(i)), chunk, 50*time.Millisecond)
		t.Cleanup(func() { _ = body.Close() })
		req, err := http.NewRequest(http.MethodPut,
			srv.URL()+"/api/batches/"+batch.ID+"/items/"+item.ID+"/content", body)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("item %d paced upload failed: %v", i, err)
		}
		respBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("item %d paced upload got %d body=%s (a slow-but-alive sender must never be cut)",
				i, resp.StatusCode, string(respBytes))
		}
		var reply struct {
			Written int64 `json:"written"`
		}
		_ = json.Unmarshal(respBytes, &reply)
		if reply.Written != size {
			t.Errorf("item %d written=%d, want %d", i, reply.Written, size)
		}
	}
	took := time.Since(start)
	if took < stallWindow {
		t.Errorf("five paced items finished in %s, under the %s stall window: the test's timing floor broke",
			took, stallWindow)
	}

	// The batch completed with exact accounting and a clean drive.
	rec = env.do(http.MethodGet, "/api/batches/"+batch.ID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("final batch status request got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.State != transfer.BatchStateCompleted || batch.TransferredBytes != 5*size {
		t.Fatalf("batch = %+v, want completed with %d bytes", batch, 5*size)
	}

	// Wind down cleanly: stop the poller first so no late t.Error can
	// race the test end, then drain the server and check Serve's exit.
	cancel()
	select {
	case <-pollDone:
	case <-time.After(2 * time.Second):
		t.Error("batch poll goroutine did not exit")
	}
	stop()
	awaitServeReturn(t, errc)
}

// ---- an aborted connection must not publish, must not cancel other
// work, and must not revoke a healthy session ----

func TestAbortedUploadCannotPublishAndKeepsSession(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	key := env.tokenKey(token)
	const size = 8192
	const partLen = 2048

	srv, errc, stop := startRealServer(t, Config{
		Services: &Services{
			Sessions:    env.sess,
			Transfers:   env.trans,
			Coordinator: env.coord,
			Registry:    env.reg,
			Diagnose:    env.diagSink,
		},
	})

	manifest, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{RelativePath: "cut-00.bin", Size: size},
			{RelativePath: "cut-01.bin", Size: size},
			{RelativePath: "cut-02.bin", Size: size},
		},
	})
	rec := env.do(http.MethodPost, "/api/batches", token, manifest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	var batch transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	approveAll(t, env, key, batch)

	// Raw connection: declare the full body length, send only a part of
	// it, then vanish mid-stream — the phone-shaped failure during PUT.
	host := strings.TrimPrefix(srv.URL(), "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	var req bytes.Buffer
	fmt.Fprintf(&req, "PUT /api/batches/%s/items/%s/content HTTP/1.1\r\n",
		batch.ID, batch.Items[0].ID)
	fmt.Fprintf(&req, "Host: %s\r\n", host)
	fmt.Fprintf(&req, "Authorization: Bearer %s\r\n", token)
	fmt.Fprintf(&req, "Content-Length: %d\r\n", size)
	fmt.Fprintf(&req, "Connection: close\r\n\r\n")
	if _, err := conn.Write(req.Bytes()); err != nil {
		t.Fatalf("write headers: %v", err)
	}
	if _, err := conn.Write(syntheticBytes(size, 0x11)[:partLen]); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // let the stream park mid-read
	_ = conn.Close()

	// Poll until the failure settles: the aborted item keeps its error,
	// the unstarted ones are canceled with it. The state must arrive
	// from the stream verdict, never from a timeout.
	deadline := time.Now().Add(5 * time.Second)
	var view transfer.BatchView
	var viewErr error
	for {
		view, viewErr = env.coord.GetBatch(key, batch.ID)
		if viewErr != nil {
			t.Fatalf("batch view: %v", viewErr)
		}
		if view.Items[0].State == transfer.ItemStateFailed &&
			view.Items[1].State == transfer.ItemStateCanceled &&
			view.Items[2].State == transfer.ItemStateCanceled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("abort not settled: item0=%s item1=%s item2=%s",
				view.Items[0].State, view.Items[1].State, view.Items[2].State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if view.State != transfer.BatchStateFailed {
		t.Errorf("batch state %q, want failed", view.State)
	}

	// Nothing from the aborted stream may be published.
	if names := receiveDirNames(t, env.store); len(names) != 0 {
		t.Errorf("receive directory holds %d names after an aborted stream: %v", len(names), names)
	}

	// Diagnostics: exactly one failure line whose reason is a verdict
	// attributable to the sender. On HTTP/1 a raw abort surfaces as
	// either a short body (truncated) or a disconnect observed through
	// the request context; both are honest. Nothing private appears.
	text := env.diagText()
	if got := strings.Count(text, "batch item failed"); got != 1 {
		t.Errorf("failed diagnostics lines = %d, want 1: %s", got, text)
	}
	if !strings.Contains(text, "reason=truncated ") &&
		!strings.Contains(text, "reason=client_disconnect ") {
		t.Errorf("failure reason is not client-attributed: %s", text)
	}
	if strings.Contains(text, token) ||
		strings.Contains(text, env.store.Dir()) ||
		strings.Contains(text, ".bin") {
		t.Error("diagnostics leaked a token, a path or a file name")
	}

	// The session itself stays authorized and its queue accepts a fresh
	// batch: the phone's recovery path is exactly this shape.
	manifest2, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{RelativePath: "after.bin", Size: size}},
	})
	rec = env.do(http.MethodPost, "/api/batches", token, manifest2)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fresh batch after failure status=%d body=%s", rec.Code, rec.Body.String())
	}
	var after transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	approveAll(t, env, key, after)
	rec = env.do(http.MethodPut,
		"/api/batches/"+after.ID+"/items/"+after.Items[0].ID+"/content",
		token, syntheticBytes(size, 0x99))
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh upload after failure status=%d body=%s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Written int64 `json:"written"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Written != size {
		t.Errorf("fresh upload written=%d, want %d", reply.Written, size)
	}
	stop()
	awaitServeReturn(t, errc)
}

// ---- manifests that declare gigabyte-scale sizes are metadata-only and
// therefore accepted; malformed or inconsistent metadata never is ----

// TestGigabyteScaleManifestsAreAcceptedMetadataOnly uploads nothing: it
// guards the acceptance of manifest *metadata* at the sizes the phone
// actually produces (a ~7 GiB folder of episodes), and the rejection of
// malformed size declarations, all through the real route.
func TestGigabyteScaleManifestsAreAcceptedMetadataOnly(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()

	// One folder batch of 24 ~300 MiB episodes: the canonical ~7 GiB
	// phone shape.
	episode := int64(300 << 20)
	items := make([]string, 0, 24)
	var declared int64
	for i := 0; i < 24; i++ {
		items = append(items, fmt.Sprintf(
			`{"relativePath":"ep%02d.mkv","size":%d}`, i+1, episode))
		declared += episode
	}
	mounted := fmt.Sprintf(`{"direction":"phone_to_desktop","kind":"folder",`+
		`"rootName":"Season 24","items":[%s]}`, strings.Join(items, ","))
	rec := env.do(http.MethodPost, "/api/batches", token, []byte(mounted))
	if rec.Code != http.StatusCreated {
		t.Fatalf("24-episode manifest status=%d body=%s", rec.Code, rec.Body.String())
	}
	var batch transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != 24 {
		t.Fatalf("batch holds %d items, want 24", len(batch.Items))
	}
	if batch.DeclaredTotalBytes != declared {
		t.Fatalf("declaredTotalBytes = %d, want the exact item sum %d", batch.DeclaredTotalBytes, declared)
	}
	if batch.State != transfer.BatchStatePending {
		t.Fatalf("batch state = %q, want pending (metadata-only: nothing uploaded, awaiting approval)", batch.State)
	}

	// One single file at seven gibibytes flat.
	single := `{"direction":"phone_to_desktop","kind":"files",` +
		`"items":[{"displayName":"episode.mkv","size":7516192768}]}`
	rec = env.do(http.MethodPost, "/api/batches", token, []byte(single))
	if rec.Code != http.StatusCreated {
		t.Fatalf("single 7 GiB manifest status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.DeclaredTotalBytes != 7<<30 {
		t.Fatalf("single-file declaredTotalBytes = %d, want %d", batch.DeclaredTotalBytes, 7<<30)
	}

	// Acceptance publishes nothing: the receive directory may hold only
	// the folder batch's allocated root directory; the flat batch, the
	// only stream target in play, put no file and no part file anywhere.
	entries, err := os.ReadDir(env.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("metadata-only run left %d receive entries (%v), want only the folder root", len(entries), receiveDirNames(t, env.store))
	}
	if !entries[0].IsDir() || strings.HasPrefix(entries[0].Name(), "episode") {
		t.Fatalf("entry = %q, want the allocated folder root only", entries[0].Name())
	}

	// Malformed sizes are rejected before they can reach the machine:
	// a string, a JSON number beyond int64, a negative count, and an
	// aggregate that overflows int64.
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"string size":       {`{"kind":"files","items":[{"displayName":"a.bin","size":"seven"}]}`, "invalid batch manifest JSON"},
		"int64 overflow":    {`{"kind":"files","items":[{"displayName":"a.bin","size":9223372036854775808}]}`, "invalid batch manifest JSON"},
		"negative size":     {`{"kind":"files","items":[{"displayName":"a.bin","size":-5}]}`, "invalid batch manifest"},
		"overflowing total": {`{"kind":"files","items":[{"displayName":"a.bin","size":9223372036854775807},{"displayName":"b.bin","size":1}]}`, "invalid batch manifest"},
	} {
		rec := env.do(http.MethodPost, "/api/batches", token, []byte(tc.body))
		code, message, _ := decodeCode(t, rec)
		if code != http.StatusBadRequest || message != tc.want {
			t.Errorf("%s: got %d error=%q, want 400 %q", name, code, message, tc.want)
		}
	}
}

// ---- a completed item must survive a later item's failure ----

// TestCompletedItemSurvivesALaterItemFailure uploads the first item to
// completion, then aborts mid-stream on the second. The finished first file
// must remain published with accounted bytes; the failed second item and
// everyone after it must not publish anything; the queue must still work.
func TestCompletedItemSurvivesALaterItemFailure(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	token := env.token()
	key := env.tokenKey(token)
	batch := env.filesBatch(token, 8, 8)
	env.approveBatch(key, batch)
	ids := env.contentIDs(key, batch)

	rec := env.do(http.MethodPut,
		"/api/batches/"+batch.ID+"/items/"+batch.Items[0].ID+"/content",
		token, syntheticBytes(8, 0x21))
	if rec.Code != http.StatusOK {
		t.Fatalf("first item upload status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Second item: declare 8 bytes, send 4, then die like a phone abort.
	pw, res := startPut(contentURL(ts.URL, batch, 1), token, 8)
	if _, err := pw.Write([]byte("1234")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second stream to start", func() bool {
		u, _ := env.trans.Get(ids[1])
		return u.State == transfer.StateUploading
	})
	pw.CloseWithError(errors.New("phone aborted"))
	if r := awaitPut(t, res); r.err == nil && r.code == http.StatusOK {
		t.Fatalf("aborted second item reported success: %+v", r)
	}

	waitFor(t, "second item settles as failed", func() bool {
		u, _ := env.trans.Get(ids[1])
		return u.State == transfer.StateFailed
	})
	if u, _ := env.trans.Get(ids[0]); u.State != transfer.StateCompleted {
		t.Fatalf("first record = %q, want completed; finished work must keep its state", u.State)
	}

	view, err := env.coord.GetBatch(key, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []transfer.ItemState{transfer.ItemStateCompleted, transfer.ItemStateFailed}
	for i := range want {
		if view.Items[i].State != want[i] {
			t.Fatalf("items = %v, want %v", itemStates(view), want)
		}
	}
	if view.State != transfer.BatchStateFailed {
		t.Fatalf("batch = %q, want failed", view.State)
	}
	// The completed item's bytes are the batch's accounted total; the
	// failed stream's partial bytes contribute nothing.
	if view.TransferredBytes != 8 {
		t.Fatalf("batch bytes = %d, want exactly the completed item's 8", view.TransferredBytes)
	}

	names := receiveDirNames(t, env.store)
	if len(names) != 1 || names[0] != "file-0.bin" {
		t.Fatalf("receive directory = %v, want exactly one published file: file-0.bin", names)
	}
	if info, err := os.Stat(filepath.Join(env.store.Dir(), "file-0.bin")); err != nil || info.Size() != 8 {
		t.Fatalf("surviving file = %v, %v (want 8 bytes)", info, err)
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

// ---- a sender may not push more bytes than it declared ----

// TestOversizedBatchItemUploadIsRefusedAndPublishesNothing declares 8 bytes
// and pushes 9: the stream must refuse the surplus, no partial data may be
// published anywhere, and the failure reason must be attributable to the
// sender rather than to the server.
func TestOversizedBatchItemUploadIsRefusedAndPublishesNothing(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	key := env.tokenKey(token)
	batch := env.filesBatch(token, 8, 8)
	env.approveBatch(key, batch)
	ids := env.contentIDs(key, batch)

	rec := env.do(http.MethodPut,
		"/api/batches/"+batch.ID+"/items/"+batch.Items[0].ID+"/content",
		token, syntheticBytes(9, 0x44))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized upload got %d body=%s, want 400", rec.Code, rec.Body.String())
	}
	var reply struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != "body exceeds the approved size" {
		t.Fatalf("oversize error = %q, want the exact oversized-body verdict", reply.Error)
	}

	// One failed item's stream verdict ends the whole batch: every other
	// item canceling is the documented sequential-queue contract.
	waitFor(t, "batch settles failed", func() bool {
		v, _ := env.coord.GetBatch(key, batch.ID)
		return v.State == transfer.BatchStateFailed
	})
	view, _ := env.coord.GetBatch(key, batch.ID)
	want := []transfer.ItemState{transfer.ItemStateFailed, transfer.ItemStateCanceled}
	for i := range want {
		if view.Items[i].State != want[i] {
			t.Fatalf("items = %v, want %v", itemStates(view), want)
		}
	}
	if view.TransferredBytes != 0 {
		t.Fatalf("batch bytes = %d, want 0: overrun bytes must not be accounted", view.TransferredBytes)
	}
	for i, wantState := range []transfer.State{transfer.StateFailed, transfer.StateCanceled} {
		if u, _ := env.trans.Get(ids[i]); u.State != wantState {
			t.Fatalf("upload record %d = %q, want %q", i, u.State, wantState)
		}
	}

	if names := receiveDirNames(t, env.store); len(names) != 0 {
		t.Errorf("oversized upload published %v", names)
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
	if got := strings.Count(env.diagText(), "reason=size_exceeded"); got != 1 {
		t.Errorf("size_exceeded diagnosed %d times, want 1: %s", got, env.diagText())
	}
}
