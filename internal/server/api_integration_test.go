package server

// Real-server API tests: cases needing actual TCP connections and the
// transport-level deadline machinery (record-level facts live in
// api_test.go). Streaming failures are observed by polling the upload
// records through the service; no fixed sleeps decide their outcome.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// startRealServer runs a server (with the given Config) on an ephemeral
// loopback port. The returned CancelFunc triggers the graceful drain (the
// cleanup calls it too, idempotently), and the returned channel carries
// Serve's final error so tests can assert a clean stop.
func startRealServer(t *testing.T, cfg Config) (*Server, <-chan error, context.CancelFunc) {
	t.Helper()
	cfg.ListenAddr = "127.0.0.1:0"
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 2 * time.Second
	}
	s := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ctx) }()
	t.Cleanup(cancel)
	select {
	case <-s.Ready():
	case err := <-errc:
		t.Fatalf("server failed to start: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("server did not become ready")
	}
	return s, errc, cancel
}

// awaitServeReturn fails when Serve does not return nil, cleanly, within
// 5s after the caller triggers its shutdown.
func awaitServeReturn(t *testing.T, errc <-chan error) {
	t.Helper()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Serve returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Serve did not return within 5s of shutdown")
	}
}

// awaitTerminalUpload polls a record until it reaches a terminal state
// (fresh state each iteration: the record's state is synchronized via the
// service), and fails when it never gets there.
func awaitTerminalUpload(t *testing.T, svc *transfer.Service, id string) transfer.Upload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		u, ok := svc.Get(id)
		if !ok {
			t.Fatalf("upload %s vanished", id)
		}
		if isTerminalState(u.State) {
			return u
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("upload %s stayed %q past the deadline", id, svcStateOf(svc, id))
	return transfer.Upload{} // unreachable
}

// svcStateOf returns the record's state ("" when unknown), for error
// messages only.
func svcStateOf(svc *transfer.Service, id string) transfer.State {
	u, ok := svc.Get(id)
	if !ok {
		return ""
	}
	return u.State
}

// ---- shutdown during an active transfer ----

// trickleBody is a client body that supplies data slowly: each Read
// emits 64 KiB after 30 ms, until stopped. It keeps the server-side
// stream parked in the *connection* read between chunks — the place
// where a shutdown's force-close visibly unblocks the handler.
type trickleBody struct {
	stop chan struct{}
}

func newTrickleBody() *trickleBody { return &trickleBody{stop: make(chan struct{})} }

func (s *trickleBody) Read(p []byte) (int, error) {
	select {
	case <-s.stop:
		return 0, io.EOF
	default:
	}
	select {
	case <-time.After(30 * time.Millisecond):
	case <-s.stop:
		return 0, io.EOF
	}
	n := 64 * 1024
	if len(p) < n {
		n = len(p)
	}
	for i := range p[:n] {
		p[i] = 'x'
	}
	return n, nil
}

func (s *trickleBody) Close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	return nil
}

func TestAPIShutdownDuringActiveTransfer(t *testing.T) {
	env := newTestEnv(t) // services wired with a throwaway receive directory
	srv, errc, stop := startRealServer(t, Config{
		ShutdownTimeout: 300 * time.Millisecond, // short drain: force-close path
		Services: &Services{
			Sessions:  env.sess,
			Transfers: env.trans,
			Registry:  env.reg,
		},
	})

	// Declare and approve a 1 MiB upload.
	tok := env.token()
	rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"tardy.bin","size":1048576}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("declare got %d (body %s), want 201", rec.Code, rec.Body.String())
	}
	up := statusJSON(t, rec)
	env.approve(tok, up.ID)

	// PUT with the trickling body. The handler parks inside its volume
	// loop; then the Serve cancellation, a too-short drain window, and
	// the force-close cut the connection mid-transfer.
	body := newTrickleBody()
	t.Cleanup(func() { _ = body.Close() })
	req, err := http.NewRequest(http.MethodPut, srv.URL()+"/api/uploads/"+up.ID+"/content", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	client := &http.Client{Timeout: 15 * time.Second}
	resErr := make(chan error, 1)
	resc := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Do(req)
		resErr <- err
		resc <- resp
	}()

	// Wait until the handler has entered the stream, then shut down.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if u, ok := env.trans.Get(up.ID); ok && u.State == transfer.StateUploading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the upload never entered the streaming state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	awaitServeReturn(t, errc)

	// The record ends terminal: the force-close makes the transfer's
	// failure honest instead of leaving a half-written file.
	u := awaitTerminalUpload(t, env.trans, up.ID)
	if u.State != transfer.StateFailed {
		t.Errorf("upload after shutdown = %q (reason=%q), want failed", u.State, u.Reason)
	}
	if names := receiveDirNames(t, env.store); len(names) != 0 {
		t.Errorf("an aborted transfer left files: %v", names)
	}

	// The client observed the cut: a transport error or an error response.
	select {
	case resp := <-resc:
		if resp != nil {
			defer resp.Body.Close()
			if resp.StatusCode < 400 {
				select {
				case cerr := <-resErr:
					if cerr == nil {
						t.Errorf("client saw a %d response with no transport error", resp.StatusCode)
					}
				case <-time.After(5 * time.Second):
					t.Error("client response was consumed but no error was reported")
				}
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client never heard from the server")
	}
}

// ---- transfer-stall: a stalled sender is cut ----

// stallBody supplies two bytes once, then parks until stopped (the server's
// rolling read deadline (TransferStall) fires while it waits for more).
// Parking on a closeable channel (rather than select{}) lets tests un-park
// the client-side goroutine on cleanup.
type stallBody struct {
	stop chan struct{}
	sent bool
}

func newStallBody() *stallBody { return &stallBody{stop: make(chan struct{})} }

func (s *stallBody) Read(p []byte) (int, error) {
	if !s.sent && len(p) >= 2 {
		s.sent = true
		p[0], p[1] = 'a', 'b'
		return 2, nil
	}
	select {
	case <-s.stop:
		return 0, io.EOF
	}
}

func (s *stallBody) Close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	return nil
}

