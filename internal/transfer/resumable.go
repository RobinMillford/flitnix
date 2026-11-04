package transfer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/itsyamin/flitnix/internal/secfile"
)

var (
	// ErrUploadLocked indicates the upload resource is held by an active operation.
	ErrUploadLocked = errors.New("transfer: upload locked by concurrent operation")

	// ErrOffsetMismatch indicates the client PATCH offset does not match committed server offset.
	ErrOffsetMismatch = errors.New("transfer: upload offset mismatch")

	// ErrChecksumMismatch indicates the write-time accumulated SHA-256 does not match expected digest.
	ErrChecksumMismatch = errors.New("transfer: checksum mismatch")

	// ErrUploadCompleted indicates the upload was already completed.
	ErrUploadCompleted = errors.New("transfer: upload already completed")

	// ErrConflict indicates a conflicting state or mismatched completion digest.
	ErrConflict = errors.New("transfer: conflict")

	// ErrInvalidDigest indicates the expected SHA-256 is not a valid 64-character lowercase hex string.
	ErrInvalidDigest = errors.New("transfer: invalid digest format")

	// ErrSubstitutionDetected indicates an external same-size file substitution was detected during publication.
	ErrSubstitutionDetected = errors.New("transfer: external same-size file substitution detected")

	// ErrUploadExpired indicates the upload has exceeded its expiration window.
	ErrUploadExpired = errors.New("transfer: upload expired")

	// ErrPartialMissing indicates the staged .part file went missing while committed
	// metadata still records bytes. The committed prefix is unrecoverable and the
	// upload must restart from zero (HTTP 410 Gone).
	ErrPartialMissing = errors.New("transfer: staged part file missing for committed offset")

	// ErrInvalidState indicates the upload is not in a valid state for the requested operation.
	ErrInvalidState = errors.New("transfer: invalid upload state")

	// ErrCrossFilesystem indicates staging and target directory reside on different filesystems.
	ErrCrossFilesystem = errors.New("transfer: cross-filesystem destination not permitted for atomic publication")
)

const (
	// StagingDirName is the directory inside receive store for staged partial files and metadata.
	StagingDirName = ".flitnix-tmp"

	// DefaultCheckpointBytes is the default byte threshold for triggering durable checkpoints (32 MB).
	DefaultCheckpointBytes int64 = 32 * 1024 * 1024

	// DefaultCheckpointDuration is the default time threshold for triggering durable checkpoints (500 ms).
	DefaultCheckpointDuration = 500 * time.Millisecond

	// DefaultUploadTTL is the 24-hour expiration window for resumable uploads.
	DefaultUploadTTL = 24 * time.Hour
)

var hex64Regex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ResumableState represents the lifecycle state of a resumable upload.
type ResumableState string

const (
	ResumablePending           ResumableState = "pending"
	ResumableActive            ResumableState = "active"
	ResumableInterrupted       ResumableState = "interrupted"
	ResumableAwaitingDigest    ResumableState = "awaiting_digest"
	ResumableVerifying         ResumableState = "verifying"
	ResumableCompleted         ResumableState = "completed"
	ResumableFailed            ResumableState = "failed"
	ResumableCancelled         ResumableState = "cancelled"
	ResumablePausedStorageFull ResumableState = "paused_storage_full"
)

// HashStateRecord persists the serialized crypto/sha256 binary state.
type HashStateRecord struct {
	Version         int    `json:"version"`
	Format          string `json:"format"`
	CommittedOffset int64  `json:"committedOffset"`
	PrefixDigest    string `json:"prefixDigest"`
	StateBase64     string `json:"stateBase64"`
}

// ResumableMeta represents the sidecar metadata (.meta) persisted on disk.
type ResumableMeta struct {
	Version         int              `json:"version"`
	TransferID      string           `json:"transferId"`
	BatchID         string           `json:"batchId,omitempty"`
	ItemID          string           `json:"itemId,omitempty"`
	SessionKey      string           `json:"sessionKey"`
	SanitizedName   string           `json:"sanitizedName"`
	RelativePath    string           `json:"relativePath,omitempty"`
	DestinationDir  string           `json:"destinationDir"`
	TotalSize       int64            `json:"totalSize"`
	CommittedOffset int64            `json:"committedOffset"`
	ExpectedSHA256  string           `json:"expectedSHA256,omitempty"`
	PrefixDigest    string           `json:"prefixDigest,omitempty"`
	HashState       *HashStateRecord `json:"hashState,omitempty"`
	State           ResumableState   `json:"state"`
	CreatedAt       time.Time        `json:"createdAt"`
	UpdatedAt       time.Time        `json:"updatedAt"`
	ExpiresAt       time.Time        `json:"expiresAt"`
	FinalPath       string           `json:"finalPath,omitempty"`

	// QuarantineReason records why startup reconciliation refused to expose
	// this upload. A quarantined upload is never publishable and never
	// reports successful recovery; its bytes are preserved, not adopted.
	QuarantineReason string `json:"quarantineReason,omitempty"`
}

// PublicationRecord is recorded in .flitnix-tmp/.published-<transferId>.json before atomic rename.
type PublicationRecord struct {
	TransferID     string    `json:"transferId"`
	ExpectedSHA256 string    `json:"expectedSHA256"`
	TargetPath     string    `json:"targetPath"`
	Size           int64     `json:"size"`
	PublishedAt    time.Time `json:"publishedAt"`
}

// ResumableUpload holds the runtime state and exclusive lifecycle lock of a resumable upload.
type ResumableUpload struct {
	lifecycleMu sync.Mutex
	metaMu      sync.RWMutex
	meta        ResumableMeta
	hasher      hash.Hash
	partFile    *os.File
}

// Meta returns a thread-safe copy of the current upload metadata.
func (u *ResumableUpload) Meta() ResumableMeta {
	u.metaMu.RLock()
	defer u.metaMu.RUnlock()
	return u.meta
}

// UpdateMeta updates the upload metadata under the metadata mutex.
func (u *ResumableUpload) UpdateMeta(fn func(m *ResumableMeta)) {
	u.metaMu.Lock()
	defer u.metaMu.Unlock()
	fn(&u.meta)
}

