package transfer

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func declareBatchItems(t *testing.T, svc *Service, session string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		up, err := svc.DeclareBatchItem(session, "", "", fmt.Sprintf("file-%d.txt", i), 4)
		if err != nil {
			t.Fatalf("DeclareBatchItem %d: %v", i, err)
		}
		if !up.BatchLinked {
			t.Fatalf("item %d is not marked batch-linked", i)
		}
		ids = append(ids, up.ID)
	}
	return ids
}

func TestBatchLinkedUploadsAreNotStandalonePendingRequests(t *testing.T) {
	svc, _ := newTestService(t)
	batchIDs := declareBatchItems(t, svc, "session", 3)
	standalone, err := svc.Declare("session", "single.txt", 4)
	if err != nil {
		t.Fatal(err)
	}
	if standalone.BatchLinked {
		t.Fatal("standalone upload must not be batch-linked")
	}
	pending := svc.ListPending()
	if len(pending) != 1 || pending[0].ID != standalone.ID {
		t.Fatalf("ListPending = %+v, want only the standalone upload", pending)
	}
	if got := svc.Counts().Pending; got != 1 {
		t.Fatalf("Counts().Pending = %d, want 1 (batch items excluded)", got)
	}
	// The records still exist for the batch layer.
	for _, id := range batchIDs {
		if u, ok := svc.Get(id); !ok || u.State != StatePending || !u.BatchLinked {
			t.Fatalf("batch record %s = %+v ok=%v", id, u, ok)
		}
	}
}

func TestBatchItemsDoNotConsumeStandaloneCapacity(t *testing.T) {
	svc, _ := newTestService(t)
	// Far more items than MaxPending (32): one batch decision governs them.
	declareBatchItems(t, svc, "session", defaultMaxPendingUploads+20)
	if _, err := svc.Declare("session", "single.txt", 1); err != nil {
		t.Fatalf("standalone declare blocked by batch items: %v", err)
	}
}

func TestDecideBatchItemsIsAllOrNothing(t *testing.T) {
	svc, _ := newTestService(t)
	ids := declareBatchItems(t, svc, "session", 4)
	if err := svc.Cancel(ids[2]); err != nil { // one record is no longer pending
		t.Fatal(err)
	}
	err := svc.DecideBatchItems(ids, true)
	if !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("DecideBatchItems = %v, want ErrAlreadyDecided", err)
	}
	for i, id := range ids {
		u, _ := svc.Get(id)
		want := StatePending
		if i == 2 {
			want = StateCanceled
		}
		if u.State != want {
			t.Fatalf("record %d state = %q, want %q: a failed decision must not touch any record", i, u.State, want)
		}
	}
}

func TestDecideBatchItemsApprovesAndRejectsEveryRecord(t *testing.T) {
	svc, _ := newTestService(t)
	approved := declareBatchItems(t, svc, "session", 3)
	rejected := declareBatchItems(t, svc, "session", 3)
	if err := svc.DecideBatchItems(approved, true); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideBatchItems(rejected, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range approved {
		if u, _ := svc.Get(id); u.State != StateApproved {
			t.Fatalf("%s = %q, want approved", id, u.State)
		}
	}
	for _, id := range rejected {
		if u, _ := svc.Get(id); u.State != StateRejected {
			t.Fatalf("%s = %q, want rejected", id, u.State)
		}
	}
	// A second decision (even the same one) is a state conflict, never silent.
	if err := svc.DecideBatchItems(approved, true); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("repeat decision = %v, want ErrAlreadyDecided", err)
	}
}

func TestDecideBatchItemsRefusesStandaloneAndBadInput(t *testing.T) {
	svc, _ := newTestService(t)
	standalone, _ := svc.Declare("session", "single.txt", 1)
	if err := svc.DecideBatchItems([]string{standalone.ID}, true); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("standalone through batch path = %v, want ErrInvalidUpload", err)
	}
	if u, _ := svc.Get(standalone.ID); u.State != StatePending {
		t.Fatalf("standalone state = %q, want untouched pending", u.State)
	}
	ids := declareBatchItems(t, svc, "session", 1)
	if err := svc.DecideBatchItems(append(ids, ids[0]), true); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("duplicate id = %v, want ErrInvalidUpload", err)
	}
	if err := svc.DecideBatchItems(nil, true); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("empty set = %v, want ErrInvalidUpload", err)
	}
	if err := svc.DecideBatchItems([]string{"nope"}, true); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("unknown id = %v, want ErrUploadNotFound", err)
	}
	if _, err := svc.DeclareBatchItem("session", "root", "", "x", 1); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("root without relative path = %v, want ErrInvalidUpload", err)
	}
}

func TestDecideBatchItemsConcurrentDecisionsHaveOneWinner(t *testing.T) {
	svc, _ := newTestService(t)
	ids := declareBatchItems(t, svc, "session", 6)
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(approve bool) {
			defer wg.Done()
			<-start
			err := svc.DecideBatchItems(ids, approve)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrAlreadyDecided):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i%2 == 0)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 23 {
		t.Fatalf("wins=%d conflicts=%d, want exactly one winner", wins.Load(), conflicts.Load())
	}
	// Whatever the winner chose, every record agrees on it.
	first, _ := svc.Get(ids[0])
	for _, id := range ids {
		if u, _ := svc.Get(id); u.State != first.State {
			t.Fatalf("records disagree: %q vs %q", u.State, first.State)
		}
	}
	if first.State != StateApproved && first.State != StateRejected {
		t.Fatalf("final state = %q", first.State)
	}
}

func TestDeclareBatchItemFolderPathKeepsRelativeLayout(t *testing.T) {
	svc, store := newTestService(t)
	root, err := store.AllocateFolderRoot("Photos")
	if err != nil {
		t.Fatal(err)
	}
	up, err := svc.DeclareBatchItem("session", root, "2026/raw/a.dng", "a.dng", 3)
	if err != nil || !up.BatchLinked {
		t.Fatalf("folder item = %+v err=%v", up, err)
	}
	if !strings.HasSuffix(up.Name, "a.dng") {
		t.Fatalf("name = %q", up.Name)
	}
}
