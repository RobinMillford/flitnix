package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestService builds a service over a throwaway temporary receive
// directory.
func newTestService(t *testing.T) (*Service, *Store) {
	t.Helper()
	st := newTestStore(t)
	svc := NewService(Config{Store: st})
	return svc, st
}

// pollUpload observes a record until it reaches the wanted state with at
// least the wanted byte count. It polls instead of sleeping to fixed
// delays, and fails fast when the record ends in an unexpected terminal
// state, so broken behavior never turns into a slow test.
func pollUpload(t *testing.T, svc *Service, id string, want State, uploadedAtLeast int64) Upload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		u, ok := svc.Get(id)
		if !ok {
			t.Fatalf("upload %s vanished while waiting for %q", id, want)
		}
		if u.State == want && u.Uploaded >= uploadedAtLeast {
			return u
		}
		if isTerminal(u.State) && u.State != want {
			t.Fatalf("upload %s ended in %q (reason=%q) while waiting for %q", id, u.State, u.Reason, want)
		}
		time.Sleep(500 * time.Microsecond)
	}
	t.Fatalf("timed out waiting for upload %s to reach %q with at least %d bytes", id, want, uploadedAtLeast)
	return Upload{} // unreachable
}

// receiveEntries lists the receive directory names, tolerating a directory
// that was never created (nothing was ever opened for writing).
func receiveEntries(t *testing.T, st *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(st.Dir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("readdir %s: %v", st.Dir(), err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestServiceFullLifecycleVerifiesDigest(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-key-full"

	up, err := svc.Declare(key, "photo.jpg", 4096)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if up.State != StatePending || up.Name != "photo.jpg" || up.Session != key || up.Size != 4096 || up.Published != "" {
		t.Fatalf("declared record is not a clean pending upload: %+v", up)
	}

	decided, err := svc.Decide(up.ID, true)
	if err != nil {
		t.Fatalf("Decide(approve): %v", err)
	}
	if decided.State != StateApproved {
		t.Fatalf("approved record is in state %q", decided.State)
	}

	payload := randomBytes(t, 4096)
	want := sha256.Sum256(payload)

	var reports []int64
	res, err := svc.SubmitContent(up.ID, key, strings.NewReader(string(payload)), func(u Upload) {
		if u.State != StateUploading {
			t.Errorf("progress reported in state %q, want uploading", u.State)
		}
		reports = append(reports, u.Uploaded)
	})
	if err != nil {
		t.Fatalf("SubmitContent: %v", err)
	}
	if res.Written != 4096 {
		t.Errorf("written = %d, want the exact approved 4096", res.Written)
	}
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("digest = %s, want %s (must cover exactly the streamed bytes)", res.SHA256, hex.EncodeToString(want[:]))
	}
	if res.Path != filepath.Join(st.Dir(), "photo.jpg") {
		t.Errorf("published path = %q, want the plain name inside the receive dir", res.Path)
	}

	// The published content equals the streamed bytes and stays private.
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("published content differs from the streamed payload")
	}
	if perm := filePerms(t, res.Path); perm&0o077 != 0 {
		t.Errorf("published file permissions %v allow group/others access", perm)
	}

	u, ok := svc.Get(up.ID)
	if !ok {
		t.Fatal("record vanished after completion")
	}
	if u.State != StateCompleted || u.Published != res.Path || u.Uploaded != 4096 || u.Reason != "" {
		t.Errorf("record after completion: %+v", u)
	}

	// The final progress flush must report the full count even for a
	// payload smaller than one stage.
	if len(reports) == 0 || reports[len(reports)-1] != 4096 {
		t.Errorf("progress reports %v: the flush must cover the tail", reports)
	}

	c := svc.Counts()
	if c.Pending != 0 || c.Uploading != 0 {
		t.Errorf("counts after completion = %+v, want zeros", c)
	}
}

func TestServiceUnapprovedAndRejectedContentRefused(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-key-refuse"
	body := strings.NewReader(strings.Repeat("a", 128))

	up, err := svc.Declare(key, "doc.pdf", 128)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}

	// Content while the user has not yet decided: refused, record intact.
	if _, err := svc.SubmitContent(up.ID, key, body, nil); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("unapproved submission err = %v, want ErrNotApproved", err)
	}
	if u, _ := svc.Get(up.ID); u.State != StatePending {
		t.Fatalf("record after refused submission is %q, want pending", u.State)
	}

	// A rejection is permanent: no content may ever be accepted.
	if _, err := svc.Decide(up.ID, false); err != nil {
		t.Fatalf("Decide(reject): %v", err)
	}
	if u, _ := svc.Get(up.ID); u.State != StateRejected {
		t.Fatalf("rejected record is in state %q", u.State)
	}
	if _, err := svc.SubmitContent(up.ID, key, body, nil); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("submission after rejection err = %v, want ErrAlreadyDecided", err)
	}
	if u, _ := svc.Get(up.ID); u.State != StateRejected {
		t.Fatalf("record after post-rejection submission is %q, want still rejected", u.State)
	}

	// Nothing may have touched the receive directory: the desktop user
	// said no, so no temporary or final file may exist.
	if names := receiveEntries(t, st); len(names) > 0 {
		t.Errorf("rejected upload left files behind: %v", names)
	}
}

