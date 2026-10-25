package transfer

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestCoordinatorFIFOAndSessionIsolation(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Now: func() time.Time { return time.Unix(100, 0) }})
	first := createCoordinatorBatch(t, c, "session-a", DirectionDesktopToPhone, "a-1", "a-2")
	second := createCoordinatorBatch(t, c, "session-a", DirectionDesktopToPhone, "b-1")
	other := createCoordinatorBatch(t, c, "session-b", DirectionDesktopToPhone, "other")

	lease := claim(t, c, "session-a")
	if lease.BatchID != first.ID || lease.Sequence != 0 {
		t.Fatalf("first claim = %+v, want first batch item", lease)
	}
	if _, err := c.ClaimNext("session-a"); !errors.Is(err, ErrNoEligibleItem) {
		t.Fatalf("concurrent claim error = %v, want no eligible item", err)
	}
	complete(t, c, "session-a", lease)

	lease = claim(t, c, "session-a")
	if lease.BatchID != first.ID || lease.Sequence != 1 {
		t.Fatalf("second claim = %+v, want second item of first batch", lease)
	}
	complete(t, c, "session-a", lease)
	lease = claim(t, c, "session-a")
	if lease.BatchID != second.ID {
		t.Fatalf("third claim batch = %q, want %q", lease.BatchID, second.ID)
	}

	otherLease := claim(t, c, "session-b")
	if otherLease.BatchID != other.ID {
		t.Fatalf("independent session claim = %q, want %q", otherLease.BatchID, other.ID)
	}
	if _, err := c.GetBatch("session-b", first.ID); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("cross-session access error = %v, want not found", err)
	}
}

