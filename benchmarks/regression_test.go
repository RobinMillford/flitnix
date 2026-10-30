package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/audit"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

type BenchRunMetrics struct {
	SelectTimeMs    float64 `json:"select_time_ms"`
	GrantTimeMs     float64 `json:"grant_time_ms"`
	TTFBMs          float64 `json:"ttfb_ms"`
	StreamTimeMs    float64 `json:"stream_time_ms"`
	TotalTimeMs     float64 `json:"total_time_ms"`
	BytesRead       int64   `json:"bytes_read"`
	StreamMBs       float64 `json:"stream_mbs"`
	EffectiveMBs    float64 `json:"effective_mbs"`
	PeakHeapAllocMB float64 `json:"peak_heap_alloc_mb"`
	CPUTimeMs       float64 `json:"cpu_time_ms"`
	IntegrityPass   bool    `json:"integrity_pass"`
}

type BenchScenarioResult struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	FileCount    int               `json:"file_count"`
	TotalBytes   int64             `json:"total_bytes"`
	Runs         []BenchRunMetrics `json:"runs"`
	MedianRun    BenchRunMetrics   `json:"median_run"`
	Pass1Percent float64           `json:"pass1_time_percent"`
}

func getBenchCPUTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err == nil {
		user := time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond
		sys := time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
		return user + sys
	}
	return 0
}

func createBenchFile(path string, size int64) {
	if fi, err := os.Stat(path); err == nil && fi.Size() == size {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	chunk := make([]byte, 1024*1024)
	for i := range chunk {
		chunk[i] = byte((i * 37) & 0xff)
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

func getMedian(runs []BenchRunMetrics) BenchRunMetrics {
	if len(runs) == 0 {
		return BenchRunMetrics{}
	}
	sorted := make([]BenchRunMetrics, len(runs))
	copy(sorted, runs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].TotalTimeMs < sorted[j].TotalTimeMs
	})
	return sorted[len(sorted)/2]
}

func TestTransferRegressionBenchmarks(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "flitnix-bench-reg-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	dataDir := filepath.Join(tempDir, "data")
	_ = os.MkdirAll(dataDir, 0o755)

	sessMgr := session.NewManager(session.Config{})
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
	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatal(err)
	}
	token := sess.Token()
	client := &http.Client{Timeout: 300 * time.Second}

	var results []BenchScenarioResult

	// 1. One 100 MB file
	f100Path := filepath.Join(dataDir, "file_100mb.bin")
	s100Size := int64(100 * 1024 * 1024)
	createBenchFile(f100Path, s100Size)
	scen1 := executeScenario(client, serverURL, token, reg, "Scenario 1: One 100 MB File", "Single 100 MB binary file download via mobile grant path", []string{f100Path}, false, 3)
	results = append(results, scen1)

	// 2. One 1 GB file
	f1GBPath := filepath.Join(dataDir, "file_1gb.bin")
	s1GBSize := int64(1024 * 1024 * 1024)
	createBenchFile(f1GBPath, s1GBSize)
	scen2 := executeScenario(client, serverURL, token, reg, "Scenario 2: One 1 GB File", "Single 1 GB binary file download via mobile grant path", []string{f1GBPath}, false, 3)
	results = append(results, scen2)

	// 3. One 2 GB file (Multi-Gigabyte)
	f2GBPath := filepath.Join(dataDir, "file_2gb.bin")
	s2GBSize := int64(2 * 1024 * 1024 * 1024)
	createBenchFile(f2GBPath, s2GBSize)
	scen3 := executeScenario(client, serverURL, token, reg, "Scenario 3: One 2 GB File (Multi-Gigabyte)", "Single 2 GB binary file download via mobile grant path", []string{f2GBPath}, false, 3)
	results = append(results, scen3)

	// 4. Batch of multiple files (5 x 200 MB = 1 GB)
	var batchPaths []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(dataDir, fmt.Sprintf("batch_part_%d.bin", i))
		createBenchFile(p, 200*1024*1024)
		batchPaths = append(batchPaths, p)
	}
	scen4 := executeScenario(client, serverURL, token, reg, "Scenario 4: Batch of Multiple Files (5 x 200 MB)", "5 x 200 MB files as uncompressed ZIP download via mobile grant path", batchPaths, true, 3)
	results = append(results, scen4)

	// 5. Batch of many small files (1,000 files x 20 KB = 20 MB)
	smallDir := filepath.Join(dataDir, "many_small")
	_ = os.MkdirAll(smallDir, 0o755)
	for i := 0; i < 1000; i++ {
		p := filepath.Join(smallDir, fmt.Sprintf("small_%04d.dat", i))
		createBenchFile(p, 20*1024)
	}
	scen5 := executeFolderScenario(client, serverURL, token, reg, "Scenario 5: Batch of Many Small Files (1,000 files x 20 KB)", "1,000 small files as uncompressed ZIP download via mobile grant path", smallDir, 1000, 3)
	results = append(results, scen5)

	// Comparison: Direct endpoint GET /api/files/{id}/download for 1 GB
	scen2Direct := executeDirectFileScenario(client, serverURL, token, reg, "Scenario 2 (Direct): 1 GB via Direct Endpoint", "Single 1 GB binary file download via GET /api/files/{id}/download", f1GBPath, 3)
	results = append(results, scen2Direct)

	// Print Summary Table
	fmt.Println("\n========================================================================================================================")
	fmt.Println(" FLITNIX PC-TO-PHONE TRANSFER BENCHMARK SUMMARY (Median of 3 Runs per Scenario)")
	fmt.Println("========================================================================================================================")
	fmt.Printf("%-38s | %10s | %10s | %10s | %12s | %12s | %10s\n",
		"Scenario", "Size", "Pass 1 (ms)", "Stream (ms)", "Stream MB/s", "Eff. MB/s", "Pass 1 %")
	fmt.Println("------------------------------------------------------------------------------------------------------------------------")
	for _, res := range results {
		m := res.MedianRun
		fmt.Printf("%-38s | %7.1f MB | %10.1f | %10.1f | %10.2f MB/s | %10.2f MB/s | %9.1f%%\n",
			res.Name, float64(res.TotalBytes)/(1024*1024), m.GrantTimeMs+m.TTFBMs, m.StreamTimeMs, m.StreamMBs, m.EffectiveMBs, res.Pass1Percent)
	}
	fmt.Println("========================================================================================================================")

	outJSON, _ := json.MarshalIndent(results, "", "  ")
	_ = os.WriteFile("regression_benchmark_results.json", outJSON, 0o644)
}

