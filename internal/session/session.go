// Package session manages the pairing sessions between the desktop and a
// phone browser.
//
// A session is a random secret token carried by the future QR URL fragment
// (stage 4). The phone reads the fragment locally and repeats the token in
// an Authorization header on every API request; the server never sees it in
// the URL.
//
// The package depends only on the Go standard library and is independent of
// both the Wails desktop layer and the HTTP layer, so it is exercised
// directly under go test.
//
// Lifecycle rules:
//
//   - Create inserts an active session with a fresh 256-bit token.
//   - Every successful Validate refreshes the session's idle deadline
//     (sliding window). An idle session expires lazily and is then answered
//     with ErrExpired.
//   - A session in use by an ongoing transfer is pinned (Begin/End) and is
//     never expired while pinned, so a large streamed file is not aborted
//     by the idle timer. Both Begin and End refresh the idle deadline, so
//     the idle window is measured from the END of the last transfer, not
//     from the start of its request: a transfer that outlasts the idle
//     window must not leave the session already expired the moment it
//     finishes (that would kill the very next batch item or status poll).
//     Once unpinned and idle for a full window, the next pass prunes it.
//   - Revocation and explicit expiry terminate a session immediately; the
//     Managing Manager remembers recent endings (bounded) so callers get a
//     precise expired/revoked/invalid distinction after the fact.
//   - Close revokes every session and stops accepting new ones.
//
// Secret handling: tokens are not logged, not sent in URLs, not persisted,
// and compared only through their SHA-256 digests using
// crypto/subtle.ConstantTimeCompare, so presented-token comparisons never
// branch or time on secret byte differences.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// TokenLen is the raw token entropy in bytes (256 bits).
const TokenLen = 32

// tokenEncodedLen is the length of a base64.RawURLEncoding-encoded token
// (43 characters, URL-safe, no padding).
const tokenEncodedLen = 43

// DefaultIdleTimeout is the idle window after which an unpinned session
// expires. Every authenticated request refreshes it.
const DefaultIdleTimeout = 10 * time.Minute

// defaultMaxSessions bounds how many sessions may be active at once.
// Session creation is a desktop action, never a request-side action, so
// this is a backstop rather than a live attack surface.
const defaultMaxSessions = 8

// maxHistory bounds how many ended sessions are remembered for accurate
// expired/revoked/invalid error reporting.
const maxHistory = 32

var (
	// ErrInvalid means the presented token matches no known session.
	ErrInvalid = errors.New("session: invalid token")

	// ErrExpired means the presented token belonged to an idle-expired
	// session.
	ErrExpired = errors.New("session: session expired")

	// ErrRevoked means the presented token belonged to an explicitly
	// revoked or closed session.
	ErrRevoked = errors.New("session: session revoked")

	// ErrClosed means the manager no longer accepts new sessions.
	ErrClosed = errors.New("session: manager closed")

	// ErrTooManySessions means the active-session cap is reached.
	ErrTooManySessions = errors.New("session: too many active sessions")
)

// Config configures a Manager. Zero values pick package defaults.
type Config struct {
	// IdleTimeout is the sliding idle window. 0 uses DefaultIdleTimeout;
	// values under one second are clamped up so a session can never live
	// for effectively zero time.
	IdleTimeout time.Duration

	// MaxSessions caps concurrent active sessions. 0 uses the default (8).
	MaxSessions int

	// Now supplies the clock; tests use it to move time around. nil uses
	// time.Now.
	Now func() time.Time

	// OnExpire, when set, is invoked (without any Manager lock held) with the
	// non-secret key of every session that ended by idle expiry. The desktop
	// uses it to release transfer state owned by a session nobody can reach
	// any more. Pinned sessions never expire, so the hook can never fire for
	// a session with a live transfer stream. It must not block for long.
	OnExpire func(key string)

	// OnEnd, when set, is invoked (without any Manager lock held) with one
	// EndEvent for every session that leaves the active set: idle expiry,
	// revocation and manager close. It is the diagnostic counterpart of
	// OnExpire: when a phone reports an unexpected "session ended", the
	// desktop can name the concrete reason from its own log. Events carry
	// only a non-secret key and a reason category. It must not block for
	// long, and it may call back into the Manager.
	OnEnd func(EndEvent)

	// OnCreate is called before a newly created session becomes active. It
	// must durably record the session or return an error.
	OnCreate func(SessionRecord) error

	// OnRevoke, when set, durably records an explicit revocation or an idle
	// expiry before the session leaves the active set. An error aborts an
	// explicit revocation (the caller must not observe a success that is not
	// durable); an error during lazy expiry is retried on the next pass, and
	// the absolute ExpiresAt already on record still stops a restart from
	// resurrecting the session.
	OnRevoke func(SessionRecord) error

	// Verifier derives the durable token verifier. The composition root wires
	// it to the installation-keyed HMAC-SHA256 from the recovery store, so a
	// persisted record never contains the raw token and cannot be verified
	// without the installation key. nil keeps the legacy unkeyed SHA-256
	// digest, which is correct for an in-memory-only manager but must never
	// be persisted.
	Verifier func(token string) [sha256.Size]byte
}

