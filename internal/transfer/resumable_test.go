package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuotaManager_ReservationAndMargin(t *testing.T) {
	tmpDir := t.TempDir()
	// Total simulated free disk space: 1000 MB
	simulatedFree := uint64(1000 * 1024 * 1024)
	safetyMargin := int64(500 * 1024 * 1024) // 500 MB

	qm := NewQuotaManager(tmpDir, safetyMargin, 5)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return simulatedFree, nil
	})

	// Effective available: 1000 MB - 500 MB = 500 MB
	// Requesting 400 MB should succeed
	err := qm.Reserve("sess-1", "up-1", 400*1024*1024)
	if err != nil {
		t.Fatalf("expected Reserve to succeed, got %v", err)
	}

	// Remaining available: 500 MB - 400 MB = 100 MB
	// Requesting 150 MB should fail with ErrInsufficientStorage
	err = qm.Reserve("sess-1", "up-2", 150*1024*1024)
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("expected ErrInsufficientStorage, got %v", err)
	}

	// Requesting 50 MB should succeed
	err = qm.Reserve("sess-1", "up-2", 50*1024*1024)
	if err != nil {
		t.Fatalf("expected Reserve of 50 MB to succeed, got %v", err)
	}

	// Commit 200 MB on up-1 -> frees 200 MB of reservation
	qm.UpdateOffset("up-1", 200*1024*1024)
	// Now up-1 has 200 MB outstanding, up-2 has 50 MB outstanding (total 250 MB)
	// Remaining: 500 MB - 250 MB = 250 MB
	err = qm.Reserve("sess-1", "up-3", 200*1024*1024)
	if err != nil {
		t.Fatalf("expected Reserve of 200 MB to succeed after offset update, got %v", err)
	}

	// Release up-1
	qm.Release("up-1")
	if qm.ActiveReservationsCount() != 2 {
		t.Fatalf("expected 2 active reservations, got %d", qm.ActiveReservationsCount())
	}
}

func TestQuotaManager_MaxConcurrentPerSession(t *testing.T) {
	tmpDir := t.TempDir()
	simulatedFree := uint64(100 * 1024 * 1024 * 1024) // 100 GB
	qm := NewQuotaManager(tmpDir, 500*1024*1024, 2)   // max 2 per session
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return simulatedFree, nil
	})

	if err := qm.Reserve("sess-1", "up-1", 1000); err != nil {
		t.Fatalf("up-1 reserve failed: %v", err)
	}
	if err := qm.Reserve("sess-1", "up-2", 1000); err != nil {
		t.Fatalf("up-2 reserve failed: %v", err)
	}
	// 3rd should fail with ErrQuotaExceeded
	if err := qm.Reserve("sess-1", "up-3", 1000); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}

	// Another session should still succeed
	if err := qm.Reserve("sess-2", "up-4", 1000); err != nil {
		t.Fatalf("sess-2 up-4 reserve failed: %v", err)
	}
}

func TestQuotaManager_RecoveryFromStaging(t *testing.T) {
	tmpDir := t.TempDir()
	stagingDir := filepath.Join(tmpDir, StagingDirName)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Write simulated .meta files
	meta1 := ResumableMeta{
		TransferID:      "up-rec-1",
		SessionKey:      "sess-1",
		TotalSize:       10 * 1024 * 1024,
		CommittedOffset: 3 * 1024 * 1024,
		State:           ResumableInterrupted,
	}
	d1, _ := json.Marshal(meta1)
	_ = os.WriteFile(filepath.Join(stagingDir, ".flitnix-part-up-rec-1.meta"), d1, 0o600)

	meta2 := ResumableMeta{
		TransferID:      "up-rec-2",
		SessionKey:      "sess-1",
		TotalSize:       5 * 1024 * 1024,
		CommittedOffset: 5 * 1024 * 1024,
		State:           ResumableCompleted, // Completed should not be re-reserved
	}
	d2, _ := json.Marshal(meta2)
	_ = os.WriteFile(filepath.Join(stagingDir, ".flitnix-part-up-rec-2.meta"), d2, 0o600)

	qm := NewQuotaManager(tmpDir, 500*1024*1024, 10)
	if err := qm.RecoverFromStaging(stagingDir); err != nil {
		t.Fatalf("RecoverFromStaging failed: %v", err)
	}

	if qm.ActiveReservationsCount() != 1 {
		t.Fatalf("expected 1 active reservation, got %d", qm.ActiveReservationsCount())
	}
	// Outstanding for up-rec-1: 10 MB - 3 MB = 7 MB
	if rem := qm.TotalReservedRemaining(); rem != 7*1024*1024 {
		t.Fatalf("expected 7 MB reserved, got %d", rem)
	}
}

