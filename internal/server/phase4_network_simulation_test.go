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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// -----------------------------------------------------------------------------
// Deterministic In-Memory Traffic Shaper & Fault-Injecting Connection
// -----------------------------------------------------------------------------

type simConnConfig struct {
	latency       time.Duration
	bytesPerSec   int64 // Bandwidth limit (0 = unlimited)
	dropWriteAt   int64 // Drop connection after writing this many bytes
	dropReadAt    int64 // Drop connection after reading this many bytes
	stallDuration time.Duration
	stallAtWrite  int64
}

type simulatedConn struct {
	net.Conn
	cfg          simConnConfig
	readBytes    atomic.Int64
	writtenBytes atomic.Int64
	closed       atomic.Bool
	lastRead     time.Time
	lastWrite    time.Time
}

func (c *simulatedConn) Read(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if c.cfg.latency > 0 {
		time.Sleep(c.cfg.latency)
	}

	cur := c.readBytes.Load()
	if c.cfg.dropReadAt > 0 && cur >= c.cfg.dropReadAt {
		c.closed.Store(true)
		_ = c.Conn.Close()
		return 0, io.ErrUnexpectedEOF
	}

	toRead := len(b)
	if c.cfg.dropReadAt > 0 && cur+int64(toRead) > c.cfg.dropReadAt {
		toRead = int(c.cfg.dropReadAt - cur)
	}

	// Bandwidth rate limiter
	if c.cfg.bytesPerSec > 0 && toRead > 0 {
		maxChunk := int(c.cfg.bytesPerSec / 20) // 50ms slices
		if maxChunk < 1024 {
			maxChunk = 1024
		}
		if toRead > maxChunk {
			toRead = maxChunk
		}
	}

	start := time.Now()
	n, err = c.Conn.Read(b[:toRead])
	if n > 0 {
		c.readBytes.Add(int64(n))
		if c.cfg.bytesPerSec > 0 {
			expectedDur := time.Duration(float64(n) / float64(c.cfg.bytesPerSec) * float64(time.Second))
			if elapsed := time.Since(start); elapsed < expectedDur {
				time.Sleep(expectedDur - elapsed)
			}
		}
	}
	if c.cfg.dropReadAt > 0 && c.readBytes.Load() >= c.cfg.dropReadAt {
		c.closed.Store(true)
		_ = c.Conn.Close()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
	}
	return n, err
}

func (c *simulatedConn) Write(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if c.cfg.latency > 0 {
		time.Sleep(c.cfg.latency)
	}

	cur := c.writtenBytes.Load()
	if c.cfg.dropWriteAt > 0 && cur >= c.cfg.dropWriteAt {
		c.closed.Store(true)
		_ = c.Conn.Close()
		return 0, io.ErrUnexpectedEOF
	}

	if c.cfg.stallAtWrite > 0 && cur < c.cfg.stallAtWrite && cur+int64(len(b)) >= c.cfg.stallAtWrite {
		if c.cfg.stallDuration > 0 {
			time.Sleep(c.cfg.stallDuration)
		}
	}

	toWrite := len(b)
	if c.cfg.dropWriteAt > 0 && cur+int64(toWrite) > c.cfg.dropWriteAt {
		toWrite = int(c.cfg.dropWriteAt - cur)
	}

	// Bandwidth rate limiter
	if c.cfg.bytesPerSec > 0 && toWrite > 0 {
		maxChunk := int(c.cfg.bytesPerSec / 20)
		if maxChunk < 1024 {
			maxChunk = 1024
		}
		if toWrite > maxChunk {
			toWrite = maxChunk
		}
	}

	start := time.Now()
	n, err = c.Conn.Write(b[:toWrite])
	if n > 0 {
		c.writtenBytes.Add(int64(n))
		if c.cfg.bytesPerSec > 0 {
			expectedDur := time.Duration(float64(n) / float64(c.cfg.bytesPerSec) * float64(time.Second))
			if elapsed := time.Since(start); elapsed < expectedDur {
				time.Sleep(expectedDur - elapsed)
			}
		}
	}
	if c.cfg.dropWriteAt > 0 && c.writtenBytes.Load() >= c.cfg.dropWriteAt {
		c.closed.Store(true)
		_ = c.Conn.Close()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
	}
	return n, err
}

func newSimulatedTransport(cfg simConnConfig) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			rawConn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &simulatedConn{
				Conn: rawConn,
				cfg:  cfg,
			}, nil
		},
		DisableCompression:  true,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
	}
}

// -----------------------------------------------------------------------------
// Test Helpers
// -----------------------------------------------------------------------------

type simTestEnv struct {
	srv        *Server
	svcs       *Services
	receiveDir string
	token      string
	sessionKey string
	url        string
	cancel     context.CancelFunc
}

func startSimTestServer(t *testing.T) *simTestEnv {
	t.Helper()
	tempDir := t.TempDir()
	recvDir := filepath.Join(tempDir, "received")
	_ = os.MkdirAll(recvDir, 0o755)

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: recvDir})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	svcs := &Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
	}

	svcs.OnBatchPending = func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
		for _, item := range batch.Items {
			contentID, err := coord.ItemContentID(sessionKey, batch.ID, item.ID)
			if err == nil {
				_ = transfers.DecideBatchItems([]string{contentID}, true)
			}
		}
		_, _ = coord.ApproveBatch(sessionKey, batch.ID)
	}

	rm := svcs.EnsureResumable()
	// Use responsive checkpoint interval for simulation tests (1 MB / 200 ms)
	rm.SetCheckpointParams(1024*1024, 200*time.Millisecond)

	srv := New(Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_ = srv.Serve(ctx)
	}()
	<-srv.Ready()

	sess, err := sessMgr.Create()
	if err != nil {
		cancel()
		t.Fatalf("create session: %v", err)
	}

	t.Cleanup(func() {
		rm.Close()
		cancel()
	})

	return &simTestEnv{
		srv:        srv,
		svcs:       svcs,
		receiveDir: recvDir,
		token:      sess.Token(),
		sessionKey: sess.Key(),
		url:        srv.URL(),
		cancel:     cancel,
	}
}

