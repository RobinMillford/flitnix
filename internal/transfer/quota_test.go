package transfer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestInspectDiskSpaceWalksUpMissingDirectory(t *testing.T) {
	base := t.TempDir()
	missingTarget := filepath.Join(base, "does", "not", "exist", "yet")

	info := InspectDiskSpace(missingTarget)
	if !info.Available {
		t.Fatalf("expected OS disk metrics to be available for %s, got error: %s", missingTarget, info.Error)
	}
	if info.TargetExists {
		t.Fatalf("expected TargetExists=false for %s", missingTarget)
	}
	if info.ProbeDir != base {
		t.Fatalf("ProbeDir = %q, want %q", info.ProbeDir, base)
	}
	if info.TotalBytes == 0 || info.FreeBytes == 0 {
		t.Fatalf("expected positive TotalBytes and FreeBytes, got total=%d free=%d", info.TotalBytes, info.FreeBytes)
	}
}

func TestQuotaManagerEnoughAndInsufficientSpace(t *testing.T) {
	dir := t.TempDir()
	qm := NewQuotaManager(QuotaConfig{
		DefaultDir: dir,
		Probe: func(d string) DiskSpaceInfo {
			return DiskSpaceInfo{
				TargetDir:    d,
				ProbeDir:     d,
				TargetExists: true,
				Available:    true,
				FilesystemID: "vol:test",
				TotalBytes:   1000,
				FreeBytes:    500,
				UsedBytes:    500,
			}
		},
	})

	if err := qm.Reserve("u1", "s1", dir, 300); err != nil {
		t.Fatalf("expected first reserve to fit: %v", err)
	}
	st := qm.Status(dir)
	if st.ReservedBytes != 300 || st.HeadroomBytes != 200 {
		t.Fatalf("unexpected status after reserve: reserved=%d headroom=%d", st.ReservedBytes, st.HeadroomBytes)
	}

	if err := qm.Reserve("u2", "s1", dir, 250); !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected ErrInsufficientStorage on second reserve, got %v", err)
	}

	// Advancing committed bytes simulates OS FreeBytes shrinking at the same rate
	// so we never double-count committed data against headroom.
	qm.Advance("u1", 200)
	if got := qm.ReservedBytes(dir); got != 100 {
		t.Fatalf("ReservedBytes after Advance(200) = %d, want 100", got)
	}

	qm.Release("u1")
	if got := qm.ReservedBytes(dir); got != 0 {
		t.Fatalf("ReservedBytes after Release = %d, want 0", got)
	}
	if err := qm.Reserve("u2", "s1", dir, 250); err != nil {
		t.Fatalf("expected reserve after release to succeed: %v", err)
	}
}

func TestQuotaManagerUnknownDiskInfo(t *testing.T) {
	dir := t.TempDir()
	qm := NewQuotaManager(QuotaConfig{
		DefaultDir: dir,
		Probe: func(d string) DiskSpaceInfo {
			return DiskSpaceInfo{
				TargetDir: d,
				Available: false,
				Error:     "unsupported virtual fs",
			}
		},
	})

	st := qm.Status(dir)
	if st.Status != StorageStatusUnknown || st.Available {
		t.Fatalf("expected unknown status, got status=%q available=%v", st.Status, st.Available)
	}
	if err := qm.Reserve("u1", "s1", dir, 10<<30); err != nil {
		t.Fatalf("unknown filesystem info should not falsely reject transfers: %v", err)
	}
	if fits, _ := qm.CheckExistingFit(dir, []string{"u1"}, 10<<30); !fits {
		t.Fatalf("expected unknown filesystem CheckExistingFit to return true")
	}
}

