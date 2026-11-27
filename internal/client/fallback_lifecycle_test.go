package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fallbackTestPayload writes a deterministic payload and returns its path and bytes.
func fallbackTestPayload(t *testing.T, size int) (string, []byte) {
	t.Helper()
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + 7)
	}
	path := filepath.Join(t.TempDir(), "fallback.bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path, payload
}

func fallbackTarget(t *testing.T, ts *httptest.Server) *Target {
	t.Helper()
	target, err := ParseTarget(ts.URL+"/#tok", "")
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	return target
}

type progressRecorder struct {
	mu     sync.Mutex
	values []int64
	totals []int64
}

func (p *progressRecorder) record(written, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values = append(p.values, written)
	p.totals = append(p.totals, total)
}

func (p *progressRecorder) snapshot() ([]int64, []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.values...), append([]int64(nil), p.totals...)
}

func assertProgress(t *testing.T, rec *progressRecorder, wantTotal, wantWritten int64) {
	t.Helper()
	values, totals := rec.snapshot()
	if len(values) == 0 {
		t.Fatal("no progress callbacks")
	}
	last := int64(-1)
	finalEvents := 0
	for i, v := range values {
		if v < last {
			t.Fatalf("progress went backwards: %d then %d", last, v)
		}
		if v > wantTotal {
			t.Fatalf("progress %d exceeds total %d", v, wantTotal)
		}
		if totals[i] != wantTotal {
			t.Fatalf("progress total = %d, want %d", totals[i], wantTotal)
		}
		if v == wantWritten {
			finalEvents++
		}
		last = v
	}
	if finalEvents != 1 {
		t.Fatalf("got %d final progress events, want exactly 1", finalEvents)
	}
}

// Scenario B/C/D/F: PATCH is rejected before its body is consumed; the PUT
// fallback must still send every byte, with correct ContentLength and digest.
func TestStreamItemEarlyPatchRejectionFallsBackWithCompleteBody(t *testing.T) {
	const payloadSize = 4 << 20
	path, payload := fallbackTestPayload(t, payloadSize)
	wantSum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(wantSum[:])

	var mu sync.Mutex
	var putLength, putBytes int64
	var putSHA string
	// A rejected PATCH can emit progress before its 405 arrives, and a retry or
	// the PUT fallback legitimately restarts progress. This test therefore
	// checks bounds and completion, not cross-attempt monotonicity; the
	// single-attempt test below checks strict monotonicity.
	type progressEvent struct {
		value int64
		total int64
	}
	var events []progressEvent

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			// Deliberately reject without reading the request body. The Go
			// transport may keep consuming it after the response is received.
			w.WriteHeader(http.StatusMethodNotAllowed)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			sum := sha256.Sum256(body)
			mu.Lock()
			putLength = r.ContentLength
			putBytes = int64(len(body))
			putSHA = hex.EncodeToString(sum[:])
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"written": len(body),
				"sha256":  hex.EncodeToString(sum[:]),
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer ts.Close()

	record := func(written, total int64) {
		mu.Lock()
		events = append(events, progressEvent{value: written, total: total})
		mu.Unlock()
	}
	res, err := NewClient(fallbackTarget(t, ts)).StreamItemWithHint(
		context.Background(), "b1", "i1", path, 0, 1<<20, record,
	)
	if err != nil {
		t.Fatalf("fallback transfer: %v", err)
	}
	if !res.Verified || res.Written != payloadSize {
		t.Fatalf("result = %+v, want verified with %d bytes", res, payloadSize)
	}
	if res.SHA256 != wantSHA || res.LocalSHA256 != wantSHA {
		t.Fatalf("digest mismatch: remote=%s local=%s want=%s", res.SHA256, res.LocalSHA256, wantSHA)
	}

	mu.Lock()
	defer mu.Unlock()
	if putLength != payloadSize {
		t.Fatalf("PUT ContentLength = %d, want %d", putLength, payloadSize)
	}
	if putBytes != payloadSize {
		t.Fatalf("PUT body length = %d, want %d", putBytes, payloadSize)
	}
	if putSHA != wantSHA {
		t.Fatalf("PUT body digest = %s, want %s", putSHA, wantSHA)
	}
	if len(events) == 0 {
		t.Fatal("no progress callbacks")
	}
	finalEvents := 0
	finalIndex := -1
	for i, e := range events {
		if e.total != payloadSize || e.value < 0 || e.value > payloadSize {
			t.Fatalf("progress (%d, %d) outside [0, %d]", e.value, e.total, payloadSize)
		}
		if e.value == payloadSize {
			finalEvents++
			finalIndex = i
		}
	}
	if finalEvents != 1 {
		t.Fatalf("got %d final progress events, want exactly 1", finalEvents)
	}
	if finalIndex != len(events)-1 {
		t.Fatalf("progress events after completion: %d of %d", len(events)-finalIndex-1, len(events))
	}
}

