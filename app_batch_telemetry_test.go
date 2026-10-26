package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// The desktop's Svelte components dereference .length, .filter and .some on
// the batch projections after assigning them straight from the bindings, so a
// nil Go slice — which crosses the binding as JSON null — would abort every
// render flush and freeze the panel numbers mid-transfer. The contract is
// therefore literally "an empty listing marshals as []", not merely "is empty".
func TestBatchProjectionsMarshalAsEmptyArraysWhenEmpty(t *testing.T) {
	h := newBatchHarness(t)

	before, err := json.Marshal(h.a.ListBatches())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != "[]" {
		t.Fatalf("ListBatches with no batches marshals as %s, want []", before)
	}
	pendingBefore, err := json.Marshal(h.a.ListPendingBatches())
	if err != nil {
		t.Fatal(err)
	}
	if string(pendingBefore) != "[]" {
		t.Fatalf("ListPendingBatches with no batches marshals as %s, want []", pendingBefore)
	}

	// The contract holds while a batch is pending and after it is decided.
	b := h.createBatch("a.txt")
	if got := h.a.ListPendingBatches(); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("pending batches = %v, want [%s]", got, b.ID)
	}
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatal(err)
	}
	// Stream its single item to completion (4-byte file, exactly one read).
	if rec := h.put(b.ID, b.Items[0].ID, []byte("aaaa")); rec.Code != http.StatusOK {
		t.Fatalf("item upload = %d %s", rec.Code, rec.Body.String())
	}
	pendingAfter, err := json.Marshal(h.a.ListPendingBatches())
	if err != nil {
		t.Fatal(err)
	}
	if string(pendingAfter) != "[]" {
		t.Fatalf("ListPendingBatches after the decision marshals as %s, want []", pendingAfter)
	}
	finished, err := json.Marshal(h.a.ListBatches())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(finished, []byte(`"state":"completed"`)) {
		t.Fatalf("ListBatches after completion = %s, want the finished batch", finished)
	}
}

// testItemSize makes each item exactly 4 MiB, so streaming through the real
// handler passes the 1 MiB progressStep four times: the desktop projection
// can be observed at 25%, 50%, 75% and at completion.
const testItemSize = 4 * 1024 * 1024

