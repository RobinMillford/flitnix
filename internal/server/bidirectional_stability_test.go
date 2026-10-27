package server

// Regression tests for one session running an incoming (phone -> desktop)
// upload and an outgoing (desktop -> phone) download at the same time.
//
// Auditing that behaviour found the two directions independent on paper:
// separate registries, separate concurrency guards, an additive session pin,
// no lock held across a byte copy in either direction, and a browser download
// that never navigates the page. What was missing was any test that actually
// ran both directions concurrently under one session, so that is what these
// do. They are test-only: the transport protocol, the authentication model,
// the sequential incoming queue and every existing limit are untouched.
//
// Every test here has the same shape. The incoming stream is held open
// mid-transfer through an io.Pipe, the outgoing stream is started while the
// incoming one is still live, and only then is anything asserted. Nothing
// sleeps to manufacture a race: each step waits on an observable server-side
// fact (a session pin, an item state, a progress counter). The outgoing
// source is a sparse file larger than the kernel send/receive buffers plus
// the client's read buffer, so the archive stream cannot silently complete
// before the overlap is asserted, and it costs no memory anywhere - both
// sides stream through fixed 64 KiB buffers, and the test writes the archive
// straight to disk rather than into a byte slice.

import (
	"archive/zip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

const (
	// overlapItemSize is the first incoming item's size. It is several times
	// the streaming progress step (1 MiB) so the coordinator records
	// genuinely increasing progress rather than a single final update.
	overlapItemSize = 4 << 20

	// overlapStep is how many bytes one observed incoming progress step is
	// worth; it matches the streaming layer's own progress interval.
	overlapStep = 1 << 20

	// overlapArchiveSize is the outgoing source's size. It has to exceed the
	// kernel send buffer + receive buffer + the client's read buffer, or the
	// whole archive could be absorbed and the stream would finish before the
	// overlap could be asserted. It is a sparse file: no disk blocks, no
	// memory, and reading it is a loopback stream.
	overlapArchiveSize = 32 << 20

	// overlapArchiveEntry is the one entry a single-file folder source
	// produces, and its verified size.
	overlapArchiveRoot = "Outgoing"
)

// overlapFixture is one live server with one authenticated session and both
// directions armed. It embeds *liveEnv so it reuses the existing harness
// helpers (startPut/awaitPut, grantFor/liveGrant, get/progress, waitFor,
// waitPinned, partFiles, approveBatch, contentURL, receiveDirNames, ...).
//
// The session record is named activeSession rather than sess so it cannot
// shadow the embedded environment's session *manager*.
type overlapFixture struct {
	*liveEnv

	tok           string
	activeSession *session.Session
	key           string

	batch     transfer.BatchView
	contentID string // item 0's streaming-service record ID

	itemSize int64
	step     int64
	written  int64
	scratch  []byte

	pw  *io.PipeWriter
	put <-chan putResult

	grant  createGrantResponse
	cookie *http.Cookie
	dl     *http.Response
}

// newOverlapFixture builds the environment, arms the first overlap cycle with
// a batch of the given item sizes, and returns it.
func newOverlapFixture(t *testing.T, itemSizes ...int64) *overlapFixture {
	t.Helper()
	if len(itemSizes) == 0 {
		t.Fatal("an overlap fixture needs at least one batch item")
	}
	live := newLiveEnv(t)
	tok := live.token()
	sess, err := live.sess.Validate(tok)
	if err != nil {
		t.Fatalf("validating the fixture's session: %v", err)
	}
	f := &overlapFixture{
		liveEnv:       live,
		tok:           tok,
		activeSession: sess,
		key:           sess.Key(),
		step:          overlapStep,
		scratch:       syntheticBytes(overlapStep, 0x5a),
	}
	f.arm(itemSizes...)
	return f
}

// arm registers one approved incoming batch and one outgoing desktop folder
// with a fresh download grant, ready for one overlap cycle. A batch is decided
// once and a grant is redeemed once, so each cycle needs its own.
func (f *overlapFixture) arm(itemSizes ...int64) {
	f.t.Helper()
	b := f.filesBatch(f.tok, itemSizes...)
	f.approveBatch(f.key, b)
	f.batch = b
	f.itemSize = itemSizes[0]

	id, err := f.coord.ItemContentID(f.key, b.ID, b.Items[0].ID)
	if err != nil {
		f.t.Fatalf("content id for item 0: %v", err)
	}
	f.contentID = id
	f.written = 0

	root := filepath.Join(f.t.TempDir(), overlapArchiveRoot)
	sparseFile(f.t, filepath.Join(root, "movie.bin"), overlapArchiveSize)
	source, err := f.reg.AddFolder(root, "")
	if err != nil {
		f.t.Fatalf("registering the outgoing folder: %v", err)
	}
	f.grant, f.cookie = liveGrant(f.t, f.ts.URL, f.tok, source.ID)
	if !f.grant.Archive {
		f.t.Fatal("the outgoing folder did not produce an archive grant")
	}
}

// ---- the incoming direction ----

// startUpload opens the incoming stream and waits until the server itself has
// confirmed it: the session is pinned and the coordinator reports item 0 as
// actively transferring. It then pushes one progress step and waits for that
// step to be recorded, so every later assertion has real bytes behind it.
func (f *overlapFixture) startUpload() {
	f.t.Helper()
	pw, put := startPut(contentURL(f.ts.URL, f.batch, 0), f.tok, f.itemSize)
	f.pw, f.put = pw, put
	// A failing assertion must not leave the body open: an unclosed pipe
	// would keep the handler alive and make httptest.Server's Close wait for
	// it. Closing is idempotent, so this is harmless after finishUpload.
	f.t.Cleanup(func() { _ = pw.Close() })
	waitPinned(f.t, f.activeSession, 1)
	waitFor(f.t, "item 0 to be actively transferring", func() bool {
		v, err := f.coord.GetBatch(f.key, f.batch.ID)
		return err == nil && len(v.Items) > 0 && v.Items[0].State == transfer.ItemStateTransferring
	})
	f.mustPush(f.step)
	waitFor(f.t, "the first incoming progress step to be recorded", func() bool {
		return f.uploadedBytes() >= f.step
	})
}

// push writes n bytes of the scratch pattern into the incoming body. A write
// error is returned rather than fatal: once a stream has been aborted, the
// pipe write is expected to fail and that failure is part of what the caller
// is asserting.
func (f *overlapFixture) push(n int64) error {
	for n > 0 {
		b := int64(len(f.scratch))
		if b > n {
			b = n
		}
		if _, err := f.pw.Write(f.scratch[:b]); err != nil {
			return err
		}
		f.written += b
		n -= b
	}
	return nil
}

// mustPush is push for the phases where the stream is known to be healthy.
func (f *overlapFixture) mustPush(n int64) {
	f.t.Helper()
	if err := f.push(n); err != nil {
		f.t.Fatalf("writing the incoming body: %v", err)
	}
}

// finishUpload sends the remaining declared bytes, closes the body and returns
// the PUT's outcome.
func (f *overlapFixture) finishUpload() putResult {
	f.t.Helper()
	if rem := f.itemSize - f.written; rem > 0 {
		f.mustPush(rem)
	}
	_ = f.pw.Close()
	return awaitPut(f.t, f.put)
}

// abortUpload drives the incoming stream with a small write so the handler
// observes an abort it cannot have seen yet, tolerating the pipe failure that
// abort causes, then closes the body and returns the PUT's outcome.
func (f *overlapFixture) abortUpload() putResult {
	f.t.Helper()
	_ = f.push(4096)
	_ = f.pw.Close()
	return awaitPut(f.t, f.put)
}

// uploadedBytes reports the coordinator's live transferred count for item 0.
func (f *overlapFixture) uploadedBytes() int64 {
	v, err := f.coord.GetBatch(f.key, f.batch.ID)
	if err != nil || len(v.Items) == 0 {
		return -1
	}
	return v.Items[0].Transferred
}

// putSmallItem uploads one trailing batch item through the recorder path. The
// small items exist so the incoming batch can reach its terminal state after
// the overlapped stream finishes.
func (f *overlapFixture) putSmallItem(index int, body []byte) {
	f.t.Helper()
	item := f.batch.Items[index]
	if int64(len(body)) != item.DeclaredSize {
		f.t.Fatalf("test bug: item %d declared %d bytes, body is %d", index, item.DeclaredSize, len(body))
	}
	rec := f.do(http.MethodPut, "/api/batches/"+f.batch.ID+"/items/"+item.ID+"/content", f.tok, body)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("uploading item %d = %d %s", index, rec.Code, rec.Body.String())
	}
}

