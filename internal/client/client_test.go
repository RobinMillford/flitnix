package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestClientLifecycle(t *testing.T) {
	const validToken = "valid-test-token-12345"

	// Mock server state
	var currentApproval transfer.ApprovalState = transfer.ApprovalPending
	var currentState transfer.BatchState = transfer.BatchStatePending

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ping does not require auth
		if r.URL.Path == "/ping" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		// Auth check
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+validToken {
			if stringsContains(auth, "expired") {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "session expired"})
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/batches":
			var manifest transfer.BatchManifest
			if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(transfer.BatchView{
				ID:            "batch-1",
				Kind:          manifest.Kind,
				ApprovalState: currentApproval,
				State:         currentState,
				Items: []transfer.ItemView{
					{
						ID:           "item-1",
						BatchID:      "batch-1",
						Sequence:     0,
						DisplayName:  manifest.Items[0].DisplayName,
						DeclaredSize: manifest.Items[0].Size,
						State:        transfer.ItemStatePending,
					},
				},
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/batches/batch-1":
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(transfer.BatchView{
				ID:            "batch-1",
				ApprovalState: currentApproval,
				State:         currentState,
			})

		case r.Method == http.MethodPut && r.URL.Path == "/api/batches/batch-1/items/item-1/content":
			hasher := sha256.New()
			written, err := io.Copy(hasher, r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			serverDigest := hex.EncodeToString(hasher.Sum(nil))
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(struct {
				Written int64              `json:"written"`
				SHA256  string             `json:"sha256"`
				Batch   transfer.BatchView `json:"batch"`
			}{
				Written: written,
				SHA256:  serverDigest,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	target, err := ParseTarget(ts.URL+"/#"+validToken, "")
	if err != nil {
		t.Fatalf("ParseTarget failed: %v", err)
	}
	cli := NewClient(target)

	ctx := context.Background()

	t.Run("ping success", func(t *testing.T) {
		if err := cli.Ping(ctx); err != nil {
			t.Fatalf("Ping failed: %v", err)
		}
	})

	t.Run("declare batch", func(t *testing.T) {
		manifest := transfer.BatchManifest{
			Direction: transfer.DirectionPhoneToDesktop,
			Kind:      transfer.BatchKindFiles,
			Items: []transfer.ManifestItem{
				{DisplayName: "test.txt", Size: 12},
			},
		}
		view, err := cli.DeclareBatch(ctx, manifest)
		if err != nil {
			t.Fatalf("DeclareBatch failed: %v", err)
		}
		if view.ID != "batch-1" {
			t.Errorf("Batch ID = %q, want batch-1", view.ID)
		}
	})

	t.Run("wait for approval - approved", func(t *testing.T) {
		currentApproval = transfer.ApprovalApproved
		currentState = transfer.BatchStateApproved

		view, err := cli.WaitForApproval(ctx, "batch-1", 10*time.Millisecond)
		if err != nil {
			t.Fatalf("WaitForApproval failed: %v", err)
		}
		if view.ApprovalState != transfer.ApprovalApproved {
			t.Errorf("ApprovalState = %s, want approved", view.ApprovalState)
		}
	})

	t.Run("stream item and verify SHA-256", func(t *testing.T) {
		tmpFile := filepath.Join(t.TempDir(), "upload.txt")
		content := []byte("hello world streaming test")
		if err := os.WriteFile(tmpFile, content, 0o644); err != nil {
			t.Fatal(err)
		}

		var progressCalls int
		res, err := cli.StreamItem(ctx, "batch-1", "item-1", tmpFile, func(written, total int64) {
			progressCalls++
		})
		if err != nil {
			t.Fatalf("StreamItem failed: %v", err)
		}
		if res.Written != int64(len(content)) {
			t.Errorf("Written = %d, want %d", res.Written, len(content))
		}
		if !res.Verified {
			t.Errorf("Verified = false, want true")
		}
		if progressCalls == 0 {
			t.Errorf("expected progress callbacks, got 0")
		}
	})

	t.Run("unauthorized token", func(t *testing.T) {
		badTarget, _ := ParseTarget(ts.URL+"/#wrong-token", "")
		badCli := NewClient(badTarget)
		_, err := badCli.PollBatch(ctx, "batch-1")
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("expected ErrUnauthorized, got %v", err)
		}
	})

	t.Run("expired session token", func(t *testing.T) {
		expiredTarget, _ := ParseTarget(ts.URL+"/#expired-token", "")
		expiredCli := NewClient(expiredTarget)
		_, err := expiredCli.PollBatch(ctx, "batch-1")
		if !errors.Is(err, ErrSessionExpired) {
			t.Errorf("expected ErrSessionExpired, got %v", err)
		}
	})

	t.Run("approval rejected", func(t *testing.T) {
		currentApproval = transfer.ApprovalRejected
		currentState = transfer.BatchStateRejected

		_, err := cli.WaitForApproval(ctx, "batch-1", 10*time.Millisecond)
		if !errors.Is(err, ErrRejected) {
			t.Errorf("expected ErrRejected, got %v", err)
		}
	})

	t.Run("approval timeout", func(t *testing.T) {
		currentApproval = transfer.ApprovalPending
		currentState = transfer.BatchStatePending

		timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()

		_, err := cli.WaitForApproval(timeoutCtx, "batch-1", 10*time.Millisecond)
		if !errors.Is(err, ErrApprovalTimeout) {
			t.Errorf("expected ErrApprovalTimeout, got %v", err)
		}
	})
}

func stringsContains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestReadHiddenInput_NonTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer r.Close()

	testInput := "  https://192.168.1.5:43821/#secret-token  \n"
	go func() {
		_, _ = w.Write([]byte(testInput))
		_ = w.Close()
	}()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("out Pipe: %v", err)
	}
	defer outR.Close()
	defer outW.Close()

	got, err := ReadHiddenInput("Enter URL: ", r, outW)
	if err != nil {
		t.Fatalf("ReadHiddenInput: %v", err)
	}
	want := "https://192.168.1.5:43821/#secret-token"
	if got != want {
		t.Errorf("ReadHiddenInput = %q, want %q", got, want)
	}
}

func TestReadHiddenInput_EOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	_ = w.Close()
	defer r.Close()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("out Pipe: %v", err)
	}
	defer outR.Close()
	defer outW.Close()

	_, err = ReadHiddenInput("", r, outW)
	if err == nil {
		t.Fatalf("expected error on empty EOF stdin, got nil")
	}
}