// TryLock attempts to acquire the upload's exclusive lifecycle lock.
func (u *ResumableUpload) TryLock() bool {
	return u.lifecycleMu.TryLock()
}

// Lock acquires the upload's exclusive lifecycle lock.
func (u *ResumableUpload) Lock() {
	u.lifecycleMu.Lock()
}

// Unlock releases the upload's exclusive lifecycle lock.
func (u *ResumableUpload) Unlock() {
	u.lifecycleMu.Unlock()
}

type resumableFsHooks struct {
	syncPartFile func(f *os.File) error
	writeMetaTmp func(f *os.File, data []byte) (int, error)
	syncMetaTmp  func(f *os.File) error
	renameMeta   func(oldPath, newPath string) error
	syncDir      func(dir string) error
}

// ResumableManager manages persistent tus uploads and staging in .flitnix-tmp/.
type ResumableManager struct {
	mu                  sync.RWMutex
	store               *Store
	quota               *QuotaManager
	stagingDir          string
	uploads             map[string]*ResumableUpload
	checkpointBytes     int64
	checkpointDuration  time.Duration
	pipelineCheckpoints bool
	stopCleaner         chan struct{}
	checkSameFs         func(path1, path2 string) (bool, error)
	fsHooks             resumableFsHooks

	// recoveryMu guards the one-shot staging recovery so the constructor's
	// pass and the explicit startup-recovery pass cannot interleave.
	recoveryMu sync.Mutex
	recovered  bool

	// startupErr latches a failed staging recovery. It is never discarded: the
	// explicit startup recovery returns it and refuses readiness.
	startupErr error
}

// NewResumableManager initializes the resumable upload manager.
func NewResumableManager(store *Store, quota *QuotaManager) *ResumableManager {
	stagingDir := filepath.Join(store.Dir(), StagingDirName)
	// The staging directory is Flitnix's own: a fixed name inside the receive
	// root that Flitnix creates and owns, so replacing its DACL cannot revoke
	// access anyone else was granted. Best effort here because the constructor
	// cannot fail; CreateUpload re-applies the policy with error propagation
	// before the first staged byte exists.
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = secfile.ProtectDir(stagingDir)

	rm := &ResumableManager{
		store:               store,
		quota:               quota,
		stagingDir:          stagingDir,
		uploads:             make(map[string]*ResumableUpload),
		checkpointBytes:     DefaultCheckpointBytes,
		checkpointDuration:  DefaultCheckpointDuration,
		pipelineCheckpoints: false,
		stopCleaner:         make(chan struct{}),
		checkSameFs:         sameFilesystem,
	}

	// Recover existing staging uploads. A failure is retained, not discarded:
	// the explicit startup recovery calls RecoverOnStartup and refuses
	// readiness when it returns a non-nil error.
	if err := rm.RecoverOnStartup(); err != nil {
		rm.startupErr = err
	}

	// Start background cleanup worker for expired uploads
	go rm.cleanupLoop()

	return rm
}

// Close terminates background routines.
func (rm *ResumableManager) Close() {
	select {
	case <-rm.stopCleaner:
	default:
		close(rm.stopCleaner)
	}
}

// SetPartFileSyncHook sets a fault-injection hook for syncing .part files.
func (rm *ResumableManager) SetPartFileSyncHook(fn func(f *os.File) error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.fsHooks.syncPartFile = fn
}

// SetMetaWriteHook sets a fault-injection hook for writing .meta.tmp files.
func (rm *ResumableManager) SetMetaWriteHook(fn func(f *os.File, data []byte) (int, error)) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.fsHooks.writeMetaTmp = fn
}

// SetMetaSyncHook sets a fault-injection hook for syncing .meta.tmp files.
func (rm *ResumableManager) SetMetaSyncHook(fn func(f *os.File) error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.fsHooks.syncMetaTmp = fn
}

// SetMetaRenameHook sets a fault-injection hook for renaming .meta.tmp to .meta.
func (rm *ResumableManager) SetMetaRenameHook(fn func(oldPath, newPath string) error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.fsHooks.renameMeta = fn
}

// SetMetaSyncDirHook sets a fault-injection hook for syncing the staging directory.
func (rm *ResumableManager) SetMetaSyncDirHook(fn func(dir string) error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.fsHooks.syncDir = fn
}

// SetSameFsCheck sets a custom function to check if two paths are on the same filesystem.
func (rm *ResumableManager) SetSameFsCheck(fn func(p1, p2 string) (bool, error)) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.checkSameFs = fn
}

func (rm *ResumableManager) syncPartFile(f *os.File) error {
	rm.mu.RLock()
	hook := rm.fsHooks.syncPartFile
	rm.mu.RUnlock()
	if hook != nil {
		return hook(f)
	}
	return f.Sync()
}

// SetCheckpointParams allows configuring the checkpoint interval (e.g. for testing or tuning).
func (rm *ResumableManager) SetCheckpointParams(bytes int64, duration time.Duration) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if bytes > 0 {
		rm.checkpointBytes = bytes
	}
	if duration > 0 {
		rm.checkpointDuration = duration
	}
}

// SetPipelineCheckpoints toggles asynchronous pipelined metadata persistence.
// If the current platform does not support verified atomic metadata replacement
// and directory fsync (e.g. Windows or non-Linux OS), this is a no-op and the
// manager retains the safe synchronous checkpoint path.
func (rm *ResumableManager) SetPipelineCheckpoints(enabled bool) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.pipelineCheckpoints = enabled && platformSupportsPipelinedCheckpoints()
}

// PipelineCheckpoints reports whether asynchronous pipelined metadata persistence is active.
func (rm *ResumableManager) PipelineCheckpoints() bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.pipelineCheckpoints && platformSupportsPipelinedCheckpoints()
}

// StagingDir returns the absolute staging directory.
func (rm *ResumableManager) StagingDir() string {
	return rm.stagingDir
}

func (rm *ResumableManager) partPath(transferID string) string {
	return filepath.Join(rm.stagingDir, fmt.Sprintf(".flitnix-part-%s.part", transferID))
}

func (rm *ResumableManager) metaPath(transferID string) string {
	return filepath.Join(rm.stagingDir, fmt.Sprintf(".flitnix-part-%s.meta", transferID))
}

