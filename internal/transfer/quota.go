package transfer

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
)

var (
	// ErrInsufficientStorage indicates that the target filesystem lacks enough
	// available headroom (after accounting for active reservations and safety
	// margin) to safely accept or continue the transfer.
	ErrInsufficientStorage = errors.New("transfer: insufficient storage space")
)

const (
	// StorageStatusHealthy means reported headroom exceeds low-space thresholds.
	StorageStatusHealthy = "healthy"
	// StorageStatusLow means free headroom is under 1 GiB (or <10% when total <= 10 GiB).
	StorageStatusLow = "low"
	// StorageStatusCritical means available headroom is exhausted or below 128 MiB.
	StorageStatusCritical = "critical"
	// StorageStatusUnknown means the OS or filesystem cannot reliably report capacity.
	StorageStatusUnknown = "unknown"

	// diskCheckInterval bounds mid-stream statfs polling frequency so gigabit
	// throughput is unaffected while external disk exhaustion is caught promptly.
	diskCheckInterval = 4 * 1024 * 1024
)

// StorageStatus is the desktop UI and diagnostic projection of disk headroom
// for a receive directory.
type StorageStatus struct {
	TargetDir     string `json:"targetDir"`
	ProbeDir      string `json:"probeDir"`
	TargetExists  bool   `json:"targetExists"`
	Available     bool   `json:"available"`
	Status        string `json:"status"` // "healthy", "low", "critical", "unknown"
	TotalBytes    uint64 `json:"totalBytes"`
	FreeBytes     uint64 `json:"freeBytes"`
	UsedBytes     uint64 `json:"usedBytes"`
	ReservedBytes int64  `json:"reservedBytes"`
	HeadroomBytes int64  `json:"headroomBytes"`
	SafetyMargin  int64  `json:"safetyMargin"`
	Error         string `json:"error,omitempty"`
}

// QuotaBatchItem describes one item within an atomic ReserveBatch call.
type QuotaBatchItem struct {
	ID    string
	Bytes int64
}

// QuotaConfig configures a QuotaManager.
type QuotaConfig struct {
	DefaultDir   string
	SafetyMargin int64
	Probe        func(dir string) DiskSpaceInfo
}

type quotaEntry struct {
	id            string
	sessionKey    string
	dir           string
	fsKey         string
	declaredBytes int64
	committed     int64
}

func (e *quotaEntry) remaining() int64 {
	rem := e.declaredBytes - e.committed
	if rem < 0 {
		return 0
	}
	return rem
}

// QuotaManager performs atomic concurrency-safe storage reservation accounting
// across pending, approved, active, and recovered uploads.
//
// Accounting model:
//   - Each reservation starts with declaredBytes and committed = 0.
//   - Remaining expected requirement is (declaredBytes - committed).
//   - As bytes are durably/physically written to disk via Advance/CheckWrite,
//     committed increases monotonically, decreasing Remaining 1:1 as OS
//     FreeBytes shrinks—preventing double-counting.
//   - Reservations are grouped by target FilesystemID (or normalized targetDir
//     fallback) so transfers targeting different volumes are accounted
//     independently while same-volume transfers share one headroom budget.
//   - When OS disk metrics are unavailable (Available == false), reservations
//     are still tracked logically and Status reports StorageStatusUnknown without
//     falsely blocking transfers on unsupported filesystems.
type QuotaManager struct {
	mu           sync.Mutex
	defaultDir   string
	safetyMargin int64
	probe        func(dir string) DiskSpaceInfo
	entries      map[string]*quotaEntry
}

// NewQuotaManager creates a storage quota and reservation manager.
func NewQuotaManager(cfg QuotaConfig) *QuotaManager {
	margin := cfg.SafetyMargin
	if margin < 0 {
		margin = 0
	}
	probe := cfg.Probe
	if probe == nil {
		probe = InspectDiskSpace
	}
	return &QuotaManager{
		defaultDir:   cfg.DefaultDir,
		safetyMargin: margin,
		probe:        probe,
		entries:      make(map[string]*quotaEntry),
	}
}

// SetDefaultDir updates the default directory inspected by Status("").
func (q *QuotaManager) SetDefaultDir(dir string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.defaultDir = dir
}

