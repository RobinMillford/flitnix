package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type EventType string

const (
	EventRoomCreated     EventType = "room_created"
	EventRoomEnded       EventType = "room_ended"
	EventRoomExpired     EventType = "room_expired"
	EventGuestJoined     EventType = "guest_joined"
	EventGuestApproved   EventType = "guest_approved"
	EventGuestRejected   EventType = "guest_rejected"
	EventGuestRevoked    EventType = "guest_revoked"
	EventUploadDeclared  EventType = "upload_declared"
	EventUploadApproved  EventType = "upload_approved"
	EventUploadRejected  EventType = "upload_rejected"
	EventUploadCompleted EventType = "upload_completed"
	EventUploadFailed    EventType = "upload_failed"
	EventUploadCanceled  EventType = "upload_canceled"
	EventExportCompleted EventType = "export_completed"
	EventExportFailed    EventType = "export_failed"
)

type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomeFailed   Outcome = "failed"
	OutcomeRejected Outcome = "rejected"
	OutcomeCanceled Outcome = "canceled"
	OutcomeInfo     Outcome = "info"
)

// Entry represents one immutable audit log record.
// Sensitive secrets (tokens, session cookies, invite keys, private content)
// must NEVER be stored in an Entry.
type Entry struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	EventType EventType `json:"eventType"`
	RoomID    string    `json:"roomId,omitempty"`
	GuestID   string    `json:"guestId,omitempty"`
	GuestName string    `json:"guestName,omitempty"`
	FileName  string    `json:"fileName,omitempty"`
	FileSize  int64     `json:"fileSize,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	Outcome   Outcome   `json:"outcome"`
	Details   string    `json:"details,omitempty"`
}

type Filter struct {
	RoomID    string    `json:"roomId,omitempty"`
	GuestID   string    `json:"guestId,omitempty"`
	EventType EventType `json:"eventType,omitempty"`
	Outcome   Outcome   `json:"outcome,omitempty"`
	Since     time.Time `json:"since,omitempty"`
	Until     time.Time `json:"until,omitempty"`
	Limit     int       `json:"limit,omitempty"`
	Offset    int       `json:"offset,omitempty"`
}

type Store struct {
	mu         sync.RWMutex
	filePath   string
	entries    []Entry
	maxEntries int
	logChan    chan Entry
	closed     chan struct{}
	closeOnce  sync.Once
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = io.ReadFull(rand.Reader, b)
	return hex.EncodeToString(b)
}

// sanitizeString removes control characters and caps string length.
func sanitizeString(s string, maxLen int) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 32 && r != 127 {
			b.WriteRune(r)
		}
	}
	res := strings.TrimSpace(b.String())
	runes := []rune(res)
	if len(runes) > maxLen {
		return string(runes[:maxLen])
	}
	return res
}

func NewStore(filePath string, maxEntries int) (*Store, error) {
	if maxEntries <= 0 {
		maxEntries = 10000
	}

	s := &Store{
		filePath:   filePath,
		maxEntries: maxEntries,
		entries:    make([]Entry, 0, 100),
		logChan:    make(chan Entry, 256),
		closed:     make(chan struct{}),
	}

	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0700); err != nil {
			return nil, fmt.Errorf("audit: cannot create audit directory: %w", err)
		}
		if err := s.loadExisting(); err != nil {
			// Non-fatal if corrupted; start fresh
			_ = err
		}
	}

	go s.worker()
	return s, nil
}

func (s *Store) loadExisting() error {
	f, err := os.Open(s.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	for dec.More() {
		var e Entry
		if err := dec.Decode(&e); err == nil {
			s.entries = append(s.entries, e)
		}
	}

	if len(s.entries) > s.maxEntries {
		s.entries = s.entries[len(s.entries)-s.maxEntries:]
	}
	return nil
}

func (s *Store) worker() {
	var file *os.File
	if s.filePath != "" {
		f, err := os.OpenFile(s.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			file = f
			defer file.Close()
		}
	}

	for {
		select {
		case <-s.closed:
			// Flush any remaining items in logChan
			for {
				select {
				case e := <-s.logChan:
					if file != nil {
						data, _ := json.Marshal(e)
						_, _ = file.Write(append(data, '\n'))
					}
				default:
					return
				}
			}
		case e := <-s.logChan:
			if file != nil {
				data, _ := json.Marshal(e)
				_, _ = file.Write(append(data, '\n'))
			}
		}
	}
}

func (s *Store) Record(e Entry) {
	if e.ID == "" {
		e.ID = randomID()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	e.GuestName = sanitizeString(e.GuestName, 40)
	e.FileName = sanitizeString(e.FileName, 120)
	e.Details = sanitizeString(e.Details, 200)

	s.mu.Lock()
	s.entries = append(s.entries, e)
	if len(s.entries) > s.maxEntries {
		s.entries = s.entries[len(s.entries)-s.maxEntries:]
	}
	s.mu.Unlock()

	select {
	case s.logChan <- e:
	default:
		// Do not block file transfer if audit channel fills up
	}
}

func (s *Store) Query(f Filter) ([]Entry, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matched []Entry
	for _, e := range s.entries {
		if f.RoomID != "" && e.RoomID != f.RoomID {
			continue
		}
		if f.GuestID != "" && e.GuestID != f.GuestID {
			continue
		}
		if f.EventType != "" && e.EventType != f.EventType {
			continue
		}
		if f.Outcome != "" && e.Outcome != f.Outcome {
			continue
		}
		if !f.Since.IsZero() && e.Timestamp.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && e.Timestamp.After(f.Until) {
			continue
		}
		matched = append(matched, e)
	}

	// Sort newest first
	sort.Slice(matched, func(i, j int) bool {
		return matched[i].Timestamp.After(matched[j].Timestamp)
	})

	total := len(matched)
	if f.Offset < 0 {
		f.Offset = 0
	}
	if f.Offset >= total {
		return []Entry{}, total
	}

	end := total
	if f.Limit > 0 && f.Offset+f.Limit < end {
		end = f.Offset + f.Limit
	}

	return matched[f.Offset:end], total
}

func (s *Store) Clear() error {
	s.mu.Lock()
	s.entries = make([]Entry, 0, 100)
	s.mu.Unlock()

	if s.filePath != "" {
		err := os.Truncate(s.filePath, 0)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
	})
}
