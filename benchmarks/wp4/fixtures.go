//go:build historical_wp4

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type FixtureMetadata struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

func EnsureFixture(dir, name string, size int64) (FixtureMetadata, error) {
	targetPath := filepath.Join(dir, name)
	if fi, err := os.Stat(targetPath); err == nil && fi.Size() == size {
		fmt.Printf("Fixture %s exists (%d bytes), computing/verifying SHA-256...\n", name, size)
		f, err := os.Open(targetPath)
		if err == nil {
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				digest := hex.EncodeToString(h.Sum(nil))
				fmt.Printf("Fixture %s ready: SHA-256 = %s\n", name, digest)
				return FixtureMetadata{
					Name:      name,
					Path:      targetPath,
					SizeBytes: size,
					SHA256:    digest,
				}, nil
			}
		}
	}

	fmt.Printf("Generating deterministic fixture %s (size %d bytes / %.2f GB)...\n", name, size, float64(size)/(1024*1024*1024))
	t0 := time.Now()

	tmpPath := targetPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return FixtureMetadata{}, fmt.Errorf("failed to create fixture file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	mw := io.MultiWriter(f, h)

	// Create 4 MB deterministic non-compressible chunk pattern
	chunkSize := 4 * 1024 * 1024
	chunk := make([]byte, chunkSize)
	for i := range chunk {
		chunk[i] = byte((i*1664525 + 1013904223) & 0xff)
	}

	var written int64
	lastReport := time.Now()
	for written < size {
		toWrite := int64(chunkSize)
		if size-written < toWrite {
			toWrite = size - written
		}
		if _, err := mw.Write(chunk[:toWrite]); err != nil {
			_ = os.Remove(tmpPath)
			return FixtureMetadata{}, fmt.Errorf("failed writing fixture: %w", err)
		}
		written += toWrite

		if time.Since(lastReport) >= 5*time.Second {
			speed := float64(written) / time.Since(t0).Seconds() / 1e6
			fmt.Printf("  -> %s: %d / %d bytes (%.1f%%) at %.1f MB/s\n", name, written, size, float64(written)*100/float64(size), speed)
			lastReport = time.Now()
		}
	}

	if err := f.Sync(); err != nil {
		_ = os.Remove(tmpPath)
		return FixtureMetadata{}, fmt.Errorf("sync failed: %w", err)
	}
	_ = f.Close()

	if err := os.Rename(tmpPath, targetPath); err != nil {
		return FixtureMetadata{}, fmt.Errorf("rename failed: %w", err)
	}

	digest := hex.EncodeToString(h.Sum(nil))
	dur := time.Since(t0)
	fmt.Printf("Generated %s in %v (throughput: %.1f MB/s) | SHA-256: %s\n", name, dur, float64(size)/dur.Seconds()/1e6, digest)

	return FixtureMetadata{
		Name:      name,
		Path:      targetPath,
		SizeBytes: size,
		SHA256:    digest,
	}, nil
}

func EnsureAllFixtures(fixturesDir string) (map[string]FixtureMetadata, error) {
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		return nil, err
	}

	specs := []struct {
		name string
		size int64
	}{
		{"payload_1gb.bin", 1 * 1024 * 1024 * 1024},
		{"payload_5gb.bin", 5 * 1024 * 1024 * 1024},
		{"payload_20gb.bin", 20 * 1024 * 1024 * 1024},
	}

	res := make(map[string]FixtureMetadata)
	for _, s := range specs {
		meta, err := EnsureFixture(fixturesDir, s.name, s.size)
		if err != nil {
			return nil, err
		}
		res[s.name] = meta
	}

	// Persist fixtures.json
	metaJson, _ := json.MarshalIndent(res, "", "  ")
	_ = os.WriteFile(filepath.Join(fixturesDir, "fixtures.json"), metaJson, 0o644)
	return res, nil
}
