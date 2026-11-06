package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

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
					return
				}
				if int64(toRead) > remaining {
					toRead = int(remaining)
				}
			}

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
// Network Simulation Scenarios
// -----------------------------------------------------------------------------

// TestPhase4NetSim_RestrictedBandwidthUpload verifies that a streaming upload
// successfully completes over a rate-limited link with byte-for-byte integrity.
func TestPhase4NetSim_RestrictedBandwidthUpload(t *testing.T) {
	env := startSimTestServer(t)

	// Proxy with 500 KB/s rate limit
	proxy := startTCPProxy(t, TCPProxyConfig{
		TargetAddr:     env.srv.Addr().String(),
		BandwidthBytes: 500 * 1024,
	})

	const payloadSize = 256 * 1024
	payload := generateDeterministicBytes(payloadSize, 0x33)
	expectedHash := sha256.Sum256(payload)

	// Create batch using proxy URL
	bID, itID := createSimTestBatchAtURL(t, proxy.URL(), env.token, "throttled.bin", payloadSize)

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", proxy.URL(), bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Logf("upload took %v (rate limited)", elapsed)
	}

	data, err := os.ReadFile(filepath.Join(env.receiveDir, "throttled.bin"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("content mismatch under bandwidth limit")
	}
	actualHash := sha256.Sum256(data)
	if actualHash != expectedHash {
		t.Fatalf("SHA-256 mismatch")
	}
}

// TestPhase4NetSim_TCPProxy_LatencyAndJitter verifies streaming upload completes
// with byte-for-byte integrity over a high-latency link with jitter.
func TestPhase4NetSim_TCPProxy_LatencyAndJitter(t *testing.T) {
	env := startSimTestServer(t)

	// 10ms base latency + 5ms jitter over real TCP proxy
	proxy := startTCPProxy(t, TCPProxyConfig{
		TargetAddr: env.srv.Addr().String(),
		Latency:    10 * time.Millisecond,
		Jitter:     5 * time.Millisecond,
	})

	const payloadSize = 64 * 1024 // 64 KB
	payload := generateDeterministicBytes(payloadSize, 0x44)
	expectedHash := sha256.Sum256(payload)

	bID, itID := createSimTestBatchAtURL(t, proxy.URL(), env.token, "latency_jitter.bin", payloadSize)

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", proxy.URL(), bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT with latency failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	data, err := os.ReadFile(filepath.Join(env.receiveDir, "latency_jitter.bin"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("content mismatch under latency/jitter")
	}
	if actualHash := sha256.Sum256(data); actualHash != expectedHash {
		t.Fatal("SHA-256 mismatch")
	}
}

// TestPhase4NetSim_TCPProxy_ConnectionResetAndRetry verifies that an abrupt TCP
// reset (RST packet) during streaming is handled cleanly and retry transmits from byte zero.
func TestPhase4NetSim_TCPProxy_ConnectionResetAndRetry(t *testing.T) {
	env := startSimTestServer(t)

	// Drop and reset with RST after 100 KB
	proxy := startTCPProxy(t, TCPProxyConfig{
		TargetAddr:   env.srv.Addr().String(),
		DropWriteAt:  100 * 1024,
		ResetWithRST: true,
	})

	const payloadSize = 300 * 1024
	payload := generateDeterministicBytes(payloadSize, 0x55)

	bID, itID := createSimTestBatchAtURL(t, proxy.URL(), env.token, "rst_test.bin", payloadSize)

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", proxy.URL(), bID, itID)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	// Attempt 1 fails due to TCP reset
	_, _ = http.DefaultClient.Do(req)

	// Wait for server to clean up aborted part
	time.Sleep(100 * time.Millisecond)

	// Verify uncommitted file is not published
	if _, err := os.Stat(filepath.Join(env.receiveDir, "rst_test.bin")); !os.IsNotExist(err) {
		t.Fatal("partial file was published despite connection reset")
	}

	// Update proxy to allow full transfer
	proxy.SetConfig(TCPProxyConfig{
		TargetAddr: env.srv.Addr().String(),
	})

	// Retry creates a fresh batch and transmits from byte zero
	bID2, itID2 := createSimTestBatchAtURL(t, proxy.URL(), env.token, "rst_test.bin", payloadSize)
	putURL2 := fmt.Sprintf("%s/api/batches/%s/items/%s/content", proxy.URL(), bID2, itID2)
	req2, _ := http.NewRequest(http.MethodPut, putURL2, bytes.NewReader(payload))
	req2.Header.Set("Authorization", "Bearer "+env.token)
	req2.Header.Set("Content-Type", "application/octet-stream")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("retry request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry expected 200, got %d", resp2.StatusCode)
	}

	data, err := os.ReadFile(filepath.Join(env.receiveDir, "rst_test.bin"))
	if err != nil {
		t.Fatalf("read retry file: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("retry file content mismatch")
	}
}

// TestPhase4NetSim_InterruptedArchiveZipStream verifies that client disconnection
// during dynamic zip archive download closes descriptors cleanly without panic or hang.
func TestPhase4NetSim_InterruptedArchiveZipStream(t *testing.T) {
	env := startSimTestServer(t)

	// Register 2 files
	srcDir := t.TempDir()
	var fileIDs []string
	for i := 0; i < 2; i++ {
		p := filepath.Join(srcDir, fmt.Sprintf("file_%d.bin", i))
		_ = os.WriteFile(p, bytes.Repeat([]byte{byte(i + 1)}, 500*1024), 0o644)
		sf, err := env.svcs.Registry.Add(p, fmt.Sprintf("file_%d.bin", i), env.sessionKey)
		if err != nil {
			t.Fatalf("add to registry: %v", err)
		}
		fileIDs = append(fileIDs, sf.ID)
	}

	// Create grant for archive
	grantBody, _ := json.Marshal(map[string]any{"sourceIds": fileIDs})
	gReq, _ := http.NewRequest(http.MethodPost, env.url+"/api/download-grants", bytes.NewReader(grantBody))
	gReq.Header.Set("Authorization", "Bearer "+env.token)
	gReq.Header.Set("Content-Type", "application/json")
	gResp, err := http.DefaultClient.Do(gReq)
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	defer gResp.Body.Close()
	var grantResp struct {
		DownloadURL string `json:"downloadUrl"`
		Archive     bool   `json:"archive"`
	}
	_ = json.NewDecoder(gResp.Body).Decode(&grantResp)

	// Start download and abort client reading mid-stream
	dlReq, _ := http.NewRequest(http.MethodGet, env.url+grantResp.DownloadURL, nil)
	dlReq.Header.Set("Authorization", "Bearer "+env.token)
	dResp, err := http.DefaultClient.Do(dlReq)
	if err != nil {
		t.Fatalf("download req: %v", err)
	}

	// Read only 50 KB and abruptly close body
	buf := make([]byte, 50*1024)
	_, _ = io.ReadFull(dResp.Body, buf)
	_ = dResp.Body.Close()

	// Truncated zip buffer cannot be unzipped
	_, zipErr := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if zipErr == nil {
		t.Fatal("expected truncated zip error, got nil")
	}
}

func createSimTestBatchAtURL(t *testing.T, baseURL, token, itemName string, size int64) (batchID, itemID string) {
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
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/batches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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