func TestServiceBoundaryChecks(t *testing.T) {
	svc, _ := newTestService(t)
	key := "sess-key-boundary"

	if _, err := svc.Declare("", "x.jpg", 10); !errors.Is(err, ErrWrongSession) {
		t.Errorf("Declare with empty session err = %v, want ErrWrongSession", err)
	}
	if _, err := svc.SubmitContent("any", "", strings.NewReader("x"), nil); !errors.Is(err, ErrWrongSession) {
		t.Errorf("SubmitContent with empty session err = %v, want ErrWrongSession", err)
	}

	up, err := svc.Declare(key, "mine.jpg", 64)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}

	if _, err := svc.SubmitContent("missing", key, strings.NewReader("x"), nil); !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("unknown ID err = %v, want ErrUploadNotFound", err)
	}
	// Cross-session touch: another token holder must never modify or
	// stream this upload, even by ID collision.
	if _, err := svc.SubmitContent(up.ID, "other-session", strings.NewReader("x"), nil); !errors.Is(err, ErrWrongSession) {
		t.Errorf("cross-session submission err = %v, want ErrWrongSession", err)
	}
	if _, err := svc.Decide("missing", true); !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("unknown-ID decision err = %v, want ErrUploadNotFound", err)
	}
	if err := svc.Cancel("missing"); !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("unknown-ID cancel err = %v, want ErrUploadNotFound", err)
	}
	if _, ok := svc.Get("missing"); ok {
		t.Error("Get returned a record for an unknown ID")
	}

	// The refused cross-session attempt left the record untouched.
	if u, _ := svc.Get(up.ID); u.State != StatePending {
		t.Errorf("record after cross-session attempt is %q, want pending", u.State)
	}
}

func TestServiceDuplicateDecideAndSubmit(t *testing.T) {
	svc, _ := newTestService(t)
	const key = "sess-key-dup"
	body := strings.NewReader(strings.Repeat("b", 32))

	up, err := svc.Declare(key, "dup.bin", 32)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	// A second approval decision cannot happen: the state machine stays
	// exactly-once, in either direction.
	if _, err := svc.Decide(up.ID, true); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("second approve err = %v, want ErrAlreadyDecided", err)
	}
	if _, err := svc.Decide(up.ID, false); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("approve after approve err = %v, want ErrAlreadyDecided", err)
	}

	if _, err := svc.SubmitContent(up.ID, key, body, nil); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// Terminal states accept neither content nor decisions.
	if _, err := svc.SubmitContent(up.ID, key, body, nil); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("post-terminal submission err = %v, want ErrAlreadyDecided", err)
	}
	if _, err := svc.Decide(up.ID, false); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("decision after completion err = %v, want ErrAlreadyDecided", err)
	}
}

func TestServiceInvalidMetadata(t *testing.T) {
	svc, _ := newTestService(t)
	const key = "sess-key-meta"

	if _, err := svc.Declare(key, "../etc/passwd", 10); !errors.Is(err, ErrInvalidFilename) {
		t.Errorf("path traversal name err = %v, want ErrInvalidFilename", err)
	}
	if _, err := svc.Declare(key, "a.jpg", -1); !errors.Is(err, ErrInvalidUpload) {
		t.Errorf("negative size err = %v, want ErrInvalidUpload", err)
	}
	// Zero size is a legal, empty transfer.
	if _, err := svc.Declare(key, "ok.jpg", 0); err != nil {
		t.Errorf("valid declaration err = %v", err)
	}
}