func (rm *ResumableManager) metaTmpPath(transferID string) string {
	return filepath.Join(rm.stagingDir, fmt.Sprintf(".flitnix-part-%s.meta.tmp", transferID))
}

func (rm *ResumableManager) publishedPath(transferID string) string {
	return filepath.Join(rm.stagingDir, fmt.Sprintf(".published-%s.json", transferID))
}

// writeDurableMeta atomically updates the sidecar .meta file.
func (rm *ResumableManager) writeDurableMeta(meta *ResumableMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	tmpPath := rm.metaTmpPath(meta.TransferID)
	finalPath := rm.metaPath(meta.TransferID)

	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("transfer: cannot open meta tmp: %w", err)
	}

	rm.mu.RLock()
	writeHook := rm.fsHooks.writeMetaTmp
	syncHook := rm.fsHooks.syncMetaTmp
	renameHook := rm.fsHooks.renameMeta
	syncDirHook := rm.fsHooks.syncDir
	rm.mu.RUnlock()

	var writeErr error
	if writeHook != nil {
		_, writeErr = writeHook(tmpFile, data)
	} else {
		_, writeErr = tmpFile.Write(data)
	}
	if writeErr != nil {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("transfer: cannot write meta tmp: %w", writeErr)
	}

	var syncErr error
	if syncHook != nil {
		syncErr = syncHook(tmpFile)
	} else {
		syncErr = tmpFile.Sync()
	}
	if syncErr != nil {
		tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("transfer: cannot sync meta tmp: %w", syncErr)
	}
	tmpFile.Close()

	var renameErr error
	if renameHook != nil {
		renameErr = renameHook(tmpPath, finalPath)
	} else {
		renameErr = renameFileWithRetry(tmpPath, finalPath)
	}
	if renameErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("transfer: cannot replace meta: %w", renameErr)
	}

	// POSIX directory sync
	var dirErr error
	if syncDirHook != nil {
		dirErr = syncDirHook(rm.stagingDir)
	} else {
		dirErr = syncDir(rm.stagingDir)
	}
	if dirErr != nil {
		return fmt.Errorf("transfer: sync staging dir: %w", dirErr)
	}

	return nil
}

// CreateUpload initializes or returns an existing resumable upload (idempotent).
func (rm *ResumableManager) CreateUpload(sessionKey, transferID, batchID, itemID, sanitizedName, relativePath string, totalSize int64, destinationDir string) (*ResumableMeta, error) {
	if destinationDir == "" {
		destinationDir = rm.store.Dir()
	}

	rm.mu.RLock()
	checkSameFs := rm.checkSameFs
	rm.mu.RUnlock()
	if checkSameFs != nil {
		same, err := checkSameFs(rm.stagingDir, destinationDir)
		if err != nil || !same {
			return nil, ErrCrossFilesystem
		}
	}

	rm.mu.Lock()
	if existing, ok := rm.uploads[transferID]; ok {
		rm.mu.Unlock()
		meta := existing.Meta()
		return &meta, nil
	}
	rm.mu.Unlock()

	// Reserve disk quota
	if rm.quota != nil {
		if err := rm.quota.Reserve(sessionKey, transferID, totalSize); err != nil {
			return nil, err
		}
	}

	now := time.Now()
	meta := ResumableMeta{
		Version:         2,
		TransferID:      transferID,
		BatchID:         batchID,
		ItemID:          itemID,
		SessionKey:      sessionKey,
		SanitizedName:   sanitizedName,
		RelativePath:    relativePath,
		DestinationDir:  destinationDir,
		TotalSize:       totalSize,
		CommittedOffset: 0,
		State:           ResumableActive,
		CreatedAt:       now,
		UpdatedAt:       now,
		ExpiresAt:       now.Add(DefaultUploadTTL),
	}

	// Ensure the staging directory exists and is private before the part file
	// is created. Windows ignores the 0700 mode, so a staging directory under
	// a wide parent would otherwise expose partially received bytes.
	if err := os.MkdirAll(rm.stagingDir, 0o700); err != nil {
		if rm.quota != nil {
			rm.quota.Release(transferID)
		}
		return nil, fmt.Errorf("transfer: create staging dir: %w", err)
	}
	if err := secfile.ProtectDir(rm.stagingDir); err != nil {
		if rm.quota != nil {
			rm.quota.Release(transferID)
		}
		return nil, fmt.Errorf("transfer: secure staging dir: %w", err)
	}

	// Create or ensure empty .part file
	partPath := rm.partPath(transferID)
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		if rm.quota != nil {
			rm.quota.Release(transferID)
		}
		return nil, fmt.Errorf("transfer: create part file: %w", err)
	}
	_ = f.Close()

	if err := rm.writeDurableMeta(&meta); err != nil {
		_ = os.Remove(partPath)
		if rm.quota != nil {
			rm.quota.Release(transferID)
		}
		return nil, err
	}

	upload := &ResumableUpload{
		meta:   meta,
		hasher: sha256.New(),
	}

	rm.mu.Lock()
	rm.uploads[transferID] = upload
	rm.mu.Unlock()

	return &meta, nil
}

// GetUpload retrieves an upload by transferID.
func (rm *ResumableManager) GetUpload(transferID string) (*ResumableUpload, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	u, ok := rm.uploads[transferID]
	return u, ok
}

// GetUploadByItem retrieves an upload by batchID and itemID.
func (rm *ResumableManager) GetUploadByItem(batchID, itemID string) (*ResumableUpload, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	for _, u := range rm.uploads {
		// Read the identity fields through the metadata mutex: a concurrent
		// durable checkpoint rewrites the whole ResumableMeta struct, so a
		// direct field read here races with it (caught by -race).
		m := u.Meta()
		if m.BatchID == batchID && m.ItemID == itemID {
			return u, true
		}
	}
	return nil, false
}

