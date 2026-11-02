package transfer

import (
	"errors"
	"testing"
)

func testManifest(size int64) BatchManifest {
	return BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFiles,
		Items:     []ManifestItem{{DisplayName: "durable.bin", Size: size}},
	}
}

// TestCoordinatorPersistFailureIsNotAcknowledged proves a coordinator
// operation whose required lifecycle state did not reach the journal is never
// reported as successful, and that the failure latches the subsystem instead
// of allowing later transitions to be acknowledged.
func TestCoordinatorPersistFailureIsNotAcknowledged(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	c.SetPersistHook(func(LifecycleEvent) error { return errors.New("journal write failed") })

	if _, err := c.CreateBatch("session-key", testManifest(4)); !errors.Is(err, ErrDurableState) {
		t.Fatalf("CreateBatch with a failing journal = %v, want ErrDurableState", err)
	}
	if err := c.DurableError(); !errors.Is(err, ErrDurableState) {
		t.Fatalf("DurableError = %v, want ErrDurableState", err)
	}

	// Even with the journal healthy again, the latched coordinator refuses to
	// acknowledge new transitions: a restart from durable state is required.
	c.SetPersistHook(func(LifecycleEvent) error { return nil })
	if _, err := c.CreateBatch("session-key", testManifest(4)); !errors.Is(err, ErrDurableState) {
		t.Fatalf("CreateBatch after a latched failure = %v, want ErrDurableState", err)
	}

	// Reads must keep working so the desktop can still project state.
	if _, err := c.ListBatches("session-key"); err != nil {
		t.Fatalf("read after durable failure = %v, want success", err)
	}
}

// TestCoordinatorPersistsEveryLifecycleTransition checks the covered
// transitions and proves byte progress is not a durable transition.
func TestCoordinatorPersistsEveryLifecycleTransition(t *testing.T) {
	var events []LifecycleEvent
	c := NewCoordinator(CoordinatorConfig{})
	c.SetPersistHook(func(ev LifecycleEvent) error {
		events = append(events, ev)
		return nil
	})

	batch, err := c.CreateBatch("session-key", testManifest(4))
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := c.ApproveBatch("session-key", batch.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	lease, err := c.ClaimNext("session-key")
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if err := c.SetItemContentID("session-key", batch.ID, batch.Items[0].ID, "content-1"); err != nil {
		t.Fatalf("SetItemContentID: %v", err)
	}

	beforeProgress := len(events)
	if err := c.UpdateItemProgress("session-key", lease, 4); err != nil {
		t.Fatalf("UpdateItemProgress: %v", err)
	}
	if len(events) != beforeProgress {
		t.Fatalf("byte progress appended %d durable event(s); it must remain a control-plane-free in-memory update", len(events)-beforeProgress)
	}

	if _, err := c.CompleteItem("session-key", lease, "digest"); err != nil {
		t.Fatalf("CompleteItem: %v", err)
	}

	var sawDeclared, sawApproval, sawItem, sawAllocation, sawBatchState bool
	for _, ev := range events {
		switch ev.Kind {
		case LifecycleBatchDeclared:
			sawDeclared = true
		case LifecycleApprovalChanged:
			sawApproval = true
		case LifecycleItemTransition:
			sawItem = true
			if ev.BatchState != "" {
				sawBatchState = true
			}
		case LifecycleUploadAllocated:
			sawAllocation = true
		}
	}
	if !sawDeclared {
		t.Fatal("batch creation was not persisted")
	}
	if !sawApproval {
		t.Fatal("approval was not persisted")
	}
	if !sawItem {
		t.Fatal("item transitions were not persisted")
	}
	if !sawAllocation {
		t.Fatal("upload allocation was not persisted")
	}
	if !sawBatchState {
		t.Fatal("terminal batch state was not persisted")
	}
}

// TestCoordinatorPersistsCancellationAndRetry covers the cancellation, retry
// and failure transitions named by the recovery architecture.
func TestCoordinatorPersistsCancellationAndRetry(t *testing.T) {
	var events []LifecycleEvent
	c := NewCoordinator(CoordinatorConfig{})
	c.SetPersistHook(func(ev LifecycleEvent) error {
		events = append(events, ev)
		return nil
	})

	batch, err := c.CreateBatch("session-key", testManifest(4))
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := c.ApproveBatch("session-key", batch.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}

	count := len(events)
	if err := c.CancelItem("session-key", batch.ID, batch.Items[0].ID); err != nil {
		t.Fatalf("CancelItem: %v", err)
	}
	if len(events) == count {
		t.Fatal("item cancellation was not persisted")
	}

	// A repeated cancel is a no-op and must not append another event.
	count = len(events)
	if err := c.CancelItem("session-key", batch.ID, batch.Items[0].ID); err != nil {
		t.Fatalf("repeated CancelItem: %v", err)
	}
	if len(events) != count {
		t.Fatal("an idempotent repeated cancel was recorded as a new transition")
	}
}

// TestCoordinatorPersistsSessionClosure covers the session-closure boundary
// including the affected batch records, so a restart cannot resurrect a
// closed queue.
func TestCoordinatorPersistsSessionClosure(t *testing.T) {
	var events []LifecycleEvent
	c := NewCoordinator(CoordinatorConfig{})
	c.SetPersistHook(func(ev LifecycleEvent) error {
		events = append(events, ev)
		return nil
	})
	batch, err := c.CreateBatch("session-key", testManifest(4))
	if err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	if _, err := c.ApproveBatch("session-key", batch.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	if err := c.CloseSession("session-key"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	for _, ev := range events {
		if ev.Kind != LifecycleSessionClosed {
			continue
		}
		if len(ev.Batches) == 0 {
			t.Fatal("session closure carried no batch records")
		}
		for _, b := range ev.Batches {
			if b.ID == batch.ID && b.State != BatchStateCanceled {
				t.Fatalf("closed batch state = %q, want canceled", b.State)
			}
		}
		return
	}
	t.Fatal("session closure was not persisted")
}
