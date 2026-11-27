package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"
)

const (
	// ShortCodeLength is the fixed decimal digit length of one-time connection codes.
	ShortCodeLength = 6

	// DefaultShortCodeTTL bounds the validity window of a single-use connection code.
	DefaultShortCodeTTL = 3 * time.Minute

	// DefaultShortCodeMaxFailures limits consecutive invalid attempts per source IP.
	DefaultShortCodeMaxFailures = 5

	// DefaultShortCodeFailureWindow is the rolling window for counting failed attempts per IP.
	DefaultShortCodeFailureWindow = 5 * time.Minute

	// DefaultShortCodeLockout is the per-IP cooldown enforced after repeated failures.
	DefaultShortCodeLockout = 2 * time.Minute

	maxUsedCodeHistory = 64
	maxIPFailureTrack  = 1024
)

var (
	ErrShortCodeInvalid     = errors.New("session: invalid connection code")
	ErrShortCodeExpired     = errors.New("session: connection code expired")
	ErrShortCodeUsed        = errors.New("session: connection code already used")
	ErrShortCodeRateLimited = errors.New("session: too many invalid connection code attempts")
)

// ShortCodeRateLimitError indicates the requesting IP is temporarily locked out.
type ShortCodeRateLimitError struct {
	RetryAfter time.Duration
}

func (e *ShortCodeRateLimitError) Error() string {
	secs := int(math.Ceil(e.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("too many failed connection code attempts; retry in %ds", secs)
}

func (e *ShortCodeRateLimitError) Unwrap() error { return ErrShortCodeRateLimited }

// ShortCodeConfig configures a ShortCodeManager. Zero values pick secure defaults.
type ShortCodeConfig struct {
	TTL             time.Duration
	MaxFailures     int
	FailureWindow   time.Duration
	LockoutDuration time.Duration
	Now             func() time.Time
}

// ShortCodeView is the desktop UI representation of a session's active short code.
// It is NEVER returned over unauthenticated HTTP endpoints or logged.
type ShortCodeView struct {
	Code             string    `json:"code"`
	FormattedCode    string    `json:"formattedCode"`
	SessionKey       string    `json:"sessionKey"`
	CreatedAt        time.Time `json:"createdAt"`
	ExpiresAt        time.Time `json:"expiresAt"`
	RemainingSeconds int       `json:"remainingSeconds"`
	Active           bool      `json:"active"`
	Used             bool      `json:"used"`
	Expired          bool      `json:"expired"`
}

// ShortCodeRedemption describes the successful atomic consumption of a code.
type ShortCodeRedemption struct {
	SessionKey string
	RedeemedAt time.Time
}

type shortCodeEntry struct {
	sessionKey string
	code       string
	digest     [sha256.Size]byte
	createdAt  time.Time
	expiresAt  time.Time
}

type historicalCode struct {
	sessionKey string
	digest     [sha256.Size]byte
	used       bool
	expired    bool
}

type ipAttemptState struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
	lastSeen    time.Time
}

// ShortCodeManager manages short-lived single-use 6-digit connection codes and
// per-IP brute-force protection.
type ShortCodeManager struct {
	mu            sync.Mutex
	ttl           time.Duration
	maxFailures   int
	failureWindow time.Duration
	lockout       time.Duration
	now           func() time.Time
	hmacKey       [32]byte

	activeBySession map[string]*shortCodeEntry
	lastBySession   map[string]ShortCodeView
	history         []historicalCode
	ipTrackers      map[string]*ipAttemptState
	childSessions   map[string][]string
}

// NewShortCodeManager constructs a ShortCodeManager with cryptographically random HMAC keying.
func NewShortCodeManager(cfg ShortCodeConfig) *ShortCodeManager {
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultShortCodeTTL
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = DefaultShortCodeMaxFailures
	}
	if cfg.FailureWindow <= 0 {
		cfg.FailureWindow = DefaultShortCodeFailureWindow
	}
	if cfg.LockoutDuration <= 0 {
		cfg.LockoutDuration = DefaultShortCodeLockout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		// Fallback is impossible on healthy OS entropy sources, still never predictable.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		key = sum
	}
	return &ShortCodeManager{
		ttl:             cfg.TTL,
		maxFailures:     cfg.MaxFailures,
		failureWindow:   cfg.FailureWindow,
		lockout:         cfg.LockoutDuration,
		now:             cfg.Now,
		hmacKey:         key,
		activeBySession: make(map[string]*shortCodeEntry),
		lastBySession:   make(map[string]ShortCodeView),
		ipTrackers:      make(map[string]*ipAttemptState),
		childSessions:   make(map[string][]string),
	}
}

// FormatShortCode formats a 6-digit code as "123 456" for readability.
func FormatShortCode(code string) string {
	if len(code) != ShortCodeLength {
		return code
	}
	return code[:3] + " " + code[3:]
}

