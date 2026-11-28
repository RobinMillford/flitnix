package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

// cancelOnFullBodyWriter mimics a browser that has received the entire file and
// immediately closes the connection. The request context is cancelled before
// the server performs the final read that would return io.EOF.
type cancelOnFullBodyWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	cancel  context.CancelFunc
	target  int64
	written int64
}

func (w *cancelOnFullBodyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	w.written += int64(n)
	if w.written >= w.target {
		w.cancel()
	}
	return n, err
}

func (w *cancelOnFullBodyWriter) bytesWritten() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

func TestStreamSingleFileFullBodyDespiteLateCancel(t *testing.T) {
	payload := bytes.Repeat([]byte("flitnix-payload-"), 4096)
	dir := t.TempDir()
	path := dir + "/shared.bin"
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	snap := captureFileSnapshot(f, path, info)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelOnFullBodyWriter{cancel: cancel, target: int64(len(payload))}

	grant := downloadGrant{
		ID:       "grant-late-cancel",
		Filename: "shared.bin",
		Entries:  []archiveEntry{{Name: "shared.bin", Path: path, Size: int64(len(payload))}},
	}
	progress := &archiveProgress{ID: "grant-late-cancel", Total: int64(len(payload)), State: progressPending}

	started, err := streamSingleFileWithSnapshot(ctx, http.MethodGet, httptest.NewRecorder(), w, grant, progress, f, snap)
	if err != nil {
		t.Fatalf("full body delivered but stream reported failure: %v", err)
	}
	if !started {
		t.Fatal("expected stream to be marked as started")
	}
	if got := w.bytesWritten(); got != int64(len(payload)) {
		t.Fatalf("wrote %d bytes; want %d", got, len(payload))
	}
	if progress.State != progressCompleted {
		t.Fatalf("progress state = %q; want %q", progress.State, progressCompleted)
	}
}
