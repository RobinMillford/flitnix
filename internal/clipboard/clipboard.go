package clipboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaxContentBytes caps synchronized clipboard text at 64 KiB.
	MaxContentBytes = 64 * 1024

	// defaultPollInterval governs how often the desktop checks the OS clipboard
	// when explicit opt-in clipboard synchronization is enabled and active.
	defaultPollInterval = 700 * time.Millisecond

	// echoWindow prevents a recently applied remote clipboard update from
	// bouncing back to peers when the local OS watcher sees it.
	echoWindow = 5 * time.Second
)

var (
	ErrDisabled         = errors.New("clipboard: automatic clipboard sync is disabled")
	ErrPaused           = errors.New("clipboard: automatic clipboard sync is paused")
	ErrPayloadTooLarge  = errors.New("clipboard: clipboard content exceeds 64 KiB limit")
	ErrInvalidUTF8      = errors.New("clipboard: clipboard content must be valid UTF-8 text")
	ErrEmptyContent     = errors.New("clipboard: clipboard text is empty")
	ErrConcealedContent = errors.New("clipboard: skipped sensitive/concealed clipboard item")
)

// Capabilities describes OS clipboard support in the current environment.
type Capabilities struct {
	Supported           bool   `json:"supported"`
	BackendName         string `json:"backendName"`
	ConcealedFilterSafe bool   `json:"concealedFilterSafe"`
	UnsupportedReason   string `json:"unsupportedReason,omitempty"`
}

// Snapshot represents one OS clipboard read result.
type Snapshot struct {
	Text      string
	Concealed bool
	Reason    string
}

// Backend abstracts OS clipboard read/write access and concealed-item checks.
type Backend interface {
	Capabilities() Capabilities
	Read(ctx context.Context) (Snapshot, error)
	WriteText(ctx context.Context, text string) error
}