func TestQuotaManagerConcurrentReservationsNeverOvercommit(t *testing.T) {
	dir := t.TempDir()
	qm := NewQuotaManager(QuotaConfig{
		DefaultDir: dir,
		Probe: func(d string) DiskSpaceInfo {
			return DiskSpaceInfo{
				TargetDir:    d,
				ProbeDir:     d,
				TargetExists: true,
				Available:    true,
				FilesystemID: "vol:concurrent",
				TotalBytes:   2000,
				FreeBytes:    1000,
				UsedBytes:    1000,
			}
		},
	})

	const workers = 12
	const batchSize = 300 // max 3 batches fit in 1000 (900 <= 1000 < 1200)
	var succeeded atomic.Int32
	var failed507 atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("b-%d", idx)
			err := qm.ReserveBatch("s1", dir, []QuotaBatchItem{
				{ID: id + "-1", Bytes: 150},
				{ID: id + "-2", Bytes: 150},
			})
			if err == nil {
				succeeded.Add(1)
			} else if errors.Is(err, ErrInsufficientStorage) {
				failed507.Add(1)
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if succeeded.Load() != 3 {
		t.Fatalf("succeeded = %d, want 3 batches (%d bytes <= 1000)", succeeded.Load(), batchSize*3)
	}
	if failed507.Load() != workers-3 {
		t.Fatalf("failed507 = %d, want %d", failed507.Load(), workers-3)
	}
	if got := qm.ReservedBytes(dir); got != 900 {
		t.Fatalf("ReservedBytes = %d, want 900", got)
	}
}

func TestQuotaManagerRecoveryReconstruction(t *testing.T) {
	dir := t.TempDir()
	qm := NewQuotaManager(QuotaConfig{
		DefaultDir: dir,
		Probe: func(d string) DiskSpaceInfo {
			return DiskSpaceInfo{
				TargetDir:    d,
				ProbeDir:     d,
				TargetExists: true,
				Available:    true,
				FilesystemID: "vol:rec",
				TotalBytes:   2000,
				FreeBytes:    800,
			}
		},
	})

	// Item declared 1000 bytes, already committed 600 bytes on disk before crash.
	// Remaining reservation must be 400 bytes, leaving 400 bytes headroom.
	qm.RestoreReservation("rec-1", "s1", dir, 1000, 600)
	st := qm.Status(dir)
	if st.ReservedBytes != 400 || st.HeadroomBytes != 400 {
		t.Fatalf("unexpected recovery status: reserved=%d headroom=%d", st.ReservedBytes, st.HeadroomBytes)
	}
	if err := qm.Reserve("new-2", "s1", dir, 401); !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected 401 bytes to exceed restored headroom 400, got %v", err)
	}
	if err := qm.Reserve("new-1", "s1", dir, 400); err != nil {
		t.Fatalf("expected 400 bytes to fit restored headroom: %v", err)
	}
}

func TestServiceExternalDiskReductionDuringStream(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var free atomic.Uint64
	free.Store(10 * 1024 * 1024) // 10 MiB initially
	qm := NewQuotaManager(QuotaConfig{
		DefaultDir: dir,
		Probe: func(d string) DiskSpaceInfo {
			f := free.Load()
			return DiskSpaceInfo{
				TargetDir:    d,
				ProbeDir:     d,
				TargetExists: true,
				Available:    true,
				FilesystemID: "vol:extdrop",
				TotalBytes:   20 * 1024 * 1024,
				FreeBytes:    f,
			}
		},
	})

	svc := NewService(Config{Store: store, Quota: qm})
	up, err := svc.Declare("sess1", "large.bin", 6*1024*1024)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	// Simulate an external application filling the disk after the first 1 MiB.
	pr, pw := io.Pipe()
	go func() {
		chunk := make([]byte, 1024*1024)
		_, _ = pw.Write(chunk)
		// Drop free space sharply before the 4 MiB diskCheckInterval triggers.
		free.Store(512 * 1024)
		for i := 0; i < 5; i++ {
			if _, werr := pw.Write(chunk); werr != nil {
				break
			}
		}
		_ = pw.Close()
	}()

	_, cerr := svc.SubmitContent(up.ID, "sess1", pr, nil)
	if !errors.Is(cerr, ErrInsufficientStorage) {
		t.Fatalf("expected ErrInsufficientStorage during mid-stream exhaustion, got %v", cerr)
	}
	if qm.ReservedBytes(dir) != 0 {
		t.Fatalf("expected reservation released on failure, got %d", qm.ReservedBytes(dir))
	}
}

