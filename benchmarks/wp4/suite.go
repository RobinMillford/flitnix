package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

type BenchmarkRunResult struct {
	RunID         string  `json:"run_id"`
	ConfigID      string  `json:"config_id"`
	ConfigName    string  `json:"config_name"`
	PayloadSize   int64   `json:"payload_size"`
	PayloadName   string  `json:"payload_name"`
	Repetition    int     `json:"repetition"`
	DurationMs    int64   `json:"duration_ms"`
	ThroughputMBs float64 `json:"throughput_mbs"`
	TTFBMs        float64 `json:"ttfb_ms"`
	NetworkTimeMs float64 `json:"network_time_ms"`
	TotalTimeMs   float64 `json:"total_time_ms"`
	CPUTimeMs     float64 `json:"cpu_time_ms"`
	HeapAllocMB   float64 `json:"heap_alloc_mb"`
	PeakHeapMB    float64 `json:"peak_heap_mb"`
	RSSMB         float64 `json:"rss_mb"`
	RequestCount  int     `json:"request_count"`
	IntegrityPass bool    `json:"integrity_pass"`
	Valid         bool    `json:"valid"`
	Notes         string  `json:"notes"`
}

type RecoveryRunResult struct {
	RunID              string `json:"run_id"`
	PayloadSize        int64  `json:"payload_size"`
	PayloadName        string `json:"payload_name"`
	InterruptPercent   int    `json:"interrupt_percent"`
	InterruptOffset    int64  `json:"interrupt_offset"`
	InterruptClientErr string `json:"interrupt_client_error"`
	// Genuine-interruption evidence. The interrupted request declares the FULL
	// upload length while its body supplies only InterruptOffset bytes, so the
	// server must observe a truncated HTTP request, not a complete partial tus
	// PATCH. These fields record what the server actually saw afterwards.
	InterruptDeclaredLength int64   `json:"interrupt_declared_length"`
	InterruptBytesSent      int64   `json:"interrupt_bytes_sent"`
	InterruptServerStatus   int     `json:"interrupt_server_status"`
	InterruptServerOffset   int64   `json:"interrupt_server_offset_header"`
	InterruptedState        string  `json:"interrupted_state"`
	ServerSawTruncation     bool    `json:"server_saw_truncation"`
	SettleObservations      int     `json:"settle_observations"`
	PrefixDigestServer      string  `json:"prefix_digest_server"`
	PrefixDigestLocal       string  `json:"prefix_digest_local"`
	SettleWaitMs            float64 `json:"settle_wait_ms"`
	ServerCommitted         int64   `json:"server_committed"`
	DetectTimeMs            float64 `json:"detect_time_ms"`
	ReconnectTimeMs         float64 `json:"reconnect_time_ms"`
	HeadTimeMs              float64 `json:"head_time_ms"`
	PrefixRebuildMs         float64 `json:"prefix_rebuild_ms"`
	PrefixVerifyMs          float64 `json:"prefix_verify_ms"`
	PrefixVerified          bool    `json:"prefix_verified"`
	ResumeStartOffset       int64   `json:"resume_start_offset"`
	ResumeAttempts          int     `json:"resume_attempts"`
	FirstResumedByteMs      float64 `json:"first_resumed_byte_ms"`
	ResumeStreamTimeMs      float64 `json:"resume_stream_time_ms"`
	FinalOffset             int64   `json:"final_offset"`
	UploadState             string  `json:"upload_state"`
	TotalCompletionMs       float64 `json:"total_completion_ms"`
	RetransmittedBytes      int64   `json:"retransmitted_bytes"`
	AuthoritativeStatus     string  `json:"authoritative_status"`
	IsolatedSession         bool    `json:"isolated_session"`
	IntegrityPass           bool    `json:"integrity_pass"`
	Valid                   bool    `json:"valid"`
	Notes                   string  `json:"notes"`
}

type BenchHarness struct {
	serverURL       string
	token           string
	services        *server.Services
	receiveDir      string
	client          *http.Client
	checkpointBytes int64
	// runID labels every row with the run that produced it so historical
	// partial or invalid datasets are never silently mixed.
	runID string
	// isolated marks a harness started only for recovery runs. Its server,
	// session, coordinator queue and staging directory are discarded after
	// the run, so an expected interruption can never poison a throughput
	// queue.
	isolated bool
}

// SetCheckpointBytes configures custom checkpoint interval bytes for Config-4.
func (h *BenchHarness) SetCheckpointBytes(b int64) {
	h.checkpointBytes = b
}

// getCPUTime and getRSSMB are platform-specific; see proc_unix.go and
// proc_other.go. They live behind build tags so `GOOS=windows go build ./...`
// does not break on Unix-only syscall.Rusage fields.

// sampleVmRSSMB reads the process's current resident set from /proc/self/status.
// Unlike getRSSMB (kernel lifetime peak via rusage Maxrss, which is constant
// across all runs of a single-process harness), this gives a per-run sample.
func sampleVmRSSMB() float64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseFloat(fields[1], 64)
				if err == nil {
					return kb / 1024.0
				}
			}
		}
	}
	return 0
}

// expectStatus validates an HTTP status against the accepted set.
func expectStatus(where string, got int, want ...int) error {
	for _, w := range want {
		if got == w {
			return nil
		}
	}
	return fmt.Errorf("%s: unexpected HTTP %d (wanted %v)", where, got, want)
}

func StartBenchServer(receiveDir string) (*BenchHarness, func(), error) {
	_ = os.MkdirAll(receiveDir, 0o755)
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: receiveDir})
	if err != nil {
		return nil, nil, err
	}

	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()

	svcs := &server.Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
	}
	// Bench-only auto-approve: mirror the trusted-device auto-approve path
	// (routes.go Gate 3: DecideBatchItems + ApproveBatch) so declared batches
	// do not sit pending-approval and every subsequent content request is
	// accepted. BOTH approval surfaces must be satisfied: the coordinator
	// batch (moves items Pending→Queued so ClaimItem works) and the
	// batch-linked upload records (SubmitContentWithPublish refuses content
	// with ErrNotApproved → HTTP 409 otherwise). Approval is consent gating,
	// not an integrity mechanism: hashing, checkpointing, and digest
	// verification are unchanged. The callback is invoked by
	// handleBatchCreate without holding the coordinator mutex (the handler
	// itself calls ApproveBatch in the same position), so a synchronous call
	// here cannot deadlock.
	svcs.OnBatchPending = func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
		for _, item := range batch.Items {
			contentID, err := coord.ItemContentID(sessionKey, batch.ID, item.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "bench auto-approve: content id for item %s: %v\n", item.ID, err)
				continue
			}
			if err := transfers.DecideBatchItems([]string{contentID}, true); err != nil {
				fmt.Fprintf(os.Stderr, "bench auto-approve: decide record %s: %v\n", contentID, err)
			}
		}
		if _, err := coord.ApproveBatch(sessionKey, batch.ID); err != nil {
			fmt.Fprintf(os.Stderr, "bench auto-approve failed for batch %s: %v\n", batch.ID, err)
		}
	}
	rm := svcs.EnsureResumable()

	srv := server.New(server.Config{
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
		return nil, nil, err
	}

	client := &http.Client{
		Timeout: 30 * time.Minute, // Allow large transfers
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}

	harness := &BenchHarness{
		serverURL:  srv.URL(),
		token:      sess.Token(),
		services:   svcs,
		receiveDir: receiveDir,
		client:     client,
	}

	cleanup := func() {
		if rm != nil {
			rm.Close()
		}
		cancel()
	}

	return harness, cleanup, nil
}