// SyncItem holds an ephemeral, strictly non-persisted clipboard update.
type SyncItem struct {
	ID             string    `json:"id"`
	Hash           string    `json:"hash"`
	OriginDeviceID string    `json:"originDeviceId,omitempty"`
	OriginName     string    `json:"originName,omitempty"`
	Content        string    `json:"content"`
	SizeBytes      int       `json:"sizeBytes"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// StatusView summarizes clipboard sync runtime state without exposing raw text.
type StatusView struct {
	Enabled             bool      `json:"enabled"`
	Paused              bool      `json:"paused"`
	Watching            bool      `json:"watching"`
	Supported           bool      `json:"supported"`
	BackendName         string    `json:"backendName"`
	ConcealedFilterSafe bool      `json:"concealedFilterSafe"`
	UnsupportedReason   string    `json:"unsupportedReason,omitempty"`
	LastSyncAt          time.Time `json:"lastSyncAt,omitempty"`
	LastOrigin          string    `json:"lastOrigin,omitempty"`
	LastSizeBytes       int       `json:"lastSizeBytes,omitempty"`
	LastSkippedReason   string    `json:"lastSkippedReason,omitempty"`
	HasPendingItem      bool      `json:"hasPendingItem"`
}

// Config configures a clipboard synchronization coordinator.
type Config struct {
	Backend      Backend
	PollInterval time.Duration
	HostDeviceID string
	HostName     string
	Now          func() time.Time
	OnLocalSync  func(item SyncItem)
	OnRemoteSync func(item SyncItem)
	OnState      func(status StatusView)
}

// Coordinator manages opt-in clipboard watching, echo-loop suppression,
// size/UTF-8 validation, and transient in-memory distribution.
// Clipboard data is never persisted to disk and never written to logs.
type Coordinator struct {
	mu           sync.Mutex
	backend      Backend
	pollInterval time.Duration
	hostDeviceID string
	hostName     string
	now          func() time.Time
	onLocalSync  func(item SyncItem)
	onRemoteSync func(item SyncItem)
	onState      func(status StatusView)

	enabled          bool
	paused           bool
	hasActivePeer    bool
	watchCancel      context.CancelFunc
	watchDone        chan struct{}
	lastLocalHash    string
	suppressedHash   string
	suppressedUntil  time.Time
	latest           *SyncItem
	lastSyncAt       time.Time
	lastOrigin       string
	lastSizeBytes    int
	lastSkipped      string
}

// NewCoordinator initializes a clipboard coordinator. By default, opt-in sync
// is OFF (`enabled == false`) and no background watcher runs.
func NewCoordinator(cfg Config) *Coordinator {
	backend := cfg.Backend
	if backend == nil {
		backend = newPlatformBackend()
	}
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Coordinator{
		backend:      backend,
		pollInterval: interval,
		hostDeviceID: cfg.HostDeviceID,
		hostName:     cfg.HostName,
		now:          now,
		onLocalSync:  cfg.OnLocalSync,
		onRemoteSync: cfg.OnRemoteSync,
		onState:      cfg.OnState,
	}
}

// ValidateText checks UTF-8 validity and the 64 KiB ceiling without trimming content whitespace.
func ValidateText(text string) error {
	if text == "" {
		return ErrEmptyContent
	}
	if len(text) > MaxContentBytes {
		return ErrPayloadTooLarge
	}
	if !utf8.ValidString(text) {
		return ErrInvalidUTF8
	}
	return nil
}

// HashText computes the SHA-256 hex digest of text for deduplication and echo checks.
func HashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func randomItemID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "clip_" + hex.EncodeToString(b[:])
}

// SetEnabled turns opt-in clipboard sync on or off. Disabling immediately stops
// watching and wipes transient in-memory clipboard content.
func (c *Coordinator) SetEnabled(enabled bool) {
	var emit StatusView
	var cb func(StatusView)

	c.mu.Lock()
	if c.enabled == enabled {
		emit = c.statusLocked()
		cb = c.onState
		c.mu.Unlock()
		if cb != nil {
			cb(emit)
		}
		return
	}
	c.enabled = enabled
	if !enabled {
		c.latest = nil
		c.lastLocalHash = ""
		c.suppressedHash = ""
		c.suppressedUntil = time.Time{}
		c.lastSkipped = ""
	}
	c.reconcileWatcherLocked()
	emit = c.statusLocked()
	cb = c.onState
	c.mu.Unlock()

	if cb != nil {
		cb(emit)
	}
}

// SetPaused temporarily suspends or resumes clipboard synchronization.
func (c *Coordinator) SetPaused(paused bool) {
	c.mu.Lock()
	c.paused = paused
	if paused {
		c.latest = nil
	}
	c.reconcileWatcherLocked()
	emit := c.statusLocked()
	cb := c.onState
	c.mu.Unlock()

	if cb != nil {
		cb(emit)
	}
}

// SetActiveAuthorizedPeers sets whether at least one authenticated, trusted
// device session is active; watching only runs while true.
func (c *Coordinator) SetActiveAuthorizedPeers(active bool) {
	c.mu.Lock()
	c.hasActivePeer = active
	if !active {
		c.latest = nil
	}
	c.reconcileWatcherLocked()
	emit := c.statusLocked()
	cb := c.onState
	c.mu.Unlock()

	if cb != nil {
		cb(emit)
	}
}

// Close halts watching and clears any transient clipboard state.
func (c *Coordinator) Close() {
	c.mu.Lock()
	c.enabled = false
	c.hasActivePeer = false
	c.latest = nil
	c.lastLocalHash = ""
	c.suppressedHash = ""
	cancel := c.watchCancel
	done := c.watchDone
	c.watchCancel = nil
	c.watchDone = nil
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// Enabled reports whether opt-in clipboard sync is enabled.
func (c *Coordinator) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// Paused reports whether clipboard sync is currently paused.
func (c *Coordinator) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

// Status returns a snapshot of clipboard synchronization status.
func (c *Coordinator) Status() StatusView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

// Latest returns the most recent transient synced item if sync is enabled & unpaused.
func (c *Coordinator) Latest() (SyncItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled || c.paused || c.latest == nil {
		return SyncItem{}, false
	}
	return *c.latest, true
}

func (c *Coordinator) statusLocked() StatusView {
	caps := c.backend.Capabilities()
	return StatusView{
		Enabled:             c.enabled,
		Paused:              c.paused,
		Watching:            c.watchCancel != nil,
		Supported:           caps.Supported,
		BackendName:         caps.BackendName,
		ConcealedFilterSafe: caps.ConcealedFilterSafe,
		UnsupportedReason:   caps.UnsupportedReason,
		LastSyncAt:          c.lastSyncAt,
		LastOrigin:          c.lastOrigin,
		LastSizeBytes:       c.lastSizeBytes,
		LastSkippedReason:   c.lastSkipped,
		HasPendingItem:      c.latest != nil,
	}
}

func (c *Coordinator) shouldWatchLocked() bool {
	return c.enabled && !c.paused && c.hasActivePeer && c.backend.Capabilities().Supported
}

func (c *Coordinator) reconcileWatcherLocked() {
	want := c.shouldWatchLocked()
	running := c.watchCancel != nil
	if want && !running {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		c.watchCancel = cancel
		c.watchDone = done
		// Snapshot initial local clipboard hash so enabling sync does not
		// unexpectedly push stale clipboard contents created before opt-in.
		if snap, err := c.backend.Read(ctx); err == nil && !snap.Concealed && ValidateText(snap.Text) == nil {
			c.lastLocalHash = HashText(snap.Text)
		}
		go c.watchLoop(ctx, done)
		return
	}
	if !want && running {
		cancel := c.watchCancel
		c.watchCancel = nil
		c.watchDone = nil
		cancel()
	}
}

func (c *Coordinator) watchLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = c.PollOnce(ctx)
		}
	}
}

// PollOnce inspects the OS clipboard once and publishes an update when appropriate.
// Exposed for deterministic testing as well as background polling.
func (c *Coordinator) PollOnce(ctx context.Context) (bool, error) {
	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		return false, ErrDisabled
	}
	if c.paused {
		c.mu.Unlock()
		return false, ErrPaused
	}
	c.mu.Unlock()

	snap, err := c.backend.Read(ctx)
	if err != nil {
		return false, err
	}
	if snap.Concealed {
		c.recordSkip("concealed item skipped")
		return false, ErrConcealedContent
	}
	if snap.Text == "" {
		return false, nil
	}
	if len(snap.Text) > MaxContentBytes {
		c.recordSkip("clipboard item exceeds 64 KiB limit")
		return false, ErrPayloadTooLarge
	}
	if !utf8.ValidString(snap.Text) {
		c.recordSkip("clipboard item is not valid UTF-8 text")
		return false, ErrInvalidUTF8
	}

	hash := HashText(snap.Text)
	now := c.now()

	c.mu.Lock()
	if !c.enabled || c.paused {
		c.mu.Unlock()
		return false, nil
	}
	if hash == c.lastLocalHash {
		c.mu.Unlock()
		return false, nil
	}
	if hash == c.suppressedHash && now.Before(c.suppressedUntil) {
		c.lastLocalHash = hash
		c.mu.Unlock()
		return false, nil
	}
	c.lastLocalHash = hash
	c.lastSkipped = ""
	item := SyncItem{
		ID:             randomItemID(),
		Hash:           hash,
		OriginDeviceID: c.hostDeviceID,
		OriginName:     c.hostName,
		Content:        snap.Text,
		SizeBytes:      len(snap.Text),
		UpdatedAt:      now,
	}
	itemCopy := item
	c.latest = &itemCopy
	c.lastSyncAt = now
	c.lastOrigin = c.hostName
	c.lastSizeBytes = item.SizeBytes
	onLocal := c.onLocalSync
	onState := c.onState
	status := c.statusLocked()
	c.mu.Unlock()

	if onLocal != nil {
		onLocal(item)
	}
	if onState != nil {
		onState(status)
	}
	return true, nil
}

func (c *Coordinator) recordSkip(reason string) {
	c.mu.Lock()
	changed := c.lastSkipped != reason
	c.lastSkipped = reason
	cb := c.onState
	status := c.statusLocked()
	c.mu.Unlock()
	if changed && cb != nil {
		cb(status)
	}
}

// ReceiveRemote accepts a clipboard update pushed by an authenticated trusted peer,
// suppresses echo loops, updates the local OS clipboard when supported, and
// returns the accepted SyncItem (or deduplicates cleanly).
func (c *Coordinator) ReceiveRemote(ctx context.Context, originDeviceID, originName, content string) (SyncItem, bool, error) {
	if err := ValidateText(content); err != nil {
		return SyncItem{}, false, err
	}

	hash := HashText(content)
	now := c.now()

	c.mu.Lock()
	if !c.enabled {
		c.mu.Unlock()
		return SyncItem{}, false, ErrDisabled
	}
	if c.paused {
		c.mu.Unlock()
		return SyncItem{}, false, ErrPaused
	}
	// Deduplicate if already the active local/suppressed hash within echo window.
	if hash == c.lastLocalHash || (hash == c.suppressedHash && now.Before(c.suppressedUntil)) {
		c.mu.Unlock()
		return SyncItem{}, false, nil
	}
	c.suppressedHash = hash
	c.suppressedUntil = now.Add(echoWindow)
	c.lastLocalHash = hash
	c.lastSkipped = ""

	if originName == "" {
		originName = "Trusted Peer"
	}
	item := SyncItem{
		ID:             randomItemID(),
		Hash:           hash,
		OriginDeviceID: originDeviceID,
		OriginName:     originName,
		Content:        content,
		SizeBytes:      len(content),
		UpdatedAt:      now,
	}
	itemCopy := item
	c.latest = &itemCopy
	c.lastSyncAt = now
	c.lastOrigin = originName
	c.lastSizeBytes = item.SizeBytes
	caps := c.backend.Capabilities()
	onRemote := c.onRemoteSync
	onState := c.onState
	status := c.statusLocked()
	c.mu.Unlock()

	if caps.Supported {
		_ = c.backend.WriteText(ctx, content)
	}
	if onRemote != nil {
		onRemote(item)
	}
	if onState != nil {
		onState(status)
	}
	return item, true, nil
}
