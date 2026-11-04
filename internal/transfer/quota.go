package transfer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	// ErrInsufficientStorage indicates not enough free disk space is available.
	ErrInsufficientStorage = errors.New("transfer: insufficient storage space")

	// ErrQuotaExceeded indicates the concurrent upload limit has been exceeded.
	ErrQuotaExceeded = errors.New("transfer: concurrent upload quota exceeded")
)

const (
	// DefaultSafetyMargin is the 500 MB disk margin that must remain free.
	DefaultSafetyMargin int64 = 500 * 1024 * 1024

	// DefaultMaxConcurrentPerSession is the maximum active resumable uploads per session.
	DefaultMaxConcurrentPerSession = 10
)

type reservation struct {
	sessionKey      string
	declaredSize    int64
	committedOffset int64
}

// QuotaManager tracks outstanding capacity reservations for resumable uploads.
type QuotaManager struct {
	mu                      sync.Mutex
	dir                     string
	safetyMargin            int64
	maxConcurrentPerSession int
	reservations            map[string]*reservation
	freeSpaceFunc           func(dir string) (uint64, error)
}

// NewQuotaManager creates a new QuotaManager.
func NewQuotaManager(dir string, safetyMargin int64, maxConcurrent int) *QuotaManager {
	if safetyMargin <= 0 {
		safetyMargin = DefaultSafetyMargin
	}
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrentPerSession
	}
	return &QuotaManager{
		dir:                     dir,
		safetyMargin:            safetyMargin,
		maxConcurrentPerSession: maxConcurrent,
		reservations:            make(map[string]*reservation),
		freeSpaceFunc:           getPlatformFreeSpace,
	}
}

// SetFreeSpaceFunc allows overriding disk free space lookup (for tests).
func (qm *QuotaManager) SetFreeSpaceFunc(f func(dir string) (uint64, error)) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	qm.freeSpaceFunc = f
}

// SetDir updates the target directory for disk space checks.
func (qm *QuotaManager) SetDir(dir string) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	qm.dir = dir
}

// Reserve atomically reserves disk space for an incoming upload.
func (qm *QuotaManager) Reserve(sessionKey, uploadID string, declaredSize int64) error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	// If already reserved for this uploadID, return nil (idempotent)
	if _, exists := qm.reservations[uploadID]; exists {
		return nil
	}

	// Check per-session concurrency limit
	sessionActive := 0
	for _, res := range qm.reservations {
		if res.sessionKey == sessionKey {
			sessionActive++
		}
	}
	if sessionActive >= qm.maxConcurrentPerSession {
		return ErrQuotaExceeded
	}

	// Calculate system free space
	checkDir := qm.dir
	if checkDir == "" {
		checkDir = "."
	}
	freeBytes, err := qm.freeSpaceFunc(checkDir)
	if err != nil {
		// If checkDir doesn't exist yet, try parent or "."
		if parent := filepath.Dir(checkDir); parent != "" && parent != checkDir {
			freeBytes, err = qm.freeSpaceFunc(parent)
		}
		if err != nil {
			return err
		}
	}

	// Sum outstanding reservations: sum(declaredSize - committedOffset)
	var outstanding int64
	for _, res := range qm.reservations {
		remaining := res.declaredSize - res.committedOffset
		if remaining > 0 {
			outstanding += remaining
		}
	}

	effectiveAvailable := int64(freeBytes) - outstanding - qm.safetyMargin
	if declaredSize > effectiveAvailable {
		return ErrInsufficientStorage
	}

	qm.reservations[uploadID] = &reservation{
		sessionKey:      sessionKey,
		declaredSize:    declaredSize,
		committedOffset: 0,
	}
	return nil
}

// UpdateOffset updates the committed byte offset for an upload, reducing uncommitted reservation.
func (qm *QuotaManager) UpdateOffset(uploadID string, committedOffset int64) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	res, ok := qm.reservations[uploadID]
	if !ok {
		return
	}
	if committedOffset > res.declaredSize {
		committedOffset = res.declaredSize
	}
	res.committedOffset = committedOffset
}

// Release releases the reservation for an upload (e.g. on completion, deletion, or cancellation).
func (qm *QuotaManager) Release(uploadID string) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	delete(qm.reservations, uploadID)
}

// ActiveReservationsCount returns the count of active reservations.
func (qm *QuotaManager) ActiveReservationsCount() int {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	return len(qm.reservations)
}

// TotalReservedRemaining returns the sum of uncommitted reserved bytes.
func (qm *QuotaManager) TotalReservedRemaining() int64 {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	var total int64
	for _, res := range qm.reservations {
		rem := res.declaredSize - res.committedOffset
		if rem > 0 {
			total += rem
		}
	}
	return total
}

// RecoverFromStaging reads all .meta files from stagingDir and reconstructs outstanding reservations.
func (qm *QuotaManager) RecoverFromStaging(stagingDir string) error {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	qm.mu.Lock()
	defer qm.mu.Unlock()

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta") {
			continue
		}
		metaPath := filepath.Join(stagingDir, entry.Name())
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}

		var partial struct {
			TransferID      string `json:"transferId"`
			SessionKey      string `json:"sessionKey"`
			TotalSize       int64  `json:"totalSize"`
			CommittedOffset int64  `json:"committedOffset"`
			State           string `json:"state"`
		}
		if err := json.Unmarshal(data, &partial); err != nil {
			continue
		}

		// Only reconstruct for active, interrupted, or awaiting_digest states
		if partial.State == "completed" || partial.State == "cancelled" || partial.State == "failed" {
			continue
		}

		if partial.TransferID != "" && partial.TotalSize > 0 {
			qm.reservations[partial.TransferID] = &reservation{
				sessionKey:      partial.SessionKey,
				declaredSize:    partial.TotalSize,
				committedOffset: partial.CommittedOffset,
			}
		}
	}
	return nil
}