// ---- the outgoing direction ----

// startDownload issues the outgoing GET and returns as soon as the response
// headers arrive; the body is left unread, so the archive stream stays live.
// The context lets a test abandon the request the way a phone does when the
// user cancels the save. It then waits for two pins, which is the observable
// proof that both transfers hold the session at once.
func (f *overlapFixture) startDownload(ctx context.Context) *http.Response {
	f.t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.ts.URL+f.grant.DownloadURL, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	req.AddCookie(f.cookie)
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		f.t.Fatalf("starting the outgoing download: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		f.t.Fatalf("outgoing download = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		_ = resp.Body.Close()
		f.t.Fatalf("outgoing Content-Type = %q, want application/zip", ct)
	}
	f.dl = resp
	waitPinned(f.t, f.activeSession, 2)
	return resp
}

// finishDownload drains whatever is left of the outgoing stream to a file on
// disk (never into memory), validates it with Go's ZIP reader and returns each
// entry's verified size. head, when non-nil, is the part of the body a test
// already consumed.
func (f *overlapFixture) finishDownload(head []byte) map[string]int64 {
	f.t.Helper()
	out := filepath.Join(f.t.TempDir(), "download.zip")
	file, err := os.Create(out)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(head) > 0 {
		if _, err := file.Write(head); err != nil {
			_ = file.Close()
			f.t.Fatal(err)
		}
	}
	written, copyErr := io.Copy(file, f.dl.Body)
	_ = f.dl.Body.Close()
	if err := file.Close(); err != nil {
		f.t.Fatal(err)
	}
	if copyErr != nil {
		f.t.Fatalf("streaming the archive failed after %d bytes: %v", written, copyErr)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		f.t.Fatalf("the downloaded archive is not a valid ZIP: %v", err)
	}
	defer zr.Close()
	sizes := make(map[string]int64, len(zr.File))
	for _, zf := range zr.File {
		if strings.HasSuffix(zf.Name, "/") {
			sizes[zf.Name] = 0
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			f.t.Fatalf("open archive entry %q: %v", zf.Name, err)
		}
		copied, err := io.Copy(io.Discard, rc) // reading to EOF verifies the CRC
		_ = rc.Close()
		if err != nil {
			f.t.Fatalf("reading archive entry %q (CRC check): %v", zf.Name, err)
		}
		if uint64(copied) != zf.UncompressedSize64 {
			f.t.Fatalf("archive entry %q: read %d bytes, header says %d", zf.Name, copied, zf.UncompressedSize64)
		}
		sizes[zf.Name] = copied
	}
	return sizes
}