// NormalizeShortCode strips formatting separators and validates 6 ASCII digits.
func NormalizeShortCode(raw string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, raw)
	if len(cleaned) != ShortCodeLength {
		return "", ErrShortCodeInvalid
	}
	for i := 0; i < len(cleaned); i++ {
		if cleaned[i] < '0' || cleaned[i] > '9' {
			return "", ErrShortCodeInvalid
		}
	}
	return cleaned, nil
}

func (m *ShortCodeManager) digestCode(code string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, m.hmacKey[:])
	_, _ = mac.Write([]byte(code))
	var out [sha256.Size]byte
	copy(out[:], mac.Sum(nil))
	return out
}

func generateSixDigitCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("session: generating connection code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (m *ShortCodeManager) pruneExpiredLocked(now time.Time) {
	for sessKey, entry := range m.activeBySession {
		if !now.Before(entry.expiresAt) {
			delete(m.activeBySession, sessKey)
			m.appendHistoryLocked(historicalCode{
				sessionKey: sessKey,
				digest:     entry.digest,
				expired:    true,
			})
			m.lastBySession[sessKey] = ShortCodeView{
				SessionKey: sessKey,
				CreatedAt:  entry.createdAt,
				ExpiresAt:  entry.expiresAt,
				Expired:    true,
			}
		}
	}
}

func (m *ShortCodeManager) appendHistoryLocked(h historicalCode) {
	m.history = append(m.history, h)
	if len(m.history) > maxUsedCodeHistory {
		copy(m.history, m.history[len(m.history)-maxUsedCodeHistory:])
		m.history = m.history[:maxUsedCodeHistory]
	}
}

