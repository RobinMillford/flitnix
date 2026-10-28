package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// receiveFiles lists the regular files in the receive directory, so a test can
// prove exactly what the user would find in their receive folder.
func receiveFiles(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read receive directory: %v", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			found = append(found, entry.Name())
		}
	}
	return found
}

func readReceived(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	return string(data)
}

// TestCancelTooLateAfterPublicationKeepsTheItemCompleted reproduces the
// reported race deterministically: the item's bytes are fully published and
// its upload record reads completed, while the coordinator still holds the
// live lease because the stream has not acknowledged the completion yet.
// A withdrawal arriving in that window used to answer 200 and relabel the item
// canceled, leaving a canceled item next to a published file. It must answer
// 409, leave the item alone, and let the acknowledgement finish the item.
func TestCancelTooLateAfterPublicationKeepsTheItemCompleted(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	b := env.filesBatch(tok, 4, 4)
	env.approveBatch(key, b)
	ids := env.contentIDs(key, b)

	lease, err := env.coord.ClaimItem(key, b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatalf("claim item: %v", err)
	}
	if err := env.coord.UpdateItemProgress(key, lease, 4); err != nil {
		t.Fatalf("progress: %v", err)
	}
	// Publication has already happened; the coordinator has not been told yet.
	res, err := env.trans.SubmitContent(ids[0], key, bytes.NewReader([]byte("aaaa")), nil)
	if err != nil {
		t.Fatalf("submit content: %v", err)
	}

	rec := env.cancelBatchItem(tok, b.ID, b.Items[0].ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel after publication = %d %s, want 409", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if body["error"] == "" {
		t.Errorf("409 body has no error message: %s", rec.Body.String())
	}

	// The item was not relabeled, the record still describes the file, and the
	// file is really there with the bytes the phone sent.
	view, err := env.coord.GetBatch(key, b.ID)
	if err != nil {
		t.Fatalf("get batch: %v", err)
	}
	if view.Items[0].State == transfer.ItemStateCanceled {
		t.Fatal("a published item was relabeled canceled by a late withdrawal")
	}
	if view.Items[0].State != transfer.ItemStateTransferring {
		t.Fatalf("item state = %q, want still transferring", view.Items[0].State)
	}
	if up, _ := env.trans.Get(ids[0]); up.State != transfer.StateCompleted {
		t.Fatalf("record state = %q, want completed", up.State)
	}
	if got := readReceived(t, res.Path); got != "aaaa" {
		t.Fatalf("published bytes = %q, want %q", got, "aaaa")
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("temporary files survived: %v", left)
	}

	// The stream's own acknowledgement is what closes the window; it must
	// still land, and it must be the transition that finishes the item.
	done, err := env.coord.CompleteItem(key, lease, res.SHA256)
	if err != nil {
		t.Fatalf("complete item after the refused cancel: %v", err)
	}
	if done.Items[0].State != transfer.ItemStateCompleted {
		t.Fatalf("item after acknowledgement = %q, want completed", done.Items[0].State)
	}

	// The sibling behind it is untouched and still completes the batch.
	code, final := env.uploadBatchItem(ts.URL, tok, b, 1, []byte("bbbb"))
	if code != http.StatusOK {
		t.Fatalf("sibling upload = %d", code)
	}
	if final.State != transfer.BatchStateCompleted || completedItems(final) != 2 {
		t.Fatalf("final batch = %q with %d completed items", final.State, completedItems(final))
	}
	if got := receiveFiles(t, env.store.Dir()); len(got) != 2 {
		t.Fatalf("receive directory = %v, want both published files", got)
	}
}

// TestCanceledRecordIsNeverReportedAsABatchFailure proves a deliberate
// cancellation of a streaming item is not mistaken for a transport fault. The
// upload record is withdrawn while the item is transferring, without the
// coordinator being told first — exactly the window the cancellation path
// itself opens between reserving the outcome and cancelling the item. The
// stream then fails with ErrCanceled; the item must end canceled (not failed),
// the batch must end canceled (not failed), and no file may appear.
func TestCanceledRecordIsNeverReportedAsABatchFailure(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()
	key := env.tokenKey(tok)
	b := env.filesBatch(tok, 8, 4)
	env.approveBatch(key, b)
	ids := env.contentIDs(key, b)

	sess, err := env.sess.Validate(tok)
	if err != nil {
		t.Fatal(err)
	}
	pw, res := startPut(contentURL(ts.URL, b, 0), tok, 8)
	if _, err := pw.Write([]byte("1234")); err != nil {
		t.Fatalf("write partial body: %v", err)
	}
	waitFor(t, "stream to start", func() bool {
		u, _ := env.trans.Get(ids[0])
		return u.State == transfer.StateUploading
	})
	waitPinned(t, sess, 1)

	authority, err := env.trans.ReserveCancel(ids[0])
	if err != nil || authority != transfer.CancelWins {
		t.Fatalf("ReserveCancel = (%v, %v), want (CancelWins, nil)", authority, err)
	}
	if err := env.trans.Cancel(ids[0]); err != nil {
		t.Fatalf("cancel record: %v", err)
	}

	go func() {
		_, _ = pw.Write([]byte("5678"))
		_ = pw.Close()
	}()
	r := awaitPut(t, res)
	if r.code == http.StatusOK {
		t.Fatalf("canceled stream reported success: %+v", r)
	}
	if r.code != http.StatusConflict {
		t.Fatalf("canceled stream status = %d, want 409", r.code)
	}

	view := waitBatchState(t, env, key, b.ID, func(v transfer.BatchView) bool {
		return v.Items[0].State == transfer.ItemStateCanceled || v.Items[0].State == transfer.ItemStateFailed
	})
	if view.Items[0].State != transfer.ItemStateCanceled {
		t.Fatalf("canceled item was reported as %q", view.Items[0].State)
	}
	if view.State == transfer.BatchStateFailed {
		t.Fatalf("a single cancellation failed the whole batch: %+v", view)
	}
	if up, _ := env.trans.Get(ids[0]); up.State != transfer.StateCanceled || up.Published != "" {
		t.Fatalf("record = %+v, want canceled with no published path", up)
	}
	waitPinned(t, sess, 0)
	waitFor(t, "part file cleanup", func() bool { return len(partFiles(t, env.store.Dir())) == 0 })
	if got := receiveFiles(t, env.store.Dir()); len(got) != 0 {
		t.Fatalf("canceled upload left files: %v", got)
	}

	code, final := env.uploadBatchItem(ts.URL, tok, b, 1, []byte("bbbb"))
	if code != http.StatusOK {
		t.Fatalf("surviving sibling = %d", code)
	}
	if final.State != transfer.BatchStateCanceled || final.TransferredBytes != 4 {
		t.Fatalf("final batch = %q with %d bytes", final.State, final.TransferredBytes)
	}
	if got := receiveFiles(t, env.store.Dir()); len(got) != 1 || readReceived(t, filepath.Join(env.store.Dir(), got[0])) != "bbbb" {
		t.Fatalf("receive directory = %v, want only the sibling's file", got)
	}
}

