package transfer

// WP4 Phase 3 crash-recovery harness: real subprocess crash tests.
//
// The parent test re-executes this test binary with FLITNIX_WP4_CRASH_MODE set.
// TestMain intercepts, runs the requested child mode (driving the real
// ResumableManager API against a shared staging directory) and then exits with
// status 1 — no defers, no flushes, no graceful shutdown. The parent then opens
// a fresh manager against the same on-disk staging state and asserts the
// recovery contract.
//
// Scenarios covered (Revision 2.8 Section 16 numbering):
//   - Scenario 8  (mode "partial-tail"):    uncheckpointed trailing bytes are
//     truncated to the committed metadata offset after a real mid-ingest
//     process crash.
//   - Scenario 14 (mode "awaiting-digest"): desktop application restart restores
//     upload metadata and hash state; completion succeeds without re-upload.
//   - Scenario 43 (mode "awaiting-digest"): restart in awaiting_digest completes
//     without re-uploading the data.
//   - Scenario 36 (mode "quota"):           outstanding quota reservations are
//     restored from staging metadata before new uploads are accepted.
//
// The harness is deterministic and byte-count driven: no sleeps, no waits. The
// child's checkpoint thresholds are chosen so checkpoints fire on byte counts
// only (the duration threshold is set to one hour).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	envCrashMode = "FLITNIX_WP4_CRASH_MODE"
	envCrashDir  = "FLITNIX_WP4_CRASH_DIR"
)

