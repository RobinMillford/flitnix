package transfer

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// holdSink parks the streaming goroutine inside its final Write. That is
// exactly the window the reported race is about: every approved byte has
// arrived, no publication has been attempted yet, and the part file still
// holds the temporary data. Unlike a timing-dependent test, the parking is a
// channel handoff, so the ordering below is decided, not hoped for.
type holdSink struct {
	once    sync.Once
	entered chan struct{} // closed when the last write is in progress
	release chan struct{} // closed (by Abort) to let that write conclude

	// releaseOnce guards the single close of release: Abort is documented as
	// idempotent, so both the cancel path and the stream's cleanup may call it.
	releaseOnce sync.Once

	mu        sync.Mutex
	aborted   bool
	committed bool
}

func (s *holdSink) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted {
		return 0, errors.New("transfer: part file already aborted")
	}
	return len(p), nil
}

func (s *holdSink) Commit() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.committed = true
	if s.aborted {
		return "", errors.New("transfer: part file already committed or aborted")
	}
	return "fake-published", nil
}

func (s *holdSink) Abort() error {
	s.mu.Lock()
	s.aborted = true
	s.mu.Unlock()
	s.releaseOnce.Do(func() { close(s.release) })
	return nil
}

func (s *holdSink) commitRan() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
}