// TestCancellationRacingPublicationIsAlwaysConsistent fires a withdrawal and
// the final write concurrently and requires the same agreement every time,
// whichever side wins: a canceled item never has a file, a completed item
// always has its exact bytes, and neither the response nor the record
// contradicts the coordinator.
func TestCancellationRacingPublicationIsAlwaysConsistent(t *testing.T) {
	const rounds = 10
	var canceled, completed int
	for round := 0; round < rounds; round++ {
		round := round
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			env := newTestEnv(t)
			ts := httptest.NewServer(env.srv.Handler())
			defer ts.Close()
			tok := env.token()
			key := env.tokenKey(tok)
			b := env.filesBatch(tok, 4)
			env.approveBatch(key, b)
			id := env.contentIDs(key, b)[0]

			// Even rounds fire the withdrawal against a stream that is mid-
			// flight; odd rounds let the upload finish first, so both possible
			// winners are covered without ever waiting for a particular
			// interleaving.
			var rec *httptest.ResponseRecorder
			var put putResult
			if round%2 == 0 {
				pw, res := startPut(contentURL(ts.URL, b, 0), tok, 4)
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					rec = env.cancelBatchItem(tok, b.ID, b.Items[0].ID)
				}()
				go func() {
					_, _ = pw.Write([]byte("aaaa"))
					_ = pw.Close()
				}()
				wg.Wait()
				put = awaitPut(t, res)
			} else {
				code, _ := env.uploadBatchItem(ts.URL, tok, b, 0, []byte("aaaa"))
				put = putResult{code: code}
				rec = env.cancelBatchItem(tok, b.ID, b.Items[0].ID)
			}

			view, err := env.coord.GetBatch(key, b.ID)
			if err != nil {
				t.Fatalf("get batch: %v", err)
			}
			up, _ := env.trans.Get(id)
			item := view.Items[0]
			files := receiveFiles(t, env.store.Dir())

			switch item.State {
			case transfer.ItemStateCanceled:
				canceled++
				if rec.Code != http.StatusOK {
					t.Fatalf("cancel of a live item = %d %s", rec.Code, rec.Body.String())
				}
				if up.State != transfer.StateCanceled {
					t.Fatalf("canceled item has record %q", up.State)
				}
				if len(files) != 0 {
					t.Fatalf("canceled item left files: %v", files)
				}
				if put.code == http.StatusOK {
					t.Fatal("canceled item's stream reported success")
				}
				if view.State != transfer.BatchStateCanceled {
					t.Fatalf("batch state = %q, want canceled", view.State)
				}
			case transfer.ItemStateCompleted:
				completed++
				if rec.Code != http.StatusConflict {
					t.Fatalf("cancel after publication = %d %s, want 409", rec.Code, rec.Body.String())
				}
				if up.State != transfer.StateCompleted || up.Published == "" {
					t.Fatalf("completed item has record %+v", up)
				}
				if put.code != http.StatusOK {
					t.Fatalf("published item's stream = %d", put.code)
				}
				if got := readReceived(t, up.Published); got != "aaaa" {
					t.Fatalf("published bytes = %q, want %q", got, "aaaa")
				}
				if len(files) != 1 {
					t.Fatalf("receive directory = %v, want the published file", files)
				}
				if view.State != transfer.BatchStateCompleted {
					t.Fatalf("batch state = %q, want completed", view.State)
				}
			default:
				t.Fatalf("item ended in unrelated state %q", item.State)
			}
			if left := partFiles(t, env.store.Dir()); len(left) != 0 {
				t.Fatalf("temporary files survived: %v", left)
			}
		})
	}
	t.Logf("outcomes: %d canceled, %d completed", canceled, completed)
}

// waitBatchState polls the authoritative snapshot until cond accepts it.
func waitBatchState(t *testing.T, env *testEnv, key, batchID string, cond func(transfer.BatchView) bool) transfer.BatchView {
	t.Helper()
	var last transfer.BatchView
	waitFor(t, "batch to settle", func() bool {
		v, err := env.coord.GetBatch(key, batchID)
		if err != nil {
			return false
		}
		last = v
		return cond(v)
	})
	return last
}
