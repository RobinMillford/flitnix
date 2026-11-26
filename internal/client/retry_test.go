package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestBackoffDelayScheduleAndJitterBounds(t *testing.T) {
	base := time.Second
	maxDelay := 15 * time.Second
	expected := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		15 * time.Second,
		15 * time.Second,
	}
	for i, wantMid := range expected {
		low := BackoffDelay(i+1, base, maxDelay, func() float64 { return 0.0 })
		high := BackoffDelay(i+1, base, maxDelay, func() float64 { return 1.0 })
		minExpected := time.Duration(float64(wantMid) * 0.79)
		maxExpected := time.Duration(float64(wantMid) * 1.21)
		if low < minExpected || high > maxExpected || low >= high {
			t.Fatalf("attempt %d: jitter range [%v, %v] outside expected [%v, %v]", i+1, low, high, minExpected, maxExpected)
		}
	}
}

func TestIsRetryableErrorClassification(t *testing.T) {
	permanent := []error{
		context.Canceled,
		ErrUnauthorized,
		ErrSessionExpired,
		ErrRejected,
		ErrBatchNotFound,
		ErrConflict,
		ErrIntegrityMismatch,
		ErrSizeMismatch,
		ErrInsufficientStorage,
		&HTTPStatusError{StatusCode: http.StatusInsufficientStorage, Code: "insufficient_storage"},
		&HTTPStatusError{StatusCode: http.StatusConflict, Code: "paused"},
		&HTTPStatusError{StatusCode: http.StatusConflict, Code: "discarded"},
		&HTTPStatusError{StatusCode: http.StatusConflict, Code: "quarantined"},
		&HTTPStatusError{StatusCode: http.StatusBadRequest, Code: "digest_mismatch"},
	}
	for _, err := range permanent {
		if IsRetryableError(err) {
			t.Fatalf("expected permanent error %v not to be retryable", err)
		}
	}

	transient := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		&HTTPStatusError{StatusCode: http.StatusBadRequest, Code: "chunk_interrupted"},
		&HTTPStatusError{StatusCode: http.StatusConflict, Code: "offset_mismatch"},
		&HTTPStatusError{StatusCode: http.StatusRequestTimeout, Code: "stall"},
		&HTTPStatusError{StatusCode: http.StatusServiceUnavailable},
		&HTTPStatusError{StatusCode: http.StatusBadGateway},
	}
	for _, err := range transient {
		if !IsRetryableError(err) {
			t.Fatalf("expected transient error %v to be retryable", err)
		}
	}
}