func (h *BenchHarness) createBatch(itemName string, size int64) (string, string, error) {
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
	req, err := http.NewRequest(http.MethodPost, h.serverURL+"/api/batches", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("createBatch failed: HTTP %d", resp.StatusCode)
	}

	var parsed struct {
		ID    string `json:"id"`
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", "", err
	}
	if len(parsed.Items) == 0 {
		return "", "", fmt.Errorf("no items returned in batch")
	}
	return parsed.ID, parsed.Items[0].ID, nil
}

func verifyReceivedFile(filePath, expectedSHA256 string, expectedSize int64) (bool, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return false, fmt.Errorf("stat failed: %w", err)
	}
	if fi.Size() != expectedSize {
		return false, fmt.Errorf("size mismatch: got %d, expected %d", fi.Size(), expectedSize)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return false, fmt.Errorf("open failed: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("hash failed: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expectedSHA256 {
		return false, fmt.Errorf("checksum mismatch: got %s, expected %s", actual, expectedSHA256)
	}
	return true, nil
}

// -----------------------------------------------------------------------------
// Configuration 1: Existing Legacy PUT Baseline
// -----------------------------------------------------------------------------
func (h *BenchHarness) RunConfig1_LegacyPUT(fixture FixtureMetadata, rep int) (BenchmarkRunResult, error) {
	configID := "Config-1"
	configName := "Legacy PUT Baseline"
	itemName := fmt.Sprintf("legacy_%d_%s", rep, fixture.Name)

	bID, itID, err := h.createBatch(itemName, fixture.SizeBytes)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: err.Error()}, err
	}

	file, err := os.Open(fixture.Path)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: err.Error()}, err
	}
	defer file.Close()

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", h.serverURL, bID, itID)
	req, err := http.NewRequest(http.MethodPut, putURL, file)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = fixture.SizeBytes

	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	rssStart := sampleVmRSSMB()
	cpuBefore := getCPUTime()
	t0 := time.Now()

	resp, err := h.client.Do(req)
	dur := time.Since(t0)
	cpuDur := getCPUTime() - cpuBefore
	runtime.ReadMemStats(&mAfter)

	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: err.Error()}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))}, fmt.Errorf("legacy PUT returned HTTP %d", resp.StatusCode)
	}

	receivedPath := filepath.Join(h.receiveDir, itemName)
	pass, verr := verifyReceivedFile(receivedPath, fixture.SHA256, fixture.SizeBytes)
	_ = os.Remove(receivedPath) // Clean up after verification to free disk

	throughput := float64(fixture.SizeBytes) / dur.Seconds() / 1e6
	heapAllocMB := float64(mAfter.HeapAlloc) / 1024 / 1024
	peakHeapMB := float64(mAfter.TotalAlloc-mBefore.TotalAlloc) / 1024 / 1024
	rssMB := sampleVmRSSMB()
	if rssStart > rssMB {
		rssMB = rssStart
	}

	notes := "Direct legacy PUT stream"
	if verr != nil {
		notes = "INTEGRITY FAILURE: " + verr.Error()
	}

	return BenchmarkRunResult{
		RunID:         h.runID,
		ConfigID:      configID,
		ConfigName:    configName,
		PayloadSize:   fixture.SizeBytes,
		PayloadName:   fixture.Name,
		Repetition:    rep,
		DurationMs:    dur.Milliseconds(),
		ThroughputMBs: throughput,
		TotalTimeMs:   dur.Seconds() * 1000,
		CPUTimeMs:     cpuDur.Seconds() * 1000,
		HeapAllocMB:   heapAllocMB,
		PeakHeapMB:    peakHeapMB,
		RSSMB:         rssMB,
		RequestCount:  1,
		IntegrityPass: pass,
		Valid:         pass && verr == nil,
		Notes:         notes,
	}, verr
}

// -----------------------------------------------------------------------------
// Configuration 2: Continuous Streaming with Server Hashing Only (Zero Checkpoints)
// -----------------------------------------------------------------------------
func (h *BenchHarness) RunConfig2_ServerHashingOnly(fixture FixtureMetadata, rep int) (BenchmarkRunResult, error) {
	configID := "Config-2"
	configName := "Continuous Streaming (Server Hash Only, No Checkpoints)"
	itemName := fmt.Sprintf("stream_srvhash_%d_%s", rep, fixture.Name)

	rm := h.services.EnsureResumable()
	// Disable intermediate checkpoints in this measurement configuration
	rm.SetCheckpointParams(1<<60, 24*time.Hour)

	bID, itID, err := h.createBatch(itemName, fixture.SizeBytes)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", h.serverURL, bID, itID)
	cReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+h.token)
	cReq.Header.Set("Tus-Resumable", "1.0.0")
	cReq.Header.Set("Upload-Length", strconv.FormatInt(fixture.SizeBytes, 10))
	cResp, err := h.client.Do(cReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	if stErr := expectStatus("tus create", cResp.StatusCode, http.StatusCreated); stErr != nil {
		cResp.Body.Close()
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}
	cResp.Body.Close()

	file, err := os.Open(fixture.Path)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	defer file.Close()

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, file)
	patchReq.Header.Set("Authorization", "Bearer "+h.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = fixture.SizeBytes

	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	cpuBefore := getCPUTime()
	t0 := time.Now()

	pResp, err := h.client.Do(patchReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	pResp.Body.Close()
	netDur := time.Since(t0)

	// Submit completion
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", h.serverURL, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": fixture.SHA256})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+h.token)
	compReq.Header.Set("Content-Type", "application/json")
	compResp, err := h.client.Do(compReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	compCode := compResp.StatusCode
	compResp.Body.Close()
	if stErr := expectStatus("complete", compCode, http.StatusOK); stErr != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}

	totalDur := time.Since(t0)
	cpuDur := getCPUTime() - cpuBefore
	runtime.ReadMemStats(&mAfter)

	receivedPath := filepath.Join(h.receiveDir, itemName)
	pass, verr := verifyReceivedFile(receivedPath, fixture.SHA256, fixture.SizeBytes)
	_ = os.Remove(receivedPath)

	throughput := float64(fixture.SizeBytes) / totalDur.Seconds() / 1e6
	heapAllocMB := float64(mAfter.HeapAlloc) / 1024 / 1024
	peakHeapMB := float64(mAfter.TotalAlloc-mBefore.TotalAlloc) / 1024 / 1024
	rssMB := sampleVmRSSMB()

	notes := "tus streaming, server hash only, zero intermediate checkpoints"
	if verr != nil {
		notes = "INTEGRITY FAILURE: " + verr.Error()
	}

	return BenchmarkRunResult{
		RunID:         h.runID,
		ConfigID:      configID,
		ConfigName:    configName,
		PayloadSize:   fixture.SizeBytes,
		PayloadName:   fixture.Name,
		Repetition:    rep,
		DurationMs:    totalDur.Milliseconds(),
		ThroughputMBs: throughput,
		NetworkTimeMs: netDur.Seconds() * 1000,
		TotalTimeMs:   totalDur.Seconds() * 1000,
		CPUTimeMs:     cpuDur.Seconds() * 1000,
		HeapAllocMB:   heapAllocMB,
		PeakHeapMB:    peakHeapMB,
		RSSMB:         rssMB,
		RequestCount:  3, // POST tus + PATCH + POST complete
		IntegrityPass: pass,
		Valid:         pass && verr == nil,
		Notes:         notes,
	}, verr
}