func TestServiceTruncatedUploadFailsAndCleansPart(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-key-trunc"

	up, err := svc.Declare(key, "video.mkv", 1<<20)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// The sender delivers a fraction of the approved size: this is the
	// classic mid-transfer disconnect or lying Content-Length.
	body := strings.Repeat("x", 4096)
	_, err = svc.SubmitContent(up.ID, key, strings.NewReader(body), nil)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated submission err = %v, want ErrTruncated", err)
	}

	u, ok := svc.Get(up.ID)
	if !ok {
		t.Fatal("record vanished after failure")
	}
	if u.State != StateFailed {
		t.Errorf("record state after truncation = %q, want failed", u.State)
	}
	if u.Reason == "" {
		t.Error("failed record must carry a reason for the desktop status view")
	}
	if u.Published != "" {
		t.Errorf("failed record published %q, must never publish", u.Published)
	}

	// The part file is gone and nothing was ever published.
	if names := receiveEntries(t, st); len(names) > 0 {
		t.Errorf("truncated upload left files behind: %v", names)
	}

	// The terminal state is final for content.
	if _, err := svc.SubmitContent(up.ID, key, strings.NewReader(body), nil); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("submission after failure err = %v, want ErrAlreadyDecided", err)
	}
	c := svc.Counts()
	if c.Pending != 0 || c.Uploading != 0 {
		t.Errorf("counts after failure = %+v, want zeros", c)
	}
}

func TestServiceInjectedSinkWriteFailure(t *testing.T) {
	// swap: the sea of package vars is restored by t.Cleanup; tests must
	// not run in parallel when they touch newPartSink.
	oldSinkFactory := newPartSink
	t.Cleanup(func() { newPartSink = oldSinkFactory })
	fake := &errSink{failAfter: 128, commitPath: "fake-path"}
	newPartSink = func(*Store, string) (contentSink, error) { return fake, nil }

	svc, _ := newTestService(t)
	const key = "sess-key-writesink"

	up, err := svc.Declare(key, "broken.bin", 4<<20)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	_, err = svc.SubmitContent(up.ID, key, strings.NewReader(strings.Repeat("y", 4<<20)), nil)
	if !errors.Is(err, errDiskFull) {
		t.Fatalf("write failure err = %v, want the wrapped fake disk error", err)
	}

	u, ok := svc.Get(up.ID)
	if !ok || u.State != StateFailed || u.Reason == "" || u.Published != "" {
		t.Errorf("record after sink failure: %+v (ok=%v)", u, ok)
	}
	if !fake.aborted {
		t.Error("sink was not aborted; the failed write must not keep any temp data")
	}
	if fake.committed {
		t.Error("a failed stream must never be committed")
	}
}

func TestServiceInjectedSinkOpenFailure(t *testing.T) {
	oldSinkFactory := newPartSink
	t.Cleanup(func() { newPartSink = oldSinkFactory })
	newPartSink = func(*Store, string) (contentSink, error) { return nil, errors.New("disk gone") }

	svc, _ := newTestService(t)
	const key = "sess-key-opensink"

	up, err := svc.Declare(key, "doomed.bin", 16)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	_, err = svc.SubmitContent(up.ID, key, strings.NewReader("payload"), nil)
	if err == nil || !strings.Contains(err.Error(), "cannot prepare the receive file") {
		t.Fatalf("open failure err = %v, want the wrapped prepare error", err)
	}
	u, ok := svc.Get(up.ID)
	if !ok || u.State != StateFailed || u.Reason != "disk gone" {
		t.Errorf("record after open failure: %+v (ok=%v)", u, ok)
	}
}