// TestMain dispatches crash-child modes. When FLITNIX_WP4_CRASH_MODE is unset
// (the normal `go test` invocation) it behaves like the default TestMain.
func TestMain(m *testing.M) {
	if mode := os.Getenv(envCrashMode); mode != "" {
		if err := runCrashChildMode(mode, os.Getenv(envCrashDir)); err != nil {
			fmt.Fprintf(os.Stderr, "wp4 crash child %q failed: %v\n", mode, err)
			os.Exit(2)
		}
		// Simulated hard crash: exit immediately, skipping every defer.
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// crashPayload builds the deterministic payload shared by child and parent
// (both run in the same test binary).
func crashPayload(size int64, fill byte) []byte {
	return bytes.Repeat([]byte{fill}, int(size))
}

// newCrashManager wires a manager with a deterministic quota budget. The
// returned manager is deliberately never Close()d in child modes: a real crash
// runs no cleanup.
func newCrashManager(store *Store, root string, simulatedFree uint64) *ResumableManager {
	qm := NewQuotaManager(root, 0, 10)
	qm.SetFreeSpaceFunc(func(string) (uint64, error) {
		return simulatedFree, nil
	})
	return NewResumableManager(store, qm)
}

// runCrashChildMode executes the child half of a crash scenario and returns.
// TestMain then exits with status 1 to simulate the crash.
func runCrashChildMode(mode, root string) error {
	if root == "" {
		return errors.New("crash child requires FLITNIX_WP4_CRASH_DIR")
	}
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		return err
	}

	switch mode {
	case "partial-tail": // Scenario 8
		rm := newCrashManager(store, root, 1<<30)
		if _, err := rm.CreateUpload("crash-sess", "wp4-sc8", "b", "it", "tail.bin", "", 300*1024, root); err != nil {
			return err
		}
		// Byte-driven checkpoints only: a one-hour duration threshold cannot fire.
		rm.SetCheckpointParams(128*1024, time.Hour)
		// Ingest 300 KB against a 256 KB copy buffer: one 256 KB chunk (checkpoint
		// commits 256 KB), then a 44 KB chunk. The next Read hard-exits the
		// process before any interruption-commit path can run, leaving the 44 KB
		// tail on disk but uncommitted.
		_, _, _ = rm.IngestStream("wp4-sc8", 0, &exitMidstreamReader{data: crashPayload(300*1024, 'D')}, nil)
		return errors.New("exitMidstreamReader must hard-exit the process mid-ingest")

	case "awaiting-digest": // Scenarios 14 & 43
		rm := newCrashManager(store, root, 1<<30)
		if _, err := rm.CreateUpload("crash-sess", "wp4-sc14", "b", "it", "crash14.bin", "", 64*1024, root); err != nil {
			return err
		}
		if _, _, err := rm.IngestStream("wp4-sc14", 0, bytes.NewReader(crashPayload(64*1024, 'A')), nil); err != nil {
			return fmt.Errorf("child ingest failed: %w", err)
		}
		// Crash before digest submission: on-disk state must be awaiting_digest.
		return nil

	case "quota": // Scenario 36
		// Leave exactly 10 MB available after the production safety margin.
		rm := newCrashManager(store, root, uint64(DefaultSafetyMargin)+10*1024*1024)
		if _, err := rm.CreateUpload("crash-sess", "wp4-sc36", "b", "it", "quota.bin", "", 6*1024*1024, root); err != nil {
			return err
		}
		// Crash without releasing the 6 MB reservation.
		return nil

	case "crash-after-meta-tmp-sync":
		rm := newCrashManager(store, root, 1<<30)
		rm.SetCheckpointParams(64*1024, time.Hour)
		if _, err := rm.CreateUpload("crash-sess", "sc-tmp-sync", "b", "it", "tmp_sync.bin", "", 128*1024, root); err != nil {
			return err
		}
		rm.SetMetaSyncHook(func(f *os.File) error {
			err := f.Sync()
			if err == nil {
				// Process termination after .meta.tmp Sync but before rename
				os.Exit(1)
			}
			return err
		})
		_, _, _ = rm.IngestStream("sc-tmp-sync", 0, &fixedChunkReader{data: crashPayload(128*1024, 'S'), chunkSize: 32 * 1024}, nil)
		return errors.New("expected crash hook to exit")

	case "crash-after-rename":
		rm := newCrashManager(store, root, 1<<30)
		rm.SetCheckpointParams(64*1024, time.Hour)
		if _, err := rm.CreateUpload("crash-sess", "sc-rename", "b", "it", "rename.bin", "", 128*1024, root); err != nil {
			return err
		}
		rm.SetMetaRenameHook(func(oldPath, newPath string) error {
			err := renameFileWithRetry(oldPath, newPath)
			if err == nil {
				// Process termination after rename but before parent-directory Sync
				os.Exit(1)
			}
			return err
		})
		_, _, _ = rm.IngestStream("sc-rename", 0, &fixedChunkReader{data: crashPayload(128*1024, 'R'), chunkSize: 32 * 1024}, nil)
		return errors.New("expected crash hook to exit")

	case "crash-after-syncdir":
		rm := newCrashManager(store, root, 1<<30)
		rm.SetCheckpointParams(64*1024, time.Hour)
		if _, err := rm.CreateUpload("crash-sess", "sc-syncdir", "b", "it", "syncdir.bin", "", 128*1024, root); err != nil {
			return err
		}
		rm.SetMetaSyncDirHook(func(dir string) error {
			err := syncDir(dir)
			if err == nil {
				// Process termination after parent-directory Sync but before HTTP response
				os.Exit(1)
			}
			return err
		})
		_, _, _ = rm.IngestStream("sc-syncdir", 0, &fixedChunkReader{data: crashPayload(128*1024, 'D'), chunkSize: 32 * 1024}, nil)
		return errors.New("expected crash hook to exit")

	case "crash-uncommitted-tail":
		rm := newCrashManager(store, root, 1<<30)
		rm.SetCheckpointParams(64*1024, time.Hour)
		if _, err := rm.CreateUpload("crash-sess", "sc-tail", "b", "it", "tail.bin", "", 128*1024, root); err != nil {
			return err
		}
		// Ingest 64 KB (checkpoint fired & durable), then 32 KB, then hard exit
		_, _, _ = rm.IngestStream("sc-tail", 0, &exitMidstreamReader{data: crashPayload(96*1024, 'T'), chunkSize: 32 * 1024}, nil)
		return errors.New("expected exitMidstreamReader to exit")
	}
	return fmt.Errorf("unknown crash mode %q", mode)
}

// exitMidstreamReader delivers its data and then hard-exits the process on the
// next Read, simulating a power-loss crash in the middle of IngestStream: no
// defers run, no interruption commit path runs, nothing is flushed.
type exitMidstreamReader struct {
	data      []byte
	pos       int
	chunkSize int
	exited    bool
}

func (r *exitMidstreamReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		if !r.exited {
			r.exited = true
			os.Exit(1)
		}
		return 0, io.EOF
	}
	n := r.chunkSize
	if n == 0 || n > len(p) {
		n = len(p)
	}
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	}
	copy(p[:n], r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

// runCrashChild spawns this test binary as a crash child and requires exit
// status 1 (the simulated crash). Any other status is a harness failure.
func runCrashChild(t *testing.T, mode, root string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		envCrashMode+"="+mode,
		envCrashDir+"="+root,
	)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Logf("crash child (%s) output:\n%s", mode, out)
		t.Fatalf("crash child %q exited with %v, want status 1 (simulated crash)", mode, err)
	}
}