// -----------------------------------------------------------------------------
// Configuration 3: Continuous Streaming with Dual (Client + Server) Hashing
// -----------------------------------------------------------------------------
type hashingReader struct {
	r      io.Reader
	hasher io.Writer
}

func (hr *hashingReader) Read(p []byte) (int, error) {
	n, err := hr.r.Read(p)
	if n > 0 {
		hr.hasher.Write(p[:n])
	}
	return n, err
}

func (h *BenchHarness) RunConfig3_DualHashing(fixture FixtureMetadata, rep int) (BenchmarkRunResult, error) {
	configID := "Config-3"
	configName := "Continuous Streaming (Client + Server Dual Hashing, No Checkpoints)"
	itemName := fmt.Sprintf("dualhash_%d_%s", rep, fixture.Name)

	rm := h.services.EnsureResumable()
	rm.SetCheckpointParams(1<<60, 24*time.Hour)

	bID, itID, err := h.createBatch(itemName, fixture.SizeBytes)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", h.serverURL, bID, itID)
	cReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+h.token)
	cReq.Header.Set("Tus-Resumable", "1.0.0")
	cReq.Header.Set("Upload-Length", strconv.FormatInt(fixture.SizeBytes, 10))
	cResp, err := h.client.Do(cReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	if stErr := expectStatus("tus create", cResp.StatusCode, http.StatusCreated); stErr != nil {
		cResp.Body.Close()
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}
	cResp.Body.Close()

	file, err := os.Open(fixture.Path)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	defer file.Close()

	// Client-side incremental hasher
	clientHasher := sha256.New()
	hr := &hashingReader{r: file, hasher: clientHasher}

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, hr)
	patchReq.Header.Set("Authorization", "Bearer "+h.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = fixture.SizeBytes

	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	rssStart := sampleVmRSSMB()
	cpuBefore := getCPUTime()
	t0 := time.Now()

	pResp, err := h.client.Do(patchReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	pResp.Body.Close()
	netDur := time.Since(t0)

	calculatedClientDigest := hex.EncodeToString(clientHasher.Sum(nil))

	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", h.serverURL, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": calculatedClientDigest})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+h.token)
	compReq.Header.Set("Content-Type", "application/json")
	compResp, err := h.client.Do(compReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	compCode := compResp.StatusCode
	compResp.Body.Close()
	if stErr := expectStatus("complete", compCode, http.StatusOK); stErr != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}

	totalDur := time.Since(t0)
	cpuDur := getCPUTime() - cpuBefore
	runtime.ReadMemStats(&mAfter)

	receivedPath := filepath.Join(h.receiveDir, itemName)
	pass, verr := verifyReceivedFile(receivedPath, fixture.SHA256, fixture.SizeBytes)
	_ = os.Remove(receivedPath)

	throughput := float64(fixture.SizeBytes) / totalDur.Seconds() / 1e6
	heapAllocMB := float64(mAfter.HeapAlloc) / 1024 / 1024
	peakHeapMB := float64(mAfter.TotalAlloc-mBefore.TotalAlloc) / 1024 / 1024
	rssMB := sampleVmRSSMB()
	if rssStart > rssMB {
		rssMB = rssStart
	}

	notes := "Client on-the-fly hashing + server hashing, zero intermediate checkpoints"
	if verr != nil {
		notes = "INTEGRITY FAILURE: " + verr.Error()
	}

	return BenchmarkRunResult{
		RunID:         h.runID,
		ConfigID:      configID,
		ConfigName:    configName,
		PayloadSize:   fixture.SizeBytes,
		PayloadName:   fixture.Name,
		Repetition:    rep,
		DurationMs:    totalDur.Milliseconds(),
		ThroughputMBs: throughput,
		NetworkTimeMs: netDur.Seconds() * 1000,
		TotalTimeMs:   totalDur.Seconds() * 1000,
		CPUTimeMs:     cpuDur.Seconds() * 1000,
		HeapAllocMB:   heapAllocMB,
		PeakHeapMB:    peakHeapMB,
		RSSMB:         rssMB,
		RequestCount:  3,
		IntegrityPass: pass && (calculatedClientDigest == fixture.SHA256),
		Valid:         pass && verr == nil,
		Notes:         notes,
	}, verr
}