func createBatchSized(h *batchHarness, names []string, size int64) transfer.BatchView {
	h.t.Helper()
	items := make([]transfer.ManifestItem, len(names))
	for i, n := range names {
		items[i] = transfer.ManifestItem{RelativePath: n, Size: size}
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
	return v
}

// gateReader streams exactly one 64 KiB read per release(), letting the test
// advance an item to an exact milestone and poll the desktop projection while
// the HTTP stream is still in flight. Streaming runs on a real goroutine
// through the real PUT handler in the background.
type gateReader struct {
	h           *batchHarness
	batchID     string
	itemID      string
	mu          sync.Mutex
	total       int64 // approved size in bytes
	released    int64 // cumulative bytes granted to the stream
	outstanding int64 // granted but not yet read bytes
	gate        chan struct{}
	code        chan int
	started     bool
}

func (h *batchHarness) gatedItem(b transfer.BatchView, item int) *gateReader {
	return &gateReader{
		h:       h,
		batchID: b.ID,
		itemID:  b.Items[item].ID,
		total:   b.Items[item].DeclaredSize,
		gate:    make(chan struct{}),
		code:    make(chan int, 1),
	}
}

// release grants up to one 64 KiB read (clamped to what is left of the
// approved size). streamBody reads in 64 KiB chunks, so one grant per chunk
// keeps the accounting exact. The grant count is capped at the approved size
// here, at grant time, so no token is ever sent whose read will not happen:
// such a token would deadlock its sender after the handler exits with the
// stream complete.
func (g *gateReader) release() {
	g.mu.Lock()
	if g.released >= g.total {
		g.mu.Unlock()
		return
	}
	chunk := int64(65536)
	if rest := g.total - g.released; chunk > rest {
		chunk = rest
	}
	g.released += chunk
	g.outstanding += chunk
	g.mu.Unlock()
	g.gate <- struct{}{}
}

func (g *gateReader) releaseAll() {
	for {
		g.mu.Lock()
		done := g.released >= g.total
		g.mu.Unlock()
		if done {
			return
		}
		g.release()
	}
}

// Read hands out exactly the bytes release() has granted. Blocks on the gate
// between grants, which is what stalls the stream mid-item.
func (g *gateReader) Read(p []byte) (int, error) {
	<-g.gate
	g.mu.Lock()
	n := len(p)
	if int64(n) > g.outstanding {
		n = int(g.outstanding)
	}
	g.outstanding -= int64(n)
	g.mu.Unlock()
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	return n, nil
}

func (g *gateReader) start() {
	if g.started {
		return
	}
	g.started = true
	go func() {
		req := httptest.NewRequest(http.MethodPut, "/api/batches/"+g.batchID+"/items/"+g.itemID+"/content", g)
		req.Header.Set("Authorization", "Bearer "+g.h.token)
		rec := httptest.NewRecorder()
		g.h.handler.ServeHTTP(rec, req)
		g.code <- rec.Code
	}()
}

// waitMilestone polls the desktop projection until the named batch shows at
// least want transferred bytes, then returns every polled view.
func (h *batchHarness) waitMilestone(batchID string, want int64) []BatchView {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		views := h.a.ListBatches()
		for _, v := range views {
			if v.ID == batchID && v.TransferredBytes >= want {
				return views
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("batch %s never reached %d transferred bytes; last listing = %+v", batchID, want, h.a.ListBatches())
		}
		time.Sleep(time.Millisecond)
	}
}

func findBatch(views []BatchView, id string) BatchView {
	for _, v := range views {
		if v.ID == id {
			return v
		}
	}
	return BatchView{}
}

func doneItems(v BatchView) int {
	n := 0
	for _, item := range v.Items {
		if item.State == "completed" {
			n++
		}
	}
	return n
}

// TestDesktopProjectionFollowsLiveBatchItemProgress streams a two-item batch
// item by item and checks, at every milestone, exactly what the desktop
// projection reports: the active batch, item bytes, the aggregate, the
// completed item kept when the next item starts, and the final move into
// history with no duplicate or reset across repeated desktop polls.
func TestDesktopProjectionFollowsLiveBatchItemProgress(t *testing.T) {
	h := newBatchHarness(t)
	b := createBatchSized(h, []string{"big1.dat", "big2.dat"}, testItemSize)

	listed, err := json.Marshal(h.a.ListBatches())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(listed, []byte(`"state":"pending"`)) {
		t.Fatalf("declared batch must be visible in the desktop projection: %s", listed)
	}
	pendingCount := len(h.a.ListPendingBatches())
	if pendingCount != 1 {
		t.Fatalf("pending projection = %d, want the declared batch", pendingCount)
	}

	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatal(err)
	}
	// Approved and queued: visible as active, not pending, not history.
	views := h.a.ListBatches()
	approved := findBatch(views, b.ID)
	if approved.State != "queued" || approved.ApprovalState != "approved" {
		t.Fatalf("approved batch in the projection = %+v", approved)
	}
	if len(h.a.ListPendingBatches()) != 0 {
		t.Fatalf("approved batch still in the approval projection")
	}

	// Item 1: stream to each 1 MiB milestone (one quarter of the item).
	g0 := h.gatedItem(b, 0)
	g0.start()
	for _, quarter := range []int64{1, 2, 3} {
		for i := 0; i < 16; i++ {
			g0.release()
		}
		views = h.waitMilestone(b.ID, quarter*1024*1024)
		live := findBatch(views, b.ID)
		if live.State != "transferring" {
			t.Fatalf("batch state at %d MiB = %q, want transferring", quarter, live.State)
		}
		if item := live.Items[0]; item.ID != b.Items[0].ID || item.State != "transferring" {
			t.Fatalf("item 0 at %d MiB = %s %s, want streaming", quarter, item.ID, item.State)
		}
		if len(h.a.ListPendingBatches()) != 0 {
			t.Fatal("approved batch appeared in the approval projection mid-stream")
		}
	}
	for i := 0; i < 16; i++ {
		g0.release()
	}
	if code := <-g0.code; code != http.StatusOK {
		t.Fatalf("item 0 upload = %d", code)
	}

	// Item 1 finished: bytes are exact, the item stays completed and the
	// batch is still active while item 2 waits.
	views = h.waitMilestone(b.ID, testItemSize)
	live := findBatch(views, b.ID)
	if live.TransferredBytes != testItemSize || live.Items[0].Transferred != testItemSize {
		t.Fatalf("aggregate = %d, item 0 = %d, both want %d", live.TransferredBytes, live.Items[0].Transferred, testItemSize)
	}
	if live.Items[0].State != "completed" {
		t.Fatalf("item 0 state = %q, want completed", live.Items[0].State)
	}
	if live.State != "queued" && live.State != "transferring" {
		t.Fatalf("batch state after item 0 = %q, want still active", live.State)
	}

	// Item 2 becomes the active one without losing item 1's completion.
	g1 := h.gatedItem(b, 1)
	g1.start()
	for i := 0; i < 16; i++ {
		g1.release()
	}
	views = h.waitMilestone(b.ID, testItemSize+1024*1024)
	live = findBatch(views, b.ID)
	if live.Items[0].State != "completed" {
		t.Fatalf("item 0 lost its completion while item 1 started")
	}
	if item := live.Items[1]; item.State != "transferring" {
		t.Fatalf("item 1 state = %q, want the active item", item.State)
	}
	if doneItems(live) != 1 {
		t.Fatalf("completed items = %d, want 1 while item 1 streams", doneItems(live))
	}
	g1.releaseAll()
	if code := <-g1.code; code != http.StatusOK {
		t.Fatalf("item 1 upload = %d", code)
	}

	// Completion: exact totals, correct terminal state, out of the active
	// count, retained as history bytes the UI expects.
	deadline := time.Now().Add(10 * time.Second)
	for {
		live = findBatch(h.a.ListBatches(), b.ID)
		if live.State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch never completed; last view = %+v", live)
		}
		time.Sleep(time.Millisecond)
	}
	if live.TransferredBytes != 2*testItemSize || live.DeclaredTotalBytes != 2*testItemSize {
		t.Fatalf("completed batch totals = %d/%d, want %d", live.TransferredBytes, live.DeclaredTotalBytes, 2*testItemSize)
	}
	if doneItems(live) != 2 {
		t.Fatalf("completed items = %d, want 2", doneItems(live))
	}
	if live.ApprovalState == "approved" && (live.State == "queued" || live.State == "transferring") {
		t.Fatal("completed batch still counts as an active transfer")
	}
	if got := h.a.ListPendingBatches(); len(got) != 0 {
		t.Fatalf("completed batch leaked into the approval projection: %+v", got)
	}

	// Repeated desktop polls: one record per batch, progress never resets,
	// listing stays deterministic.
	first := h.a.ListBatches()
	if len(first) != 1 {
		t.Fatalf("listing = %d batches, want 1", len(first))
	}
	for i := 0; i < 20; i++ {
		again := findBatch(h.a.ListBatches(), b.ID)
		if again.State != "completed" || again.TransferredBytes != 2*testItemSize {
			t.Fatalf("poll %d changed or reset the record: %+v", i, again)
		}
		if doneItems(again) != 2 {
			t.Fatalf("poll %d lost completed items", i)
		}
		if again.ID != first[0].ID {
			t.Fatalf("poll %d reordered the listing", i)
		}
	}
}