// Scenario A/F: a normal multi-chunk PATCH upload must be exact and progressive.
func TestStreamItemPatchSuccessHasExactLengthAndProgress(t *testing.T) {
	const payloadSize = 1 << 20
	path, payload := fallbackTestPayload(t, payloadSize)
	wantSum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(wantSum[:])

	var mu sync.Mutex
	received := make([]byte, 0, payloadSize)
	lengths := make([]int64, 0, 4)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		lengths = append(lengths, r.ContentLength)
		received = append(received, body...)
		total := len(received)
		sum := sha256.Sum256(received)
		mu.Unlock()
		if total < payloadSize {
			w.Header().Set("Upload-Offset", strconv.Itoa(total))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"written": total,
			"sha256":  hex.EncodeToString(sum[:]),
		})
	}))
	defer ts.Close()

	rec := &progressRecorder{}
	res, err := NewClient(fallbackTarget(t, ts)).StreamItemWithHint(
		context.Background(), "b1", "i2", path, 0, 256<<10, rec.record,
	)
	if err != nil {
		t.Fatalf("PATCH transfer: %v", err)
	}
	if !res.Verified || res.Written != payloadSize || res.SHA256 != wantSHA {
		t.Fatalf("result = %+v, want verified %d bytes with %s", res, payloadSize, wantSHA)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lengths) != 4 {
		t.Fatalf("got %d PATCH requests, want 4", len(lengths))
	}
	for i, n := range lengths {
		if n != 256<<10 {
			t.Fatalf("PATCH %d ContentLength = %d, want %d", i, n, 256<<10)
		}
	}
	assertProgress(t, rec, payloadSize, payloadSize)
}

// Scenario E: cancellation during the fallback must stop the transfer and must
// not produce a verified result.
func TestStreamItemFallbackCancellation(t *testing.T) {
	path, _ := fallbackTestPayload(t, 1<<20)
	putStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	var cancelObserved atomic.Bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPatch:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodPut:
			close(putStarted)
			// Drain the body so the connection can close promptly, then wait
			// for the client's cancellation to reach the server. The timeout
			// is only a guard so a broken cancellation cannot hang the suite.
			go func() { _, _ = io.Copy(io.Discard, r.Body) }()
			select {
			case <-r.Context().Done():
				cancelObserved.Store(true)
			case <-time.After(2 * time.Second):
			}
			close(handlerDone)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-putStarted:
			cancel()
		case <-time.After(5 * time.Second):
		}
	}()

	res, err := NewClient(fallbackTarget(t, ts)).StreamItemWithHint(ctx, "b1", "i3", path, 0, 256<<10, nil)
	if err == nil {
		t.Fatalf("canceled transfer returned a result: %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("PUT handler did not finish")
	}
	if !cancelObserved.Load() {
		t.Fatal("server did not observe the client cancellation")
	}
}

