package transfer

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/secfile"
)

const (
	// DefaultChunkSize is the server-advertised chunk size (16 MiB) for resumable
	// chunked PATCH uploads.
	DefaultChunkSize int64 = 16 << 20

	// MaxChunkSize bounds a single PATCH request body so server memory and crash
	// rollback remain strictly bounded.
	MaxChunkSize int64 = 64 << 20

	// DefaultStagingTTL bounds how long abandoned resumable partial files stay
	// before startup Phase 10 cleanup reclaims them.
	DefaultStagingTTL = 24 * time.Hour

	// MetaSuffix is appended to a part file path to name its HMAC-protected sidecar.
	MetaSuffix = ".meta"

	// MetaTmpSuffix is the atomic replacement staging suffix before rename.
	MetaTmpSuffix = ".meta.tmp"

	resumableMetaVersion = 1
	resumableMetaDomain  = "flitnix-resumable-meta-v1\x00"
	maxMetaFileBytes     = 64 << 10
)

var (
	// ErrOffsetMismatch means the PATCH Upload-Offset did not match the server's
	// durably committed byte offset.
	ErrOffsetMismatch = errors.New("transfer: upload offset mismatch")

	// ErrChunkInterrupted means a resumable PATCH chunk ended or failed before all
	// chunk bytes arrived; previously committed bytes remain safe on disk.
	ErrChunkInterrupted = errors.New("transfer: upload chunk interrupted")

	// ErrUploadPaused means the upload is paused and refuses chunks until resumed.
	ErrUploadPaused = errors.New("transfer: upload is paused")

	// ErrUploadDiscarded means the user explicitly discarded partial upload data.
	ErrUploadDiscarded = errors.New("transfer: upload partial data discarded")

	// ErrStagingQuarantined means resumable staging failed integrity verification.
	ErrStagingQuarantined = errors.New("transfer: staged upload quarantined")

	// ErrInvalidChunk means the chunk headers or length were malformed.
	ErrInvalidChunk = errors.New("transfer: invalid chunk parameters")
)

// OffsetMismatchError carries both the client's supplied offset and the server's
// authoritative committed offset so callers can align cleanly.
type OffsetMismatchError struct {
	Expected int64
	Got      int64
}

func (e *OffsetMismatchError) Error() string {
	return fmt.Sprintf("%s: expected offset %d, got %d", ErrOffsetMismatch, e.Expected, e.Got)
}

func (e *OffsetMismatchError) Unwrap() error { return ErrOffsetMismatch }

// ChunkResult describes the outcome of one resumable PATCH chunk.
type ChunkResult struct {
	Written         int64  // total file bytes committed so far (== CommittedOffset)
	ChunkWritten    int64  // bytes added in this PATCH request
	CommittedOffset int64  // authoritative durable offset after fsync + meta rename
	Completed       bool   // true when CommittedOffset == DeclaredSize and file published
	SHA256          string // full-file SHA-256 hex digest once Completed is true
	Path            string // final published path once Completed is true
}

// HeadResult describes authoritative resumable upload offset metadata for HEAD.
type HeadResult struct {
	UploadID         string
	DeclaredSize     int64
	CommittedOffset  int64
	ChunkSize        int64
	State            State
	Quarantined      bool
	QuarantineReason string
}