// -----------------------------------------------------------------------------
// Configuration 4: Primary Production Architecture (Continuous Streaming + Durable Checkpoints)
// -----------------------------------------------------------------------------
func (h *BenchHarness) RunConfig4_PrimaryProduction(fixture FixtureMetadata, rep int) (BenchmarkRunResult, error) {
	cpBytes := h.checkpointBytes
	if cpBytes <= 0 {
		cpBytes = transfer.DefaultCheckpointBytes
	}
	configID := "Config-4"
	configName := "Primary Production Architecture (Checkpoints 32MB/500ms + Dual Hash)"
	if cpBytes != transfer.DefaultCheckpointBytes {
		configID = fmt.Sprintf("Config-4-%dMB", cpBytes/(1024*1024))
		configName = fmt.Sprintf("Primary Production Architecture (Checkpoints %dMB/500ms + Dual Hash)", cpBytes/(1024*1024))
	}
	itemName := fmt.Sprintf("prod_primary_%d_%s", rep, fixture.Name)

	rm := h.services.EnsureResumable()
	rm.SetCheckpointParams(cpBytes, transfer.DefaultCheckpointDuration)

	bID, itID, err := h.createBatch(itemName, fixture.SizeBytes)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", h.serverURL, bID, itID)
	cReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+h.token)
	cReq.Header.Set("Tus-Resumable", "1.0.0")
	cReq.Header.Set("Upload-Length", strconv.FormatInt(fixture.SizeBytes, 10))
	cResp, err := h.client.Do(cReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	if stErr := expectStatus("tus create", cResp.StatusCode, http.StatusCreated); stErr != nil {
		cResp.Body.Close()
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}
	cResp.Body.Close()

	file, err := os.Open(fixture.Path)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	defer file.Close()

	clientHasher := sha256.New()
	hr := &hashingReader{r: file, hasher: clientHasher}

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, hr)
	patchReq.Header.Set("Authorization", "Bearer "+h.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = fixture.SizeBytes

	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	rssStart := sampleVmRSSMB()
	cpuBefore := getCPUTime()
	t0 := time.Now()

	pResp, err := h.client.Do(patchReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	patchCode := pResp.StatusCode
	pResp.Body.Close()
	netDur := time.Since(t0)
	if stErr := expectStatus("tus PATCH", patchCode, http.StatusNoContent); stErr != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}

	calculatedClientDigest := hex.EncodeToString(clientHasher.Sum(nil))

	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", h.serverURL, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": calculatedClientDigest})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+h.token)
	compReq.Header.Set("Content-Type", "application/json")
	compResp, err := h.client.Do(compReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	compCode := compResp.StatusCode
	compResp.Body.Close()
	if stErr := expectStatus("complete", compCode, http.StatusOK); stErr != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}

	// Query authoritative status
	statusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/status", h.serverURL, bID, itID)
	stReq, _ := http.NewRequest(http.MethodGet, statusURL, nil)
	stReq.Header.Set("Authorization", "Bearer "+h.token)
	stResp, err := h.client.Do(stReq)
	if err == nil {
		if stErr := expectStatus("item status", stResp.StatusCode, http.StatusOK); stErr != nil {
			stResp.Body.Close()
			return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
		}
		stResp.Body.Close()
	}

	totalDur := time.Since(t0)
	cpuDur := getCPUTime() - cpuBefore
	runtime.ReadMemStats(&mAfter)

	receivedPath := filepath.Join(h.receiveDir, itemName)
	pass, verr := verifyReceivedFile(receivedPath, fixture.SHA256, fixture.SizeBytes)
	_ = os.Remove(receivedPath)

	throughput := float64(fixture.SizeBytes) / totalDur.Seconds() / 1e6
	heapAllocMB := float64(mAfter.HeapAlloc) / 1024 / 1024
	peakHeapMB := float64(mAfter.TotalAlloc-mBefore.TotalAlloc) / 1024 / 1024
	rssMB := sampleVmRSSMB()
	if rssStart > rssMB {
		rssMB = rssStart
	}

	notes := "Full production path: 32MB/500ms checkpoints + client hash + complete digest"
	if rm.PipelineCheckpoints() {
		notes += " (pipelined metadata persistence)"
	} else {
		notes += " (synchronous metadata persistence)"
	}
	if verr != nil {
		notes = "INTEGRITY FAILURE: " + verr.Error()
	}

	return BenchmarkRunResult{
		RunID:         h.runID,
		ConfigID:      configID,
		ConfigName:    configName,
		PayloadSize:   fixture.SizeBytes,
		PayloadName:   fixture.Name,
		Repetition:    rep,
		DurationMs:    totalDur.Milliseconds(),
		ThroughputMBs: throughput,
		NetworkTimeMs: netDur.Seconds() * 1000,
		TotalTimeMs:   totalDur.Seconds() * 1000,
		CPUTimeMs:     cpuDur.Seconds() * 1000,
		HeapAllocMB:   heapAllocMB,
		PeakHeapMB:    peakHeapMB,
		RSSMB:         rssMB,
		RequestCount:  4, // POST tus + PATCH + POST complete + GET status
		IntegrityPass: pass && (calculatedClientDigest == fixture.SHA256),
		Valid:         pass && verr == nil,
		Notes:         notes,
	}, verr
}

// -----------------------------------------------------------------------------
// Configuration 5: Direct Blob XHR Fallback Evaluation (8, 16, 32, 64 MB slices)
// -----------------------------------------------------------------------------
func (h *BenchHarness) RunConfig5_FallbackSlice(fixture FixtureMetadata, sliceSize int64, rep int) (BenchmarkRunResult, error) {
	configID := fmt.Sprintf("Config-5-%dMB", sliceSize/(1024*1024))
	configName := fmt.Sprintf("Blob XHR Fallback (%d MB slices)", sliceSize/(1024*1024))
	itemName := fmt.Sprintf("fallback_%dmb_%d_%s", sliceSize/(1024*1024), rep, fixture.Name)

	rm := h.services.EnsureResumable()
	rm.SetCheckpointParams(transfer.DefaultCheckpointBytes, transfer.DefaultCheckpointDuration)

	bID, itID, err := h.createBatch(itemName, fixture.SizeBytes)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", h.serverURL, bID, itID)
	cReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+h.token)
	cReq.Header.Set("Tus-Resumable", "1.0.0")
	cReq.Header.Set("Upload-Length", strconv.FormatInt(fixture.SizeBytes, 10))
	cResp, err := h.client.Do(cReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	if stErr := expectStatus("tus create", cResp.StatusCode, http.StatusCreated); stErr != nil {
		cResp.Body.Close()
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}
	cResp.Body.Close()

	file, err := os.Open(fixture.Path)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	defer file.Close()

	clientHasher := sha256.New()
	totalBytes := fixture.SizeBytes
	var offset int64 = 0
	requestCount := 1 // POST create

	runtime.GC()
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	rssStart := sampleVmRSSMB()
	cpuBefore := getCPUTime()
	t0 := time.Now()

	buf := make([]byte, sliceSize)
	for offset < totalBytes {
		toRead := sliceSize
		if totalBytes-offset < toRead {
			toRead = totalBytes - offset
		}

		n, rErr := io.ReadFull(file, buf[:toRead])
		if rErr != nil && rErr != io.ErrUnexpectedEOF && rErr != io.EOF {
			return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, rErr
		}
		sliceData := buf[:n]
		clientHasher.Write(sliceData)

		patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(sliceData))
		patchReq.Header.Set("Authorization", "Bearer "+h.token)
		patchReq.Header.Set("Tus-Resumable", "1.0.0")
		patchReq.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
		patchReq.ContentLength = int64(n)

		pResp, pErr := h.client.Do(patchReq)
		if pErr != nil {
			return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, pErr
		}
		pResp.Body.Close()
		requestCount++

		if pResp.StatusCode != http.StatusNoContent {
			return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, fmt.Errorf("slice PATCH returned HTTP %d", pResp.StatusCode)
		}
		offset += int64(n)
	}

	calculatedClientDigest := hex.EncodeToString(clientHasher.Sum(nil))

	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", h.serverURL, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": calculatedClientDigest})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+h.token)
	compReq.Header.Set("Content-Type", "application/json")
	compResp, err := h.client.Do(compReq)
	if err != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false}, err
	}
	compCode := compResp.StatusCode
	compResp.Body.Close()
	requestCount++
	if stErr := expectStatus("complete", compCode, http.StatusOK); stErr != nil {
		return BenchmarkRunResult{ConfigID: configID, ConfigName: configName, Valid: false, Notes: stErr.Error()}, stErr
	}

	totalDur := time.Since(t0)
	cpuDur := getCPUTime() - cpuBefore
	runtime.ReadMemStats(&mAfter)

	receivedPath := filepath.Join(h.receiveDir, itemName)
	pass, verr := verifyReceivedFile(receivedPath, fixture.SHA256, fixture.SizeBytes)
	_ = os.Remove(receivedPath)

	throughput := float64(fixture.SizeBytes) / totalDur.Seconds() / 1e6
	heapAllocMB := float64(mAfter.HeapAlloc) / 1024 / 1024
	peakHeapMB := float64(mAfter.TotalAlloc-mBefore.TotalAlloc) / 1024 / 1024
	rssMB := sampleVmRSSMB()
	if rssStart > rssMB {
		rssMB = rssStart
	}

	notes := fmt.Sprintf("Direct Blob XHR fallback with %d MB requests", sliceSize/(1024*1024))
	if verr != nil {
		notes = "INTEGRITY FAILURE: " + verr.Error()
	}

	return BenchmarkRunResult{
		RunID:         h.runID,
		ConfigID:      configID,
		ConfigName:    configName,
		PayloadSize:   fixture.SizeBytes,
		PayloadName:   fixture.Name,
		Repetition:    rep,
		DurationMs:    totalDur.Milliseconds(),
		ThroughputMBs: throughput,
		TotalTimeMs:   totalDur.Seconds() * 1000,
		CPUTimeMs:     cpuDur.Seconds() * 1000,
		HeapAllocMB:   heapAllocMB,
		PeakHeapMB:    peakHeapMB,
		RSSMB:         rssMB,
		RequestCount:  requestCount,
		IntegrityPass: pass && (calculatedClientDigest == fixture.SHA256),
		Valid:         pass && verr == nil,
		Notes:         notes,
	}, verr
}

