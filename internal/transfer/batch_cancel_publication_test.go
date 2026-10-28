package transfer

import (
	"errors"
	"testing"
	"time"
)

// TestCancelBatchDuringPublicationPreservesPublishingItemAndCancelsSiblings tests
// that whole-batch cancellation while an item is actively publishing preserves
// the publishing item, cancels its queued siblings, keeps the batch nonterminal
// until the commit outcome settles, and finalizes the batch with exact byte counts.
func TestCancelBatchDuringPublicationPreservesPublishingItemAndCancelsSiblings(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Now: func() time.Time { return time.Unix(100, 0) }})
	const sess = "sess-1"
	b := createCoordinatorBatch(t, c, sess, DirectionDesktopToPhone, "f0", "f1", "f2")

	// Complete item 0.
	l0 := claim(t, c, sess)
	complete(t, c, sess, l0)

	// Claim item 1 and begin publishing.
	l1 := claim(t, c, sess)
	if err := c.UpdateItemProgress(sess, l1, l1.DeclaredSize); err != nil {
		t.Fatalf("UpdateItemProgress: %v", err)
	}
	if err := c.MarkItemPublishing(sess, l1); err != nil {
		t.Fatalf("MarkItemPublishing: %v", err)
	}

	// Cancel the batch while item 1 is publishing.
	view, err := c.CancelBatch(sess, b.ID)
	if err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	// Batch must not be terminal yet because item 1 is still settling.
	if view.State != BatchStateTransferring {
		t.Fatalf("batch state during publication settle = %q, want %q", view.State, BatchStateTransferring)
	}
	if view.Items[0].State != ItemStateCompleted {
		t.Fatalf("item 0 = %q, want completed", view.Items[0].State)
	}
	if view.Items[1].State != ItemStateTransferring {
		t.Fatalf("item 1 = %q, want transferring (publishing)", view.Items[1].State)
	}
	if view.Items[2].State != ItemStateCanceled {
		t.Fatalf("item 2 = %q, want canceled", view.Items[2].State)
	}

	// Settle publication: commit succeeds.
	finalView, err := c.CompleteItem(sess, l1, "sha-f1")
	if err != nil {
		t.Fatalf("CompleteItem after cancel: %v", err)
	}
	if finalView.State != BatchStateCanceled {
		t.Fatalf("final batch state = %q, want %q", finalView.State, BatchStateCanceled)
	}
	if finalView.Items[0].State != ItemStateCompleted {
		t.Fatalf("final item 0 = %q, want completed", finalView.Items[0].State)
	}
	if finalView.Items[1].State != ItemStateCompleted {
		t.Fatalf("final item 1 = %q, want completed", finalView.Items[1].State)
	}
	if finalView.Items[2].State != ItemStateCanceled {
		t.Fatalf("final item 2 = %q, want canceled", finalView.Items[2].State)
	}
	expectedBytes := l0.DeclaredSize + l1.DeclaredSize
	if finalView.TransferredBytes != expectedBytes {
		t.Fatalf("transferred bytes = %d, want %d", finalView.TransferredBytes, expectedBytes)
	}
}

// TestCancelBatchDuringPublicationWhenCommitFails tests that if the in-flight
// commit fails after whole-batch cancellation, the item ends failed and the
// batch reflects the failure accurately without crediting the failed item.
func TestCancelBatchDuringPublicationWhenCommitFails(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Now: func() time.Time { return time.Unix(100, 0) }})
	const sess = "sess-2"
	b := createCoordinatorBatch(t, c, sess, DirectionDesktopToPhone, "f0", "f1", "f2")

	l0 := claim(t, c, sess)
	complete(t, c, sess, l0)

	l1 := claim(t, c, sess)
	if err := c.UpdateItemProgress(sess, l1, l1.DeclaredSize); err != nil {
		t.Fatalf("UpdateItemProgress: %v", err)
	}
	if err := c.MarkItemPublishing(sess, l1); err != nil {
		t.Fatalf("MarkItemPublishing: %v", err)
	}

	_, err := c.CancelBatch(sess, b.ID)
	if err != nil {
		t.Fatalf("CancelBatch: %v", err)
	}

	// Commit fails.
	commitErr := errors.New("disk full")
	finalView, err := c.FailBatch(sess, l1, commitErr)
	if err != nil {
		t.Fatalf("FailBatch: %v", err)
	}
	if finalView.Items[0].State != ItemStateCompleted {
		t.Fatalf("item 0 = %q, want completed", finalView.Items[0].State)
	}
	if finalView.Items[1].State != ItemStateFailed {
		t.Fatalf("item 1 = %q, want failed", finalView.Items[1].State)
	}
	if finalView.Items[2].State != ItemStateCanceled {
		t.Fatalf("item 2 = %q, want canceled", finalView.Items[2].State)
	}
	if finalView.TransferredBytes != l0.DeclaredSize {
		t.Fatalf("transferred bytes = %d, want %d", finalView.TransferredBytes, l0.DeclaredSize)
	}
}

