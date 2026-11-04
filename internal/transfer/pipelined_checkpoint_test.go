package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPipelinedCheckpoint_Success verifies that pipelined checkpoints stream correctly,
// persist checkpoints at boundaries, and successfully transition to awaiting_digest.
// On non-Linux platforms (Windows, macOS), where pipelining is intentionally disabled,
// it validates that SetPipelineCheckpoints(true) safely falls back to synchronous mode.
func TestPipelinedCheckpoint_Success(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	rm.SetPipelineCheckpoints(true)
	if platformSupportsPipelinedCheckpoints() {
		if !rm.PipelineCheckpoints() {
			t.Fatal("expected PipelineCheckpoints to be true on Linux")
		}
	} else {
		if rm.PipelineCheckpoints() {
			t.Fatal("expected PipelineCheckpoints to be false on non-Linux platform")
		}
	}

	// 512 KiB total with 64 KiB checkpoint threshold -> 8 checkpoints
	const totalSize = 512 * 1024
	rm.SetCheckpointParams(64*1024, time.Hour)

	_, err = rm.CreateUpload("sess", "pipe-1", "b", "it", "pipe1.bin", "", totalSize, root)
	if err != nil {
		t.Fatal(err)
	}

	data := crashPayload(totalSize, 'P')
	committed, state, err := rm.IngestStream("pipe-1", 0, bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("IngestStream failed: %v", err)
	}
	if committed != totalSize {
		t.Fatalf("expected committed offset %d, got %d", totalSize, committed)
	}
	if state != ResumableAwaitingDigest {
		t.Fatalf("expected state awaiting_digest, got %s", state)
	}

	// Verify on-disk .meta has totalSize
	metaPath := rm.metaPath("pipe-1")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(metaData, []byte(fmt.Sprintf(`"committedOffset":%d`, totalSize))) {
		t.Fatalf(".meta does not contain expected committed offset: %s", string(metaData))
	}

	// Finalize complete
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	rec, err := rm.FinalizeComplete("pipe-1", digest)
	if err != nil {
		t.Fatalf("FinalizeComplete failed: %v", err)
	}
	if rec.Size != totalSize {
		t.Fatalf("expected finalized size %d, got %d", totalSize, rec.Size)
	}
}

type fixedChunkReader struct {
	data      []byte
	chunkSize int
	pos       int
}

func (cr *fixedChunkReader) Read(p []byte) (int, error) {
	if cr.pos >= len(cr.data) {
		return 0, io.EOF
	}
	n := cr.chunkSize
	if len(cr.data)-cr.pos < n {
		n = len(cr.data) - cr.pos
	}
	if len(p) < n {
		n = len(p)
	}
	copy(p[:n], cr.data[cr.pos:cr.pos+n])
	cr.pos += n
	return n, nil
}

// TestPipelinedCheckpoint_FaultInjection_MetaSyncError verifies that if .meta.tmp Sync fails,
// the error propagates to the stream, and the previous durable checkpoint is preserved.
func TestPipelinedCheckpoint_FaultInjection_MetaSyncError(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	rm.SetPipelineCheckpoints(true)
	rm.SetCheckpointParams(64*1024, time.Hour)

	const totalSize = 256 * 1024
	_, err = rm.CreateUpload("sess", "pipe-sync-err", "b", "it", "syncerr.bin", "", totalSize, root)
	if err != nil {
		t.Fatal(err)
	}

	var syncCalls int32
	rm.SetMetaSyncHook(func(f *os.File) error {
		if atomic.AddInt32(&syncCalls, 1) == 2 {
			return errors.New("simulated I/O disk error on meta.tmp sync")
		}
		return f.Sync()
	})

	data := crashPayload(totalSize, 'E')
	rdr := &fixedChunkReader{data: data, chunkSize: 64 * 1024}
	_, _, streamErr := rm.IngestStream("pipe-sync-err", 0, rdr, nil)
	if streamErr == nil {
		t.Fatal("expected streamErr due to simulated meta.tmp sync error, got nil")
	}
	t.Logf("Stream error correctly caught: %v", streamErr)

	// Verify that the upload remains at the last good checkpoint (64 KiB), not corrupted
	up, ok := rm.GetUpload("pipe-sync-err")
	if !ok {
		t.Fatal("upload not found")
	}
	if off := up.Meta().CommittedOffset; off != 64*1024 {
		t.Fatalf("expected committed offset 64 KiB, got %d", off)
	}
}