func TestServiceTempFileNoSpaceMapsToErrInsufficientStorage(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	svc := NewService(Config{Store: store})
	up, err := svc.Declare("sess1", "nospc.bin", 128)
	if err != nil {
		t.Fatalf("Declare: %v", err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	orig := newPartSink
	newPartSink = func(_ *Store, _ string) (contentSink, error) {
		return &failingNoSpaceSink{}, nil
	}
	t.Cleanup(func() { newPartSink = orig })

	_, cerr := svc.SubmitContent(up.ID, "sess1", bytes.NewReader(make([]byte, 128)), nil)
	if !errors.Is(cerr, ErrInsufficientStorage) {
		t.Fatalf("expected ENOSPC write to map to ErrInsufficientStorage, got %v", cerr)
	}
}

type failingNoSpaceSink struct{}

func (f *failingNoSpaceSink) Write(_ []byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "/tmp/part", Err: syscall.ENOSPC}
}
func (f *failingNoSpaceSink) Commit() (string, error) { return "", nil }
func (f *failingNoSpaceSink) Abort() error            { return nil }

func TestServiceDecideAndCancelReleaseReservation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var free atomic.Uint64
	free.Store(1000)
	qm := NewQuotaManager(QuotaConfig{
		DefaultDir: dir,
		Probe: func(d string) DiskSpaceInfo {
			return DiskSpaceInfo{
				TargetDir:    d,
				ProbeDir:     d,
				TargetExists: true,
				Available:    true,
				FilesystemID: "vol:decide",
				TotalBytes:   2000,
				FreeBytes:    free.Load(),
			}
		},
	})
	svc := NewService(Config{Store: store, Quota: qm})

	// Pending batches are checked against current free headroom and reserved atomically on approval.
	up, err := svc.DeclareBatchItem("sess1", "", "", "photo.jpg", 600)
	if err != nil {
		t.Fatalf("DeclareBatchItem: %v", err)
	}
	if err := svc.DecideBatchItems([]string{up.ID}, true); err != nil {
		t.Fatalf("DecideBatchItems approve: %v", err)
	}
	if qm.ReservedBytes(dir) != 600 {
		t.Fatalf("expected 600 reserved after approve, got %d", qm.ReservedBytes(dir))
	}

	// Attempting a second approval that would overcommit free space is refused.
	upOver, err := svc.DeclareBatchItem("sess1", "", "", "photo2.jpg", 500)
	if err == nil {
		// Fit check saw 400 headroom < 500 -> rejected before approval, or test shrink case:
		t.Fatalf("expected 500 to fail against 400 remaining headroom")
	}
	_ = upOver

	// Rejecting/canceling releases reservation cleanly; test shrink before approve on a fresh item.
	if err := svc.Cancel(up.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if qm.ReservedBytes(dir) != 0 {
		t.Fatalf("expected 0 reserved after cancel, got %d", qm.ReservedBytes(dir))
	}

	up3, err := svc.DeclareBatchItem("sess1", "", "", "photo3.jpg", 600)
	if err != nil {
		t.Fatalf("DeclareBatchItem 3: %v", err)
	}
	// Shrink free space externally below declared batch size before desktop approves.
	free.Store(400)
	if err := svc.DecideBatchItems([]string{up3.ID}, true); !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected ErrInsufficientStorage on approve when free shrank, got %v", err)
	}
	if err := svc.DecideBatchItems([]string{up3.ID}, false); err != nil {
		t.Fatalf("DecideBatchItems reject: %v", err)
	}
	if qm.ReservedBytes(dir) != 0 {
		t.Fatalf("expected 0 reserved after reject, got %d", qm.ReservedBytes(dir))
	}

	// Free space is 400 now: new 350 upload succeeds on approve, then Cancel releases it.
	up2, err := svc.Declare("sess1", "fit.jpg", 350)
	if err != nil {
		t.Fatalf("Declare after reject: %v", err)
	}
	if _, err := svc.Decide(up2.ID, true); err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	if qm.ReservedBytes(dir) != 350 {
		t.Fatalf("expected 350 reserved, got %d", qm.ReservedBytes(dir))
	}
	if err := svc.Cancel(up2.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if qm.ReservedBytes(dir) != 0 {
		t.Fatalf("expected 0 reserved after cancel, got %d", qm.ReservedBytes(dir))
	}
	_ = strings.Contains("", "")
}