// SessionRecord is the non-secret state required to restore a session.
type SessionRecord struct {
	// SessionID is a stable identity reference. It is not a credential.
	SessionID string

	// SessionKey is the legacy stable owner key (hex SHA-256 of the token)
	// that resumable metadata already binds to.
	SessionKey string

	// Verifier is the installation-keyed HMAC-SHA256 of the token. The raw
	// token is never part of a record.
	Verifier [sha256.Size]byte

	// Generation increments on credential rotation or revocation so an old
	// verifier can be invalidated deliberately.
	Generation uint64

	Status    Status
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time
}

// EndReason categorizes why a session ended. Values are stable identifiers
// suitable for logs and error reports; they never carry any secret.
type EndReason string

const (
	// EndIdleExpiry: the session sat idle (unpinned) past its window and
	// expired lazily on the next pass.
	EndIdleExpiry EndReason = "idle_expiry"

	// EndRevoked: the session was ended explicitly — by the desktop's own
	// EndSession flow, by a Revoke call, or by RevokeSession.
	EndRevoked EndReason = "revoked"

	// EndShutdown: the Manager itself closed, terminating every session it
	// still held (desktop shutdown or server replacement).
	EndShutdown EndReason = "shutdown"
)

// EndEvent is one notification that a session left the active set.
type EndEvent struct {
	// Key is the session's non-secret identifier (hex SHA-256 of its
	// token). It is safe to log; the token itself never is.
	Key string

	// Reason is why the session ended.
	Reason EndReason
}

// Status describes why a session ended.
type Status string

const (
	StatusActive  Status = "active"
	StatusExpired Status = "expired"
	StatusRevoked Status = "revoked"
)

// Session is one pairing token. All read accessors are safe for concurrent
// use; mutable state is atomic or only mutated under the owning Manager's
// lock.
type Session struct {
	token  string // raw token: never log, never persist outside process memory
	digest [sha256.Size]byte
	key    string // cached hex representation of digest for zero-alloc Key() reads

	// verifier is what durable records hold and what Validate compares. It is
	// HMAC-SHA256 under the installation key when one is configured.
	verifier [sha256.Size]byte

	id         string
	generation uint64

	created    time.Time
	idle       time.Duration
	expiresN   atomic.Int64 // unix nanos of the idle deadline
	revokedN   atomic.Int64 // nonzero once revoked
	pinned     atomic.Int32 // transfers currently using the session
	nowFactory func() time.Time
}

// Manager owns all sessions. Every method is safe for concurrent use.
type Manager struct {
	mu   sync.Mutex
	idle time.Duration
	max  int
	now  func() time.Time

	active []*Session // creation-ordered active sessions
	hist   []histItem // bounded memory of ended sessions for error accuracy
	closed bool

	onExpire func(key string)
	expired  []string // keys of freshly expired sessions awaiting OnExpire

	onEnd    func(EndEvent)
	onCreate func(SessionRecord) error
	onRevoke func(SessionRecord) error
	ended    []EndEvent // end events awaiting OnEnd, in end order

	// verifier is immutable after construction, so readers need no lock.
	verifier func(string) [sha256.Size]byte
}