func executeScenario(client *http.Client, serverURL, token string, reg *transfer.Registry, name, desc string, paths []string, asArchive bool, runs int) BenchScenarioResult {
	var metrics []BenchRunMetrics
	var totalBytes int64
	for _, p := range paths {
		fi, _ := os.Stat(p)
		totalBytes += fi.Size()
	}

	for r := 0; r < runs; r++ {
		runtime.GC()
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)
		cpuBefore := getBenchCPUTime()

		t0 := time.Now()
		var sourceIDs []string
		for _, p := range paths {
			source, err := reg.AddFileSource(p, filepath.Base(p), "")
			if err != nil {
				panic(err)
			}
			sourceIDs = append(sourceIDs, source.ID)
		}
		selectDur := time.Since(t0)

		t1 := time.Now()
		grantReqBody, _ := json.Marshal(map[string]any{"sourceIds": sourceIDs})
		req, _ := http.NewRequest("POST", serverURL+"/api/download-grants", bytes.NewReader(grantReqBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}
		grantDur := time.Since(t1)

		var grantRes struct {
			DownloadURL string `json:"downloadUrl"`
			SHA256      string `json:"sha256"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&grantRes)
		cookies := resp.Cookies()
		resp.Body.Close()

		t2 := time.Now()
		getReq, _ := http.NewRequest("GET", serverURL+grantRes.DownloadURL, nil)
		for _, c := range cookies {
			getReq.AddCookie(c)
		}
		getResp, err := client.Do(getReq)
		if err != nil {
			panic(err)
		}
		ttfbDur := time.Since(t2)

		t3 := time.Now()
		hasher := sha256.New()
		buf := make([]byte, 64*1024)
		nRead, err := io.CopyBuffer(hasher, getResp.Body, buf)
		getResp.Body.Close()
		if err != nil {
			panic(err)
		}
		streamDur := time.Since(t3)
		totalDur := time.Since(t0)
		cpuAfter := getBenchCPUTime()
		runtime.ReadMemStats(&mAfter)

		actualSHA := hex.EncodeToString(hasher.Sum(nil))
		integrityPass := (grantRes.SHA256 == "" || grantRes.SHA256 == actualSHA)

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

		metrics = append(metrics, BenchRunMetrics{
			SelectTimeMs:    float64(selectDur.Microseconds()) / 1000.0,
			GrantTimeMs:     float64(grantDur.Microseconds()) / 1000.0,
			TTFBMs:          float64(ttfbDur.Microseconds()) / 1000.0,
			StreamTimeMs:    float64(streamDur.Microseconds()) / 1000.0,
			TotalTimeMs:     float64(totalDur.Microseconds()) / 1000.0,
			BytesRead:       nRead,
			StreamMBs:       float64(nRead) / streamSec / 1000000.0,
			EffectiveMBs:    float64(nRead) / totalSec / 1000000.0,
			PeakHeapAllocMB: float64(mAfter.HeapAlloc) / (1024 * 1024),
			CPUTimeMs:       float64((cpuAfter - cpuBefore).Microseconds()) / 1000.0,
			IntegrityPass:   integrityPass,
		})
	}

	median := getMedian(metrics)
	pass1Pct := (median.GrantTimeMs / median.TotalTimeMs) * 100.0

	return BenchScenarioResult{
		Name:         name,
		Description:  desc,
		FileCount:    len(paths),
		TotalBytes:   totalBytes,
		Runs:         metrics,
		MedianRun:    median,
		Pass1Percent: pass1Pct,
	}
}

func executeFolderScenario(client *http.Client, serverURL, token string, reg *transfer.Registry, name, desc, folder string, fileCount int, runs int) BenchScenarioResult {
	var metrics []BenchRunMetrics
	var totalBytes int64

	for r := 0; r < runs; r++ {
		runtime.GC()
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)
		cpuBefore := getBenchCPUTime()

		t0 := time.Now()
		source, err := reg.AddFolder(folder, "")
		if err != nil {
			panic(err)
		}
		selectDur := time.Since(t0)

		t1 := time.Now()
		grantReqBody, _ := json.Marshal(map[string]any{"sourceIds": []string{source.ID}})
		req, _ := http.NewRequest("POST", serverURL+"/api/download-grants", bytes.NewReader(grantReqBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}
		grantDur := time.Since(t1)

		var grantRes struct {
			DownloadURL string `json:"downloadUrl"`
			SHA256      string `json:"sha256"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&grantRes)
		cookies := resp.Cookies()
		resp.Body.Close()

		t2 := time.Now()
		getReq, _ := http.NewRequest("GET", serverURL+grantRes.DownloadURL, nil)
		for _, c := range cookies {
			getReq.AddCookie(c)
		}
		getResp, err := client.Do(getReq)
		if err != nil {
			panic(err)
		}
		ttfbDur := time.Since(t2)

		t3 := time.Now()
		hasher := sha256.New()
		buf := make([]byte, 64*1024)
		nRead, err := io.CopyBuffer(hasher, getResp.Body, buf)
		getResp.Body.Close()
		if err != nil {
			panic(err)
		}
		streamDur := time.Since(t3)
		totalDur := time.Since(t0)
		cpuAfter := getBenchCPUTime()
		runtime.ReadMemStats(&mAfter)

		actualSHA := hex.EncodeToString(hasher.Sum(nil))
		integrityPass := (grantRes.SHA256 == "" || grantRes.SHA256 == actualSHA)
		reg.Remove(source.ID)

		streamSec := streamDur.Seconds()
		if streamSec <= 0 {
			streamSec = 0.0001
		}
		totalSec := totalDur.Seconds()
		if totalSec <= 0 {
			totalSec = 0.0001
		}
		totalBytes = nRead

		metrics = append(metrics, BenchRunMetrics{
			SelectTimeMs:    float64(selectDur.Microseconds()) / 1000.0,
			GrantTimeMs:     float64(grantDur.Microseconds()) / 1000.0,
			TTFBMs:          float64(ttfbDur.Microseconds()) / 1000.0,
			StreamTimeMs:    float64(streamDur.Microseconds()) / 1000.0,
			TotalTimeMs:     float64(totalDur.Microseconds()) / 1000.0,
			BytesRead:       nRead,
			StreamMBs:       float64(nRead) / streamSec / 1000000.0,
			EffectiveMBs:    float64(nRead) / totalSec / 1000000.0,
			PeakHeapAllocMB: float64(mAfter.HeapAlloc) / (1024 * 1024),
			CPUTimeMs:       float64((cpuAfter - cpuBefore).Microseconds()) / 1000.0,
			IntegrityPass:   integrityPass,
		})
	}

	median := getMedian(metrics)
	pass1Pct := (median.GrantTimeMs / median.TotalTimeMs) * 100.0

	return BenchScenarioResult{
		Name:         name,
		Description:  desc,
		FileCount:    fileCount,
		TotalBytes:   totalBytes,
		Runs:         metrics,
		MedianRun:    median,
		Pass1Percent: pass1Pct,
	}
}

