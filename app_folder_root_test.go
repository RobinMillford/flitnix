package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// createFolderBatch declares a folder batch of 4-byte files under root and
// resolves each item's upload record, exactly like the phone's folder upload.
func (h *batchHarness) createFolderBatch(root string, rel ...string) transfer.BatchView {
	h.t.Helper()
	items := make([]transfer.ManifestItem, len(rel))
	for i, p := range rel {
		items[i] = transfer.ManifestItem{RelativePath: p, Size: 4}
	}
	body, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop, Kind: transfer.BatchKindFolder,
		RootName: root, Items: items,
	})
	rec := h.do(http.MethodPost, "/api/batches", body)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create folder batch = %d %s", rec.Code, rec.Body.String())
	}
	var v transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		h.t.Fatal(err)
	}
	for _, item := range v.Items {
		id, err := h.a.coordinator.ItemContentID(h.sess.Key(), v.ID, item.ID)
		if err != nil {
			h.t.Fatal(err)
		}
		h.ids[v.ID] = append(h.ids[v.ID], id)
	}
	return v
}

// rootPath is the receive directory the batch reserved for itself.
func rootPath(t *testing.T, h *batchHarness, v transfer.BatchView) string {
	t.Helper()
	if v.Kind != transfer.BatchKindFolder || v.RootName == "" {
		t.Fatalf("batch %s is not a folder batch (kind=%q root=%q)", v.ID, v.Kind, v.RootName)
	}
	return filepath.Join(h.a.store.Dir(), v.RootName)
}

// TestAbandonedFolderBatchRootIsRemoved pins the cleanup contract for the
// directory a folder batch reserves before the desktop decides: reserving it
// early is what makes the root collision-safe, but a batch that never
// publishes into it must not leave an empty directory named after the sender's
// folder in the user's receive directory for the rest of the installation's
// life. Batch-creation failure already discards the root; rejection,
// cancellation and session end must too.
func TestAbandonedFolderBatchRootIsRemoved(t *testing.T) {
	cases := []struct {
		name string
		end  func(t *testing.T, h *batchHarness, v transfer.BatchView)
	}{
		{"rejected", func(t *testing.T, h *batchHarness, v transfer.BatchView) {
			if _, err := h.a.RejectBatch(v.ID); err != nil {
				t.Fatalf("RejectBatch: %v", err)
			}
		}},
		{"canceled", func(t *testing.T, h *batchHarness, v transfer.BatchView) {
			if err := h.a.CancelBatch(v.ID); err != nil {
				t.Fatalf("CancelBatch: %v", err)
			}
		}},
		{"session ended", func(t *testing.T, h *batchHarness, v transfer.BatchView) {
			if err := h.a.EndSession(h.sess.Key()); err != nil {
				t.Fatalf("EndSession: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newBatchHarness(t)
			v := h.createFolderBatch("Docs", "a.txt", "nested/b.txt")
			root := rootPath(t, h, v)
			if info, err := os.Stat(root); err != nil || !info.IsDir() {
				t.Fatalf("folder root was not reserved: info=%v err=%v", info, err)
			}
			tc.end(t, h, v)
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("abandoned folder root %q still exists after %s (stat err=%v)", v.RootName, tc.name, err)
			}
		})
	}
}

// TestCanceledFolderBatchKeepsItsPublishedFiles proves the cleanup is
// conservative: a root that already received a published file must survive,
// because the file the user approved the batch for is inside it.
func TestCanceledFolderBatchKeepsItsPublishedFiles(t *testing.T) {
	h := newBatchHarness(t)
	v := h.createFolderBatch("Keep", "a.txt", "b.txt")
	root := rootPath(t, h, v)
	if _, err := h.a.ApproveBatch(v.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	if rec := h.put(v.ID, v.Items[0].ID, []byte("data")); rec.Code != http.StatusOK {
		t.Fatalf("first item PUT = %d %s", rec.Code, rec.Body.String())
	}
	if err := h.a.CancelBatch(v.ID); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	published := filepath.Join(root, "a.txt")
	if got, err := os.ReadFile(published); err != nil {
		t.Fatalf("published file vanished with the canceled batch: %v", err)
	} else if string(got) != "data" {
		t.Fatalf("published content = %q, want %q", got, "data")
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("the canceled item b.txt was published (stat err=%v)", err)
	}
}
