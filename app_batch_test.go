package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// batchHarness drives a real App (coordinator, upload service, session
// manager) through the same HTTP handlers the phone uses, so approval tests
// exercise exactly the production wiring.
type batchHarness struct {
	t       *testing.T
	a       *App
	handler http.Handler
	sess    *session.Session
	token   string
	ids     map[string][]string // batch ID -> upload record IDs, captured at creation
}

func newBatchHarness(t *testing.T) *batchHarness {
	t.Helper()
	a := newTestApp(t)
	a.store = mustStore(t)
	a.transfers = transfer.NewService(transfer.Config{Store: a.store})
	srv := server.New(server.Config{Services: &server.Services{
		Sessions: a.sessions, Transfers: a.transfers, Coordinator: a.coordinator,
		Registry: a.registry, Downloads: a.downloads,
	}})
	s, err := a.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	return &batchHarness{t: t, a: a, handler: srv.Handler(), sess: s, token: s.Token(), ids: map[string][]string{}}
}

func mustStore(t *testing.T) *transfer.Store {
	t.Helper()
	st, err := transfer.NewStore(transfer.StoreConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (h *batchHarness) do(method, path string, body []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// createBatch declares a flat batch of 4-byte files and returns its view.
func (h *batchHarness) createBatch(names ...string) transfer.BatchView {
	h.t.Helper()
	items := make([]transfer.ManifestItem, len(names))
	for i, n := range names {
		items[i] = transfer.ManifestItem{RelativePath: n, Size: 4}
	}
	body, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop, Kind: transfer.BatchKindFiles, Items: items,
	})
	rec := h.do(http.MethodPost, "/api/batches", body)
	if rec.Code != http.StatusCreated {
		h.t.Fatalf("create batch = %d %s", rec.Code, rec.Body.String())
	}
	var v transfer.BatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		h.t.Fatal(err)
	}
	for _, item := range v.Items {
		id, err := h.a.coordinator.ItemContentID(h.sess.Key(), v.ID, item.ID)
		if err != nil {
			h.t.Fatal(err)
		}
		h.ids[v.ID] = append(h.ids[v.ID], id)
	}
	return v
}

func (h *batchHarness) put(batchID, itemID string, data []byte) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(http.MethodPut, "/api/batches/"+batchID+"/items/"+itemID+"/content", data)
}