// SetProbeHook replaces the OS disk-space probe (used in tests). Passing nil
// restores InspectDiskSpace.
func (q *QuotaManager) SetProbeHook(fn func(dir string) DiskSpaceInfo) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if fn == nil {
		q.probe = InspectDiskSpace
		return
	}
	q.probe = fn
}

func (q *QuotaManager) resolveDirLocked(dir string) string {
	if dir != "" {
		return dir
	}
	return q.defaultDir
}

func fsMatchKey(info DiskSpaceInfo, dir string) string {
	if info.FilesystemID != "" {
		return info.FilesystemID
	}
	if info.ProbeDir != "" {
		return "probe:" + filepath.Clean(info.ProbeDir)
	}
	if info.TargetDir != "" {
		return "target:" + filepath.Clean(info.TargetDir)
	}
	return "dir:" + filepath.Clean(dir)
}

func (q *QuotaManager) reservedForVolumeLocked(fsKey string, excludeIDs map[string]struct{}) int64 {
	var total int64
	for id, e := range q.entries {
		if _, skip := excludeIDs[id]; skip {
			continue
		}
		if e.fsKey == fsKey || e.fsKey == "" || fsKey == "" {
			total += e.remaining()
		}
	}
	return total
}

func (q *QuotaManager) statusForLocked(dir string, excludeIDs map[string]struct{}) (StorageStatus, string) {
	target := q.resolveDirLocked(dir)
	info := q.probe(target)
	fsKey := fsMatchKey(info, target)
	reserved := q.reservedForVolumeLocked(fsKey, excludeIDs)

	st := StorageStatus{
		TargetDir:     info.TargetDir,
		ProbeDir:      info.ProbeDir,
		TargetExists:  info.TargetExists,
		Available:     info.Available,
		TotalBytes:    info.TotalBytes,
		FreeBytes:     info.FreeBytes,
		UsedBytes:     info.UsedBytes,
		ReservedBytes: reserved,
		SafetyMargin:  q.safetyMargin,
		Error:         info.Error,
	}

	if !info.Available {
		st.Status = StorageStatusUnknown
		st.HeadroomBytes = 0
		return st, fsKey
	}

	freeSigned := clampUint64ToInt64(info.FreeBytes)
	headroom := freeSigned - reserved - q.safetyMargin
	if headroom < 0 {
		headroom = 0
	}
	st.HeadroomBytes = headroom

	switch {
	case freeSigned-reserved-q.safetyMargin <= 0:
		st.Status = StorageStatusCritical
	case headroom < 128<<20 && info.TotalBytes >= 512<<20:
		st.Status = StorageStatusCritical
	case headroom < 1<<30 && info.TotalBytes >= 4<<30:
		st.Status = StorageStatusLow
	case info.TotalBytes > 0 && float64(headroom) < float64(info.TotalBytes)*0.10:
		st.Status = StorageStatusLow
	default:
		st.Status = StorageStatusHealthy
	}

	return st, fsKey
}

func clampUint64ToInt64(v uint64) int64 {
	const maxI64 = uint64(1<<63 - 1)
	if v >= maxI64 {
		return int64(maxI64)
	}
	return int64(v)
}

// Status returns the current disk space and reservation summary for dir
// (or DefaultDir when dir is empty).
func (q *QuotaManager) Status(dir string) StorageStatus {
	q.mu.Lock()
	defer q.mu.Unlock()
	st, _ := q.statusForLocked(dir, nil)
	return st
}

