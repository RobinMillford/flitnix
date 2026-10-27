package transfer

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// approveCoordinatorBatch creates a phone-to-desktop batch and approves it so
// its items enter the session queue.
func approveCoordinatorBatch(t *testing.T, c *Coordinator, session string, names ...string) BatchView {
	t.Helper()
	batch := createCoordinatorBatch(t, c, session, DirectionPhoneToDesktop, names...)
	if _, err := c.ApproveBatch(session, batch.ID); err != nil {
		t.Fatalf("approve batch: %v", err)
	}
	return batch
}

// TestCancelQueuedItemLetsRemainingItemsFinish reproduces the reported defect:
// canceling one queued item must not leave the batch stuck in "queued" once the
// remaining items are completed. A partially completed batch finishes as
// "canceled" (an intentional item cancellation, never a failure) and keeps the
// completed items and their byte counts.
func TestCancelQueuedItemLetsRemainingItemsFinish(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Now: func() time.Time { return time.Unix(100, 0) }})
	batch := approveCoordinatorBatch(t, c, "phone", "a.txt", "b.txt", "c.txt")

	first := claim(t, c, "phone")
	complete(t, c, "phone", first)

	if err := c.CancelItem("phone", batch.ID, batch.Items[1].ID); err != nil {
		t.Fatalf("cancel queued item: %v", err)
	}

	last := claim(t, c, "phone")
	if last.ItemID != batch.Items[2].ID {
		t.Fatalf("claimed item = %q, want the item after the canceled one", last.ItemID)
	}
	if err := c.UpdateItemProgress("phone", last, last.DeclaredSize); err != nil {
		t.Fatalf("report progress: %v", err)
	}
	view, err := c.CompleteItem("phone", last, "checksum")
	if err != nil {
		t.Fatalf("complete remaining item: %v", err)
	}

	if view.State != BatchStateCanceled {
		t.Fatalf("batch state = %q after canceling one item and completing the rest, want %q", view.State, BatchStateCanceled)
	}
	if view.CompletedAt.IsZero() {
		t.Fatal("terminal batch has no CompletedAt")
	}
	if view.Items[0].State != ItemStateCompleted || view.Items[1].State != ItemStateCanceled || view.Items[2].State != ItemStateCompleted {
		t.Fatalf("item states = %q/%q/%q, want completed/canceled/completed",
			view.Items[0].State, view.Items[1].State, view.Items[2].State)
	}
	if view.TransferredBytes != 8 {
		t.Fatalf("transferred bytes = %d, want the two completed items only", view.TransferredBytes)
	}
	if _, err := c.PeekNext("phone"); !errors.Is(err, ErrNoEligibleItem) {
		t.Fatalf("peek after terminal batch = %v, want no eligible item", err)
	}
}

// TestCancelActiveItemIsIdempotent pins the cancel transition rules.
func TestCancelActiveItemIsIdempotent(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := approveCoordinatorBatch(t, c, "phone", "a.txt", "b.txt")
	lease := claim(t, c, "phone")

	if err := c.CancelItem("phone", batch.ID, lease.ItemID); err != nil {
		t.Fatalf("cancel active item: %v", err)
	}
	if err := c.UpdateItemProgress("phone", lease, 2); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("progress after cancel = %v, want lease invalid", err)
	}
	// A repeated cancel of an already canceled item is a no-op.
	if err := c.CancelItem("phone", batch.ID, lease.ItemID); err != nil {
		t.Fatalf("repeated cancel = %v, want nil", err)
	}

	next := claim(t, c, "phone")
	if err := c.UpdateItemProgress("phone", next, next.DeclaredSize); err != nil {
		t.Fatalf("report progress: %v", err)
	}
	view, err := c.CompleteItem("phone", next, "checksum")
	if err != nil {
		t.Fatalf("complete remaining item: %v", err)
	}
	if view.State != BatchStateCanceled {
		t.Fatalf("batch state = %q, want %q", view.State, BatchStateCanceled)
	}
}