func (h *batchHarness) view(id string) transfer.BatchView {
	h.t.Helper()
	v, err := h.a.coordinator.GetBatch(h.sess.Key(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func (h *batchHarness) contentID(b transfer.BatchView, item int) string {
	h.t.Helper()
	return h.ids[b.ID][item]
}

// assertCoherent proves the coordinator and the upload service agree on
// every item: no item is approved in one subsystem and pending, rejected or
// canceled in the other.
func (h *batchHarness) assertCoherent(b transfer.BatchView) {
	h.t.Helper()
	v := h.view(b.ID)
	for i, item := range v.Items {
		up, ok := h.a.transfers.Get(h.contentID(b, i))
		if !ok {
			h.t.Fatalf("item %d has no upload record", i)
		}
		var want []transfer.State
		switch item.State {
		case transfer.ItemStatePending:
			want = []transfer.State{transfer.StatePending}
		case transfer.ItemStateQueued, transfer.ItemStateApproved:
			want = []transfer.State{transfer.StateApproved}
		case transfer.ItemStateTransferring:
			want = []transfer.State{transfer.StateUploading}
		case transfer.ItemStateCompleted:
			want = []transfer.State{transfer.StateCompleted}
		case transfer.ItemStateRejected:
			// A record already approved cannot be "rejected" by the upload
			// state machine; withdrawing it ends canceled.
			want = []transfer.State{transfer.StateRejected, transfer.StateCanceled}
		case transfer.ItemStateCanceled:
			want = []transfer.State{transfer.StateCanceled, transfer.StateRejected}
		case transfer.ItemStateFailed:
			want = []transfer.State{transfer.StateFailed}
		}
		match := false
		for _, w := range want {
			match = match || up.State == w
		}
		if !match {
			h.t.Fatalf("item %d: coordinator=%q but upload service=%q (want one of %v)", i, item.State, up.State, want)
		}
	}
}

func pendingBatchIDs(a *App) []string {
	var ids []string
	for _, b := range a.ListPendingBatches() {
		ids = append(ids, b.ID)
	}
	return ids
}

func TestApproveBatchOnceThenUploadEveryItemToCompletion(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("a.txt", "b.txt", "c.txt", "d.txt")
	if got := pendingBatchIDs(h.a); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("pending batches = %v, want [%s]", got, b.ID)
	}
	v, err := h.a.ApproveBatch(b.ID)
	if err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}
	if v.ApprovalState != "approved" || v.State != "queued" {
		t.Fatalf("approved view = %+v", v)
	}
	h.assertCoherent(b)
	if got := pendingBatchIDs(h.a); len(got) != 0 {
		t.Fatalf("approved batch still pending: %v", got)
	}

	for i, item := range b.Items {
		rec := h.put(b.ID, item.ID, []byte(fmt.Sprintf("d%03d", i)))
		if rec.Code != http.StatusOK {
			t.Fatalf("item %d upload = %d %s", i, rec.Code, rec.Body.String())
		}
		h.assertCoherent(b)
	}
	final := h.view(b.ID)
	if final.State != "completed" || final.TransferredBytes != 16 {
		t.Fatalf("final batch = %+v", final)
	}
	for _, item := range final.Items {
		if item.State != "completed" {
			t.Fatalf("item %s = %q", item.ID, item.State)
		}
	}
	if got := pendingBatchIDs(h.a); len(got) != 0 {
		t.Fatalf("completed batch listed as pending: %v", got)
	}
	// The completed batch is reported complete, never pending or active.
	for _, listed := range h.a.ListBatches() {
		if listed.ID == b.ID && (listed.State != "completed" || listed.ApprovalState != "approved") {
			t.Fatalf("listed batch = %+v", listed)
		}
	}
}

func TestRepeatedApprovalIsIdempotent(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("a.txt", "b.txt")
	first, err := h.a.ApproveBatch(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.a.ApproveBatch(b.ID)
	if err != nil {
		t.Fatalf("second approval must be an idempotent success, got %v", err)
	}
	if second.ApprovalState != "approved" || second.State != first.State {
		t.Fatalf("second view = %+v, first = %+v", second, first)
	}
	h.assertCoherent(b)

	// It stays idempotent while the batch progresses and after completion.
	if rec := h.put(b.ID, b.Items[0].ID, []byte("aaaa")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatalf("approval mid-batch = %v", err)
	}
	if rec := h.put(b.ID, b.Items[1].ID, []byte("bbbb")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if v, err := h.a.ApproveBatch(b.ID); err != nil || v.State != "completed" {
		t.Fatalf("approval after completion = %+v, %v", v, err)
	}
	h.assertCoherent(b)
}

func TestConcurrentApprovalsAreDeterministic(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("a.txt", "b.txt", "c.txt")
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := h.a.ApproveBatch(b.ID)
			if err == nil && v.ApprovalState != "approved" {
				err = fmt.Errorf("returned view not approved: %+v", v)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent approval failed instead of being idempotent: %v", err)
		}
	}
	h.assertCoherent(b)
	for _, item := range h.view(b.ID).Items {
		if item.State != "queued" {
			t.Fatalf("item state = %q, want queued exactly once", item.State)
		}
	}
	// The queue really starts progressing.
	if rec := h.put(b.ID, b.Items[0].ID, []byte("aaaa")); rec.Code != http.StatusOK {
		t.Fatalf("first upload after concurrent approval = %d %s", rec.Code, rec.Body.String())
	}
}

func TestConcurrentApproveAndRejectEndInOneCoherentState(t *testing.T) {
	for round := 0; round < 20; round++ {
		h := newBatchHarness(t)
		b := h.createBatch("a.txt", "b.txt")
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, fn := range []func(){
			func() { _, _ = h.a.ApproveBatch(b.ID) },
			func() { _, _ = h.a.RejectBatch(b.ID) },
			func() { _ = h.a.CancelBatch(b.ID) },
		} {
			wg.Add(1)
			go func(fn func()) { defer wg.Done(); <-start; fn() }(fn)
		}
		close(start)
		wg.Wait()
		h.assertCoherent(b)
		if got := pendingBatchIDs(h.a); len(got) != 0 {
			t.Fatalf("round %d: decided batch still pending: %v", round, got)
		}
	}
}

func TestApprovalAfterRejectionOrCancellationIsRefused(t *testing.T) {
	h := newBatchHarness(t)

	rejected := h.createBatch("r1.txt", "r2.txt")
	if _, err := h.a.RejectBatch(rejected.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.ApproveBatch(rejected.ID); !errors.Is(err, transfer.ErrInvalidTransition) {
		t.Fatalf("approve after reject = %v, want ErrInvalidTransition", err)
	}
	h.assertCoherent(rejected)
	if v := h.view(rejected.ID); v.State != "rejected" {
		t.Fatalf("rejected batch changed to %+v", v)
	}

	canceled := h.createBatch("c1.txt", "c2.txt")
	if err := h.a.CancelBatch(canceled.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.ApproveBatch(canceled.ID); !errors.Is(err, transfer.ErrInvalidTransition) {
		t.Fatalf("approve after cancel = %v, want ErrInvalidTransition", err)
	}
	h.assertCoherent(canceled)

	// Cancelling an approved batch is terminal too: re-approval is a conflict,
	// not a silent success.
	approvedThenCanceled := h.createBatch("x1.txt")
	if _, err := h.a.ApproveBatch(approvedThenCanceled.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.a.CancelBatch(approvedThenCanceled.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.ApproveBatch(approvedThenCanceled.ID); !errors.Is(err, transfer.ErrInvalidTransition) {
		t.Fatalf("approve after approve+cancel = %v, want ErrInvalidTransition", err)
	}
	h.assertCoherent(approvedThenCanceled)
	// Neither upload may start on a canceled batch.
	if rec := h.put(approvedThenCanceled.ID, approvedThenCanceled.Items[0].ID, []byte("xxxx")); rec.Code == http.StatusOK {
		t.Fatal("content accepted for a canceled batch")
	}
}

func TestRejectAndCancelMultiItemBatches(t *testing.T) {
	h := newBatchHarness(t)
	rej := h.createBatch("a.txt", "b.txt", "c.txt")
	v, err := h.a.RejectBatch(rej.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "rejected" || v.ApprovalState != "rejected" {
		t.Fatalf("rejected view = %+v", v)
	}
	for i := range rej.Items {
		if up, _ := h.a.transfers.Get(h.contentID(rej, i)); up.State != transfer.StateRejected {
			t.Fatalf("upload %d = %q, want rejected", i, up.State)
		}
	}
	if again, err := h.a.RejectBatch(rej.ID); err != nil || again.State != "rejected" {
		t.Fatalf("repeat reject = %+v, %v", again, err)
	}

	can := h.createBatch("a.txt", "b.txt", "c.txt")
	if err := h.a.CancelBatch(can.ID); err != nil {
		t.Fatal(err)
	}
	for i := range can.Items {
		if up, _ := h.a.transfers.Get(h.contentID(can, i)); up.State != transfer.StateCanceled {
			t.Fatalf("upload %d = %q, want canceled", i, up.State)
		}
	}
	if err := h.a.CancelBatch(can.ID); !errors.Is(err, transfer.ErrInvalidTransition) {
		t.Fatalf("second cancel = %v, want ErrInvalidTransition", err)
	}
	if _, err := h.a.RejectBatch(can.ID); !errors.Is(err, transfer.ErrInvalidTransition) {
		t.Fatalf("reject after cancel = %v, want ErrInvalidTransition", err)
	}
	// Rejecting or cancelling an approved batch also stops every record.
	appr := h.createBatch("a.txt", "b.txt")
	if _, err := h.a.ApproveBatch(appr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.RejectBatch(appr.ID); err != nil {
		t.Fatal(err)
	}
	h.assertCoherent(appr)
	for i := range appr.Items {
		if up, _ := h.a.transfers.Get(h.contentID(appr, i)); up.State == transfer.StateApproved {
			t.Fatalf("approved record %d survived rejecting its batch", i)
		}
	}
	if got := pendingBatchIDs(h.a); len(got) != 0 {
		t.Fatalf("pending projection = %v, want empty", got)
	}
}

func TestStandaloneLegacyApprovalStillWorks(t *testing.T) {
	h := newBatchHarness(t)
	rec := h.do(http.MethodPost, "/api/uploads", []byte(`{"name":"legacy.txt","size":6}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("declare = %d %s", rec.Code, rec.Body.String())
	}
	var up struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}
	pending := h.a.ListPendingUploads()
	if len(pending) != 1 || pending[0].ID != up.ID {
		t.Fatalf("pending uploads = %+v, want the standalone request", pending)
	}
	if _, err := h.a.ApproveUpload(up.ID); err != nil {
		t.Fatalf("legacy approve: %v", err)
	}
	if rec := h.do(http.MethodPut, "/api/uploads/"+up.ID+"/content", []byte("legacy")); rec.Code != http.StatusOK {
		t.Fatalf("legacy content = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.a.ApproveUpload(up.ID); err == nil {
		t.Fatal("legacy double approval must still be refused")
	}
	var found bool
	for _, u := range h.a.RecentUploads() {
		if u.ID == up.ID {
			found = u.State == "completed"
		}
	}
	if !found {
		t.Fatal("completed standalone upload missing from RecentUploads")
	}

	// And legacy rejection of another standalone request.
	rec = h.do(http.MethodPost, "/api/uploads", []byte(`{"name":"nope.txt","size":1}`))
	var second struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if v, err := h.a.RejectUpload(second.ID); err != nil || v.State != "rejected" {
		t.Fatalf("legacy reject = %+v, %v", v, err)
	}
}

func TestBatchItemsAreNeverIndependentApprovalRequests(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("a.txt", "b.txt", "c.txt", "d.txt")

	if got := h.a.ListPendingUploads(); len(got) != 0 {
		t.Fatalf("batch items surfaced as standalone requests: %+v", got)
	}
	if got := h.a.RecentUploads(); len(got) != 0 {
		t.Fatalf("batch items surfaced as standalone uploads: %+v", got)
	}
	contentID := h.contentID(b, 0)
	if _, err := h.a.ApproveUpload(contentID); err == nil || !strings.Contains(err.Error(), "batch") {
		t.Fatalf("ApproveUpload on a batch item = %v, want a batch-owned refusal", err)
	}
	if _, err := h.a.RejectUpload(contentID); err == nil {
		t.Fatal("RejectUpload on a batch item must be refused")
	}
	if err := h.a.CancelUpload(contentID); err == nil {
		t.Fatal("CancelUpload on a batch item must be refused")
	}
	h.assertCoherent(b)
	if up, _ := h.a.transfers.Get(contentID); up.State != transfer.StatePending {
		t.Fatalf("refused individual decisions changed the record to %q", up.State)
	}

	// After the batch decision there is still only one authority and one
	// consistent view, and no "waiting for phone" duplicate record appears.
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.a.RecentUploads(); len(got) != 0 {
		t.Fatalf("approved batch items leaked into RecentUploads: %+v", got)
	}
	var active int
	for _, listed := range h.a.ListBatches() {
		if listed.ApprovalState == "approved" && (listed.State == "queued" || listed.State == "transferring") {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active batches = %d, want 1", active)
	}
}

func TestFailedApprovalLeavesNoContradictoryState(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("a.txt", "b.txt", "c.txt")
	// One record is withdrawn behind the batch's back (as a session cleanup
	// racing with the decision could do).
	if err := h.a.transfers.Cancel(h.contentID(b, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.ApproveBatch(b.ID); err == nil {
		t.Fatal("approval of a batch with a withdrawn record must fail")
	}
	h.assertCoherent(b)
	for i := range b.Items {
		up, _ := h.a.transfers.Get(h.contentID(b, i))
		if up.State == transfer.StateApproved {
			t.Fatalf("record %d was left approved by a failed approval", i)
		}
	}
	if v := h.view(b.ID); v.State == "queued" || v.State == "transferring" {
		t.Fatalf("batch %q is live although its approval failed", v.State)
	}
	if got := pendingBatchIDs(h.a); len(got) != 0 {
		t.Fatalf("failed batch stays in the pending inbox: %v", got)
	}
	// Nothing can be uploaded for it either.
	if rec := h.put(b.ID, b.Items[0].ID, []byte("aaaa")); rec.Code == http.StatusOK {
		t.Fatal("content accepted for a batch whose approval failed")
	}
}

func TestApprovalWhileSessionClosedDoesNotLeaveApprovedRecords(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("a.txt", "b.txt")
	if err := h.a.coordinator.CloseSession(h.sess.Key()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.ApproveBatch(b.ID); err == nil {
		t.Fatal("approval on a closed session must fail")
	}
	for i := range b.Items {
		if up, _ := h.a.transfers.Get(h.contentID(b, i)); up.State == transfer.StateApproved {
			t.Fatalf("record %d approved although the coordinator refused", i)
		}
	}
}

func TestBatchHistoryAndActiveCountsUseAuthoritativeStates(t *testing.T) {
	h := newBatchHarness(t)
	done := h.createBatch("a.txt")
	if _, err := h.a.ApproveBatch(done.ID); err != nil {
		t.Fatal(err)
	}
	if rec := h.put(done.ID, done.Items[0].ID, []byte("aaaa")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	live := h.createBatch("b.txt")
	if _, err := h.a.ApproveBatch(live.ID); err != nil {
		t.Fatal(err)
	}
	pend := h.createBatch("p.txt")
	rej := h.createBatch("r.txt")
	if _, err := h.a.RejectBatch(rej.ID); err != nil {
		t.Fatal(err)
	}
	can := h.createBatch("c.txt")
	if err := h.a.CancelBatch(can.ID); err != nil {
		t.Fatal(err)
	}

	pending := pendingBatchIDs(h.a)
	if len(pending) != 1 || pending[0] != pend.ID {
		t.Fatalf("pending projection = %v, want only [%s]", pending, pend.ID)
	}
	byID := map[string]BatchView{}
	for _, v := range h.a.ListBatches() {
		byID[v.ID] = v
	}
	for id, want := range map[string]string{
		done.ID: "completed", live.ID: "queued", pend.ID: "pending", rej.ID: "rejected", can.ID: "canceled",
	} {
		if byID[id].State != want {
			t.Fatalf("batch %s state = %q, want %q", id, byID[id].State, want)
		}
	}
}

func TestIdleSessionExpiryReleasesItsBatchesAndUploads(t *testing.T) {
	a := newTestApp(t)
	a.store = mustStore(t)
	a.transfers = transfer.NewService(transfer.Config{Store: a.store})
	var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	a.sessions = session.NewManager(session.Config{IdleTimeout: time.Minute, Now: clock, OnExpire: a.releaseSession})
	srv := server.New(server.Config{Services: &server.Services{
		Sessions: a.sessions, Transfers: a.transfers, Coordinator: a.coordinator,
		Registry: a.registry, Downloads: a.downloads,
	}})
	s, err := a.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	h := &batchHarness{t: t, a: a, handler: srv.Handler(), sess: s, token: s.Token(), ids: map[string][]string{}}
	pending := h.createBatch("p.txt")
	approved := h.createBatch("q.txt")
	if _, err := a.ApproveBatch(approved.ID); err != nil {
		t.Fatal(err)
	}
	rec := h.do(http.MethodPost, "/api/uploads", []byte(`{"name":"solo.txt","size":1}`))
	var solo struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &solo)

	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if n := a.sessions.Len(); n != 0 {
		t.Fatalf("expired session still listed: %d", n)
	}
	if up, _ := a.transfers.Get(solo.ID); up.State != transfer.StateCanceled {
		t.Fatalf("pending standalone upload = %q after session expiry, want canceled", up.State)
	}
	if up, _ := a.transfers.Get(h.contentID(pending, 0)); up.State != transfer.StateCanceled {
		t.Fatalf("pending batch record = %q after expiry", up.State)
	}
	if up, _ := a.transfers.Get(h.contentID(approved, 0)); up.State != transfer.StateCanceled {
		t.Fatalf("approved batch record = %q after expiry", up.State)
	}
	if got := a.ListPendingBatches(); len(got) != 0 {
		t.Fatalf("expired session's batches still pending: %+v", got)
	}
}

func TestClosedSessionBatchHistoryStaysVisibleToTheDesktop(t *testing.T) {
	a := newTestApp(t)
	a.store = mustStore(t)
	a.transfers = transfer.NewService(transfer.Config{Store: a.store})
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	a.sessions = session.NewManager(session.Config{
		IdleTimeout: time.Minute, Now: clock,
		OnExpire: a.releaseSession, OnEnd: a.logSessionEnd,
	})
	srv := server.New(server.Config{Services: &server.Services{
		Sessions: a.sessions, Transfers: a.transfers, Coordinator: a.coordinator,
		Registry: a.registry, Downloads: a.downloads,
	}})
	s, err := a.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	h := &batchHarness{t: t, a: a, handler: srv.Handler(), sess: s, token: s.Token(), ids: map[string][]string{}}
	done := h.createBatch("done1.txt", "done2.txt")
	if _, err := a.ApproveBatch(done.ID); err != nil {
		t.Fatal(err)
	}
	for i, item := range done.Items {
		if rec := h.put(done.ID, item.ID, []byte(fmt.Sprintf("d%03d", i))); rec.Code != http.StatusOK {
			t.Fatalf("item %d upload = %d", i, rec.Code)
		}
	}
	pending := h.createBatch("waiting.txt")

	// Idle expiry: the session ends, its cleanup runs, but its batch
	// history must stay visible to the desktop.
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if n := a.sessions.Len(); n != 0 {
		t.Fatalf("expired session still listed: %d", n)
	}

	views := a.ListBatches()
	byID := map[string]BatchView{}
	counts := map[string]int{}
	for _, v := range views {
		byID[v.ID] = v
		counts[v.ID]++
	}
	if len(counts) != 2 || counts[done.ID] != 1 || counts[pending.ID] != 1 {
		t.Fatalf("merged listing must show each closed-session batch exactly once: %v", counts)
	}
	if byID[done.ID].State != "completed" || byID[done.ID].ApprovalState != "approved" {
		t.Fatalf("completed batch after expiry = %+v", byID[done.ID])
	}
	if v := byID[pending.ID]; v.State != "canceled" || v.ApprovalState != "pending" {
		t.Fatalf("never-approved batch after expiry = %+v (canceled state, real approval state)", v)
	}
	if got := a.ListPendingBatches(); len(got) != 0 {
		t.Fatalf("closed session's history leaked into the pending inbox: %+v", got)
	}

	// The ordering must be deterministic across consecutive desktop polls.
	first, second := a.ListBatches(), a.ListBatches()
	if len(first) != 2 {
		t.Fatalf("listing = %d batches, want 2", len(first))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("consecutive desktop polls disagreed on order: [%s,%s] then [%s,%s]",
				first[0].ID, first[1].ID, second[0].ID, second[1].ID)
		}
	}

	// Decisions still fail closed on a session nobody can reach: neither
	// the finished batch nor the canceled one can be re-approved.
	if _, err := a.ApproveBatch(done.ID); err == nil {
		t.Fatal("ApproveBatch succeeded for a batch of an ended session")
	}
	if _, err := a.ApproveBatch(pending.ID); err == nil {
		t.Fatal("ApproveBatch succeeded for a batch of an ended session")
	}
}

func TestCompletedWorkIsNotFailedByLaterSessionCleanup(t *testing.T) {
	h := newBatchHarness(t)
	b := h.createBatch("a.txt", "b.txt")
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatal(err)
	}
	if rec := h.put(b.ID, b.Items[0].ID, []byte("aaaa")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := h.put(b.ID, b.Items[1].ID, []byte("bbbb")); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	h.a.releaseSession(h.sess.Key()) // stale cleanup arriving after completion
	h.a.releaseSession(h.sess.Key()) // and again: idempotent
	for i := range b.Items {
		if up, _ := h.a.transfers.Get(h.contentID(b, i)); up.State != transfer.StateCompleted {
			t.Fatalf("completed upload %d became %q after cleanup", i, up.State)
		}
	}
}

func TestSessionExpiryDuringABatchDecisionDoesNotDeadlock(t *testing.T) {
	a := newTestApp(t)
	a.store = mustStore(t)
	a.transfers = transfer.NewService(transfer.Config{Store: a.store})
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	a.sessions = session.NewManager(session.Config{IdleTimeout: time.Minute, Now: clock, OnExpire: a.releaseSession})
	srv := server.New(server.Config{Services: &server.Services{
		Sessions: a.sessions, Transfers: a.transfers, Coordinator: a.coordinator,
		Registry: a.registry, Downloads: a.downloads,
	}})
	owner, err := a.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	h := &batchHarness{t: t, a: a, handler: srv.Handler(), sess: owner, token: owner.Token(), ids: map[string][]string{}}
	batch := h.createBatch("a.txt")

	// A second session goes idle; the owner stays alive with a request.
	if _, err := a.sessions.Create(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(30 * time.Second)
	mu.Unlock()
	if rec := h.do(http.MethodGet, "/api/batches/"+batch.ID, nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	mu.Lock()
	now = now.Add(45 * time.Second) // the idle session is now stale, the owner is not
	mu.Unlock()

	done := make(chan error, 1)
	go func() {
		_, err := a.ApproveBatch(batch.ID) // prunes the stale session while deciding
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ApproveBatch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ApproveBatch deadlocked against session-expiry cleanup")
	}
	if a.sessions.Len() != 1 {
		t.Fatalf("sessions = %d, want only the owner", a.sessions.Len())
	}
}
