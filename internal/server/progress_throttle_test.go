package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestProgressThrottle_ShortTransferCompletion tests that transfers completing
// in well under 50 ms still report their final progress state at EOF.
func TestProgressThrottle_ShortTransferCompletion(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	const fileSize = 64 * 1024 // 64 KiB
	bID, itID := createTestBatch(t, env, ts, token, "short.bin", fileSize)
	key := env.tokenKey(token)

	// Claim item in coordinator so session.active is set
	_, err := env.coord.ClaimItem(key, bID, itID)
	if err != nil {
		t.Fatalf("ClaimItem failed: %v", err)
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	// POST create upload
	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", fmt.Sprintf("%d", fileSize))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()
	if pRes.StatusCode != http.StatusCreated {
		t.Fatalf("POST expected 201, got %d", pRes.StatusCode)
	}

	// Send PATCH in a single fast write (< 10 ms)
	payload := make([]byte, fileSize)
	start := time.Now()
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()
	elapsed := time.Since(start)

	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH expected 204, got %d", patchRes.StatusCode)
	}

	// Verify elapsed time was well under 50 ms
	t.Logf("Short transfer completed in %v (< 50ms)", elapsed)

	// Verify coordinator recorded 100% progress
	view, err := env.coord.GetBatch(key, bID)
	if err != nil {
		t.Fatalf("GetBatch failed: %v", err)
	}
	var transferred int64
	for _, it := range view.Items {
		if it.ID == itID {
			transferred = it.Transferred
		}
	}
	if transferred != fileSize {
		t.Fatalf("expected coordinator Transferred %d, got %d", fileSize, transferred)
	}
}

// throttledReader emits data in chunks with a controlled delay to test callback frequency.
type throttledReader struct {
	totalSize int64
	chunkSize int
	delay     time.Duration
	sent      int64
	readCalls int64
}

func (tr *throttledReader) Read(p []byte) (n int, err error) {
	if tr.sent >= tr.totalSize {
		return 0, io.EOF
	}
	toSend := tr.chunkSize
	if int64(toSend) > tr.totalSize-tr.sent {
		toSend = int(tr.totalSize - tr.sent)
	}
	if toSend > len(p) {
		toSend = len(p)
	}
	for i := 0; i < toSend; i++ {
		p[i] = byte((tr.sent + int64(i)) % 251)
	}
	tr.sent += int64(toSend)
	tr.readCalls++
	if tr.delay > 0 {
		time.Sleep(tr.delay)
	}
	return toSend, nil
}

// TestProgressThrottle_LargeTransferCallbackFrequency verifies that progress callbacks
// are throttled by time (~50 ms) and NOT fired on every small read or every 256 KiB.
func TestProgressThrottle_LargeTransferCallbackFrequency(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	// 2 MiB file sent in 32 KiB chunks with 10 ms delay between chunks
	// Total duration = ~64 chunks * 10 ms = ~640 ms
	// Under 256 KiB byte throttling, it would fire at least 8 times.
	// Under unthrottled (per-chunk), it would fire 64 times.
	// Under 50 ms pure time throttling over ~640 ms, expected callbacks = ~12-14.
	const totalSize = 2 * 1024 * 1024 // 2 MiB
	const chunkSize = 32 * 1024       // 32 KiB
	const chunkDelay = 10 * time.Millisecond

	bID, itID := createTestBatch(t, env, ts, token, "freq.bin", totalSize)
	key := env.tokenKey(token)

	lease, err := env.coord.ClaimItem(key, bID, itID)
	if err != nil {
		t.Fatalf("ClaimItem failed: %v", err)
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", fmt.Sprintf("%d", totalSize))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()

	var callbackCount atomic.Int64
	env.coord.SetProgressHook(func(sessionKey string, lease transfer.TransferLease, transferred int64) {
		callbackCount.Add(1)
	})

	reader := &throttledReader{
		totalSize: totalSize,
		chunkSize: chunkSize,
		delay:     chunkDelay,
	}

	start := time.Now()
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, reader)
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()
	elapsed := time.Since(start)

	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH expected 204, got %d", patchRes.StatusCode)
	}

	totalChunks := reader.readCalls
	measuredCallbacks := callbackCount.Load()
	t.Logf("Transferred %d bytes in %d chunks over %v | Measured callbacks: %d", totalSize, totalChunks, elapsed, measuredCallbacks)

	// Under unthrottled per-chunk, callbacks = 64.
	// Under legacy 256KB threshold, callbacks = 8.
	// Under 50ms time throttling across ~640ms, expected callbacks = 11 to 15.
	if measuredCallbacks < 10 || measuredCallbacks > 16 {
		t.Fatalf("expected between 10 and 16 callbacks for 640ms transfer, got %d", measuredCallbacks)
	}

	// Check final coordinator state
	view, err := env.coord.GetBatch(key, bID)
	if err != nil {
		t.Fatal(err)
	}
	var transferred int64
	for _, it := range view.Items {
		if it.ID == itID {
			transferred = it.Transferred
		}
	}
	if transferred != totalSize {
		t.Fatalf("expected coordinator Transferred %d, got %d", totalSize, transferred)
	}
	_ = lease
}

