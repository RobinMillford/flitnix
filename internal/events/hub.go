// Package events provides a bounded, non-blocking, scope-isolated pub/sub
// event hub for real-time state updates across desktop (Wails), personal
// mobile sessions (SSE), and Guest Drop Box sessions (SSE).
//
// Design invariants:
//   - Publishing never blocks transfer or HTTP hot paths.
//   - Internal routing keys (sessionKey, roomID, guestID) are never serialized
//     into client JSON payloads.
//   - Per-subscriber outbound queues are bounded; progress events yield to
//     critical state transitions under congestion, and overflow triggers an
//     authoritative resync signal.
package events

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ScopePersonal = "personal"
	ScopeGuest    = "guest"
	ScopeDesktop  = "desktop"

	MaxPayloadBytes       = 16 * 1024
	DefaultQueueSize      = 64
	ProgressBackpressure  = 48 // shed progress events once queue >= 75% full
	MaxGlobalPersonalSubs = 64
	MaxPerSessionSubs     = 8
	MaxGlobalGuestSubs    = 128
	MaxPerGuestSubs       = 4
	MaxDesktopSubs        = 16

	DefaultProgressInterval = 200 * time.Millisecond
	maxThrottleEntries      = 1024
)

// Standard event type names.
const (
	TypeBatchPending       = "batch.pending"
	TypeBatchUpdated       = "batch.updated"
	TypeBatchProgress      = "batch.progress"
	TypeUploadPending      = "upload.pending"
	TypeUploadUpdated      = "upload.updated"
	TypeUploadProgress     = "upload.progress"
	TypeDownloadUpdated    = "download.updated"
	TypeDownloadProgress   = "download.progress"
	TypeTextMessage        = "text.message"
	TypeTextCleared        = "text.cleared"
	TypeFilesUpdated       = "files.updated"
	TypeSessionCreated     = "session.created"
	TypeSessionEnded       = "session.ended"
	TypeVisibilityChanged  = "visibility.changed"
	TypePairingPrompt      = "pairing.prompt"
	TypePairingResolved    = "pairing.resolved"
	TypeGuestStatus        = "guest.status"
	TypeGuestRoomUpdated   = "guest.room.updated"
	TypeGuestUploadUpdated = "guest.upload.updated"
	TypeGuestProgress      = "guest.upload.progress"
	TypePeerTransfer       = "peer.transfer.updated"
	TypeHistoryUpdated     = "history.updated"
)

var (
	ErrHubClosed       = errors.New("events: hub closed")
	ErrTooManyStreams  = errors.New("events: too many active event streams")
	ErrPayloadTooLarge = errors.New("events: serialized event payload exceeds limit")
	ErrInvalidScope    = errors.New("events: invalid subscription scope")
)

