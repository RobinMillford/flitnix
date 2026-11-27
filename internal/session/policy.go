package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	pinMinLen     = 4
	pinMaxLen     = 32
	pinHashRounds = 10000
	pinSaltSize   = 16
)

var (
	// ErrInvalidPINFormat indicates a configured PIN did not satisfy length bounds.
	ErrInvalidPINFormat = errors.New("session: PIN must be between 4 and 32 characters")

	// ErrDownloadLimitReached indicates a share session or room has reached its configured download cap.
	ErrDownloadLimitReached = errors.New("session: download limit reached")
)

// PinVerifier validates a user-supplied PIN using a salted iterative SHA-256
// digest compared in constant time. Plaintext PINs are never stored or logged.
type PinVerifier struct {
	enabled bool
	salt    [pinSaltSize]byte
	digest  [sha256.Size]byte
}

// NewPinVerifier constructs a salted verifier from pin. An empty pin disables
// PIN protection and returns a zero PinVerifier without error.
func NewPinVerifier(pin string) (PinVerifier, error) {
	trimmed := strings.TrimSpace(pin)
	if trimmed == "" {
		return PinVerifier{}, nil
	}
	if len([]rune(trimmed)) < pinMinLen || len(trimmed) > pinMaxLen {
		return PinVerifier{}, ErrInvalidPINFormat
	}
	var salt [pinSaltSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return PinVerifier{}, fmt.Errorf("session: generating PIN salt: %w", err)
	}
	return PinVerifier{
		enabled: true,
		salt:    salt,
		digest:  derivePINDigest(salt, trimmed),
	}, nil
}

func derivePINDigest(salt [pinSaltSize]byte, pin string) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte("flitnix-pin-v1"))
	h.Write(salt[:])
	h.Write([]byte{0})
	h.Write([]byte(pin))
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	for i := 1; i < pinHashRounds; i++ {
		h.Reset()
		h.Write(salt[:])
		h.Write(sum[:])
		copy(sum[:], h.Sum(nil))
	}
	return sum
}

// Enabled reports whether PIN verification is active.
func (p PinVerifier) Enabled() bool { return p.enabled }

// Verify returns true iff PIN protection is disabled or candidate matches in constant time.
func (p PinVerifier) Verify(candidate string) bool {
	if !p.enabled {
		return true
	}
	trimmed := strings.TrimSpace(candidate)
	if trimmed == "" || len(trimmed) > pinMaxLen {
		return false
	}
	got := derivePINDigest(p.salt, trimmed)
	return subtle.ConstantTimeCompare(got[:], p.digest[:]) == 1
}

// DownloadQuota enforces atomic download reservations and completed-download caps
// shared by personal share sessions and guest rooms.
type DownloadQuota struct {
	mu        sync.Mutex
	limit     int
	completed int
	reserved  map[string]struct{}
}

// Init sets the maximum number of completed downloads (0 means unlimited).
func (q *DownloadQuota) Init(limit int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if limit < 0 {
		limit = 0
	}
	q.limit = limit
	q.completed = 0
	q.reserved = nil
}

// TryReserve atomically reserves a download slot for reservationID.
// Calling TryReserve again with the same reservationID is idempotent so
// resumable HTTP Range continuations on one grant do not consume extra slots.
func (q *DownloadQuota) TryReserve(reservationID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit <= 0 {
		return true
	}
	if reservationID != "" {
		if _, ok := q.reserved[reservationID]; ok {
			return true
		}
	}
	if q.completed+len(q.reserved) >= q.limit {
		return false
	}
	if q.reserved == nil {
		q.reserved = make(map[string]struct{})
	}
	if reservationID != "" {
		q.reserved[reservationID] = struct{}{}
	}
	return true
}

// Release frees a previously reserved download slot (for failed/aborted/expired grants).
func (q *DownloadQuota) Release(reservationID string) {
	if reservationID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.reserved, reservationID)
}

// Consume finalizes reservationID as a completed download and returns the new
// completed count and whether the limit is now reached.
func (q *DownloadQuota) Consume(reservationID string) (completed int, exhausted bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit <= 0 {
		q.completed++
		return q.completed, false
	}
	delete(q.reserved, reservationID)
	q.completed++
	return q.completed, q.completed >= q.limit
}

// Stats returns (limit, completed, remaining). remaining is -1 when unlimited.
func (q *DownloadQuota) Stats() (limit, completed, remaining int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	limit = q.limit
	completed = q.completed
	if limit <= 0 {
		return 0, completed, -1
	}
	rem := limit - completed
	if rem < 0 {
		rem = 0
	}
	return limit, completed, rem
}

// Exhausted reports whether all download slots have completed.
func (q *DownloadQuota) Exhausted() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.limit > 0 && q.completed >= q.limit
}

// PolicyOptions configures optional PIN protection, custom expiration, and
// download limits for a new sharing session.
type PolicyOptions struct {
	PinCode        string
	MaxDownloads   int
	ExpiresIn      time.Duration
	OneTimeAutoEnd bool
}

// HashUnlockProof produces a hex digest for tracking session PIN unlock cookies
// without holding the raw cookie value.
func HashUnlockProof(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