// restoreHasher restores the running SHA-256 hasher up to meta.CommittedOffset.
func (rm *ResumableManager) restoreHasher(meta *ResumableMeta, partFilePath string) (hash.Hash, error) {
	// Attempt binary unmarshaling if present
	if meta.HashState != nil && meta.HashState.StateBase64 != "" && meta.HashState.CommittedOffset == meta.CommittedOffset {
		stateBytes, err := base64.StdEncoding.DecodeString(meta.HashState.StateBase64)
		if err == nil {
			h := sha256.New()
			if u, ok := h.(interface{ UnmarshalBinary([]byte) error }); ok {
				if err := u.UnmarshalBinary(stateBytes); err == nil {
					if hex.EncodeToString(h.Sum(nil)) == meta.HashState.PrefixDigest {
						return h, nil
					}
				}
			}
		}
	}

	// Fallback: Re-hash committed prefix from .part file
	h := sha256.New()
	if meta.CommittedOffset <= 0 {
		return h, nil
	}

	f, err := os.Open(partFilePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	bufPtr := GetCopyBuffer()
	defer PutCopyBuffer(bufPtr)
	buf := *bufPtr

	var readTotal int64
	for readTotal < meta.CommittedOffset {
		toRead := int64(len(buf))
		if meta.CommittedOffset-readTotal < toRead {
			toRead = meta.CommittedOffset - readTotal
		}
		n, err := f.Read(buf[:toRead])
		if n > 0 {
			h.Write(buf[:n])
			readTotal += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && readTotal == meta.CommittedOffset {
				break
			}
			return nil, fmt.Errorf("transfer: reading partial for hash rebuild: %w", err)
		}
	}

	return h, nil
}

// executeDurableCheckpoint commits uncommitted bytes and updates .meta.
func (rm *ResumableManager) executeDurableCheckpoint(partFile *os.File, currentOffset int64, streamHasher hash.Hash, upload *ResumableUpload) error {
	// 1. Flush written data to physical disk
	if err := rm.syncPartFile(partFile); err != nil {
		return fmt.Errorf("transfer: partFile.Sync: %w", err)
	}

	// 2. Serialize SHA-256 hasher state
	var hashRec *HashStateRecord
	prefixDigest := hex.EncodeToString(streamHasher.Sum(nil))
	if bm, ok := streamHasher.(interface{ MarshalBinary() ([]byte, error) }); ok {
		stateBytes, err := bm.MarshalBinary()
		if err == nil {
			hashRec = &HashStateRecord{
				Version:         1,
				Format:          "crypto/sha256/binary-opaque",
				CommittedOffset: currentOffset,
				PrefixDigest:    prefixDigest,
				StateBase64:     base64.StdEncoding.EncodeToString(stateBytes),
			}
		}
	}

	// 3. Prepare candidate metadata without mutating upload.meta yet
	candidateMeta := upload.Meta()
	candidateMeta.CommittedOffset = currentOffset
	candidateMeta.PrefixDigest = prefixDigest
	candidateMeta.HashState = hashRec
	candidateMeta.UpdatedAt = time.Now()

	// 4. Atomically persist metadata to disk
	if err := rm.writeDurableMeta(&candidateMeta); err != nil {
		return fmt.Errorf("transfer: durable meta write: %w", err)
	}

	// 5. Only after disk sync and atomic replacement succeed, commit to memory!
	upload.UpdateMeta(func(meta *ResumableMeta) {
		*meta = candidateMeta
	})

	// 6. Update quota reservation offset
	if rm.quota != nil {
		rm.quota.UpdateOffset(candidateMeta.TransferID, currentOffset)
	}

	return nil
}

// IngestStream streams bytes for a PATCH request using the Write-Then-Hash loop.
func (rm *ResumableManager) IngestStream(transferID string, clientOffset int64, body io.Reader, onProgress func(offset int64)) (committedOffset int64, state ResumableState, err error) {
	upload, ok := rm.GetUpload(transferID)
	if !ok {
		return 0, "", ErrUploadNotFound
	}

	// Exclusive per-upload lifecycle lock: non-blocking TryLock returns 423 on contention
	if !upload.lifecycleMu.TryLock() {
		m := upload.Meta()
		return m.CommittedOffset, m.State, ErrUploadLocked
	}
	defer upload.lifecycleMu.Unlock()

	meta := upload.Meta()

	// Check if already completed
	if meta.State == ResumableCompleted {
		return meta.CommittedOffset, ResumableCompleted, ErrUploadCompleted
	}

	// Offset verification
	if clientOffset != meta.CommittedOffset {
		return meta.CommittedOffset, meta.State, ErrOffsetMismatch
	}

	// Expiration enforcement: once the TTL window has passed the upload is no longer
	// resumable. A zero ExpiresAt disables enforcement (uploads created outside
	// CreateUpload are never expired).
	if !meta.ExpiresAt.IsZero() && time.Now().After(meta.ExpiresAt) {
		return meta.CommittedOffset, meta.State, ErrUploadExpired
	}

	partPath := rm.partPath(transferID)

	// Missing staged part: with committed bytes on record, OpenFile below would
	// silently fabricate a sparse hole via O_CREATE. That would corrupt the upload,
	// so refuse with ErrPartialMissing when a committed prefix exists. A missing
	// part with zero committed bytes is a legitimate fresh create.
	if meta.CommittedOffset > 0 {
		if _, err := os.Stat(partPath); err != nil {
			if os.IsNotExist(err) {
				return meta.CommittedOffset, meta.State, ErrPartialMissing
			}
			return meta.CommittedOffset, meta.State, fmt.Errorf("transfer: stat part file: %w", err)
		}
	}

	// Ensure .part file is open
	partFile, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return meta.CommittedOffset, meta.State, fmt.Errorf("transfer: open part file: %w", err)
	}
	defer partFile.Close()

	// Check on-disk size: truncate any trailing uncommitted bytes beyond committedOffset
	fi, err := partFile.Stat()
	if err != nil {
		return meta.CommittedOffset, meta.State, err
	}
	if fi.Size() > meta.CommittedOffset {
		if err := partFile.Truncate(meta.CommittedOffset); err != nil {
			return meta.CommittedOffset, meta.State, fmt.Errorf("transfer: truncate trailing bytes: %w", err)
		}
	}

	// Seek to committed offset
	if _, err := partFile.Seek(meta.CommittedOffset, io.SeekStart); err != nil {
		return meta.CommittedOffset, meta.State, fmt.Errorf("transfer: seek part file: %w", err)
	}

	// Restore hasher up to committed offset
	streamHasher, err := rm.restoreHasher(&meta, partPath)
	if err != nil {
		return meta.CommittedOffset, meta.State, fmt.Errorf("transfer: restore hasher: %w", err)
	}

	currentOffset := meta.CommittedOffset
	var uncommittedBytes int64
	lastCheckpointTime := time.Now()

	bufPtr := GetCopyBuffer()
	defer PutCopyBuffer(bufPtr)
	buf := *bufPtr

	checkpointBytes := rm.checkpointBytes
	checkpointDuration := rm.checkpointDuration

	rm.mu.RLock()
	isPipelined := rm.pipelineCheckpoints
	rm.mu.RUnlock()

	type pipelinedCheckpoint struct {
		candidateMeta ResumableMeta
		errCh         chan error
	}
	var pendingCP *pipelinedCheckpoint

	awaitPendingCP := func() error {
		if pendingCP == nil {
			return nil
		}
		err := <-pendingCP.errCh
		if err != nil {
			pendingCP = nil
			return err
		}
		committedMeta := pendingCP.candidateMeta
		upload.UpdateMeta(func(m *ResumableMeta) {
			*m = committedMeta
		})
		if rm.quota != nil {
			rm.quota.UpdateOffset(committedMeta.TransferID, committedMeta.CommittedOffset)
		}
		pendingCP = nil
		return nil
	}
	defer func() {
		_ = awaitPendingCP()
	}()

	commitCheckpoint := func(offset int64, isFinal bool) error {
		if err := awaitPendingCP(); err != nil {
			return err
		}

		if err := rm.syncPartFile(partFile); err != nil {
			return fmt.Errorf("transfer: partFile.Sync: %w", err)
		}

		var hashRec *HashStateRecord
		prefixDigest := hex.EncodeToString(streamHasher.Sum(nil))
		if bm, ok := streamHasher.(interface{ MarshalBinary() ([]byte, error) }); ok {
			stateBytes, err := bm.MarshalBinary()
			if err == nil {
				hashRec = &HashStateRecord{
					Version:         1,
					Format:          "crypto/sha256/binary-opaque",
					CommittedOffset: offset,
					PrefixDigest:    prefixDigest,
					StateBase64:     base64.StdEncoding.EncodeToString(stateBytes),
				}
			}
		}

		candidateMeta := upload.Meta()
		candidateMeta.CommittedOffset = offset
		candidateMeta.PrefixDigest = prefixDigest
		candidateMeta.HashState = hashRec
		candidateMeta.UpdatedAt = time.Now()

		if !isPipelined || isFinal {
			if err := rm.writeDurableMeta(&candidateMeta); err != nil {
				return fmt.Errorf("transfer: durable meta write: %w", err)
			}
			upload.UpdateMeta(func(meta *ResumableMeta) {
				*meta = candidateMeta
			})
			if rm.quota != nil {
				rm.quota.UpdateOffset(candidateMeta.TransferID, offset)
			}
			return nil
		}

		task := &pipelinedCheckpoint{
			candidateMeta: candidateMeta,
			errCh:         make(chan error, 1),
		}
		pendingCP = task
		go func(cp *pipelinedCheckpoint) {
			cp.errCh <- rm.writeDurableMeta(&cp.candidateMeta)
		}(task)

		return nil
	}

	for {
		nRead, rerr := body.Read(buf)
		if nRead > 0 {
			// Step 1: Write bytes to partial file
			nWritten, werr := partFile.Write(buf[:nRead])
			if nWritten > 0 {
				// Step 2: Feed ONLY successfully written bytes into hasher!
				streamHasher.Write(buf[:nWritten])
				currentOffset += int64(nWritten)
				uncommittedBytes += int64(nWritten)
				if onProgress != nil {
					onProgress(currentOffset)
				}
			}
			if werr != nil {
				_ = awaitPendingCP()
				// Check for ENOSPC
				if errors.Is(werr, syscall.ENOSPC) {
					if uncommittedBytes > 0 {
						_ = commitCheckpoint(currentOffset, true)
					}
					var snap ResumableMeta
					upload.UpdateMeta(func(m *ResumableMeta) {
						m.State = ResumablePausedStorageFull
						snap = *m
					})
					_ = rm.writeDurableMeta(&snap)
					return snap.CommittedOffset, ResumablePausedStorageFull, ErrInsufficientStorage
				}
				// Other write error: commit valid portion and return
				if uncommittedBytes > 0 {
					_ = commitCheckpoint(currentOffset, true)
				}
				return upload.Meta().CommittedOffset, upload.Meta().State, werr
			}
		}

		// Step 3: Checkpoint threshold
		if uncommittedBytes >= checkpointBytes || time.Since(lastCheckpointTime) >= checkpointDuration {
			if syncErr := commitCheckpoint(currentOffset, false); syncErr != nil {
				return upload.Meta().CommittedOffset, upload.Meta().State, syncErr
			}
			uncommittedBytes = 0
			lastCheckpointTime = time.Now()
		}

		// Step 4: EOF or interruption
		if rerr != nil {
			if uncommittedBytes > 0 {
				// Commit trailing uncommitted bytes; NEVER ignore checkpoint error!
				if syncErr := commitCheckpoint(currentOffset, true); syncErr != nil {
					return upload.Meta().CommittedOffset, upload.Meta().State, syncErr
				}
				uncommittedBytes = 0
			} else {
				if syncErr := awaitPendingCP(); syncErr != nil {
					return upload.Meta().CommittedOffset, upload.Meta().State, syncErr
				}
			}

			if !errors.Is(rerr, io.EOF) {
				// Premature socket close or cancellation (io.ErrUnexpectedEOF, context.Canceled, etc.)
				var snap ResumableMeta
				upload.UpdateMeta(func(m *ResumableMeta) {
					m.State = ResumableInterrupted
					snap = *m
				})
				if syncErr := rm.writeDurableMeta(&snap); syncErr != nil {
					return snap.CommittedOffset, snap.State, fmt.Errorf("transfer: durable meta write on interruption: %w", syncErr)
				}
				return snap.CommittedOffset, ResumableInterrupted, rerr
			}

			// Clean EOF of request body reached
			break
		}
	}

	// Ensure all background checkpoint work has settled
	if err := awaitPendingCP(); err != nil {
		return upload.Meta().CommittedOffset, upload.Meta().State, err
	}

	// Update in-memory hasher reference
	upload.hasher = streamHasher

	// Transition to awaiting_digest when full declared size is reached
	if currentOffset == meta.TotalSize {
		candidate := upload.Meta()
		candidate.State = ResumableAwaitingDigest
		if err := rm.writeDurableMeta(&candidate); err != nil {
			return candidate.CommittedOffset, candidate.State, err
		}
		upload.UpdateMeta(func(m *ResumableMeta) {
			m.State = ResumableAwaitingDigest
		})
		return currentOffset, ResumableAwaitingDigest, nil
	}

	upload.UpdateMeta(func(m *ResumableMeta) {
		m.State = ResumableActive
	})
	return currentOffset, ResumableActive, nil
}

