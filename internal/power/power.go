package power

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

var (
	// ErrClosed indicates the power inhibitor manager has been closed.
	ErrClosed = errors.New("power: sleep inhibitor is closed")

	// ErrInhibitionFailed indicates that the native platform sleep inhibition API failed.
	ErrInhibitionFailed = errors.New("power: native sleep inhibition failed")

	// ErrUnsupportedPlatform indicates that system sleep inhibition is not supported.
	ErrUnsupportedPlatform = errors.New("power: sleep inhibition not supported on this platform")
)

// Lease represents an active sleep inhibition lease for an ongoing file transfer.
// Release must be called when the transfer finishes, fails, or cancels.
// Release is idempotent and safe for concurrent calls.
type Lease interface {
	Release()
}

// Status represents the observable state of system idle sleep inhibition.
type Status struct {
	Supported bool   `json:"supported"`
	Active    bool   `json:"active"`
	Holders   int    `json:"holders"`
	Reason    string `json:"reason,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Inhibitor manages system idle sleep inhibition during active transfers.
type Inhibitor interface {
	io.Closer
	Acquire(reason string) (Lease, error)
	Status() Status
}

// backend is the low-level platform interface for sleep inhibition.
type backend interface {
	Supported() bool
	Inhibit(reason string) error
	Uninhibit() error
	Close() error
}

// Manager is a thread-safe, reference-counted implementation of Inhibitor.
type Manager struct {
	mu           sync.Mutex
	backend      backend
	refCount     int
	closed       bool
	lastError    string
	activeReason string
	nextLeaseID  int64
	leases       map[int64]string
}

// New constructs a new platform-appropriate sleep inhibitor.
func New() Inhibitor {
	b, err := newNativeBackend()
	if err != nil {
		return NewWithBackend(&unsupportedBackend{err: err})
	}
	return NewWithBackend(b)
}

// NewWithBackend constructs a Manager with an explicit backend (useful for testing).
func NewWithBackend(b backend) *Manager {
	return &Manager{
		backend: b,
		leases:  make(map[int64]string),
	}
}

// Acquire requests a sleep inhibition lease for a transfer.
// If this is the first active lease, native sleep inhibition is activated.
// Returns an error if the manager is closed or if the native API call fails.
func (m *Manager) Acquire(reason string) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrClosed
	}

	if m.backend == nil || !m.backend.Supported() {
		m.lastError = "sleep inhibition not supported"
		return nil, ErrUnsupportedPlatform
	}

	if m.refCount == 0 {
		if err := m.backend.Inhibit(reason); err != nil {
			m.lastError = err.Error()
			return nil, fmt.Errorf("%w: %v", ErrInhibitionFailed, err)
		}
		m.lastError = ""
		m.activeReason = reason
	}

	m.refCount++
	m.nextLeaseID++
	id := m.nextLeaseID
	m.leases[id] = reason

	var once sync.Once
	return &leaseImpl{
		id:     id,
		reason: reason,
		release: func() {
			once.Do(func() {
				m.releaseLease(id)
			})
		},
	}, nil
}

func (m *Manager) releaseLease(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.leases[id]; !exists {
		return
	}
	delete(m.leases, id)
	m.refCount--

	if m.refCount <= 0 {
		m.refCount = 0
		m.activeReason = ""
		if !m.closed && m.backend != nil && m.backend.Supported() {
			if err := m.backend.Uninhibit(); err != nil {
				m.lastError = err.Error()
			}
		}
	}
}

// Status returns the current sleep inhibition state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	supported := m.backend != nil && m.backend.Supported()
	return Status{
		Supported: supported,
		Active:    supported && m.refCount > 0 && !m.closed,
		Holders:   m.refCount,
		Reason:    m.activeReason,
		Error:     m.lastError,
	}
}

// Close releases all active native power inhibitors and shuts down the manager.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	var err error
	if m.refCount > 0 && m.backend != nil && m.backend.Supported() {
		m.refCount = 0
		m.activeReason = ""
		err = m.backend.Uninhibit()
	}
	clear(m.leases)

	if m.backend != nil {
		if closeErr := m.backend.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

type leaseImpl struct {
	id      int64
	reason  string
	release func()
}

func (l *leaseImpl) Release() {
	if l.release != nil {
		l.release()
	}
}

type unsupportedBackend struct {
	err error
}

func (b *unsupportedBackend) Supported() bool {
	return false
}

func (b *unsupportedBackend) Inhibit(reason string) error {
	if b.err != nil {
		return b.err
	}
	return ErrUnsupportedPlatform
}

func (b *unsupportedBackend) Uninhibit() error {
	return nil
}

func (b *unsupportedBackend) Close() error {
	return nil
}