// TestCancellationThatLosesTheClaimRaceKeepsTheItemCanceled pins the exact
// interleaving the HTTP handler relies on: the item was already claimed, then
// the phone canceled it before the handler registered its cancellation
// callback. RegisterCancellation must fail with an invalid lease, the item
// must stay canceled, and the rest of the batch must remain uploadable.
func TestCancellationThatLosesTheClaimRaceKeepsTheItemCanceled(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := approveCoordinatorBatch(t, c, "phone", "a.txt", "b.txt")

	lease, err := c.ClaimItem("phone", batch.ID, batch.Items[0].ID)
	if err != nil {
		t.Fatalf("claim item: %v", err)
	}
	if err := c.CancelItem("phone", batch.ID, lease.ItemID); err != nil {
		t.Fatalf("cancel claimed item: %v", err)
	}
	called := false
	if err := c.RegisterCancellation("phone", lease, func() { called = true }); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("register after cancel = %v, want lease invalid", err)
	}
	if called {
		t.Fatal("a cancellation callback registered after the cancel was invoked")
	}
	view, err := c.GetBatch("phone", batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Items[0].State != ItemStateCanceled {
		t.Fatalf("canceled item resurrected as %q", view.Items[0].State)
	}
	if err := c.UpdateItemProgress("phone", lease, 1); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("progress for the canceled lease = %v, want lease invalid", err)
	}

	next := claim(t, c, "phone")
	if next.ItemID != batch.Items[1].ID {
		t.Fatalf("next claimed item = %q, want the surviving sibling", next.ItemID)
	}
	complete(t, c, "phone", next)
	view, _ = c.GetBatch("phone", batch.ID)
	if view.State != BatchStateCanceled {
		t.Fatalf("batch state = %q, want %q", view.State, BatchStateCanceled)
	}
}

// TestConcurrentCancelAndClaimLeaveOneCoherentOutcome runs both interleavings
// under the real mutex. Whichever side wins, the item ends canceled, no lease
// stays active, and the sibling remains claimable.
func TestConcurrentCancelAndClaimLeaveOneCoherentOutcome(t *testing.T) {
	for round := 0; round < 50; round++ {
		c := NewCoordinator(CoordinatorConfig{})
		batch := approveCoordinatorBatch(t, c, "phone", "a.txt", "b.txt")

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = c.ClaimItem("phone", batch.ID, batch.Items[0].ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = c.CancelItem("phone", batch.ID, batch.Items[0].ID)
		}()
		close(start)
		wg.Wait()

		view, err := c.GetBatch("phone", batch.ID)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if view.Items[0].State != ItemStateCanceled {
			t.Fatalf("round %d: canceled item = %q", round, view.Items[0].State)
		}
		if view.State == BatchStateCompleted || view.State == BatchStateFailed {
			t.Fatalf("round %d: batch state = %q after one cancel", round, view.State)
		}
		next, err := c.ClaimItem("phone", batch.ID, batch.Items[1].ID)
		if err != nil {
			t.Fatalf("round %d: surviving sibling not claimable: %v", round, err)
		}
		complete(t, c, "phone", next)
		view, _ = c.GetBatch("phone", batch.ID)
		if view.State != BatchStateCanceled || view.TransferredBytes != 4 {
			t.Fatalf("round %d: final view = %+v", round, view)
		}
	}
}

// TestCancelItemRejectsCompletedAndForeignItems keeps decisions scoped.
func TestCancelItemRejectsCompletedAndForeignItems(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := approveCoordinatorBatch(t, c, "phone", "a.txt", "b.txt")
	other := approveCoordinatorBatch(t, c, "other", "x.txt")

	lease := claim(t, c, "phone")
	complete(t, c, "phone", lease)
	if err := c.CancelItem("phone", batch.ID, lease.ItemID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancel completed item = %v, want invalid transition", err)
	}
	if err := c.CancelItem("phone", other.ID, other.Items[0].ID); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("cancel foreign batch item = %v, want batch not found", err)
	}
	if err := c.CancelItem("phone", batch.ID, "missing"); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("cancel missing item = %v, want item not found", err)
	}
}