// downloadRecord returns the outgoing telemetry record owned by this session.
func (f *overlapFixture) downloadRecord() (transfer.Download, bool) {
	for _, d := range f.dls.List() {
		if d.Session == f.key {
			return d, true
		}
	}
	return transfer.Download{}, false
}

// downloadProcessed reports the outgoing stream's server-side byte counter.
func (f *overlapFixture) downloadProcessed() int64 {
	p, ok := f.srv.grants.getProgress(f.grant.ProgressID, f.key)
	if !ok {
		return -1
	}
	return p.Processed
}

// downloadProgressState reports the outgoing grant progress record's state.
func (f *overlapFixture) downloadProgressState() string {
	p, ok := f.srv.grants.getProgress(f.grant.ProgressID, f.key)
	if !ok {
		return "missing"
	}
	return p.State
}

// isTerminalDownload reports whether an outgoing record has finished.
func isTerminalDownload(s transfer.DownloadState) bool {
	switch s {
	case transfer.DownloadSent, transfer.DownloadFailed, transfer.DownloadCanceled:
		return true
	default:
		return false
	}
}

// ---- shared assertions ----

func (f *overlapFixture) batchView() transfer.BatchView {
	v, err := f.coord.GetBatch(f.key, f.batch.ID)
	if err != nil {
		f.t.Fatalf("get batch: %v", err)
	}
	return v
}