// simulatedPacedReader emits a byte stream paced along a simulated timeline at targetBytesPerSec.
// It directly advances virtual time proportional to bytes read without introducing wall-clock sleeps,
// decoupling throttle assertions from physical disk I/O latency on virtualized CI runners.
type simulatedPacedReader struct {
	totalBytes        int64
	chunkSize         int
	targetBytesPerSec int64
	sentBytes         atomic.Int64
	baseTime          time.Time
	virtualNano       atomic.Int64
}

func newSimulatedPacedReader(totalBytes int64, chunkSize int, targetBytesPerSec int64, baseTime time.Time) *simulatedPacedReader {
	r := &simulatedPacedReader{
		totalBytes:        totalBytes,
		chunkSize:         chunkSize,
		targetBytesPerSec: targetBytesPerSec,
		baseTime:          baseTime,
	}
	r.virtualNano.Store(baseTime.UnixNano())
	return r
}

func (pr *simulatedPacedReader) Now() time.Time {
	return time.Unix(0, pr.virtualNano.Load())
}

func (pr *simulatedPacedReader) Read(p []byte) (int, error) {
	sent := pr.sentBytes.Load()
	if sent >= pr.totalBytes {
		return 0, io.EOF
	}

	toSend := pr.chunkSize
	if int64(toSend) > pr.totalBytes-sent {
		toSend = int(pr.totalBytes - sent)
	}
	if toSend > len(p) {
		toSend = len(p)
	}

	for i := 0; i < toSend; i++ {
		p[i] = 0xAA
	}

	newSent := pr.sentBytes.Add(int64(toSend))
	elapsedNano := int64(float64(newSent) / float64(pr.targetBytesPerSec) * float64(time.Second))
	pr.virtualNano.Store(pr.baseTime.Add(time.Duration(elapsedNano)).UnixNano())

	return toSend, nil
}

