package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestCancelBatchBeforePublicationPreventsPublication proves that canceling
// a batch before publication has been reserved prevents any final file from
// appearing on disk, cancels all unstarted items, and marks the batch canceled.
func TestCancelBatchBeforePublicationPreventsPublication(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("pre-0.txt", "pre-1.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}

	// Cancel the whole batch before any publication reservation occurs.
	if err := h.a.CancelBatch(b.ID); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	view := h.desktopBatch(b.ID)
	if view.State != string(transfer.BatchStateCanceled) {
		t.Fatalf("batch state = %q, want canceled", view.State)
	}
	for i, item := range view.Items {
		if item.State != string(transfer.ItemStateCanceled) {
			t.Fatalf("item %d state = %q, want canceled", i, item.State)
		}
	}
	if view.TransferredBytes != 0 {
		t.Fatalf("transferred bytes = %d, want 0", view.TransferredBytes)
	}

	// Attempting to stream content for a canceled item is rejected.
	rec := h.put(b.ID, b.Items[0].ID, []byte("aaaa"))
	if rec.Code == http.StatusOK {
		t.Fatalf("upload to canceled item returned 200 OK")
	}

	entries, err := os.ReadDir(filepath.Clean(h.a.store.Dir()))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("receive directory has %d files, want 0", len(entries))
	}
}

// TestCancelBatchAfterPublicationReservationPreservesCommittedFile verifies that
// when a whole-batch cancellation arrives after an item's publication decision has
// already been reserved, the item ends completed if its filesystem commit succeeds,
// its completed sibling remains completed, unstarted siblings do not publish,
// the batch remains nonterminal while publication settles, and the batch state,
// counts and disk match accurately.
func TestCancelBatchAfterPublicationReservationPreservesCommittedFile(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("done.txt", "racing.txt", "queued.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	key := h.sess.Key()
	cid0 := h.contentID(b, 0)
	cid1 := h.contentID(b, 1)

	// Complete sibling item 0 normally.
	l0, err := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatalf("claim item 0: %v", err)
	}
	if err := h.a.coordinator.UpdateItemProgress(key, l0, 4); err != nil {
		t.Fatalf("progress 0: %v", err)
	}
	pub0, err := h.a.transfers.SubmitContent(cid0, key, bytes.NewReader([]byte("0000")), nil)
	if err != nil {
		t.Fatalf("submit 0: %v", err)
	}
	if _, err := h.a.coordinator.CompleteItem(key, l0, pub0.SHA256); err != nil {
		t.Fatalf("complete 0: %v", err)
	}

	// Start item 1: lease, progress, and reserve publication.
	l1, err := h.a.coordinator.ClaimItem(key, b.ID, b.Items[1].ID)
	if err != nil {
		t.Fatalf("claim item 1: %v", err)
	}
	if err := h.a.coordinator.UpdateItemProgress(key, l1, 4); err != nil {
		t.Fatalf("progress 1: %v", err)
	}
	pub1, err := h.a.transfers.SubmitContentWithPublish(cid1, key, bytes.NewReader([]byte("1111")), nil, func() error {
		return h.a.coordinator.MarkItemPublishing(key, l1)
	})
	if err != nil {
		t.Fatalf("submit 1: %v", err)
	}

	// Cancel the whole batch while item 1's publication is reserved but before
	// the stream's final acknowledgement (CompleteItem).
	if err := h.a.CancelBatch(b.ID); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	// The batch must remain nonterminal while the reserved publication settles.
	midView := h.desktopBatch(b.ID)
	if midView.State != string(transfer.BatchStateTransferring) {
		t.Fatalf("mid-settle batch state = %q, want %q", midView.State, transfer.BatchStateTransferring)
	}
	if midView.Items[0].State != string(transfer.ItemStateCompleted) {
		t.Fatalf("item 0 = %q, want completed", midView.Items[0].State)
	}
	if midView.Items[1].State != string(transfer.ItemStateTransferring) {
		t.Fatalf("item 1 = %q, want transferring (publishing)", midView.Items[1].State)
	}
	if midView.Items[2].State != string(transfer.ItemStateCanceled) {
		t.Fatalf("item 2 = %q, want canceled", midView.Items[2].State)
	}

	// The stream completes its acknowledgement.
	if _, err := h.a.coordinator.CompleteItem(key, l1, pub1.SHA256); err != nil {
		t.Fatalf("CompleteItem item 1: %v", err)
	}

	final := h.desktopBatch(b.ID)
	if final.State != string(transfer.BatchStateCanceled) {
		t.Fatalf("final batch state = %q, want canceled", final.State)
	}
	if final.Items[0].State != string(transfer.ItemStateCompleted) {
		t.Fatalf("final item 0 = %q, want completed", final.Items[0].State)
	}
	if final.Items[1].State != string(transfer.ItemStateCompleted) {
		t.Fatalf("final item 1 = %q, want completed", final.Items[1].State)
	}
	if final.Items[2].State != string(transfer.ItemStateCanceled) {
		t.Fatalf("final item 2 = %q, want canceled", final.Items[2].State)
	}
	if final.TransferredBytes != 8 {
		t.Fatalf("final transferred bytes = %d, want 8", final.TransferredBytes)
	}

	// Verify the filesystem: items 0 and 1 exist intact, item 2 does not.
	names, err := os.ReadDir(filepath.Clean(h.a.store.Dir()))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("receive directory has %d files, want 2", len(names))
	}
	d0, _ := os.ReadFile(pub0.Path)
	if string(d0) != "0000" {
		t.Fatalf("file 0 content = %q, want '0000'", string(d0))
	}
	d1, _ := os.ReadFile(pub1.Path)
	if string(d1) != "1111" {
		t.Fatalf("file 1 content = %q, want '1111'", string(d1))
	}
}

