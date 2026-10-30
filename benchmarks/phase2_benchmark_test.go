package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/audit"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// --- Microbenchmarks for testing.B ---

func BenchmarkStreamingCopyBufferSizes(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"Buf32K", 32 * 1024},
		{"Buf64K", 64 * 1024},
		{"Buf128K", 128 * 1024},
		{"Buf256K", 256 * 1024},
		{"Buf512K", 512 * 1024},
	}

	testDataSize := int64(10 * 1024 * 1024) // 10 MB per iteration
	sourceData := make([]byte, testDataSize)
	for i := range sourceData {
		sourceData[i] = byte(i & 0xff)
	}

	for _, tc := range sizes {
		b.Run(tc.name, func(b *testing.B) {
			buf := make([]byte, tc.size)
			hasher := sha256.New()
			b.SetBytes(testDataSize)
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				src := bytes.NewReader(sourceData)
				hasher.Reset()
				tee := io.MultiWriter(io.Discard, hasher)
				_, err := io.CopyBuffer(tee, src, buf)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBufferAllocationVsPool(b *testing.B) {
	bufSize := 256 * 1024
	testDataSize := int64(5 * 1024 * 1024)
	sourceData := make([]byte, testDataSize)

	b.Run("FreshAllocEachOp", func(b *testing.B) {
		b.SetBytes(testDataSize)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			buf := make([]byte, bufSize)
			src := bytes.NewReader(sourceData)
			_, _ = io.CopyBuffer(io.Discard, src, buf)
		}
	})

	var pool = sync.Pool{
		New: func() any {
			b := make([]byte, bufSize)
			return &b
		},
	}

	b.Run("SyncPoolReuse", func(b *testing.B) {
		b.SetBytes(testDataSize)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			bufPtr := pool.Get().(*[]byte)
			buf := *bufPtr
			src := bytes.NewReader(sourceData)
			_, _ = io.CopyBuffer(io.Discard, src, buf)
			pool.Put(bufPtr)
		}
	})
}

// --- End-to-End Baseline Test Suite ---

type Phase2Metrics struct {
	TTFBMs          float64 `json:"ttfb_ms"`
	StreamTimeMs    float64 `json:"stream_time_ms"`
	TotalTimeMs     float64 `json:"total_time_ms"`
	BytesRead       int64   `json:"bytes_read"`
	StreamMBs       float64 `json:"stream_mbs"`
	EffectiveMBs    float64 `json:"effective_mbs"`
	PeakHeapAllocMB float64 `json:"peak_heap_alloc_mb"`
	AllocBytesTotal int64   `json:"alloc_bytes_total"`
	NumGC           uint32  `json:"num_gc"`
	GoroutinesPeak  int     `json:"goroutines_peak"`
	CPUTimeMs       float64 `json:"cpu_time_ms"`
	IntegrityPass   bool    `json:"integrity_pass"`
}

type Phase2ScenarioResult struct {
	Category    string          `json:"category"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	FileCount   int             `json:"file_count"`
	TotalBytes  int64           `json:"total_bytes"`
	Concurrency int             `json:"concurrency"`
	Runs        []Phase2Metrics `json:"runs"`
	Median      Phase2Metrics   `json:"median"`
}

func getPhase2CPUTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
		sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
		return user + sys
	}
	return 0
}

func getPhase2Median(runs []Phase2Metrics) Phase2Metrics {
	if len(runs) == 0 {
		return Phase2Metrics{}
	}
	sorted := make([]Phase2Metrics, len(runs))
	copy(sorted, runs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].TotalTimeMs < sorted[j].TotalTimeMs
	})
	return sorted[len(sorted)/2]
}

func createTestPayload(path string, size int64) {
	if fi, err := os.Stat(path); err == nil && fi.Size() == size {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	chunk := make([]byte, 256*1024)
	for i := range chunk {
		chunk[i] = byte((i * 41) & 0xff)
	}

	var written int64
	for written < size {
		toWrite := int64(len(chunk))
		if size-written < toWrite {
			toWrite = size - written
		}
		if _, err := f.Write(chunk[:toWrite]); err != nil {
			panic(err)
		}
		written += toWrite
	}
}

func TestPhase2BaselineSuite(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "flitnix-phase2-bench-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	dataDir := filepath.Join(tempDir, "data")
	_ = os.MkdirAll(dataDir, 0o755)

	sessMgr := session.NewManager(session.Config{MaxSessions: 128})
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: filepath.Join(tempDir, "recv")})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	reg := transfer.NewRegistry()
	dls := transfer.NewDownloadRegistry()
	auditStore, _ := audit.NewStore(filepath.Join(tempDir, "audit.db"), 1000)

	svcs := &server.Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    reg,
		Downloads:   dls,
		Audit:       auditStore,
	}

	srv := server.New(server.Config{
		ListenAddr: "127.0.0.1:0",
		Services:   svcs,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Serve(ctx)
	}()
	<-srv.Ready()

	serverURL := srv.URL()
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 300 * time.Second,
	}

	var allResults []Phase2ScenarioResult

	// ==========================================
	// Category A: Single-File Transfers
	// ==========================================
	t.Log("Benchmarking Category A: Single-File Transfers...")

	// A1: 100 MB
	f100 := filepath.Join(dataDir, "single_100mb.bin")
	createTestPayload(f100, 100*1024*1024)
	resA1 := runSingleFileBenchmark(client, serverURL, sessMgr, reg, "A", "100 MB File", "Single 100 MB download via grant stream", f100, 3)
	allResults = append(allResults, resA1)

	// A2: 1 GB
	f1GB := filepath.Join(dataDir, "single_1gb.bin")
	createTestPayload(f1GB, 1024*1024*1024)
	resA2 := runSingleFileBenchmark(client, serverURL, sessMgr, reg, "A", "1 GB File", "Single 1 GB download via grant stream", f1GB, 3)
	allResults = append(allResults, resA2)

	// A3: 2 GB
	f2GB := filepath.Join(dataDir, "single_2gb.bin")
	createTestPayload(f2GB, 2*1024*1024*1024)
	resA3 := runSingleFileBenchmark(client, serverURL, sessMgr, reg, "A", "2 GB File", "Single 2 GB download via grant stream", f2GB, 3)
	allResults = append(allResults, resA3)

	// ==========================================
	// Category B: Multiple-File Transfers
	// ==========================================
	t.Log("Benchmarking Category B: Multiple-File Transfers...")

	// B1: 10 medium files (10 x 10 MB = 100 MB)
	var b1Paths []string
	for i := 0; i < 10; i++ {
		p := filepath.Join(dataDir, fmt.Sprintf("batch_10x10mb_%d.bin", i))
		createTestPayload(p, 10*1024*1024)
		b1Paths = append(b1Paths, p)
	}
	resB1 := runBatchBenchmark(client, serverURL, sessMgr, reg, "B", "10 Medium Files (10x10MB)", "10 files x 10 MB as ZIP archive download", b1Paths, 3)
	allResults = append(allResults, resB1)

	// B2: 100 medium files (100 x 1 MB = 100 MB)
	var b2Paths []string
	b2Dir := filepath.Join(dataDir, "batch_100x1mb")
	for i := 0; i < 100; i++ {
		p := filepath.Join(b2Dir, fmt.Sprintf("file_%03d.bin", i))
		createTestPayload(p, 1*1024*1024)
		b2Paths = append(b2Paths, p)
	}
	resB2 := runBatchBenchmark(client, serverURL, sessMgr, reg, "B", "100 Medium Files (100x1MB)", "100 files x 1 MB as ZIP archive download", b2Paths, 3)
	allResults = append(allResults, resB2)

	// B3: 500 small files (500 x 20 KB = 10 MB)
	b3Dir := filepath.Join(dataDir, "batch_500x20kb")
	for i := 0; i < 500; i++ {
		p := filepath.Join(b3Dir, fmt.Sprintf("small_%03d.dat", i))
		createTestPayload(p, 20*1024)
	}
	resB3 := runFolderBenchmark(client, serverURL, sessMgr, reg, "B", "500 Small Files (500x20KB)", "500 small files in folder as ZIP archive", b3Dir, 500, 3)
	allResults = append(allResults, resB3)

	// B4: 1,000 small files (1,000 x 20 KB = 20 MB)
	b4Dir := filepath.Join(dataDir, "batch_1000x20kb")
	for i := 0; i < 1000; i++ {
		p := filepath.Join(b4Dir, fmt.Sprintf("small_%04d.dat", i))
		createTestPayload(p, 20*1024)
	}
	resB4 := runFolderBenchmark(client, serverURL, sessMgr, reg, "B", "1,000 Small Files (1000x20KB)", "1,000 small files in folder as ZIP archive", b4Dir, 1000, 3)
	allResults = append(allResults, resB4)

	// ==========================================
	// Category C: Archive Transfers
	// ==========================================
	t.Log("Benchmarking Category C: Archive Transfers...")

	// C1: Small folder (5 files x 1 MB = 5 MB)
	c1Dir := filepath.Join(dataDir, "folder_small")
	for i := 0; i < 5; i++ {
		createTestPayload(filepath.Join(c1Dir, fmt.Sprintf("doc_%d.pdf", i)), 1024*1024)
	}
	resC1 := runFolderBenchmark(client, serverURL, sessMgr, reg, "C", "Small Folder (5x1MB)", "Small folder with 5 files as ZIP", c1Dir, 5, 3)
	allResults = append(allResults, resC1)

	// C2: Medium folder (20 files x 5 MB = 100 MB)
	c2Dir := filepath.Join(dataDir, "folder_medium")
	for i := 0; i < 20; i++ {
		createTestPayload(filepath.Join(c2Dir, fmt.Sprintf("video_%d.mp4", i)), 5*1024*1024)
	}
	resC2 := runFolderBenchmark(client, serverURL, sessMgr, reg, "C", "Medium Folder (20x5MB)", "Medium folder with 20 files as ZIP", c2Dir, 20, 3)
	allResults = append(allResults, resC2)

	// C3: Large folder (5 files x 100 MB = 500 MB)
	c3Dir := filepath.Join(dataDir, "folder_large")
	for i := 0; i < 5; i++ {
		createTestPayload(filepath.Join(c3Dir, fmt.Sprintf("archive_%d.iso", i)), 100*1024*1024)
	}
	resC3 := runFolderBenchmark(client, serverURL, sessMgr, reg, "C", "Large Folder (5x100MB)", "Large folder with 5x100MB files as ZIP", c3Dir, 5, 3)
	allResults = append(allResults, resC3)

	// ==========================================
	// Category D: Concurrency (1, 2, 4, 8 streams)
	// ==========================================
	t.Log("Benchmarking Category D: Concurrency...")

	concurrencyLevels := []int{1, 2, 4, 8}
	payloadPerStream := int64(100 * 1024 * 1024) // 100 MB per stream
	for _, conc := range concurrencyLevels {
		var concPaths []string
		for i := 0; i < conc; i++ {
			p := filepath.Join(dataDir, fmt.Sprintf("conc_%d_%d.bin", conc, i))
			createTestPayload(p, payloadPerStream)
			concPaths = append(concPaths, p)
		}
		resD := runConcurrentBenchmark(client, serverURL, sessMgr, reg, conc, concPaths, 3)
		allResults = append(allResults, resD)
	}

	// Print Summary Table
	fmt.Println("\n==================================================================================================================================================")
	fmt.Println(" FLITNIX PHASE 2 TRANSFER ENGINE BENCHMARK REPORT (Median of 3 Runs per Scenario)")
	fmt.Println("==================================================================================================================================================")
	fmt.Printf("%-3s | %-32s | %9s | %7s | %8s | %10s | %11s | %11s | %9s | %8s\n",
		"Cat", "Scenario", "Size", "Streams", "TTFB(ms)", "Time(ms)", "Stream MB/s", "Eff. MB/s", "Heap(MB)", "CPU(ms)")
	fmt.Println("--------------------------------------------------------------------------------------------------------------------------------------------------")
	for _, res := range allResults {
		m := res.Median
		fmt.Printf("%-3s | %-32s | %7.1f MB | %7d | %8.2f | %10.1f | %9.1f MB/s | %9.1f MB/s | %7.2f MB | %8.1f\n",
			res.Category, res.Name, float64(res.TotalBytes)/(1024*1024), res.Concurrency, m.TTFBMs, m.TotalTimeMs, m.StreamMBs, m.EffectiveMBs, m.PeakHeapAllocMB, m.CPUTimeMs)
	}
	fmt.Println("==================================================================================================================================================")

	outJSON, _ := json.MarshalIndent(allResults, "", "  ")
	_ = os.WriteFile("phase2_baseline_results.json", outJSON, 0o644)
}

func runSingleFileBenchmark(client *http.Client, serverURL string, sessMgr *session.Manager, reg *transfer.Registry, cat, name, desc, filePath string, runs int) Phase2ScenarioResult {
	fi, _ := os.Stat(filePath)
	totalBytes := fi.Size()
	var metrics []Phase2Metrics

	for r := 0; r < runs; r++ {
		runtime.GC()
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)
		cpuBefore := getPhase2CPUTime()

		sess, _ := sessMgr.Create()
		token := sess.Token()

		t0 := time.Now()
		source, err := reg.AddFileSource(filePath, filepath.Base(filePath), "")
		if err != nil {
			panic(err)
		}

		grantReqBody, _ := json.Marshal(map[string]any{"sourceIds": []string{source.ID}})
		req, _ := http.NewRequest("POST", serverURL+"/api/download-grants", bytes.NewReader(grantReqBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}

		var grantRes struct {
			DownloadURL string `json:"downloadUrl"`
			ProgressID  string `json:"progressId"`
			SHA256      string `json:"sha256"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&grantRes)
		cookies := resp.Cookies()
		resp.Body.Close()

		t1 := time.Now()
		getReq, _ := http.NewRequest("GET", serverURL+grantRes.DownloadURL, nil)
		for _, c := range cookies {
			getReq.AddCookie(c)
		}
		getResp, err := client.Do(getReq)
		if err != nil {
			panic(err)
		}
		ttfbDur := time.Since(t1)

		t2 := time.Now()
		hasher := sha256.New()
		buf := make([]byte, 256*1024)
		nRead, err := io.CopyBuffer(hasher, getResp.Body, buf)
		getResp.Body.Close()
		if err != nil {
			panic(err)
		}
		streamDur := time.Since(t2)
		totalDur := time.Since(t0)
		cpuAfter := getPhase2CPUTime()
		runtime.ReadMemStats(&mAfter)

		actualSHA := hex.EncodeToString(hasher.Sum(nil))

		// Check progress API for published digest
		pReq, _ := http.NewRequest("GET", serverURL+"/api/download-progress/"+grantRes.ProgressID, nil)
		pReq.Header.Set("Authorization", "Bearer "+token)
		pResp, err := client.Do(pReq)
		var pData struct {
			State  string `json:"state"`
			SHA256 string `json:"sha256"`
		}
		if err == nil {
			_ = json.NewDecoder(pResp.Body).Decode(&pData)
			pResp.Body.Close()
		}
		integrityPass := (pData.State == "completed" && pData.SHA256 == actualSHA)

		reg.Remove(source.ID)

		streamSec := streamDur.Seconds()
		if streamSec <= 0 {
			streamSec = 0.0001
		}
		totalSec := totalDur.Seconds()
		if totalSec <= 0 {
			totalSec = 0.0001
		}

		metrics = append(metrics, Phase2Metrics{
			TTFBMs:          float64(ttfbDur.Microseconds()) / 1000.0,
			StreamTimeMs:    float64(streamDur.Microseconds()) / 1000.0,
			TotalTimeMs:     float64(totalDur.Microseconds()) / 1000.0,
			BytesRead:       nRead,
			StreamMBs:       float64(nRead) / streamSec / 1000000.0,
			EffectiveMBs:    float64(nRead) / totalSec / 1000000.0,
			PeakHeapAllocMB: float64(mAfter.HeapAlloc) / (1024 * 1024),
			AllocBytesTotal: int64(mAfter.TotalAlloc - mBefore.TotalAlloc),
			NumGC:           mAfter.NumGC - mBefore.NumGC,
			GoroutinesPeak:  runtime.NumGoroutine(),
			CPUTimeMs:       float64((cpuAfter - cpuBefore).Microseconds()) / 1000.0,
			IntegrityPass:   integrityPass,
		})
	}

	return Phase2ScenarioResult{
		Category:    cat,
		Name:        name,
		Description: desc,
		FileCount:   1,
		TotalBytes:  totalBytes,
		Concurrency: 1,
		Runs:        metrics,
		Median:      getPhase2Median(metrics),
	}
}