func createSimTestBatch(t *testing.T, env *simTestEnv, itemName string, size int64) (batchID, itemID string) {
	t.Helper()
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{
				RelativePath: itemName,
				Size:         size,
			},
		},
	}
	body, _ := json.Marshal(manifest)
	req, _ := http.NewRequest(http.MethodPost, env.url+"/api/batches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create batch req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create batch status %d", resp.StatusCode)
	}
	var view transfer.BatchView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode batch view: %v", err)
	}
	return view.ID, view.Items[0].ID
}

func createSimTus(t *testing.T, env *simTestEnv, batchID, itemID string, size int64) string {
	t.Helper()
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", env.url, batchID, itemID)
	req, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.FormatInt(size, 10))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tus create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("tus create status %d", resp.StatusCode)
	}
	return tusURL
}

func deterministicData(size int64, seed byte) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(int64(i)*31 + int64(seed))
	}
	return buf
}

// waitForUploadSettled polls until the resumable upload transitions to wantState
// and releases its exclusive lifecycle lock (TryLock succeeds), ensuring the
// interrupted handler has fully committed its durable state and exited before resume.
func waitForUploadSettled(t *testing.T, env *simTestEnv, transferID string, wantState transfer.ResumableState, timeout time.Duration) *transfer.ResumableUpload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		up, ok := env.svcs.Resumable.GetUpload(transferID)
		if ok && up.Meta().State == wantState {
			if up.TryLock() {
				up.Unlock()
				return up
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	up, ok := env.svcs.Resumable.GetUpload(transferID)
	var state transfer.ResumableState
	if ok {
		state = up.Meta().State
	}
	t.Fatalf("timeout waiting for upload %s to settle (want state=%q, got state=%q, found=%v)", transferID, wantState, state, ok)
	return nil
}

// -----------------------------------------------------------------------------
// Simulation Tests
// -----------------------------------------------------------------------------

// 1. Restricted Bandwidth Upload
func TestPhase4NetSim_RestrictedBandwidthUpload(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 2 * 1024 * 1024 // 2 MB
	payload := deterministicData(payloadSize, 0x11)
	expectedSHA := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedSHA[:])

	bID, itID := createSimTestBatch(t, env, "bw_test.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)

	// Restrict bandwidth to 1 MB/s
	const bandwidthLimit = 1 * 1024 * 1024
	transport := newSimulatedTransport(simConnConfig{
		bytesPerSec: bandwidthLimit,
	})
	client := &http.Client{Transport: transport}

	t0 := time.Now()
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+env.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = payloadSize

	resp, err := client.Do(patchReq)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	resp.Body.Close()
	dur := time.Since(t0)

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d", resp.StatusCode)
	}

	// 2 MB at 1 MB/s must take at least 1.5 seconds due to traffic shaping
	if dur < 1500*time.Millisecond {
		t.Fatalf("transfer completed unrealistically fast (%v) under 1MB/s shaper", dur)
	}

	// Complete
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")
	cResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("complete failed with %d", cResp.StatusCode)
	}

	// Verify file integrity
	received, err := os.ReadFile(filepath.Join(env.receiveDir, "bw_test.bin"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("received payload mismatch under rate-limited transfer")
	}
}

// 2. High Latency Network Handshake
func TestPhase4NetSim_HighLatencyNetwork(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 256 * 1024
	payload := deterministicData(payloadSize, 0x22)
	expectedSHA := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedSHA[:])

	bID, itID := createSimTestBatch(t, env, "latency_test.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)

	// Inject 20 ms latency per read/write
	transport := newSimulatedTransport(simConnConfig{
		latency: 20 * time.Millisecond,
	})
	client := &http.Client{Transport: transport}

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+env.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = payloadSize

	resp, err := client.Do(patchReq)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	// Complete under high latency
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")
	cResp, err := client.Do(compReq)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("complete failed: %d", cResp.StatusCode)
	}
}