// assertBatchCompleted checks the incoming batch ended exactly as a sequential
// upload should: every item completed, the batch completed, and the accounting
// matching the manifest.
func (f *overlapFixture) assertBatchCompleted() {
	f.t.Helper()
	v := f.batchView()
	for i, item := range v.Items {
		if item.State != transfer.ItemStateCompleted {
			f.t.Fatalf("item %d = %q, want completed (items %v)", i, item.State, itemStates(v))
		}
	}
	if v.State != transfer.BatchStateCompleted {
		f.t.Fatalf("incoming batch = %q, want completed", v.State)
	}
	var want int64
	for _, item := range v.Items {
		want += item.DeclaredSize
	}
	if v.TransferredBytes != want {
		f.t.Fatalf("incoming batch accounting = %d bytes, want %d", v.TransferredBytes, want)
	}
}

// assertBatchCanceled reads the batch through the history projection, which
// deliberately outlives a closed session, so it works after a revocation too.
func (f *overlapFixture) assertBatchCanceled() {
	f.t.Helper()
	views, ok := f.coord.SessionHistory(f.key)
	if !ok {
		f.t.Fatal("the coordinator forgot the session's batch history")
	}
	for _, v := range views {
		if v.ID != f.batch.ID {
			continue
		}
		if v.State != transfer.BatchStateCanceled {
			f.t.Fatalf("incoming batch = %q, want canceled", v.State)
		}
		for i, item := range v.Items {
			if item.State != transfer.ItemStateCanceled {
				f.t.Fatalf("item %d = %q, want canceled", i, item.State)
			}
		}
		return
	}
	f.t.Fatal("the incoming batch is missing from the session history")
}

// assertClean proves both directions released everything they held: no session
// pin, no live coordinator lease, no outgoing grant slot, no temporary receive
// file, and no outgoing record left mid-transfer.
func (f *overlapFixture) assertClean() {
	f.t.Helper()
	waitPinned(f.t, f.activeSession, 0)
	waitFor(f.t, "the outgoing grant slot to be released", func() bool {
		_, _, active := f.srv.grants.size()
		return active == 0
	})
	// QueueStatus fails closed once the session is closed, which is expected
	// after a revocation; only a live session can hold a lease.
	if q, err := f.coord.QueueStatus(f.key); err == nil && q.ActiveLeaseID != "" {
		f.t.Fatalf("a coordinator lease was still held: %q", q.ActiveLeaseID)
	}
	if left := partFiles(f.t, f.store.Dir()); len(left) != 0 {
		f.t.Fatalf("temporary receive files survived the transfers: %v", left)
	}
	for _, name := range receiveDirNames(f.t, f.store) {
		if strings.Contains(name, ".part.") {
			f.t.Fatalf("an unpublished receive file survived: %q", name)
		}
	}
	for _, d := range f.dls.List() {
		if d.Session != f.key {
			continue
		}
		if !isTerminalDownload(d.State) {
			f.t.Fatalf("an outgoing record is stuck in %q", d.State)
		}
	}
}