func TestStreamItemAutomaticChunkRetryResumesFromServerOffset(t *testing.T) {
	const tok = "valid-auto-retry-token-123456789012345678901"
	payload := make([]byte, 512*1024+123)
	for i := range payload {
		payload[i] = byte(i*17 + 5)
	}
	wantSum := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(wantSum[:])

	var mu sync.Mutex
	var committed []byte
	var patchCalls int
	var pingCalls int
	const chunkSize = int64(128 * 1024)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			mu.Lock()
			pingCalls++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodHead:
			mu.Lock()
			off := len(committed)
			mu.Unlock()
			w.Header().Set("Upload-Offset", strconv.Itoa(off))
			w.Header().Set("Upload-Length", strconv.Itoa(len(payload)))
			w.Header().Set("Upload-Chunk-Size", strconv.FormatInt(chunkSize, 10))
			w.WriteHeader(http.StatusOK)
		case http.MethodPatch:
			off, _ := strconv.Atoi(r.Header.Get("Upload-Offset"))
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			patchCalls++
			// Inject transient interruption on 2nd PATCH and 503 on 4th PATCH!
			if patchCalls == 2 || patchCalls == 4 {
				w.Header().Set("Upload-Offset", strconv.Itoa(len(committed)))
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code":            "chunk_interrupted",
					"error":           "simulated wifi blip",
					"committedOffset": len(committed),
				})
				return
			}
			if off != len(committed) {
				w.Header().Set("Upload-Offset", strconv.Itoa(len(committed)))
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code":           "offset_mismatch",
					"expectedOffset": len(committed),
				})
				return
			}
			committed = append(committed, body...)
			if len(committed) < len(payload) {
				w.Header().Set("Upload-Offset", strconv.Itoa(len(committed)))
				w.WriteHeader(http.StatusNoContent)
				return
			}
			sum := sha256.Sum256(committed)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(struct {
				Written int64              `json:"written"`
				SHA256  string             `json:"sha256"`
				Batch   transfer.BatchView `json:"batch"`
			}{
				Written: int64(len(committed)),
				SHA256:  hex.EncodeToString(sum[:]),
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer ts.Close()

	srcPath := filepath.Join(t.TempDir(), "roaming.bin")
	if err := os.WriteFile(srcPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	target, err := ParseTarget(ts.URL+"/#"+tok, "")
	if err != nil {
		t.Fatal(err)
	}
	cli := NewClient(target)
	var retryEvents []string
	cli.SetRetryPolicy(RetryPolicy{
		MaxRetries: 4,
		BaseDelay:  5 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
		OnRetry: func(attempt, max int, delay time.Duration, err error) {
			mu.Lock()
			retryEvents = append(retryEvents, fmt.Sprintf("%d/%d", attempt, max))
			mu.Unlock()
		},
	})

	res, err := cli.StreamItem(context.Background(), "b1", "i1", srcPath, nil)
	if err != nil {
		t.Fatalf("StreamItem failed despite auto-retry: %v", err)
	}
	if !res.Verified || res.SHA256 != wantHex || !bytes.Equal(committed, payload) {
		t.Fatalf("verified=%v sha=%s (want %s) len=%d", res.Verified, res.SHA256, wantHex, len(committed))
	}
	if len(retryEvents) != 2 || pingCalls < 2 {
		t.Fatalf("expected 2 retry events and ping probes, got events=%v pingCalls=%d", retryEvents, pingCalls)
	}
}

func TestStreamItemStopsImmediatelyOnPermanent507Error(t *testing.T) {
	const tok = "valid-perm-error-token-12345678901234567890"
	var patches int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Upload-Offset", "0")
			w.Header().Set("Upload-Length", "1024")
			w.Header().Set("Upload-Chunk-Size", "512")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodPatch {
			patches++
			w.WriteHeader(http.StatusInsufficientStorage)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code":  "insufficient_storage",
				"error": "disk full",
			})
			return
		}
	}))
	defer ts.Close()

	src := filepath.Join(t.TempDir(), "full.bin")
	_ = os.WriteFile(src, make([]byte, 1024), 0o600)
	target, _ := ParseTarget(ts.URL+"/#"+tok, "")
	cli := NewClient(target)
	cli.SetRetryPolicy(RetryPolicy{MaxRetries: 5, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond})

	_, err := cli.StreamItem(context.Background(), "b1", "i1", src, nil)
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected ErrInsufficientStorage, got %v", err)
	}
	if patches != 1 {
		t.Fatalf("expected permanent 507 NOT to retry, got %d PATCH attempts", patches)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		raw  string
		want time.Duration
		ok   bool
	}{
		{"delta seconds", "10", 10 * time.Second, true},
		{"delta zero", "0", 0, true},
		{"negative rejected", "-5", 0, false},
		{"malformed", "abc", 0, false},
		{"empty", "", 0, false},
		{"http date future", now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{"http date past", now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
		{"invalid date", "Wed, 99 Nov 2099 00:00:00 GMT", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRetryAfter(tc.raw, now)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (value %q)", ok, tc.ok, tc.raw)
			}
			if ok && (got <= tc.want-2*time.Second || got > tc.want+2*time.Second) {
				t.Fatalf("delay = %v, want ~%v (value %q)", got, tc.want, tc.raw)
			}
		})
	}
	// Extremely large delta values clamp to the maximum automatic wait.
	if d, ok := ParseRetryAfter("99999999999999999999", now); !ok || d != MaxAutoRetryWait {
		t.Fatalf("huge delta = %v/%v, want %v/true", d, ok, MaxAutoRetryWait)
	}
}

func TestRetryDelayHonorsServerRetryAfter(t *testing.T) {
	policy := DefaultRetryPolicy()
	cause := &HTTPStatusError{StatusCode: http.StatusServiceUnavailable, RetryAfter: 3 * time.Second}
	if d := RetryDelay(cause, 1, policy, func() float64 { return 1.0 }); d < 3*time.Second {
		t.Fatalf("RetryDelay = %v, must be >= server-requested 3s", d)
	}
	if _, ok := RetryAfterOf(cause); !ok {
		t.Fatal("RetryAfterOf should extract the server request")
	}
	if d, ok := RetryAfterOf(&HTTPStatusError{StatusCode: http.StatusBadGateway}); ok || d != 0 {
		t.Fatalf("RetryAfterOf without header = %v/%v, want 0/false", d, ok)
	}
}