func runBatchBenchmark(client *http.Client, serverURL string, sessMgr *session.Manager, reg *transfer.Registry, cat, name, desc string, paths []string, runs int) Phase2ScenarioResult {
	var totalBytes int64
	for _, p := range paths {
		fi, _ := os.Stat(p)
		totalBytes += fi.Size()
	}
	var metrics []Phase2Metrics

	for r := 0; r < runs; r++ {
		runtime.GC()
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)
		cpuBefore := getPhase2CPUTime()

		sess, _ := sessMgr.Create()
		token := sess.Token()

		t0 := time.Now()
		var sourceIDs []string
		for _, p := range paths {
			source, err := reg.AddFileSource(p, filepath.Base(p), "")
			if err != nil {
				panic(err)
			}
			sourceIDs = append(sourceIDs, source.ID)
		}

		grantReqBody, _ := json.Marshal(map[string]any{"sourceIds": sourceIDs})
		req, _ := http.NewRequest("POST", serverURL+"/api/download-grants", bytes.NewReader(grantReqBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}

		var grantRes struct {
			DownloadURL string `json:"downloadUrl"`
			ProgressID  string `json:"progressId"`
			SHA256      string `json:"sha256"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&grantRes)
		cookies := resp.Cookies()
		resp.Body.Close()

		t1 := time.Now()
		getReq, _ := http.NewRequest("GET", serverURL+grantRes.DownloadURL, nil)
		for _, c := range cookies {
			getReq.AddCookie(c)
		}
		getResp, err := client.Do(getReq)
		if err != nil {
			panic(err)
		}
		ttfbDur := time.Since(t1)

		t2 := time.Now()
		hasher := sha256.New()
		buf := make([]byte, 256*1024)
		nRead, err := io.CopyBuffer(hasher, getResp.Body, buf)
		getResp.Body.Close()
		if err != nil {
			panic(err)
		}
		streamDur := time.Since(t2)
		totalDur := time.Since(t0)
		cpuAfter := getPhase2CPUTime()
		runtime.ReadMemStats(&mAfter)

		actualSHA := hex.EncodeToString(hasher.Sum(nil))

		// Check progress API for published digest
		pReq, _ := http.NewRequest("GET", serverURL+"/api/download-progress/"+grantRes.ProgressID, nil)
		pReq.Header.Set("Authorization", "Bearer "+token)
		pResp, err := client.Do(pReq)
		var pData struct {
			State  string `json:"state"`
			SHA256 string `json:"sha256"`
		}
		if err == nil {
			_ = json.NewDecoder(pResp.Body).Decode(&pData)
			pResp.Body.Close()
		}
		integrityPass := (pData.State == "completed" && pData.SHA256 == actualSHA)

		for _, id := range sourceIDs {
			reg.Remove(id)
		}

		streamSec := streamDur.Seconds()
		if streamSec <= 0 {
			streamSec = 0.0001
		}
		totalSec := totalDur.Seconds()
		if totalSec <= 0 {
			totalSec = 0.0001
		}

		metrics = append(metrics, Phase2Metrics{
			TTFBMs:          float64(ttfbDur.Microseconds()) / 1000.0,
			StreamTimeMs:    float64(streamDur.Microseconds()) / 1000.0,
			TotalTimeMs:     float64(totalDur.Microseconds()) / 1000.0,
			BytesRead:       nRead,
			StreamMBs:       float64(nRead) / streamSec / 1000000.0,
			EffectiveMBs:    float64(nRead) / totalSec / 1000000.0,
			PeakHeapAllocMB: float64(mAfter.HeapAlloc) / (1024 * 1024),
			AllocBytesTotal: int64(mAfter.TotalAlloc - mBefore.TotalAlloc),
			NumGC:           mAfter.NumGC - mBefore.NumGC,
			GoroutinesPeak:  runtime.NumGoroutine(),
			CPUTimeMs:       float64((cpuAfter - cpuBefore).Microseconds()) / 1000.0,
			IntegrityPass:   integrityPass,
		})
	}

	return Phase2ScenarioResult{
		Category:    cat,
		Name:        name,
		Description: desc,
		FileCount:   len(paths),
		TotalBytes:  totalBytes,
		Concurrency: 1,
		Runs:        metrics,
		Median:      getPhase2Median(metrics),
	}
}

func runFolderBenchmark(client *http.Client, serverURL string, sessMgr *session.Manager, reg *transfer.Registry, cat, name, desc, folderPath string, fileCount, runs int) Phase2ScenarioResult {
	var metrics []Phase2Metrics
	var totalBytes int64

	for r := 0; r < runs; r++ {
		runtime.GC()
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)
		cpuBefore := getPhase2CPUTime()

		sess, _ := sessMgr.Create()
		token := sess.Token()

		t0 := time.Now()
		source, err := reg.AddFolder(folderPath, "")
		if err != nil {
			panic(err)
		}

		grantReqBody, _ := json.Marshal(map[string]any{"sourceIds": []string{source.ID}})
		req, _ := http.NewRequest("POST", serverURL+"/api/download-grants", bytes.NewReader(grantReqBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}

		var grantRes struct {
			DownloadURL string `json:"downloadUrl"`
			ProgressID  string `json:"progressId"`
			SHA256      string `json:"sha256"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&grantRes)
		cookies := resp.Cookies()
		resp.Body.Close()

		t1 := time.Now()
		getReq, _ := http.NewRequest("GET", serverURL+grantRes.DownloadURL, nil)
		for _, c := range cookies {
			getReq.AddCookie(c)
		}
		getResp, err := client.Do(getReq)
		if err != nil {
			panic(err)
		}
		ttfbDur := time.Since(t1)

		t2 := time.Now()
		hasher := sha256.New()
		buf := make([]byte, 256*1024)
		nRead, err := io.CopyBuffer(hasher, getResp.Body, buf)
		getResp.Body.Close()
		if err != nil {
			panic(err)
		}
		streamDur := time.Since(t2)
		totalDur := time.Since(t0)
		cpuAfter := getPhase2CPUTime()
		runtime.ReadMemStats(&mAfter)

		actualSHA := hex.EncodeToString(hasher.Sum(nil))

		// Check progress API for published digest
		pReq, _ := http.NewRequest("GET", serverURL+"/api/download-progress/"+grantRes.ProgressID, nil)
		pReq.Header.Set("Authorization", "Bearer "+token)
		pResp, err := client.Do(pReq)
		var pData struct {
			State  string `json:"state"`
			SHA256 string `json:"sha256"`
		}
		if err == nil {
			_ = json.NewDecoder(pResp.Body).Decode(&pData)
			pResp.Body.Close()
		}
		integrityPass := (pData.State == "completed" && pData.SHA256 == actualSHA)

		reg.Remove(source.ID)
		totalBytes = nRead

		streamSec := streamDur.Seconds()
		if streamSec <= 0 {
			streamSec = 0.0001
		}
		totalSec := totalDur.Seconds()
		if totalSec <= 0 {
			totalSec = 0.0001
		}

		metrics = append(metrics, Phase2Metrics{
			TTFBMs:          float64(ttfbDur.Microseconds()) / 1000.0,
			StreamTimeMs:    float64(streamDur.Microseconds()) / 1000.0,
			TotalTimeMs:     float64(totalDur.Microseconds()) / 1000.0,
			BytesRead:       nRead,
			StreamMBs:       float64(nRead) / streamSec / 1000000.0,
			EffectiveMBs:    float64(nRead) / totalSec / 1000000.0,
			PeakHeapAllocMB: float64(mAfter.HeapAlloc) / (1024 * 1024),
			AllocBytesTotal: int64(mAfter.TotalAlloc - mBefore.TotalAlloc),
			NumGC:           mAfter.NumGC - mBefore.NumGC,
			GoroutinesPeak:  runtime.NumGoroutine(),
			CPUTimeMs:       float64((cpuAfter - cpuBefore).Microseconds()) / 1000.0,
			IntegrityPass:   integrityPass,
		})
	}

	return Phase2ScenarioResult{
		Category:    cat,
		Name:        name,
		Description: desc,
		FileCount:   fileCount,
		TotalBytes:  totalBytes,
		Concurrency: 1,
		Runs:        metrics,
		Median:      getPhase2Median(metrics),
	}
}