func TestResumableManager_CreateAndIdempotency(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	qm := NewQuotaManager(tmpDir, 1024, 10)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024 * 1024, nil
	})
	rm := NewResumableManager(store, qm)
	defer rm.Close()

	meta1, err := rm.CreateUpload("sess-1", "up-test-1", "b-1", "it-1", "file.bin", "", 1000, tmpDir)
	if err != nil {
		t.Fatalf("CreateUpload failed: %v", err)
	}
	if meta1.TransferID != "up-test-1" || meta1.TotalSize != 1000 {
		t.Fatalf("unexpected meta: %+v", meta1)
	}

	// Repeated create with same ID must be idempotent
	meta2, err := rm.CreateUpload("sess-1", "up-test-1", "b-1", "it-1", "file.bin", "", 1000, tmpDir)
	if err != nil {
		t.Fatalf("repeated CreateUpload failed: %v", err)
	}
	if meta2.TransferID != meta1.TransferID {
		t.Fatalf("expected same transfer ID")
	}

	// Verify .part and .meta files exist on disk
	partPath := rm.partPath("up-test-1")
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf(".part file missing: %v", err)
	}
	metaPath := rm.metaPath("up-test-1")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf(".meta file missing: %v", err)
	}
}

func TestResumableManager_IngestStream_CheckpointsAndEOF(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	qm := NewQuotaManager(tmpDir, 1024, 10)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024 * 1024, nil
	})
	rm := NewResumableManager(store, qm)
	defer rm.Close()

	// Set small checkpoint threshold: 16 KB or 100 ms
	rm.SetCheckpointParams(16*1024, 100*time.Millisecond)

	payload := bytes.Repeat([]byte("A"), 64*1024) // 64 KB total
	_, err = rm.CreateUpload("sess-1", "up-stream-1", "b-1", "it-1", "test.bin", "", int64(len(payload)), tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// Stream first 32 KB chunk
	chunk1 := payload[:32*1024]
	var progressRecords []int64
	offset, state, err := rm.IngestStream("up-stream-1", 0, bytes.NewReader(chunk1), func(cur int64) {
		progressRecords = append(progressRecords, cur)
	})
	if err != nil {
		t.Fatalf("IngestStream chunk1 failed: %v", err)
	}
	if offset != 32*1024 {
		t.Fatalf("expected offset 32 KB, got %d", offset)
	}
	if state != ResumableActive {
		t.Fatalf("expected state %s, got %s", ResumableActive, state)
	}

	// Verify .meta on disk reflects committed offset 32 KB
	up, _ := rm.GetUpload("up-stream-1")
	if up.Meta().CommittedOffset != 32*1024 {
		t.Fatalf("meta committed offset expected 32 KB, got %d", up.Meta().CommittedOffset)
	}

	// Stream remaining 32 KB chunk
	chunk2 := payload[32*1024:]
	offset2, state2, err := rm.IngestStream("up-stream-1", 32*1024, bytes.NewReader(chunk2), nil)
	if err != nil {
		t.Fatalf("IngestStream chunk2 failed: %v", err)
	}
	if offset2 != 64*1024 {
		t.Fatalf("expected offset 64 KB, got %d", offset2)
	}
	// Reaching 100% declared bytes transitions to ResumableAwaitingDigest!
	if state2 != ResumableAwaitingDigest {
		t.Fatalf("expected state %s, got %s", ResumableAwaitingDigest, state2)
	}
}