func TestCoordinatorApprovalAndRejection(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	incoming := createCoordinatorBatch(t, c, "phone", DirectionPhoneToDesktop, "incoming")
	if incoming.ApprovalState != ApprovalPending || incoming.State != BatchStatePending {
		t.Fatalf("incoming initial state = %s/%s", incoming.ApprovalState, incoming.State)
	}
	if _, err := c.ClaimNext("phone"); !errors.Is(err, ErrNoEligibleItem) {
		t.Fatalf("pending claim error = %v", err)
	}
	approved, err := c.ApproveBatch("phone", incoming.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.State != BatchStateQueued || approved.Items[0].State != ItemStateQueued {
		t.Fatalf("approved state = %s/%s", approved.State, approved.Items[0].State)
	}
	if _, err := c.ApproveBatch("phone", incoming.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("duplicate approval error = %v", err)
	}

	rejected := createCoordinatorBatch(t, c, "phone", DirectionPhoneToDesktop, "rejected")
	if _, err := c.RejectBatch("phone", rejected.ID); err != nil {
		t.Fatal(err)
	}
	view, err := c.GetBatch("phone", rejected.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != BatchStateRejected || view.Items[0].State != ItemStateRejected {
		t.Fatalf("rejected state = %s/%s", view.State, view.Items[0].State)
	}
}

func TestCoordinatorFailurePauseRetryAndStaleLease(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := createCoordinatorBatch(t, c, "session", DirectionDesktopToPhone, "one", "two")
	first := claim(t, c, "session")
	if err := c.UpdateItemProgress("session", first, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FailItem("session", first, errors.New("network lost")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ClaimNext("session"); !errors.Is(err, ErrQueuePaused) {
		t.Fatalf("claim after failure = %v, want queue paused", err)
	}
	failed, err := c.GetBatch("session", batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Items[0].State != ItemStateFailed || failed.Items[0].Transferred != 3 ||
		failed.Items[1].State != ItemStateQueued {
		t.Fatalf("failure snapshot = %+v", failed.Items)
	}
	retried, err := c.RetryItem("session", batch.ID, failed.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Items[0].State != ItemStateQueued || retried.TransferredBytes != 0 {
		t.Fatalf("retry snapshot = %+v", retried)
	}
	secondAttempt := claim(t, c, "session")
	if secondAttempt.ItemID != first.ItemID || secondAttempt.LeaseID == first.LeaseID {
		t.Fatalf("retry lease = %+v, old = %+v", secondAttempt, first)
	}
	if _, err := c.CompleteItem("session", first, "stale"); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("stale completion error = %v", err)
	}
	complete(t, c, "session", secondAttempt)
	third := claim(t, c, "session")
	complete(t, c, "session", third)
	final, err := c.GetBatch("session", batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != BatchStateCompleted {
		t.Fatalf("final batch state = %s", final.State)
	}
}

func TestCoordinatorProgressBoundsAndZeroByteCompletion(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := createCoordinatorBatchWithSizes(t, c, "session", DirectionDesktopToPhone, []string{"zero", "small"}, []int64{0, 4})
	zero := claim(t, c, "session")
	if err := c.UpdateItemProgress("session", zero, 1); !errors.Is(err, ErrProgressInvalid) {
		t.Fatalf("oversized zero-byte update = %v", err)
	}
	complete(t, c, "session", zero)
	small := claim(t, c, "session")
	if err := c.UpdateItemProgress("session", small, 5); !errors.Is(err, ErrProgressInvalid) {
		t.Fatalf("oversized update = %v", err)
	}
	if err := c.UpdateItemProgress("session", small, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateItemProgress("session", small, 1); !errors.Is(err, ErrProgressInvalid) {
		t.Fatalf("regressing update = %v", err)
	}
	complete(t, c, "session", small)
	view, err := c.GetBatch("session", batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.TransferredBytes != 4 || view.DeclaredTotalBytes != 4 {
		t.Fatalf("aggregate progress = %d/%d", view.TransferredBytes, view.DeclaredTotalBytes)
	}
}

func TestCoordinatorCancellationCallbackRunsOutsideLock(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := createCoordinatorBatch(t, c, "session", DirectionDesktopToPhone, "active")
	lease := claim(t, c, "session")
	called := make(chan struct{})
	if err := c.RegisterCancellation("session", lease, func() {
		if _, err := c.GetBatch("session", batch.ID); err != nil {
			t.Errorf("callback could not inspect coordinator: %v", err)
		}
		close(called)
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.CancelItem("session", batch.ID, lease.ItemID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("cancellation callback did not run")
	}
	if _, err := c.ClaimNext("session"); !errors.Is(err, ErrNoEligibleItem) {
		t.Fatalf("claim after canceled item = %v", err)
	}
}

func TestCoordinatorBatchAndSessionCancellation(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := createCoordinatorBatch(t, c, "session", DirectionDesktopToPhone, "one", "two")
	lease := claim(t, c, "session")
	if _, err := c.CancelBatch("session", batch.ID); err != nil {
		t.Fatal(err)
	}
	view, err := c.GetBatch("session", batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != BatchStateCanceled || view.Items[0].State != ItemStateCanceled || view.Items[1].State != ItemStateCanceled {
		t.Fatalf("canceled batch = %+v", view)
	}
	if _, err := c.CompleteItem("session", lease, "late"); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("late completion error = %v", err)
	}

	other := createCoordinatorBatch(t, c, "other", DirectionDesktopToPhone, "other")
	if err := c.CloseSession("other"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ClaimNext("other"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed session claim = %v", err)
	}
	if err := c.CloseSession("other"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetBatch("other", other.ID); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed session access = %v", err)
	}
}

func TestCoordinatorSnapshotsAreIndependent(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := createCoordinatorBatch(t, c, "session", DirectionDesktopToPhone, "file")
	snapshot, err := c.GetBatch("session", batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Items[0].DisplayName = "mutated"
	list, err := c.ListBatches("session")
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Items[0].DisplayName == "mutated" {
		t.Fatal("caller mutation changed coordinator state")
	}
}

func TestCoordinatorConcurrentOperations(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	batch := createCoordinatorBatch(t, c, "session", DirectionDesktopToPhone, "file")
	lease := claim(t, c, "session")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = c.UpdateItemProgress("session", lease, 1)
	}()
	go func() {
		defer wg.Done()
		_ = c.CancelItem("session", batch.ID, lease.ItemID)
	}()
	wg.Wait()
	if _, err := c.CompleteItem("session", lease, "late"); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("concurrent stale completion = %v", err)
	}
}

func createCoordinatorBatch(t *testing.T, c *Coordinator, session string, direction BatchDirection, names ...string) BatchView {
	t.Helper()
	sizes := make([]int64, len(names))
	for i := range sizes {
		sizes[i] = 4
	}
	return createCoordinatorBatchWithSizes(t, c, session, direction, names, sizes)
}

func createCoordinatorBatchWithSizes(t *testing.T, c *Coordinator, session string, direction BatchDirection, names []string, sizes []int64) BatchView {
	t.Helper()
	items := make([]ManifestItem, len(names))
	for i, name := range names {
		items[i] = ManifestItem{DisplayName: name, Size: sizes[i]}
	}
	view, err := c.CreateBatch(session, BatchManifest{
		Direction: direction, Kind: BatchKindFiles, Items: items,
	})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func claim(t *testing.T, c *Coordinator, session string) TransferLease {
	t.Helper()
	lease, err := c.ClaimNext(session)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func complete(t *testing.T, c *Coordinator, session string, lease TransferLease) {
	t.Helper()
	if lease.DeclaredSize > 0 {
		if err := c.UpdateItemProgress(session, lease, lease.DeclaredSize); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.CompleteItem(session, lease, "checksum"); err != nil {
		t.Fatal(err)
	}
}