// TestWp4Crash_Scenario8_UncheckpointedTailTruncatedToCommittedOffset verifies
// Scenario 8 with a real process crash: bytes written past the last durable
// checkpoint are truncated to the committed metadata offset on restart, and the
// upload resumes from the recovered checkpoint.
func TestWp4Crash_Scenario8_UncheckpointedTailTruncatedToCommittedOffset(t *testing.T) {
	root := t.TempDir()
	runCrashChild(t, "partial-tail", root)

	staging := filepath.Join(root, StagingDirName)
	partPath := filepath.Join(staging, ".flitnix-part-wp4-sc8.part")

	// Pre-recovery: the crashed process left the uncheckpointed tail on disk
	// (300 KB written, only 256 KB durably committed).
	fi, err := os.Stat(partPath)
	if err != nil {
		t.Fatalf("part file missing after crash: %v", err)
	}
	if fi.Size() != 300*1024 {
		t.Fatalf("expected 300 KB of bytes on disk after mid-ingest crash, got %d", fi.Size())
	}

	// Recovery: a fresh manager must reconcile the staging state.
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := newCrashManager(store, root, 1<<30)
	defer rm.Close()

	upload, ok := rm.GetUpload("wp4-sc8")
	if !ok {
		t.Fatal("upload not registered after recovery")
	}
	if got := upload.Meta().CommittedOffset; got != 256*1024 {
		t.Fatalf("expected committed offset 256 KB (one 256 KB chunk, one 128 KB checkpoint threshold), got %d", got)
	}
	if got := upload.Meta().State; got != ResumableInterrupted {
		t.Fatalf("expected state interrupted, got %s", got)
	}
	fi, err = os.Stat(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 256*1024 {
		t.Fatalf("expected part file truncated to committed metadata offset (256 KB), got %d", fi.Size())
	}

	// The upload resumes from the recovered checkpoint and completes.
	rest := crashPayload(44*1024, 'D')
	off, state, err := rm.IngestStream("wp4-sc8", 256*1024, bytes.NewReader(rest), nil)
	if err != nil {
		t.Fatalf("resume after crash recovery failed: %v", err)
	}
	if off != 300*1024 || state != ResumableAwaitingDigest {
		t.Fatalf("expected 300 KB awaiting_digest after resume, got %d / %s", off, state)
	}
}

// TestWp4Crash_Scenarios14_43_RestartInAwaitingDigestCompletesWithoutReupload
// verifies Scenarios 14 and 43 with a real process crash: after a desktop
// restart in awaiting_digest, the metadata and hash state are restored and the
// upload completes without re-transmitting a single byte.
func TestWp4Crash_Scenarios14_43_RestartInAwaitingDigestCompletesWithoutReupload(t *testing.T) {
	root := t.TempDir()
	runCrashChild(t, "awaiting-digest", root)

	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := newCrashManager(store, root, 1<<30)
	defer rm.Close()

	upload, ok := rm.GetUpload("wp4-sc14")
	if !ok {
		t.Fatal("upload not restored after restart")
	}
	if got := upload.Meta().State; got != ResumableAwaitingDigest {
		t.Fatalf("expected state awaiting_digest after restart, got %s", got)
	}
	if got := upload.Meta().CommittedOffset; got != 64*1024 {
		t.Fatalf("expected committed offset 64 KB after restart, got %d", got)
	}

	// Complete without re-uploading any data.
	payload := crashPayload(64*1024, 'A')
	sum := sha256.Sum256(payload)
	rec, err := rm.FinalizeComplete("wp4-sc14", hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("FinalizeComplete after restart: %v", err)
	}
	data, err := os.ReadFile(rec.TargetPath)
	if err != nil {
		t.Fatalf("published file unreadable: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("published file content mismatch after crash recovery")
	}

	// Idempotent repeat after recovery: a matching digest returns the existing
	// publication without error (Scenario 44 semantics adjacent).
	if _, err := rm.FinalizeComplete("wp4-sc14", hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("idempotent re-finalize after restart: %v", err)
	}
}

// TestWp4Crash_Scenario36_QuotaReservationsRestoredFromStaging verifies
// Scenario 36 with a real process crash: outstanding quota reservations are
// restored from staging metadata before new uploads are accepted.
func TestWp4Crash_Scenario36_QuotaReservationsRestoredFromStaging(t *testing.T) {
	root := t.TempDir()
	runCrashChild(t, "quota", root)

	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	// 10 MB simulated budget; the crashed upload reserved 6 MB.
	qm := NewQuotaManager(root, DefaultSafetyMargin, 10)
	qm.SetFreeSpaceFunc(func(string) (uint64, error) {
		return uint64(DefaultSafetyMargin) + 10*1024*1024, nil
	})
	// The constructor runs RecoverOnStartup, which replays quota reservations.
	rm := NewResumableManager(store, qm)
	defer rm.Close()

	// The restored reservation must cap new uploads in the same session.
	if err := qm.Reserve("crash-sess", "new-upload", 5*1024*1024); !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected restored reservation to cap new uploads (only 4 MB left), got %v", err)
	}
	if err := qm.Reserve("crash-sess", "new-upload", 4*1024*1024); err != nil {
		t.Fatalf("expected remaining 4 MB to be reservable, got %v", err)
	}

	// The crashed upload itself is registered and resumable.
	if upload, ok := rm.GetUpload("wp4-sc36"); !ok {
		t.Fatal("crashed upload not registered after recovery")
	} else if upload.Meta().CommittedOffset != 0 {
		t.Fatalf("expected zero committed bytes, got %d", upload.Meta().CommittedOffset)
	}
}

// TestCrashBoundary_AfterMetaTmpSync_RollsBackToPreviousCheckpoint tests process termination
// right after .meta.tmp Sync completes, but before os.Rename executes.
// The unrenamed .meta.tmp must be safely ignored, recovery must load canonical .meta (offset 0),
// uncheckpointed part-file bytes must be truncated, and resumption must complete with full SHA-256 integrity.
func TestCrashBoundary_AfterMetaTmpSync_RollsBackToPreviousCheckpoint(t *testing.T) {
	root := t.TempDir()
	runCrashChild(t, "crash-after-meta-tmp-sync", root)

	staging := filepath.Join(root, StagingDirName)
	tmpPath := filepath.Join(staging, ".flitnix-part-sc-tmp-sync.meta.tmp")
	metaPath := filepath.Join(staging, ".flitnix-part-sc-tmp-sync.meta")
	partPath := filepath.Join(staging, ".flitnix-part-sc-tmp-sync.part")

	// Pre-recovery assertions: .meta.tmp was synced on disk
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("expected .meta.tmp to exist on disk after crash, got %v", err)
	}
	mData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read canonical meta before recovery: %v", err)
	}
	var preMeta ResumableMeta
	if err := json.Unmarshal(mData, &preMeta); err != nil {
		t.Fatalf("unmarshal canonical meta: %v", err)
	}
	if preMeta.CommittedOffset != 0 {
		t.Fatalf("expected canonical meta to still record offset 0 before recovery, got %d", preMeta.CommittedOffset)
	}

	// Recovery: fresh manager reconciles staging
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := newCrashManager(store, root, 1<<30)
	defer rm.Close()

	upload, ok := rm.GetUpload("sc-tmp-sync")
	if !ok {
		t.Fatal("upload not registered after recovery")
	}
	// Recovery MUST NOT advance merely because .meta.tmp was synced!
	if got := upload.Meta().CommittedOffset; got != 0 {
		t.Fatalf("expected recovery offset 0, got %d", got)
	}
	// Verify part file was truncated to 0
	pfi, err := os.Stat(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if pfi.Size() != 0 {
		t.Fatalf("expected part file truncated to 0, got %d", pfi.Size())
	}

	// Resume from 0 and complete transfer
	payload := crashPayload(128*1024, 'S')
	off, state, err := rm.IngestStream("sc-tmp-sync", 0, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if off != 128*1024 || state != ResumableAwaitingDigest {
		t.Fatalf("expected 128 KB awaiting_digest, got %d / %s", off, state)
	}

	expectedSum := sha256.Sum256(payload)
	pub, err := rm.FinalizeComplete("sc-tmp-sync", hex.EncodeToString(expectedSum[:]))
	if err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if pub == nil || pub.ExpectedSHA256 != hex.EncodeToString(expectedSum[:]) {
		t.Fatalf("unexpected publication record: %+v", pub)
	}
}

// TestCrashBoundary_AfterRename_ProcessCrashPreservesRenamedMeta tests process termination
// immediately after atomic os.Rename succeeds, before syncDir runs.
// In a process termination, the OS VFS preserves the rename; recovery recognizes the 64 KB checkpoint,
// and the upload resumes from 64 KB with full SHA-256 integrity.
func TestCrashBoundary_AfterRename_ProcessCrashPreservesRenamedMeta(t *testing.T) {
	root := t.TempDir()
	runCrashChild(t, "crash-after-rename", root)

	staging := filepath.Join(root, StagingDirName)
	metaPath := filepath.Join(staging, ".flitnix-part-sc-rename.meta")
	partPath := filepath.Join(staging, ".flitnix-part-sc-rename.part")

	// Pre-recovery assertions: canonical .meta was replaced on disk
	mData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read canonical meta before recovery: %v", err)
	}
	var preMeta ResumableMeta
	if err := json.Unmarshal(mData, &preMeta); err != nil {
		t.Fatalf("unmarshal canonical meta: %v", err)
	}
	if preMeta.CommittedOffset != 64*1024 {
		t.Fatalf("expected canonical meta to record offset 64 KB, got %d", preMeta.CommittedOffset)
	}

	// Recovery: fresh manager reconciles staging
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := newCrashManager(store, root, 1<<30)
	defer rm.Close()

	upload, ok := rm.GetUpload("sc-rename")
	if !ok {
		t.Fatal("upload not registered after recovery")
	}
	if got := upload.Meta().CommittedOffset; got != 64*1024 {
		t.Fatalf("expected recovery offset 64 KB, got %d", got)
	}
	if got := upload.Meta().State; got != ResumableInterrupted {
		t.Fatalf("expected state interrupted, got %s", got)
	}

	pfi, err := os.Stat(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if pfi.Size() != 64*1024 {
		t.Fatalf("expected part file size 64 KB, got %d", pfi.Size())
	}

	// Resume from 64 KB to 128 KB
	fullPayload := crashPayload(128*1024, 'R')
	off, state, err := rm.IngestStream("sc-rename", 64*1024, bytes.NewReader(fullPayload[64*1024:]), nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if off != 128*1024 || state != ResumableAwaitingDigest {
		t.Fatalf("expected 128 KB awaiting_digest, got %d / %s", off, state)
	}

	expectedSum := sha256.Sum256(fullPayload)
	pub, err := rm.FinalizeComplete("sc-rename", hex.EncodeToString(expectedSum[:]))
	if err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if pub == nil || pub.ExpectedSHA256 != hex.EncodeToString(expectedSum[:]) {
		t.Fatalf("unexpected publication record: %+v", pub)
	}
}

// TestCrashBoundary_AfterSyncDir_RecoversCommittedOffset tests process termination
// after parent directory sync completes, but before the HTTP response is sent.
// Both part-file data, canonical metadata, and directory link are fully durable on disk.
func TestCrashBoundary_AfterSyncDir_RecoversCommittedOffset(t *testing.T) {
	root := t.TempDir()
	runCrashChild(t, "crash-after-syncdir", root)

	staging := filepath.Join(root, StagingDirName)
	metaPath := filepath.Join(staging, ".flitnix-part-sc-syncdir.meta")
	partPath := filepath.Join(staging, ".flitnix-part-sc-syncdir.part")

	// Pre-recovery assertions
	mData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read canonical meta before recovery: %v", err)
	}
	var preMeta ResumableMeta
	if err := json.Unmarshal(mData, &preMeta); err != nil {
		t.Fatalf("unmarshal canonical meta: %v", err)
	}
	if preMeta.CommittedOffset != 64*1024 {
		t.Fatalf("expected canonical meta offset 64 KB, got %d", preMeta.CommittedOffset)
	}

	// Recovery
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := newCrashManager(store, root, 1<<30)
	defer rm.Close()

	upload, ok := rm.GetUpload("sc-syncdir")
	if !ok {
		t.Fatal("upload not registered after recovery")
	}
	if got := upload.Meta().CommittedOffset; got != 64*1024 {
		t.Fatalf("expected recovery offset 64 KB, got %d", got)
	}

	pfi, err := os.Stat(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if pfi.Size() != 64*1024 {
		t.Fatalf("expected part file size 64 KB, got %d", pfi.Size())
	}

	// Resume and complete
	fullPayload := crashPayload(128*1024, 'D')
	off, state, err := rm.IngestStream("sc-syncdir", 64*1024, bytes.NewReader(fullPayload[64*1024:]), nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if off != 128*1024 || state != ResumableAwaitingDigest {
		t.Fatalf("expected 128 KB awaiting_digest, got %d / %s", off, state)
	}

	expectedSum := sha256.Sum256(fullPayload)
	pub, err := rm.FinalizeComplete("sc-syncdir", hex.EncodeToString(expectedSum[:]))
	if err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if pub == nil || pub.ExpectedSHA256 != hex.EncodeToString(expectedSum[:]) {
		t.Fatalf("unexpected publication record: %+v", pub)
	}
}

// TestCrashBoundary_UncommittedTailTruncated tests process termination after
// additional uncommitted part-file bytes have been written past the last durable checkpoint.
// Pre-recovery part file has 96 KB on disk; recovery truncates part file back to the durable 64 KB
// checkpoint, prevents acknowledgement of the uncommitted 32 KB tail, and completes cleanly.
func TestCrashBoundary_UncommittedTailTruncated(t *testing.T) {
	root := t.TempDir()
	runCrashChild(t, "crash-uncommitted-tail", root)

	staging := filepath.Join(root, StagingDirName)
	partPath := filepath.Join(staging, ".flitnix-part-sc-tail.part")
	metaPath := filepath.Join(staging, ".flitnix-part-sc-tail.meta")

	// Pre-recovery assertions: part file has 96 KB, meta has 64 KB
	pfiPre, err := os.Stat(partPath)
	if err != nil {
		t.Fatalf("part file stat before recovery: %v", err)
	}
	if pfiPre.Size() != 96*1024 {
		t.Fatalf("expected uncheckpointed part file size 96 KB before recovery, got %d", pfiPre.Size())
	}

	mData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read canonical meta: %v", err)
	}
	var preMeta ResumableMeta
	if err := json.Unmarshal(mData, &preMeta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if preMeta.CommittedOffset != 64*1024 {
		t.Fatalf("expected committed offset 64 KB before recovery, got %d", preMeta.CommittedOffset)
	}

	// Recovery
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := newCrashManager(store, root, 1<<30)
	defer rm.Close()

	upload, ok := rm.GetUpload("sc-tail")
	if !ok {
		t.Fatal("upload not registered after recovery")
	}
	if got := upload.Meta().CommittedOffset; got != 64*1024 {
		t.Fatalf("expected recovery offset 64 KB, got %d", got)
	}

	// Verify the 32 KB uncommitted tail was truncated
	pfiPost, err := os.Stat(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if pfiPost.Size() != 64*1024 {
		t.Fatalf("expected part file truncated to 64 KB after recovery, got %d", pfiPost.Size())
	}

	// Resume from 64 KB
	fullPayload := crashPayload(128*1024, 'T')
	off, state, err := rm.IngestStream("sc-tail", 64*1024, bytes.NewReader(fullPayload[64*1024:]), nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if off != 128*1024 || state != ResumableAwaitingDigest {
		t.Fatalf("expected 128 KB awaiting_digest, got %d / %s", off, state)
	}

	expectedSum := sha256.Sum256(fullPayload)
	pub, err := rm.FinalizeComplete("sc-tail", hex.EncodeToString(expectedSum[:]))
	if err != nil {
		t.Fatalf("finalize failed: %v", err)
	}
	if pub == nil || pub.ExpectedSHA256 != hex.EncodeToString(expectedSum[:]) {
		t.Fatalf("unexpected publication record: %+v", pub)
	}
}