// FinalizeComplete verifies the expected SHA-256 digest and safely publishes the file.
func (rm *ResumableManager) FinalizeComplete(transferID, expectedSHA256 string) (*PublicationRecord, error) {
	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	if !hex64Regex.MatchString(expectedSHA256) {
		return nil, ErrInvalidDigest
	}

	upload, ok := rm.GetUpload(transferID)
	if !ok {
		return nil, ErrUploadNotFound
	}

	// Exclusive per-upload lock
	if !upload.lifecycleMu.TryLock() {
		return nil, ErrUploadLocked
	}
	defer upload.lifecycleMu.Unlock()

	meta := upload.Meta()

	// Check idempotent repeat
	if meta.State == ResumableCompleted {
		if strings.EqualFold(meta.ExpectedSHA256, expectedSHA256) {
			return &PublicationRecord{
				TransferID:     transferID,
				ExpectedSHA256: meta.ExpectedSHA256,
				TargetPath:     meta.FinalPath,
				Size:           meta.TotalSize,
				PublishedAt:    meta.UpdatedAt,
			}, nil
		}
		return nil, ErrConflict
	}

	if meta.State != ResumableAwaitingDigest && meta.State != ResumableVerifying {
		return nil, fmt.Errorf("%w: current state is %s", ErrInvalidState, meta.State)
	}

	var metaSnap ResumableMeta
	upload.UpdateMeta(func(m *ResumableMeta) {
		m.State = ResumableVerifying
		m.ExpectedSHA256 = expectedSHA256
		metaSnap = *m
	})
	_ = rm.writeDurableMeta(&metaSnap)

	// Determine write-time SHA-256
	partPath := rm.partPath(transferID)
	var actualDigest string
	if upload.hasher != nil && meta.CommittedOffset == meta.TotalSize {
		actualDigest = hex.EncodeToString(upload.hasher.Sum(nil))
	} else if meta.PrefixDigest != "" && meta.CommittedOffset == meta.TotalSize {
		actualDigest = meta.PrefixDigest
	} else {
		// Calculate digest from .part
		h, err := rm.restoreHasher(&meta, partPath)
		if err != nil {
			return nil, err
		}
		actualDigest = hex.EncodeToString(h.Sum(nil))
	}

	if actualDigest != expectedSHA256 {
		upload.UpdateMeta(func(m *ResumableMeta) {
			m.State = ResumableFailed
			metaSnap = *m
		})
		_ = rm.writeDurableMeta(&metaSnap)
		return nil, ErrChecksumMismatch
	}

	// Resolve target publication path
	destDir := meta.DestinationDir
	if destDir == "" {
		destDir = rm.store.Dir()
	}

	rm.mu.RLock()
	checkSameFs := rm.checkSameFs
	rm.mu.RUnlock()
	if checkSameFs != nil {
		same, err := checkSameFs(rm.stagingDir, destDir)
		if err != nil || !same {
			return nil, ErrCrossFilesystem
		}
	}

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return nil, fmt.Errorf("transfer: create destination dir: %w", err)
	}
	// destDir is user-selected: a configured output folder, a documents
	// folder, or a folder chosen per transfer. Its DACL is deliberately left
	// alone, because replacing it would revoke access the user granted to
	// other people or other programs. Confidentiality is enforced on the file
	// being published instead, below.

	var targetPath string
	if meta.RelativePath != "" {
		targetPath = filepath.Join(destDir, filepath.FromSlash(meta.RelativePath))
		_ = os.MkdirAll(filepath.Dir(targetPath), 0o700)
	} else {
		targetPath = filepath.Join(destDir, meta.SanitizedName)
	}

	// Check if target file already exists
	if fi, err := os.Stat(targetPath); err == nil {
		// File exists; compute hash to check if it's already this exact file
		if fi.Size() == meta.TotalSize {
			existingHash, hashErr := computeFileSHA256(targetPath)
			if hashErr == nil && existingHash == expectedSHA256 {
				// Identical target already published
				_ = os.Remove(partPath)
				_ = os.Remove(rm.metaPath(transferID))
				_ = os.Remove(rm.metaTmpPath(transferID))
				upload.UpdateMeta(func(m *ResumableMeta) {
					m.State = ResumableCompleted
					m.FinalPath = targetPath
				})
				if rm.quota != nil {
					rm.quota.Release(transferID)
				}
				return &PublicationRecord{
					TransferID:     transferID,
					ExpectedSHA256: expectedSHA256,
					TargetPath:     targetPath,
					Size:           meta.TotalSize,
					PublishedAt:    time.Now(),
				}, nil
			}
		}
		// If collision, choose a collision-safe variant
		targetPath = rm.resolveCollisionVariant(destDir, meta.SanitizedName)
	}

	// 1. Secure the file while it is still in Flitnix's own staging directory.
	//
	// The DACL is marked protected, so moving the file into destDir cannot make
	// it inherit that directory's wider access, and the descriptor travels with
	// the file through the rename below - publication stays atomic and no ACL
	// work happens per chunk. A failure here stops publication rather than
	// publishing a file with inherited broad access.
	if err := secfile.ProtectFile(partPath); err != nil {
		return nil, fmt.Errorf("transfer: secure published file: %w", err)
	}

	// 2. Write durable publication record
	pubRecord := PublicationRecord{
		TransferID:     transferID,
		ExpectedSHA256: expectedSHA256,
		TargetPath:     targetPath,
		Size:           meta.TotalSize,
		PublishedAt:    time.Now(),
	}
	pubBytes, err := json.Marshal(pubRecord)
	if err != nil {
		return nil, err
	}
	pubPath := rm.publishedPath(transferID)
	if err := os.WriteFile(pubPath, pubBytes, 0o600); err != nil {
		return nil, fmt.Errorf("transfer: write publication record: %w", err)
	}

	// 3. Atomic rename from .part to targetPath
	if err := renameFileWithRetry(partPath, targetPath); err != nil {
		_ = os.Remove(pubPath)
		return nil, fmt.Errorf("transfer: publish rename: %w", err)
	}

	// 4. Clean up staging files
	_ = os.Remove(pubPath)
	_ = os.Remove(rm.metaPath(transferID))
	_ = os.Remove(rm.metaTmpPath(transferID))

	upload.UpdateMeta(func(m *ResumableMeta) {
		m.State = ResumableCompleted
		m.FinalPath = targetPath
	})

	if rm.quota != nil {
		rm.quota.Release(transferID)
	}

	return &pubRecord, nil
}

