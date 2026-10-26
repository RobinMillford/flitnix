package transfer

import (
	"errors"
	"fmt"
	"testing"
)

func approvedBatch(t *testing.T, c *Coordinator, session string, names ...string) BatchView {
	t.Helper()
	b := createCoordinatorBatch(t, c, session, DirectionPhoneToDesktop, names...)
	if _, err := c.ApproveBatch(session, b.ID); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClaimItemOnlyLeasesTheNextItem(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	b := approvedBatch(t, c, "s", "a.txt", "b.txt")

	// Naming the second item first must not lease anything.
	if _, err := c.ClaimItem("s", b.ID, b.Items[1].ID); !errors.Is(err, ErrNoEligibleItem) {
		t.Fatalf("out-of-order claim = %v, want ErrNoEligibleItem", err)
	}
	if _, err := c.ClaimItem("s", "other-batch", b.Items[0].ID); !errors.Is(err, ErrNoEligibleItem) {
		t.Fatalf("wrong batch claim = %v, want ErrNoEligibleItem", err)
	}
	snap, _ := c.QueueStatus("s")
	if snap.ActiveLeaseID != "" {
		t.Fatal("a refused claim left a lease behind: the queue would be stuck")
	}
	lease, err := c.ClaimItem("s", b.ID, b.Items[0].ID)
	if err != nil || lease.ItemID != b.Items[0].ID {
		t.Fatalf("in-order claim = %+v, %v", lease, err)
	}
	if _, err := c.ClaimItem("s", b.ID, b.Items[1].ID); !errors.Is(err, ErrNoEligibleItem) {
		t.Fatalf("claim while a lease is active = %v, want ErrNoEligibleItem", err)
	}
}

func TestFailBatchEndsBatchWithoutPausingTheQueue(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	first := approvedBatch(t, c, "s", "a.txt", "b.txt", "c.txt")
	second := approvedBatch(t, c, "s", "next.txt")

	done := claim(t, c, "s")
	complete(t, c, "s", done)
	lease := claim(t, c, "s")
	if err := c.UpdateItemProgress("s", lease, 2); err != nil {
		t.Fatal(err)
	}
	view, err := c.FailBatch("s", lease, errors.New("connection reset"))
	if err != nil {
		t.Fatal(err)
	}
	if view.State != BatchStateFailed {
		t.Fatalf("batch state = %q, want failed", view.State)
	}
	states := []ItemState{ItemStateCompleted, ItemStateFailed, ItemStateCanceled}
	for i, want := range states {
		if view.Items[i].State != want {
			t.Fatalf("item %d = %q, want %q", i, view.Items[i].State, want)
		}
	}
	if view.TransferredBytes != 4 {
		t.Fatalf("TransferredBytes = %d, want only the completed item's 4 (no false progress)", view.TransferredBytes)
	}
	// A repeated/stale failure is rejected and changes nothing.
	if _, err := c.FailBatch("s", lease, errors.New("again")); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("repeat FailBatch = %v, want ErrLeaseInvalid", err)
	}
	// The next batch is not blocked behind the failed one.
	next, err := c.ClaimItem("s", second.ID, second.Items[0].ID)
	if err != nil {
		t.Fatalf("queue blocked after a failed batch: %v", err)
	}
	complete(t, c, "s", next)
	got, _ := c.GetBatch("s", first.ID)
	if got.State != BatchStateFailed {
		t.Fatalf("failed batch changed state to %q", got.State)
	}
}

func TestFailBatchAfterDesktopCancelDoesNothing(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	b := approvedBatch(t, c, "s", "a.txt", "b.txt")
	lease := claim(t, c, "s")
	if _, err := c.CancelBatch("s", b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FailBatch("s", lease, errors.New("stream cut")); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("FailBatch after cancel = %v, want ErrLeaseInvalid", err)
	}
	got, _ := c.GetBatch("s", b.ID)
	if got.State != BatchStateCanceled {
		t.Fatalf("state = %q, want canceled to stay authoritative", got.State)
	}
}

func TestFinishedBatchHistoryIsBounded(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	var firstID string
	for i := 0; i < terminalBatchHistory+10; i++ {
		b := approvedBatch(t, c, "s", fmt.Sprintf("f%d.txt", i))
		if i == 0 {
			firstID = b.ID
		}
		lease := claim(t, c, "s")
		complete(t, c, "s", lease)
	}
	// One more creation triggers the prune of the oldest finished batches.
	live := createCoordinatorBatch(t, c, "s", DirectionPhoneToDesktop, "live.txt")
	list, err := c.ListBatches("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) > terminalBatchHistory+1 {
		t.Fatalf("retained %d batches, want at most %d finished + 1 live", len(list), terminalBatchHistory)
	}
	if _, err := c.GetBatch("s", firstID); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("oldest finished batch should be forgotten, got %v", err)
	}
	if _, err := c.GetBatch("s", live.ID); err != nil {
		t.Fatalf("live batch must never be pruned: %v", err)
	}
}