func TestServiceCancelMidStream(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-key-cancel"
	declared := int64(4 << 20)

	up, err := svc.Declare(key, "stream.bin", declared)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := svc.SubmitContent(up.ID, key, pr, nil)
		done <- err
	}()
	t.Cleanup(func() { _ = pw.Close() })

	// Feed one chunk to prove the stream is live, then cancel it. The
	// abort may already have killed the stream racing the tail write, so
	// the post-cancel follow-up write happens from a helper: it can never
	// jam the main goroutine even when the reader is already gone (the
	// cleanup's pipe close unblocks any stuck helper).
	chunk := make([]byte, 512*1024)
	if _, err := pw.Write(chunk); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	u := pollUpload(t, svc, up.ID, StateUploading, 0)
	if u.Uploaded > int64(len(chunk)) {
		t.Errorf("uploaded %d exceeds approved %d", u.Uploaded, declared)
	}

	if err := svc.Cancel(up.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Kick the parked reader so the stream surfaces its failure through
	// the sink; the helper's write simply stays pending if the reader is
	// already gone.
	go func() { _, _ = pw.Write([]byte("kick")) }()

	select {
	case serr := <-done:
		if !errors.Is(serr, ErrCanceled) {
			t.Fatalf("canceled submission err = %v, want ErrCanceled", serr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not conclude after Cancel")
	}

	if u, _ := svc.Get(up.ID); u.State != StateCanceled {
		t.Errorf("record after cancel = %q, want canceled", u.State)
	}
	// The half-received bytes are discarded.
	if names := receiveEntries(t, st); len(names) > 0 {
		t.Errorf("canceled upload left files behind: %v", names)
	}
}

// commitRaceSink mirrors PartFile's decided flag so a Commit that arrives
// after an Abort fails exactly like the production part file does. Commit
// parks on release until the test has injected the cancel, reproducing the
// window between the last successful write and the part-file mutex.
type commitRaceSink struct {
	commitEntered chan struct{} // closed when Commit begins
	release       chan struct{} // closed by the test to let Commit finish

	// decided is written by Abort (test goroutine, after the Submit
	// goroutine is parked in Commit) and read by Commit (Submit
	// goroutine, after release); the channel handoff orders both.
	decided bool
}

func (s *commitRaceSink) Write(p []byte) (int, error) { return len(p), nil }

func (s *commitRaceSink) Commit() (string, error) {
	close(s.commitEntered)
	<-s.release
	if s.decided {
		return "", errors.New("transfer: part file already committed or aborted")
	}
	return "fake-published", nil
}

func (s *commitRaceSink) Abort() error {
	s.decided = true
	return nil
}

func TestServiceCancelDuringCommitKeepsCanceled(t *testing.T) {
	// A cancel can land in the window between the final successful write
	// and publication: the part file holds the mutex for fsync, which is
	// real time on large files, while the record still reads uploading.
	// When the cancel's abort wins the part-file mutex the commit fails;
	// the record must stay canceled instead of being relabeled failed,
	// and the phone must still receive ErrCanceled.
	oldSinkFactory := newPartSink
	t.Cleanup(func() { newPartSink = oldSinkFactory })
	race := &commitRaceSink{
		commitEntered: make(chan struct{}),
		release:       make(chan struct{}),
	}
	newPartSink = func(*Store, string) (contentSink, error) { return race, nil }

	svc, _ := newTestService(t)
	const key = "sess-key-commitcancel"
	const size = 256

	up, err := svc.Declare(key, "commit.bin", size)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.SubmitContent(up.ID, key, strings.NewReader(strings.Repeat("z", size)), nil)
		done <- err
	}()

	<-race.commitEntered
	if err := svc.Cancel(up.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(race.release)

	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("commit-time cancel err = %v, want ErrCanceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submission did not conclude after Cancel")
	}
	if u, ok := svc.Get(up.ID); !ok || u.State != StateCanceled {
		t.Errorf("record after commit-time cancel = %+v (ok=%v), want canceled", u, ok)
	}
}

// commitSuccessRaceSink is the success twin of commitRaceSink: the commit
// wins the part-file race. It mirrors the production PartFile, whose mutex
// is held across the publishing fsync — an Abort that arrives once the
// commit has begun cannot decide the file; it queues behind the mutex and
// finds the publication already done. Holding the sink mutex across the
// park makes the ordering deterministic: the test releases the commit, the
// commit finishes under the mutex, and only then does Abort run.
type commitSuccessRaceSink struct {
	mu            sync.Mutex
	commitEntered chan struct{} // closed when Commit begins
	release       chan struct{} // closed by the test to let Commit finish
	abortStarted  chan struct{} // closed when Abort begins (state write already landed)

	decided bool // written by Abort after the commit has published
}

func (s *commitSuccessRaceSink) Write(p []byte) (int, error) { return len(p), nil }

func (s *commitSuccessRaceSink) Commit() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.commitEntered)
	<-s.release
	if s.decided {
		return "", errors.New("transfer: part file already committed or aborted")
	}
	return "fake-published", nil
}

func (s *commitSuccessRaceSink) Abort() error {
	close(s.abortStarted)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decided = true
	return nil
}