// resolveCollisionVariant finds an unused filename with (1), (2), etc.
func (rm *ResumableManager) resolveCollisionVariant(dir, name string) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext))
}

// CancelUpload coordinates upload cancellation and purges staging files.
func (rm *ResumableManager) CancelUpload(transferID string) error {
	upload, ok := rm.GetUpload(transferID)
	if !ok {
		// If not in memory, clean up any lingering disk files
		_ = os.Remove(rm.partPath(transferID))
		_ = os.Remove(rm.metaPath(transferID))
		_ = os.Remove(rm.metaTmpPath(transferID))
		_ = os.Remove(rm.publishedPath(transferID))
		if rm.quota != nil {
			rm.quota.Release(transferID)
		}
		return nil
	}

	if !upload.lifecycleMu.TryLock() {
		return ErrUploadLocked
	}
	defer upload.lifecycleMu.Unlock()

	upload.UpdateMeta(func(m *ResumableMeta) {
		m.State = ResumableCancelled
	})
	_ = os.Remove(rm.partPath(transferID))
	_ = os.Remove(rm.metaPath(transferID))
	_ = os.Remove(rm.metaTmpPath(transferID))
	_ = os.Remove(rm.publishedPath(transferID))

	if rm.quota != nil {
		rm.quota.Release(transferID)
	}

	rm.mu.Lock()
	delete(rm.uploads, transferID)
	rm.mu.Unlock()

	return nil
}