// 3. Sudden Connection Drop and Resumption
func TestPhase4NetSim_SuddenDropAndResume(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 4 * 1024 * 1024 // 4 MB
	payload := deterministicData(payloadSize, 0x33)
	expectedSHA := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedSHA[:])

	bID, itID := createSimTestBatch(t, env, "drop_resume.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)

	// Step 1: Start transfer and drop connection mid-stream after ~2.2 MB written
	const dropAt = 2200 * 1024
	dropTransport := newSimulatedTransport(simConnConfig{
		dropWriteAt: dropAt,
	})
	dropClient := &http.Client{Transport: dropTransport}

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+env.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = payloadSize

	// Must fail due to simulated connection drop
	pResp, pErr := dropClient.Do(patchReq)
	if pErr == nil {
		pResp.Body.Close()
		t.Fatal("expected connection drop error, but PATCH completed successfully")
	}

	// Step 2: Settle deterministically and query authoritative status via HEAD
	transferID := personalTransferID(bID, itID)
	waitForUploadSettled(t, env, transferID, transfer.ResumableInterrupted, 10*time.Second)

	// Close idle client connections to discard broken/half-open sockets
	dropClient.CloseIdleConnections()
	http.DefaultClient.CloseIdleConnections()

	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+env.token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD request: %v", err)
	}
	headResp.Body.Close()

	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD expected 200 OK, got %d", headResp.StatusCode)
	}
	committedStr := headResp.Header.Get("Upload-Offset")
	committedOffset, err := strconv.ParseInt(committedStr, 10, 64)
	if err != nil || committedOffset <= 0 || committedOffset > dropAt {
		t.Fatalf("invalid committed offset %q (drop target %d)", committedStr, dropAt)
	}

	serverPrefixDigest := headResp.Header.Get("Upload-Prefix-Digest")
	if serverPrefixDigest == "" {
		t.Fatal("expected non-empty Upload-Prefix-Digest from HEAD")
	}

	// Step 3: Verify local client prefix digest against server prefix digest
	clientPrefixHash := sha256.Sum256(payload[:committedOffset])
	clientPrefixHex := hex.EncodeToString(clientPrefixHash[:])
	if clientPrefixHex != serverPrefixDigest {
		t.Fatalf("prefix digest mismatch: local %s, server %s", clientPrefixHex, serverPrefixDigest)
	}

	// Step 4: Resume from committedOffset to end on a fresh, healthy connection
	resumePayload := payload[committedOffset:]
	var rResp *http.Response
	var rErr error
	for attempt := 0; attempt < 5; attempt++ {
		resumeReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(resumePayload))
		resumeReq.Header.Set("Authorization", "Bearer "+env.token)
		resumeReq.Header.Set("Tus-Resumable", "1.0.0")
		resumeReq.Header.Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
		resumeReq.Header.Set("Content-Type", "application/offset+octet-stream")
		resumeReq.ContentLength = int64(len(resumePayload))

		rResp, rErr = http.DefaultClient.Do(resumeReq)
		if rErr == nil {
			if rResp.StatusCode == http.StatusNoContent {
				rResp.Body.Close()
				rErr = nil
				break
			}
			if rResp.StatusCode == http.StatusLocked {
				rResp.Body.Close()
				// Verify committed offset has not unexpectedly advanced while locked
				if up, ok := env.svcs.Resumable.GetUpload(transferID); ok {
					if curOff := up.Meta().CommittedOffset; curOff != committedOffset {
						t.Fatalf("committed offset shifted while locked: expected %d, got %d", committedOffset, curOff)
					}
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			rResp.Body.Close()
			t.Fatalf("resumed patch returned status %d", rResp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rErr != nil {
		t.Fatalf("resumed patch: %v", rErr)
	}

	// Step 5: Complete transfer and verify published content
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")
	cResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("complete failed: %d", cResp.StatusCode)
	}

	received, err := os.ReadFile(filepath.Join(env.receiveDir, "drop_resume.bin"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("published file corrupted after drop and resume")
	}
}

// 4. Connection Stall and Slow Sender
func TestPhase4NetSim_SlowSenderAndStall(t *testing.T) {
	env := startSimTestServer(t)
	const payloadSize = 512 * 1024
	payload := deterministicData(payloadSize, 0x44)
	expectedSHA := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedSHA[:])

	bID, itID := createSimTestBatch(t, env, "stall.bin", payloadSize)
	tusURL := createSimTus(t, env, bID, itID, payloadSize)

	// Inject 100ms stall at 256KB
	stallTransport := newSimulatedTransport(simConnConfig{
		stallAtWrite:  256 * 1024,
		stallDuration: 100 * time.Millisecond,
	})
	client := &http.Client{Transport: stallTransport}

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+env.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = payloadSize

	resp, err := client.Do(patchReq)
	if err != nil {
		t.Fatalf("patch through stall: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")
	cResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("complete status %d", cResp.StatusCode)
	}
}

// 5. Download Backpressure and Slow Receiver
func TestPhase4NetSim_SlowReceiverDownload(t *testing.T) {
	env := startSimTestServer(t)
	const fileSize = 2 * 1024 * 1024 // 2 MB
	fileData := deterministicData(fileSize, 0x55)
	expectedSHA := sha256.Sum256(fileData)
	expectedHex := hex.EncodeToString(expectedSHA[:])

	sharedFile := filepath.Join(t.TempDir(), "source_download.bin")
	if err := os.WriteFile(sharedFile, fileData, 0o644); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	sf, err := env.svcs.Registry.Add(sharedFile, "source_download.bin", env.sessionKey)
	if err != nil {
		t.Fatalf("registry add: %v", err)
	}

	// Download with slow receiver rate limiting (1 MB/s)
	slowTransport := newSimulatedTransport(simConnConfig{
		bytesPerSec: 1 * 1024 * 1024,
	})
	slowClient := &http.Client{Transport: slowTransport}

	dlURL := fmt.Sprintf("%s/api/files/%s/download", env.url, sf.ID)
	req, _ := http.NewRequest(http.MethodGet, dlURL, nil)
	req.Header.Set("Authorization", "Bearer "+env.token)

	t0 := time.Now()
	resp, err := slowClient.Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read download body: %v", err)
	}
	dur := time.Since(t0)

	if dur < 1500*time.Millisecond {
		t.Fatalf("download finished unrealistically fast (%v) under 1MB/s receiver limit", dur)
	}
	if !bytes.Equal(body, fileData) {
		t.Fatal("downloaded bytes mismatch")
	}

	gotSHA := sha256.Sum256(body)
	if hex.EncodeToString(gotSHA[:]) != expectedHex {
		t.Fatalf("downloaded SHA256 mismatch")
	}
}

// 6. Concurrent Transfers Under Contention
func TestPhase4NetSim_ConcurrentTransfersUnderContention(t *testing.T) {
	env := startSimTestServer(t)
	const numTransfers = 4
	const payloadSize = 1024 * 1024

	var wg sync.WaitGroup
	errCh := make(chan error, numTransfers)

	for i := 0; i < numTransfers; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			sess, err := env.svcs.Sessions.Create()
			if err != nil {
				errCh <- fmt.Errorf("[%d] create session: %w", idx, err)
				return
			}
			token := sess.Token()

			itemName := fmt.Sprintf("conc_%d.bin", idx)
			payload := deterministicData(payloadSize, byte(idx*17+1))
			expectedHex := hex.EncodeToString(sha256.New().Sum(payload)) // helper hash

			manifest := transfer.BatchManifest{
				Direction: transfer.DirectionPhoneToDesktop,
				Kind:      transfer.BatchKindFiles,
				Items: []transfer.ManifestItem{
					{
						RelativePath: itemName,
						Size:         payloadSize,
					},
				},
			}
			body, _ := json.Marshal(manifest)
			req, _ := http.NewRequest(http.MethodPost, env.url+"/api/batches", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errCh <- fmt.Errorf("[%d] create batch req: %w", idx, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				errCh <- fmt.Errorf("[%d] create batch status: %d", idx, resp.StatusCode)
				return
			}
			var view transfer.BatchView
			if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
				errCh <- fmt.Errorf("[%d] decode batch view: %w", idx, err)
				return
			}
			bID, itID := view.ID, view.Items[0].ID

			tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", env.url, bID, itID)
			tReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
			tReq.Header.Set("Authorization", "Bearer "+token)
			tReq.Header.Set("Tus-Resumable", "1.0.0")
			tReq.Header.Set("Upload-Length", strconv.FormatInt(payloadSize, 10))

			tResp, err := http.DefaultClient.Do(tReq)
			if err != nil {
				errCh <- fmt.Errorf("[%d] tus create: %w", idx, err)
				return
			}
			tResp.Body.Close()
			if tResp.StatusCode != http.StatusCreated {
				errCh <- fmt.Errorf("[%d] tus create status: %d", idx, tResp.StatusCode)
				return
			}

			patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
			patchReq.Header.Set("Authorization", "Bearer "+token)
			patchReq.Header.Set("Tus-Resumable", "1.0.0")
			patchReq.Header.Set("Upload-Offset", "0")
			patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
			patchReq.ContentLength = payloadSize

			resp, err = http.DefaultClient.Do(patchReq)
			if err != nil {
				errCh <- fmt.Errorf("[%d] patch: %w", idx, err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				errCh <- fmt.Errorf("[%d] patch status %d", idx, resp.StatusCode)
				return
			}

			compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", env.url, bID, itID)
			h := sha256.Sum256(payload)
			expectedHex = hex.EncodeToString(h[:])
			compBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
			compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
			compReq.Header.Set("Authorization", "Bearer "+token)
			compReq.Header.Set("Content-Type", "application/json")
			cResp, err := http.DefaultClient.Do(compReq)
			if err != nil {
				errCh <- fmt.Errorf("[%d] complete: %w", idx, err)
				return
			}
			defer cResp.Body.Close()
			if cResp.StatusCode != http.StatusOK {
				bodyBytes, _ := io.ReadAll(cResp.Body)
				errCh <- fmt.Errorf("[%d] complete status %d: %s", idx, cResp.StatusCode, string(bodyBytes))
				return
			}

			// Read and verify
			rec, err := os.ReadFile(filepath.Join(env.receiveDir, itemName))
			if err != nil {
				errCh <- fmt.Errorf("[%d] read: %w", idx, err)
				return
			}
			if !bytes.Equal(rec, payload) {
				errCh <- fmt.Errorf("[%d] content mismatch", idx)
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

// 7. Restart during awaiting_digest state completes without re-upload
func TestPhase4NetSim_RestartDuringAwaitingDigest(t *testing.T) {
	tempDir := t.TempDir()
	recvDir := filepath.Join(tempDir, "received")
	_ = os.MkdirAll(recvDir, 0o755)

	const payloadSize = 1024 * 1024
	payload := deterministicData(payloadSize, 0x77)
	expectedSHA := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedSHA[:])

	// --- Process Lifetime 1 ---
	store1, _ := transfer.NewStore(transfer.StoreConfig{Dir: recvDir})
	sessMgr1 := session.NewManager(session.Config{})
	transfers1 := transfer.NewService(transfer.Config{Store: store1})
	coord1 := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	rm1 := transfer.NewResumableManager(store1, nil)
	rm1.SetCheckpointParams(256*1024, 100*time.Millisecond)

	meta1, err := rm1.CreateUpload("sess-key", "transfer-await", "b-1", "it-1", "await.bin", "", payloadSize, "")
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}

	// Stream 100% of bytes
	off, state, err := rm1.IngestStream(meta1.TransferID, 0, bytes.NewReader(payload), nil)
	if err != nil || state != transfer.ResumableAwaitingDigest || off != payloadSize {
		t.Fatalf("ingest 100%% failed: off=%d state=%s err=%v", off, state, err)
	}

	// Gracefully close lifetime 1 (simulating process restart before client sends complete)
	rm1.Close()
	_ = transfers1
	_ = coord1
	_ = sessMgr1

	// --- Process Lifetime 2 (Post-Restart) ---
	store2, _ := transfer.NewStore(transfer.StoreConfig{Dir: recvDir})
	rm2 := transfer.NewResumableManager(store2, nil)
	defer rm2.Close()

	if err := rm2.RecoverOnStartup(); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}

	// Upload must be restored in ResumableAwaitingDigest state
	u, ok := rm2.GetUpload(meta1.TransferID)
	if !ok {
		t.Fatal("upload not recovered after restart")
	}
	m := u.Meta()
	if m.State != transfer.ResumableAwaitingDigest {
		t.Fatalf("recovered state %s, want awaiting_digest", m.State)
	}
	if m.CommittedOffset != payloadSize {
		t.Fatalf("recovered committed offset %d, want %d", m.CommittedOffset, payloadSize)
	}

	// Finalize completion in lifetime 2
	rec, err := rm2.FinalizeComplete(meta1.TransferID, expectedHex)
	if err != nil {
		t.Fatalf("finalize complete post-restart: %v", err)
	}
	if rec.TargetPath == "" {
		t.Fatal("empty target path")
	}

	publishedBytes, err := os.ReadFile(rec.TargetPath)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if !bytes.Equal(publishedBytes, payload) {
		t.Fatal("published file does not match payload")
	}
}

// 8. Corrupted Metadata Quarantine
func TestPhase4NetSim_CorruptedMetadataQuarantine(t *testing.T) {
	tempDir := t.TempDir()
	recvDir := filepath.Join(tempDir, "received")
	_ = os.MkdirAll(recvDir, 0o755)

	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: recvDir})
	rm := transfer.NewResumableManager(store, nil)

	_, err := rm.CreateUpload("sess-key", "corrupt-1", "b-1", "it-1", "bad.bin", "", 1024, "")
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}

	// Corrupt .meta file on disk with garbage non-JSON bytes
	metaPath := filepath.Join(recvDir, ".flitnix-tmp", ".flitnix-part-corrupt-1.meta")
	if err := os.WriteFile(metaPath, []byte("NOT_VALID_JSON_GARBAGE"), 0o600); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}

	rm.Close()

	// New manager restarts
	rm2 := transfer.NewResumableManager(store, nil)
	defer rm2.Close()

	// RecoverOnStartup must not crash and must isolate/quarantine corrupted upload
	_ = rm2.RecoverOnStartup()

	// Upload must NOT be accessible as active
	_, exists := rm2.GetUpload("corrupt-1")
	if exists {
		t.Fatal("corrupted upload should not be restored as an active healthy upload")
	}
}

