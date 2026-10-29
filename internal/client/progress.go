package client

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"math"
	"time"
)

// FormatBytes formats a byte count into binary units (KiB, MiB, GiB).
func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// FormatSpeed formats byte rate per second into readable string.
func FormatSpeed(bytesPerSec float64) string {
	if bytesPerSec <= 0 || math.IsNaN(bytesPerSec) || math.IsInf(bytesPerSec, 0) {
		return "0 B/s"
	}
	return fmt.Sprintf("%s/s", FormatBytes(int64(bytesPerSec)))
}

// FormatETA formats remaining seconds into human-readable duration.
func FormatETA(seconds float64) string {
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return "0s"
	}
	s := int(seconds)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	s = s % 60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

// HashingReader wraps an io.Reader, computes SHA-256 inline while reading,
// and invokes a progress callback on each chunk read.
type HashingReader struct {
	reader     io.Reader
	hasher     hash.Hash
	total      int64
	written    int64
	onProgress func(written, total int64)
}

// NewHashingReader creates a HashingReader.
func NewHashingReader(r io.Reader, total int64, onProgress func(written, total int64)) *HashingReader {
	h := sha256.New()
	return &HashingReader{
		reader:     io.TeeReader(r, h),
		hasher:     h,
		total:      total,
		onProgress: onProgress,
	}
}

func (hr *HashingReader) Read(p []byte) (int, error) {
	n, err := hr.reader.Read(p)
	if n > 0 {
		hr.written += int64(n)
		if hr.onProgress != nil {
			hr.onProgress(hr.written, hr.total)
		}
	}
	return n, err
}

// SHA256 returns the hexadecimal SHA-256 digest of bytes read so far.
func (hr *HashingReader) SHA256() string {
	return hex.EncodeToString(hr.hasher.Sum(nil))
}

// Written returns the total number of bytes read.
func (hr *HashingReader) Written() int64 {
	return hr.written
}

// SpeedTracker tracks moving average transfer rate.
type SpeedTracker struct {
	startTime  time.Time
	lastTime   time.Time
	lastBytes  int64
	currentBps float64
}

// NewSpeedTracker initializes a SpeedTracker.
func NewSpeedTracker() *SpeedTracker {
	now := time.Now()
	return &SpeedTracker{
		startTime: now,
		lastTime:  now,
	}
}

// Update updates the speed estimate with the current cumulative bytes transferred.
func (st *SpeedTracker) Update(transferredBytes int64) float64 {
	now := time.Now()
	elapsed := now.Sub(st.lastTime).Seconds()
	if elapsed >= 0.25 {
		deltaBytes := transferredBytes - st.lastBytes
		st.currentBps = float64(deltaBytes) / elapsed
		st.lastTime = now
		st.lastBytes = transferredBytes
	}
	return st.currentBps
}

// OverallSpeed returns average speed across the whole duration.
func (st *SpeedTracker) OverallSpeed(totalBytes int64) float64 {
	dur := time.Since(st.startTime).Seconds()
	if dur <= 0 {
		return 0
	}
	return float64(totalBytes) / dur
}
