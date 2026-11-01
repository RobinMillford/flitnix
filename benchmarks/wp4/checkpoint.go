package main

import (
	"crypto/sha256"
	"encoding"
	"encoding/csv"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type LatencyStats struct {
	Operation   string        `json:"operation"`
	Iterations  int           `json:"iterations"`
	Min         time.Duration `json:"min"`
	Median      time.Duration `json:"median"`
	P95         time.Duration `json:"p95"`
	P99         time.Duration `json:"p99"`
	Max         time.Duration `json:"max"`
	Avg         time.Duration `json:"avg"`
	FailureRate float64       `json:"failure_rate"`
}

func computeLatencyStats(op string, latencies []time.Duration, failures int) LatencyStats {
	n := len(latencies)
	if n == 0 {
		return LatencyStats{Operation: op}
	}
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	var total time.Duration
	for _, l := range latencies {
		total += l
	}

	p50Idx := int(float64(n) * 0.50)
	p95Idx := int(float64(n) * 0.95)
	p99Idx := int(float64(n) * 0.99)
	if p50Idx >= n {
		p50Idx = n - 1
	}
	if p95Idx >= n {
		p95Idx = n - 1
	}
	if p99Idx >= n {
		p99Idx = n - 1
	}

	totalOps := n + failures
	failRate := 0.0
	if totalOps > 0 {
		failRate = float64(failures) / float64(totalOps) * 100.0
	}

	return LatencyStats{
		Operation:   op,
		Iterations:  n,
		Min:         latencies[0],
		Median:      latencies[p50Idx],
		P95:         latencies[p95Idx],
		P99:         latencies[p99Idx],
		Max:         latencies[n-1],
		Avg:         total / time.Duration(n),
		FailureRate: failRate,
	}
}

func BenchmarkFilesystemCheckpoints(benchDir string) ([]LatencyStats, error) {
	tmpDir := filepath.Join(benchDir, "checkpoint_bench_tmp")
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed creating bench temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	var results []LatencyStats

	// 1. partFile.Sync() latency (with 32 MB write batches)
	fmt.Println("Running benchmark 1/6: partFile.Sync() [32 MB dirty writes]...")
	{
		iterations := 50
		partPath := filepath.Join(tmpDir, "test.part")
		partFile, err := os.OpenFile(partPath, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		defer partFile.Close()

		chunk32MB := make([]byte, 32*1024*1024)
		rand.Read(chunk32MB[:1024]) // seed variance

		var latencies []time.Duration
		failures := 0
		for i := 0; i < iterations; i++ {
			if _, err := partFile.Write(chunk32MB); err != nil {
				failures++
				continue
			}
			t0 := time.Now()
			if err := partFile.Sync(); err != nil {
				failures++
			} else {
				latencies = append(latencies, time.Since(t0))
			}
		}
		partFile.Close()
		_ = os.Remove(partPath)

		stats := computeLatencyStats("partFile.Sync (32MB dirty)", latencies, failures)
		results = append(results, stats)
		fmt.Printf("  -> partFile.Sync: p50=%v, p95=%v, p99=%v, avg=%v (failures: %d)\n", stats.Median, stats.P95, stats.P99, stats.Avg, failures)
	}

	// 2. .meta.tmp file Sync latency (1 KB JSON metadata)
	fmt.Println("Running benchmark 2/6: .meta.tmp Sync() [1 KB metadata]...")
	{
		iterations := 200
		metaPayload := []byte(`{"transferId":"bench-100","totalSize":5368709120,"committedOffset":2147483648,"expectedSHA256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","state":"active","updatedAt":"2026-11-01T10:00:00Z"}`)
		var latencies []time.Duration
		failures := 0

		for i := 0; i < iterations; i++ {
			tmpMetaPath := filepath.Join(tmpDir, fmt.Sprintf("upload_%d.meta.tmp", i))
			f, err := os.OpenFile(tmpMetaPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				failures++
				continue
			}
			_, _ = f.Write(metaPayload)

			t0 := time.Now()
			if err := f.Sync(); err != nil {
				failures++
			} else {
				latencies = append(latencies, time.Since(t0))
			}
			f.Close()
			_ = os.Remove(tmpMetaPath)
		}

		stats := computeLatencyStats(".meta.tmp Sync (1KB)", latencies, failures)
		results = append(results, stats)
		fmt.Printf("  -> .meta.tmp Sync: p50=%v, p95=%v, p99=%v, avg=%v (failures: %d)\n", stats.Median, stats.P95, stats.P99, stats.Avg, failures)
	}

	// 3. Atomic metadata replacement/rename (os.Rename)
	fmt.Println("Running benchmark 3/6: os.Rename(.meta.tmp -> .meta)...")
	{
		iterations := 200
		metaPayload := []byte(`{"committedOffset":1048576}`)
		var latencies []time.Duration
		failures := 0

		for i := 0; i < iterations; i++ {
			tmpPath := filepath.Join(tmpDir, fmt.Sprintf("ren_%d.meta.tmp", i))
			dstPath := filepath.Join(tmpDir, fmt.Sprintf("ren_%d.meta", i))
			_ = os.WriteFile(tmpPath, metaPayload, 0o644)

			t0 := time.Now()
			if err := os.Rename(tmpPath, dstPath); err != nil {
				failures++
			} else {
				latencies = append(latencies, time.Since(t0))
			}
			_ = os.Remove(dstPath)
		}

		stats := computeLatencyStats("Atomic Rename (.meta.tmp -> .meta)", latencies, failures)
		results = append(results, stats)
		fmt.Printf("  -> Atomic Rename: p50=%v, p95=%v, p99=%v, avg=%v (failures: %d)\n", stats.Median, stats.P95, stats.P99, stats.Avg, failures)
	}

	// 4. POSIX parent directory Sync
	fmt.Println("Running benchmark 4/6: Parent Directory Sync()...")
	{
		iterations := 200
		var latencies []time.Duration
		failures := 0

		dirFile, err := os.Open(tmpDir)
		if err != nil {
			return nil, err
		}
		defer dirFile.Close()

		for i := 0; i < iterations; i++ {
			t0 := time.Now()
			if err := dirFile.Sync(); err != nil {
				failures++
			} else {
				latencies = append(latencies, time.Since(t0))
			}
		}

		stats := computeLatencyStats("Parent Directory Sync", latencies, failures)
		results = append(results, stats)
		fmt.Printf("  -> Parent Dir Sync: p50=%v, p95=%v, p99=%v, avg=%v (failures: %d)\n", stats.Median, stats.P95, stats.P99, stats.Avg, failures)
	}

	// 5. SHA-256 binary state serialization
	fmt.Println("Running benchmark 5/6: SHA-256 Binary State Serialization...")
	{
		iterations := 1000
		h := sha256.New()
		sampleData := make([]byte, 1024*1024)
		h.Write(sampleData)

		bm, ok := h.(encoding.BinaryMarshaler)
		if !ok {
			fmt.Println("  (Warning: sha256.New() does not implement BinaryMarshaler on this runtime)")
		} else {
			var latencies []time.Duration
			failures := 0
			for i := 0; i < iterations; i++ {
				t0 := time.Now()
				data, err := bm.MarshalBinary()
				if err != nil || len(data) == 0 {
					failures++
				} else {
					latencies = append(latencies, time.Since(t0))
				}
			}
			stats := computeLatencyStats("SHA-256 State MarshalBinary", latencies, failures)
			results = append(results, stats)
			fmt.Printf("  -> MarshalBinary: p50=%v, p95=%v, p99=%v, avg=%v (failures: %d)\n", stats.Median, stats.P95, stats.P99, stats.Avg, failures)
		}
	}

	// 6. SHA-256 binary state unmarshaling
	fmt.Println("Running benchmark 6/6: SHA-256 Binary State Unmarshaling...")
	{
		iterations := 1000
		hSource := sha256.New()
		hSource.Write(make([]byte, 1024*1024))
		bm := hSource.(encoding.BinaryMarshaler)
		stateBytes, _ := bm.MarshalBinary()

		var latencies []time.Duration
		failures := 0
		for i := 0; i < iterations; i++ {
			hTarget := sha256.New()
			bu := hTarget.(encoding.BinaryUnmarshaler)
			t0 := time.Now()
			if err := bu.UnmarshalBinary(stateBytes); err != nil {
				failures++
			} else {
				latencies = append(latencies, time.Since(t0))
			}
		}
		stats := computeLatencyStats("SHA-256 State UnmarshalBinary", latencies, failures)
		results = append(results, stats)
		fmt.Printf("  -> UnmarshalBinary: p50=%v, p95=%v, p99=%v, avg=%v (failures: %d)\n", stats.Median, stats.P95, stats.P99, stats.Avg, failures)
	}

	return results, nil
}

func SaveCheckpointLatencyCSV(path string, stats []LatencyStats) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	_ = w.Write([]string{
		"Operation",
		"Iterations",
		"MinLatencyUs",
		"MedianLatencyUs",
		"P95LatencyUs",
		"P99LatencyUs",
		"MaxLatencyUs",
		"AvgLatencyUs",
		"FailureRatePercent",
	})

	for _, s := range stats {
		_ = w.Write([]string{
			s.Operation,
			strconv.Itoa(s.Iterations),
			fmt.Sprintf("%.2f", float64(s.Min.Microseconds())),
			fmt.Sprintf("%.2f", float64(s.Median.Microseconds())),
			fmt.Sprintf("%.2f", float64(s.P95.Microseconds())),
			fmt.Sprintf("%.2f", float64(s.P99.Microseconds())),
			fmt.Sprintf("%.2f", float64(s.Max.Microseconds())),
			fmt.Sprintf("%.2f", float64(s.Avg.Microseconds())),
			fmt.Sprintf("%.2f", s.FailureRate),
		})
	}
	return nil
}
