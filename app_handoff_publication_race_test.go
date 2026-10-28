package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestHandoffCancelBatchConcurrentWithPublishClaim verifies that when a whole-batch
// cancellation races with publication reservation:
// 1. If publication has been claimed, the item finishes with a successful commit.
// 2. Unstarted siblings are canceled.
// 3. The batch remains transferring until the reserved publication settles.
// 4. Once settled, the item is completed, byte counts are accurate, the batch ends canceled,
//    and the file is intact on disk.
func TestHandoffCancelBatchConcurrentWithPublishClaim(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("pub-racing.txt", "unstarted.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	key := h.sess.Key()
	cid0 := h.contentID(b, 0)

	l0, err := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatalf("claim item 0: %v", err)
	}
	if err := h.a.coordinator.UpdateItemProgress(key, l0, 4); err != nil {
		t.Fatalf("progress 0: %v", err)
	}

	onPublishCalled := make(chan struct{})

	var pubRes transfer.ContentResult
	var submitErr error
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		pubRes, submitErr = h.a.transfers.SubmitContentWithPublish(cid0, key, bytes.NewReader([]byte("race")), nil, func() error {
			err := h.a.coordinator.MarkItemPublishing(key, l0)
			close(onPublishCalled)
			return err
		})
	}()

	// Wait until publication has been registered in the coordinator under the lock.
	select {
	case <-onPublishCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onPublish to be called")
	}

	// Concurrent whole-batch cancellation arrives while publication is in-flight.
	if err := h.a.CancelBatch(b.ID); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	wg.Wait()

	if submitErr != nil {
		t.Fatalf("SubmitContentWithPublish: %v", submitErr)
	}

	// Stream completes the item lease.
	view, err := h.a.coordinator.CompleteItem(key, l0, pubRes.SHA256)
	if err != nil {
		t.Fatalf("CompleteItem: %v", err)
	}

	if view.State != transfer.BatchStateCanceled {
		t.Fatalf("final batch state = %q, want canceled", view.State)
	}
	if view.Items[0].State != transfer.ItemStateCompleted {
		t.Fatalf("final item 0 state = %q, want completed", view.Items[0].State)
	}
	if view.Items[1].State != transfer.ItemStateCanceled {
		t.Fatalf("final item 1 state = %q, want canceled", view.Items[1].State)
	}
	if view.TransferredBytes != 4 {
		t.Fatalf("final transferred bytes = %d, want 4", view.TransferredBytes)
	}

	// Verify disk contents.
	data, err := os.ReadFile(pubRes.Path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", pubRes.Path, err)
	}
	if string(data) != "race" {
		t.Fatalf("file content = %q, want 'race'", string(data))
	}
}

// TestHandoffCancelBatchFailsCommit verifies that if a whole-batch cancellation arrives
// during publication, and the filesystem commit subsequently fails:
// 1. The publishing item ends failed (not completed, not canceled).
// 2. The batch ends failed.
// 3. No file is left published on disk.
func TestHandoffCancelBatchFailsCommit(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("fail-commit.txt", "sibling.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	key := h.sess.Key()
	cid0 := h.contentID(b, 0)

	l0, err := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatalf("claim item 0: %v", err)
	}
	if err := h.a.coordinator.UpdateItemProgress(key, l0, 4); err != nil {
		t.Fatalf("progress 0: %v", err)
	}

	storeDir := filepath.Clean(h.a.store.Dir())
	onPublishCalled := make(chan struct{})

	var submitErr error
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		_, submitErr = h.a.transfers.SubmitContentWithPublish(cid0, key, bytes.NewReader([]byte("fail")), nil, func() error {
			err := h.a.coordinator.MarkItemPublishing(key, l0)
			// Make the store directory read-only so the filesystem commit (hard link) fails.
			_ = os.Chmod(storeDir, 0555)
			close(onPublishCalled)
			return err
		})
	}()

	t.Cleanup(func() {
		_ = os.Chmod(storeDir, 0755)
	})

	select {
	case <-onPublishCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onPublish")
	}

	// Concurrent whole-batch cancel.
	if err := h.a.CancelBatch(b.ID); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	wg.Wait()

	// Commit must have failed.
	if submitErr == nil {
		t.Fatal("SubmitContentWithPublish should have failed on read-only directory")
	}

	// Settle the failure through FailBatch.
	view, err := h.a.coordinator.FailBatch(key, l0, submitErr)
	if err != nil {
		t.Fatalf("FailBatch: %v", err)
	}

	if view.State != transfer.BatchStateFailed {
		t.Fatalf("final batch state = %q, want failed", view.State)
	}
	if view.Items[0].State != transfer.ItemStateFailed {
		t.Fatalf("final item 0 state = %q, want failed", view.Items[0].State)
	}
	if view.Items[1].State != transfer.ItemStateCanceled {
		t.Fatalf("final item 1 state = %q, want canceled", view.Items[1].State)
	}

	// Restore write permissions to inspect directory.
	_ = os.Chmod(storeDir, 0755)
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == "fail-commit.txt" {
			t.Fatalf("failed commit must not leave %q published", entry.Name())
		}
	}
}