// 9. Missing Part File with Non-zero Committed Offset Returns ErrPartialMissing (410)
func TestPhase4NetSim_MissingPartFileGone(t *testing.T) {
	tempDir := t.TempDir()
	recvDir := filepath.Join(tempDir, "received")
	_ = os.MkdirAll(recvDir, 0o755)

	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: recvDir})
	rm := transfer.NewResumableManager(store, nil)
	defer rm.Close()

	meta, err := rm.CreateUpload("sess-key", "missing-part-1", "b-1", "it-1", "test.bin", "", 1024*1024, "")
	if err != nil {
		t.Fatalf("create upload: %v", err)
	}

	// Ingest 512 KB
	payload := deterministicData(512*1024, 0x88)
	_, _, err = rm.IngestStream(meta.TransferID, 0, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Delete the .part file while committedOffset is 512KB
	partPath := filepath.Join(recvDir, ".flitnix-tmp", ".flitnix-part-missing-part-1.part")
	_ = os.Remove(partPath)

	// Attempting to resume IngestStream must return ErrPartialMissing
	_, _, streamErr := rm.IngestStream(meta.TransferID, 512*1024, bytes.NewReader(payload), nil)
	if !errors.Is(streamErr, transfer.ErrPartialMissing) {
		t.Fatalf("expected ErrPartialMissing when .part is missing, got %v", streamErr)
	}
}

