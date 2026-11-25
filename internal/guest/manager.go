package guest

import (
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

type Manager struct {
	mu         sync.RWMutex
	activeRoom *Room
	quota      *transfer.QuotaManager
	stopTicker chan struct{}
	closeOnce  sync.Once
	done       chan struct{}
}

func NewManager() *Manager {
	m := &Manager{
		stopTicker: make(chan struct{}),
		done:       make(chan struct{}),
	}
	go m.expiryLoop()
	return m
}

// SetQuotaManager attaches a shared storage quota manager used by future
// created guest rooms when RoomConfig.Quota is nil.
func (m *Manager) SetQuotaManager(q *transfer.QuotaManager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quota = q
}

func (m *Manager) expiryLoop() {
	defer close(m.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopTicker:
			return
		case <-ticker.C:
			m.mu.RLock()
			r := m.activeRoom
			m.mu.RUnlock()
			if r != nil {
				r.CheckExpiry()
			}
		}
	}
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		close(m.stopTicker)
	})
	<-m.done

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.activeRoom != nil {
		m.activeRoom.End()
		m.activeRoom = nil
	}
}

func (m *Manager) CreateRoom(destRoot string, duration time.Duration) (*Room, error) {
	return m.CreateRoomWithConfig(RoomConfig{
		DestinationRoot: destRoot,
		Duration:        duration,
	})
}

func (m *Manager) CreateRoomWithConfig(cfg RoomConfig) (*Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.activeRoom != nil {
		m.activeRoom.End()
	}

	if cfg.Quota == nil {
		cfg.Quota = m.quota
	}
	r, err := NewRoomWithConfig(cfg)
	if err != nil {
		return nil, err
	}
	m.activeRoom = r
	return r, nil
}

func (m *Manager) ActiveRoom() *Room {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.activeRoom != nil {
		m.activeRoom.CheckExpiry()
	}
	return m.activeRoom
}

func (m *Manager) EndRoom() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.activeRoom == nil {
		return ErrRoomNotFound
	}
	m.activeRoom.End()
	return nil
}

func (m *Manager) ValidateGuest(token string) (*Room, *Guest, error) {
	m.mu.RLock()
	r := m.activeRoom
	m.mu.RUnlock()

	if r == nil {
		return nil, nil, ErrRoomNotFound
	}
	r.CheckExpiry()
	g, err := r.ValidateGuestToken(token)
	if err != nil {
		return r, nil, err
	}
	return r, g, nil
}