// ResumableMeta is the authenticated durable staging checkpoint stored alongside
// each .part file on disk.
type ResumableMeta struct {
	Version         int       `json:"version"`
	UploadID        string    `json:"uploadId"`
	BatchID         string    `json:"batchId,omitempty"`
	ItemID          string    `json:"itemId,omitempty"`
	SessionKey      string    `json:"sessionKey"`
	RootName        string    `json:"rootName,omitempty"`
	RelativePath    string    `json:"relativePath,omitempty"`
	FinalName       string    `json:"finalName"`
	PartRelPath     string    `json:"partRelPath"`
	DeclaredSize    int64     `json:"declaredSize"`
	CommittedOffset int64     `json:"committedOffset"`
	ChunkSize       int64     `json:"chunkSize"`
	ExpectedSHA256  string    `json:"expectedSha256,omitempty"`
	SHA256State     []byte    `json:"sha256State,omitempty"`
	LastModifiedMs  int64     `json:"lastModifiedMs,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type signedMetaEnvelope struct {
	Payload json.RawMessage `json:"payload"`
	MAC     string          `json:"mac"`
}

// StagedArtifact represents one discovered staging metadata sidecar during startup scan.
type StagedArtifact struct {
	MetaPath string
	PartPath string
	Meta     ResumableMeta
	Err      error // non-nil if corrupt, malformed, or unauthenticated
}

func computeMetaMAC(key []byte, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(resumableMetaDomain))
	_, _ = m.Write(payload)
	return m.Sum(nil)
}

// WriteResumableMeta writes meta atomically to metaPath using .meta.tmp -> fsync ->
// rename -> dir fsync, authenticated with key.
func WriteResumableMeta(metaPath string, key []byte, meta ResumableMeta) error {
	if metaPath == "" || len(key) == 0 {
		return errors.New("transfer: invalid metadata path or key")
	}
	meta.Version = resumableMetaVersion
	if meta.ChunkSize <= 0 {
		meta.ChunkSize = DefaultChunkSize
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = time.Now().UTC()
	}
	payloadBytes, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("transfer: marshal resumable metadata: %w", err)
	}
	env := signedMetaEnvelope{
		Payload: payloadBytes,
		MAC:     hex.EncodeToString(computeMetaMAC(key, payloadBytes)),
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("transfer: encode signed metadata: %w", err)
	}

	tmpPath := metaPath + ".tmp"
	if strings.HasSuffix(metaPath, MetaSuffix) {
		tmpPath = strings.TrimSuffix(metaPath, MetaSuffix) + MetaTmpSuffix
	}
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm)
	if err != nil {
		if IsNoSpaceError(err) {
			return fmt.Errorf("%w: %v", ErrInsufficientStorage, err)
		}
		return fmt.Errorf("transfer: write metadata temp file: %w", err)
	}
	if err := secfile.ProtectFile(tmpPath); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("transfer: protect metadata file: %w", err)
	}
	if _, err := f.Write(encoded); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		if IsNoSpaceError(err) {
			return fmt.Errorf("%w: %v", ErrInsufficientStorage, err)
		}
		return fmt.Errorf("transfer: write metadata: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		if IsNoSpaceError(err) {
			return fmt.Errorf("%w: %v", ErrInsufficientStorage, err)
		}
		return fmt.Errorf("transfer: sync metadata: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("transfer: close metadata: %w", err)
	}
	if err := renameFileWithRetry(tmpPath, metaPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("transfer: commit metadata: %w", err)
	}
	if err := syncDir(filepath.Dir(metaPath)); err != nil {
		return fmt.Errorf("transfer: sync metadata dir: %w", err)
	}
	return nil
}

// ReadResumableMeta reads and verifies the HMAC authenticity and schema of metaPath.
func ReadResumableMeta(metaPath string, key []byte) (ResumableMeta, error) {
	if metaPath == "" || len(key) == 0 {
		return ResumableMeta{}, fmt.Errorf("%w: missing path or key", ErrStagingQuarantined)
	}
	info, err := os.Lstat(metaPath)
	if err != nil {
		return ResumableMeta{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ResumableMeta{}, fmt.Errorf("%w: metadata path is not a regular file", ErrStagingQuarantined)
	}
	if info.Size() <= 0 || info.Size() > maxMetaFileBytes {
		return ResumableMeta{}, fmt.Errorf("%w: invalid metadata file size %d", ErrStagingQuarantined, info.Size())
	}
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return ResumableMeta{}, fmt.Errorf("%w: read metadata: %v", ErrStagingQuarantined, err)
	}
	var env signedMetaEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return ResumableMeta{}, fmt.Errorf("%w: parse metadata envelope: %v", ErrStagingQuarantined, err)
	}
	if len(env.Payload) == 0 || env.MAC == "" {
		return ResumableMeta{}, fmt.Errorf("%w: incomplete metadata envelope", ErrStagingQuarantined)
	}
	expected := hex.EncodeToString(computeMetaMAC(key, env.Payload))
	if !hmac.Equal([]byte(expected), []byte(env.MAC)) {
		return ResumableMeta{}, fmt.Errorf("%w: metadata HMAC verification failed", ErrStagingQuarantined)
	}
	var meta ResumableMeta
	if err := json.Unmarshal(env.Payload, &meta); err != nil {
		return ResumableMeta{}, fmt.Errorf("%w: decode metadata payload: %v", ErrStagingQuarantined, err)
	}
	if meta.Version != resumableMetaVersion || meta.UploadID == "" || meta.SessionKey == "" ||
		meta.DeclaredSize < 0 || meta.CommittedOffset < 0 || meta.CommittedOffset > meta.DeclaredSize ||
		meta.PartRelPath == "" || meta.FinalName == "" {
		return ResumableMeta{}, fmt.Errorf("%w: invalid metadata fields", ErrStagingQuarantined)
	}
	return meta, nil
}

// ReopenPartForAppend validates partPath inside store, truncates any torn uncommitted
// tail beyond committedOffset, fsyncs, and positions the write cursor at committedOffset.
func (s *Store) ReopenPartForAppend(partPath string, committedOffset int64) (*os.File, bool, error) {
	if !s.withinDir(partPath) {
		return nil, false, fmt.Errorf("%w: staging file outside receive directory", ErrStagingQuarantined)
	}
	info, err := os.Lstat(partPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, fmt.Errorf("%w: staging part file missing", ErrStagingQuarantined)
		}
		return nil, false, fmt.Errorf("%w: stat staging part: %v", ErrStagingQuarantined, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: staging part is a symlink or irregular file", ErrStagingQuarantined)
	}
	if info.Size() < committedOffset {
		return nil, false, fmt.Errorf("%w: staging file truncated (%d < committed %d)", ErrStagingQuarantined, info.Size(), committedOffset)
	}
	f, err := os.OpenFile(partPath, os.O_RDWR, filePerm)
	if err != nil {
		return nil, false, fmt.Errorf("%w: open staging part: %v", ErrStagingQuarantined, err)
	}
	truncatedTail := false
	if info.Size() > committedOffset {
		if err := f.Truncate(committedOffset); err != nil {
			_ = f.Close()
			return nil, false, fmt.Errorf("transfer: truncate torn chunk tail: %w", err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, false, fmt.Errorf("transfer: sync truncated part file: %w", err)
		}
		truncatedTail = true
	}
	if _, err := f.Seek(committedOffset, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("transfer: seek staging part to offset %d: %w", committedOffset, err)
	}
	return f, truncatedTail, nil
}

// CommitResumablePart atomically hard-links partPath to a collision-safe final path
// in finalDir, then cleans up partPath, metaPath, and meta temp files.
func (s *Store) CommitResumablePart(partPath, metaPath, finalDir, finalName string) (string, error) {
	return s.CommitResumablePartWithModTime(partPath, metaPath, finalDir, finalName, 0)
}

// CommitResumablePartWithModTime atomically publishes partPath while preserving an
// optional Unix-millisecond modification timestamp when enabled on the Store.
func (s *Store) CommitResumablePartWithModTime(partPath, metaPath, finalDir, finalName string, modTimeMs int64) (string, error) {
	if !s.withinDir(partPath) || !s.withinDir(finalDir) {
		return "", fmt.Errorf("transfer: refusing to commit outside receive directory")
	}
	if modTimeMs > 0 && s.PreserveModTimes() {
		modTime := time.UnixMilli(modTimeMs)
		_ = os.Chtimes(partPath, modTime, modTime)
	}
	for n := 0; n < commitVariants; n++ {
		final := filepath.Join(finalDir, variantName(finalName, n))
		if !s.withinDir(final) {
			return "", fmt.Errorf("transfer: refusing to write outside receive directory: %s", final)
		}
		err := s.linkFile(partPath, final)
		if err == nil {
			_ = os.Remove(partPath)
			removeMetaFiles(metaPath)
			_ = syncDir(finalDir)
			return final, nil
		}
		switch {
		case os.IsExist(err):
			continue
		case isLinkUnsupported(err):
			return "", fmt.Errorf("transfer: atomic commit failed: the filesystem rejected hard links; the completed data is kept at %s", partPath)
		default:
			if IsNoSpaceError(err) {
				return "", fmt.Errorf("%w: %v", ErrInsufficientStorage, err)
			}
			return "", fmt.Errorf("transfer: committing resumable file: %w", err)
		}
	}
	return "", fmt.Errorf("transfer: could not find a free filename for %q after %d variants", finalName, commitVariants)
}

func removeMetaFiles(metaPath string) {
	if metaPath == "" {
		return
	}
	_ = os.Remove(metaPath)
	if strings.HasSuffix(metaPath, MetaSuffix) {
		_ = os.Remove(strings.TrimSuffix(metaPath, MetaSuffix) + MetaTmpSuffix)
	}
	_ = os.Remove(metaPath + ".tmp")
}

func removeResumablePaths(partPath, metaPath string) {
	if partPath != "" {
		_ = os.Remove(partPath)
	}
	removeMetaFiles(metaPath)
}

// ScanResumableMeta walks storeDir looking for staging metadata sidecars (.meta),
// removing any leftover uncommitted .meta.tmp files and reporting valid or corrupt
// metadata files plus orphan .part files without metadata.
func (s *Store) ScanResumableMeta() (artifacts []StagedArtifact, orphanParts []string, err error) {
	dir := s.Dir()
	info, statErr := os.Lstat(dir)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil, nil, nil
	}
	if statErr != nil {
		return nil, nil, statErr
	}
	if !info.IsDir() {
		return nil, nil, nil
	}

	key := s.MetaKey()
	metaParts := make(map[string]struct{})
	var allParts []string

	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, MetaTmpSuffix) && strings.Contains(name, partSuffixMarker) {
			_ = os.Remove(path)
			return nil
		}
		if strings.HasSuffix(name, MetaSuffix) && strings.Contains(name, partSuffixMarker) {
			partPath := strings.TrimSuffix(path, MetaSuffix)
			metaParts[partPath] = struct{}{}
			meta, rerr := ReadResumableMeta(path, key)
			if rerr == nil && meta.PartRelPath != "" {
				expectedPart := filepath.Join(dir, filepath.FromSlash(meta.PartRelPath))
				if expectedPart != partPath {
					rerr = fmt.Errorf("%w: metadata part path mismatch", ErrStagingQuarantined)
				}
			}
			artifacts = append(artifacts, StagedArtifact{
				MetaPath: path,
				PartPath: partPath,
				Meta:     meta,
				Err:      rerr,
			})
			return nil
		}
		if strings.Contains(name, partSuffixMarker) && !strings.HasSuffix(name, MetaSuffix) && !strings.HasSuffix(name, MetaTmpSuffix) {
			allParts = append(allParts, path)
		}
		return nil
	})
	for _, p := range allParts {
		if _, hasMeta := metaParts[p]; !hasMeta {
			orphanParts = append(orphanParts, p)
		}
	}
	return artifacts, orphanParts, walkErr
}

// QuarantineFiles moves corrupted or inconsistent staging files out of the receive
// directory into quarantineDir (preserving forensic evidence privately), or deletes
// them when no quarantineDir is configured.
func QuarantineFiles(quarantineDir, partPath, metaPath, reason string) error {
	if quarantineDir == "" {
		removeResumablePaths(partPath, metaPath)
		return nil
	}
	if err := os.MkdirAll(quarantineDir, dirPerm); err != nil {
		removeResumablePaths(partPath, metaPath)
		return err
	}
	_ = secfile.ProtectDir(quarantineDir)
	stamp := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	for _, p := range []string{partPath, metaPath} {
		if p == "" {
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		dst := filepath.Join(quarantineDir, stamp+"-"+filepath.Base(p))
		if err := renameFileWithRetry(p, dst); err != nil {
			_ = os.Remove(p)
		} else {
			_ = secfile.ProtectFile(dst)
		}
	}
	if reason != "" {
		notePath := filepath.Join(quarantineDir, stamp+".reason.txt")
		_ = os.WriteFile(notePath, []byte(reason+"\n"), filePerm)
		_ = secfile.ProtectFile(notePath)
	}
	if strings.HasSuffix(metaPath, MetaSuffix) {
		_ = os.Remove(strings.TrimSuffix(metaPath, MetaSuffix) + MetaTmpSuffix)
	}
	return nil
}

// metaPathForPart returns the metadata sidecar path for partPath.
func metaPathForPart(partPath string) string {
	return partPath + MetaSuffix
}

// newHashAtState creates a SHA-256 hasher, optionally restoring marshaled state.
func newHashAtState(saved []byte) (hash.Hash, error) {
	h := sha256.New()
	if len(saved) == 0 {
		return h, nil
	}
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, errors.New("transfer: sha256 implementation does not support state restore")
	}
	if err := u.UnmarshalBinary(saved); err != nil {
		return nil, fmt.Errorf("%w: invalid saved sha256 state: %v", ErrStagingQuarantined, err)
	}
	return h, nil
}

func marshalHashState(h hash.Hash) ([]byte, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, errors.New("transfer: sha256 implementation does not support state snapshot")
	}
	return m.MarshalBinary()
}

// ---- Service resumable methods ----

// SetBatchContext attaches the owning Coordinator batch and item IDs to an
// approved/pending upload record so resumable metadata sidecars bind tightly.
func (s *Service) SetBatchContext(uploadID, batchID, itemID string) {
	u, err := s.lookup(uploadID, "")
	if err != nil {
		return
	}
	u.mu.Lock()
	u.batchID = batchID
	u.itemID = itemID
	u.mu.Unlock()
}

// RestoreUploadOpts configures restoring a durable upload record during recovery.
type RestoreUploadOpts struct {
	ID              string
	SessionKey      string
	BatchID         string
	ItemID          string
	Name            string
	Root            string
	RelativePath    string
	Size            int64
	LastModifiedMs  int64
	ExpectedSHA256  string
	State           State
	Reason          string
	PartPath        string
	MetaPath        string
	FinalDir        string
	CommittedOffset int64
	ChunkSize       int64
	SHA256State     []byte
	CreatedAt       time.Time
	BatchLinked     bool
	Quarantined     bool
}

// RestoreApprovedUpload reconstructs an in-memory upload record from durable
// recovery journal / resumable metadata during startup recovery.
func (s *Service) RestoreApprovedUpload(opts RestoreUploadOpts) error {
	if opts.ID == "" || opts.SessionKey == "" {
		return ErrInvalidUpload
	}
	safeName, err := SanitizeName(opts.Name)
	if err != nil {
		safeName = opts.Name
	}
	s.mu.Lock()
	if _, exists := s.uploads[opts.ID]; exists {
		s.mu.Unlock()
		return nil
	}
	s.nextSeq++
	chunkSz := opts.ChunkSize
	if chunkSz <= 0 {
		chunkSz = DefaultChunkSize
	}
	created := opts.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	st := opts.State
	if st == "" {
		st = StateApproved
	}
	u := &upload{
		id:              opts.ID,
		name:            safeName,
		root:            opts.Root,
		relativePath:    opts.RelativePath,
		session:         opts.SessionKey,
		size:            opts.Size,
		lastModifiedMs:  opts.LastModifiedMs,
		created:         created,
		managed:         opts.BatchLinked,
		seq:             s.nextSeq,
		expectedSHA256:  opts.ExpectedSHA256,
		state:           st,
		reason:          opts.Reason,
		batchID:         opts.BatchID,
		itemID:          opts.ItemID,
		partPath:        opts.PartPath,
		metaPath:        opts.MetaPath,
		finalDir:        opts.FinalDir,
		committedOffset: opts.CommittedOffset,
		chunkSize:       chunkSz,
		hashState:       append([]byte(nil), opts.SHA256State...),
		quarantined:     opts.Quarantined,
		quarantineMsg:   opts.Reason,
	}
	u.uploaded.Store(opts.CommittedOffset)
	s.uploads[opts.ID] = u
	s.mu.Unlock()
	return nil
}

// QuarantineUpload marks an upload failed/quarantined so it cannot be resumed or published.
func (s *Service) QuarantineUpload(id, reason string) {
	u, err := s.lookup(id, "")
	if err != nil {
		return
	}
	u.mu.Lock()
	u.quarantined = true
	u.quarantineMsg = reason
	u.pub = pubCancelReserved
	u.state = StateFailed
	u.reason = "quarantined: " + reason
	u.committedOffset = 0
	u.uploaded.Store(0)
	partPath, metaPath := u.partPath, u.metaPath
	u.partPath, u.metaPath, u.hashState = "", "", nil
	if s.quota != nil {
		s.quota.Release(u.id)
	}
	u.mu.Unlock()
	removeResumablePaths(partPath, metaPath)
}

// HeadUpload returns authoritative resumable offset and status for HEAD requests.
func (s *Service) HeadUpload(id, sessionKey string) (HeadResult, error) {
	if sessionKey == "" {
		return HeadResult{}, ErrWrongSession
	}
	u, err := s.lookup(id, sessionKey)
	if err != nil {
		return HeadResult{}, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	chunkSz := u.chunkSize
	if chunkSz <= 0 {
		chunkSz = DefaultChunkSize
	}
	res := HeadResult{
		UploadID:         u.id,
		DeclaredSize:     u.size,
		CommittedOffset:  u.committedOffset,
		ChunkSize:        chunkSz,
		State:            u.state,
		Quarantined:      u.quarantined,
		QuarantineReason: u.quarantineMsg,
	}
	if u.state == StateCompleted {
		res.CommittedOffset = u.size
	}
	return res, nil
}

// Pause suspends a resumable upload without deleting durable partial data.
func (s *Service) Pause(id string) error {
	u, err := s.lookup(id, "")
	if err != nil {
		return err
	}
	u.mu.Lock()
	switch u.state {
	case StateApproved, StateInterrupted, StateUploading:
		cancel := u.chunkCancel
		u.state = StatePaused
		u.reason = "paused"
		u.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	case StatePaused:
		u.mu.Unlock()
		return nil
	default:
		st := u.state
		u.mu.Unlock()
		return fmt.Errorf("%w: cannot pause in state %q", ErrAlreadyDecided, st)
	}
}

// Resume moves a paused upload back into a resumable state.
func (s *Service) Resume(id string) error {
	u, err := s.lookup(id, "")
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	switch u.state {
	case StatePaused:
		if u.committedOffset > 0 {
			u.state = StateInterrupted
		} else {
			u.state = StateApproved
		}
		u.reason = ""
		return nil
	case StateApproved, StateInterrupted:
		return nil
	default:
		return fmt.Errorf("%w: cannot resume in state %q", ErrAlreadyDecided, u.state)
	}
}

// Discard explicitly discards a resumable or pending/paused/interrupted upload,
// deleting its .part and .meta files and releasing reserved disk quota.
func (s *Service) Discard(id string) error {
	u, err := s.lookup(id, "")
	if err != nil {
		return err
	}
	u.mu.Lock()
	if u.pub == pubPublishing || u.state == StateCompleted {
		u.mu.Unlock()
		return ErrTooLateToCancel
	}
	if u.state == StateDiscarded {
		u.mu.Unlock()
		return nil
	}
	cancel := u.chunkCancel
	partPath, metaPath := u.partPath, u.metaPath
	u.pub = pubCancelReserved
	u.state = StateDiscarded
	u.reason = "discarded by user"
	u.committedOffset = 0
	u.uploaded.Store(0)
	u.partPath, u.metaPath, u.hashState = "", "", nil
	if s.quota != nil {
		s.quota.Release(u.id)
	}
	u.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	removeResumablePaths(partPath, metaPath)
	return nil
}

// SubmitChunk streams a bounded byte chunk starting at offset into the staged
// .part file, fsyncs both the data and signed .meta checkpoint, and publishes
// the completed file when the final chunk brings CommittedOffset to DeclaredSize.
func (s *Service) SubmitChunk(
	ctx context.Context,
	id, sessionKey string,
	offset, chunkLen int64,
	body io.Reader,
	onProgress func(Upload),
	onPublish func() error,
) (ChunkResult, error) {
	if sessionKey == "" || body == nil {
		return ChunkResult{}, ErrWrongSession
	}
	u, err := s.lookup(id, sessionKey)
	if err != nil {
		return ChunkResult{}, err
	}

	u.mu.Lock()
	if u.quarantined {
		reason := u.quarantineMsg
		u.mu.Unlock()
		return ChunkResult{}, fmt.Errorf("%w: %s", ErrStagingQuarantined, reason)
	}
	switch u.state {
	case StatePending:
		u.mu.Unlock()
		return ChunkResult{}, ErrNotApproved
	case StateCanceled:
		u.mu.Unlock()
		return ChunkResult{}, ErrCanceled
	case StatePaused:
		u.mu.Unlock()
		return ChunkResult{}, ErrUploadPaused
	case StateDiscarded:
		u.mu.Unlock()
		return ChunkResult{}, ErrUploadDiscarded
	case StateRejected, StateFailed, StateCompleted:
		u.mu.Unlock()
		return ChunkResult{}, fmt.Errorf("%w: state is %q", ErrAlreadyDecided, u.state)
	}
	if u.chunkInFlight {
		u.mu.Unlock()
		return ChunkResult{}, ErrUploadUnderway
	}

	curOffset := u.committedOffset
	if offset != curOffset {
		u.mu.Unlock()
		return ChunkResult{}, &OffsetMismatchError{Expected: curOffset, Got: offset}
	}
	if chunkLen < 0 || chunkLen > MaxChunkSize || curOffset+chunkLen > u.size {
		u.mu.Unlock()
		return ChunkResult{}, fmt.Errorf("%w: offset=%d len=%d declared=%d", ErrInvalidChunk, offset, chunkLen, u.size)
	}
	if u.size > 0 && chunkLen == 0 {
		u.mu.Unlock()
		return ChunkResult{}, fmt.Errorf("%w: zero-length chunk for non-empty upload", ErrInvalidChunk)
	}

	// Check initial storage headroom before touching disk.
	targetDir := s.targetDir()
	if s.quota != nil && chunkLen > 0 {
		if err := s.quota.CheckWrite(id, targetDir, curOffset, chunkLen); err != nil {
			u.state = StateInterrupted
			u.reason = err.Error()
			u.mu.Unlock()
			return ChunkResult{}, err
		}
	}

	// Open new staging file or reopen existing one cleanly.
	var partFile *os.File
	var hasher hash.Hash
	var partPath, metaPath, finalDir, name, root, relPath string
	var createdAt time.Time
	var expectedSHA, batchID, itemID string
	var lastModMs int64

	name = u.name
	root = u.root
	relPath = u.relativePath
	createdAt = u.created
	expectedSHA = u.expectedSHA256
	batchID = u.batchID
	itemID = u.itemID
	lastModMs = u.lastModifiedMs

	metaKey := s.store.MetaKey()
	if u.partPath == "" {
		hasher = sha256.New()
		hstate, herr := marshalHashState(hasher)
		if herr != nil {
			u.mu.Unlock()
			return ChunkResult{}, herr
		}
		var pf *PartFile
		var perr error
		if u.relativePath != "" {
			pf, perr = s.store.OpenPartPath(u.root, u.relativePath)
		} else {
			pf, perr = s.store.OpenPart(u.name)
		}
		if perr != nil {
			u.state = StateInterrupted
			u.reason = perr.Error()
			u.mu.Unlock()
			return ChunkResult{}, perr
		}
		partPath = pf.PartPath()
		finalDir = pf.FinalDir()
		metaPath = metaPathForPart(partPath)
		partFile = pf.f

		// Defer writing .meta until an intermediate chunk boundary (< u.size) is
		// committed so single-chunk uploads never pay extra .meta fsync + unlink I/O.
		u.partPath = partPath
		u.metaPath = metaPath
		u.finalDir = finalDir
		u.hashState = hstate
		u.chunkSize = DefaultChunkSize
	} else {
		partPath = u.partPath
		metaPath = u.metaPath
		finalDir = u.finalDir
		if finalDir == "" {
			finalDir = filepath.Dir(partPath)
			u.finalDir = finalDir
		}
		h, herr := newHashAtState(u.hashState)
		if herr != nil {
			u.quarantined = true
			u.quarantineMsg = herr.Error()
			u.state = StateFailed
			u.reason = herr.Error()
			u.mu.Unlock()
			return ChunkResult{}, herr
		}
		hasher = h
		rf, _, rerr := s.store.ReopenPartForAppend(partPath, curOffset)
		if rerr != nil {
			if errors.Is(rerr, ErrStagingQuarantined) {
				u.quarantined = true
				u.quarantineMsg = rerr.Error()
				u.state = StateFailed
			} else {
				u.state = StateInterrupted
			}
			u.reason = rerr.Error()
			u.mu.Unlock()
			return ChunkResult{}, rerr
		}
		partFile = rf
	}

	chunkCtx, chunkCancel := context.WithCancel(ctx)
	u.chunkInFlight = true
	u.chunkCancel = chunkCancel
	u.state = StateUploading
	u.reason = ""
	chunkStart := time.Now()
	u.lastAt = chunkStart
	u.lastBytes = curOffset
	u.mu.Unlock()

	var bw *bufio.Writer
	rollbackInterrupted := func(cause error) (ChunkResult, error) {
		if bw != nil {
			_ = bw.Flush()
		}
		_ = partFile.Truncate(curOffset)
		_ = partFile.Sync()
		_ = partFile.Close()

		u.mu.Lock()
		u.chunkInFlight = false
		u.chunkCancel = nil
		u.uploaded.Store(curOffset)
		if s.quota != nil {
			s.quota.Advance(id, curOffset)
		}
		wasCanceled := u.pub == pubCancelReserved || u.state == StateCanceled
		wasDiscarded := u.state == StateDiscarded
		wasPaused := u.state == StatePaused
		if wasCanceled {
			u.partPath, u.metaPath, u.committedOffset, u.hashState = "", "", 0, nil
			if s.quota != nil {
				s.quota.Release(id)
			}
			u.mu.Unlock()
			removeResumablePaths(partPath, metaPath)
			return ChunkResult{}, fmt.Errorf("%w: %v", ErrCanceled, cause)
		}
		if wasDiscarded {
			u.partPath, u.metaPath, u.committedOffset, u.hashState = "", "", 0, nil
			if s.quota != nil {
				s.quota.Release(id)
			}
			u.mu.Unlock()
			removeResumablePaths(partPath, metaPath)
			return ChunkResult{}, ErrUploadDiscarded
		}
		if wasPaused {
			// Paused, not canceled: keep durable partial data (.part/.meta),
			// committed offset, hash state, and quota reservation for resume.
			// partFile was truncated to curOffset above; no partials removed.
			u.mu.Unlock()
			return ChunkResult{CommittedOffset: curOffset}, ErrUploadPaused
		}
		u.state = StateInterrupted
		u.reason = cause.Error()
		u.mu.Unlock()
		return ChunkResult{CommittedOffset: curOffset}, cause
	}

	defer chunkCancel()

	// Stream chunkLen bytes (+ 1 probe byte to detect overrun beyond Content-Length).
	bp := GetCopyBuffer()
	defer PutCopyBuffer(bp)
	buf := *bp

	var chunkWritten int64
	var sinceStep int64
	var sinceDiskCheck int64
	bw = GetBufioWriter(partFile)
	defer PutBufioWriter(bw)
	mw := io.MultiWriter(bw, hasher)

	limited := io.LimitReader(body, chunkLen+1)
	for {
		if chunkCtx.Err() != nil {
			return rollbackInterrupted(ErrChunkInterrupted)
		}
		n, rerr := limited.Read(buf)
		if n > 0 {
			if chunkWritten+int64(n) > chunkLen {
				return rollbackInterrupted(ErrSizeExceeded)
			}
			sinceDiskCheck += int64(n)
			if s.quota != nil && sinceDiskCheck >= diskCheckInterval {
				sinceDiskCheck = 0
				if qerr := s.quota.CheckWrite(id, targetDir, curOffset+chunkWritten, int64(n)); qerr != nil {
					return rollbackInterrupted(qerr)
				}
			}
			wn, werr := mw.Write(buf[:n])
			chunkWritten += int64(wn)
			sinceStep += int64(wn)
			if werr != nil {
				if IsNoSpaceError(werr) {
					return rollbackInterrupted(fmt.Errorf("%w: %v", ErrInsufficientStorage, werr))
				}
				return rollbackInterrupted(werr)
			}
			if wn != n {
				return rollbackInterrupted(io.ErrShortWrite)
			}
			totalSoFar := curOffset + chunkWritten
			u.uploaded.Store(totalSoFar)
			if s.quota != nil {
				s.quota.Advance(id, totalSoFar)
			}
			if sinceStep >= progressStep {
				sinceStep = 0
				u.mu.Lock()
				now := time.Now()
				if elapsed := now.Sub(u.lastAt).Seconds(); elapsed >= 0.01 && totalSoFar > u.lastBytes {
					inst := float64(totalSoFar-u.lastBytes) / elapsed
					if u.speed <= 0 {
						u.speed = inst
					} else {
						u.speed = 0.7*u.speed + 0.3*inst
					}
					u.lastAt = now
					u.lastBytes = totalSoFar
				}
				u.mu.Unlock()
				if onProgress != nil {
					onProgress(u.snapshot())
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rollbackInterrupted(fmt.Errorf("%w: %v", ErrChunkInterrupted, rerr))
		}
	}

	if chunkWritten < chunkLen {
		return rollbackInterrupted(fmt.Errorf("%w: received %d of %d chunk bytes", ErrChunkInterrupted, chunkWritten, chunkLen))
	}

	// Check before fsync if user canceled/discarded/paused right as chunk finished.
	u.mu.Lock()
	if u.pub == pubCancelReserved || u.state == StateCanceled || u.state == StateDiscarded || u.state == StatePaused {
		u.mu.Unlock()
		if stateOf(u) == StatePaused {
			return rollbackInterrupted(ErrUploadPaused)
		}
		return rollbackInterrupted(ErrCanceled)
	}
	u.mu.Unlock()

	// Crash-safe commit ordering:
	// 1. Flush coalesced buffer and fsync .part data.
	if err := bw.Flush(); err != nil {
		if IsNoSpaceError(err) {
			return rollbackInterrupted(fmt.Errorf("%w: %v", ErrInsufficientStorage, err))
		}
		return rollbackInterrupted(err)
	}
	if err := partFile.Sync(); err != nil {
		if IsNoSpaceError(err) {
			return rollbackInterrupted(fmt.Errorf("%w: %v", ErrInsufficientStorage, err))
		}
		return rollbackInterrupted(err)
	}
	if err := partFile.Close(); err != nil {
		return rollbackInterrupted(err)
	}

	newOffset := curOffset + chunkWritten
	if newOffset < u.size {
		newHashState, herr := marshalHashState(hasher)
		if herr != nil {
			return ChunkResult{}, herr
		}
		partRel, relErr := filepath.Rel(s.store.Dir(), partPath)
		if relErr != nil {
			return ChunkResult{}, relErr
		}

		// 2. Write + fsync .meta.tmp -> rename to .meta -> syncDir for intermediate checkpoints.
		updatedMeta := ResumableMeta{
			UploadID:        id,
			BatchID:         batchID,
			ItemID:          itemID,
			SessionKey:      sessionKey,
			RootName:        root,
			RelativePath:    relPath,
			FinalName:       name,
			PartRelPath:     filepath.ToSlash(partRel),
			DeclaredSize:    u.size,
			LastModifiedMs:  lastModMs,
			CommittedOffset: newOffset,
			ChunkSize:       DefaultChunkSize,
			ExpectedSHA256:  expectedSHA,
			SHA256State:     newHashState,
			CreatedAt:       createdAt,
			UpdatedAt:       time.Now().UTC(),
		}
		if err := WriteResumableMeta(metaPath, metaKey, updatedMeta); err != nil {
			// Notice .part has newOffset bytes, .meta still points to curOffset.
			// Truncate .part back to curOffset to match authoritative .meta.
			f2, _, oerr := s.store.ReopenPartForAppend(partPath, curOffset)
			if oerr == nil {
				_ = f2.Close()
			}
			u.mu.Lock()
			u.chunkInFlight = false
			u.chunkCancel = nil
			u.uploaded.Store(curOffset)
			u.state = StateInterrupted
			u.reason = err.Error()
			u.mu.Unlock()
			return ChunkResult{CommittedOffset: curOffset}, err
		}

		// Durable checkpoint succeeded. Update in-memory committed state.
		u.mu.Lock()
		u.committedOffset = newOffset
		u.hashState = newHashState
		u.uploaded.Store(newOffset)
		if s.quota != nil {
			s.quota.Advance(id, newOffset)
		}
		now := time.Now()
		if u.speed <= 0 && chunkWritten > 0 {
			if elapsed := now.Sub(chunkStart).Seconds(); elapsed > 0 {
				u.speed = float64(chunkWritten) / elapsed
			}
		}
		u.lastAt = now
		u.lastBytes = newOffset
		u.chunkInFlight = false
		u.chunkCancel = nil
		u.mu.Unlock()

		if onProgress != nil {
			onProgress(u.snapshot())
		}
		return ChunkResult{
			Written:         newOffset,
			ChunkWritten:    chunkWritten,
			CommittedOffset: newOffset,
			Completed:       false,
		}, nil
	}

	// Final chunk arrived: verify digest & atomically publish without writing a
	// short-lived .meta sidecar that CommitResumablePartWithModTime would delete immediately.
	sumHex := hex.EncodeToString(hasher.Sum(nil))
	if expectedSHA != "" && !strings.EqualFold(sumHex, expectedSHA) {
		u.mu.Lock()
		u.chunkInFlight = false
		u.chunkCancel = nil
		u.pub = pubCancelReserved
		u.state = StateFailed
		u.reason = fmt.Sprintf("digest mismatch: expected %s, got %s", expectedSHA, sumHex)
		u.partPath, u.metaPath, u.committedOffset, u.hashState = "", "", 0, nil
		if s.quota != nil {
			s.quota.Release(id)
		}
		u.mu.Unlock()
		removeResumablePaths(partPath, metaPath)
		return ChunkResult{}, ErrDigestMismatch
	}

	u.mu.Lock()
	u.committedOffset = newOffset
	u.uploaded.Store(newOffset)
	if s.quota != nil {
		s.quota.Advance(id, newOffset)
	}
	now := time.Now()
	if u.speed <= 0 && chunkWritten > 0 {
		if elapsed := now.Sub(chunkStart).Seconds(); elapsed > 0 {
			u.speed = float64(chunkWritten) / elapsed
		}
	}
	u.lastAt = now
	u.lastBytes = newOffset
	u.mu.Unlock()
	if onProgress != nil {
		onProgress(u.snapshot())
	}

	u.mu.Lock()
	u.chunkInFlight = false
	u.chunkCancel = nil
	switch u.pub {
	case pubCancelReserved:
		u.state = StateCanceled
		u.reason = "canceled"
		u.partPath, u.metaPath, u.committedOffset, u.hashState = "", "", 0, nil
		if s.quota != nil {
			s.quota.Release(id)
		}
		u.mu.Unlock()
		removeResumablePaths(partPath, metaPath)
		return ChunkResult{}, fmt.Errorf("%w: canceled before publication", ErrCanceled)
	case pubPublishing:
		u.mu.Unlock()
		return ChunkResult{}, fmt.Errorf("%w: publication already under way", ErrAlreadyDecided)
	}
	if onPublish != nil {
		if err := onPublish(); err != nil {
			u.pub = pubCancelReserved
			u.state = StateCanceled
			u.reason = "canceled"
			u.partPath, u.metaPath, u.committedOffset, u.hashState = "", "", 0, nil
			if s.quota != nil {
				s.quota.Release(id)
			}
			u.mu.Unlock()
			removeResumablePaths(partPath, metaPath)
			return ChunkResult{}, fmt.Errorf("%w: canceled before publication: %v", ErrCanceled, err)
		}
	}
	u.pub = pubPublishing
	u.mu.Unlock()

	finalPath, cerr := s.store.CommitResumablePartWithModTime(partPath, metaPath, finalDir, name, lastModMs)
	u.mu.Lock()
	if cerr != nil {
		u.reason = cerr.Error()
		if s.quota != nil {
			s.quota.Release(id)
		}
		if u.state == StateCanceled {
			u.mu.Unlock()
			return ChunkResult{}, fmt.Errorf("%w: %v", ErrCanceled, cerr)
		}
		u.state = StateFailed
		u.mu.Unlock()
		if errors.Is(cerr, ErrInsufficientStorage) {
			return ChunkResult{}, cerr
		}
		return ChunkResult{}, fmt.Errorf("%s: %w", name, cerr)
	}
	u.state = StateCompleted
	u.published = finalPath
	u.sha256 = sumHex
	u.partPath, u.metaPath, u.hashState = "", "", nil
	if s.quota != nil {
		s.quota.Release(id)
	}
	u.mu.Unlock()

	return ChunkResult{
		Written:         newOffset,
		ChunkWritten:    chunkWritten,
		CommittedOffset: newOffset,
		Completed:       true,
		SHA256:          sumHex,
		Path:            finalPath,
	}, nil
}