// -----------------------------------------------------------------------------
// Configuration 6: Recovery Performance After Interruption (10%, 50%, 99%)
// -----------------------------------------------------------------------------

// interruptReader models a client whose connection dies mid-body: it yields at
// most `remaining` bytes and then fails every further Read with err.
//
// It is only meaningful when the request declares a Content-Length LARGER than
// `remaining`: the transport then aborts the connection mid-body instead of
// terminating the request cleanly, and the server observes a truncated request
// (io.ErrUnexpectedEOF) rather than a complete partial tus PATCH. Declaring
// Content-Length == remaining — the previous defect — produced a well-formed
// request whose body happened to be short, which proved nothing about
// interruption recovery.
type interruptReader struct {
	r         io.Reader
	remaining int64
	err       error
	// sent counts bytes actually produced, so the harness can prove how much
	// of the declared request body reached the transport.
	sent int64
}

func (ir *interruptReader) Read(p []byte) (int, error) {
	if ir.remaining <= 0 {
		return 0, ir.err
	}
	if int64(len(p)) > ir.remaining {
		p = p[:ir.remaining]
	}
	n, err := ir.r.Read(p)
	ir.remaining -= int64(n)
	ir.sent += int64(n)
	if err != nil {
		return n, err
	}
	return n, nil
}

// hashPrefix returns a SHA-256 hasher primed with the first n bytes of path.
// Used to rebuild the client-side hasher over exactly the prefix the server
// reports as durably committed.
func hashPrefix(path string, n int64) (hash.Hash, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if n > 0 {
		if _, err := io.CopyN(h, f, n); err != nil {
			return nil, fmt.Errorf("hash prefix: %w", err)
		}
	}
	return h, nil
}