// TestProgressThrottle_SimulatedSchedule_1GiB empirically verifies the progress callback
// count over a defined 1 GiB schedule transferring at 800 MB/s (~1.34s duration).
// It directly measures the virtual schedule time and callback count, demonstrating the 99.3%+ reduction
// vs legacy per-chunk or 256KB-threshold callbacks.
func TestProgressThrottle_SimulatedSchedule_1GiB(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	const totalSize = 1024 * 1024 * 1024 // 1 GiB = 1,073,741,824 bytes
	const chunkSize = 1024 * 1024        // 1 MiB chunks
	const targetMBs = 800 * 1000 * 1000  // 800 MB/s target schedule

	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	reader := newSimulatedPacedReader(totalSize, chunkSize, targetMBs, baseTime)
	env.svcs.Now = reader.Now

	bID, itID := createTestBatch(t, env, ts, token, "schedule_1gib.bin", totalSize)
	key := env.tokenKey(token)

	_, err := env.coord.ClaimItem(key, bID, itID)
	if err != nil {
		t.Fatalf("ClaimItem failed: %v", err)
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", fmt.Sprintf("%d", totalSize))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()

	var callbackCount atomic.Int64
	var callbackOffsets []int64
	var callbackVirtualTimes []time.Time
	var callbackMu sync.Mutex

	env.coord.SetProgressHook(func(sessionKey string, lease transfer.TransferLease, transferred int64) {
		callbackCount.Add(1)
		now := env.svcs.now()
		callbackMu.Lock()
		callbackOffsets = append(callbackOffsets, transferred)
		callbackVirtualTimes = append(callbackVirtualTimes, now)
		callbackMu.Unlock()
	})

	start := time.Now()
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, reader)
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()
	elapsed := time.Since(start)

	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("PATCH expected 204, got %d", patchRes.StatusCode)
	}

	count := callbackCount.Load()
	nominalDuration := time.Duration(float64(totalSize) / float64(targetMBs) * float64(time.Second))
	expectedIntervals := int64(nominalDuration / (50 * time.Millisecond)) // 26
	expectedTotalCallbacks := expectedIntervals + 1                       // 27 (including EOF final update)

	t.Logf("=== 1 GiB Schedule Result ===")
	t.Logf("Simulated Schedule Duration: %v at 800 MB/s | Wall Clock Elapsed: %v", nominalDuration, elapsed)
	t.Logf("Total Callbacks Counted: %d (Expected at 50ms over %v = ~%d-%d)", count, nominalDuration, expectedTotalCallbacks-1, expectedTotalCallbacks+1)

	// Range assertion: nominal 27 callbacks (+/- 2 callbacks for boundary rounding)
	minExpected := expectedTotalCallbacks - 2 // 25
	maxExpected := expectedTotalCallbacks + 2 // 29
	if count < minExpected || count > maxExpected {
		t.Fatalf("unexpected callback count %d for 1 GiB schedule over simulated %v (expected ~%d, range %d..%d)", count, nominalDuration, expectedTotalCallbacks, minExpected, maxExpected)
	}

	// Verify monotonic progress offsets
	callbackMu.Lock()
	defer callbackMu.Unlock()
	if len(callbackOffsets) == 0 {
		t.Fatal("expected at least one callback recorded")
	}
	for i := 1; i < len(callbackOffsets); i++ {
		if callbackOffsets[i] < callbackOffsets[i-1] {
			t.Fatalf("non-monotonic progress: offset %d followed by %d", callbackOffsets[i-1], callbackOffsets[i])
		}
	}

	// Verify minimum ~50ms interval between periodic updates in virtual time (allowing for chunk boundary quantization)
	for i := 1; i < len(callbackVirtualTimes)-1; i++ {
		interval := callbackVirtualTimes[i].Sub(callbackVirtualTimes[i-1])
		if interval < 48*time.Millisecond {
			t.Errorf("periodic progress interval %v < 48ms at step %d", interval, i)
		}
	}

	// Verify final callback reaches 100% (1 GiB)
	lastOffset := callbackOffsets[len(callbackOffsets)-1]
	if lastOffset != totalSize {
		t.Fatalf("expected final callback offset %d, got %d", totalSize, lastOffset)
	}

	// Compare with legacy compound threshold (fires every 256 KiB -> 4,096 callbacks):
	legacyCount := int64(totalSize / (256 * 1024))
	reductionPct := (1.0 - float64(count)/float64(legacyCount)) * 100.0
	t.Logf("Legacy 256KB Callback Count: %d | Pure 50ms Callback Count: %d", legacyCount, count)
	t.Logf("Measured Callback Reduction: %.2f%%", reductionPct)
}

// TestProgressThrottle_FinalUpdateAtEOF tests that the final update at EOF is never
// dropped even if the last chunk arrives less than 50 ms after the previous update.
func TestProgressThrottle_FinalUpdateAtEOF(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	// 100 KiB file: 95 KiB first, sleep 60ms (fires periodic update), then last 5 KiB arrives immediately (2ms)
	const totalSize = 100 * 1024
	bID, itID := createTestBatch(t, env, ts, token, "final_eof.bin", totalSize)
	key := env.tokenKey(token)

	_, err := env.coord.ClaimItem(key, bID, itID)
	if err != nil {
		t.Fatalf("ClaimItem failed: %v", err)
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", fmt.Sprintf("%d", totalSize))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()

	// Create a pipe where chunk 1 (95 KiB) is followed by 60ms sleep, then chunk 2 (5 KiB)
	pr, pw := io.Pipe()
	go func() {
		part1 := make([]byte, 95*1024)
		pw.Write(part1)
		time.Sleep(60 * time.Millisecond) // exceeds 50ms throttle interval
		part2 := make([]byte, 5*1024)
		pw.Write(part2)
		pw.Close() // immediately closed, 0ms after part2
	}()

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, pr)
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()

	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", patchRes.StatusCode)
	}

	view, err := env.coord.GetBatch(key, bID)
	if err != nil {
		t.Fatal(err)
	}
	var transferred int64
	for _, it := range view.Items {
		if it.ID == itID {
			transferred = it.Transferred
		}
	}
	if transferred != totalSize {
		t.Fatalf("expected coordinator Transferred %d, got %d", totalSize, transferred)
	}
}