// assertAnotherSessionIsUnaffected proves the outgoing slot is genuinely per
// session: a second session completes a download while this session is running
// both directions.
func (f *overlapFixture) assertAnotherSessionIsUnaffected() {
	f.t.Helper()
	other := f.token()
	file := f.shareFile(f.t.TempDir(), "other.txt", []byte("independent session"))
	grant, cookie := f.grantFor(other, file.ID)
	resp, err := f.get(grant, cookie)
	if err != nil {
		f.t.Fatalf("a second session's download: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatalf("reading a second session's download: %v", err)
	}
	if string(body) != "independent session" {
		f.t.Fatalf("a second session's download = %q", body)
	}
}

// ---- the primary test ----

// TestUploadAndDownloadOverlapInOneSession runs a real server, holds an
// incoming batch item mid-stream, starts an outgoing archive under the same
// session while that upload is live, and proves both directions progress
// during the same interval and then finish independently.
func TestUploadAndDownloadOverlapInOneSession(t *testing.T) {
	f := newOverlapFixture(t, overlapItemSize, 5, 6)

	// The incoming batch is approved and item 0 is streaming. Nothing about
	// the overlap is assumed before this returns: the server itself has
	// confirmed the item's state and the session's pin.
	f.startUpload()

	// The outgoing archive starts while the incoming stream is still open,
	// still holding its pin, with 3 MiB of its declared 4 MiB unsent.
	f.startDownload(context.Background())

	// The outgoing direction is live, with an active telemetry record rather
	// than a finished one.
	rec, ok := f.downloadRecord()
	if !ok {
		t.Fatal("no outgoing download record was created for the session")
	}
	if rec.Kind != "archive" {
		t.Fatalf("outgoing record kind = %q, want archive", rec.Kind)
	}
	if !isLiveDownload(rec.State) {
		t.Fatalf("outgoing record state = %q during the overlap, want a live state", rec.State)
	}

	// The client really receives bytes while the upload is live.
	head := make([]byte, 64<<10)
	if _, err := io.ReadFull(f.dl.Body, head); err != nil {
		t.Fatalf("reading the start of the outgoing archive: %v", err)
	}
	waitFor(t, "the outgoing stream to report progress", func() bool {
		return f.downloadProcessed() > 0
	})

	// Both transfers hold the session at once: one pin per direction.
	if n := f.activeSession.PinnedTransfers(); n != 2 {
		t.Fatalf("session pins during the overlap = %d, want 2 (one per direction)", n)
	}

	// Independent sessions stay isolated while this one runs both directions.
	f.assertAnotherSessionIsUnaffected()
	if n := f.activeSession.PinnedTransfers(); n != 2 {
		t.Fatalf("pins after a second session's download = %d, want 2", n)
	}

	// The incoming batch records increasing progress during the overlap.
	before := f.uploadedBytes()
	if before < overlapStep {
		t.Fatalf("incoming progress before the overlap = %d bytes, want at least %d", before, overlapStep)
	}
	f.mustPush(overlapStep)
	waitFor(t, "the incoming upload to record a further progress step", func() bool {
		return f.uploadedBytes() >= before+overlapStep
	})
	if after := f.uploadedBytes(); after <= before {
		t.Fatalf("incoming progress did not advance during the overlap: %d -> %d", before, after)
	}
	if n := f.activeSession.PinnedTransfers(); n != 2 {
		t.Fatalf("session pins after both directions progressed = %d, want 2", n)
	}

	// The outgoing stream is drained to completion first. Finishing it may
	// release only its own pin: the count must settle at 1, never 0, because
	// the incoming upload is still streaming.
	sizes := f.finishDownload(head)
	wantEntry := overlapArchiveRoot + "/movie.bin"
	if len(sizes) != 1 || sizes[wantEntry] != overlapArchiveSize {
		t.Fatalf("downloaded archive entries = %v, want exactly %s of %d bytes", sizes, wantEntry, overlapArchiveSize)
	}
	waitPinned(t, f.activeSession, 1)
	waitFor(t, "the outgoing progress record to complete", func() bool {
		return f.downloadProgressState() == progressCompleted
	})
	if n := f.activeSession.PinnedTransfers(); n != 1 {
		t.Fatalf("pins after the outgoing record completed = %d, want 1", n)
	}

	// The incoming upload continues after the download ended and finishes
	// normally, with no resumed claim or duplicate record.
	if r := f.finishUpload(); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("the upload did not survive the download: %+v", r)
	}
	v := f.batchView()
	if v.Items[0].State != transfer.ItemStateCompleted {
		t.Fatalf("item 0 = %q, want completed", v.Items[0].State)
	}
	if v.State != transfer.BatchStateQueued {
		t.Fatalf("incoming batch after item 0 = %q, want queued (two items still to send)", v.State)
	}
	if u, ok := f.trans.Get(f.contentID); !ok || u.State != transfer.StateCompleted || u.Uploaded != f.itemSize {
		t.Fatalf("item 0's record = %+v, want completed with %d bytes", u, f.itemSize)
	}

	// The queue drains the remaining items exactly as it does without an
	// interleaved download.
	f.putSmallItem(1, []byte("abcde"))
	f.putSmallItem(2, []byte("abcdef"))
	f.assertBatchCompleted()

	f.assertClean()
}