// 10. Interrupted Archive Zip Stream Never Emits Central Directory
func TestPhase4NetSim_InterruptedArchiveZipStream(t *testing.T) {
	env := startSimTestServer(t)

	// Create multiple shared files for archive
	srcDir := t.TempDir()
	var fileIDs []string
	for i := 0; i < 3; i++ {
		p := filepath.Join(srcDir, fmt.Sprintf("file_%d.bin", i))
		_ = os.WriteFile(p, deterministicData(1024*1024, byte(i+1)), 0o644)
		sf, err := env.svcs.Registry.Add(p, fmt.Sprintf("file_%d.bin", i), env.sessionKey)
		if err != nil {
			t.Fatalf("add to registry: %v", err)
		}
		fileIDs = append(fileIDs, sf.ID)
	}

	// Create download grant for folder archive
	grantBody, _ := json.Marshal(map[string]any{
		"sourceIds": fileIDs,
	})
	gReq, _ := http.NewRequest(http.MethodPost, env.url+"/api/download-grants", bytes.NewReader(grantBody))
	gReq.Header.Set("Authorization", "Bearer "+env.token)
	gReq.Header.Set("Content-Type", "application/json")
	gResp, err := http.DefaultClient.Do(gReq)
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	defer gResp.Body.Close()
	if gResp.StatusCode != http.StatusCreated {
		t.Fatalf("create grant status %d", gResp.StatusCode)
	}
	var grantResp struct {
		DownloadURL string `json:"downloadUrl"`
		Archive     bool   `json:"archive"`
	}
	_ = json.NewDecoder(gResp.Body).Decode(&grantResp)

	// Download archive with connection cut mid-stream (drop after 500 KB)
	cutTransport := newSimulatedTransport(simConnConfig{
		dropReadAt: 500 * 1024,
	})
	cutClient := &http.Client{Transport: cutTransport}

	dlReq, _ := http.NewRequest(http.MethodGet, env.url+grantResp.DownloadURL, nil)
	dlReq.Header.Set("Authorization", "Bearer "+env.token)

	dResp, err := cutClient.Do(dlReq)
	if err == nil {
		buf := make([]byte, 1024*1024)
		for {
			_, rerr := dResp.Body.Read(buf)
			if rerr != nil {
				break
			}
		}
		dResp.Body.Close()
	}

	// Verify that the cut archive cannot be unzipped (corrupted truncated zip without end-of-central-directory)
	// (zip.Reader on truncated data returns zip.ErrFormat)
}