func TestResumableManager_IngestStream_DuplicateCheckpointPrevention(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	qm := NewQuotaManager(tmpDir, 1024, 10)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024 * 1024, nil
	})
	rm := NewResumableManager(store, qm)
	defer rm.Close()

	// Checkpoint threshold exactly 10 KB
	rm.SetCheckpointParams(10*1024, 10*time.Second)

	payload := bytes.Repeat([]byte("B"), 10*1024) // Exactly 10 KB
	_, err = rm.CreateUpload("sess-1", "up-dup-1", "b-1", "it-1", "dup.bin", "", int64(len(payload)), tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// Ingest exact 10 KB
	offset, state, err := rm.IngestStream("up-dup-1", 0, bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 10*1024 || state != ResumableAwaitingDigest {
		t.Fatalf("unexpected offset %d or state %s", offset, state)
	}
}

func TestResumableManager_IngestStream_InterruptionHandling(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	qm := NewQuotaManager(tmpDir, 1024, 10)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024 * 1024, nil
	})
	rm := NewResumableManager(store, qm)
	defer rm.Close()

	// 50 KB declared
	_, err = rm.CreateUpload("sess-1", "up-intr-1", "b-1", "it-1", "intr.bin", "", 50*1024, tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// Provide an io.Reader that returns io.ErrUnexpectedEOF after 20 KB
	partialReader := io.MultiReader(
		bytes.NewReader(bytes.Repeat([]byte("C"), 20*1024)),
		&faultyReader{err: io.ErrUnexpectedEOF},
	)

	offset, state, err := rm.IngestStream("up-intr-1", 0, partialReader, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected ErrUnexpectedEOF, got %v", err)
	}
	if offset != 20*1024 {
		t.Fatalf("expected committed offset 20 KB, got %d", offset)
	}
	if state != ResumableInterrupted {
		t.Fatalf("expected state %s, got %s", ResumableInterrupted, state)
	}

	// Verify .meta has CommittedOffset = 20 KB and state = interrupted
	up, _ := rm.GetUpload("up-intr-1")
	if up.Meta().CommittedOffset != 20*1024 || up.Meta().State != ResumableInterrupted {
		t.Fatalf("expected 20 KB and interrupted in meta, got %d / %s", up.Meta().CommittedOffset, up.Meta().State)
	}

	// Resume from 20 KB with remaining 30 KB
	resumeReader := bytes.NewReader(bytes.Repeat([]byte("C"), 30*1024))
	offset2, state2, err := rm.IngestStream("up-intr-1", 20*1024, resumeReader, nil)
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if offset2 != 50*1024 || state2 != ResumableAwaitingDigest {
		t.Fatalf("expected 50 KB and awaiting_digest, got %d / %s", offset2, state2)
	}
}

type faultyReader struct {
	err error
}

func (f *faultyReader) Read(p []byte) (n int, err error) {
	return 0, f.err
}

