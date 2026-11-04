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
	"sync/atomic"
	"testing"
	"time"
)

// TestIntervalDurability_Matrix runs core durability, recovery, truncation,
// and resumability scenarios across 32 MiB, 64 MiB, and 128 MiB checkpoint intervals.
func TestIntervalDurability_Matrix(t *testing.T) {
	intervals := []struct {
		name          string
		intervalBytes int64
		payloadBytes  int64
	}{
		{
			name:          "32MiB Interval",
			intervalBytes: 32 * 1024 * 1024,
			payloadBytes:  96 * 1024 * 1024, // 3 checkpoints
		},
		{
			name:          "64MiB Interval",
			intervalBytes: 64 * 1024 * 1024,
			payloadBytes:  160 * 1024 * 1024, // 2 checkpoints + 32MB tail
		},
		{
			name:          "128MiB Interval",
			intervalBytes: 128 * 1024 * 1024,
			payloadBytes:  300 * 1024 * 1024, // 2 checkpoints + 44MB tail
		},
	}

	for _, tc := range intervals {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Run("InterruptedRecoveryAndResumption", func(t *testing.T) {
				root := t.TempDir()
				store, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm := NewResumableManager(store, nil)
				rm.SetPipelineCheckpoints(true)
				rm.SetCheckpointParams(tc.intervalBytes, time.Hour)

				uploadID := fmt.Sprintf("up-inter-%d", tc.intervalBytes)
				_, err = rm.CreateUpload("sess-1", uploadID, "b", "it", "test.bin", "", tc.payloadBytes, root)
				if err != nil {
					t.Fatal(err)
				}

				// Graceful client interruption: stream partway through
				interruptBytes := tc.intervalBytes + tc.intervalBytes/2
				payload := crashPayload(interruptBytes, 'A')

				pr, pw := io.Pipe()
				go func() {
					_, _ = pw.Write(payload)
					_ = pw.CloseWithError(io.ErrUnexpectedEOF)
				}()

				committed, state, streamErr := rm.IngestStream(uploadID, 0, pr, nil)
				if streamErr == nil {
					t.Fatal("expected error on truncated stream")
				}
				if state != ResumableInterrupted {
					t.Fatalf("expected state interrupted, got %s", state)
				}
				// On graceful stream termination, server commits all received bytes up to interruptBytes
				if committed != interruptBytes {
					t.Fatalf("expected committed offset %d, got %d", interruptBytes, committed)
				}

				rm.Close()

				// Simulate process restart and recovery
				store2, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm2 := NewResumableManager(store2, nil)
				rm2.SetPipelineCheckpoints(true)
				rm2.SetCheckpointParams(tc.intervalBytes, time.Hour)
				defer rm2.Close()

				recoveredUp, ok := rm2.GetUpload(uploadID)
				if !ok {
					t.Fatal("upload not recovered")
				}
				if recoveredUp.Meta().CommittedOffset != interruptBytes {
					t.Fatalf("expected recovered offset %d, got %d", interruptBytes, recoveredUp.Meta().CommittedOffset)
				}

				// Verify part file size matches committed bytes
				partPath := filepath.Join(root, StagingDirName, fmt.Sprintf(".flitnix-part-%s.part", uploadID))
				pfi, err := os.Stat(partPath)
				if err != nil {
					t.Fatal(err)
				}
				if pfi.Size() != interruptBytes {
					t.Fatalf("expected part file size %d after recovery, got %d", interruptBytes, pfi.Size())
				}

				// Resume from recovered offset to completion
				fullPayload := crashPayload(tc.payloadBytes, 'A')
				remainingRdr := bytes.NewReader(fullPayload[interruptBytes:])
				finalOff, finalState, err := rm2.IngestStream(uploadID, interruptBytes, remainingRdr, nil)
				if err != nil {
					t.Fatalf("resume failed: %v", err)
				}
				if finalOff != tc.payloadBytes || finalState != ResumableAwaitingDigest {
					t.Fatalf("expected complete at %d awaiting_digest, got %d / %s", tc.payloadBytes, finalOff, finalState)
				}

				// Finalize and verify full SHA-256
				h := sha256.Sum256(fullPayload)
				digest := hex.EncodeToString(h[:])
				rec, err := rm2.FinalizeComplete(uploadID, digest)
				if err != nil {
					t.Fatalf("FinalizeComplete failed: %v", err)
				}
				if rec.Size != tc.payloadBytes || rec.ExpectedSHA256 != digest {
					t.Fatalf("final record mismatch: %+v", rec)
				}
			})

			t.Run("UncommittedTailTruncationOnRecovery", func(t *testing.T) {
				root := t.TempDir()
				store, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm := NewResumableManager(store, nil)
				rm.SetPipelineCheckpoints(true)
				rm.SetCheckpointParams(tc.intervalBytes, time.Hour)

				uploadID := fmt.Sprintf("up-tail-%d", tc.intervalBytes)
				_, err = rm.CreateUpload("sess-1", uploadID, "b", "it", "tail.bin", "", tc.payloadBytes, root)
				if err != nil {
					t.Fatal(err)
				}

				// Ingest exactly 1 interval, triggering a clean checkpoint
				firstChunk := crashPayload(tc.intervalBytes, 'T')
				rdr := &fixedChunkReader{data: firstChunk, chunkSize: 32 * 1024}
				off, _, err := rm.IngestStream(uploadID, 0, rdr, nil)
				if err != nil {
					t.Fatalf("ingest failed: %v", err)
				}
				if off != tc.intervalBytes {
					t.Fatalf("expected %d committed, got %d", tc.intervalBytes, off)
				}

				rm.Close()

				// Simulate uncommitted tail bytes written past the checkpoint (as would happen in a power cut / kill -9)
				partPath := filepath.Join(root, StagingDirName, fmt.Sprintf(".flitnix-part-%s.part", uploadID))
				f, err := os.OpenFile(partPath, os.O_WRONLY|os.O_APPEND, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				uncommittedTail := crashPayload(16*1024*1024, 'X') // 16 MB uncommitted tail
				if _, err := f.Write(uncommittedTail); err != nil {
					t.Fatal(err)
				}
				f.Close()

				// Verify part file has dirty tail on disk before recovery
				pfiPre, err := os.Stat(partPath)
				if err != nil {
					t.Fatal(err)
				}
				expectedPreSize := tc.intervalBytes + 16*1024*1024
				if pfiPre.Size() != expectedPreSize {
					t.Fatalf("expected part file size %d before recovery, got %d", expectedPreSize, pfiPre.Size())
				}

				// Recover: startup reconciliation must discover that .meta records tc.intervalBytes and truncate the tail
				store2, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm2 := NewResumableManager(store2, nil)
				rm2.SetPipelineCheckpoints(true)
				defer rm2.Close()

				up, ok := rm2.GetUpload(uploadID)
				if !ok {
					t.Fatal("upload not recovered")
				}
				if up.Meta().CommittedOffset != tc.intervalBytes {
					t.Fatalf("expected recovered offset %d, got %d", tc.intervalBytes, up.Meta().CommittedOffset)
				}

				pfiPost, err := os.Stat(partPath)
				if err != nil {
					t.Fatal(err)
				}
				if pfiPost.Size() != tc.intervalBytes {
					t.Fatalf("expected part file truncated to %d, got %d", tc.intervalBytes, pfiPost.Size())
				}

				// Resumption from truncated boundary succeeds to 100%
				fullPayload := crashPayload(tc.payloadBytes, 'T')
				resOff, resState, err := rm2.IngestStream(uploadID, tc.intervalBytes, bytes.NewReader(fullPayload[tc.intervalBytes:]), nil)
				if err != nil {
					t.Fatalf("resumed stream failed: %v", err)
				}
				if resOff != tc.payloadBytes || resState != ResumableAwaitingDigest {
					t.Fatalf("expected %d awaiting_digest, got %d / %s", tc.payloadBytes, resOff, resState)
				}

				h := sha256.Sum256(fullPayload)
				digest := hex.EncodeToString(h[:])
				rec, err := rm2.FinalizeComplete(uploadID, digest)
				if err != nil {
					t.Fatalf("FinalizeComplete failed: %v", err)
				}
				if rec.ExpectedSHA256 != digest {
					t.Fatalf("digest mismatch: expected %s, got %s", digest, rec.ExpectedSHA256)
				}
			})

			t.Run("FaultInjection_MetaSyncErrorFailsClosed", func(t *testing.T) {
				root := t.TempDir()
				store, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm := NewResumableManager(store, nil)
				defer rm.Close()

				rm.SetPipelineCheckpoints(true)
				rm.SetCheckpointParams(tc.intervalBytes, time.Hour)

				uploadID := fmt.Sprintf("up-err-%d", tc.intervalBytes)
				_, err = rm.CreateUpload("sess-1", uploadID, "b", "it", "err.bin", "", tc.payloadBytes, root)
				if err != nil {
					t.Fatal(err)
				}

				// Fail on the 2nd checkpoint sync
				var syncCalls int32
				rm.SetMetaSyncHook(func(f *os.File) error {
					if atomic.AddInt32(&syncCalls, 1) == 2 {
						return errors.New("simulated I/O disk failure on meta.tmp sync")
					}
					return f.Sync()
				})

				payload := crashPayload(tc.payloadBytes, 'F')
				// Stream enough to trigger at least 2 checkpoints
				rdr := &fixedChunkReader{data: payload, chunkSize: 32 * 1024}
				_, _, streamErr := rm.IngestStream(uploadID, 0, rdr, nil)
				if streamErr == nil {
					t.Fatal("expected stream error for injected sync failure")
				}

				// Verify upload state preserved last good checkpoint (1st interval), not corrupted
				up, ok := rm.GetUpload(uploadID)
				if !ok {
					t.Fatal("upload missing")
				}
				if off := up.Meta().CommittedOffset; off != tc.intervalBytes {
					t.Fatalf("expected committed offset rolled back to 1st checkpoint %d, got %d", tc.intervalBytes, off)
				}
			})

			t.Run("CrashBoundary_AfterPartFileSync_BeforeMetaTmpSync", func(t *testing.T) {
				root := t.TempDir()
				store, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm := NewResumableManager(store, nil)
				rm.SetPipelineCheckpoints(true)
				rm.SetCheckpointParams(tc.intervalBytes, time.Hour)

				uploadID := fmt.Sprintf("up-b1-%d", tc.intervalBytes)
				_, err = rm.CreateUpload("sess-1", uploadID, "b", "it", "b1.bin", "", tc.payloadBytes, root)
				if err != nil {
					t.Fatal(err)
				}

				// Hook: writeMetaTmp fails, simulating crash after partFile.Sync() before meta.tmp is written
				rm.SetMetaWriteHook(func(f *os.File, data []byte) (int, error) {
					return 0, errors.New("simulated crash after partFile.Sync before meta.tmp write")
				})

				payload := crashPayload(tc.payloadBytes, '1')
				rdr := &fixedChunkReader{data: payload, chunkSize: 32 * 1024}
				_, _, _ = rm.IngestStream(uploadID, 0, rdr, nil)
				rm.Close()

				// Recovery: startup reconciliation discovers no valid .meta at intervalBytes; recovers 0
				store2, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm2 := NewResumableManager(store2, nil)
				rm2.SetPipelineCheckpoints(true)
				rm2.SetCheckpointParams(tc.intervalBytes, time.Hour)
				defer rm2.Close()

				up, ok := rm2.GetUpload(uploadID)
				if !ok {
					t.Fatal("upload not recovered")
				}
				if up.Meta().CommittedOffset != 0 {
					t.Fatalf("expected 0 recovered, got %d", up.Meta().CommittedOffset)
				}
				partPath := filepath.Join(root, StagingDirName, fmt.Sprintf(".flitnix-part-%s.part", uploadID))
				pfi, err := os.Stat(partPath)
				if err != nil {
					t.Fatal(err)
				}
				if pfi.Size() != 0 {
					t.Fatalf("expected part file truncated to 0, got %d", pfi.Size())
				}

				// Resumption from 0 succeeds to 100%
				off, state, err := rm2.IngestStream(uploadID, 0, bytes.NewReader(payload), nil)
				if err != nil || off != tc.payloadBytes || state != ResumableAwaitingDigest {
					t.Fatalf("resume failed: %v, off=%d, state=%s", err, off, state)
				}
				h := sha256.Sum256(payload)
				digest := hex.EncodeToString(h[:])
				rec, err := rm2.FinalizeComplete(uploadID, digest)
				if err != nil || rec.ExpectedSHA256 != digest {
					t.Fatalf("finalize failed: %v", err)
				}
			})

			t.Run("CrashBoundary_AfterMetaTmpSync_BeforeRename", func(t *testing.T) {
				root := t.TempDir()
				store, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm := NewResumableManager(store, nil)
				rm.SetPipelineCheckpoints(true)
				rm.SetCheckpointParams(tc.intervalBytes, time.Hour)

				uploadID := fmt.Sprintf("up-b2-%d", tc.intervalBytes)
				_, err = rm.CreateUpload("sess-1", uploadID, "b", "it", "b2.bin", "", tc.payloadBytes, root)
				if err != nil {
					t.Fatal(err)
				}

				// Hook: syncMetaTmp succeeds, but returns error right before rename
				rm.SetMetaSyncHook(func(f *os.File) error {
					if err := f.Sync(); err != nil {
						return err
					}
					return errors.New("simulated crash after meta.tmp sync before rename")
				})

				payload := crashPayload(tc.payloadBytes, '2')
				rdr := &fixedChunkReader{data: payload, chunkSize: 32 * 1024}
				_, _, _ = rm.IngestStream(uploadID, 0, rdr, nil)
				rm.Close()

				// Recovery: .meta.tmp must be purged; canonical .meta was at 0
				store2, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm2 := NewResumableManager(store2, nil)
				rm2.SetPipelineCheckpoints(true)
				rm2.SetCheckpointParams(tc.intervalBytes, time.Hour)
				defer rm2.Close()

				up, ok := rm2.GetUpload(uploadID)
				if !ok {
					t.Fatal("upload not recovered")
				}
				if up.Meta().CommittedOffset != 0 {
					t.Fatalf("expected 0 recovered, got %d", up.Meta().CommittedOffset)
				}

				// Resumption from 0 succeeds to 100%
				off, state, err := rm2.IngestStream(uploadID, 0, bytes.NewReader(payload), nil)
				if err != nil || off != tc.payloadBytes || state != ResumableAwaitingDigest {
					t.Fatalf("resume failed: %v, off=%d, state=%s", err, off, state)
				}
				h := sha256.Sum256(payload)
				digest := hex.EncodeToString(h[:])
				rec, err := rm2.FinalizeComplete(uploadID, digest)
				if err != nil || rec.ExpectedSHA256 != digest {
					t.Fatalf("finalize failed: %v", err)
				}
			})

			t.Run("CrashBoundary_AfterRename_BeforeSyncDir", func(t *testing.T) {
				root := t.TempDir()
				store, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm := NewResumableManager(store, nil)
				rm.SetPipelineCheckpoints(true)
				rm.SetCheckpointParams(tc.intervalBytes, time.Hour)

				uploadID := fmt.Sprintf("up-b3-%d", tc.intervalBytes)
				_, err = rm.CreateUpload("sess-1", uploadID, "b", "it", "b3.bin", "", tc.payloadBytes, root)
				if err != nil {
					t.Fatal(err)
				}

				// Hook: rename succeeds, then renameHook returns error simulating crash before syncDir
				rm.SetMetaRenameHook(func(oldPath, newPath string) error {
					if err := renameFileWithRetry(oldPath, newPath); err != nil {
						return err
					}
					return errors.New("simulated crash after rename before syncDir")
				})

				payload := crashPayload(tc.payloadBytes, '3')
				rdr := &fixedChunkReader{data: payload, chunkSize: 32 * 1024}
				_, _, _ = rm.IngestStream(uploadID, 0, rdr, nil)
				rm.Close()

				// Recovery: in process crash, VFS has preserved rename; recovered offset is tc.intervalBytes
				store2, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm2 := NewResumableManager(store2, nil)
				rm2.SetPipelineCheckpoints(true)
				rm2.SetCheckpointParams(tc.intervalBytes, time.Hour)
				defer rm2.Close()

				up, ok := rm2.GetUpload(uploadID)
				if !ok {
					t.Fatal("upload not recovered")
				}
				if up.Meta().CommittedOffset != tc.intervalBytes {
					t.Fatalf("expected %d recovered, got %d", tc.intervalBytes, up.Meta().CommittedOffset)
				}

				// Resumption from tc.intervalBytes succeeds to 100%
				off, state, err := rm2.IngestStream(uploadID, tc.intervalBytes, bytes.NewReader(payload[tc.intervalBytes:]), nil)
				if err != nil || off != tc.payloadBytes || state != ResumableAwaitingDigest {
					t.Fatalf("resume failed: %v, off=%d, state=%s", err, off, state)
				}
				h := sha256.Sum256(payload)
				digest := hex.EncodeToString(h[:])
				rec, err := rm2.FinalizeComplete(uploadID, digest)
				if err != nil || rec.ExpectedSHA256 != digest {
					t.Fatalf("finalize failed: %v", err)
				}
			})

			t.Run("CrashBoundary_AfterSyncDir_BeforeAck", func(t *testing.T) {
				root := t.TempDir()
				store, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm := NewResumableManager(store, nil)
				rm.SetPipelineCheckpoints(true)
				rm.SetCheckpointParams(tc.intervalBytes, time.Hour)

				uploadID := fmt.Sprintf("up-b4-%d", tc.intervalBytes)
				_, err = rm.CreateUpload("sess-1", uploadID, "b", "it", "b4.bin", "", tc.payloadBytes, root)
				if err != nil {
					t.Fatal(err)
				}

				// Hook: syncDir succeeds, then returns error simulating crash before HTTP ACK
				rm.SetMetaSyncDirHook(func(dir string) error {
					if err := syncDir(dir); err != nil {
						return err
					}
					return errors.New("simulated crash after syncDir before ACK")
				})

				payload := crashPayload(tc.payloadBytes, '4')
				rdr := &fixedChunkReader{data: payload, chunkSize: 32 * 1024}
				_, _, _ = rm.IngestStream(uploadID, 0, rdr, nil)
				rm.Close()

				// Recovery: directory is synced; recovered offset is tc.intervalBytes
				store2, err := NewStore(StoreConfig{Dir: root})
				if err != nil {
					t.Fatal(err)
				}
				rm2 := NewResumableManager(store2, nil)
				rm2.SetPipelineCheckpoints(true)
				rm2.SetCheckpointParams(tc.intervalBytes, time.Hour)
				defer rm2.Close()

				up, ok := rm2.GetUpload(uploadID)
				if !ok {
					t.Fatal("upload not recovered")
				}
				if up.Meta().CommittedOffset != tc.intervalBytes {
					t.Fatalf("expected %d recovered, got %d", tc.intervalBytes, up.Meta().CommittedOffset)
				}

				// Resumption from tc.intervalBytes succeeds to 100%
				off, state, err := rm2.IngestStream(uploadID, tc.intervalBytes, bytes.NewReader(payload[tc.intervalBytes:]), nil)
				if err != nil || off != tc.payloadBytes || state != ResumableAwaitingDigest {
					t.Fatalf("resume failed: %v, off=%d, state=%s", err, off, state)
				}
				h := sha256.Sum256(payload)
				digest := hex.EncodeToString(h[:])
				rec, err := rm2.FinalizeComplete(uploadID, digest)
				if err != nil || rec.ExpectedSHA256 != digest {
					t.Fatalf("finalize failed: %v", err)
				}
			})
		})
	}
}