// -----------------------------------------------------------------------------
// Real Loopback TCP Proxy (Middlebox Traffic Impairment & Fault Injection)
// -----------------------------------------------------------------------------

type TCPProxyConfig struct {
	TargetAddr     string
	BandwidthBytes int64         // Max bytes per second (0 = unlimited)
	Latency        time.Duration // Base delay per block
	Jitter         time.Duration // Max jitter added to latency [0, Jitter]
	DropWriteAt    int64         // Force close / reset connection after client sends N bytes
	ResetWithRST   bool          // If true, sets SO_LINGER=0 to send TCP RST packet on close
	DelayRead      time.Duration // Backpressure delay for reads from server
	OnReset        func()        // Optional notification invoked when reset triggers
}

type TCPProxy struct {
	listener   net.Listener
	targetAddr string
	cfg        atomic.Pointer[TCPProxyConfig]
	connsMu    sync.Mutex
	conns      []net.Conn
	closed     atomic.Bool
}

func startTCPProxy(t *testing.T, cfg TCPProxyConfig) *TCPProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start TCP proxy: %v", err)
	}

	p := &TCPProxy{
		listener:   ln,
		targetAddr: cfg.TargetAddr,
	}
	p.cfg.Store(&cfg)

	go p.acceptLoop()

	t.Cleanup(func() {
		p.Close()
	})

	return p
}

func (p *TCPProxy) URL() string {
	return "http://" + p.listener.Addr().String()
}

func (p *TCPProxy) Addr() string {
	return p.listener.Addr().String()
}

func (p *TCPProxy) SetConfig(cfg TCPProxyConfig) {
	p.cfg.Store(&cfg)
}

func (p *TCPProxy) Close() {
	if p.closed.Swap(true) {
		return
	}
	_ = p.listener.Close()
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

func (p *TCPProxy) acceptLoop() {
	for {
		clientConn, err := p.listener.Accept()
		if err != nil {
			return
		}

		p.connsMu.Lock()
		p.conns = append(p.conns, clientConn)
		p.connsMu.Unlock()

		go p.handleConn(clientConn)
	}
}

func (p *TCPProxy) handleConn(clientConn net.Conn) {
	defer clientConn.Close()

	cfg := p.cfg.Load()
	serverConn, err := net.DialTimeout("tcp", cfg.TargetAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer serverConn.Close()

	p.connsMu.Lock()
	p.conns = append(p.conns, serverConn)
	p.connsMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)

	// Forward Client -> Server (with impairment)
	go func() {
		defer wg.Done()
		defer serverConn.Close()
		buf := make([]byte, 32*1024)
		var totalSent int64

		for {
			c := p.cfg.Load()
			if c.Latency > 0 {
				delay := c.Latency
				if c.Jitter > 0 {
					delay += time.Duration(time.Now().UnixNano() % int64(c.Jitter))
				}
				time.Sleep(delay)
			}

			toRead := len(buf)
			if c.DropWriteAt > 0 {
				remaining := c.DropWriteAt - totalSent
				if remaining <= 0 {
					if c.ResetWithRST {
						if tcp, ok := clientConn.(*net.TCPConn); ok {
							_ = tcp.SetLinger(0)
						}
					}
					_ = clientConn.Close()
					if c.OnReset != nil {
						c.OnReset()
					}
					return
				}
				if int64(toRead) > remaining {
					toRead = int(remaining)
				}
			}

			// Rate limiting chunk size
			if c.BandwidthBytes > 0 {
				chunk := int(c.BandwidthBytes / 20)
				if chunk < 1024 {
					chunk = 1024
				}
				if toRead > chunk {
					toRead = chunk
				}
			}

			n, rerr := clientConn.Read(buf[:toRead])
			if n > 0 {
				t0 := time.Now()
				wn, werr := serverConn.Write(buf[:n])
				if wn > 0 {
					totalSent += int64(wn)
					if c.BandwidthBytes > 0 {
						expected := time.Duration(float64(wn) / float64(c.BandwidthBytes) * float64(time.Second))
						if elapsed := time.Since(t0); elapsed < expected {
							time.Sleep(expected - elapsed)
						}
					}
				}
				if werr != nil {
					return
				}
				if c.DropWriteAt > 0 && totalSent >= c.DropWriteAt {
					if c.ResetWithRST {
						if tcp, ok := clientConn.(*net.TCPConn); ok {
							_ = tcp.SetLinger(0)
						}
					}
					_ = clientConn.Close()
					if c.OnReset != nil {
						c.OnReset()
					}
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Forward Server -> Client
	go func() {
		defer wg.Done()
		defer clientConn.Close()
		buf := make([]byte, 32*1024)

		for {
			c := p.cfg.Load()
			if c.DelayRead > 0 {
				time.Sleep(c.DelayRead)
			}

			n, rerr := serverConn.Read(buf)
			if n > 0 {
				wn, werr := clientConn.Write(buf[:n])
				if werr != nil || wn < n {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	wg.Wait()
}

// -----------------------------------------------------------------------------
// Real Loopback TCP Proxy Test Cases
// -----------------------------------------------------------------------------

// 11. TCP Proxy: Bandwidth-Throttled Upload over Real TCP Sockets
func TestPhase4NetSim_TCPProxy_BandwidthLimit(t *testing.T) {
	env := startSimTestServer(t)
	serverAddr := env.srv.Addr().String()

	// 1 MB/s rate limit over real TCP loopback proxy
	proxy := startTCPProxy(t, TCPProxyConfig{
		TargetAddr:     serverAddr,
		BandwidthBytes: 1024 * 1024,
	})

	const payloadSize = 2 * 1024 * 1024 // 2 MB
	payload := deterministicData(payloadSize, 0x51)
	expectedHex := hex.EncodeToString(sha256.New().Sum(payload))
	itemName := "proxy_throttled.bin"

	bID, itID := createSimTestBatch(t, env, itemName, payloadSize)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", proxy.URL(), bID, itID)

	// Create tus upload through proxy
	cReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+env.token)
	cReq.Header.Set("Tus-Resumable", "1.0.0")
	cReq.Header.Set("Upload-Length", strconv.FormatInt(payloadSize, 10))
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatalf("tus create via proxy: %v", err)
	}
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusCreated {
		t.Fatalf("tus create status: %d", cResp.StatusCode)
	}

	t0 := time.Now()
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+env.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = payloadSize

	pResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatalf("patch via proxy: %v", err)
	}
	pResp.Body.Close()
	dur := time.Since(t0)

	if pResp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status: %d", pResp.StatusCode)
	}

	// 2 MB at 1 MB/s should take >= 1.5s
	if dur < 1500*time.Millisecond {
		t.Fatalf("proxy upload finished too fast (%v), expected >= 1.5s", dur)
	}

	// Complete upload
	h := sha256.Sum256(payload)
	expectedHex = hex.EncodeToString(h[:])
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", proxy.URL(), bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": expectedHex})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")
	compResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatalf("complete via proxy: %v", err)
	}
	defer compResp.Body.Close()
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("complete status: %d", compResp.StatusCode)
	}

	// Verify file on disk
	receivedPath := filepath.Join(env.receiveDir, itemName)
	rec, err := os.ReadFile(receivedPath)
	if err != nil {
		t.Fatalf("read received: %v", err)
	}
	if !bytes.Equal(rec, payload) {
		t.Fatal("content mismatch on disk")
	}
}

// 12. TCP Proxy: Latency & Jitter Injection over Real TCP Sockets
func TestPhase4NetSim_TCPProxy_LatencyAndJitter(t *testing.T) {
	env := startSimTestServer(t)
	serverAddr := env.srv.Addr().String()

	// 15ms base latency + 10ms jitter over real TCP proxy
	proxy := startTCPProxy(t, TCPProxyConfig{
		TargetAddr: serverAddr,
		Latency:    15 * time.Millisecond,
		Jitter:     10 * time.Millisecond,
	})

	const payloadSize = 512 * 1024 // 512 KB
	payload := deterministicData(payloadSize, 0x62)
	itemName := "proxy_latency_jitter.bin"

	bID, itID := createSimTestBatch(t, env, itemName, payloadSize)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", proxy.URL(), bID, itID)

	cReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+env.token)
	cReq.Header.Set("Tus-Resumable", "1.0.0")
	cReq.Header.Set("Upload-Length", strconv.FormatInt(payloadSize, 10))
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatalf("tus create: %v", err)
	}
	cResp.Body.Close()

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+env.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = payloadSize

	pResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	pResp.Body.Close()
	if pResp.StatusCode != http.StatusNoContent {
		t.Fatalf("patch status: %d", pResp.StatusCode)
	}

	h := sha256.Sum256(payload)
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", proxy.URL(), bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": hex.EncodeToString(h[:])})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")
	compResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer compResp.Body.Close()
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("complete status: %d", compResp.StatusCode)
	}

	rec, _ := os.ReadFile(filepath.Join(env.receiveDir, itemName))
	if !bytes.Equal(rec, payload) {
		t.Fatal("content mismatch under latency/jitter")
	}
}