// Event is the client-visible JSON event envelope. Routing identifiers are kept
// strictly outside this struct so session keys never leak to clients.
type Event struct {
	ID        uint64          `json:"id"`
	Type      string          `json:"type"`
	Timestamp int64           `json:"ts"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// DesktopEvent wraps an Event with internal desktop routing metadata for
// in-process desktop bridges (Wails runtime, native notifications).
type DesktopEvent struct {
	Event
	Scope      string
	SessionKey string
	RoomID     string
	GuestID    string
	RawPayload any
}

func isProgressType(eventType string) bool {
	switch eventType {
	case TypeBatchProgress, TypeUploadProgress, TypeDownloadProgress, TypeGuestProgress:
		return true
	default:
		return false
	}
}

// Subscription represents one active event stream consumer.
type Subscription struct {
	hub        *Hub
	id         uint64
	scope      string
	sessionKey string
	roomID     string
	guestID    string

	ch       chan Event
	resyncCh chan struct{}
	done     chan struct{}
	closeOne sync.Once
	closed   atomic.Bool
	reason   atomic.Value // string
}

// C returns the bounded channel of outgoing events.
func (s *Subscription) C() <-chan Event { return s.ch }

// Resync fires when the subscriber's queue overflowed and the client should
// reconcile authoritative state via REST.
func (s *Subscription) Resync() <-chan struct{} { return s.resyncCh }

// Done is closed when the subscription is terminated (revoked, expired, or hub closed).
func (s *Subscription) Done() <-chan struct{} { return s.done }

// CloseReason returns the optional termination reason (e.g. "session_revoked").
func (s *Subscription) CloseReason() string {
	v := s.reason.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// Close unsubscribes and closes the subscription safely and idempotently.
func (s *Subscription) Close() {
	s.closeWithReason("")
}

func (s *Subscription) closeWithReason(reason string) {
	s.closeOne.Do(func() {
		if reason != "" {
			s.reason.Store(reason)
		}
		s.closed.Store(true)
		close(s.done)
		if s.hub != nil {
			s.hub.removeSub(s)
		}
	})
}

func (s *Subscription) enqueue(ev Event, progress bool) {
	if s.closed.Load() {
		return
	}
	if progress && len(s.ch) >= ProgressBackpressure {
		return
	}
	select {
	case s.ch <- ev:
	default:
		if !progress {
			select {
			case s.resyncCh <- struct{}{}:
			default:
			}
		}
	}
}

// DesktopSubscription receives all desktop-visible events including RawPayload.
type DesktopSubscription struct {
	hub      *Hub
	id       uint64
	ch       chan DesktopEvent
	resyncCh chan struct{}
	done     chan struct{}
	closeOne sync.Once
	closed   atomic.Bool
}

func (s *DesktopSubscription) C() <-chan DesktopEvent  { return s.ch }
func (s *DesktopSubscription) Resync() <-chan struct{} { return s.resyncCh }
func (s *DesktopSubscription) Done() <-chan struct{}   { return s.done }
func (s *DesktopSubscription) Close() {
	s.closeOne.Do(func() {
		s.closed.Store(true)
		close(s.done)
		if s.hub != nil {
			s.hub.removeDesktopSub(s)
		}
	})
}

func (s *DesktopSubscription) enqueue(ev DesktopEvent, progress bool) {
	if s.closed.Load() {
		return
	}
	if progress && len(s.ch) >= ProgressBackpressure {
		return
	}
	select {
	case s.ch <- ev:
	default:
		if !progress {
			select {
			case s.resyncCh <- struct{}{}:
			default:
			}
		}
	}
}

// Hub coordinates bounded real-time event fan-out.
type Hub struct {
	seq atomic.Uint64

	mu           sync.RWMutex
	closed       bool
	personalSubs map[string]map[uint64]*Subscription
	totalPers    int
	guestSubs    map[string]map[uint64]*Subscription // key: roomID + "\x00" + guestID
	totalGuest   int
	desktopSubs  map[uint64]*DesktopSubscription

	throttleMu sync.Mutex
	lastProg   map[string]time.Time
	now        func() time.Time
}

// NewHub constructs an active event Hub.
func NewHub() *Hub {
	return &Hub{
		personalSubs: make(map[string]map[uint64]*Subscription),
		guestSubs:    make(map[string]map[uint64]*Subscription),
		desktopSubs:  make(map[uint64]*DesktopSubscription),
		lastProg:     make(map[string]time.Time),
		now:          time.Now,
	}
}

func guestSubKey(roomID, guestID string) string {
	return roomID + "\x00" + guestID
}

// SubscribePersonal registers an SSE subscriber scoped to a single personal sessionKey.
func (h *Hub) SubscribePersonal(sessionKey string) (*Subscription, error) {
	if sessionKey == "" {
		return nil, ErrInvalidScope
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrHubClosed
	}
	if h.totalPers >= MaxGlobalPersonalSubs || len(h.personalSubs[sessionKey]) >= MaxPerSessionSubs {
		return nil, ErrTooManyStreams
	}
	id := h.seq.Add(1)
	sub := &Subscription{
		hub:        h,
		id:         id,
		scope:      ScopePersonal,
		sessionKey: sessionKey,
		ch:         make(chan Event, DefaultQueueSize),
		resyncCh:   make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	if h.personalSubs[sessionKey] == nil {
		h.personalSubs[sessionKey] = make(map[uint64]*Subscription)
	}
	h.personalSubs[sessionKey][id] = sub
	h.totalPers++
	return sub, nil
}

// SubscribeGuest registers an SSE subscriber scoped to a single guest in a room.
func (h *Hub) SubscribeGuest(roomID, guestID string) (*Subscription, error) {
	if roomID == "" || guestID == "" {
		return nil, ErrInvalidScope
	}
	k := guestSubKey(roomID, guestID)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrHubClosed
	}
	if h.totalGuest >= MaxGlobalGuestSubs || len(h.guestSubs[k]) >= MaxPerGuestSubs {
		return nil, ErrTooManyStreams
	}
	id := h.seq.Add(1)
	sub := &Subscription{
		hub:      h,
		id:       id,
		scope:    ScopeGuest,
		roomID:   roomID,
		guestID:  guestID,
		ch:       make(chan Event, DefaultQueueSize),
		resyncCh: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	if h.guestSubs[k] == nil {
		h.guestSubs[k] = make(map[uint64]*Subscription)
	}
	h.guestSubs[k][id] = sub
	h.totalGuest++
	return sub, nil
}

// SubscribeDesktop registers an internal desktop observer (Wails UI + notifications).
func (h *Hub) SubscribeDesktop() (*DesktopSubscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrHubClosed
	}
	if len(h.desktopSubs) >= MaxDesktopSubs {
		return nil, ErrTooManyStreams
	}
	id := h.seq.Add(1)
	sub := &DesktopSubscription{
		hub:      h,
		id:       id,
		ch:       make(chan DesktopEvent, DefaultQueueSize),
		resyncCh: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	h.desktopSubs[id] = sub
	return sub, nil
}

func (h *Hub) removeSub(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch sub.scope {
	case ScopePersonal:
		if m := h.personalSubs[sub.sessionKey]; m != nil {
			if _, ok := m[sub.id]; ok {
				delete(m, sub.id)
				h.totalPers--
				if len(m) == 0 {
					delete(h.personalSubs, sub.sessionKey)
				}
			}
		}
	case ScopeGuest:
		k := guestSubKey(sub.roomID, sub.guestID)
		if m := h.guestSubs[k]; m != nil {
			if _, ok := m[sub.id]; ok {
				delete(m, sub.id)
				h.totalGuest--
				if len(m) == 0 {
					delete(h.guestSubs, k)
				}
			}
		}
	}
}

func (h *Hub) removeDesktopSub(sub *DesktopSubscription) {
	h.mu.Lock()
	delete(h.desktopSubs, sub.id)
	h.mu.Unlock()
}

// AllowProgress reports whether a progress update for key should be emitted now,
// enforcing minInterval throttling and bounding internal throttle map growth.
func (h *Hub) AllowProgress(key string, minInterval time.Duration) bool {
	if minInterval <= 0 {
		minInterval = DefaultProgressInterval
	}
	now := h.now()
	h.throttleMu.Lock()
	defer h.throttleMu.Unlock()
	if last, ok := h.lastProg[key]; ok && now.Sub(last) < minInterval {
		return false
	}
	if len(h.lastProg) >= maxThrottleEntries {
		cutoff := now.Add(-10 * time.Second)
		for k, ts := range h.lastProg {
			if ts.Before(cutoff) {
				delete(h.lastProg, k)
			}
		}
		if len(h.lastProg) >= maxThrottleEntries {
			h.lastProg = make(map[string]time.Time)
		}
	}
	h.lastProg[key] = now
	return true
}

func (h *Hub) buildEvent(eventType string, payload any) (Event, bool) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Event{}, false
		}
		if len(b) > MaxPayloadBytes {
			return Event{}, false
		}
		raw = b
	}
	return Event{
		ID:        h.seq.Add(1),
		Type:      eventType,
		Timestamp: h.now().UnixMilli(),
		Payload:   raw,
	}, true
}

// PublishPersonal emits an event to all personal SSE subscribers for sessionKey
// (or all personal subscribers if sessionKey == "") and to desktop subscribers.
func (h *Hub) PublishPersonal(sessionKey, eventType string, payload any) {
	ev, ok := h.buildEvent(eventType, payload)
	if !ok {
		return
	}
	progress := isProgressType(eventType)

	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return
	}
	var targets []*Subscription
	if sessionKey == "" {
		for _, m := range h.personalSubs {
			for _, sub := range m {
				targets = append(targets, sub)
			}
		}
	} else if m := h.personalSubs[sessionKey]; m != nil {
		targets = make([]*Subscription, 0, len(m))
		for _, sub := range m {
			targets = append(targets, sub)
		}
	}
	desktops := make([]*DesktopSubscription, 0, len(h.desktopSubs))
	for _, d := range h.desktopSubs {
		desktops = append(desktops, d)
	}
	h.mu.RUnlock()

	for _, sub := range targets {
		sub.enqueue(ev, progress)
	}
	if len(desktops) > 0 {
		dev := DesktopEvent{
			Event:      ev,
			Scope:      ScopePersonal,
			SessionKey: sessionKey,
			RawPayload: payload,
		}
		for _, d := range desktops {
			d.enqueue(dev, progress)
		}
	}
}

// PublishGuest emits an event to matching guest SSE subscribers (or all guests
// in roomID when guestID == "") and to desktop subscribers.
func (h *Hub) PublishGuest(roomID, guestID, eventType string, payload any) {
	ev, ok := h.buildEvent(eventType, payload)
	if !ok {
		return
	}
	progress := isProgressType(eventType)

	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return
	}
	var targets []*Subscription
	if guestID == "" {
		prefix := roomID + "\x00"
		for k, m := range h.guestSubs {
			if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
				for _, sub := range m {
					targets = append(targets, sub)
				}
			}
		}
	} else if m := h.guestSubs[guestSubKey(roomID, guestID)]; m != nil {
		targets = make([]*Subscription, 0, len(m))
		for _, sub := range m {
			targets = append(targets, sub)
		}
	}
	desktops := make([]*DesktopSubscription, 0, len(h.desktopSubs))
	for _, d := range h.desktopSubs {
		desktops = append(desktops, d)
	}
	h.mu.RUnlock()

	for _, sub := range targets {
		sub.enqueue(ev, progress)
	}
	if len(desktops) > 0 {
		dev := DesktopEvent{
			Event:      ev,
			Scope:      ScopeGuest,
			RoomID:     roomID,
			GuestID:    guestID,
			RawPayload: payload,
		}
		for _, d := range desktops {
			d.enqueue(dev, progress)
		}
	}
}

// PublishDesktop emits an event visible only to desktop subscribers (never mobile HTTP).
func (h *Hub) PublishDesktop(eventType string, payload any) {
	ev, ok := h.buildEvent(eventType, payload)
	if !ok {
		return
	}
	progress := isProgressType(eventType)

	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return
	}
	desktops := make([]*DesktopSubscription, 0, len(h.desktopSubs))
	for _, d := range h.desktopSubs {
		desktops = append(desktops, d)
	}
	h.mu.RUnlock()

	if len(desktops) > 0 {
		dev := DesktopEvent{
			Event:      ev,
			Scope:      ScopeDesktop,
			RawPayload: payload,
		}
		for _, d := range desktops {
			d.enqueue(dev, progress)
		}
	}
}

// CloseSession terminates every personal SSE subscription for sessionKey.
func (h *Hub) CloseSession(sessionKey, reason string) {
	if sessionKey == "" {
		return
	}
	h.mu.RLock()
	m := h.personalSubs[sessionKey]
	targets := make([]*Subscription, 0, len(m))
	for _, sub := range m {
		targets = append(targets, sub)
	}
	h.mu.RUnlock()

	for _, sub := range targets {
		sub.closeWithReason(reason)
	}
}

// CloseGuest terminates every guest SSE subscription for (roomID, guestID).
func (h *Hub) CloseGuest(roomID, guestID, reason string) {
	if roomID == "" || guestID == "" {
		return
	}
	h.mu.RLock()
	m := h.guestSubs[guestSubKey(roomID, guestID)]
	targets := make([]*Subscription, 0, len(m))
	for _, sub := range m {
		targets = append(targets, sub)
	}
	h.mu.RUnlock()

	for _, sub := range targets {
		sub.closeWithReason(reason)
	}
}

// CloseRoom terminates every guest SSE subscription in roomID.
func (h *Hub) CloseRoom(roomID, reason string) {
	if roomID == "" {
		return
	}
	prefix := roomID + "\x00"
	h.mu.RLock()
	var targets []*Subscription
	for k, m := range h.guestSubs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			for _, sub := range m {
				targets = append(targets, sub)
			}
		}
	}
	h.mu.RUnlock()

	for _, sub := range targets {
		sub.closeWithReason(reason)
	}
}

// Stats returns active subscriber counts for testing and diagnostics.
func (h *Hub) Stats() (personal, guest, desktop int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.totalPers, h.totalGuest, len(h.desktopSubs)
}

// Close terminates all active subscriptions and marks the hub closed.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	var subs []*Subscription
	for _, m := range h.personalSubs {
		for _, sub := range m {
			subs = append(subs, sub)
		}
	}
	for _, m := range h.guestSubs {
		for _, sub := range m {
			subs = append(subs, sub)
		}
	}
	var desktops []*DesktopSubscription
	for _, d := range h.desktopSubs {
		desktops = append(desktops, d)
	}
	h.mu.Unlock()

	for _, sub := range subs {
		sub.closeWithReason("server_shutdown")
	}
	for _, d := range desktops {
		d.Close()
	}
}