// GetOrCreate returns the current active code for sessionKey or creates a new one.
func (m *ShortCodeManager) GetOrCreate(sessionKey string) (ShortCodeView, error) {
	if strings.TrimSpace(sessionKey) == "" {
		return ShortCodeView{}, errors.New("session: empty session key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.pruneExpiredLocked(now)
	if entry, ok := m.activeBySession[sessionKey]; ok {
		return viewFromEntry(entry, now), nil
	}
	return m.mintLocked(sessionKey, now)
}

// Regenerate revokes any existing code for sessionKey and creates a fresh one.
func (m *ShortCodeManager) Regenerate(sessionKey string) (ShortCodeView, error) {
	if strings.TrimSpace(sessionKey) == "" {
		return ShortCodeView{}, errors.New("session: empty session key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.pruneExpiredLocked(now)
	if prev, ok := m.activeBySession[sessionKey]; ok {
		delete(m.activeBySession, sessionKey)
		m.appendHistoryLocked(historicalCode{
			sessionKey: sessionKey,
			digest:     prev.digest,
			expired:    true,
		})
	}
	return m.mintLocked(sessionKey, now)
}

// Current returns the current state of sessionKey's short code without minting one.
func (m *ShortCodeManager) Current(sessionKey string) ShortCodeView {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.pruneExpiredLocked(now)
	if entry, ok := m.activeBySession[sessionKey]; ok {
		return viewFromEntry(entry, now)
	}
	if last, ok := m.lastBySession[sessionKey]; ok {
		return last
	}
	return ShortCodeView{SessionKey: sessionKey}
}

func (m *ShortCodeManager) mintLocked(sessionKey string, now time.Time) (ShortCodeView, error) {
	for attempt := 0; attempt < 8; attempt++ {
		code, err := generateSixDigitCode()
		if err != nil {
			return ShortCodeView{}, err
		}
		dig := m.digestCode(code)
		collision := false
		for _, other := range m.activeBySession {
			if subtle.ConstantTimeCompare(other.digest[:], dig[:]) == 1 {
				collision = true
				break
			}
		}
		if collision {
			continue
		}
		entry := &shortCodeEntry{
			sessionKey: sessionKey,
			code:       code,
			digest:     dig,
			createdAt:  now,
			expiresAt:  now.Add(m.ttl),
		}
		m.activeBySession[sessionKey] = entry
		view := viewFromEntry(entry, now)
		m.lastBySession[sessionKey] = view
		return view, nil
	}
	return ShortCodeView{}, errors.New("session: failed to mint unique connection code")
}

func viewFromEntry(entry *shortCodeEntry, now time.Time) ShortCodeView {
	rem := int(math.Ceil(entry.expiresAt.Sub(now).Seconds()))
	if rem < 0 {
		rem = 0
	}
	return ShortCodeView{
		Code:             entry.code,
		FormattedCode:    FormatShortCode(entry.code),
		SessionKey:       entry.sessionKey,
		CreatedAt:        entry.createdAt,
		ExpiresAt:        entry.expiresAt,
		RemainingSeconds: rem,
		Active:           rem > 0,
	}
}

// Redeem atomically verifies and consumes a one-time 6-digit code from clientIP.
func (m *ShortCodeManager) Redeem(clientIP, rawCode string) (ShortCodeRedemption, error) {
	ipKey := strings.TrimSpace(clientIP)
	if ipKey == "" {
		ipKey = "unknown"
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.pruneExpiredLocked(now)

	tracker := m.getOrCreateIPTrackerLocked(ipKey, now)
	if !tracker.lockedUntil.IsZero() && now.Before(tracker.lockedUntil) {
		return ShortCodeRedemption{}, &ShortCodeRateLimitError{
			RetryAfter: tracker.lockedUntil.Sub(now),
		}
	}
	if !tracker.lockedUntil.IsZero() && !now.Before(tracker.lockedUntil) {
		tracker.lockedUntil = time.Time{}
		tracker.failures = 0
		tracker.windowStart = time.Time{}
	}

	code, err := NormalizeShortCode(rawCode)
	if err != nil {
		m.recordFailureLocked(tracker, now)
		return ShortCodeRedemption{}, ErrShortCodeInvalid
	}

	candidateDigest := m.digestCode(code)

	var matched *shortCodeEntry
	for _, entry := range m.activeBySession {
		if subtle.ConstantTimeCompare(entry.digest[:], candidateDigest[:]) == 1 {
			matched = entry
		}
	}
	if matched != nil {
		delete(m.activeBySession, matched.sessionKey)
		m.appendHistoryLocked(historicalCode{
			sessionKey: matched.sessionKey,
			digest:     matched.digest,
			used:       true,
		})
		m.lastBySession[matched.sessionKey] = ShortCodeView{
			SessionKey: matched.sessionKey,
			CreatedAt:  matched.createdAt,
			ExpiresAt:  matched.expiresAt,
			Used:       true,
		}
		delete(m.ipTrackers, ipKey)
		return ShortCodeRedemption{
			SessionKey: matched.sessionKey,
			RedeemedAt: now,
		}, nil
	}

	// Distinguish recently used or expired codes for clear user feedback.
	var cause error = ErrShortCodeInvalid
	for i := len(m.history) - 1; i >= 0; i-- {
		h := m.history[i]
		if subtle.ConstantTimeCompare(h.digest[:], candidateDigest[:]) == 1 {
			if h.used {
				cause = ErrShortCodeUsed
			} else if h.expired {
				cause = ErrShortCodeExpired
			}
			break
		}
	}

	m.recordFailureLocked(tracker, now)
	if !tracker.lockedUntil.IsZero() && now.Before(tracker.lockedUntil) {
		return ShortCodeRedemption{}, &ShortCodeRateLimitError{
			RetryAfter: tracker.lockedUntil.Sub(now),
		}
	}
	return ShortCodeRedemption{}, cause
}

func (m *ShortCodeManager) getOrCreateIPTrackerLocked(ip string, now time.Time) *ipAttemptState {
	if tr, ok := m.ipTrackers[ip]; ok {
		tr.lastSeen = now
		return tr
	}
	if len(m.ipTrackers) >= maxIPFailureTrack {
		var oldestIP string
		var oldestTime time.Time
		for k, v := range m.ipTrackers {
			if oldestIP == "" || v.lastSeen.Before(oldestTime) {
				oldestIP = k
				oldestTime = v.lastSeen
			}
		}
		if oldestIP != "" {
			delete(m.ipTrackers, oldestIP)
		}
	}
	tr := &ipAttemptState{lastSeen: now}
	m.ipTrackers[ip] = tr
	return tr
}

func (m *ShortCodeManager) recordFailureLocked(tr *ipAttemptState, now time.Time) {
	if tr.windowStart.IsZero() || now.Sub(tr.windowStart) > m.failureWindow {
		tr.windowStart = now
		tr.failures = 0
	}
	tr.failures++
	if tr.failures >= m.maxFailures {
		tr.lockedUntil = now.Add(m.lockout)
	}
}

// TrackChildSession links a newly minted Bearer session key to the session key
// whose short code authorized it, so ending the parent session can revoke children.
func (m *ShortCodeManager) TrackChildSession(parentKey, childKey string) {
	if parentKey == "" || childKey == "" || parentKey == childKey {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.childSessions[parentKey] = append(m.childSessions[parentKey], childKey)
}

// RevokeForSession invalidates any active short code for sessionKey and returns
// child session keys minted via that session's short codes.
func (m *ShortCodeManager) RevokeForSession(sessionKey string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.activeBySession, sessionKey)
	delete(m.lastBySession, sessionKey)
	children := m.childSessions[sessionKey]
	delete(m.childSessions, sessionKey)
	return children
}