// TestPipelinedCheckpoint_FaultInjection_RenameError verifies that if os.Rename fails,
// the stream aborts cleanly and recovery retains the earlier checkpoint.
func TestPipelinedCheckpoint_FaultInjection_RenameError(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	rm.SetPipelineCheckpoints(true)
	rm.SetCheckpointParams(64*1024, time.Hour)

	const totalSize = 256 * 1024
	_, err = rm.CreateUpload("sess", "pipe-rename-err", "b", "it", "renameerr.bin", "", totalSize, root)
	if err != nil {
		t.Fatal(err)
	}

	var renameCalls int32
	rm.SetMetaRenameHook(func(oldPath, newPath string) error {
		if atomic.AddInt32(&renameCalls, 1) == 2 {
			return errors.New("simulated rename failure")
		}
		return os.Rename(oldPath, newPath)
	})

	data := crashPayload(totalSize, 'R')
	rdr := &fixedChunkReader{data: data, chunkSize: 64 * 1024}
	_, _, streamErr := rm.IngestStream("pipe-rename-err", 0, rdr, nil)
	if streamErr == nil {
		t.Fatal("expected streamErr due to simulated rename error, got nil")
	}

	up, ok := rm.GetUpload("pipe-rename-err")
	if !ok {
		t.Fatal("upload not found")
	}
	if off := up.Meta().CommittedOffset; off != 64*1024 {
		t.Fatalf("expected committed offset 64 KiB, got %d", off)
	}
}

// TestPipelinedCheckpoint_BackpressureEnforcement verifies that when the background
// metadata task is delayed, the ingest loop enforces backpressure at the next boundary
// and does not allow queue overflow.
func TestPipelinedCheckpoint_BackpressureEnforcement(t *testing.T) {
	if !platformSupportsPipelinedCheckpoints() {
		t.Skip("skipping pipelined backpressure test: platform uses synchronous checkpointing")
	}
	root := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	rm.SetPipelineCheckpoints(true)
	rm.SetCheckpointParams(64*1024, time.Hour)

	const totalSize = 256 * 1024 // 4 checkpoints
	_, err = rm.CreateUpload("sess", "pipe-backpressure", "b", "it", "backpressure.bin", "", totalSize, root)
	if err != nil {
		t.Fatal(err)
	}

	var syncCount int32
	rm.SetMetaSyncHook(func(f *os.File) error {
		atomic.AddInt32(&syncCount, 1)
		time.Sleep(20 * time.Millisecond) // inject delay
		return f.Sync()
	})

	data := crashPayload(totalSize, 'B')
	rdr := &fixedChunkReader{data: data, chunkSize: 64 * 1024}
	start := time.Now()
	committed, state, err := rm.IngestStream("pipe-backpressure", 0, rdr, nil)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("IngestStream failed: %v", err)
	}
	if committed != totalSize {
		t.Fatalf("expected committed %d, got %d", totalSize, committed)
	}
	if state != ResumableAwaitingDigest {
		t.Fatalf("expected state awaiting_digest, got %s", state)
	}

	// Verify backpressure waited for all 4 checkpoints (at least 4 * 20ms = 80ms)
	t.Logf("Backpressure held stream correctly over %v (syncs=%d)", elapsed, syncCount)
	if elapsed < 70*time.Millisecond {
		t.Fatalf("expected backpressure delay >= 70ms, got %v", elapsed)
	}
}