func executeDirectFileScenario(client *http.Client, serverURL, token string, reg *transfer.Registry, name, desc, path string, runs int) BenchScenarioResult {
	var metrics []BenchRunMetrics
	fi, _ := os.Stat(path)
	totalBytes := fi.Size()

	for r := 0; r < runs; r++ {
		runtime.GC()
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)
		cpuBefore := getBenchCPUTime()

		t0 := time.Now()
		sf, err := reg.Add(path, filepath.Base(path), "")
		if err != nil {
			panic(err)
		}
		selectDur := time.Since(t0)

		t1 := time.Now()
		req, _ := http.NewRequest("GET", serverURL+"/api/files/"+sf.ID+"/download", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}
		ttfbDur := time.Since(t1)

		t2 := time.Now()
		hasher := sha256.New()
		buf := make([]byte, 64*1024)
		nRead, err := io.CopyBuffer(hasher, resp.Body, buf)
		resp.Body.Close()
		if err != nil {
			panic(err)
		}
		streamDur := time.Since(t2)
		totalDur := time.Since(t0)
		cpuAfter := getBenchCPUTime()
		runtime.ReadMemStats(&mAfter)

		actualSHA := hex.EncodeToString(hasher.Sum(nil))
		headerSHA := resp.Header.Get("X-Content-SHA256")
		integrityPass := (headerSHA == actualSHA)
		reg.Remove(sf.ID)

		streamSec := streamDur.Seconds()
		if streamSec <= 0 {
			streamSec = 0.0001
		}
		totalSec := totalDur.Seconds()
		if totalSec <= 0 {
			totalSec = 0.0001
		}

		metrics = append(metrics, BenchRunMetrics{
			SelectTimeMs:    float64(selectDur.Microseconds()) / 1000.0,
			GrantTimeMs:     0,
			TTFBMs:          float64(ttfbDur.Microseconds()) / 1000.0,
			StreamTimeMs:    float64(streamDur.Microseconds()) / 1000.0,
			TotalTimeMs:     float64(totalDur.Microseconds()) / 1000.0,
			BytesRead:       nRead,
			StreamMBs:       float64(nRead) / streamSec / 1000000.0,
			EffectiveMBs:    float64(nRead) / totalSec / 1000000.0,
			PeakHeapAllocMB: float64(mAfter.HeapAlloc) / (1024 * 1024),
			CPUTimeMs:       float64((cpuAfter - cpuBefore).Microseconds()) / 1000.0,
			IntegrityPass:   integrityPass,
		})
	}

	median := getMedian(metrics)
	pass1Pct := (median.TTFBMs / median.TotalTimeMs) * 100.0

	return BenchScenarioResult{
		Name:         name,
		Description:  desc,
		FileCount:    1,
		TotalBytes:   totalBytes,
		Runs:         metrics,
		MedianRun:    median,
		Pass1Percent: pass1Pct,
	}
}

