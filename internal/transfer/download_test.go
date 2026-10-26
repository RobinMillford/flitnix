package transfer

import (
	"sync"
	"testing"
	"time"
)

func TestDownloadRegistryTracksSuccessfulTransfer(t *testing.T) {
	r := NewDownloadRegistry()
	d, err := r.Start("file-id", "photo.bin", "session-key", 100)
	if err != nil {
		t.Fatal(err)
	}
	r.Progress(d.ID, 40)
	r.Progress(d.ID, 100)
	r.Finish(d.ID, 100)

	got := r.List()[0]
	if got.State != DownloadSent || got.Written != 100 || got.Size != 100 {
		t.Fatalf("download = %#v, want sent 100/100", got)
	}
	if got.FileID != "file-id" || got.Session != "session-key" {
		t.Fatalf("download identity was not retained: %#v", got)
	}
}

func TestDownloadRegistryMarksPartialTransferFailed(t *testing.T) {
	r := NewDownloadRegistry()
	d, err := r.Start("file-id", "partial.bin", "session-key", 100)
	if err != nil {
		t.Fatal(err)
	}

	r.Progress(d.ID, 37)
	r.Fail(d.ID, "connection ended", 37)

	got := r.List()[0]
	if got.State != DownloadFailed || got.Written != 37 || got.Reason == "" {
		t.Fatalf("download = %#v, want failed 37 with reason", got)
	}
}

func TestDownloadRegistryTracksArchiveLifecycleAndIgnoresLateUpdates(t *testing.T) {
	r := NewDownloadRegistry()
	d, err := r.StartKind("grant-id", "photos.zip", "session-key", 100, "archive")
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != "archive" || d.State != DownloadStarting || !d.Started.IsZero() {
		t.Fatalf("initial archive record = %#v", d)
	}

	r.Progress(d.ID, 25)
	r.Begin(d.ID)
	r.Finalize(d.ID)
	r.Finish(d.ID, 100)
	r.Progress(d.ID, 50)
	r.Fail(d.ID, "late failure", 50)

	got := r.List()[0]
	if got.State != DownloadSent || got.Written != 100 || got.Finished.IsZero() || got.Started.IsZero() {
		t.Fatalf("archive record = %#v, want completed 100-byte transfer", got)
	}
}

func TestDownloadRegistryConcurrentTransfersRemainDistinct(t *testing.T) {
	r := NewDownloadRegistry()
	const count = 32
	ids := make([]string, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, err := r.Start("file", "file.bin", "session", 1000)
			if err != nil {
				t.Errorf("start %d: %v", i, err)
				return
			}
			r.Progress(d.ID, int64(i))
			r.Finish(d.ID, int64(i))
			ids[i] = d.ID
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, count)
	for _, id := range ids {
		if id == "" || seen[id] {
			t.Fatalf("duplicate or empty transfer ID: %q", id)
		}
		seen[id] = true
	}
	if got := len(r.List()); got != count {
		t.Fatalf("registry has %d records, want %d", got, count)
	}
}

func TestDownloadRegistryCancelsSessionTransfers(t *testing.T) {
	r := NewDownloadRegistry()
	d, err := r.Start("file-id", "file.bin", "session-key", 100)
	if err != nil {
		t.Fatal(err)
	}
	r.CancelSession("session-key")
	got := r.List()[0]
	if got.State != DownloadCanceled || got.Finished.IsZero() {
		t.Fatalf("download = %#v, want canceled with finish time", got)
	}
	if time.Since(got.Finished) < 0 {
		t.Fatal("finish time is in the future")
	}
	// A terminal record must not be changed by a late completion callback.
	r.Finish(d.ID, 100)
	if got := r.List()[0]; got.State != DownloadCanceled {
		t.Fatalf("late completion changed canceled transfer to %q", got.State)
	}
}