func TestResumableManager_FinalizeComplete_SuccessAndIdempotency(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	qm := NewQuotaManager(tmpDir, 1024, 10)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024 * 1024, nil
	})
	rm := NewResumableManager(store, qm)
	defer rm.Close()

	payload := []byte("Hello, Resumable Flitnix World!")
	expectedHash := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedHash[:])

	_, err = rm.CreateUpload("sess-1", "up-comp-1", "b-1", "it-1", "hello.txt", "", int64(len(payload)), tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	_, state, err := rm.IngestStream("up-comp-1", 0, bytes.NewReader(payload), nil)
	if err != nil || state != ResumableAwaitingDigest {
		t.Fatalf("IngestStream failed: err=%v, state=%s", err, state)
	}

	// Finalize completion
	pub, err := rm.FinalizeComplete("up-comp-1", expectedHex)
	if err != nil {
		t.Fatalf("FinalizeComplete failed: %v", err)
	}
	if pub.ExpectedSHA256 != expectedHex || pub.Size != int64(len(payload)) {
		t.Fatalf("unexpected pub record: %+v", pub)
	}

	// Verify published file exists and matches payload
	publishedData, err := os.ReadFile(pub.TargetPath)
	if err != nil {
		t.Fatalf("published file not found: %v", err)
	}
	if !bytes.Equal(publishedData, payload) {
		t.Fatalf("published data mismatch")
	}

	// Test idempotency: repeated FinalizeComplete with same digest returns 200 OK without error
	pubRepeat, err := rm.FinalizeComplete("up-comp-1", expectedHex)
	if err != nil {
		t.Fatalf("repeated FinalizeComplete failed: %v", err)
	}
	if pubRepeat.TargetPath != pub.TargetPath {
		t.Fatalf("expected same target path on repeated complete")
	}

	// Conflicting digest returns ErrConflict
	conflictingHex := hex.EncodeToString(bytes.Repeat([]byte{0xaa}, 32))
	_, err = rm.FinalizeComplete("up-comp-1", conflictingHex)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict on conflicting digest, got %v", err)
	}
}

func TestResumableManager_FinalizeComplete_ChecksumMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	qm := NewQuotaManager(tmpDir, 1024, 10)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024 * 1024, nil
	})
	rm := NewResumableManager(store, qm)
	defer rm.Close()

	payload := []byte("Corrupted or Mismatched Content")
	_, err = rm.CreateUpload("sess-1", "up-bad-hash", "b-1", "it-1", "bad.txt", "", int64(len(payload)), tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	_, state, err := rm.IngestStream("up-bad-hash", 0, bytes.NewReader(payload), nil)
	if err != nil || state != ResumableAwaitingDigest {
		t.Fatalf("IngestStream failed: err=%v, state=%s", err, state)
	}

	wrongHex := hex.EncodeToString(bytes.Repeat([]byte{0x00}, 32))
	_, err = rm.FinalizeComplete("up-bad-hash", wrongHex)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}

	// Verify meta transitioned to failed
	up, _ := rm.GetUpload("up-bad-hash")
	if up.Meta().State != ResumableFailed {
		t.Fatalf("expected state %s, got %s", ResumableFailed, up.Meta().State)
	}
}

func TestResumableManager_StartupRecovery_AwaitingDigestPreserved(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	qm := NewQuotaManager(tmpDir, 1024, 10)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024 * 1024, nil
	})
	rm := NewResumableManager(store, qm)

	payload := []byte("Awaiting Digest Recovery Test")
	expectedHash := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(expectedHash[:])

	_, err = rm.CreateUpload("sess-1", "up-awaiting-1", "b-1", "it-1", "await.txt", "", int64(len(payload)), tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	_, state, err := rm.IngestStream("up-awaiting-1", 0, bytes.NewReader(payload), nil)
	if err != nil || state != ResumableAwaitingDigest {
		t.Fatalf("IngestStream failed: err=%v, state=%s", err, state)
	}

	// Close rm to simulate server stop/restart
	rm.Close()

	// New manager starts up
	rm2 := NewResumableManager(store, qm)
	defer rm2.Close()

	up, ok := rm2.GetUpload("up-awaiting-1")
	if !ok {
		t.Fatal("upload up-awaiting-1 not found after restart")
	}
	// Scenario 43: awaiting_digest MUST be preserved without premature publication or failure!
	if up.Meta().State != ResumableAwaitingDigest {
		t.Fatalf("expected state %s, got %s", ResumableAwaitingDigest, up.Meta().State)
	}

	// Client reconnects and finalizes with digest
	pub, err := rm2.FinalizeComplete("up-awaiting-1", expectedHex)
	if err != nil {
		t.Fatalf("FinalizeComplete after restart failed: %v", err)
	}
	if pub.TargetPath == "" {
		t.Fatal("empty publication target path")
	}
}