// Scenario D: a large fallback streams through without buffering the whole
// file in the client's reader, and every byte/digest is exact.
func TestStreamItemFallbackLargeFileStreamsExactBytes(t *testing.T) {
	const payloadSize = 16 << 20
	path, payload := fallbackTestPayload(t, payloadSize)
	wantSum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(wantSum[:])

	var mu sync.Mutex
	var putLength, putRead int64
	var putSHA string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Stream the body into a hasher; do not keep it in memory.
		hasher := sha256.New()
		n, err := io.Copy(hasher, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sum := hex.EncodeToString(hasher.Sum(nil))
		mu.Lock()
		putLength = r.ContentLength
		putRead = n
		putSHA = sum
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"written": n, "sha256": sum})
	}))
	defer ts.Close()

	res, err := NewClient(fallbackTarget(t, ts)).StreamItemWithHint(
		context.Background(), "b1", "large", path, 0, 1<<20, nil,
	)
	if err != nil {
		t.Fatalf("large fallback: %v", err)
	}
	if !res.Verified || res.Written != payloadSize || res.SHA256 != wantSHA {
		t.Fatalf("result = %+v, want verified %d bytes with %s", res, payloadSize, wantSHA)
	}
	mu.Lock()
	defer mu.Unlock()
	if putLength != payloadSize || putRead != payloadSize {
		t.Fatalf("PUT length=%d read=%d, want %d", putLength, putRead, payloadSize)
	}
	if putSHA != wantSHA {
		t.Fatalf("PUT digest = %s, want %s", putSHA, wantSHA)
	}
}

// Direct guard for the root cause: hashing a prefix must not move the shared
// file offset that an in-flight request body may still be reading from.
func TestComputeFilePrefixHashLeavesFileOffsetUnchanged(t *testing.T) {
	path, payload := fallbackTestPayload(t, 4096)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	_, sumFunc, err := computeFilePrefixHash(f, 1024)
	if err != nil {
		t.Fatalf("computeFilePrefixHash: %v", err)
	}
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if offset != 0 {
		t.Fatalf("file offset moved to %d, want 0", offset)
	}
	wantSum := sha256.Sum256(payload[:1024])
	if got := hex.EncodeToString(sumFunc()); got != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("prefix digest = %s, want %s", got, hex.EncodeToString(wantSum[:]))
	}
}

func TestChunkProgressStopWaitsForInFlightCallback(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	reader := &chunkProgressReader{
		r:        bytes.NewReader(bytes.Repeat([]byte{1}, 256<<10)),
		chunkLen: 256 << 10,
		total:    256 << 10,
		onProgress: func(sent, total int64) {
			once.Do(func() { close(entered) })
			<-release
		},
	}
	readDone := make(chan struct{})
	go func() {
		_, _ = reader.Read(make([]byte, 256<<10))
		close(readDone)
	}()
	<-entered

	stopDone := make(chan struct{})
	go func() {
		reader.stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("stop returned while a PATCH progress callback was still running")
	case <-time.After(100 * time.Millisecond):
	}
	releaseAll()
	<-stopDone
	<-readDone
}

type gatedCountingReader struct {
	mu      sync.Mutex
	reads   int
	data    []byte
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedCountingReader) Read(p []byte) (int, error) {
	g.mu.Lock()
	g.reads++
	g.mu.Unlock()
	g.once.Do(func() { close(g.entered) })
	<-g.release
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, g.data)
	g.data = g.data[n:]
	return n, nil
}

func (g *gatedCountingReader) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

func TestChunkProgressStopWaitsForInFlightReadAndRetiresBody(t *testing.T) {
	underlying := &gatedCountingReader{
		data:    bytes.Repeat([]byte{2}, 4096),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(underlying.release) }) }
	defer releaseAll()

	reader := &chunkProgressReader{r: underlying, chunkLen: 4096, total: 4096}
	readDone := make(chan struct{})
	go func() {
		_, _ = reader.Read(make([]byte, 4096))
		close(readDone)
	}()
	<-underlying.entered

	stopDone := make(chan struct{})
	go func() {
		reader.stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("stop returned while the PATCH body was still reading")
	case <-time.After(100 * time.Millisecond):
	}
	releaseAll()
	<-stopDone
	<-readDone

	before := underlying.count()
	n, err := reader.Read(make([]byte, 16))
	if n != 0 || err != io.EOF {
		t.Fatalf("retired body read = (%d, %v), want (0, EOF)", n, err)
	}
	if got := underlying.count(); got != before {
		t.Fatalf("retired body performed %d more underlying reads", got-before)
	}
}