func TestRetryDelayOrDeferredCap(t *testing.T) {
	cli := &Client{}
	// Within the cap: normal delay, no error.
	if d, err := cli.retryDelayOrDeferred(&HTTPStatusError{StatusCode: 503, RetryAfter: time.Second}, 1, DefaultRetryPolicy()); err != nil || d <= 0 {
		t.Fatalf("short Retry-After = %v/%v, want positive delay/nil error", d, err)
	}
	// Beyond the cap: deferred retry — an error is returned and callers must
	// not retry earlier than the server requested.
	_, err := cli.retryDelayOrDeferred(&HTTPStatusError{StatusCode: 503, RetryAfter: MaxAutoRetryWait + time.Minute}, 1, DefaultRetryPolicy())
	if err == nil {
		t.Fatal("expected deferred retry error beyond MaxAutoRetryWait")
	}
	if !strings.Contains(err.Error(), "server requested retry after") {
		t.Fatalf("deferred error should report the requested delay, got: %v", err)
	}
}

func TestRetryableStatusClassificationIncludes429And503(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		if !IsRetryableError(&HTTPStatusError{StatusCode: code}) {
			t.Fatalf("status %d should be retryable", code)
		}
	}
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInsufficientStorage} {
		if IsRetryableError(&HTTPStatusError{StatusCode: code}) {
			t.Fatalf("status %d must not be retryable", code)
		}
	}
}

// TestStreamItemRetryAfterIntegration verifies the retry loop honors a 429
// Retry-After header: OnRetry reports a delay >= the requested value, and the
// upload resumes and completes after the wait.
func TestStreamItemRetryAfterIntegration(t *testing.T) {
	const tok = "valid-retry-after-token-1234567890123456"
	payload := make([]byte, 64*1024)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	wantSum := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(wantSum[:])

	var mu sync.Mutex
	var committed []byte
	var patchCalls int

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodHead:
			mu.Lock()
			off := len(committed)
			mu.Unlock()
			w.Header().Set("Upload-Offset", strconv.Itoa(off))
			w.WriteHeader(http.StatusOK)
		case http.MethodPatch:
			mu.Lock()
			defer mu.Unlock()
			patchCalls++
			if patchCalls == 1 {
				// First PATCH: ask the client to wait at least 2 seconds.
				w.Header().Set("Upload-Offset", strconv.Itoa(len(committed)))
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			committed = append(committed, body...)
			sum := sha256.Sum256(committed)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(struct {
				Written int64  `json:"written"`
				SHA256  string `json:"sha256"`
			}{int64(len(committed)), hex.EncodeToString(sum[:])})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer ts.Close()

	srcPath := filepath.Join(t.TempDir(), "ra.bin")
	if err := os.WriteFile(srcPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := ParseTarget(ts.URL+"/#"+tok, "")
	if err != nil {
		t.Fatal(err)
	}
	cli := NewClient(target)

	var retryDelays []time.Duration
	var retryErrs []error
	cli.SetRetryPolicy(RetryPolicy{
		MaxRetries: 3,
		BaseDelay:  time.Millisecond,
		MaxDelay:   10 * time.Millisecond,
		OnRetry: func(_, _ int, delay time.Duration, err error) {
			mu.Lock()
			defer mu.Unlock()
			retryDelays = append(retryDelays, delay)
			retryErrs = append(retryErrs, err)
		},
	})

	start := time.Now()
	res, err := cli.StreamItem(context.Background(), "b1", "i1", srcPath, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("StreamItem failed despite Retry-After retry: %v", err)
	}
	if !res.Verified || res.SHA256 != wantHex {
		t.Fatalf("verified=%v sha=%s want=%s", res.Verified, res.SHA256, wantHex)
	}
	if len(retryDelays) != 1 || retryDelays[0] < 2*time.Second {
		t.Fatalf("expected one retry with delay >= 2s, got delays=%v", retryDelays)
	}
	if len(retryErrs) != 1 {
		t.Fatalf("expected one retry error recorded, got %d", len(retryErrs))
	}
	if elapsed < 2*time.Second {
		t.Fatalf("client retried before the server-requested delay; elapsed=%v delays=%v", elapsed, retryDelays)
	}
}