// TestCloseSessionDuringPublicationPreservesPublishingItemAndAllowsSettling tests
// that CloseSession does not cancel an in-flight publication, leaves the batch
// nonterminal until settling, and lets the stream complete the item into retained history.
func TestCloseSessionDuringPublicationPreservesPublishingItemAndAllowsSettling(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Now: func() time.Time { return time.Unix(100, 0) }})
	const sess = "sess-close-pub"
	_ = createCoordinatorBatch(t, c, sess, DirectionDesktopToPhone, "f0", "f1")

	l0 := claim(t, c, sess)
	if err := c.UpdateItemProgress(sess, l0, l0.DeclaredSize); err != nil {
		t.Fatalf("UpdateItemProgress: %v", err)
	}
	if err := c.MarkItemPublishing(sess, l0); err != nil {
		t.Fatalf("MarkItemPublishing: %v", err)
	}

	if err := c.CloseSession(sess); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	// New operations must be rejected.
	if _, err := c.ClaimNext(sess); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("ClaimNext on closed session = %v, want ErrSessionClosed", err)
	}

	// The in-flight publishing stream settles successfully.
	finalView, err := c.CompleteItem(sess, l0, "sha-f0")
	if err != nil {
		t.Fatalf("CompleteItem after CloseSession: %v", err)
	}
	if finalView.Items[0].State != ItemStateCompleted {
		t.Fatalf("item 0 = %q, want completed", finalView.Items[0].State)
	}
	if finalView.Items[1].State != ItemStateCanceled {
		t.Fatalf("item 1 = %q, want canceled", finalView.Items[1].State)
	}
	if finalView.State != BatchStateCanceled {
		t.Fatalf("final batch state = %q, want canceled", finalView.State)
	}

	// History retains the completed item accurately.
	history, ok := c.SessionHistory(sess)
	if !ok || len(history) != 1 {
		t.Fatalf("SessionHistory ok=%v len=%d", ok, len(history))
	}
	if history[0].Items[0].State != ItemStateCompleted {
		t.Fatalf("history item 0 = %q, want completed", history[0].Items[0].State)
	}
	if history[0].Items[0].SHA256 != "sha-f0" {
		t.Fatalf("history item 0 sha = %q, want sha-f0", history[0].Items[0].SHA256)
	}
	if history[0].TransferredBytes != l0.DeclaredSize {
		t.Fatalf("history bytes = %d, want %d", history[0].TransferredBytes, l0.DeclaredSize)
	}
}

// TestCloseSessionDuringPublicationWhenCommitFails tests that if commit fails
// after CloseSession, the item ends failed in retained history.
func TestCloseSessionDuringPublicationWhenCommitFails(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Now: func() time.Time { return time.Unix(100, 0) }})
	const sess = "sess-close-fail"
	_ = createCoordinatorBatch(t, c, sess, DirectionDesktopToPhone, "f0")

	l0 := claim(t, c, sess)
	if err := c.UpdateItemProgress(sess, l0, l0.DeclaredSize); err != nil {
		t.Fatalf("UpdateItemProgress: %v", err)
	}
	if err := c.MarkItemPublishing(sess, l0); err != nil {
		t.Fatalf("MarkItemPublishing: %v", err)
	}

	if err := c.CloseSession(sess); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	finalView, err := c.FailBatch(sess, l0, errors.New("write failed"))
	if err != nil {
		t.Fatalf("FailBatch after CloseSession: %v", err)
	}
	if finalView.Items[0].State != ItemStateFailed {
		t.Fatalf("item 0 = %q, want failed", finalView.Items[0].State)
	}

	history, ok := c.SessionHistory(sess)
	if !ok || len(history) != 1 {
		t.Fatalf("SessionHistory ok=%v len=%d", ok, len(history))
	}
	if history[0].Items[0].State != ItemStateFailed {
		t.Fatalf("history item 0 = %q, want failed", history[0].Items[0].State)
	}
	if history[0].TransferredBytes != 0 {
		t.Fatalf("history bytes = %d, want 0", history[0].TransferredBytes)
	}
}
