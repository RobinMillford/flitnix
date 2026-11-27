package devices

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// VisibilityMode defines how the local Flitnix desktop exposes itself on the LAN
// and handles unsolicited peer access.
//
// Policy semantics:
//   - VisibilityHidden: suppresses all public LAN discovery announcements and blocks
//     unsolicited peer access and bookmark device-session bootstrap. Explicitly issued
//     QR sessions created by the desktop user retain their authorization unless paused/revoked.
//   - VisibilityTrustedOnly: suppresses identifiable public discovery advertisements
//     (emitting only keyed non-identifying probes if discovery is active) and restricts
//     unsolicited access to enrolled, non-revoked trusted devices.
//   - VisibilityTemporaryEveryone: enables open LAN discovery and incoming requests for
//     10 minutes, then automatically reverts to the previous restrictive mode (hidden or trusted).
//   - VisibilityEveryone: continuously announces sanitized metadata on the LAN and accepts
//     incoming requests (still requiring manual approval for unknown devices).
type VisibilityMode string

const (
	VisibilityHidden            VisibilityMode = "hidden"
	VisibilityTrustedOnly       VisibilityMode = "trusted"
	VisibilityTemporaryEveryone VisibilityMode = "temporary_everyone"
	VisibilityEveryone          VisibilityMode = "everyone"

	DefaultTemporaryVisibilityDuration = 10 * time.Minute
)

var (
	ErrInvalidVisibilityMode = errors.New("devices: invalid visibility mode")
)

// VisibilityState is the serializable snapshot of the visibility state machine.
type VisibilityState struct {
	Mode                    VisibilityMode `json:"mode"`
	PreviousRestrictiveMode VisibilityMode `json:"previousRestrictiveMode"`
	TemporaryExpiresAt      time.Time      `json:"temporaryExpiresAt,omitempty"`
	QRSharingPaused         bool           `json:"qrSharingPaused"`
	RemainingSeconds        int            `json:"remainingSeconds"`
	UpdatedAt               time.Time      `json:"updatedAt"`
}

// ParseVisibilityMode validates and normalizes a string visibility mode.
func ParseVisibilityMode(raw string) (VisibilityMode, error) {
	switch VisibilityMode(strings.ToLower(strings.TrimSpace(raw))) {
	case VisibilityHidden, "off":
		return VisibilityHidden, nil
	case VisibilityTrustedOnly, "trusted_only":
		return VisibilityTrustedOnly, nil
	case VisibilityTemporaryEveryone, "10m", "everyone_10m":
		return VisibilityTemporaryEveryone, nil
	case VisibilityEveryone, "open":
		return VisibilityEveryone, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidVisibilityMode, raw)
	}
}

func isRestrictiveMode(m VisibilityMode) bool {
	return m == VisibilityHidden || m == VisibilityTrustedOnly
}

// VisibilityManager enforces visibility state transitions and temporary-window
// expiration atomically.
type VisibilityManager struct {
	mu        sync.Mutex
	state     VisibilityState
	now       func() time.Time
	onChange  func(VisibilityState)
	onPersist func(VisibilityState) error
}

// NewVisibilityManager initializes a VisibilityManager from persisted state.
// Any expired temporary open window is immediately reverted to its previous
// restrictive mode so expired open sessions never resurrect after a restart.
func NewVisibilityManager(initial VisibilityState, now func() time.Time) *VisibilityManager {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	prev := initial.PreviousRestrictiveMode
	if !isRestrictiveMode(prev) {
		prev = VisibilityTrustedOnly
	}

	mode := initial.Mode
	if parsed, err := ParseVisibilityMode(string(mode)); err == nil {
		mode = parsed
	} else {
		mode = VisibilityTrustedOnly
	}

	curTime := now().UTC()
	expiresAt := initial.TemporaryExpiresAt.UTC()
	if mode == VisibilityTemporaryEveryone {
		if expiresAt.IsZero() || !expiresAt.After(curTime) {
			mode = prev
			expiresAt = time.Time{}
		}
	} else {
		expiresAt = time.Time{}
		if isRestrictiveMode(mode) {
			prev = mode
		}
	}

	st := VisibilityState{
		Mode:                    mode,
		PreviousRestrictiveMode: prev,
		TemporaryExpiresAt:      expiresAt,
		QRSharingPaused:         initial.QRSharingPaused,
		UpdatedAt:               curTime,
	}
	st.RemainingSeconds = computeRemainingSeconds(st, curTime)

	return &VisibilityManager{
		state: st,
		now:   now,
	}
}

// SetHooks installs optional change notification and persistence callbacks.
func (m *VisibilityManager) SetHooks(onChange func(VisibilityState), onPersist func(VisibilityState) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = onChange
	m.onPersist = onPersist
}

func computeRemainingSeconds(st VisibilityState, now time.Time) int {
	if st.Mode != VisibilityTemporaryEveryone || st.TemporaryExpiresAt.IsZero() {
		return 0
	}
	diff := st.TemporaryExpiresAt.Sub(now)
	if diff <= 0 {
		return 0
	}
	return int(math.Ceil(diff.Seconds()))
}