// waitForUploadSettled blocks until the server's resumable manager reports a
// stable committed offset for the item AND the upload's exclusive lifecycle
// lock is free. That lock is what an in-flight PATCH handler holds while it
// drains buffered body bytes and writes its final durable checkpoint; issuing
// HEAD or a resumed PATCH before it is released is what produced the 423 Locked
// (10%) and stale-offset 409 (50%/99%) responses in the previous run.
//
// This mirrors TestTusScenarios_1_2_3_InterruptionAndResume
// (internal/server/tus_upload_test.go), which polls CommittedOffset until it
// stops changing, with an additional lock check so the resumed PATCH cannot
// race the handler's final checkpoint.
func (h *BenchHarness) waitForUploadSettled(batchID, itemID string, timeout time.Duration) (int64, time.Duration, error) {
	rm := h.services.EnsureResumable()
	if rm == nil {
		return 0, 0, fmt.Errorf("resumable manager unavailable")
	}
	start := time.Now()
	var last int64 = -1
	stable := 0
	for {
		if up, ok := rm.GetUploadByItem(batchID, itemID); ok {
			off := up.Meta().CommittedOffset
			locked := !up.TryLock()
			if !locked {
				// Re-read while holding the lifecycle lock: a handler that has
				// released it cannot commit again without re-acquiring it.
				off = up.Meta().CommittedOffset
				up.Unlock()
			}
			if locked {
				stable = 0
			} else if off == last {
				stable++
			} else {
				last = off
				stable = 0
			}
			if !locked && stable >= 2 {
				return off, time.Since(start), nil
			}
		}
		if time.Since(start) >= timeout {
			if up, ok := rm.GetUploadByItem(batchID, itemID); ok {
				return up.Meta().CommittedOffset, time.Since(start),
					fmt.Errorf("upload %s/%s did not settle within %s (committed=%d)",
						batchID, itemID, timeout, up.Meta().CommittedOffset)
			}
			return 0, time.Since(start), fmt.Errorf("upload %s/%s not found within %s", batchID, itemID, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForInterruptedSettlement is the recovery-specific settlement gate. It
// returns only once the server has: (a) finished the interrupted PATCH handler,
// proven by the terminal `interrupted` state that IngestStream writes when the
// request body ends prematurely; (b) released the upload lifecycle lock; and
// (c) reported the same committed offset on two consecutive observations.
//
// Requiring the interrupted state — not merely "an upload exists and its lock is
// free" — prevents the harness from settling on a freshly created upload whose
// PATCH handler has not started yet.
func (h *BenchHarness) waitForInterruptedSettlement(batchID, itemID string, timeout time.Duration, maxCommitted int64) (int64, transfer.ResumableState, time.Duration, int, error) {
	rm := h.services.EnsureResumable()
	if rm == nil {
		return 0, "", 0, 0, fmt.Errorf("resumable manager unavailable")
	}
	start := time.Now()
	var last int64 = -1
	stable := 0
	observations := 0

	for {
		if up, ok := rm.GetUploadByItem(batchID, itemID); ok {
			if !up.TryLock() {
				// Handler in flight: its final checkpoint may still move the offset.
				stable = 0
				last = -1
			} else {
				m := up.Meta()
				off, state := m.CommittedOffset, m.State
				up.Unlock()
				observations++
				if state != transfer.ResumableInterrupted {
					// Not yet the terminal post-interruption state.
					stable = 0
					last = off
				} else if off == last {
					stable++
				} else {
					last = off
					stable = 0
				}
				if stable >= 2 {
					if off <= 0 {
						return off, state, time.Since(start), observations,
							fmt.Errorf("server committed 0 bytes: the interrupted request left no recoverable prefix")
					}
					if off > maxCommitted {
						return off, state, time.Since(start), observations,
							fmt.Errorf("server committed %d bytes, beyond the %d bytes the client supplied", off, maxCommitted)
					}
					return off, state, time.Since(start), observations, nil
				}
			}
		}
		if time.Since(start) >= timeout {
			if up, ok := rm.GetUploadByItem(batchID, itemID); ok {
				m := up.Meta()
				return m.CommittedOffset, m.State, time.Since(start), observations,
					fmt.Errorf("upload %s/%s did not settle as interrupted within %s (state=%s committed=%d)",
						batchID, itemID, timeout, m.State, m.CommittedOffset)
			}
			return 0, "", time.Since(start), observations,
				fmt.Errorf("upload %s/%s not found within %s", batchID, itemID, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// checkPrefixDigest enforces the strict prefix-integrity rule: for any nonzero
// committed offset the server MUST present a well-formed Upload-Prefix-Digest
// that matches the locally recomputed digest of the committed prefix. Missing,
// malformed and mismatched digests all invalidate the recovery.
func checkPrefixDigest(committedOffset int64, serverDigest, localDigest string) error {
	if committedOffset <= 0 {
		// Nothing committed: there is no prefix whose integrity could differ.
		return nil
	}
	if serverDigest == "" {
		return fmt.Errorf("committed offset %d but server supplied no Upload-Prefix-Digest; prefix integrity cannot be established", committedOffset)
	}
	if !isLowerHex64(serverDigest) {
		return fmt.Errorf("server supplied malformed Upload-Prefix-Digest %q", serverDigest)
	}
	if localDigest != serverDigest {
		return fmt.Errorf("prefix digest mismatch: local %s != server %s", localDigest, serverDigest)
	}
	return nil
}

// isLowerHex64 reports whether s is exactly 64 lowercase hex characters, the
// only shape a SHA-256 prefix digest may take.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RunConfig6_InterruptionRecovery interrupts an in-flight tus upload at
// interruptPercent of the payload, waits for the server to settle, discovers the
// authoritative committed offset with HEAD, rebuilds the client hasher over that
// prefix, resumes from the server's offset, completes with the full digest and
// verifies the published file.
func (h *BenchHarness) RunConfig6_InterruptionRecovery(fixture FixtureMetadata, interruptPercent int) (RecoveryRunResult, error) {
	res := RecoveryRunResult{
		RunID:            h.runID,
		PayloadSize:      fixture.SizeBytes,
		PayloadName:      fixture.Name,
		InterruptPercent: interruptPercent,
		IsolatedSession:  h.isolated,
	}
	itemName := fmt.Sprintf("recover_%dpct_%s", interruptPercent, fixture.Name)

	rm := h.services.EnsureResumable()
	rm.SetCheckpointParams(transfer.DefaultCheckpointBytes, transfer.DefaultCheckpointDuration)

	bID, itID, err := h.createBatch(itemName, fixture.SizeBytes)
	if err != nil {
		return res, err
	}

	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", h.serverURL, bID, itID)
	cReq, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	cReq.Header.Set("Authorization", "Bearer "+h.token)
	cReq.Header.Set("Tus-Resumable", "1.0.0")
	cReq.Header.Set("Upload-Length", strconv.FormatInt(fixture.SizeBytes, 10))
	cResp, err := h.client.Do(cReq)
	if err != nil {
		return res, err
	}
	cCode := cResp.StatusCode
	cResp.Body.Close()
	if stErr := expectStatus("tus create", cCode, http.StatusCreated); stErr != nil {
		return res, stErr
	}

	interruptByteTarget := (fixture.SizeBytes * int64(interruptPercent)) / 100
	if interruptByteTarget <= 0 {
		interruptByteTarget = 1
	}
	res.InterruptOffset = interruptByteTarget

	// Step 1: interrupt the PATCH. The request declares the FULL upload length
	// while the reader supplies only interruptByteTarget bytes and then fails,
	// so the transport aborts mid-body. The server therefore observes a genuine
	// truncated HTTP request (io.ErrUnexpectedEOF) and must keep the durably
	// committed prefix. Declaring ContentLength == interruptByteTarget would
	// describe a complete request and prove nothing about interruption recovery.
	file, err := os.Open(fixture.Path)
	if err != nil {
		return res, err
	}
	interrupted := &interruptReader{r: file, remaining: interruptByteTarget, err: io.ErrUnexpectedEOF}

	patchReq, _ := http.NewRequest(http.MethodPatch, tusURL, interrupted)
	patchReq.Header.Set("Authorization", "Bearer "+h.token)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Upload-Offset", "0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.ContentLength = fixture.SizeBytes
	res.InterruptDeclaredLength = fixture.SizeBytes

	t0Total := time.Now()
	t0Interrupt := time.Now()
	pResp, pErr := h.client.Do(patchReq)
	tDetect := time.Since(t0Interrupt)
	res.InterruptBytesSent = interrupted.sent
	_ = file.Close()
	if pResp != nil {
		res.InterruptServerStatus = pResp.StatusCode
		if oh := strings.TrimSpace(pResp.Header.Get("Upload-Offset")); oh != "" {
			if parsed, parseErr := strconv.ParseInt(oh, 10, 64); parseErr == nil {
				res.InterruptServerOffset = parsed
			}
		}
		pResp.Body.Close()
		if pResp.StatusCode == http.StatusNoContent {
			return res, fmt.Errorf("interrupted PATCH unexpectedly returned 204 No Content: the server saw a complete request, not a truncated one")
		}
		res.InterruptClientErr = fmt.Sprintf("client observed HTTP %d", pResp.StatusCode)
	}
	if pErr != nil {
		res.InterruptClientErr = pErr.Error()
	}
	if res.InterruptClientErr == "" {
		res.InterruptClientErr = "client observed no error (server closed the stream)"
	}
	res.DetectTimeMs = tDetect.Seconds() * 1000
	if res.InterruptBytesSent != interruptByteTarget {
		return res, fmt.Errorf("interrupt reader supplied %d bytes, expected exactly %d", res.InterruptBytesSent, interruptByteTarget)
	}

	// Step 2: bounded server settlement. The response above may never arrive —
	// the connection was aborted mid-body — so the server-side metadata is the
	// only authoritative evidence that an interruption actually happened. Wait
	// for the handler to reach its terminal `interrupted` state, release the
	// lifecycle lock, and stop moving the committed offset.
	settledOffset, settledState, settleDur, settleObs, settleErr := h.waitForInterruptedSettlement(bID, itID, 10*time.Second, interruptByteTarget)
	res.SettleWaitMs = settleDur.Seconds() * 1000
	res.SettleObservations = settleObs
	res.InterruptedState = string(settledState)
	res.ServerSawTruncation = settledState == transfer.ResumableInterrupted
	if settleErr != nil {
		res.Notes = strings.TrimSpace(res.Notes + " | settlement: " + settleErr.Error())
		return res, settleErr
	}
	if !res.ServerSawTruncation {
		return res, fmt.Errorf("server state after interruption was %q, expected %q", settledState, transfer.ResumableInterrupted)
	}
	if settledOffset >= fixture.SizeBytes {
		return res, fmt.Errorf("server committed %d of %d bytes: the upload is not recoverable", settledOffset, fixture.SizeBytes)
	}

	// Simulated client reconnect latency. This is a harness simulation, not a
	// measured network event, and is labelled as such in every artifact.
	t0Reconnect := time.Now()
	time.Sleep(10 * time.Millisecond)
	tReconnect := time.Since(t0Reconnect)
	res.ReconnectTimeMs = tReconnect.Seconds() * 1000

	// Step 3: HEAD discovery. The committed offset is authoritative; it is not
	// assumed to equal the interrupt byte target.
	t0Head := time.Now()
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+h.token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headResp, err := h.client.Do(headReq)
	if err != nil {
		return res, fmt.Errorf("HEAD query failed: %w", err)
	}
	headCode := headResp.StatusCode
	serverOffsetStr := headResp.Header.Get("Upload-Offset")
	serverPrefixDigest := headResp.Header.Get("Upload-Prefix-Digest")
	headResp.Body.Close()
	if stErr := expectStatus("tus HEAD", headCode, http.StatusOK); stErr != nil {
		return res, stErr
	}
	res.HeadTimeMs = time.Since(t0Head).Seconds() * 1000

	serverOffset, err := strconv.ParseInt(strings.TrimSpace(serverOffsetStr), 10, 64)
	if err != nil {
		return res, fmt.Errorf("HEAD returned unparsable Upload-Offset %q", serverOffsetStr)
	}
	if serverOffset < 0 || serverOffset > fixture.SizeBytes {
		return res, fmt.Errorf("HEAD returned out-of-range Upload-Offset %d", serverOffset)
	}
	headerLength := strings.TrimSpace(headResp.Header.Get("Upload-Length"))
	if headerLength == "" {
		return res, fmt.Errorf("HEAD returned no Upload-Length header")
	}
	if parsedLen, parseErr := strconv.ParseInt(headerLength, 10, 64); parseErr != nil || parsedLen != fixture.SizeBytes {
		return res, fmt.Errorf("HEAD returned Upload-Length %q, expected %d", headerLength, fixture.SizeBytes)
	}
	res.ServerCommitted = serverOffset
	if serverOffset <= 0 {
		return res, fmt.Errorf("HEAD reported committed offset 0: no recoverable prefix survived the interruption")
	}
	if serverOffset > res.InterruptOffset {
		return res, fmt.Errorf("HEAD committed offset %d exceeds the %d bytes the client supplied", serverOffset, res.InterruptOffset)
	}
	// Both views are server-side and no writer exists after settlement, so any
	// disagreement means the upload moved underneath us and HEAD cannot be
	// trusted as the resume point.
	if serverOffset != settledOffset {
		return res, fmt.Errorf("HEAD offset %d disagrees with the settled authoritative offset %d", serverOffset, settledOffset)
	}

	// Step 4: rebuild the hasher over the committed prefix and verify it
	// against the server's Upload-Prefix-Digest before resuming.
	t0Rebuild := time.Now()
	rebuiltHasher, err := hashPrefix(fixture.Path, serverOffset)
	if err != nil {
		return res, err
	}
	res.PrefixRebuildMs = time.Since(t0Rebuild).Seconds() * 1000

	t0Verify := time.Now()
	localPrefixDigest := hex.EncodeToString(rebuiltHasher.Sum(nil))
	res.PrefixVerifyMs = time.Since(t0Verify).Seconds() * 1000
	res.PrefixDigestServer = serverPrefixDigest
	res.PrefixDigestLocal = localPrefixDigest

	// Strict verification: a nonzero committed offset REQUIRES a well-formed
	// digest that matches. Treating an absent header as success (the previous
	// `serverPrefixDigest == "" || ...` form) would let a server that lost its
	// hash state resume from a prefix the client cannot confirm.
	if err := checkPrefixDigest(serverOffset, serverPrefixDigest, localPrefixDigest); err != nil {
		return res, fmt.Errorf("HEAD prefix verification: %w", err)
	}
	res.PrefixVerified = true

	// Step 5: resume from the server's committed offset. Only the two documented
	// transient refusals are retried (423 Locked, 409 offset mismatch) and each
	// retry resumes from the offset the server reports in that same response.
	const maxResumeAttempts = 3
	fullDigest := ""
	for attempt := 1; ; attempt++ {
		res.ResumeAttempts = attempt
		res.ResumeStartOffset = serverOffset

		hasher, hErr := hashPrefix(fixture.Path, serverOffset)
		if hErr != nil {
			return res, hErr
		}
		remainingFile, openErr := os.Open(fixture.Path)
		if openErr != nil {
			return res, openErr
		}
		if _, seekErr := remainingFile.Seek(serverOffset, io.SeekStart); seekErr != nil {
			_ = remainingFile.Close()
			return res, seekErr
		}

		t0FirstByte := time.Now()
		hr := &hashingReader{r: remainingFile, hasher: hasher}
		resumePatchReq, _ := http.NewRequest(http.MethodPatch, tusURL, hr)
		resumePatchReq.Header.Set("Authorization", "Bearer "+h.token)
		resumePatchReq.Header.Set("Tus-Resumable", "1.0.0")
		resumePatchReq.Header.Set("Upload-Offset", strconv.FormatInt(serverOffset, 10))
		resumePatchReq.Header.Set("Content-Type", "application/offset+octet-stream")
		resumePatchReq.ContentLength = fixture.SizeBytes - serverOffset
		res.FirstResumedByteMs = time.Since(t0FirstByte).Seconds() * 1000

		t0Resume := time.Now()
		resumeResp, resumeErr := h.client.Do(resumePatchReq)
		res.ResumeStreamTimeMs = time.Since(t0Resume).Seconds() * 1000
		_ = remainingFile.Close()
		if resumeErr != nil {
			return res, fmt.Errorf("resumed PATCH failed: %w", resumeErr)
		}
		resumeCode := resumeResp.StatusCode
		authoritativeOffset := strings.TrimSpace(resumeResp.Header.Get("Upload-Offset"))
		resumeState := resumeResp.Header.Get("Upload-State")
		resumeBody, _ := io.ReadAll(io.LimitReader(resumeResp.Body, 4096))
		resumeResp.Body.Close()
		resumeBodyText := strings.TrimSpace(string(resumeBody))

		if resumeCode == http.StatusNoContent {
			finalOffset, parseErr := strconv.ParseInt(authoritativeOffset, 10, 64)
			if parseErr != nil {
				return res, fmt.Errorf("resumed PATCH returned unparsable Upload-Offset %q", authoritativeOffset)
			}
			res.FinalOffset = finalOffset
			res.UploadState = resumeState
			if finalOffset != fixture.SizeBytes {
				return res, fmt.Errorf("resumed PATCH reported offset %d, expected %d", finalOffset, fixture.SizeBytes)
			}
			if resumeState != string(transfer.ResumableAwaitingDigest) {
				return res, fmt.Errorf("resumed PATCH reported state %q, expected %q", resumeState, transfer.ResumableAwaitingDigest)
			}
			fullDigest = hex.EncodeToString(hasher.Sum(nil))
			break
		}

		if resumeCode == http.StatusConflict && authoritativeOffset == "" {
			// A 409 without Upload-Offset is not a stale-offset conflict: it is an
			// approval/state refusal (e.g. "upload awaits desktop approval") and
			// must never be retried as if the offsets had merely diverged.
			return res, fmt.Errorf("resumed tus PATCH refused with HTTP 409 and no Upload-Offset: %s", resumeBodyText)
		}
		transient := resumeCode == http.StatusLocked || resumeCode == http.StatusConflict
		if !transient {
			return res, fmt.Errorf("resumed tus PATCH returned HTTP %d: %s", resumeCode, resumeBodyText)
		}
		if attempt >= maxResumeAttempts {
			return res, fmt.Errorf("resumed tus PATCH still refused after %d attempts (HTTP %d, Upload-Offset %q)", attempt, resumeCode, authoritativeOffset)
		}

		nextOffset := int64(-1)
		if authoritativeOffset != "" {
			if parsed, parseErr := strconv.ParseInt(authoritativeOffset, 10, 64); parseErr == nil {
				nextOffset = parsed
			}
		}
		if nextOffset < 0 {
			// No offset in the refusal: re-settle and re-discover.
			if settled, _, sErr := h.waitForUploadSettled(bID, itID, time.Second); sErr == nil {
				nextOffset = settled
			}
		}
		if nextOffset < 0 || nextOffset > fixture.SizeBytes {
			return res, fmt.Errorf("cannot determine authoritative offset after HTTP %d", resumeCode)
		}
		serverOffset = nextOffset
	}

	// Step 6: complete with the full-file digest and confirm authoritative state.
	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", h.serverURL, bID, itID)
	compBody, _ := json.Marshal(map[string]string{"expectedSHA256": fullDigest})
	compReq, _ := http.NewRequest(http.MethodPost, completeURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+h.token)
	compReq.Header.Set("Content-Type", "application/json")
	compResp, err := h.client.Do(compReq)
	if err != nil {
		return res, fmt.Errorf("completion failed: %w", err)
	}
	compCode := compResp.StatusCode
	compResp.Body.Close()
	if stErr := expectStatus("recovery complete", compCode, http.StatusOK); stErr != nil {
		return res, stErr
	}

	statusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/status", h.serverURL, bID, itID)
	stReq, _ := http.NewRequest(http.MethodGet, statusURL, nil)
	stReq.Header.Set("Authorization", "Bearer "+h.token)
	stResp, err := h.client.Do(stReq)
	if err != nil {
		return res, fmt.Errorf("status query failed: %w", err)
	}
	stCode := stResp.StatusCode
	var statusBody struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(stResp.Body).Decode(&statusBody)
	stResp.Body.Close()
	if stErr := expectStatus("recovery status", stCode, http.StatusOK); stErr != nil {
		return res, stErr
	}
	res.AuthoritativeStatus = statusBody.Status
	if statusBody.Status != string(transfer.ResumableCompleted) {
		return res, fmt.Errorf("authoritative status is %q, expected %q", statusBody.Status, transfer.ResumableCompleted)
	}

	res.TotalCompletionMs = time.Since(t0Total).Seconds() * 1000

	receivedPath := filepath.Join(h.receiveDir, itemName)
	pass, verr := verifyReceivedFile(receivedPath, fixture.SHA256, fixture.SizeBytes)
	_ = os.Remove(receivedPath)

	// Retransmission is measured against what the server durably committed, not
	// against the intended percentage: bytes the client pushed but the server
	// never checkpointed must be sent again.
	retransmitted := interruptByteTarget - serverOffset
	if retransmitted < 0 {
		retransmitted = 0
	}
	res.RetransmittedBytes = retransmitted
	res.IntegrityPass = pass && fullDigest == fixture.SHA256 && verr == nil
	res.Valid = res.IntegrityPass && res.PrefixVerified && res.ServerSawTruncation &&
		res.ServerCommitted > 0 && res.ServerCommitted < fixture.SizeBytes

	note := "Loopback harness: interrupt = full Content-Length declared, body aborted with io.ErrUnexpectedEOF after the target prefix, so the server observed a truncated HTTP request; server evidence = interrupted state + committed offset strictly between 0 and Upload-Length; reconnect wait = SIMULATED 10ms sleep; settle = bounded poll for the interrupted state, a free lifecycle lock and a stable committed offset before HEAD; retransmitted = interrupt target - server committed offset"
	if h.isolated {
		note += "; isolated recovery session"
	}
	if verr != nil {
		note = "INTEGRITY FAILURE: " + verr.Error() + " | " + note
	}
	res.Notes = strings.TrimSpace(res.Notes + " | " + note)
	if !res.Valid {
		if verr != nil {
			return res, verr
		}
		return res, fmt.Errorf("recovery validation failed (integrity=%v digestMatch=%v prefixVerified=%v)", pass, fullDigest == fixture.SHA256, res.PrefixVerified)
	}
	return res, nil
}