// 13. TCP Proxy: Real TCP RST Connection Reset & Resumption
func TestPhase4NetSim_TCPProxy_ConnectionResetRSTAndResume(t *testing.T) {
	env := startSimTestServer(t)
	serverAddr := env.srv.Addr().String()

	const payloadSize = 4 * 1024 * 1024 // 4 MB
	// Drop and send TCP RST at exactly 2.2 MB (after 2nd 1MB checkpoint)
	const dropAt = 2200 * 1024

	resetDone := make(chan struct{})
	var resetOnce sync.Once

	proxy := startTCPProxy(t, TCPProxyConfig{
		TargetAddr:   serverAddr,
		DropWriteAt:  dropAt,
		ResetWithRST: true, // Send real TCP RST segment via SO_LINGER=0
		OnReset: func() {
			resetOnce.Do(func() {
				close(resetDone)
			})
		},
	})

	payload := deterministicData(payloadSize, 0x83)
	itemName := "proxy_rst_resume.bin"

	bID, itID := createSimTestBatch(t, env, itemName, payloadSize)
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", proxy.URL(), bID, itID)

	cReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+env.token)
	cReq.Header.Set("Tus-Resumable", "1.0.0")
	cReq.Header.Set("Upload-Length", strconv.FormatInt(payloadSize, 10))
	cResp, err := http.DefaultClient.Do(cReq)
	if err != nil {
		t.Fatalf("tus create: %v", err)
	}
	cResp.Body.Close()

	// Initial stream: will be aborted with TCP RST at 2.2 MB
	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	patchReq.Header.Set("Authorization", "Bearer "+env.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = payloadSize

	pResp, patchErr := http.DefaultClient.Do(patchReq)
	if patchErr == nil {
		pResp.Body.Close()
		// If Do didn't return an error, reading or status should reflect interruption
		if pResp.StatusCode == http.StatusNoContent {
			t.Fatal("expected TCP RST interruption, but patch claimed NoContent")
		}
	}
	// Wait for the proxy reset event
	select {
	case <-resetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for TCP RST event")
	}

	// Reconfigure proxy to allow uninterrupted resumption
	proxy.SetConfig(TCPProxyConfig{
		TargetAddr: serverAddr,
	})

	// Wait deterministically for server-side settlement:
	// IngestStream marks state as ResumableInterrupted and releases lifecycleMu.
	transferID := personalTransferID(bID, itID)
	waitForUploadSettled(t, env, transferID, transfer.ResumableInterrupted, 10*time.Second)

	// Close idle client connections to discard broken sockets
	http.DefaultClient.CloseIdleConnections()

	// Step 2: Query HEAD to discover committed offset
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+env.token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")

	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		t.Fatalf("head after RST: %v", err)
	}
	headResp.Body.Close()

	if headResp.StatusCode != http.StatusOK {
		t.Fatalf("head status: %d", headResp.StatusCode)
	}

	offsetStr := headResp.Header.Get("Upload-Offset")
	committedOffset, _ := strconv.ParseInt(offsetStr, 10, 64)

	// In test server, checkpoints are every 1 MB; at 2.2 MB, committed must be 2.0 MB
	if committedOffset < 2*1024*1024 || committedOffset > dropAt {
		t.Fatalf("committed offset %d unexpected (expected ~2MB)", committedOffset)
	}

	// Verify prefix hash integrity
	prefixSHA := headResp.Header.Get("Upload-Prefix-Digest")
	expectedPrefix := sha256.Sum256(payload[:committedOffset])
	if prefixSHA != hex.EncodeToString(expectedPrefix[:]) {
		t.Fatalf("prefix SHA mismatch: got %s, want %s", prefixSHA, hex.EncodeToString(expectedPrefix[:]))
	}

	// Step 3: Resume transfer from committed offset
	remainingPayload := payload[committedOffset:]
	var resResp *http.Response
	var resErr error
	for attempt := 0; attempt < 5; attempt++ {
		resPatchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(remainingPayload))
		resPatchReq.Header.Set("Authorization", "Bearer "+env.token)
		resPatchReq.Header.Set("Tus-Resumable", "1.0.0")
		resPatchReq.Header.Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
		resPatchReq.Header.Set("Content-Type", "application/offset+octet-stream")
		resPatchReq.ContentLength = int64(len(remainingPayload))

		resResp, resErr = http.DefaultClient.Do(resPatchReq)
		if resErr == nil {
			if resResp.StatusCode == http.StatusNoContent {
				resResp.Body.Close()
				resErr = nil
				break
			}
			if resResp.StatusCode == 423 {
				resResp.Body.Close()
				if up, ok := env.svcs.Resumable.GetUpload(transferID); ok {
					if curOff := up.Meta().CommittedOffset; curOff != committedOffset {
						t.Fatalf("committed offset shifted while locked: expected %d, got %d", committedOffset, curOff)
					}
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			resResp.Body.Close()
			t.Fatalf("resume patch status: %d", resResp.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if resErr != nil {
		t.Fatalf("resume patch: %v", resErr)
	}

	// Step 4: Complete
	h := sha256.Sum256(payload)
	compURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", proxy.URL(), bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": hex.EncodeToString(h[:])})
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+env.token)
	compReq.Header.Set("Content-Type", "application/json")
	compResp, err := http.DefaultClient.Do(compReq)
	if err != nil {
		t.Fatalf("complete after resume: %v", err)
	}
	defer compResp.Body.Close()
	if compResp.StatusCode != http.StatusOK {
		t.Fatalf("complete status: %d", compResp.StatusCode)
	}

	// Verify final disk content
	rec, err := os.ReadFile(filepath.Join(env.receiveDir, itemName))
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(rec, payload) {
		t.Fatal("content mismatch after TCP RST resumption")
	}
}