// Evaluate inspects the current state, automatically reverting an expired
// temporary open session back to the previous restrictive mode.
func (m *VisibilityManager) Evaluate() VisibilityState {
	m.mu.Lock()
	now := m.now().UTC()
	changed := false
	if m.state.Mode == VisibilityTemporaryEveryone && (m.state.TemporaryExpiresAt.IsZero() || !now.Before(m.state.TemporaryExpiresAt)) {
		prev := m.state.PreviousRestrictiveMode
		if !isRestrictiveMode(prev) {
			prev = VisibilityTrustedOnly
		}
		m.state.Mode = prev
		m.state.TemporaryExpiresAt = time.Time{}
		m.state.UpdatedAt = now
		changed = true
	}
	m.state.RemainingSeconds = computeRemainingSeconds(m.state, now)
	st := m.state
	onPersist := m.onPersist
	onChange := m.onChange
	m.mu.Unlock()

	if changed {
		if onPersist != nil {
			_ = onPersist(st)
		}
		if onChange != nil {
			onChange(st)
		}
	}
	return st
}

// SetMode transitions the visibility state machine to mode.
// When entering VisibilityTemporaryEveryone, temporaryDuration defaults to 10 minutes
// if <= 0, and PreviousRestrictiveMode is preserved (or set to VisibilityTrustedOnly
// if coming from VisibilityEveryone).
func (m *VisibilityManager) SetMode(mode VisibilityMode, temporaryDuration time.Duration) (VisibilityState, error) {
	parsed, err := ParseVisibilityMode(string(mode))
	if err != nil {
		return VisibilityState{}, err
	}

	m.mu.Lock()
	now := m.now().UTC()
	next := m.state
	next.UpdatedAt = now

	if parsed == VisibilityTemporaryEveryone {
		if temporaryDuration <= 0 {
			temporaryDuration = DefaultTemporaryVisibilityDuration
		}
		if isRestrictiveMode(m.state.Mode) {
			next.PreviousRestrictiveMode = m.state.Mode
		} else if !isRestrictiveMode(next.PreviousRestrictiveMode) {
			next.PreviousRestrictiveMode = VisibilityTrustedOnly
		}
		next.Mode = VisibilityTemporaryEveryone
		next.TemporaryExpiresAt = now.Add(temporaryDuration)
	} else {
		next.Mode = parsed
		next.TemporaryExpiresAt = time.Time{}
		if isRestrictiveMode(parsed) {
			next.PreviousRestrictiveMode = parsed
		}
	}
	next.RemainingSeconds = computeRemainingSeconds(next, now)

	onPersist := m.onPersist
	onChange := m.onChange
	prevState := m.state
	m.state = next
	m.mu.Unlock()

	if onPersist != nil {
		if perr := onPersist(next); perr != nil {
			m.mu.Lock()
			m.state = prevState
			m.mu.Unlock()
			return VisibilityState{}, perr
		}
	}
	if onChange != nil {
		onChange(next)
	}
	return next, nil
}

// SetQRSharingPaused pauses or resumes new transfers on QR sessions without
// changing the discovery visibility mode.
func (m *VisibilityManager) SetQRSharingPaused(paused bool) (VisibilityState, error) {
	m.mu.Lock()
	now := m.now().UTC()
	next := m.state
	next.QRSharingPaused = paused
	next.UpdatedAt = now
	next.RemainingSeconds = computeRemainingSeconds(next, now)
	onPersist := m.onPersist
	onChange := m.onChange
	prevState := m.state
	m.state = next
	m.mu.Unlock()

	if onPersist != nil {
		if perr := onPersist(next); perr != nil {
			m.mu.Lock()
			m.state = prevState
			m.mu.Unlock()
			return VisibilityState{}, perr
		}
	}
	if onChange != nil {
		onChange(next)
	}
	return next, nil
}

// AllowsPublicDiscovery reports whether sanitized metadata may be broadcast openly.
func (s VisibilityState) AllowsPublicDiscovery() bool {
	return s.Mode == VisibilityEveryone || s.Mode == VisibilityTemporaryEveryone
}

// AllowsTrustedDiscovery reports whether keyed trusted-only discovery beacons are active.
func (s VisibilityState) AllowsTrustedDiscovery() bool {
	return s.Mode != VisibilityHidden
}

// AllowsDeviceReconnect reports whether enrolled devices may bootstrap a session
// via bookmark/challenge-response without scanning a new QR code.
func (s VisibilityState) AllowsDeviceReconnect() bool {
	return s.Mode != VisibilityHidden
}

// AllowsUnsolicitedPeerAccess reports whether unknown peers may discover or initiate
// unsolicited peer connection probes.
func (s VisibilityState) AllowsUnsolicitedPeerAccess() bool {
	return s.Mode == VisibilityEveryone || s.Mode == VisibilityTemporaryEveryone
}