// isLiveDownload reports whether an outgoing record still represents a running
// stream.
func isLiveDownload(s transfer.DownloadState) bool {
	switch s {
	case transfer.DownloadTransferring, transfer.DownloadFinalizing:
		return true
	default:
		return false
	}
}

// ---- isolation tests ----

// TestCancelledDownloadLeavesTheUploadRunning abandons the outgoing request the
// way a phone does when the download is cancelled, and proves the incoming
// upload is untouched: it keeps its pin, finishes normally, and the batch
// reaches its terminal state.
func TestCancelledDownloadLeavesTheUploadRunning(t *testing.T) {
	f := newOverlapFixture(t, overlapItemSize)

	f.startUpload()
	ctx, cancel := context.WithCancel(context.Background())
	f.startDownload(ctx)
	if _, err := io.ReadFull(f.dl.Body, make([]byte, 32<<10)); err != nil {
		t.Fatalf("reading the start of the outgoing archive: %v", err)
	}

	// The outgoing stream has to still be running for a cancel to be
	// meaningful: this asserts the premise rather than trusting socket
	// buffering to be small enough.
	if rec, ok := f.downloadRecord(); !ok || !isLiveDownload(rec.State) {
		t.Fatalf("outgoing record = %+v, want a live stream before the cancel", rec)
	}

	// The phone cancels the save.
	cancel()
	_ = f.dl.Body.Close()

	waitFor(t, "the outgoing record to reach a terminal state", func() bool {
		rec, ok := f.downloadRecord()
		return ok && isTerminalDownload(rec.State)
	})
	if rec, _ := f.downloadRecord(); rec.State != transfer.DownloadFailed {
		t.Fatalf("a cancelled outgoing stream ended %q, want failed", rec.State)
	}

	// The incoming upload still holds its own pin and still works.
	waitPinned(t, f.activeSession, 1)
	if r := f.finishUpload(); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("the upload did not survive a cancelled download: %+v", r)
	}
	f.assertBatchCompleted()
	f.assertClean()
}