// TestServiceCancelDuringSuccessfulCommitKeepsCompleted pins the published-
// complete-wins policy: when a cancel's abort loses the part-file race to a
// commit that succeeds, the file is published and the record must read
// completed — reporting canceled would contradict the file on disk, and
// unlinking a published file would break atomic publication. The cancel
// request is genuinely lost: it arrived in the commit window, but the
// publication it raced against was already inevitable.
func TestServiceCancelDuringSuccessfulCommitKeepsCompleted(t *testing.T) {
	oldSinkFactory := newPartSink
	t.Cleanup(func() { newPartSink = oldSinkFactory })
	race := &commitSuccessRaceSink{
		commitEntered: make(chan struct{}),
		release:       make(chan struct{}),
		abortStarted:  make(chan struct{}),
	}
	newPartSink = func(*Store, string) (contentSink, error) { return race, nil }

	svc, _ := newTestService(t)
	const key = "sess-key-commitwins"
	const size = 256

	up, err := svc.Declare(key, "commit-wins.bin", size)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.SubmitContent(up.ID, key, strings.NewReader(strings.Repeat("z", size)), nil)
		done <- err
	}()

	<-race.commitEntered
	// Cancel from the commit window. Its state write lands before Abort
	// begins (Cancel unlocks the record mutex before touching the sink),
	// and Abort then parks behind the publishing commit's mutex.
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- svc.Cancel(up.ID) }()
	<-race.abortStarted
	close(race.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("submission after a successful commit race err = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("submission did not conclude after releasing the commit")
	}
	if err := <-cancelDone; err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	u, ok := svc.Get(up.ID)
	if !ok {
		t.Fatalf("record vanished after the successful commit race")
	}
	if u.State != StateCompleted {
		t.Errorf("record after commit won the cancel race = %q, want completed", u.State)
	}
	if u.Published != "fake-published" {
		t.Errorf("published path after the race = %q, want fake-published", u.Published)
	}
}

func TestServiceCancelPendingUpload(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-key-cancelpend"

	up, err := svc.Declare(key, "never.bin", 100)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if err := svc.Cancel(up.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if u, _ := svc.Get(up.ID); u.State != StateCanceled {
		t.Errorf("canceled pending record state = %q", u.State)
	}
	if _, err := svc.SubmitContent(up.ID, key, strings.NewReader(strings.Repeat("x", 100)), nil); !errors.Is(err, ErrCanceled) {
		t.Errorf("submission after cancel err = %v, want ErrCanceled", err)
	}
	if _, err := svc.Decide(up.ID, true); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("decision after cancel err = %v, want ErrAlreadyDecided", err)
	}
	if names := receiveEntries(t, st); len(names) > 0 {
		t.Errorf("canceled pending upload left files: %v", names)
	}
}

