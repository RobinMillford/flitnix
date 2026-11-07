package netenv

import (
	"context"
	"sync"
	"time"
)

// MonitorOption configures a Monitor instance.
type MonitorOption func(*Monitor)

// WithPollInterval sets the fallback periodic reconciliation interval (defaults to 2 seconds).
func WithPollInterval(d time.Duration) MonitorOption {
	return func(m *Monitor) {
		if d > 0 {
			m.pollInterval = d
		}
	}
}

// WithDebounceDelay sets the debounce window for coalescing rapid OS notifications (defaults to 150ms).
func WithDebounceDelay(d time.Duration) MonitorOption {
	return func(m *Monitor) {
		if d > 0 {
			m.debounceDelay = d
		}
	}
}

// WithPreferredInterface injects a getter for the currently preferred network interface.
func WithPreferredInterface(fn func() string) MonitorOption {
	return func(m *Monitor) {
		m.preferredInterface = fn
	}
}

// WithServerAddress injects a getter for the active server listening address.
func WithServerAddress(fn func() string) MonitorOption {
	return func(m *Monitor) {
		m.serverAddress = fn
	}
}

// WithNativeNotifier overrides the OS native notifier loop, primarily for deterministic testing.
func WithNativeNotifier(fn func(ctx context.Context, notify chan<- struct{})) MonitorOption {
	return func(m *Monitor) {
		m.nativeNotifier = fn
	}
}

// Monitor observes system network state using native OS notifications combined with
// a periodic reconciliation fallback.
type Monitor struct {
	provider           Provider
	pollInterval       time.Duration
	debounceDelay      time.Duration
	preferredInterface func() string
	serverAddress      func() string
	nativeNotifier     func(ctx context.Context, notify chan<- struct{})

	mu        sync.RWMutex
	state     NetworkState
	listeners []func(NetworkState)

	cancel context.CancelFunc
	done   chan struct{}
}

// NewMonitor creates a network monitor using the given provider.
func NewMonitor(provider Provider, opts ...MonitorOption) *Monitor {
	if provider == nil {
		provider = DefaultSource()
	}

	m := &Monitor{
		provider:       provider,
		pollInterval:   2 * time.Second,
		debounceDelay:  150 * time.Millisecond,
		nativeNotifier: defaultNativeNotifier,
		state: NetworkState{
			Status: StatusScanning,
		},
	}

	for _, opt := range opts {
		opt(m)
	}

	return m
}

// Start begins monitoring. It immediately evaluates the initial state,
// launches OS native notification listening, and schedules periodic reconciliation.
func (m *Monitor) Start(ctx context.Context) {
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		return
	}

	monitorCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	m.mu.Unlock()

	// Initial evaluation
	_, _ = m.evaluateOnce()

	notifyCh := make(chan struct{}, 16)

	// Launch native OS notifier
	if m.nativeNotifier != nil {
		go m.nativeNotifier(monitorCtx, notifyCh)
	}

	// Launch reconciliation and debounce coordinator
	go m.loop(monitorCtx, notifyCh)
}

// Stop cleanly terminates the monitor and waits for background goroutines to finish.
func (m *Monitor) Stop() {
	m.mu.Lock()
	if m.cancel == nil {
		m.mu.Unlock()
		return
	}
	cancel := m.cancel
	done := m.done
	m.cancel = nil
	m.mu.Unlock()

	cancel()
	<-done
}

// State returns the current NetworkState snapshot.
func (m *Monitor) State() NetworkState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// OnChange registers a callback that is invoked whenever a meaningful change in NetworkState occurs.
func (m *Monitor) OnChange(fn func(NetworkState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listeners = append(m.listeners, fn)
}

// Refresh performs an immediate, synchronous network evaluation, updates cached state,
// notifies listeners if state changed, and returns the newly detected state.
func (m *Monitor) Refresh() (NetworkState, error) {
	InvalidateMetadataCache()
	return m.evaluateOnce()
}

func (m *Monitor) evaluateOnce() (NetworkState, error) {
	views, err := Enumerate(m.provider)

	pref := ""
	if m.preferredInterface != nil {
		pref = m.preferredInterface()
	}

	srvAddr := ""
	if m.serverAddress != nil {
		srvAddr = m.serverAddress()
	}

	newState := EvaluateState(views, pref, srvAddr, err)

	m.mu.Lock()
	changed := !newState.Equal(m.state)
	m.state = newState
	var callbacks []func(NetworkState)
	if changed {
		callbacks = append([]func(NetworkState){}, m.listeners...)
	}
	m.mu.Unlock()

	for _, cb := range callbacks {
		cb(newState)
	}

	return newState, err
}

func (m *Monitor) loop(ctx context.Context, notifyCh <-chan struct{}) {
	defer close(m.done)

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	var debounceTimer *time.Timer
	var debounceCh <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return

		case <-notifyCh:
			// Coalesce / debounce rapid OS notifications
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.NewTimer(m.debounceDelay)
			debounceCh = debounceTimer.C

		case <-debounceCh:
			debounceTimer = nil
			debounceCh = nil
			InvalidateMetadataCache()
			_, _ = m.evaluateOnce()

		case <-ticker.C:
			_, _ = m.evaluateOnce()
		}
	}
}