// TestPipelinedCheckpoint_InterruptedStreamAndRecovery tests that an interrupted stream
// settles the pending checkpoint cleanly and allows subsequent resumption from disk.
func TestPipelinedCheckpoint_InterruptedStreamAndRecovery(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)

	rm.SetPipelineCheckpoints(true)
	rm.SetCheckpointParams(64*1024, time.Hour)

	const totalSize = 256 * 1024
	_, err = rm.CreateUpload("sess", "pipe-interrupt", "b", "it", "interrupt.bin", "", totalSize, root)
	if err != nil {
		t.Fatal(err)
	}

	// Stream 150 KiB (checkpoint at 64 KiB, checkpoint at 128 KiB, tail 22 KiB)
	pr, pw := io.Pipe()
	go func() {
		pw.Write(crashPayload(150*1024, 'I'))
		pw.CloseWithError(io.ErrUnexpectedEOF)
	}()

	committed, state, streamErr := rm.IngestStream("pipe-interrupt", 0, pr, nil)
	if streamErr == nil {
		t.Fatal("expected stream error for truncated body")
	}
	if state != ResumableInterrupted {
		t.Fatalf("expected state interrupted, got %s", state)
	}
	if committed < 128*1024 {
		t.Fatalf("expected committed offset at least 128 KiB, got %d", committed)
	}
	t.Logf("Interrupted stream committed %d bytes with state %s", committed, state)

	rm.Close()

	// Reopen store to simulate recovery
	store2, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm2 := NewResumableManager(store2, nil)
	rm2.SetPipelineCheckpoints(true)
	defer rm2.Close()

	up, ok := rm2.GetUpload("pipe-interrupt")
	if !ok {
		t.Fatal("upload not recovered")
	}
	recoveredOff := up.Meta().CommittedOffset
	if recoveredOff != committed {
		t.Fatalf("expected recovered offset %d, got %d", committed, recoveredOff)
	}

	// Resume from recovered offset to completion
	remaining := totalSize - recoveredOff
	resumedCommitted, resumedState, err := rm2.IngestStream("pipe-interrupt", recoveredOff, bytes.NewReader(crashPayload(remaining, 'I')), nil)
	if err != nil {
		t.Fatalf("resumed IngestStream failed: %v", err)
	}
	if resumedCommitted != totalSize {
		t.Fatalf("expected resumed committed %d, got %d", totalSize, resumedCommitted)
	}
	if resumedState != ResumableAwaitingDigest {
		t.Fatalf("expected state awaiting_digest, got %s", resumedState)
	}

	// Verify final digest
	fullData := crashPayload(totalSize, 'I')
	h := sha256.Sum256(fullData)
	digest := hex.EncodeToString(h[:])
	rec, err := rm2.FinalizeComplete("pipe-interrupt", digest)
	if err != nil {
		t.Fatalf("FinalizeComplete failed: %v", err)
	}
	if rec.Size != totalSize {
		t.Fatalf("expected size %d, got %d", totalSize, rec.Size)
	}
}