func TestStreamItemWithHintSkipsHeadAndThrottlesProgress(t *testing.T) {
	payload := make([]byte, 2<<20) // 2 MiB
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	tmpPath := filepath.Join(t.TempDir(), "stream.bin")
	if err := os.WriteFile(tmpPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	var headCalls atomic.Int32
	var patchCalls atomic.Int32
	var putCalls atomic.Int32
	var allowPatch atomic.Bool
	allowPatch.Store(true)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		case http.MethodPatch:
			patchCalls.Add(1)
			if !allowPatch.Load() {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("X-Flitnix-Compact-Ack") != "1" {
				t.Errorf("missing X-Flitnix-Compact-Ack header on PATCH")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"written": len(body),
				"sha256":  hex.EncodeToString(sum[:]),
			})
		case http.MethodPut:
			putCalls.Add(1)
			if r.Header.Get("X-Flitnix-Compact-Ack") != "1" {
				t.Errorf("missing X-Flitnix-Compact-Ack header on PUT")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"written": len(body),
				"sha256":  hex.EncodeToString(sum[:]),
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer ts.Close()

	target, err := ParseTarget(ts.URL+"/#tok", "")
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	cli := NewClient(target)

	var progressCount int
	var lastSent, lastTotal int64
	res, err := cli.StreamItemWithHint(context.Background(), "b1", "i1", tmpPath, 0, 16<<20, func(sent, total int64) {
		progressCount++
		lastSent = sent
		lastTotal = total
	})
	if err != nil {
		t.Fatalf("StreamItemWithHint PATCH failed: %v", err)
	}
	if !res.Verified || res.Written != int64(len(payload)) {
		t.Fatalf("unexpected PATCH result: %+v", res)
	}
	if headCalls.Load() != 0 {
		t.Fatalf("expected 0 HEAD requests when initialOffset=0 and chunkSizeHint>0, got %d", headCalls.Load())
	}
	if patchCalls.Load() != 1 {
		t.Fatalf("expected 1 PATCH request, got %d", patchCalls.Load())
	}
	if lastSent != int64(len(payload)) || lastTotal != int64(len(payload)) {
		t.Fatalf("final progress = (%d,%d), want (%d,%d)", lastSent, lastTotal, len(payload), len(payload))
	}
	// At 256 KiB throttle threshold, 2 MiB produces ~8 callbacks (not 64+ at 32 KiB reads).
	if progressCount < 2 || progressCount > 24 {
		t.Fatalf("expected throttled progressCount in [2..24], got %d", progressCount)
	}

	// Verify automatic fallback to PUT when PATCH returns 405 Method Not Allowed.
	allowPatch.Store(false)
	var putProgCount int
	res2, err := cli.StreamItemWithHint(context.Background(), "b1", "i2", tmpPath, 0, 16<<20, func(sent, total int64) {
		putProgCount++
	})
	if err != nil {
		t.Fatalf("StreamItemWithHint PUT fallback failed: %v", err)
	}
	if !res2.Verified || putCalls.Load() != 1 || putProgCount > 24 {
		t.Fatalf("unexpected PUT fallback stats: verified=%v putCalls=%d putProgCount=%d", res2.Verified, putCalls.Load(), putProgCount)
	}
}