func TestServiceCancelApprovedUpload(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-key-cancelapp"

	up, err := svc.Declare(key, "never-streamed.bin", 100)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if u, _ := svc.Get(up.ID); u.State != StateApproved {
		t.Fatalf("record state after approval = %q, want approved", u.State)
	}

	// The desktop changes its mind before any bytes arrive.
	if err := svc.Cancel(up.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if u, _ := svc.Get(up.ID); u.State != StateCanceled {
		t.Errorf("canceled approved record state = %q, want canceled", u.State)
	}
	if _, err := svc.SubmitContent(up.ID, key, strings.NewReader(strings.Repeat("x", 100)), nil); !errors.Is(err, ErrCanceled) {
		t.Errorf("submission after cancel err = %v, want ErrCanceled", err)
	}
	if names := receiveEntries(t, st); len(names) > 0 {
		t.Errorf("canceled approved upload left files: %v", names)
	}
}

func TestServiceCancelSessionOnEnd(t *testing.T) {
	svc, st := newTestService(t)
	keyA, keyB := "sess-a", "sess-b"

	// Session A: one pending upload, one mid-stream upload.
	upA1, err := svc.Declare(keyA, "a-pending.bin", 4096)
	if err != nil {
		t.Fatalf("Declare(A pending): %v", err)
	}
	upA2, err := svc.Declare(keyA, "a-stream.bin", 2<<20)
	if err != nil {
		t.Fatalf("Declare(A stream): %v", err)
	}
	if _, err := svc.Decide(upA2.ID, true); err != nil {
		t.Fatalf("Decide(A stream): %v", err)
	}
	// Session B: an unrelated upload that must survive the cleanup of A.
	upB, err := svc.Declare(keyB, "b.bin", 16)
	if err != nil {
		t.Fatalf("Declare(B): %v", err)
	}

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := svc.SubmitContent(upA2.ID, keyA, pr, nil)
		done <- err
	}()
	t.Cleanup(func() { _ = pw.Close() })
	chunk := make([]byte, 512*1024)
	if _, err := pw.Write(chunk); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	pollUpload(t, svc, upA2.ID, StateUploading, 0)

	// The desktop ends session A: pending and streaming uploads of that
	// session die, nothing else. As in the mid-stream cancel case the
	// abort may already have concluded the stream.
	svc.CancelSession(keyA)

	if u, _ := svc.Get(upA1.ID); u.State != StateCanceled {
		t.Errorf("pending upload of ended session: state %q, want canceled", u.State)
	}
	go func() { _, _ = pw.Write([]byte("kick")) }()
	select {
	case serr := <-done:
		if !errors.Is(serr, ErrCanceled) {
			t.Fatalf("streaming upload after CancelSession err = %v, want ErrCanceled", serr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not conclude after CancelSession")
	}
	if u, _ := svc.Get(upA2.ID); u.State != StateCanceled {
		t.Errorf("streaming upload of ended session: state %q, want canceled", u.State)
	}
	if u, _ := svc.Get(upB.ID); u.State != StatePending {
		t.Errorf("other session's upload: state %q, want pending (untouched)", u.State)
	}

	// No half-received leftovers, and the ended session cannot resume.
	if names := receiveEntries(t, st); len(names) > 0 {
		t.Errorf("session cleanup left files behind: %v", names)
	}
	if _, err := svc.SubmitContent(upA1.ID, keyA, strings.NewReader("x"), nil); !errors.Is(err, ErrCanceled) {
		t.Errorf("submission after session cancel err = %v, want ErrCanceled", err)
	}
}

func TestServiceMaxPendingLimit(t *testing.T) {
	svc, _ := newTestService(t)
	svc = NewService(Config{
		Store:  newTestStore(t),
		Limits: Limits{MaxPending: 2},
	})
	const key = "sess-limits"

	u1, err := svc.Declare(key, "one.bin", 1)
	if err != nil {
		t.Fatalf("Declare 1: %v", err)
	}
	u2, err := svc.Declare(key, "two.bin", 1)
	if err != nil {
		t.Fatalf("Declare 2: %v", err)
	}
	if _, err := svc.Declare(key, "three.bin", 1); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("third declare err = %v, want ErrNoCapacity", err)
	}
	c := svc.Counts()
	if c.Pending != 2 {
		t.Fatalf("pending count = %d, want 2", c.Pending)
	}

	// Terminal records free their slot: rejecting one makes room again.
	if _, err := svc.Decide(u1.ID, false); err != nil {
		t.Fatalf("reject u1: %v", err)
	}
	u4, err := svc.Declare(key, "four.bin", 1)
	if err != nil {
		t.Fatalf("Declare 4 after rejection: %v", err)
	}

	pending := svc.ListPending()
	if len(pending) != 2 {
		t.Fatalf("ListPending = %d entries, want 2", len(pending))
	}
	ids := make(map[string]bool, len(pending))
	for _, p := range pending {
		ids[p.ID] = true
	}
	if !ids[u2.ID] || !ids[u4.ID] {
		t.Errorf("ListPending does not track the live pending uploads: %v", ids)
	}
}

func TestServiceMaxActiveLimit(t *testing.T) {
	st := newTestStore(t)
	svc := NewService(Config{
		Store:  st,
		Limits: Limits{MaxPending: 8, MaxActive: 1},
	})
	const key = "sess-active"

	declared := int64(2) * 512 * 1024
	up, err := svc.Declare(key, "one.bin", declared)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := svc.SubmitContent(up.ID, key, pr, nil)
		done <- err
	}()
	t.Cleanup(func() { _ = pw.Close() })
	chunk := make([]byte, 512*1024)
	if _, err := pw.Write(chunk); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	pollUpload(t, svc, up.ID, StateUploading, 0)

	// While one stream is parked mid-transfer nothing else may be
	// declared: the active cap protects the desktop from a stuck peer.
	if _, err := svc.Declare(key, "two.bin", 16); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("declare while streaming err = %v, want ErrNoCapacity", err)
	}

	// Finish the first stream; the cap lifts.
	if _, err := pw.Write(make([]byte, 512*1024)); err != nil {
		t.Fatalf("second pipe write: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("first stream failed: %v", err)
	}
	pollUpload(t, svc, up.ID, StateCompleted, declared)

	up2, err := svc.Declare(key, "two.bin", 4)
	if err != nil {
		t.Fatalf("declare after first completed: %v", err)
	}
	if _, err := svc.Decide(up2.ID, true); err != nil {
		t.Fatalf("Decide two: %v", err)
	}
	if _, err := svc.SubmitContent(up2.ID, key, strings.NewReader("data"), nil); err != nil {
		t.Fatalf("submit two: %v", err)
	}
}