// TestSynchronousCheckpoint_Fallback verifies that when checkpoint pipelining is
// disabled (either natively on Windows/macOS or via SetPipelineCheckpoints(false)),
// ResumableManager operates correctly with durable synchronous checkpoints:
// 1. Correctly disables checkpoint pipelining.
// 2. Continues using synchronous checkpoint persistence.
// 3. Preserves the durable part-file synchronization order (part sync before meta sync before meta rename).
// 4. Persists canonical metadata correctly on disk.
// 5. Recovers interrupted transfers across restarts.
// 6. Truncates uncommitted trailing bytes cleanly.
// 7. Preserves final SHA-256 integrity on completion.
func TestSynchronousCheckpoint_Fallback(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	// 1. Test pipeline disablement / platform awareness:
	rm.SetPipelineCheckpoints(true)
	if !platformSupportsPipelinedCheckpoints() {
		// On non-Linux platforms (Windows, macOS), pipelining MUST remain disabled.
		if rm.PipelineCheckpoints() {
			t.Fatal("expected PipelineCheckpoints to be false on non-Linux platform")
		}
	} else {
		// On Linux, pipelining is supported, so explicitly disable it to test synchronous fallback.
		rm.SetPipelineCheckpoints(false)
		if rm.PipelineCheckpoints() {
			t.Fatal("expected PipelineCheckpoints to be false after SetPipelineCheckpoints(false)")
		}
	}

	// 2 & 3. Track synchronization order to prove synchronous ordering.
	var orderMu sync.Mutex
	var events []string
	rm.SetPartFileSyncHook(func(f *os.File) error {
		orderMu.Lock()
		events = append(events, "part_sync")
		orderMu.Unlock()
		return f.Sync()
	})
	rm.SetMetaSyncHook(func(f *os.File) error {
		orderMu.Lock()
		events = append(events, "meta_sync")
		orderMu.Unlock()
		return f.Sync()
	})
	rm.SetMetaRenameHook(func(oldPath, newPath string) error {
		orderMu.Lock()
		events = append(events, "meta_rename")
		orderMu.Unlock()
		return renameFileWithRetry(oldPath, newPath)
	})

	const checkpointInterval = 64 * 1024
	rm.SetCheckpointParams(checkpointInterval, time.Hour)

	const totalSize = 256 * 1024
	fullData := crashPayload(totalSize, 'S')
	uploadID := "sync-fallback-test"
	_, err = rm.CreateUpload("sess", uploadID, "b", "it", "sync_test.bin", "", totalSize, root)
	if err != nil {
		t.Fatal(err)
	}

	// Reset event log after initial upload creation so we track checkpointing order during streaming
	orderMu.Lock()
	events = nil
	orderMu.Unlock()

	// 4. Stream 100 KiB (one 64 KiB checkpoint, plus 36 KiB trailing uncommitted bytes) then interrupt
	interruptBytes := 100 * 1024
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(fullData[:interruptBytes])
		_ = pw.CloseWithError(io.ErrUnexpectedEOF)
	}()

	committed, state, streamErr := rm.IngestStream(uploadID, 0, pr, nil)
	if streamErr == nil {
		t.Fatal("expected stream error on interrupted body")
	}
	if state != ResumableInterrupted {
		t.Fatalf("expected state interrupted, got %s", state)
	}
	if committed != int64(interruptBytes) {
		t.Fatalf("expected committed offset %d, got %d", interruptBytes, committed)
	}

	// Verify synchronization order:
	orderMu.Lock()
	recorded := append([]string(nil), events...)
	orderMu.Unlock()
	if len(recorded) < 3 {
		t.Fatalf("expected at least 3 sync events, got %v", recorded)
	}
	// First 3 events must be: part_sync, meta_sync, meta_rename
	if recorded[0] != "part_sync" || recorded[1] != "meta_sync" || recorded[2] != "meta_rename" {
		t.Fatalf("unexpected synchronization order: %v (want [part_sync, meta_sync, meta_rename])", recorded[:3])
	}

	// Verify canonical metadata on disk at the committed boundary
	metaPath := rm.metaPath(uploadID)
	rawMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed to read .meta file: %v", err)
	}
	var meta ResumableMeta
	if err := json.Unmarshal(rawMeta, &meta); err != nil {
		t.Fatalf("failed to unmarshal .meta JSON: %v", err)
	}
	if meta.CommittedOffset != int64(interruptBytes) {
		t.Fatalf("meta CommittedOffset = %d, want %d", meta.CommittedOffset, interruptBytes)
	}
	if meta.State != ResumableInterrupted {
		t.Fatalf("meta State = %s, want interrupted", meta.State)
	}
	if meta.PrefixDigest == "" {
		t.Fatal("meta PrefixDigest is empty")
	}

	// 5 & 6. Simulate crash with uncommitted trailing bytes:
	// Append 16 KiB uncommitted dirty bytes to part file past the committed offset.
	rm.Close()

	partPath := rm.partPath(uploadID)
	pf, err := os.OpenFile(partPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	dirtyTail := crashPayload(16*1024, 'X')
	if _, err := pf.Write(dirtyTail); err != nil {
		t.Fatal(err)
	}
	pf.Close()

	// Verify part file has dirty tail before recovery (100 KiB + 16 KiB = 116 KiB)
	pfiPre, err := os.Stat(partPath)
	if err != nil || pfiPre.Size() != int64(interruptBytes+16*1024) {
		t.Fatalf("part file size before recovery = %d, want %d", pfiPre.Size(), interruptBytes+16*1024)
	}

	// Recovery on startup must detect committedOffset=100 KiB and truncate the 16 KiB dirty tail!
	store2, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	rm2 := NewResumableManager(store2, nil)
	defer rm2.Close()

	if err := rm2.RecoverOnStartup(); err != nil {
		t.Fatalf("RecoverOnStartup failed: %v", err)
	}

	upRecovered, ok := rm2.GetUpload(uploadID)
	if !ok {
		t.Fatal("upload not found after recovery")
	}
	if upRecovered.Meta().CommittedOffset != int64(interruptBytes) {
		t.Fatalf("recovered offset = %d, want %d", upRecovered.Meta().CommittedOffset, interruptBytes)
	}

	// Check truncation of part file back to 100 KiB
	pfiPost, err := os.Stat(partPath)
	if err != nil || pfiPost.Size() != int64(interruptBytes) {
		t.Fatalf("part file size after recovery = %d, want %d (uncommitted trailing bytes not truncated)", pfiPost.Size(), interruptBytes)
	}

	// 7. Resume from 100 KiB to completion (256 KiB) and verify final SHA-256 integrity
	rCommitted, rState, rErr := rm2.IngestStream(uploadID, int64(interruptBytes), bytes.NewReader(fullData[interruptBytes:]), nil)
	if rErr != nil {
		t.Fatalf("resumed stream failed: %v", rErr)
	}
	if rCommitted != totalSize || rState != ResumableAwaitingDigest {
		t.Fatalf("resumed stream returned committed=%d, state=%s", rCommitted, rState)
	}

	h := sha256.Sum256(fullData)
	expectedHex := hex.EncodeToString(h[:])
	rec, err := rm2.FinalizeComplete(uploadID, expectedHex)
	if err != nil {
		t.Fatalf("FinalizeComplete failed: %v", err)
	}
	if rec.Size != totalSize {
		t.Fatalf("finalized size = %d, want %d", rec.Size, totalSize)
	}

	publishedBytes, err := os.ReadFile(rec.TargetPath)
	if err != nil || !bytes.Equal(publishedBytes, fullData) {
		t.Fatalf("published data corrupted: err=%v", err)
	}
}