// RecoverOnStartup scans the staging directory, restores records, and reconciles state.
func (rm *ResumableManager) RecoverOnStartup() error {
	rm.recoveryMu.Lock()
	defer rm.recoveryMu.Unlock()
	if rm.startupErr != nil {
		return rm.startupErr
	}
	if rm.recovered {
		return nil
	}
	if err := rm.recoverStaging(); err != nil {
		rm.startupErr = fmt.Errorf("transfer: staging recovery: %w", err)
		return rm.startupErr
	}
	rm.recovered = true
	return nil
}

// recoverStaging is the one-shot scan. Its error is fatal to startup: an
// unknown staging state must not be served.
func (rm *ResumableManager) recoverStaging() error {
	entries, err := os.ReadDir(rm.stagingDir)
	if err != nil {
		// Only a genuinely absent staging directory is a clean start. The
		// error itself cannot decide that: on Windows ReadDir on a regular
		// file reports ERROR_PATH_NOT_FOUND, which os.IsNotExist considers a
		// missing path, so the platform's own classification would let a
		// corrupt staging path pass startup recovery. Lstat answers the real
		// question, and every other failure stays fatal (fail closed).
		if _, statErr := os.Lstat(rm.stagingDir); os.IsNotExist(statErr) {
			return nil
		}
		return err
	}

	// 1. Process any .published-*.json records first
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".published-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		pubFile := filepath.Join(rm.stagingDir, entry.Name())
		data, err := os.ReadFile(pubFile)
		if err != nil {
			continue
		}
		var pub PublicationRecord
		if err := json.Unmarshal(data, &pub); err != nil {
			continue
		}

		transferID := pub.TransferID
		partPath := rm.partPath(transferID)

		// Check destination file
		if fi, err := os.Stat(pub.TargetPath); err == nil && fi.Size() == pub.Size {
			actualSHA, err := computeFileSHA256(pub.TargetPath)
			if err == nil && actualSHA == pub.ExpectedSHA256 {
				// Publication was successful before crash; clean up staging files
				_ = os.Remove(partPath)
				_ = os.Remove(rm.metaPath(transferID))
				_ = os.Remove(rm.metaTmpPath(transferID))
				_ = os.Remove(pubFile)
				continue
			} else {
				// Target exists with same size, but hash does not match -> Substitution attack!
				_ = os.Remove(pubFile)
				// Leave .part and mark failed in .meta
				metaPath := rm.metaPath(transferID)
				if mData, mErr := os.ReadFile(metaPath); mErr == nil {
					var m ResumableMeta
					if json.Unmarshal(mData, &m) == nil {
						m.State = ResumableFailed
						_ = rm.writeDurableMeta(&m)
					}
				}
				continue
			}
		}

		// Destination does not exist; if .part exists and matches, complete publication
		if pfi, err := os.Stat(partPath); err == nil && pfi.Size() == pub.Size {
			partSHA, err := computeFileSHA256(partPath)
			if err == nil && partSHA == pub.ExpectedSHA256 {
				_ = renameFileWithRetry(partPath, pub.TargetPath)
				_ = os.Remove(pubFile)
				_ = os.Remove(rm.metaPath(transferID))
				_ = os.Remove(rm.metaTmpPath(transferID))
				continue
			}
		}
	}

	// 2. Clean up any unrenamed orphaned temporary metadata (.meta.tmp) from pre-crash attempts
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".meta.tmp") {
			_ = os.Remove(filepath.Join(rm.stagingDir, entry.Name()))
		}
	}

	// 3. Load all canonical .meta files
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".meta") {
			continue
		}
		metaFile := filepath.Join(rm.stagingDir, entry.Name())
		data, err := os.ReadFile(metaFile)
		if err != nil {
			continue
		}

		var meta ResumableMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			// Corrupted metadata: quarantine
			_ = os.Rename(metaFile, metaFile+".corrupted")
			continue
		}

		partPath := rm.partPath(meta.TransferID)

		// Check if partial file exists
		pfi, err := os.Stat(partPath)
		if err != nil {
			// Part file missing: register the upload anyway so HEAD exposes the
			// recorded metadata and PATCH fails with ErrPartialMissing (410 Gone)
			// instead of 404 Not Found. Fresh creates (CommittedOffset == 0) can
			// still recreate the part file on the next PATCH via O_CREATE.
			if meta.State == ResumableActive {
				meta.State = ResumableInterrupted
				_ = rm.writeDurableMeta(&meta)
			}
			upload := &ResumableUpload{
				meta: meta,
			}
			rm.mu.Lock()
			rm.uploads[meta.TransferID] = upload
			rm.mu.Unlock()
			continue
		}

		// Truncate any trailing uncommitted bytes beyond CommittedOffset
		if pfi.Size() > meta.CommittedOffset {
			if f, err := os.OpenFile(partPath, os.O_RDWR, 0o600); err == nil {
				_ = f.Truncate(meta.CommittedOffset)
				_ = f.Close()
			}
		}

		// Reconcile state
		if meta.State == ResumableActive {
			meta.State = ResumableInterrupted
			_ = rm.writeDurableMeta(&meta)
		}
		// Notice: ResumableAwaitingDigest is PRESERVED without premature publication or failure!

		upload := &ResumableUpload{
			meta: meta,
		}

		rm.mu.Lock()
		rm.uploads[meta.TransferID] = upload
		rm.mu.Unlock()
	}

	// 3. Reconstruct quota reservations
	if rm.quota != nil {
		_ = rm.quota.RecoverFromStaging(rm.stagingDir)
	}

	return nil
}