func TestServiceZeroSizeUpload(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-zero"

	up, err := svc.Declare(key, "empty.txt", 0)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	res, err := svc.SubmitContent(up.ID, key, strings.NewReader(""), nil)
	if err != nil {
		t.Fatalf("SubmitContent: %v", err)
	}
	if res.Written != 0 {
		t.Errorf("written = %d, want 0", res.Written)
	}
	want := sha256.Sum256(nil)
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("digest = %s, want the digest of empty input", res.SHA256)
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("published file missing: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("published size = %d, want 0", info.Size())
	}
	if u, _ := svc.Get(up.ID); u.State != StateCompleted || u.Uploaded != 0 {
		t.Errorf("record after zero-size upload: %+v", u)
	}
	if names := receiveEntries(t, st); len(names) != 1 {
		t.Errorf("expected exactly the published file in the receive dir, got %v", names)
	}
}

func TestServiceProgressObservedLiveAndFinal(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-progress"

	total := 8 * 512 * 1024 // 4 MiB in eight 512 KiB chunks
	payload := randomBytes(t, total)
	up, err := svc.Declare(key, "progress.bin", int64(total))
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	pr, pw := io.Pipe()
	type outcome struct {
		res ContentResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := svc.SubmitContent(up.ID, key, pr, nil)
		done <- outcome{res, err}
	}()
	t.Cleanup(func() { _ = pw.Close() })

	// Feed exactly one progressStep (2 x 512 KiB): once the second Write
	// returns, every byte has been consumed and the 1 MiB step flush has
	// fired, so the live counter is deterministically observable while
	// the remaining 3 MiB stay unsent.
	first := payload[:progressStep]
	if _, err := pw.Write(first[:512*1024]); err != nil {
		t.Fatalf("first pipe write: %v", err)
	}
	if _, err := pw.Write(first[512*1024:]); err != nil {
		t.Fatalf("second pipe write: %v", err)
	}
	mid := pollUpload(t, svc, up.ID, StateUploading, progressStep)
	if mid.Uploaded != progressStep {
		t.Errorf("live progress = %d, want exactly %d after one step consumed", mid.Uploaded, progressStep)
	}

	rest := payload[progressStep:]
	if _, err := pw.Write(rest); err != nil {
		t.Fatalf("rest pipe write: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("pipe close: %v", err)
	}
	out := <-done
	if out.err != nil {
		t.Fatalf("successful stream returned an error: %v", out.err)
	}
	res := out.res

	// A completed record accepts no further content.
	if _, err := svc.SubmitContent(up.ID, key, strings.NewReader(""), nil); !errors.Is(err, ErrAlreadyDecided) {
		t.Errorf("post-completion submission err = %v, want ErrAlreadyDecided", err)
	}

	want := sha256.Sum256(payload)
	u := pollUpload(t, svc, up.ID, StateCompleted, int64(total))
	if u.Uploaded != int64(total) {
		t.Errorf("final Uploaded = %d, want %d (the flush must cover the tail)", u.Uploaded, total)
	}

	// Digest and disk content must both cover exactly the streamed bytes.
	data, err := os.ReadFile(filepath.Join(st.Dir(), "progress.bin"))
	if err != nil {
		t.Fatalf("read published: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("disk content differs from the streamed payload")
	}
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("result digest = %s, want %s", res.SHA256, hex.EncodeToString(want[:]))
	}
}

func TestServiceSameNameUploadsPublishDistinctFiles(t *testing.T) {
	svc, st := newTestService(t)
	const key = "sess-collide"

	up1, err := svc.Declare(key, "report.pdf", 5)
	if err != nil {
		t.Fatalf("Declare 1: %v", err)
	}
	if _, err := svc.Decide(up1.ID, true); err != nil {
		t.Fatalf("Decide 1: %v", err)
	}
	res1, err := svc.SubmitContent(up1.ID, key, strings.NewReader("alpha"), nil)
	if err != nil {
		t.Fatalf("submit 1: %v", err)
	}

	up2, err := svc.Declare(key, "report.pdf", 4)
	if err != nil {
		t.Fatalf("Declare 2: %v", err)
	}
	if _, err := svc.Decide(up2.ID, true); err != nil {
		t.Fatalf("Decide 2: %v", err)
	}
	res2, err := svc.SubmitContent(up2.ID, key, strings.NewReader("beta"), nil)
	if err != nil {
		t.Fatalf("submit 2: %v", err)
	}

	// The first file must be untouched; the second goes to the numbered
	// variant. Nothing was ever overwritten.
	if filepath.Base(res1.Path) != "report.pdf" || filepath.Base(res2.Path) != "report (1).pdf" {
		t.Errorf("published names are %q and %q; collisions must become numbered variants", filepath.Base(res1.Path), filepath.Base(res2.Path))
	}
	first, _ := os.ReadFile(res1.Path)
	second, _ := os.ReadFile(res2.Path)
	if string(first) != "alpha" || string(second) != "beta" {
		t.Errorf("collision published the wrong contents: %q and %q", first, second)
	}
	if names := receiveEntries(t, st); len(names) != 2 {
		t.Errorf("expected exactly two published files, got %v", names)
	}
}

func TestServiceTerminalHistoryPrunesOldest(t *testing.T) {
	svc, _ := newTestService(t)
	const key = "sess-prune"

	var first, last string
	for i := 0; i < 100; i++ {
		up, err := svc.Declare(key, fmt.Sprintf("file-%d.txt", i), 1)
		if err != nil {
			t.Fatalf("declare %d: %v", i, err)
		}
		if i == 0 {
			first = up.ID
		}
		last = up.ID
		if _, err := svc.Decide(up.ID, false); err != nil {
			t.Fatalf("reject %d: %v", i, err)
		}
	}

	// The history is bounded: only the newest terminal records survive,
	// and the very first one is definitely gone.
	list := svc.List()
	if len(list) > defaultTerminalHistory+1 || len(list) < defaultTerminalHistory {
		t.Errorf("terminal history length = %d, want the %d-record retention window", len(list), defaultTerminalHistory)
	}
	if _, ok := svc.Get(first); ok {
		t.Error("the oldest terminal record survived pruning")
	}
	if u, ok := svc.Get(last); !ok || u.State != StateRejected {
		t.Errorf("the newest record was wrongly pruned: (%v, %v)", u, ok)
	}
}

func TestServiceExpectedSHA256DigestVerification(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	svc := NewService(Config{Store: store})
	const key = "sess-digest"

	content := []byte("hello digest verification")
	h := sha256.Sum256(content)
	correctDigest := hex.EncodeToString(h[:])
	wrongDigest := "0000000000000000000000000000000000000000000000000000000000000000"

	// 1. Success with matching digest
	up1, err := svc.DeclareWithDigest(key, "match.txt", int64(len(content)), correctDigest, "dev-1")
	if err != nil {
		t.Fatalf("DeclareWithDigest: %v", err)
	}
	if _, err := svc.Decide(up1.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	res1, err := svc.SubmitContent(up1.ID, key, bytes.NewReader(content), nil)
	if err != nil {
		t.Fatalf("SubmitContent matching: %v", err)
	}
	if res1.SHA256 != correctDigest {
		t.Fatalf("res1 SHA256 %s != %s", res1.SHA256, correctDigest)
	}

	// Verify published file exists
	if _, err := os.Stat(res1.Path); err != nil {
		t.Fatalf("published file does not exist: %v", err)
	}

	// 2. Failure with mismatched digest
	up2, err := svc.DeclareWithDigest(key, "mismatch.txt", int64(len(content)), wrongDigest, "dev-1")
	if err != nil {
		t.Fatalf("DeclareWithDigest: %v", err)
	}
	if _, err := svc.Decide(up2.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	_, err = svc.SubmitContent(up2.ID, key, bytes.NewReader(content), nil)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("expected ErrDigestMismatch, got: %v", err)
	}

	// Verify upload state is StateFailed
	snap2, ok := svc.Get(up2.ID)
	if !ok || snap2.State != StateFailed {
		t.Fatalf("expected StateFailed, got state=%s", snap2.State)
	}

	// Verify no file was published in receive dir
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "mismatch") {
			t.Fatalf("part file was not cleaned up / file was published: %s", e.Name())
		}
	}
}