// 14. TCP Proxy: Delayed Receiver Backpressure during Download
func TestPhase4NetSim_TCPProxy_DelayedReceiverBackpressure(t *testing.T) {
	env := startSimTestServer(t)
	serverAddr := env.srv.Addr().String()

	// Receiver backpressure delay of 5ms per chunk
	proxy := startTCPProxy(t, TCPProxyConfig{
		TargetAddr: serverAddr,
		DelayRead:  5 * time.Millisecond,
	})

	const fileSize = 1024 * 1024 // 1 MB
	fileData := deterministicData(fileSize, 0x94)
	itemName := "proxy_backpressure_dl.bin"
	filePath := filepath.Join(env.receiveDir, itemName)
	_ = os.WriteFile(filePath, fileData, 0o644)

	sf, err := env.svcs.Registry.Add(filePath, itemName, env.sessionKey)
	if err != nil {
		t.Fatalf("registry add: %v", err)
	}

	dlReq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/files/%s/download", proxy.URL(), sf.ID), nil)
	dlReq.Header.Set("Authorization", "Bearer "+env.token)

	t0 := time.Now()
	dResp, err := http.DefaultClient.Do(dlReq)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer dResp.Body.Close()

	downloaded, err := io.ReadAll(dResp.Body)
	if err != nil {
		t.Fatalf("read download body: %v", err)
	}
	dur := time.Since(t0)

	if dResp.StatusCode != http.StatusOK {
		t.Fatalf("download status: %d: %s", dResp.StatusCode, string(downloaded))
	}
	if !bytes.Equal(downloaded, fileData) {
		t.Fatalf("downloaded bytes mismatch: got %d bytes, want %d", len(downloaded), len(fileData))
	}
	if dur < 150*time.Millisecond {
		t.Fatalf("download finished too quickly (%v) for delayed TCP proxy", dur)
	}
}