// TestCancelledUploadLeavesTheDownloadRunning cancels the incoming batch the
// way the desktop does, and proves the outgoing stream is untouched: it is
// still live at the moment the upload dies, and it delivers a complete,
// valid archive afterwards.
func TestCancelledUploadLeavesTheDownloadRunning(t *testing.T) {
	f := newOverlapFixture(t, overlapItemSize)

	f.startUpload()
	f.startDownload(context.Background())

	// The desktop cancels the incoming batch (App.CancelBatch).
	cancelled, err := f.coord.CancelBatch(f.key, f.batch.ID)
	if err != nil {
		t.Fatalf("cancel the incoming batch: %v", err)
	}
	if cancelled.State != transfer.BatchStateCanceled {
		t.Fatalf("incoming batch after cancel = %q", cancelled.State)
	}

	// Drive the incoming stream so it observes the abort it cannot have seen.
	if r := f.abortUpload(); r.err == nil && r.code == http.StatusOK {
		t.Fatalf("a cancelled upload reported success: %+v", r)
	}
	waitFor(t, "the incoming record to end canceled", func() bool {
		u, ok := f.trans.Get(f.contentID)
		return ok && u.State == transfer.StateCanceled
	})
	if v := f.batchView(); v.State != transfer.BatchStateCanceled {
		t.Fatalf("incoming batch = %q, want canceled (a stream error must not reopen it)", v.State)
	}

	// The outgoing stream is still live at this instant and still holds its
	// share of the session.
	rec, ok := f.downloadRecord()
	if !ok || !isLiveDownload(rec.State) {
		t.Fatalf("outgoing record = %+v, want a live stream after the upload was cancelled", rec)
	}
	waitPinned(t, f.activeSession, 1)

	// And it delivers a complete archive.
	wantEntry := overlapArchiveRoot + "/movie.bin"
	sizes := f.finishDownload(nil)
	if len(sizes) != 1 || sizes[wantEntry] != overlapArchiveSize {
		t.Fatalf("downloaded archive entries = %v, want exactly %s of %d bytes", sizes, wantEntry, overlapArchiveSize)
	}
	waitFor(t, "the outgoing progress record to complete", func() bool {
		return f.downloadProgressState() == progressCompleted
	})
	f.assertClean()
}

// TestBidirectionalPollingDuringOverlap runs both progress endpoints
// concurrently from both directions while both streams are live, and proves
// the polling neither blocks nor disturbs either transfer.
func TestBidirectionalPollingDuringOverlap(t *testing.T) {
	f := newOverlapFixture(t, overlapItemSize)

	f.startUpload()
	f.startDownload(context.Background())

	// Drain the outgoing body off the test goroutine so the archive stream
	// keeps moving while the polls run: a live stream is what they must
	// tolerate.
	drained := make(chan struct {
		n   int64
		err error
	}, 1)
	go func() {
		n, err := io.Copy(io.Discard, f.dl.Body)
		_ = f.dl.Body.Close()
		drained <- struct {
			n   int64
			err error
		}{n, err}
	}()

	const rounds = 12
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if rec := f.do(http.MethodGet, "/api/batches/"+f.batch.ID, f.tok, nil); rec.Code != http.StatusOK {
				t.Errorf("incoming batch status poll = %d", rec.Code)
			}
		}()
		go func() {
			defer wg.Done()
			if rec := f.do(http.MethodGet, "/api/download-progress/"+f.grant.ProgressID, f.tok, nil); rec.Code != http.StatusOK {
				t.Errorf("outgoing progress poll = %d", rec.Code)
			}
		}()
	}
	wg.Wait()

	// The noise left both transfers exactly as they were.
	if n := f.activeSession.PinnedTransfers(); n != 2 {
		t.Fatalf("pins after concurrent polling = %d, want 2", n)
	}
	if v := f.batchView(); v.Items[0].State != transfer.ItemStateTransferring {
		t.Fatalf("item 0 = %q after concurrent polling, want transferring", v.Items[0].State)
	}
	if rec, ok := f.downloadRecord(); !ok || !isLiveDownload(rec.State) {
		t.Fatalf("outgoing record = %+v after concurrent polling, want a live stream", rec)
	}

	got := <-drained
	if got.err != nil {
		t.Fatalf("the drained outgoing stream failed: %v", got.err)
	}
	if got.n < overlapArchiveSize {
		t.Fatalf("the drained archive is %d bytes, want at least %d", got.n, overlapArchiveSize)
	}
	waitFor(t, "the outgoing progress record to complete", func() bool {
		return f.downloadProgressState() == progressCompleted
	})
	if r := f.finishUpload(); r.err != nil || r.code != http.StatusOK {
		t.Fatalf("the upload after concurrent polling = %+v", r)
	}
	f.assertBatchCompleted()
	f.assertClean()
}