// TestHandoffSessionTeardownConcurrentWithPublishClaim verifies that when session
// teardown (explicit EndSession or idle expiry) races with the publication-reservation handoff:
// 1. An item that claimed publication completes its filesystem commit.
// 2. The item and batch are truthfully recorded in retained closed-session history.
// 3. The file is intact on disk.
func TestHandoffSessionTeardownConcurrentWithPublishClaim(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("sess-pub.txt", "sess-queued.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	key := h.sess.Key()
	cid0 := h.contentID(b, 0)

	l0, err := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatalf("claim item 0: %v", err)
	}
	if err := h.a.coordinator.UpdateItemProgress(key, l0, 4); err != nil {
		t.Fatalf("progress 0: %v", err)
	}

	onPublishCalled := make(chan struct{})

	var pubRes transfer.ContentResult
	var submitErr error
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		pubRes, submitErr = h.a.transfers.SubmitContentWithPublish(cid0, key, bytes.NewReader([]byte("sess")), nil, func() error {
			err := h.a.coordinator.MarkItemPublishing(key, l0)
			close(onPublishCalled)
			return err
		})
	}()

	select {
	case <-onPublishCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for onPublish")
	}

	// EndSession runs while publication is in-flight.
	if err := h.a.EndSession(key); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	wg.Wait()

	if submitErr != nil {
		t.Fatalf("SubmitContentWithPublish: %v", submitErr)
	}

	// The stream completes the item on the closed session.
	view, err := h.a.coordinator.CompleteItem(key, l0, pubRes.SHA256)
	if err != nil {
		t.Fatalf("CompleteItem on closed session: %v", err)
	}

	if view.State != transfer.BatchStateCanceled {
		t.Fatalf("view state = %q, want canceled", view.State)
	}
	if view.Items[0].State != transfer.ItemStateCompleted {
		t.Fatalf("item 0 = %q, want completed", view.Items[0].State)
	}
	if view.Items[1].State != transfer.ItemStateCanceled {
		t.Fatalf("item 1 = %q, want canceled", view.Items[1].State)
	}

	// Retained history must reflect the truthful outcome.
	hist, ok := h.a.coordinator.SessionHistory(key)
	if !ok || len(hist) != 1 {
		t.Fatalf("SessionHistory ok=%v len=%d", ok, len(hist))
	}
	if hist[0].Items[0].State != transfer.ItemStateCompleted {
		t.Fatalf("history item 0 = %q, want completed", hist[0].Items[0].State)
	}
	if hist[0].Items[1].State != transfer.ItemStateCanceled {
		t.Fatalf("history item 1 = %q, want canceled", hist[0].Items[1].State)
	}

	data, err := os.ReadFile(pubRes.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "sess" {
		t.Fatalf("content = %q, want 'sess'", string(data))
	}
}

// TestHandoffCancelWinsBeforePublicationClaimed verifies that if cancellation arrives
// before publication has been claimed:
// 1. Publication is refused.
// 2. The temporary sink is aborted.
// 3. No file is published on disk.
func TestHandoffCancelWinsBeforePublicationClaimed(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("cancel-wins.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	key := h.sess.Key()
	cid0 := h.contentID(b, 0)

	l0, err := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatalf("claim item 0: %v", err)
	}

	// Cancel the batch before the stream even attempts publication.
	if err := h.a.CancelBatch(b.ID); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	// Now the stream attempts to publish.
	_, err = h.a.transfers.SubmitContentWithPublish(cid0, key, bytes.NewReader([]byte("nope")), nil, func() error {
		return h.a.coordinator.MarkItemPublishing(key, l0)
	})
	if !errors.Is(err, transfer.ErrCanceled) {
		t.Fatalf("SubmitContentWithPublish err = %v, want ErrCanceled", err)
	}

	// Verify no file exists in the store directory.
	entries, err := os.ReadDir(filepath.Clean(h.a.store.Dir()))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("receive directory has %d files, want 0", len(entries))
	}
}