func TestPausedFailedBatchIsNotPruned(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})
	b := approvedBatch(t, c, "s", "bad.txt")
	lease := claim(t, c, "s")
	if _, err := c.FailItem("s", lease, errors.New("disk error")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < terminalBatchHistory+5; i++ {
		createCoordinatorBatch(t, c, "s", DirectionPhoneToDesktop, fmt.Sprintf("x%d.txt", i))
	}
	if _, err := c.GetBatch("s", b.ID); err != nil {
		t.Fatalf("a failed batch that still pauses the queue was pruned: %v", err)
	}
}

func TestSessionHistorySurvivesCloseSessionAndReadsReadOnly(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{})

	// One session carrying every terminal shape: completed, failed,
	// canceled, rejected, plus one still-pending batch.
	created := approvedBatch(t, c, "s", "done.txt") // phone->desktop, approved
	lease := claim(t, c, "s")
	complete(t, c, "s", lease)

	failed := approvedBatch(t, c, "s", "bad.txt")
	flease := claim(t, c, "s")
	if _, err := c.FailItem("s", flease, errors.New("disk error")); err != nil {
		t.Fatal(err)
	}

	canceled := approvedBatch(t, c, "s", "gone.txt")
	if _, err := c.CancelBatch("s", canceled.ID); err != nil {
		t.Fatal(err)
	}

	rejected := createCoordinatorBatch(t, c, "s", DirectionPhoneToDesktop, "no.txt")
	if _, err := c.RejectBatch("s", rejected.ID); err != nil {
		t.Fatal(err)
	}

	pending := createCoordinatorBatch(t, c, "s", DirectionPhoneToDesktop, "waiting.txt")

	// Registration order as delivered to a live session.
	live, err := c.ListBatches("s")
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 5 || live[0].ID != created.ID || live[1].ID != failed.ID ||
		live[2].ID != canceled.ID || live[3].ID != rejected.ID || live[4].ID != pending.ID {
		t.Fatalf("live listing = %v, want 5 batches in registration order", live)
	}
	// The failed batch is still failed while the session lives.
	for i, want := range []BatchState{BatchStateCompleted, BatchStateFailed, BatchStateCanceled, BatchStateRejected, BatchStatePending} {
		if live[i].State != want {
			t.Fatalf("live batch %d state = %q, want %q", i, live[i].State, want)
		}
	}

	if err := c.CloseSession("s"); err != nil {
		t.Fatal(err)
	}

	// After the ending: ListBatches fails closed for everyone, history
	// survives for projections.
	if _, err := c.ListBatches("s"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed-session ListBatches = %v, want ErrSessionClosed", err)
	}
	history, ok := c.SessionHistory("s")
	if !ok {
		t.Fatal("SessionHistory lost a session it once tracked")
	}
	if len(history) != len(live) {
		t.Fatalf("history = %d batches, live listing had %d", len(history), len(live))
	}
	for i := range live {
		if history[i].ID != live[i].ID {
			t.Fatalf("history[%d] = %s, want %s (registration order changed after close)", i, history[i].ID, live[i].ID)
		}
	}
	// CloseSession's contract: completed and rejected keep their state;
	// every other batch shows canceled ("session closed"), and approval
	// states are untouched. History must faithfully report exactly that.
	states := map[string]BatchState{
		created.ID:  BatchStateCompleted,
		failed.ID:   BatchStateCanceled,
		canceled.ID: BatchStateCanceled,
		rejected.ID: BatchStateRejected,
		pending.ID:  BatchStateCanceled,
	}
	approval := map[string]ApprovalState{
		created.ID:  ApprovalApproved,
		failed.ID:   ApprovalApproved,
		canceled.ID: ApprovalApproved,
		rejected.ID: ApprovalRejected,
		pending.ID:  ApprovalPending,
	}
	for id, want := range states {
		for _, h := range history {
			if h.ID == id && h.State != want {
				t.Fatalf("batch %s state = %q, want %q", id, h.State, want)
			}
			if h.ID == id && h.ApprovalState != approval[id] {
				t.Fatalf("batch %s approval = %q, want %q", id, h.ApprovalState, approval[id])
			}
		}
	}

	keys := c.SessionKeys()
	if len(keys) != 1 || keys[0] != "s" {
		t.Fatalf("SessionKeys = %v, want [s]", keys)
	}
	if _, ok := c.SessionHistory("unknown"); ok {
		t.Fatal("SessionHistory invented history for an unknown key")
	}
	if _, ok := c.SessionHistory(""); ok {
		t.Fatal("SessionHistory invented history for an empty key")
	}

	// Closed sessions are read-only: no decision reattaches through history.
	if _, err := c.ApproveBatch("s", created.ID); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("approve on closed session = %v, want ErrSessionClosed", err)
	}
	if _, err := c.GetBatch("s", created.ID); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("GetBatch on closed session = %v, want ErrSessionClosed", err)
	}

	// Closing the whole coordinator keeps nothing readable; keys empty out.
	c.Close()
	if _, ok := c.SessionHistory("s"); ok {
		t.Fatal("SessionHistory served records after the coordinator itself closed")
	}
	if got := c.SessionKeys(); got != nil {
		t.Fatalf("SessionKeys after close = %v, want empty", got)
	}
}