// TestRevokingTheSessionEndsBothDirections applies exactly what App.EndSession
// does - cancel the session's uploads, close its queue, revoke the token - and
// proves both live directions terminate deliberately rather than one of them
// quietly declaring success or the connection being sealed as a complete
// archive.
func TestRevokingTheSessionEndsBothDirections(t *testing.T) {
	f := newOverlapFixture(t, overlapItemSize)

	f.startUpload()
	f.startDownload(context.Background())

	// The desktop ends the session: its records are cancelled, its queue is
	// closed, and the token dies.
	f.trans.CancelSession(f.key)
	_ = f.coord.CloseSession(f.key)
	f.sess.RevokeSession(f.activeSession)

	// The incoming stream is driven so it observes the abort.
	if r := f.abortUpload(); r.err == nil && r.code == http.StatusOK {
		t.Fatalf("a revoked session's upload reported success: %+v", r)
	}
	waitFor(t, "the incoming record to end canceled", func() bool {
		u, ok := f.trans.Get(f.contentID)
		return ok && u.State == transfer.StateCanceled
	})

	// The outgoing stream notices the revocation on its next read and the
	// connection is torn down instead of being sealed by a ZIP central
	// directory that would make the truncated body look complete.
	if _, err := io.Copy(io.Discard, f.dl.Body); err == nil {
		t.Fatal("the outgoing stream finished cleanly although its session was revoked")
	}
	_ = f.dl.Body.Close()
	waitFor(t, "the outgoing progress record to end failed", func() bool {
		return f.downloadProgressState() == progressFailed
	})
	p, ok := f.srv.grants.getProgress(f.grant.ProgressID, f.key)
	if !ok || p.Error != "the session ended during the download" {
		// Only the record's own fields are printed: the progress value embeds
		// a mutex, so it is never passed to a formatting call as a whole.
		t.Fatalf("outgoing failure = found:%v state:%q error:%q, want the session-ended reason", ok, p.State, p.Error)
	}

	// No approved record lingers, no lease survives, no temporary file is
	// left, and the token is genuinely dead.
	if rec := f.do(http.MethodGet, "/api/batches/"+f.batch.ID, f.tok, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked token = %d, want 401", rec.Code)
	}
	f.assertBatchCanceled()
	f.assertClean()
}

// TestRepeatedOverlapLeavesNoTransferStateBehind runs the overlap three times
// in one long-lived session and asserts after each cycle that no pin, lease,
// grant slot, temporary receive file or mid-transfer record accumulated.
func TestRepeatedOverlapLeavesNoTransferStateBehind(t *testing.T) {
	f := newOverlapFixture(t, overlapItemSize)

	for cycle := 0; cycle < 3; cycle++ {
		if cycle > 0 {
			f.arm(overlapItemSize)
		}
		f.startUpload()
		f.startDownload(context.Background())
		if n := f.activeSession.PinnedTransfers(); n != 2 {
			t.Fatalf("cycle %d: pins during the overlap = %d, want 2", cycle, n)
		}

		wantEntry := overlapArchiveRoot + "/movie.bin"
		sizes := f.finishDownload(nil)
		if len(sizes) != 1 || sizes[wantEntry] != overlapArchiveSize {
			t.Fatalf("cycle %d: downloaded archive entries = %v", cycle, sizes)
		}
		if r := f.finishUpload(); r.err != nil || r.code != http.StatusOK {
			t.Fatalf("cycle %d: the upload = %+v", cycle, r)
		}
		f.assertBatchCompleted()
		f.assertClean()
	}

	// Three cycles published exactly three files and left nothing partial.
	names := receiveDirNames(t, f.store)
	if len(names) != 3 {
		t.Fatalf("receive directory holds %v, want exactly the three published items", names)
	}
	for _, name := range names {
		if strings.Contains(name, ".part.") {
			t.Fatalf("an unpublished temporary file survived: %q", name)
		}
	}
}
