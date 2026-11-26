package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestDeclareUploadRejectsNegativeLastModifiedMs verifies that the standalone
// direct-declare route rejects an explicitly negative lastModifiedMs with
// 400 before any upload record is created.
func TestDeclareUploadRejectsNegativeLastModifiedMs(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"bad.txt","size":4,"lastModifiedMs":-10}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative lastModifiedMs = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := len(env.trans.List()); got != 0 {
		t.Fatalf("negative declare created %d upload records, want 0", got)
	}
}

// TestDeclareUploadRejectsExcessiveFutureLastModifiedMs verifies future
// timestamps beyond the skew window are rejected before any upload exists.
func TestDeclareUploadRejectsExcessiveFutureLastModifiedMs(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	future := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	body := fmt.Sprintf(`{"name":"future.txt","size":4,"lastModifiedMs":%d}`, future)
	rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("excessive future lastModifiedMs = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := len(env.trans.List()); got != 0 {
		t.Fatalf("future declare created %d upload records, want 0", got)
	}
}

// TestDeclareUploadMissingLastModifiedMsCompat verifies older clients that
// omit lastModifiedMs remain valid.
func TestDeclareUploadMissingLastModifiedMsCompat(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	rec := env.do(http.MethodPost, "/api/uploads", tok, []byte(`{"name":"plain.txt","size":4}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("declare without lastModifiedMs = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestBatchDeclareRejectsNegativeLastModifiedMs verifies the batch
// direct-declare path rejects negative timestamps consistently with the
// standalone path and leaves no batch behind.
func TestBatchDeclareRejectsNegativeLastModifiedMs(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	body, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "neg.txt", Size: 4, LastModifiedMs: -10},
		},
	})
	rec := env.do(http.MethodPost, "/api/batches", tok, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("batch with negative lastModifiedMs = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if batches, err := env.coord.ListBatches(env.tokenKey(tok)); err == nil && len(batches) != 0 {
		t.Fatalf("negative batch declare created %d batches, want 0", len(batches))
	}
}

// TestBatchDeclareRejectsExcessiveFutureLastModifiedMs verifies the batch path
// rejects future timestamps beyond the skew window.
func TestBatchDeclareRejectsExcessiveFutureLastModifiedMs(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	future := time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	body, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "future.txt", Size: 4, LastModifiedMs: future},
		},
	})
	rec := env.do(http.MethodPost, "/api/batches", tok, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("batch with future lastModifiedMs = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestBatchDeclareAppliesValidLastModifiedMs verifies a valid old timestamp is
// accepted and surfaced on the created batch view.
func TestBatchDeclareAppliesValidLastModifiedMs(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	origMs := time.Date(2019, 3, 14, 9, 26, 0, 0, time.UTC).UnixMilli()
	body, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "dated.txt", Size: 4, LastModifiedMs: origMs},
		},
	})
	rec := env.do(http.MethodPost, "/api/batches", tok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("batch with valid lastModifiedMs = %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var view transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 1 || view.Items[0].LastModifiedMs != origMs {
		t.Fatalf("batch item LastModifiedMs = %+v, want %d", view.Items, origMs)
	}
}