// SetCreateHook installs the durable creation hook used by a composition
// root after restoring an existing manager.
func (m *Manager) SetCreateHook(hook func(SessionRecord) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onCreate = hook
}

// SetRevokeHook installs the durable revocation/expiry sink. A revocation is
// committed before it is applied; an expiry is committed best effort because
// the absolute ExpiresAt on record is the authoritative guard.
func (m *Manager) SetRevokeHook(hook func(SessionRecord) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onRevoke = hook
}

// SetVerifier installs the installation-keyed token verifier. It is only
// legal before any session exists (normally during startup recovery), so a
// live session can never be interpreted with a different derivation than the
// record it was restored from.
func (m *Manager) SetVerifier(fn func(token string) [sha256.Size]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) > 0 {
		return errors.New("session: cannot change the token verifier while sessions are active")
	}
	m.verifier = fn
	return nil
}

type histItem struct {
	verifier [sha256.Size]byte
	status   Status
}

// NewManager prepares a session manager. Construction cannot fail.
func NewManager(cfg Config) *Manager {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.IdleTimeout < time.Second {
		cfg.IdleTimeout = time.Second
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{
		idle: cfg.IdleTimeout, max: cfg.MaxSessions, now: now,
		onExpire: cfg.OnExpire, onEnd: cfg.OnEnd, onCreate: cfg.OnCreate,
		onRevoke: cfg.OnRevoke, verifier: cfg.Verifier,
	}
}

// verify derives the lookup value for a presented token: the
// installation-keyed HMAC when configured, otherwise the legacy digest.
func (m *Manager) verify(token string) [sha256.Size]byte {
	if m.verifier != nil {
		return m.verifier(token)
	}
	return digestToken(token)
}

// newSessionID mints a stable, non-secret session identity.
func newSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("session: generating session id: %w", err)
	}
	return "sess_" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// recordFor projects a live session into its durable record.
func (m *Manager) recordFor(s *Session, status Status) SessionRecord {
	rec := SessionRecord{
		SessionID:  s.id,
		SessionKey: s.Key(),
		Verifier:   s.verifier,
		Generation: s.generation,
		Status:     status,
		CreatedAt:  s.created,
		ExpiresAt:  s.ExpiresAt(),
	}
	if status == StatusRevoked {
		rec.RevokedAt = m.now()
	}
	return rec
}

