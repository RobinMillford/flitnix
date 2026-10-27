package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// cancelBatchItem issues the phone's withdrawal of one batch item.
func (e *testEnv) cancelBatchItem(tok, batchID, itemID string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(http.MethodDelete, "/api/batches/"+batchID+"/items/"+itemID, tok, nil)
}

// uploadBatchItem streams one approved item and returns the status plus the
// authoritative batch snapshot the handler answered with.
func (e *testEnv) uploadBatchItem(base, tok string, b transfer.BatchView, item int, data []byte) (int, transfer.BatchView) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPut, contentURL(base, b, item), bytes.NewReader(data))
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("item %d: %v", item, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var res struct {
		Written int64              `json:"written"`
		SHA256  string             `json:"sha256"`
		Batch   transfer.BatchView `json:"batch"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &res); err != nil {
			e.t.Fatalf("item %d: decode %q: %v", item, body, err)
		}
	}
	return resp.StatusCode, res.Batch
}

func completedItems(v transfer.BatchView) int {
	n := 0
	for _, item := range v.Items {
		if item.State == transfer.ItemStateCompleted {
			n++
		}
	}
	return n
}

// TestBatchItemCancelIsReflectedInStatusAndLetsTheRestFinish reproduces the
// reported flow end to end: withdraw one queued item, observe it in the
// authoritative status, upload the remaining items, and end in a terminal
// "canceled" batch that keeps the completed work and byte counts.
func TestBatchItemCancelIsReflectedInStatusAndLetsTheRestFinish(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	b := env.filesBatch(tok, 4, 5, 6)
	env.approveBatch(key, b)
	ids := env.contentIDs(key, b)

	rec := env.cancelBatchItem(tok, b.ID, b.Items[1].ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	var afterCancel transfer.BatchView
	decodeBody(t, rec, &afterCancel)
	if afterCancel.Items[1].State != transfer.ItemStateCanceled {
		t.Fatalf("canceled item = %q", afterCancel.Items[1].State)
	}
	if afterCancel.State != transfer.BatchStateQueued || afterCancel.TransferredBytes != 0 {
		t.Fatalf("batch after cancel = %q with %d bytes", afterCancel.State, afterCancel.TransferredBytes)
	}
	if up, _ := env.trans.Get(ids[1]); up.State != transfer.StateCanceled {
		t.Fatalf("upload record = %q, want canceled", up.State)
	}

	// The status endpoint reports the same authoritative state the phone
	// received from the cancel, so a refresh never disagrees.
	status := env.do(http.MethodGet, "/api/batches/"+b.ID, tok, nil)
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	var refreshed transfer.BatchView
	decodeBody(t, status, &refreshed)
	if refreshed.Items[1].State != transfer.ItemStateCanceled || refreshed.State != transfer.BatchStateQueued {
		t.Fatalf("refreshed = %+v", refreshed)
	}

	if code, v := env.uploadBatchItem(ts.URL, tok, b, 0, []byte("aaaa")); code != http.StatusOK || v.State != transfer.BatchStateQueued {
		t.Fatalf("first survivor = %d %+v", code, v)
	}
	code, final := env.uploadBatchItem(ts.URL, tok, b, 2, []byte("cccccc"))
	if code != http.StatusOK {
		t.Fatalf("last survivor = %d", code)
	}
	if final.State != transfer.BatchStateCanceled {
		t.Fatalf("terminal batch = %q, want canceled", final.State)
	}
	if final.CompletedAt.IsZero() {
		t.Fatal("terminal batch has no completion time")
	}
	wantItems := []transfer.ItemState{
		transfer.ItemStateCompleted, transfer.ItemStateCanceled, transfer.ItemStateCompleted,
	}
	for i, want := range wantItems {
		if final.Items[i].State != want {
			t.Fatalf("items = %v, want %v", itemStates(final), wantItems)
		}
	}
	if final.TransferredBytes != 10 || completedItems(final) != 2 {
		t.Fatalf("completed work = %d bytes / %d items", final.TransferredBytes, completedItems(final))
	}
	wantRecords := []transfer.State{transfer.StateCompleted, transfer.StateCanceled, transfer.StateCompleted}
	for i, want := range wantRecords {
		if up, _ := env.trans.Get(ids[i]); up.State != want {
			t.Fatalf("record %d = %q, want %q", i, up.State, want)
		}
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("temporary files survived: %v", left)
	}
}

// TestBatchItemCancelBeforeApprovalKeepsBatchPending proves a withdrawal made
// before the desktop decision neither releases the batch nor gets resurrected
// by the approval of the remaining items.
func TestBatchItemCancelBeforeApprovalKeepsBatchPending(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	b := env.filesBatch(tok, 4, 5, 6)
	ids := env.contentIDs(key, b)

	rec := env.cancelBatchItem(tok, b.ID, b.Items[1].ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", rec.Code, rec.Body.String())
	}
	var pending transfer.BatchView
	decodeBody(t, rec, &pending)
	if pending.State != transfer.BatchStatePending || pending.ApprovalState != transfer.ApprovalPending {
		t.Fatalf("withdrawn pending batch = %+v", pending)
	}
	if up, _ := env.trans.Get(ids[1]); up.State != transfer.StateCanceled {
		t.Fatalf("withdrawn record = %q, want canceled", up.State)
	}
	for _, i := range []int{0, 2} {
		if up, _ := env.trans.Get(ids[i]); up.State != transfer.StatePending {
			t.Fatalf("record %d = %q, want still pending", i, up.State)
		}
	}

	// The desktop decides the records that still await a decision, exactly
	// like App.ApproveBatch does after skipping the withdrawn item.
	if err := env.trans.DecideBatchItems([]string{ids[0], ids[2]}, true); err != nil {
		t.Fatalf("decide surviving items: %v", err)
	}
	approved, err := env.coord.ApproveBatch(key, b.ID)
	if err != nil {
		t.Fatalf("approve batch: %v", err)
	}
	want := []transfer.ItemState{
		transfer.ItemStateQueued, transfer.ItemStateCanceled, transfer.ItemStateQueued,
	}
	for i, state := range want {
		if approved.Items[i].State != state {
			t.Fatalf("approved items = %v, want %v", itemStates(approved), want)
		}
	}
	if up, _ := env.trans.Get(ids[1]); up.State != transfer.StateCanceled {
		t.Fatalf("approval resurrected the withdrawn record: %q", up.State)
	}

	if code, _ := env.uploadBatchItem(ts.URL, tok, b, 0, []byte("aaaa")); code != http.StatusOK {
		t.Fatalf("first survivor = %d", code)
	}
	code, final := env.uploadBatchItem(ts.URL, tok, b, 2, []byte("cccccc"))
	if code != http.StatusOK {
		t.Fatalf("last survivor = %d", code)
	}
	if final.State != transfer.BatchStateCanceled || final.TransferredBytes != 10 {
		t.Fatalf("terminal batch = %q with %d bytes", final.State, final.TransferredBytes)
	}
}

// TestBatchItemCancelIsScopedToTheAuthenticatedSession ensures a batch or
// item ID guessed by another session is indistinguishable from a missing one.
func TestBatchItemCancelIsScopedToTheAuthenticatedSession(t *testing.T) {
	env := newTestEnv(t)
	tokA, tokB := env.token(), env.token()
	keyA := env.tokenKey(tokA)
	b := env.filesBatch(tokA, 4, 5)
	env.approveBatch(keyA, b)

	if rec := env.cancelBatchItem(tokB, b.ID, b.Items[0].ID); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign cancel = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.cancelBatchItem(tokA, "does-not-exist", b.Items[0].ID); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown batch = %d %s", rec.Code, rec.Body.String())
	}
	if rec := env.cancelBatchItem(tokA, b.ID, "does-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown item = %d %s", rec.Code, rec.Body.String())
	}

	view, err := env.coord.GetBatch(keyA, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range itemStates(view) {
		if state != transfer.ItemStateQueued {
			t.Fatalf("item %d = %q after foreign cancel attempts", i, state)
		}
	}
}

// TestBatchItemCancelIsIdempotentButNeverRewritesFinishedItems covers repeated
// cancels (a retry after a lost response) and the refusal to relabel a
// completed item as canceled.
func TestBatchItemCancelIsIdempotentButNeverRewritesFinishedItems(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	b := env.filesBatch(tok, 4, 4, 4)
	env.approveBatch(key, b)

	for attempt := 1; attempt <= 2; attempt++ {
		rec := env.cancelBatchItem(tok, b.ID, b.Items[1].ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d = %d %s", attempt, rec.Code, rec.Body.String())
		}
		var v transfer.BatchView
		decodeBody(t, rec, &v)
		if v.Items[1].State != transfer.ItemStateCanceled {
			t.Fatalf("attempt %d item = %q", attempt, v.Items[1].State)
		}
	}

	if code, _ := env.uploadBatchItem(ts.URL, tok, b, 0, []byte("aaaa")); code != http.StatusOK {
		t.Fatalf("completed item upload = %d", code)
	}
	if rec := env.cancelBatchItem(tok, b.ID, b.Items[0].ID); rec.Code != http.StatusConflict {
		t.Fatalf("cancel completed item = %d %s", rec.Code, rec.Body.String())
	}
	if view, _ := env.coord.GetBatch(key, b.ID); view.Items[0].State != transfer.ItemStateCompleted {
		t.Fatalf("completed item rewritten as %q", view.Items[0].State)
	}

	if code, _ := env.uploadBatchItem(ts.URL, tok, b, 2, []byte("cccc")); code != http.StatusOK {
		t.Fatalf("last survivor = %d", code)
	}
	// The batch is terminal, but retrying the already canceled withdrawal is
	// still a safe no-op so the phone can converge after a lost response.
	rec := env.cancelBatchItem(tok, b.ID, b.Items[1].ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("terminal retry = %d %s", rec.Code, rec.Body.String())
	}
	var terminal transfer.BatchView
	decodeBody(t, rec, &terminal)
	if terminal.State != transfer.BatchStateCanceled || terminal.Items[1].State != transfer.ItemStateCanceled {
		t.Fatalf("terminal retry = %+v", terminal)
	}
	if rec := env.cancelBatchItem(tok, b.ID, b.Items[0].ID); rec.Code != http.StatusConflict {
		t.Fatalf("cancel completed terminal item = %d", rec.Code)
	}
}

// TestCancelActiveBatchItemReleasesResourcesAndSiblingStillUploads withdraws a
// streaming item mid-transfer: the stream is aborted, its pin and part file
// are released, and the sibling behind it still uploads to completion.
func TestCancelActiveBatchItemReleasesResourcesAndSiblingStillUploads(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	sess, err := env.sess.Validate(tok)
	if err != nil {
		t.Fatal(err)
	}
	b := env.filesBatch(tok, 8, 4)
	env.approveBatch(key, b)
	ids := env.contentIDs(key, b)

	pw, res := startPut(contentURL(ts.URL, b, 0), tok, 8)
	if _, err := pw.Write([]byte("1234")); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	waitFor(t, "stream to start", func() bool {
		u, _ := env.trans.Get(ids[0])
		return u.State == transfer.StateUploading
	})
	waitPinned(t, sess, 1)
	if left := partFiles(t, env.store.Dir()); len(left) != 1 {
		t.Fatalf("part files before cancel = %v", left)
	}

	rec := env.cancelBatchItem(tok, b.ID, b.Items[0].ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel active item = %d %s", rec.Code, rec.Body.String())
	}
	var view transfer.BatchView
	decodeBody(t, rec, &view)
	if view.Items[0].State != transfer.ItemStateCanceled || view.TransferredBytes != 0 {
		t.Fatalf("canceled stream view = %+v", view)
	}

	go func() {
		_, _ = pw.Write([]byte("5678"))
		_ = pw.Close()
	}()
	r := awaitPut(t, res)
	if r.err == nil && r.code == http.StatusOK {
		t.Fatalf("canceled stream reported success: %+v", r)
	}
	waitFor(t, "canceled record", func() bool {
		u, _ := env.trans.Get(ids[0])
		return u.State == transfer.StateCanceled
	})
	waitPinned(t, sess, 0)
	waitFor(t, "part file cleanup", func() bool { return len(partFiles(t, env.store.Dir())) == 0 })

	code, final := env.uploadBatchItem(ts.URL, tok, b, 1, []byte("bbbb"))
	if code != http.StatusOK {
		t.Fatalf("surviving sibling = %d", code)
	}
	if final.State != transfer.BatchStateCanceled || final.TransferredBytes != 4 {
		t.Fatalf("final batch = %q with %d bytes", final.State, final.TransferredBytes)
	}
	want := []transfer.ItemState{transfer.ItemStateCanceled, transfer.ItemStateCompleted}
	for i, state := range want {
		if final.Items[i].State != state {
			t.Fatalf("items = %v, want %v", itemStates(final), want)
		}
	}
	if got := env.trans.List(); len(got) != 2 {
		t.Fatalf("upload records = %d, want 2", len(got))
	}
}