// CheckAvailable verifies whether needBytes can fit in dir without recording a
// reservation. Useful as a fast pre-flight rejection before filesystem folder setup.
func (q *QuotaManager) CheckAvailable(dir string, needBytes int64) (StorageStatus, error) {
	if needBytes < 0 {
		return StorageStatus{}, fmt.Errorf("%w: negative requirement %d", ErrInvalidUpload, needBytes)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	st, _ := q.statusForLocked(dir, nil)
	if st.Available && needBytes > st.HeadroomBytes {
		return st, fmt.Errorf("%w: need %d bytes, only %d bytes headroom available", ErrInsufficientStorage, needBytes, st.HeadroomBytes)
	}
	return st, nil
}

// CheckExistingFit verifies whether an already-reserved set of IDs (for example
// a pending batch in the Approval Inbox) still fits on disk right now, taking
// into account any external disk shrinkage since declaration and other active
// reservations on the same volume.
func (q *QuotaManager) CheckExistingFit(dir string, itemIDs []string, fallbackNeedBytes int64) (bool, StorageStatus) {
	q.mu.Lock()
	defer q.mu.Unlock()

	exclude := make(map[string]struct{}, len(itemIDs))
	var need int64
	matched := 0
	for _, id := range itemIDs {
		exclude[id] = struct{}{}
		if e, ok := q.entries[id]; ok {
			need += e.remaining()
			matched++
		}
	}
	if matched == 0 && fallbackNeedBytes > 0 {
		need = fallbackNeedBytes
	}

	st, _ := q.statusForLocked(dir, exclude)
	if !st.Available {
		return true, st
	}
	return need <= st.HeadroomBytes, st
}

// Reserve atomically checks disk headroom and reserves declaredBytes for id.
func (q *QuotaManager) Reserve(id, sessionKey, dir string, declaredBytes int64) error {
	if id == "" {
		return errors.New("transfer: empty reservation id")
	}
	if declaredBytes < 0 {
		return fmt.Errorf("%w: negative reservation size %d", ErrInvalidUpload, declaredBytes)
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	target := q.resolveDirLocked(dir)
	exclude := map[string]struct{}{id: {}}
	st, fsKey := q.statusForLocked(target, exclude)

	committed := int64(0)
	if existing, ok := q.entries[id]; ok {
		committed = existing.committed
		if committed > declaredBytes {
			committed = declaredBytes
		}
	}
	need := declaredBytes - committed
	if st.Available && need > st.HeadroomBytes {
		return fmt.Errorf("%w: need %d bytes, only %d bytes headroom available", ErrInsufficientStorage, need, st.HeadroomBytes)
	}

	q.entries[id] = &quotaEntry{
		id:            id,
		sessionKey:    sessionKey,
		dir:           target,
		fsKey:         fsKey,
		declaredBytes: declaredBytes,
		committed:     committed,
	}
	return nil
}

// ReserveBatch atomically checks and reserves capacity for all items in a batch
// together under one lock. Either every item is reserved or none is.
func (q *QuotaManager) ReserveBatch(sessionKey, dir string, items []QuotaBatchItem) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	target := q.resolveDirLocked(dir)
	exclude := make(map[string]struct{}, len(items))
	var totalNeed int64
	for _, it := range items {
		if it.ID == "" {
			return errors.New("transfer: empty batch reservation id")
		}
		if it.Bytes < 0 {
			return fmt.Errorf("%w: negative batch item size %d", ErrInvalidUpload, it.Bytes)
		}
		exclude[it.ID] = struct{}{}
		committed := int64(0)
		if existing, ok := q.entries[it.ID]; ok {
			committed = existing.committed
			if committed > it.Bytes {
				committed = it.Bytes
			}
		}
		totalNeed += it.Bytes - committed
	}

	st, fsKey := q.statusForLocked(target, exclude)
	if st.Available && totalNeed > st.HeadroomBytes {
		return fmt.Errorf("%w: need %d bytes, only %d bytes headroom available", ErrInsufficientStorage, totalNeed, st.HeadroomBytes)
	}

	for _, it := range items {
		committed := int64(0)
		if existing, ok := q.entries[it.ID]; ok {
			committed = existing.committed
			if committed > it.Bytes {
				committed = it.Bytes
			}
		}
		q.entries[it.ID] = &quotaEntry{
			id:            it.ID,
			sessionKey:    sessionKey,
			dir:           target,
			fsKey:         fsKey,
			declaredBytes: it.Bytes,
			committed:     committed,
		}
	}
	return nil
}

// Replace atomically removes oldID (for example a temporary batch reservation)
// and installs per-item reservations for items without re-running disk headroom
// checks or creating an intermediate unreserved window.
func (q *QuotaManager) Replace(oldID, sessionKey, dir string, items []QuotaBatchItem) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.entries, oldID)
	target := q.resolveDirLocked(dir)
	info := q.probe(target)
	fsKey := fsMatchKey(info, target)
	for _, it := range items {
		if it.ID == "" || it.Bytes < 0 {
			continue
		}
		q.entries[it.ID] = &quotaEntry{
			id:            it.ID,
			sessionKey:    sessionKey,
			dir:           target,
			fsKey:         fsKey,
			declaredBytes: it.Bytes,
			committed:     0,
		}
	}
}