// generateToken returns a fresh URL-safe 256-bit token.
func generateToken() (string, error) {
	var raw [TokenLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("session: generating token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func digestToken(tok string) [sha256.Size]byte { return sha256.Sum256([]byte(tok)) }

// Create inserts a new active session and returns it, including its raw
// token (the desktop needs it, e.g. to build the QR URL in stage 4).
func (m *Manager) Create() (*Session, error) {
	tok, err := generateToken()
	if err != nil {
		return nil, err
	}
	defer m.notifyExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	m.pruneLocked(m.now())
	if len(m.active) >= m.max {
		return nil, fmt.Errorf("%w (limit %d)", ErrTooManySessions, m.max)
	}
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	now := m.now()
	d := digestToken(tok)
	s := &Session{
		token:      tok,
		digest:     d,
		key:        hex.EncodeToString(d[:]),
		verifier:   m.verify(tok),
		id:         id,
		generation: 1,
		created:    now,
		idle:       m.idle,
		nowFactory: m.now,
	}
	s.expiresN.Store(now.Add(m.idle).UnixNano())
	if m.onCreate != nil {
		record := m.recordFor(s, StatusActive)
		if err := m.onCreate(record); err != nil {
			return nil, fmt.Errorf("session: persist creation: %w", err)
		}
	}
	m.active = append(m.active, s)
	return s, nil
}

// Restore inserts an authenticated session from durable state. The raw token
// is intentionally not required or stored: Validate derives the
// installation-keyed verifier and compares it in constant time.
//
// Expiry is durable. A record whose ExpiresAt has already passed is not
// resurrected with a fresh idle window; it is remembered as expired so a
// presented token still gets ErrExpired rather than ErrInvalid. The same
// applies to revoked records, which never become active again.
func (m *Manager) Restore(record SessionRecord) error {
	if record.SessionID == "" || record.SessionKey == "" || record.Verifier == ([sha256.Size]byte{}) {
		return errors.New("session: invalid restore record")
	}
	keyBytes, err := hex.DecodeString(record.SessionKey)
	if err != nil || len(keyBytes) != sha256.Size {
		return errors.New("session: invalid restore session key")
	}
	var digest [sha256.Size]byte
	copy(digest[:], keyBytes)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}

	switch record.Status {
	case StatusActive:
		// fall through to the expiry check below
	case StatusRevoked:
		m.hist = append(m.hist, histItem{verifier: record.Verifier, status: StatusRevoked})
		return nil
	case StatusExpired:
		m.hist = append(m.hist, histItem{verifier: record.Verifier, status: StatusExpired})
		return nil
	default:
		return errors.New("session: invalid restore status")
	}

	if !record.ExpiresAt.IsZero() && !record.ExpiresAt.After(m.now()) {
		m.hist = append(m.hist, histItem{verifier: record.Verifier, status: StatusExpired})
		return nil
	}
	if len(m.active) >= m.max {
		return fmt.Errorf("%w (limit %d)", ErrTooManySessions, m.max)
	}

	s := &Session{
		digest:     digest,
		key:        hex.EncodeToString(digest[:]),
		verifier:   record.Verifier,
		id:         record.SessionID,
		generation: record.Generation,
		created:    record.CreatedAt,
		idle:       m.idle,
		nowFactory: m.now,
	}
	s.expiresN.Store(record.ExpiresAt.UnixNano())
	m.active = append(m.active, s)
	return nil
}

// Validate authenticates a presented token. On success it returns the
// session and refreshes its idle deadline. Errors distinguish unknown,
// expired and revoked tokens. Comparisons run in constant time per
// candidate via crypto/subtle over 32-byte digests; the digest is compared
// rather than the token itself, so no token material is ever staged for a
// linear comparison.
func (m *Manager) Validate(token string) (*Session, error) {
	if len(token) != tokenEncodedLen {
		return nil, ErrInvalid
	}
	if _, err := base64.RawURLEncoding.Strict().DecodeString(token); err != nil {
		return nil, ErrInvalid
	}
	d := m.verify(token)

	defer m.notifyExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(m.now())

	var found *Session
	for _, s := range m.active {
		if subtle.ConstantTimeCompare(d[:], s.verifier[:]) == 1 {
			found = s
		}
	}
	if found == nil {
		return nil, m.histStatus(d)
	}
	found.expiresN.Store(m.now().Add(found.idle).UnixNano())
	return found, nil
}

// histStatus turns a digest that no longer points to an active session into
// the most accurate error for the caller.
func (m *Manager) histStatus(d [sha256.Size]byte) error {
	for _, h := range m.hist {
		if subtle.ConstantTimeCompare(d[:], h.verifier[:]) == 1 {
			if h.status == StatusExpired {
				return ErrExpired
			}
			return ErrRevoked
		}
	}
	return ErrInvalid
}

// pruneLocked expires idle unpinned sessions and trims the bounded history.
// Callers must hold m.mu.
func (m *Manager) pruneLocked(now time.Time) {
	// Collect first: endSessionLocked rewrites m.active, so it must never run
	// while ranging over it. Pinned sessions survive the idle deadline; they
	// are never dropped here because that would abort an active transfer.
	var stale []*Session
	for _, s := range m.active {
		if s.staleUnpinned(now) {
			stale = append(stale, s)
		}
	}
	for _, s := range stale {
		// Durable expiry is best effort. The absolute ExpiresAt already on
		// record is what stops a restart from resurrecting the session, so a
		// failed expiry append must never keep a stale session authorized:
		// the in-memory session still ends here.
		if m.onRevoke != nil {
			_ = m.onRevoke(m.recordFor(s, StatusExpired))
		}
		m.endSessionLocked(s, StatusExpired)
		if m.onExpire != nil {
			m.expired = append(m.expired, s.Key())
		}
		if m.onEnd != nil {
			m.ended = append(m.ended, EndEvent{Key: s.Key(), Reason: EndIdleExpiry})
		}
	}
	for len(m.hist) > maxHistory {
		m.hist = m.hist[len(m.hist)-maxHistory:]
	}
}

// endSessionLocked moves a session out of the active list into the bounded
// history with the given end status. Callers must hold m.mu. The order in
// the history is append order, oldest first.
func (m *Manager) endSessionLocked(s *Session, status Status) {
	for i, cur := range m.active {
		if cur == s {
			m.active = append(m.active[:i], m.active[i+1:]...)
			break
		}
	}
	m.hist = append(m.hist, histItem{verifier: s.verifier, status: status})
	if len(m.hist) > maxHistory {
		m.hist = m.hist[len(m.hist)-maxHistory:]
	}
}

// Revoke ends the session identified by the given token. After it returns,
// the token no longer validates.
//
// Revocation is durable: the revocation record (with its bumped generation)
// is committed before the session leaves the active set, so a success can
// never be undone by a restart. If the durable write fails the session stays
// active and the error is returned.
func (m *Manager) Revoke(token string) error {
	if len(token) != tokenEncodedLen {
		return ErrInvalid
	}
	d := m.verify(token)
	defer m.notifyExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.active {
		if subtle.ConstantTimeCompare(d[:], s.verifier[:]) == 1 {
			return m.revokeLocked(s)
		}
	}
	return ErrInvalid
}

// RevokeSession ends the given session. After it returns, Validate of the
// session's token fails.
//
// The revocation is committed durably first; a durable-write failure is
// returned and the session stays active, because reporting success for a
// revocation that a restart would undo is a security boundary violation.
func (m *Manager) RevokeSession(s *Session) error {
	if s == nil {
		return ErrInvalid
	}
	defer m.notifyExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokeLocked(s)
}

// revokeLocked persists then applies one revocation. Callers hold m.mu.
func (m *Manager) revokeLocked(s *Session) error {
	s.generation++
	if m.onRevoke != nil {
		if err := m.onRevoke(m.recordFor(s, StatusRevoked)); err != nil {
			// Roll the generation back so a later retry is not a generation
			// bump that never reached durable state.
			s.generation--
			return fmt.Errorf("session: persist revocation: %w", err)
		}
	}
	s.revokedN.Store(m.now().UnixNano())
	m.endSessionLocked(s, StatusRevoked)
	if m.onEnd != nil {
		m.ended = append(m.ended, EndEvent{Key: s.Key(), Reason: EndRevoked})
	}
	return nil
}

// Shutdown stops the manager without revoking its sessions.
//
// A graceful desktop shutdown must not destroy sessions that are meant to
// survive a restart: the durable records stay active and the next process
// restores them. Use Close for an explicit teardown that must end every
// session.
func (m *Manager) Shutdown() {
	defer m.notifyExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	m.active = nil
}

// Close revokes every active session and stops the manager. Validate
// afterwards fails for every previously active token, and Create fails with
// ErrClosed.
//
// Close is an explicit teardown. Graceful application shutdown must call
// Shutdown instead so that sessions meant to survive a restart are not
// durably revoked. Any durable revocation error is deliberately swallowed
// here: Close reports nothing and the in-memory session still ends, which is
// the failing side of the security boundary.
func (m *Manager) Close() {
	defer m.notifyExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.closed = true
	for len(m.active) > 0 {
		s := m.active[0]
		s.generation++
		if m.onRevoke != nil {
			_ = m.onRevoke(m.recordFor(s, StatusRevoked))
		}
		s.revokedN.Store(m.now().UnixNano())
		m.endSessionLocked(s, StatusRevoked)
		if m.onEnd != nil {
			m.ended = append(m.ended, EndEvent{Key: s.Key(), Reason: EndShutdown})
		}
	}
}

// ActiveSessions returns the currently active sessions in creation order.
// The sessions share state with the manager; use the documented methods to
// read or end them.
func (m *Manager) ActiveSessions() []*Session {
	defer m.notifyExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(m.now())
	out := make([]*Session, len(m.active))
	copy(out, m.active)
	return out
}

// Len returns the number of active sessions.
func (m *Manager) Len() int {
	defer m.notifyExpired()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(m.now())
	return len(m.active)
}

// notifyExpired delivers pending end callbacks after the Manager lock is
// released, so a callback may freely call back into the Manager. It drains
// both queues: OnExpire (idle-expired keys only, transfer release) and OnEnd
// (every ending, diagnostics). Queues hold work only between an ending and
// the next drain — every method that can end a session drains on its way
// out — so memory stays bounded even if a callback never returns.
func (m *Manager) notifyExpired() {
	if m.onExpire == nil && m.onEnd == nil {
		return
	}
	m.mu.Lock()
	expired := m.expired
	m.expired = nil
	ended := m.ended
	m.ended = nil
	m.mu.Unlock()
	for _, key := range expired {
		if m.onExpire != nil {
			m.onExpire(key)
		}
	}
	for _, event := range ended {
		if m.onEnd != nil {
			m.onEnd(event)
		}
	}
}

// ---- session predicates shared by manager and session ----

// staleUnpinned reports whether the session's idle deadline has passed
// while no ongoing transfer holds a pin on it.
func (s *Session) staleUnpinned(now time.Time) bool {
	return s.pinned.Load() == 0 && now.After(time.Unix(0, s.expiresN.Load()))
}

// Begin marks the session as being used by an ongoing transfer. A pinned
// session survives its idle deadline, so an in-flight large transfer is
// never aborted by the idle timer. Every Begin must be paired with an End
// (defer End).
func (s *Session) Begin() {
	s.pinned.Add(1)
	s.touch()
}

// touch restarts the idle window from now.
func (s *Session) touch() {
	s.expiresN.Store(s.nowFactory().Add(s.idle).UnixNano())
}

// End releases one pin acquired by Begin. The pin count never goes below
// zero even if a handler pair Begin/End is buggy.
func (s *Session) End() {
	// The idle window restarts when the transfer ends. Without this the
	// deadline would still be the one set when the transfer's request began,
	// so any transfer longer than the idle window would leave the session
	// expired the instant it was unpinned. The deadline is refreshed BEFORE
	// the pin is released: a concurrent prune must never observe "unpinned"
	// together with the stale deadline.
	s.touch()
	if s.pinned.Add(-1) < 0 {
		s.pinned.Store(0)
	}
}

// PinnedTransfers reports how many ongoing transfers currently pin this
// session.
func (s *Session) PinnedTransfers() int32 { return s.pinned.Load() }

// Token returns the raw session token. Treat it as a secret: do not log it
// and never send it anywhere except the intended QR flow (stage 4).
func (s *Session) Token() string { return s.token }

// Key returns a stable non-secret identifier for the session (hex-encoded
// SHA-256 of the token). Transfer records bind to this key instead of
// propagating the raw token around the system.
func (s *Session) Key() string {
	if s.key != "" {
		return s.key
	}
	return hex.EncodeToString(s.digest[:])
}

// ID returns the stable session identity reference. It is not a credential.
func (s *Session) ID() string { return s.id }

// Generation reports the current credential generation.
func (s *Session) Generation() uint64 { return s.generation }

// Digest returns the raw 32-byte SHA-256 digest of the session token.
func (s *Session) Digest() [32]byte { return s.digest }

// Created returns the creation time.
func (s *Session) Created() time.Time { return s.created }

// IdleTimeout returns the configured idle window.
func (s *Session) IdleTimeout() time.Duration { return s.idle }

// ExpiresAt returns the moment the session becomes idle-expired, assuming
// no further activity and no active transfers.
func (s *Session) ExpiresAt() time.Time { return time.Unix(0, s.expiresN.Load()) }

// Status reports the current lifecycle status. A session pinned by ongoing
// transfers past its idle deadline still reports active; it expires once
// unpinned and staled, at the next validation pass.
func (s *Session) Status() Status {
	if s.revokedN.Load() != 0 {
		return StatusRevoked
	}
	if s.staleUnpinned(s.nowFactory()) {
		return StatusExpired
	}
	return StatusActive
}