// TestRepeatCleanupAfterCompletionKeepsTerminalState confirms that repeated
// cleanup conditions the operator could trigger after completion never turn a
// finished batch into a failed or active one.
func TestRepeatCleanupAfterCompletionKeepsTerminalState(t *testing.T) {
	h := newBatchHarness(t)
	b := createBatchSized(h, []string{"a.dat"}, testItemSize)
	if _, err := h.a.ApproveBatch(b.ID); err != nil {
		t.Fatal(err)
	}
	g := h.gatedItem(b, 0)
	g.start()
	g.releaseAll()
	if code := <-g.code; code != http.StatusOK {
		t.Fatalf("item upload = %d", code)
	}
	before := findBatch(h.a.ListBatches(), b.ID)
	if before.State != "completed" || doneItems(before) != 1 {
		t.Fatalf("completed batch = %+v", before)
	}
	// A stale cleanup pass arriving after completion.
	h.a.releaseSession(h.sess.Key())
	h.a.releaseSession(h.sess.Key())
	after := findBatch(h.a.ListBatches(), b.ID)
	if after.State != "completed" || after.ApprovalState != "approved" || after.TransferredBytes != testItemSize {
		t.Fatalf("cleanup turned a completed batch into %+v", after)
	}
	if got := h.a.ListPendingBatches(); len(got) != 0 {
		t.Fatalf("completed batch leaks into the approval projection: %+v", got)
	}
}