// RestoreReservation reconstructs a reservation during crash recovery without
// failing startup if disk headroom became constrained after the previous crash.
func (q *QuotaManager) RestoreReservation(id, sessionKey, dir string, declaredBytes, committedBytes int64) {
	if id == "" || declaredBytes < 0 {
		return
	}
	if committedBytes < 0 {
		committedBytes = 0
	}
	if committedBytes > declaredBytes {
		committedBytes = declaredBytes
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	target := q.resolveDirLocked(dir)
	info := q.probe(target)
	q.entries[id] = &quotaEntry{
		id:            id,
		sessionKey:    sessionKey,
		dir:           target,
		fsKey:         fsMatchKey(info, target),
		declaredBytes: declaredBytes,
		committed:     committedBytes,
	}
}

// Advance monotonically updates the bytes already committed to disk for id so
// remaining reservation shrinks as OS FreeBytes shrinks.
func (q *QuotaManager) Advance(id string, committedBytes int64) {
	if id == "" || committedBytes <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.entries[id]
	if !ok {
		return
	}
	if committedBytes > e.declaredBytes {
		committedBytes = e.declaredBytes
	}
	if committedBytes > e.committed {
		e.committed = committedBytes
	}
}

// CheckWrite updates committed progress and verifies that the target filesystem
// still has sufficient free space for the remaining write/transfer when metrics
// are available. If external processes exhausted disk space mid-transfer,
// CheckWrite returns ErrInsufficientStorage.
func (q *QuotaManager) CheckWrite(id, dir string, writtenSoFar, nextBytes int64) error {
	if nextBytes < 0 {
		nextBytes = 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	var remaining int64
	target := q.resolveDirLocked(dir)
	if e, ok := q.entries[id]; ok {
		if target == "" {
			target = e.dir
		}
		if writtenSoFar > e.committed {
			if writtenSoFar > e.declaredBytes {
				e.committed = e.declaredBytes
			} else {
				e.committed = writtenSoFar
			}
		}
		remaining = e.remaining()
	}

	info := q.probe(target)
	if !info.Available {
		return nil
	}

	freeSigned := clampUint64ToInt64(info.FreeBytes)
	neededNow := nextBytes
	if remaining > neededNow {
		neededNow = remaining
	}
	if freeSigned-q.safetyMargin < neededNow {
		return fmt.Errorf("%w: only %d bytes free on %s, need %d bytes to finish", ErrInsufficientStorage, info.FreeBytes, info.TargetDir, neededNow)
	}
	return nil
}

// Release removes a reservation by ID (idempotent).
func (q *QuotaManager) Release(id string) {
	if id == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.entries, id)
}

// ReleaseIDs removes multiple reservations at once (idempotent).
func (q *QuotaManager) ReleaseIDs(ids []string) {
	if len(ids) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range ids {
		delete(q.entries, id)
	}
}

// ReleaseSession releases every reservation owned by sessionKey.
func (q *QuotaManager) ReleaseSession(sessionKey string) {
	if sessionKey == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, e := range q.entries {
		if e.sessionKey == sessionKey {
			delete(q.entries, id)
		}
	}
}

// InspectStorageUnknown returns an unknown StorageStatus projection for dir
// when no QuotaManager is attached.
func InspectStorageUnknown(dir string) StorageStatus {
	return StorageStatus{
		TargetDir: dir,
		Available: false,
		Status:    StorageStatusUnknown,
	}
}

// ReservedBytes returns total outstanding uncommitted reserved bytes for dir
// (or all volumes when dir is empty).
func (q *QuotaManager) ReservedBytes(dir string) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if dir == "" {
		var total int64
		for _, e := range q.entries {
			total += e.remaining()
		}
		return total
	}
	info := q.probe(q.resolveDirLocked(dir))
	fsKey := fsMatchKey(info, dir)
	return q.reservedForVolumeLocked(fsKey, nil)
}