func TestBenchmarkBufferSizes(t *testing.T) {
	tempDir := t.TempDir()
	testPath := filepath.Join(tempDir, "bench_500mb.bin")
	size := int64(500 * 1024 * 1024)
	createBenchFile(testPath, size)

	bufferSizes := []int{
		64 * 1024,
		128 * 1024,
		256 * 1024,
		512 * 1024,
	}

	fmt.Println("\n==================================================================")
	fmt.Println(" Buffer Size & Streaming Microbenchmark (500 MB with SHA-256)")
	fmt.Println("==================================================================")

	for _, bufSize := range bufferSizes {
		var durations []time.Duration
		for run := 0; run < 3; run++ {
			f, err := os.Open(testPath)
			if err != nil {
				t.Fatal(err)
			}
			hasher := sha256.New()
			devNull := io.Discard
			tee := io.MultiWriter(devNull, hasher)
			buf := make([]byte, bufSize)

			start := time.Now()
			n, err := io.CopyBuffer(tee, f, buf)
			dur := time.Since(start)
			f.Close()
			if err != nil || n != size {
				t.Fatalf("copy failed: %v", err)
			}
			durations = append(durations, dur)
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		median := durations[len(durations)/2]
		mbs := float64(size) / median.Seconds() / 1000000.0
		fmt.Printf("Buffer Size: %4d KiB | Duration: %6.1f ms | Throughput: %8.2f MB/s\n",
			bufSize/1024, float64(median.Microseconds())/1000.0, mbs)
	}
	fmt.Println("==================================================================")
}

func TestBenchmarkDeadlineThrottling(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	totalBytes := int64(500 * 1024 * 1024)
	data := make([]byte, 64*1024)
	stall := 15 * time.Second

	// 1. Unthrottled run
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 128*1024)
		_, _ = io.CopyBuffer(io.Discard, conn, buf)
	}()

	conn1, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	start1 := time.Now()
	var written1 int64
	for written1 < totalBytes {
		_ = conn1.SetWriteDeadline(time.Now().Add(stall))
		n, err := conn1.Write(data)
		if err != nil {
			break
		}
		written1 += int64(n)
	}
	conn1.Close()
	dur1 := time.Since(start1)
	<-done1

	mbs1 := float64(written1) / dur1.Seconds() / 1000000.0
	fmt.Printf("\nUnthrottled SetWriteDeadline (every 64KB write): %6.1f ms | %8.2f MB/s\n",
		float64(dur1.Microseconds())/1000.0, mbs1)

	// 2. Throttled run (extend deadline every 512KB or 250ms)
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 128*1024)
		_, _ = io.CopyBuffer(io.Discard, conn, buf)
	}()

	conn2, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	start2 := time.Now()
	var written2 int64
	var lastExtend time.Time
	var bytesSince int64
	for written2 < totalBytes {
		now := time.Now()
		bytesSince += int64(len(data))
		if lastExtend.IsZero() || bytesSince >= 512*1024 || now.Sub(lastExtend) >= 250*time.Millisecond {
			_ = conn2.SetWriteDeadline(now.Add(stall))
			lastExtend = now
			bytesSince = 0
		}
		n, err := conn2.Write(data)
		if err != nil {
			break
		}
		written2 += int64(n)
	}
	conn2.Close()
	dur2 := time.Since(start2)
	<-done2

	mbs2 := float64(written2) / dur2.Seconds() / 1000000.0
	fmt.Printf("Throttled SetWriteDeadline (every 512KB/250ms):  %6.1f ms | %8.2f MB/s\n\n",
		float64(dur2.Microseconds())/1000.0, mbs2)
}