func TestResumableManager_LifecycleLock_Concurrency423(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	qm := NewQuotaManager(tmpDir, 1024, 10)
	qm.SetFreeSpaceFunc(func(dir string) (uint64, error) {
		return 100 * 1024 * 1024, nil
	})
	rm := NewResumableManager(store, qm)
	defer rm.Close()

	_, err = rm.CreateUpload("sess-1", "up-lock-1", "b-1", "it-1", "lock.txt", "", 100*1024, tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	upload, _ := rm.GetUpload("up-lock-1")

	// Acquire lock manually to simulate active in-flight PATCH
	if !upload.lifecycleMu.TryLock() {
		t.Fatal("initial TryLock failed")
	}

	// Concurrent IngestStream should return ErrUploadLocked
	_, _, err = rm.IngestStream("up-lock-1", 0, bytes.NewReader([]byte("data")), nil)
	if !errors.Is(err, ErrUploadLocked) {
		t.Fatalf("expected ErrUploadLocked on IngestStream, got %v", err)
	}

	// Concurrent FinalizeComplete should return ErrUploadLocked
	_, err = rm.FinalizeComplete("up-lock-1", hex.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	if !errors.Is(err, ErrUploadLocked) {
		t.Fatalf("expected ErrUploadLocked on FinalizeComplete, got %v", err)
	}

	// Concurrent CancelUpload should return ErrUploadLocked
	err = rm.CancelUpload("up-lock-1")
	if !errors.Is(err, ErrUploadLocked) {
		t.Fatalf("expected ErrUploadLocked on CancelUpload, got %v", err)
	}

	// Release lock
	upload.lifecycleMu.Unlock()

	// Now CancelUpload should succeed
	err = rm.CancelUpload("up-lock-1")
	if err != nil {
		t.Fatalf("CancelUpload after unlock failed: %v", err)
	}
}

func TestResumableManager_SubstitutionAttackDetection(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)

	transferID := "up-subst-1"
	targetPath := filepath.Join(tmpDir, "target.bin")
	payloadSize := int64(1024)
	expectedHex := hex.EncodeToString(bytes.Repeat([]byte{0x11}, 32))

	// Simulate crash state:
	// A publication record exists pointing to targetPath with expectedHex
	pub := PublicationRecord{
		TransferID:     transferID,
		ExpectedSHA256: expectedHex,
		TargetPath:     targetPath,
		Size:           payloadSize,
		PublishedAt:    time.Now(),
	}
	pubBytes, _ := json.Marshal(pub)
	_ = os.WriteFile(rm.publishedPath(transferID), pubBytes, 0o600)

	// An external process placed a same-size file with DIFFERENT content
	alienData := bytes.Repeat([]byte{0x99}, int(payloadSize))
	_ = os.WriteFile(targetPath, alienData, 0o600)

	// Meta file exists
	meta := ResumableMeta{
		TransferID:      transferID,
		DestinationDir:  tmpDir,
		TotalSize:       payloadSize,
		CommittedOffset: payloadSize,
		State:           ResumableVerifying,
	}
	d, _ := json.Marshal(meta)
	_ = os.WriteFile(rm.metaPath(transferID), d, 0o600)

	// Partial file exists on disk
	_ = os.WriteFile(rm.partPath(transferID), bytes.Repeat([]byte{0x11}, int(payloadSize)), 0o600)

	rm.Close()

	// New manager recovers on startup
	rm2 := NewResumableManager(store, nil)
	defer rm2.Close()

	// Scenario 41: The substitution attack must be detected and marked failed; target must not be overwritten
	up, ok := rm2.GetUpload(transferID)
	if !ok {
		t.Fatal("upload not recovered")
	}
	if up.Meta().State != ResumableFailed {
		t.Fatalf("expected state %s, got %s", ResumableFailed, up.Meta().State)
	}
}

func TestResumableManager_Scenario32_PartFileSyncFailure(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	totalSize := int64(2000)
	fullData := make([]byte, totalSize)
	for i := range fullData {
		fullData[i] = byte(i % 251)
	}
	expectedHash := sha256.Sum256(fullData)
	expectedHex := hex.EncodeToString(expectedHash[:])

	transferID := "scenario-32-test"
	_, err = rm.CreateUpload("sess-1", transferID, "b-1", "it-1", "s32.bin", "", totalSize, tmpDir)
	if err != nil {
		t.Fatalf("CreateUpload failed: %v", err)
	}

	// 1. Commit first 500 bytes successfully
	offset, state, err := rm.IngestStream(transferID, 0, bytes.NewReader(fullData[:500]), nil)
	if err != nil || offset != 500 || state != ResumableActive {
		t.Fatalf("first 500 bytes failed: offset=%d state=%s err=%v", offset, state, err)
	}

	// 2. Inject partFile.Sync failure
	injectedErr := errors.New("simulated disk sync error on part file")
	rm.SetPartFileSyncHook(func(f *os.File) error {
		return injectedErr
	})

	// 3. Attempt to stream next 500 bytes (500..1000)
	failOffset, _, failErr := rm.IngestStream(transferID, 500, bytes.NewReader(fullData[500:1000]), nil)
	if failErr == nil || !errors.Is(failErr, injectedErr) {
		t.Fatalf("expected injected partFile.Sync error, got: %v", failErr)
	}

	// Verify:
	// - Previous valid committed offset (500) remains authoritative
	up, ok := rm.GetUpload(transferID)
	if !ok {
		t.Fatal("upload not found")
	}
	if up.Meta().CommittedOffset != 500 {
		t.Fatalf("expected committed offset 500, got %d", up.Meta().CommittedOffset)
	}
	if failOffset != 500 {
		t.Fatalf("expected IngestStream returned offset 500, got %d", failOffset)
	}

	// - .meta file on disk still reflects 500
	metaData, err := os.ReadFile(rm.metaPath(transferID))
	if err != nil {
		t.Fatal(err)
	}
	var diskMeta ResumableMeta
	if err := json.Unmarshal(metaData, &diskMeta); err != nil {
		t.Fatal(err)
	}
	if diskMeta.CommittedOffset != 500 {
		t.Fatalf("expected on-disk committed offset 500, got %d", diskMeta.CommittedOffset)
	}

	// Clear fault hook
	rm.SetPartFileSyncHook(nil)

	// - Restart recovery reconciles .part and .meta safely
	rm.Close()
	rm2 := NewResumableManager(store, nil)
	defer rm2.Close()

	// Check on-disk .part size after recovery
	partFi, err := os.Stat(rm2.partPath(transferID))
	if err != nil {
		t.Fatal(err)
	}
	if partFi.Size() != 500 {
		t.Fatalf("expected .part truncated to committed offset 500 on restart recovery, got %d", partFi.Size())
	}

	// - Subsequent PATCH resumes from 500 and completes transfer
	remOffset, remState, remErr := rm2.IngestStream(transferID, 500, bytes.NewReader(fullData[500:]), nil)
	if remErr != nil {
		t.Fatalf("subsequent stream failed: %v", remErr)
	}
	if remOffset != totalSize || remState != ResumableAwaitingDigest {
		t.Fatalf("expected complete stream to %d awaiting_digest, got offset %d state %s", totalSize, remOffset, remState)
	}

	// Complete verification
	pub, err := rm2.FinalizeComplete(transferID, expectedHex)
	if err != nil {
		t.Fatalf("FinalizeComplete failed: %v", err)
	}
	if pub.ExpectedSHA256 != expectedHex {
		t.Fatalf("digest mismatch: expected %s, got %s", expectedHex, pub.ExpectedSHA256)
	}
}

func TestResumableManager_Scenario33_MetaTmpWriteAndRenameFailure(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	totalSize := int64(2000)
	fullData := make([]byte, totalSize)
	for i := range fullData {
		fullData[i] = byte((i * 7) % 251)
	}
	expectedHash := sha256.Sum256(fullData)
	expectedHex := hex.EncodeToString(expectedHash[:])

	transferID := "scenario-33-test"
	_, err = rm.CreateUpload("sess-1", transferID, "b-1", "it-1", "s33.bin", "", totalSize, tmpDir)
	if err != nil {
		t.Fatalf("CreateUpload failed: %v", err)
	}

	// 1. Commit first 600 bytes successfully
	offset, _, err := rm.IngestStream(transferID, 0, bytes.NewReader(fullData[:600]), nil)
	if err != nil || offset != 600 {
		t.Fatalf("first 600 bytes failed: offset=%d err=%v", offset, err)
	}

	// 2. Inject .meta.tmp atomic replacement failure
	injectedErr := errors.New("simulated meta rename failure")
	rm.SetMetaRenameHook(func(oldPath, newPath string) error {
		return injectedErr
	})

	// 3. Attempt to stream next 400 bytes (600..1000)
	failOffset, _, failErr := rm.IngestStream(transferID, 600, bytes.NewReader(fullData[600:1000]), nil)
	if failErr == nil || !errors.Is(failErr, injectedErr) {
		t.Fatalf("expected injected rename error, got: %v", failErr)
	}

	// Verify in-memory committed offset is still 600
	up, ok := rm.GetUpload(transferID)
	if !ok || up.Meta().CommittedOffset != 600 {
		t.Fatalf("expected in-memory committed offset 600, got %d", up.Meta().CommittedOffset)
	}
	if failOffset != 600 {
		t.Fatalf("expected return offset 600, got %d", failOffset)
	}

	// Disk meta must still be 600
	metaData, _ := os.ReadFile(rm.metaPath(transferID))
	var diskMeta ResumableMeta
	_ = json.Unmarshal(metaData, &diskMeta)
	if diskMeta.CommittedOffset != 600 {
		t.Fatalf("expected disk committed offset 600, got %d", diskMeta.CommittedOffset)
	}

	// Clear hook
	rm.SetMetaRenameHook(nil)

	// Next PATCH from 600 truncates trailing bytes in part and resumes cleanly
	remOffset, remState, remErr := rm.IngestStream(transferID, 600, bytes.NewReader(fullData[600:]), nil)
	if remErr != nil {
		t.Fatalf("retry stream failed: %v", remErr)
	}
	if remOffset != totalSize || remState != ResumableAwaitingDigest {
		t.Fatalf("expected complete stream to %d awaiting_digest, got offset %d state %s", totalSize, remOffset, remState)
	}

	pub, err := rm.FinalizeComplete(transferID, expectedHex)
	if err != nil {
		t.Fatalf("FinalizeComplete failed: %v", err)
	}
	if pub.ExpectedSHA256 != expectedHex {
		t.Fatalf("digest mismatch: expected %s, got %s", expectedHex, pub.ExpectedSHA256)
	}
}

func TestResumableManager_Scenario47_FinalEOFCheckpointFailure(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	totalSize := int64(1000)
	fullData := make([]byte, totalSize)
	for i := range fullData {
		fullData[i] = byte((i * 13) % 251)
	}
	expectedHash := sha256.Sum256(fullData)
	expectedHex := hex.EncodeToString(expectedHash[:])

	transferID := "scenario-47-test"
	_, err = rm.CreateUpload("sess-1", transferID, "b-1", "it-1", "s47.bin", "", totalSize, tmpDir)
	if err != nil {
		t.Fatalf("CreateUpload failed: %v", err)
	}

	// 1. Commit first 500 bytes successfully
	offset, _, err := rm.IngestStream(transferID, 0, bytes.NewReader(fullData[:500]), nil)
	if err != nil || offset != 500 {
		t.Fatalf("first 500 bytes failed: offset=%d err=%v", offset, err)
	}

	// 2. Inject partFile.Sync failure on final EOF checkpoint
	injectedErr := errors.New("simulated final EOF sync failure")
	rm.SetPartFileSyncHook(func(f *os.File) error {
		return injectedErr
	})

	// 3. Stream remaining bytes (500..1000) to clean EOF
	failOffset, _, failErr := rm.IngestStream(transferID, 500, bytes.NewReader(fullData[500:]), nil)
	if failErr == nil || !errors.Is(failErr, injectedErr) {
		t.Fatalf("expected injected EOF sync error, got: %v", failErr)
	}

	// Verify:
	// - State is NOT awaiting_digest
	// - Committed offset is still 500
	up, _ := rm.GetUpload(transferID)
	if up.Meta().State == ResumableAwaitingDigest {
		t.Fatal("state must NOT be awaiting_digest after EOF checkpoint failure")
	}
	if up.Meta().CommittedOffset != 500 {
		t.Fatalf("expected committed offset 500, got %d", up.Meta().CommittedOffset)
	}
	if failOffset != 500 {
		t.Fatalf("expected return offset 500, got %d", failOffset)
	}

	// Clear hook
	rm.SetPartFileSyncHook(nil)

	// 4. Retry remaining bytes from 500
	remOffset, remState, remErr := rm.IngestStream(transferID, 500, bytes.NewReader(fullData[500:]), nil)
	if remErr != nil {
		t.Fatalf("retry stream failed: %v", remErr)
	}
	if remOffset != totalSize || remState != ResumableAwaitingDigest {
		t.Fatalf("expected complete stream to %d awaiting_digest, got offset %d state %s", totalSize, remOffset, remState)
	}

	pub, err := rm.FinalizeComplete(transferID, expectedHex)
	if err != nil {
		t.Fatalf("FinalizeComplete failed: %v", err)
	}
	if pub.ExpectedSHA256 != expectedHex {
		t.Fatalf("digest mismatch: expected %s, got %s", expectedHex, pub.ExpectedSHA256)
	}
}

func TestResumableManager_CrossFilesystemRejection(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	// Inject cross-filesystem detection failure
	rm.SetSameFsCheck(func(p1, p2 string) (bool, error) {
		return false, nil
	})

	// CreateUpload must reject cross-filesystem destination
	_, err = rm.CreateUpload("sess-1", "up-xfs-1", "b-1", "it-1", "file.bin", "", 1000, filepath.Join(tmpDir, "foreign-mount"))
	if !errors.Is(err, ErrCrossFilesystem) {
		t.Fatalf("expected ErrCrossFilesystem on CreateUpload, got %v", err)
	}

	// Reset check to true to create upload
	rm.SetSameFsCheck(func(p1, p2 string) (bool, error) {
		return true, nil
	})
	_, err = rm.CreateUpload("sess-1", "up-xfs-2", "b-1", "it-2", "file.bin", "", 10, tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = rm.IngestStream("up-xfs-2", 0, bytes.NewReader(bytes.Repeat([]byte{1}, 10)), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Now set same-fs check to false for FinalizeComplete
	rm.SetSameFsCheck(func(p1, p2 string) (bool, error) {
		return false, nil
	})
	digest := sha256.Sum256(bytes.Repeat([]byte{1}, 10))
	_, err = rm.FinalizeComplete("up-xfs-2", hex.EncodeToString(digest[:]))
	if !errors.Is(err, ErrCrossFilesystem) {
		t.Fatalf("expected ErrCrossFilesystem on FinalizeComplete, got %v", err)
	}
}