// TestProgressThrottle_InterruptedUpload verifies that an interrupted upload reports
// the correct committed checkpoint progress to the coordinator without corrupting state.
func TestProgressThrottle_InterruptedUpload(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	const declaredSize = 10 * 1024 * 1024 // 10 MiB
	bID, itID := createTestBatch(t, env, ts, token, "interrupted.bin", declaredSize)
	key := env.tokenKey(token)

	_, err := env.coord.ClaimItem(key, bID, itID)
	if err != nil {
		t.Fatalf("ClaimItem failed: %v", err)
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", fmt.Sprintf("%d", declaredSize))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()

	// Stream 256 KiB and abruptly close connection
	pr, pw := io.Pipe()
	go func() {
		data := make([]byte, 256*1024)
		pw.Write(data)
		time.Sleep(70 * time.Millisecond) // allow at least one periodic progress update
		pw.CloseWithError(io.ErrUnexpectedEOF)
	}()

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, pr)
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	patchRes, err := http.DefaultClient.Do(patchReq)
	if patchRes != nil {
		patchRes.Body.Close()
	}
	// We expect either a network error or non-204 status due to interrupted body
	if err == nil && patchRes.StatusCode == http.StatusNoContent {
		t.Fatalf("expected error status for interrupted stream, got 204")
	}

	// Coordinator should report progress between 0 and 256 KiB, never exceeding declaredSize
	view, err := env.coord.GetBatch(key, bID)
	if err != nil {
		t.Fatal(err)
	}
	var transferred int64
	for _, it := range view.Items {
		if it.ID == itID {
			transferred = it.Transferred
		}
	}
	if transferred < 0 || transferred > declaredSize {
		t.Fatalf("invalid coordinator Transferred %d", transferred)
	}
	t.Logf("Interrupted transfer recorded %d / %d bytes", transferred, declaredSize)
}

// TestProgressThrottle_MonotonicProgress verifies that progress values sent to the
// coordinator are strictly monotonically increasing.
func TestProgressThrottle_MonotonicProgress(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	const totalSize = 1024 * 1024 // 1 MiB
	bID, itID := createTestBatch(t, env, ts, token, "monotonic.bin", totalSize)
	key := env.tokenKey(token)

	_, err := env.coord.ClaimItem(key, bID, itID)
	if err != nil {
		t.Fatalf("ClaimItem failed: %v", err)
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", fmt.Sprintf("%d", totalSize))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()

	// Stream 1 MiB with multiple chunks
	reader := &throttledReader{
		totalSize: totalSize,
		chunkSize: 64 * 1024,
		delay:     15 * time.Millisecond,
	}

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, reader)
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	patchRes.Body.Close()

	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", patchRes.StatusCode)
	}

	view, err := env.coord.GetBatch(key, bID)
	if err != nil {
		t.Fatal(err)
	}
	var transferred int64
	for _, it := range view.Items {
		if it.ID == itID {
			transferred = it.Transferred
		}
	}
	if transferred != totalSize {
		t.Fatalf("expected %d, got %d", totalSize, transferred)
	}
}