func TestAPITransferStallCutsUpload(t *testing.T) {
	env := newTestEnv(t)
	srv, _, _ := startRealServer(t, Config{
		TransferStall:   300 * time.Millisecond, // far shorter than the client's stall
		ShutdownTimeout: 2 * time.Second,
		Services: &Services{
			Sessions:  env.sess,
			Transfers: env.trans,
			Registry:  env.reg,
		},
	})
	start := time.Now()

	// Declare and approve an 8-byte upload; only two ever arrive.
	tok := env.token()
	rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"stall.bin","size":8}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("declare got %d (body %s), want 201", rec.Code, rec.Body.String())
	}
	up := statusJSON(t, rec)
	env.approve(tok, up.ID)

	body := newStallBody()
	t.Cleanup(func() { _ = body.Close() }) // un-park the transport's writer goroutine
	req, err := http.NewRequest(http.MethodPut, srv.URL()+"/api/uploads/"+up.ID+"/content", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	client := &http.Client{Timeout: 15 * time.Second}
	resErr := make(chan error, 1)
	resc := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Do(req)
		resErr <- err
		resc <- resp
	}()

	awaitTerminalUpload(t, env.trans, up.ID)
	took := time.Since(start)
	if took < 250*time.Millisecond {
		t.Errorf("stall cut happened after %s; the window is 300ms, so this may be a misuse", took)
	}
	if took > 5*time.Second {
		t.Errorf("stall cut took %s; the window is 300ms", took)
	}
	if names := receiveDirNames(t, env.store); len(names) != 0 {
		t.Errorf("a stalled upload left files: %v", names)
	}

	// The client sees the cut through the transport: either a response
	// with an error code, or a transport failure.
	select {
	case resp := <-resc:
		if resp != nil {
			defer resp.Body.Close()
			if resp.StatusCode < 400 {
				select {
				case cerr := <-resErr:
					if cerr == nil {
						t.Errorf("client saw a %d response with no transport error", resp.StatusCode)
					}
				case <-time.After(5 * time.Second):
					t.Error("client response was consumed but no error was reported")
				}
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client never heard from the server")
	}
}

// ---- transfer-stall: a stalled receiver is cut ----

func TestAPITransferStallCutsDownload(t *testing.T) {
	env := newTestEnv(t)
	srv, errc, stop := startRealServer(t, Config{
		TransferStall: 300 * time.Millisecond,
		Services: &Services{
			Sessions:  env.sess,
			Transfers: env.trans,
			Registry:  env.reg,
		},
	})

	// 64 MiB is far beyond what loopback TCP buffers can absorb, so the
	// server's writer blocks once the client stops reading.
	dir := t.TempDir()
	payload := make([]byte, 64<<20)
	path := filepath.Join(dir, "buried.bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("prepare file: %v", err)
	}
	sf, err := env.reg.Add(path, "buried.bin", "")
	if err != nil {
		t.Fatalf("register file: %v", err)
	}

	tok := env.token()
	req, err := http.NewRequest(http.MethodGet, srv.URL()+"/api/files/"+sf.ID+"/download", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("download request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download started with %d, want 200", resp.StatusCode)
	}

	// Read only the first chunk, then go silent: stop reading entirely.
	// The server's writer should block once the receive window closes
	// and be cut by the 300ms write stall.
	buf := make([]byte, 64*1024)
	n, rerr := resp.Body.Read(buf)
	if rerr != nil || n == 0 {
		t.Fatalf("the first read of a healthy download failed (n=%d err=%v)", n, rerr)
	}

	// The client is fully parked here; it cannot observe the cut while
	// silent. After a window that comfortably exceeds the stall (5x),
	// resuming must surface a mid-body cut, never a complete body.
	const silent = 1500 * time.Millisecond
	time.Sleep(silent)

	var lastErr error
	deadline := time.Now().Add(7 * time.Second)
	for {
		_, lastErr = resp.Body.Read(buf)
		if lastErr != nil && lastErr != io.EOF {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	// A clean EOF means the server delivered the promised body through
	// buffers alone while the client was parked, or was never cut: both
	// are failures for the stall guarantee.
	if lastErr == nil || lastErr == io.EOF {
		t.Errorf("parked %s, the download body still ended cleanly (%v); want a mid-body cut by the stall", silent, lastErr)
	}
	stop()
	awaitServeReturn(t, errc)
}