// TestReserveCancelBeforePublicationRefusesTheCommit pins Case A at the
// decision point: a cancel that reserves the outcome while the last bytes are
// still being written makes the stream refuse publication, so no final file is
// linked into place and the record ends canceled rather than completed.
func TestReserveCancelBeforePublicationRefusesTheCommit(t *testing.T) {
	oldSinkFactory := newPartSink
	t.Cleanup(func() { newPartSink = oldSinkFactory })
	hold := &holdSink{entered: make(chan struct{}), release: make(chan struct{})}
	newPartSink = func(*Store, string) (contentSink, error) { return hold, nil }

	svc, _ := newTestService(t)
	const key = "sess-key-reserve-wins"
	const size = 4

	up, err := svc.Declare(key, "reserve-wins.bin", size)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.SubmitContent(up.ID, key, strings.NewReader("abcd"), nil)
		done <- err
	}()

	<-hold.entered // the body is fully received, publication is undecided

	authority, err := svc.ReserveCancel(up.ID)
	if err != nil {
		t.Fatalf("ReserveCancel: %v", err)
	}
	if authority != CancelWins {
		t.Fatalf("authority = %v, want CancelWins", authority)
	}
	if err := svc.Cancel(up.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case serr := <-done:
		if !errors.Is(serr, ErrCanceled) {
			t.Fatalf("submission err = %v, want ErrCanceled", serr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submission did not conclude after the cancel")
	}
	if hold.commitRan() {
		t.Error("the commit ran even though the cancel had reserved the outcome")
	}
	u, ok := svc.Get(up.ID)
	if !ok {
		t.Fatal("record vanished")
	}
	if u.State != StateCanceled {
		t.Errorf("record state = %q, want canceled", u.State)
	}
	if u.Published != "" {
		t.Errorf("canceled record reports a published path %q", u.Published)
	}
	// The reservation survives its own completion: repeating the assertion is
	// still a cancel, which is what an idempotent retry needs.
	if authority, err := svc.ReserveCancel(up.ID); err != nil || authority != CancelWins {
		t.Errorf("second ReserveCancel = (%v, %v), want (CancelWins, nil)", authority, err)
	}
}

// TestReserveCancelAfterPublicationDecidedIsTooLate pins Case B and the
// overlap case: once the stream has claimed publication, a cancel can no
// longer change the outcome. The publication completes, the record reads
// completed with the real path, and every later cancel is reported as too
// late instead of relabeling the record.
func TestReserveCancelAfterPublicationDecidedIsTooLate(t *testing.T) {
	oldSinkFactory := newPartSink
	t.Cleanup(func() { newPartSink = oldSinkFactory })
	race := &commitSuccessRaceSink{
		commitEntered: make(chan struct{}),
		release:       make(chan struct{}),
		abortStarted:  make(chan struct{}),
	}
	newPartSink = func(*Store, string) (contentSink, error) { return race, nil }

	svc, _ := newTestService(t)
	const key = "sess-key-reserve-loses"
	const size = 4

	up, err := svc.Declare(key, "reserve-loses.bin", size)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.SubmitContent(up.ID, key, strings.NewReader("abcd"), nil)
		done <- err
	}()

	<-race.commitEntered // publication is already under way

	authority, err := svc.ReserveCancel(up.ID)
	if authority != CancelLostToPublication {
		t.Fatalf("authority = %v, want CancelLostToPublication", authority)
	}
	if !errors.Is(err, ErrTooLateToCancel) {
		t.Fatalf("ReserveCancel err = %v, want ErrTooLateToCancel", err)
	}

	close(race.release)
	select {
	case serr := <-done:
		if serr != nil {
			t.Fatalf("submission err = %v, want nil (publication won)", serr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submission did not conclude")
	}

	u, ok := svc.Get(up.ID)
	if !ok {
		t.Fatal("record vanished")
	}
	if u.State != StateCompleted || u.Published != "fake-published" {
		t.Fatalf("record after the lost cancel = %+v, want completed at fake-published", u)
	}
	// A cancel arriving after the fact is still honest: it is a no-op, and the
	// record keeps describing the published file.
	if err := svc.Cancel(up.ID); err != nil {
		t.Fatalf("Cancel after publication: %v", err)
	}
	if u, _ := svc.Get(up.ID); u.State != StateCompleted {
		t.Fatalf("late cancel rewrote the record to %q", u.State)
	}
	if authority, err := svc.ReserveCancel(up.ID); authority != CancelLostToPublication || !errors.Is(err, ErrTooLateToCancel) {
		t.Errorf("late reserve = (%v, %v), want (CancelLostToPublication, ErrTooLateToCancel)", authority, err)
	}
}

// TestReserveCancelRefusesFinishedUploads proves a cancel arriving after an
// upload ended another way (a failed stream, or a rejection) is refused with
// the record's own terminal state as the authority, rather than being reported
// as a cancellation that never happened.
func TestReserveCancelRefusesFinishedUploads(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-key-reserve-terminal"

	// A body longer than the approved size fails the stream.
	over, err := svc.Declare(key, "over.bin", 2)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(over.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := svc.SubmitContent(over.ID, key, strings.NewReader("toolong"), nil); err == nil {
		t.Fatal("oversized submission should fail")
	}
	if u, _ := svc.Get(over.ID); u.State != StateFailed {
		t.Fatalf("oversized record = %q, want failed", u.State)
	}
	if authority, err := svc.ReserveCancel(over.ID); authority != CancelLostToTerminal || !errors.Is(err, ErrTooLateToCancel) {
		t.Errorf("reserve on failed upload = (%v, %v), want (CancelLostToTerminal, ErrTooLateToCancel)", authority, err)
	}

	// A completed upload is a publication win, not a terminal dead end.
	done, err := svc.Declare(key, "done.bin", 4)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(done.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if _, err := svc.SubmitContent(done.ID, key, strings.NewReader("abcd"), nil); err != nil {
		t.Fatalf("SubmitContent: %v", err)
	}
	if authority, err := svc.ReserveCancel(done.ID); authority != CancelLostToPublication || !errors.Is(err, ErrTooLateToCancel) {
		t.Errorf("reserve on completed upload = (%v, %v), want (CancelLostToPublication, ErrTooLateToCancel)", authority, err)
	}
	if names := receiveEntries(t, st); len(names) != 1 {
		t.Errorf("receive directory = %v, want exactly the one published file", names)
	}
	if _, err := svc.ReserveCancel("no-such-upload"); !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("reserve on unknown upload err = %v, want ErrUploadNotFound", err)
	}
}