func runConcurrentBenchmark(client *http.Client, serverURL string, sessMgr *session.Manager, reg *transfer.Registry, concurrency int, filePaths []string, runs int) Phase2ScenarioResult {
	var totalBytes int64
	for _, p := range filePaths {
		fi, _ := os.Stat(p)
		totalBytes += fi.Size()
	}

	var metrics []Phase2Metrics

	for r := 0; r < runs; r++ {
		runtime.GC()
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)
		cpuBefore := getPhase2CPUTime()

		// Each concurrent stream uses its own session to represent independent client streams
		type task struct {
			filePath string
			token    string
			sourceID string
			grantURL string
			cookies  []*http.Cookie
			progID   string
		}

		tasks := make([]*task, concurrency)
		for i := 0; i < concurrency; i++ {
			s, _ := sessMgr.Create()
			src, err := reg.AddFileSource(filePaths[i], filepath.Base(filePaths[i]), "")
			if err != nil {
				panic(err)
			}
			tasks[i] = &task{
				filePath: filePaths[i],
				token:    s.Token(),
				sourceID: src.ID,
			}
		}

		t0 := time.Now()
		// Phase 1: create grants
		for i := 0; i < concurrency; i++ {
			grantReqBody, _ := json.Marshal(map[string]any{"sourceIds": []string{tasks[i].sourceID}})
			req, _ := http.NewRequest("POST", serverURL+"/api/download-grants", bytes.NewReader(grantReqBody))
			req.Header.Set("Authorization", "Bearer "+tasks[i].token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				panic(err)
			}
			var gRes struct {
				DownloadURL string `json:"downloadUrl"`
				ProgressID  string `json:"progressId"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&gRes)
			tasks[i].cookies = resp.Cookies()
			tasks[i].grantURL = gRes.DownloadURL
			tasks[i].progID = gRes.ProgressID
			resp.Body.Close()
		}

		// Phase 2: download concurrently
		var wg sync.WaitGroup
		var totalRead int64
		var readMu sync.Mutex
		allIntegrity := true
		var integrityMu sync.Mutex

		t1 := time.Now()
		var firstByteTimes []time.Duration
		var fbMu sync.Mutex

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func(t *task) {
				defer wg.Done()
				tReq, _ := http.NewRequest("GET", serverURL+t.grantURL, nil)
				for _, c := range t.cookies {
					tReq.AddCookie(c)
				}
				tResp, err := client.Do(tReq)
				if err != nil {
					panic(err)
				}
				fb := time.Since(t1)
				fbMu.Lock()
				firstByteTimes = append(firstByteTimes, fb)
				fbMu.Unlock()

				hasher := sha256.New()
				buf := make([]byte, 256*1024)
				n, rErr := io.CopyBuffer(hasher, tResp.Body, buf)
				tResp.Body.Close()
				if rErr != nil {
					panic(rErr)
				}

				readMu.Lock()
				totalRead += n
				readMu.Unlock()

				actualHex := hex.EncodeToString(hasher.Sum(nil))

				// Check progress API
				pReq, _ := http.NewRequest("GET", serverURL+"/api/download-progress/"+t.progID, nil)
				pReq.Header.Set("Authorization", "Bearer "+t.token)
				pResp, pErr := client.Do(pReq)
				if pErr == nil {
					var pData struct {
						State  string `json:"state"`
						SHA256 string `json:"sha256"`
					}
					_ = json.NewDecoder(pResp.Body).Decode(&pData)
					pResp.Body.Close()
					integrityMu.Lock()
					if pData.State != "completed" || pData.SHA256 != actualHex {
						allIntegrity = false
					}
					integrityMu.Unlock()
				}
			}(tasks[i])
		}

		wg.Wait()
		totalDur := time.Since(t0)
		streamDur := time.Since(t1)
		cpuAfter := getPhase2CPUTime()
		runtime.ReadMemStats(&mAfter)

		for i := 0; i < concurrency; i++ {
			reg.Remove(tasks[i].sourceID)
		}

		var avgTTFB float64
		for _, d := range firstByteTimes {
			avgTTFB += float64(d.Microseconds()) / 1000.0
		}
		if len(firstByteTimes) > 0 {
			avgTTFB /= float64(len(firstByteTimes))
		}

		streamSec := streamDur.Seconds()
		if streamSec <= 0 {
			streamSec = 0.0001
		}
		totalSec := totalDur.Seconds()
		if totalSec <= 0 {
			totalSec = 0.0001
		}

		metrics = append(metrics, Phase2Metrics{
			TTFBMs:          avgTTFB,
			StreamTimeMs:    float64(streamDur.Microseconds()) / 1000.0,
			TotalTimeMs:     float64(totalDur.Microseconds()) / 1000.0,
			BytesRead:       totalRead,
			StreamMBs:       float64(totalRead) / streamSec / 1000000.0,
			EffectiveMBs:    float64(totalRead) / totalSec / 1000000.0,
			PeakHeapAllocMB: float64(mAfter.HeapAlloc) / (1024 * 1024),
			AllocBytesTotal: int64(mAfter.TotalAlloc - mBefore.TotalAlloc),
			NumGC:           mAfter.NumGC - mBefore.NumGC,
			GoroutinesPeak:  runtime.NumGoroutine(),
			CPUTimeMs:       float64((cpuAfter - cpuBefore).Microseconds()) / 1000.0,
			IntegrityPass:   allIntegrity,
		})
	}

	return Phase2ScenarioResult{
		Category:    "D",
		Name:        fmt.Sprintf("Concurrency: %d Streams", concurrency),
		Description: fmt.Sprintf("%d simultaneous active 100 MB streams", concurrency),
		FileCount:   concurrency,
		TotalBytes:  totalBytes,
		Concurrency: concurrency,
		Runs:        metrics,
		Median:      getPhase2Median(metrics),
	}
}