// Reconcile authorises every restored upload against the trusted durable
// ledger. authorize must return an error for any upload whose owner, batch,
// item, approval or destination binding cannot be proven.
//
// An upload that fails authorization is quarantined: its metadata is marked
// failed with the reason, its .part and .meta are renamed aside (preserved,
// never deleted), it is removed from the live index, and its quota
// reservation is released. It can never be published and never reports a
// successful recovery. The function returns how many uploads were
// quarantined.
//
// An upload that cannot be locked is reported as an error: silently skipping
// it would leave unverified state addressable by a client.
func (rm *ResumableManager) Reconcile(authorize func(ResumableMeta) error) (int, error) {
	if authorize == nil {
		return 0, errors.New("transfer: reconcile requires an authorization function")
	}

	rm.mu.RLock()
	ids := make([]string, 0, len(rm.uploads))
	for id := range rm.uploads {
		ids = append(ids, id)
	}
	rm.mu.RUnlock()

	quarantined := 0
	for _, id := range ids {
		upload, ok := rm.GetUpload(id)
		if !ok {
			continue
		}
		meta := upload.Meta()
		if err := authorize(meta); err != nil {
			if qerr := rm.quarantine(id, err); qerr != nil {
				return quarantined, qerr
			}
			quarantined++
		}
	}
	return quarantined, nil
}

// quarantine isolates one upload whose durable identity cannot be trusted.
func (rm *ResumableManager) quarantine(transferID string, reason error) error {
	upload, ok := rm.GetUpload(transferID)
	if !ok {
		return nil
	}
	if !upload.TryLock() {
		return fmt.Errorf("transfer: upload %s is busy and cannot be quarantined", transferID)
	}
	defer upload.Unlock()

	upload.UpdateMeta(func(m *ResumableMeta) {
		m.State = ResumableFailed
		m.QuarantineReason = reason.Error()
		m.UpdatedAt = time.Now()
	})
	meta := upload.Meta()

	// Preserve the record of why even if the live file cannot be rewritten.
	_ = rm.writeDurableMeta(&meta)

	stamp := fmt.Sprintf(".quarantine-%d", time.Now().UTC().UnixNano())
	if err := os.Rename(rm.metaPath(transferID), rm.metaPath(transferID)+stamp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("transfer: quarantine metadata for %s: %w", transferID, err)
	}
	if err := os.Rename(rm.partPath(transferID), rm.partPath(transferID)+stamp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("transfer: quarantine payload for %s: %w", transferID, err)
	}

	rm.mu.Lock()
	delete(rm.uploads, transferID)
	rm.mu.Unlock()
	if rm.quota != nil {
		rm.quota.Release(transferID)
	}
	return nil
}

// CleanupExpired cleans up uploads that have exceeded the 24-hour TTL.
func (rm *ResumableManager) CleanupExpired() {
	rm.mu.RLock()
	var toCheck []string
	for id := range rm.uploads {
		toCheck = append(toCheck, id)
	}
	rm.mu.RUnlock()

	now := time.Now()
	for _, id := range toCheck {
		upload, ok := rm.GetUpload(id)
		if !ok {
			continue
		}
		meta := upload.Meta()
		if !meta.ExpiresAt.IsZero() && now.After(meta.ExpiresAt) {
			if upload.lifecycleMu.TryLock() {
				_ = os.Remove(rm.partPath(id))
				_ = os.Remove(rm.metaPath(id))
				_ = os.Remove(rm.metaTmpPath(id))
				_ = os.Remove(rm.publishedPath(id))
				if rm.quota != nil {
					rm.quota.Release(id)
				}
				rm.mu.Lock()
				delete(rm.uploads, id)
				rm.mu.Unlock()
				upload.lifecycleMu.Unlock()
			}
		}
	}
}

func (rm *ResumableManager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rm.stopCleaner:
			return
		case <-ticker.C:
			rm.CleanupExpired()
		}
	}
}

// computeFileSHA256 streams a file from disk to compute its lowercase hex SHA-256.
func computeFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	bufPtr := GetCopyBuffer()
	defer PutCopyBuffer(bufPtr)
	if _, err := io.CopyBuffer(h, f, *bufPtr); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