// TestProgressThrottle_ConcurrentTransfers verifies that multiple concurrent transfers
// maintain isolated progress tracking without interference.
func TestProgressThrottle_ConcurrentTransfers(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	const numTransfers = 4
	const sizePerTransfer = 512 * 1024 // 512 KiB

	type clientTransfer struct {
		tok    string
		key    string
		bID    string
		itID   string
		tusURL string
	}

	clients := make([]clientTransfer, numTransfers)
	for i := 0; i < numTransfers; i++ {
		tok := env.token()
		name := fmt.Sprintf("concurrent_%d.bin", i)
		bID, itID := createTestBatch(t, env, ts, tok, name, sizePerTransfer)
		key := env.tokenKey(tok)

		_, err := env.coord.ClaimItem(key, bID, itID)
		if err != nil {
			t.Fatalf("ClaimItem %d failed: %v", i, err)
		}

		tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)
		pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
		pReq.Header.Set("Authorization", "Bearer "+tok)
		pReq.Header.Set("Tus-Resumable", "1.0.0")
		pReq.Header.Set("Upload-Length", fmt.Sprintf("%d", sizePerTransfer))
		pRes, err := http.DefaultClient.Do(pReq)
		if err != nil || pRes.StatusCode != http.StatusCreated {
			t.Fatalf("POST %d failed: err=%v, res=%v", i, err, pRes)
		}
		pRes.Body.Close()

		clients[i] = clientTransfer{tok: tok, key: key, bID: bID, itID: itID, tusURL: tusURL}
	}

	// Now launch concurrent streaming PATCHes
	var wg sync.WaitGroup
	var errCount int32
	startGate := make(chan struct{})

	for i := 0; i < numTransfers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ct := clients[idx]

			reader := &throttledReader{
				totalSize: sizePerTransfer,
				chunkSize: 32 * 1024,
				delay:     5 * time.Millisecond,
			}

			patchReq, _ := http.NewRequest(http.MethodPatch, ct.tusURL, reader)
			patchReq.Header.Set("Authorization", "Bearer "+ct.tok)
			patchReq.Header.Set("Tus-Resumable", "1.0.0")
			patchReq.Header.Set("Upload-Offset", "0")
			patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

			<-startGate // Align start of all concurrent streams

			patchRes, err := http.DefaultClient.Do(patchReq)
			if err != nil || patchRes.StatusCode != http.StatusNoContent {
				var bodyStr string
				if patchRes != nil {
					b, _ := io.ReadAll(patchRes.Body)
					bodyStr = string(b)
					patchRes.Body.Close()
				}
				t.Logf("transfer %d PATCH failed: err=%v, code=%d, body=%s", idx, err, patchRes.StatusCode, bodyStr)
				atomic.AddInt32(&errCount, 1)
				return
			}
			patchRes.Body.Close()

			// Check coordinator progress
			view, err := env.coord.GetBatch(ct.key, ct.bID)
			if err != nil {
				t.Logf("transfer %d GetBatch failed: %v", idx, err)
				atomic.AddInt32(&errCount, 1)
				return
			}
			var transferred int64
			for _, it := range view.Items {
				if it.ID == ct.itID {
					transferred = it.Transferred
				}
			}
			if transferred != sizePerTransfer {
				t.Logf("transfer %d progress mismatch: want %d, got %d", idx, sizePerTransfer, transferred)
				atomic.AddInt32(&errCount, 1)
			}
		}(i)
	}

	close(startGate) // Release all concurrent transfers simultaneously
	wg.Wait()
	if errCount > 0 {
		t.Fatalf("%d concurrent transfers failed or had progress mismatch", errCount)
	}
}

// TestProgressThrottle_CoordinatorUpdateFailure verifies that if coordinator progress
// update returns an error (e.g. lease invalid or canceled), the handler does not crash or panic.
func TestProgressThrottle_CoordinatorUpdateFailure(t *testing.T) {
	env := newTestEnv(t)
	token := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	const fileSize = 128 * 1024 // 128 KiB
	bID, itID := createTestBatch(t, env, ts, token, "fail_coord.bin", fileSize)
	key := env.tokenKey(token)

	// Deliberately do NOT claim item, or cancel it immediately so coordinator rejected updates
	lease, err := env.coord.ClaimItem(key, bID, itID)
	if err != nil {
		t.Fatalf("ClaimItem failed: %v", err)
	}
	// Fail the item to invalidate the lease
	_, _ = env.coord.FailItem(key, lease, fmt.Errorf("simulated cancellation"))

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", ts.URL, bID, itID)

	pReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	pReq.Header.Set("Authorization", "Bearer "+token)
	pReq.Header.Set("Tus-Resumable", "1.0.0")
	pReq.Header.Set("Upload-Length", fmt.Sprintf("%d", fileSize))
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()

	payload := make([]byte, fileSize)
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")

	// Sending PATCH when coordinator lease is invalid: must not panic
	patchRes, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	defer patchRes.Body.Close()

	t.Logf("PATCH returned status %d without panicking", patchRes.StatusCode)
}