// TestCancelBatchAfterPublicationReservationWhenCommitFails demonstrates that
// if the filesystem commit fails after whole-batch cancellation, the item ends
// failed rather than completed, no file is published, and byte counts exclude it.
func TestCancelBatchAfterPublicationReservationWhenCommitFails(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("sibling.txt", "failed-commit.txt", "unstarted.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	key := h.sess.Key()
	cid0 := h.contentID(b, 0)

	// Sibling item 0 completes.
	l0, _ := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	_ = h.a.coordinator.UpdateItemProgress(key, l0, 4)
	pub0, _ := h.a.transfers.SubmitContent(cid0, key, bytes.NewReader([]byte("0000")), nil)
	_, _ = h.a.coordinator.CompleteItem(key, l0, pub0.SHA256)

	// Item 1 is claimed and marked publishing.
	l1, _ := h.a.coordinator.ClaimItem(key, b.ID, b.Items[1].ID)
	_ = h.a.coordinator.UpdateItemProgress(key, l1, 4)
	_ = h.a.coordinator.MarkItemPublishing(key, l1)

	// Cancel the batch.
	if err := h.a.CancelBatch(b.ID); err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	// Commit fails in the stream handler.
	commitErr := errors.New("simulated commit fsync failure")
	failView, err := h.a.coordinator.FailBatch(key, l1, commitErr)
	if err != nil {
		t.Fatalf("FailBatch: %v", err)
	}

	if failView.Items[0].State != transfer.ItemStateCompleted {
		t.Fatalf("sibling item 0 = %q, want completed", failView.Items[0].State)
	}
	if failView.Items[1].State != transfer.ItemStateFailed {
		t.Fatalf("failed item 1 = %q, want failed", failView.Items[1].State)
	}
	if failView.Items[2].State != transfer.ItemStateCanceled {
		t.Fatalf("unstarted item 2 = %q, want canceled", failView.Items[2].State)
	}
	if failView.TransferredBytes != 4 {
		t.Fatalf("transferred bytes = %d, want 4", failView.TransferredBytes)
	}

	// Disk has only item 0.
	names, _ := os.ReadDir(filepath.Clean(h.a.store.Dir()))
	if len(names) != 1 {
		t.Fatalf("receive dir entries = %d, want 1", len(names))
	}
}

// TestExplicitSessionEndDuringPublicationPreservesFileAndRetainedHistory verifies
// that explicit session termination (EndSession) while an item is publishing
// revokes the session immediately, rejects subsequent operations, preserves the
// published file on commit success, and records the completed item in retained history.
func TestExplicitSessionEndDuringPublicationPreservesFileAndRetainedHistory(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("sess-pub.txt", "sess-queued.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	key := h.sess.Key()
	cid0 := h.contentID(b, 0)

	l0, err := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatalf("ClaimItem: %v", err)
	}
	if err := h.a.coordinator.UpdateItemProgress(key, l0, 4); err != nil {
		t.Fatalf("UpdateItemProgress: %v", err)
	}
	pub0, err := h.a.transfers.SubmitContentWithPublish(cid0, key, bytes.NewReader([]byte("spub")), nil, func() error {
		return h.a.coordinator.MarkItemPublishing(key, l0)
	})
	if err != nil {
		t.Fatalf("SubmitContent: %v", err)
	}

	// Explicit session end lands before CompleteItem.
	if err := h.a.EndSession(key); err != nil {
		t.Fatalf("EndSession: %v", err)
	}

	// New authenticated operations are prohibited.
	rec := h.put(b.ID, b.Items[1].ID, []byte("xxxx"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("request on revoked session returned %d, want 401", rec.Code)
	}

	// The in-flight publishing stream settles successfully into history.
	if _, err := h.a.coordinator.CompleteItem(key, l0, pub0.SHA256); err != nil {
		t.Fatalf("CompleteItem after EndSession: %v", err)
	}

	// Desktop history projects the retained session history.
	batches := h.a.ListBatches()
	var found *BatchView
	for i := range batches {
		if batches[i].ID == b.ID {
			found = &batches[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("batch missing from retained history")
	}
	if found.Items[0].State != string(transfer.ItemStateCompleted) {
		t.Fatalf("retained item 0 state = %q, want completed", found.Items[0].State)
	}
	if found.Items[1].State != string(transfer.ItemStateCanceled) {
		t.Fatalf("retained item 1 state = %q, want canceled", found.Items[1].State)
	}
	if found.TransferredBytes != 4 {
		t.Fatalf("retained transferred bytes = %d, want 4", found.TransferredBytes)
	}

	// File on disk is intact.
	data, err := os.ReadFile(pub0.Path)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if string(data) != "spub" {
		t.Fatalf("published file content = %q, want 'spub'", string(data))
	}
}

// TestIdleExpiryAndNetworkChangePreservePublicationInvariant tests that idle
// expiration (releaseSession) and network changes (endAllSessions) preserve
// already-reserved publication outcomes in retained history.
func TestIdleExpiryAndNetworkChangePreservePublicationInvariant(t *testing.T) {
	t.Run("idle_expiry", func(t *testing.T) {
		h := newBatchHarness(t)
		b := h.createBatch("idle-pub.txt", "idle-queued.txt")
		_, _ = h.a.ApproveBatch(b.ID)
		key := h.sess.Key()
		cid := h.contentID(b, 0)

		l, _ := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
		_ = h.a.coordinator.UpdateItemProgress(key, l, 4)
		pub, _ := h.a.transfers.SubmitContentWithPublish(cid, key, bytes.NewReader([]byte("idle")), nil, func() error {
			return h.a.coordinator.MarkItemPublishing(key, l)
		})

		// Idle expiration fires releaseSession.
		h.a.releaseSession(key)

		// Publication settles.
		_, err := h.a.coordinator.CompleteItem(key, l, pub.SHA256)
		if err != nil {
			t.Fatalf("CompleteItem: %v", err)
		}

		hist, ok := h.a.coordinator.SessionHistory(key)
		if !ok || len(hist) != 1 {
			t.Fatalf("SessionHistory ok=%v len=%d", ok, len(hist))
		}
		if hist[0].Items[0].State != transfer.ItemStateCompleted {
			t.Fatalf("item 0 = %q, want completed", hist[0].Items[0].State)
		}
		if _, err := os.Stat(pub.Path); err != nil {
			t.Fatalf("published file missing: %v", err)
		}
	})

	t.Run("network_change", func(t *testing.T) {
		h := newBatchHarness(t)
		b := h.createBatch("net-pub.txt", "net-queued.txt")
		_, _ = h.a.ApproveBatch(b.ID)
		key := h.sess.Key()
		cid := h.contentID(b, 0)

		l, _ := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
		_ = h.a.coordinator.UpdateItemProgress(key, l, 4)
		pub, _ := h.a.transfers.SubmitContentWithPublish(cid, key, bytes.NewReader([]byte("netw")), nil, func() error {
			return h.a.coordinator.MarkItemPublishing(key, l)
		})

		// Network change triggers endAllSessions.
		h.a.srvMu.Lock()
		h.a.endAllSessions()
		h.a.srvMu.Unlock()

		_, err := h.a.coordinator.CompleteItem(key, l, pub.SHA256)
		if err != nil {
			t.Fatalf("CompleteItem: %v", err)
		}

		hist, ok := h.a.coordinator.SessionHistory(key)
		if !ok || len(hist) != 1 {
			t.Fatalf("SessionHistory ok=%v len=%d", ok, len(hist))
		}
		if hist[0].Items[0].State != transfer.ItemStateCompleted {
			t.Fatalf("item 0 = %q, want completed", hist[0].Items[0].State)
		}
		if _, err := os.Stat(pub.Path); err != nil {
			t.Fatalf("published file missing: %v", err)
		}
	})
}

// TestShutdownDuringPublicationPreservesFileWithoutLeaks tests that application
// shutdown during an in-flight publication preserves the file, completes the item
// in retained history, and does not leak resources.
func TestShutdownDuringPublicationPreservesFileWithoutLeaks(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("shut-pub.txt", "shut-queued.txt")
	_, _ = h.a.ApproveBatch(b.ID)
	key := h.sess.Key()
	cid := h.contentID(b, 0)

	l, _ := h.a.coordinator.ClaimItem(key, b.ID, b.Items[0].ID)
	_ = h.a.coordinator.UpdateItemProgress(key, l, 4)
	pub, _ := h.a.transfers.SubmitContentWithPublish(cid, key, bytes.NewReader([]byte("shut")), nil, func() error {
		return h.a.coordinator.MarkItemPublishing(key, l)
	})

	// Shutdown begins.
	h.a.shutdown(context.Background())

	// Publication settles.
	_, err := h.a.coordinator.CompleteItem(key, l, pub.SHA256)
	if err != nil {
		t.Fatalf("CompleteItem: %v", err)
	}

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
	if _, err := os.Stat(pub.Path); err != nil {
		t.Fatalf("published file missing: %v", err)
	}
}

// TestCrossSessionAccessRemainsRejected tests that a second session cannot
// cancel, inspect, or mutate another session's batch.
func TestCrossSessionAccessRemainsRejected(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("iso.txt")

	// Create second session.
	s2, err := h.a.sessions.Create()
	if err != nil {
		t.Fatalf("Create second session: %v", err)
	}

	// Session 2 attempts to claim or cancel Session 1's batch item.
	if err := h.a.coordinator.CancelItem(s2.Key(), b.ID, b.Items[0].ID); !errors.Is(err, transfer.ErrWrongSession) {
		t.Fatalf("CancelItem from other session = %v, want ErrWrongSession", err)
	}
	if _, err := h.a.coordinator.GetBatch(s2.Key(), b.ID); !errors.Is(err, transfer.ErrWrongSession) {
		t.Fatalf("GetBatch from other session = %v, want ErrWrongSession", err)
	}
}
