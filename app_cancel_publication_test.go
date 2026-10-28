package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestLateCancelKeepsDesktopHistoryHonest reproduces the reported race from the
// desktop's side. The item's bytes are already published while the coordinator
// still holds the lease (the window between publication and the stream's
// acknowledgement), and the phone withdraws the item in exactly that window.
// The desktop must keep showing a completed item, and the final history, the
// upload record and the receive folder must all agree on the publication.
func TestLateCancelKeepsDesktopHistoryHonest(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("a.txt", "b.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	key := h.sess.Key()
	contentID := h.contentID(b, 0)

	// Publish the first item the way the stream handler does, but stop before
	// its acknowledgement: item leasing, then content, exactly as the HTTP
	// handlers order them.
	lease, err := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatalf("ClaimItem: %v", err)
	}
	if err := h.a.coordinator.UpdateItemProgress(key, lease, 4); err != nil {
		t.Fatalf("UpdateItemProgress: %v", err)
	}
	published, err := h.a.transfers.SubmitContent(contentID, key, bytes.NewReader([]byte("aaaa")), nil)
	if err != nil {
		t.Fatalf("SubmitContent: %v", err)
	}

	// The withdrawal arrives after the file exists.
	if rec := h.cancelItem(b.ID, b.Items[0].ID); rec.Code != http.StatusConflict {
		t.Fatalf("cancel of a published item = %d %s, want 409", rec.Code, rec.Body.String())
	}

	view := h.desktopBatch(b.ID)
	if got := view.Items[0].State; got == string(transfer.ItemStateCanceled) {
		t.Fatalf("desktop history shows a published item as canceled: %+v", view.Items[0])
	}
	if view.State == string(transfer.BatchStateCanceled) || view.State == string(transfer.BatchStateFailed) {
		t.Fatalf("desktop batch state = %q after a refused cancel", view.State)
	}
	if pending := pendingBatchIDs(h.a); len(pending) != 0 {
		t.Fatalf("published batch reappeared in the pending inbox: %v", pending)
	}
	data, err := os.ReadFile(published.Path)
	if err != nil {
		t.Fatalf("read the published file: %v", err)
	}
	if string(data) != "aaaa" {
		t.Fatalf("published bytes = %q, want %q", data, "aaaa")
	}

	// The stream's acknowledgement closes the window and finishes the item.
	if _, err := h.a.coordinator.CompleteItem(key, lease, published.SHA256); err != nil {
		t.Fatalf("CompleteItem after the refused cancel: %v", err)
	}
	view = h.desktopBatch(b.ID)
	if got := view.Items[0].State; got != string(transfer.ItemStateCompleted) {
		t.Fatalf("desktop item 0 = %q, want completed", got)
	}

	// The sibling still uploads, and the batch closes as completed.
	if rec := h.put(b.ID, b.Items[1].ID, []byte("bbbb")); rec.Code != http.StatusOK {
		t.Fatalf("upload item 1 = %d %s", rec.Code, rec.Body.String())
	}
	final := h.desktopBatch(b.ID)
	if final.State != string(transfer.BatchStateCompleted) {
		t.Fatalf("desktop batch state = %q, want completed", final.State)
	}
	for i, item := range final.Items {
		if item.State != string(transfer.ItemStateCompleted) {
			t.Fatalf("desktop item %d = %q, want completed", i, item.State)
		}
	}
	for _, item := range final.Items {
		id, err := h.a.coordinator.ItemContentID(key, b.ID, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		up, ok := h.a.transfers.Get(id)
		if !ok || up.State != transfer.StateCompleted {
			t.Fatalf("record for %s = %+v, want completed", item.DisplayName, up)
		}
		if _, err := os.Stat(up.Published); err != nil {
			t.Fatalf("published file for %s: %v", item.DisplayName, err)
		}
	}
	if names, err := os.ReadDir(filepath.Clean(h.a.store.Dir())); err != nil {
		t.Fatal(err)
	} else if len(names) != 2 {
		t.Fatalf("receive directory has %d entries, want the 2 published files", len(names))
	}
	// A second withdrawal of the same item stays refused, so a retried request
	// never converges on a canceled history.
	if rec := h.cancelItem(b.ID, b.Items[0].ID); rec.Code != http.StatusConflict {
		t.Fatalf("retried cancel of a published item = %d %s", rec.Code, rec.Body.String())
	}
}

// desktopBatch returns the desktop projection of one batch.
func (h *batchHarness) desktopBatch(id string) BatchView {
	h.t.Helper()
	for _, v := range h.a.ListBatches() {
		if v.ID == id {
			return v
		}
	}
	h.t.Fatalf("batch %s missing from the desktop projection", id)
	return BatchView{}
}
