// Package history provides durable, thread-safe personal transfer history
// persisted cleanly as private (0600) JSON Lines separate from Guest Drop Box audit logs.
package history

import (
	"bufio"
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
	"unicode"
)

const (
	DirectionIncoming     = "incoming"
	DirectionOutgoing     = "outgoing"
	DirectionBatch        = "batch"
	DirectionPeerOutgoing = "peer_outgoing"

	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
	StatusRejected  = "rejected"
	StatusDiscarded = "discarded"

	defaultQueryLimit = 50
	maxQueryLimit     = 500
)

// Record represents a single completed, failed, or canceled personal transfer
// (never containing tokens, session keys, or raw secrets).
type Record struct {
	ID          string    `json:"id"`
	DedupKey    string    `json:"dedupKey,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	Direction   string    `json:"direction"`
	Kind        string    `json:"kind,omitempty"`
	DisplayName string    `json:"displayName"`
	ItemCount   int       `json:"itemCount,omitempty"`
	Size        int64     `json:"size"`
	Transferred int64     `json:"transferred"`
	Status      string    `json:"status"`
	DeviceID    string    `json:"deviceId,omitempty"`
	DeviceName  string    `json:"deviceName,omitempty"`
	SHA256      string    `json:"sha256,omitempty"`
	Error       string    `json:"error,omitempty"`
	BatchID     string    `json:"batchId,omitempty"`
	TransferID  string    `json:"transferId,omitempty"`
	LocalPath   string    `json:"localPath,omitempty"`
}

// Filter specifies search, facet, date, and pagination parameters for Query.
type Filter struct {
	Query     string
	Direction string
	Status    string
	Device    string
	Since     time.Time
	Until     time.Time
	Limit     int
	Offset    int
}

// Stats summarizes persisted transfer history across all stored records.
type Stats struct {
	TotalRecords          int   `json:"totalRecords"`
	CompletedCount        int   `json:"completedCount"`
	FailedCount           int   `json:"failedCount"`
	CanceledCount         int   `json:"canceledCount"`
	TotalBytesTransferred int64 `json:"totalBytesTransferred"`
	IncomingBytes         int64 `json:"incomingBytes"`
	OutgoingBytes         int64 `json:"outgoingBytes"`
}

// QueryResult bundles matching page records, filtered total count, summary
// statistics, and backing file size.
type QueryResult struct {
	Records        []Record `json:"records"`
	Total          int      `json:"total"`
	Stats          Stats    `json:"stats"`
	StoreSizeBytes int64    `json:"storeSizeBytes"`
}

type writeOp struct {
	kind   string // "append", "rewrite", "flush"
	record Record
	done   chan error
}

// Store manages thread-safe JSONL persistence, deduplication, querying, and retention pruning.
type Store struct {
	mu         sync.RWMutex
	filePath   string
	records    []Record
	dedupIndex map[string]int // dedupKey -> index in records
	ops        chan writeOp
	closed     bool
	closeOnce  sync.Once
	workerDone chan struct{}
}

// NewStore opens or initializes a persistent transfer history store at filePath.
// Pass "" for an in-memory-only store (useful in tests).
func NewStore(filePath string) (*Store, error) {
	s := &Store{
		filePath:   filePath,
		records:    make([]Record, 0, 64),
		dedupIndex: make(map[string]int),
		ops:        make(chan writeOp, 256),
		workerDone: make(chan struct{}),
	}

	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
			return nil, fmt.Errorf("history: create dir: %w", err)
		}
		s.loadExisting()
	}

	go s.worker()
	return s, nil
}

func randomID() string {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("hist-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func sanitizeText(s string, maxLen int) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 0x20 && r != 0x7f) && !((r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)) {
			b.WriteRune(r)
		} else if unicode.IsSpace(r) && r != '\n' && r != '\r' {
			b.WriteByte(' ')
		}
	}
	res := strings.TrimSpace(b.String())
	runes := []rune(res)
	if len(runes) > maxLen {
		return string(runes[:maxLen-1]) + "…"
	}
	return res
}

func normalizeRecord(r Record) Record {
	if r.ID == "" {
		r.ID = randomID()
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	} else {
		r.Timestamp = r.Timestamp.UTC()
	}
	r.DisplayName = sanitizeText(r.DisplayName, 200)
	if r.DisplayName == "" {
		r.DisplayName = "Unnamed transfer"
	}
	r.DeviceName = sanitizeText(r.DeviceName, 80)
	r.DeviceID = sanitizeText(r.DeviceID, 64)
	r.Error = sanitizeText(r.Error, 200)
	r.SHA256 = strings.ToLower(strings.TrimSpace(r.SHA256))
	if r.ItemCount <= 0 {
		r.ItemCount = 1
	}
	if r.Size < 0 {
		r.Size = 0
	}
	if r.Transferred < 0 {
		r.Transferred = 0
	}
	return r
}

func (s *Store) loadExisting() {
	f, err := os.Open(s.filePath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // skip corrupted trailing/partial lines safely
		}
		rec = normalizeRecord(rec)
		s.upsertLocked(rec)
	}
	s.sortDescAndReindexLocked()
}

func (s *Store) upsertLocked(rec Record) bool {
	if rec.DedupKey != "" {
		if idx, exists := s.dedupIndex[rec.DedupKey]; exists && idx >= 0 && idx < len(s.records) {
			existing := s.records[idx]
			if rec.LocalPath == "" {
				rec.LocalPath = existing.LocalPath
			}
			if existing.Status == rec.Status && existing.Transferred >= rec.Transferred && existing.LocalPath == rec.LocalPath {
				return false
			}
			rec.ID = existing.ID
			s.records[idx] = rec
			return true
		}
	}
	s.records = append(s.records, rec)
	if rec.DedupKey != "" {
		s.dedupIndex[rec.DedupKey] = len(s.records) - 1
	}
	return true
}

func (s *Store) sortDescAndReindexLocked() {
	sort.SliceStable(s.records, func(i, j int) bool {
		if !s.records[i].Timestamp.Equal(s.records[j].Timestamp) {
			return s.records[i].Timestamp.After(s.records[j].Timestamp)
		}
		return s.records[i].ID > s.records[j].ID
	})
	s.dedupIndex = make(map[string]int, len(s.records))
	for i, rec := range s.records {
		if rec.DedupKey != "" {
			s.dedupIndex[rec.DedupKey] = i
		}
	}
}

func (s *Store) worker() {
	defer close(s.workerDone)
	for op := range s.ops {
		var err error
		switch op.kind {
		case "append":
			err = s.appendFile(op.record)
		case "rewrite":
			err = s.rewriteFileSnapshot()
		case "flush":
			err = nil
		}
		if op.done != nil {
			op.done <- err
		}
	}
}

func (s *Store) appendFile(rec Record) error {
	if s.filePath == "" {
		return nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func (s *Store) rewriteFileSnapshot() error {
	if s.filePath == "" {
		return nil
	}
	s.mu.RLock()
	// Persist oldest-to-newest on disk so sequential appends naturally preserve chronological order.
	snapshot := make([]Record, len(s.records))
	copy(snapshot, s.records)
	s.mu.RUnlock()

	sort.SliceStable(snapshot, func(i, j int) bool {
		return snapshot[i].Timestamp.Before(snapshot[j].Timestamp)
	})

	tmpPath := fmt.Sprintf("%s.tmp.%d", s.filePath, os.Getpid())
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 32*1024)
	for _, rec := range snapshot {
		b, mErr := json.Marshal(rec)
		if mErr != nil {
			continue
		}
		if _, wErr := bw.Write(b); wErr != nil {
			f.Close()
			_ = os.Remove(tmpPath)
			return wErr
		}
		if err := bw.WriteByte('\n'); err != nil {
			f.Close()
			_ = os.Remove(tmpPath)
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, s.filePath)
}

// Record stores a transfer outcome in memory immediately and persists asynchronously.
// Returns the stored record (with generated ID) and true if new/updated.
func (s *Store) Record(r Record) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	norm := normalizeRecord(r)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Record{}, false
	}
	_, existed := s.dedupIndex[norm.DedupKey]
	changed := s.upsertLocked(norm)
	if changed {
		s.sortDescAndReindexLocked()
	}
	s.mu.Unlock()

	if !changed {
		return norm, false
	}

	opKind := "append"
	if existed {
		// Rewriting keeps JSONL compact when a record updates in place.
		opKind = "rewrite"
	}
	select {
	case s.ops <- writeOp{kind: opKind, record: norm}:
	default:
	}
	return norm, true
}

// Get retrieves a stored record by ID.
func (s *Store) Get(id string) (Record, bool) {
	if s == nil || id == "" {
		return Record{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.records {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// Query filters, paginates, and summarizes persisted transfer history.
func (s *Store) Query(f Filter) QueryResult {
	if s == nil {
		return QueryResult{Records: []Record{}}
	}

	limit := f.Limit
	if limit <= 0 {
		limit = defaultQueryLimit
	} else if limit > maxQueryLimit {
		limit = maxQueryLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	q := strings.ToLower(strings.TrimSpace(f.Query))
	dirFilter := strings.ToLower(strings.TrimSpace(f.Direction))
	statusFilter := strings.ToLower(strings.TrimSpace(f.Status))
	devFilter := strings.ToLower(strings.TrimSpace(f.Device))

	s.mu.RLock()
	stats := Stats{TotalRecords: len(s.records)}
	matched := make([]Record, 0, min(len(s.records), limit))
	totalMatched := 0

	for _, rec := range s.records {
		switch rec.Status {
		case StatusCompleted:
			stats.CompletedCount++
		case StatusFailed:
			stats.FailedCount++
		case StatusCanceled, StatusRejected, StatusDiscarded:
			stats.CanceledCount++
		}
		if rec.Status == StatusCompleted {
			bytes := rec.Transferred
			if bytes <= 0 {
				bytes = rec.Size
			}
			stats.TotalBytesTransferred += bytes
			if rec.Direction == DirectionIncoming || rec.Direction == DirectionBatch {
				stats.IncomingBytes += bytes
			} else {
				stats.OutgoingBytes += bytes
			}
		}

		if dirFilter != "" && dirFilter != "all" && strings.ToLower(rec.Direction) != dirFilter {
			continue
		}
		if statusFilter != "" && statusFilter != "all" {
			st := strings.ToLower(rec.Status)
			if statusFilter == "canceled" {
				if st != StatusCanceled && st != StatusRejected && st != StatusDiscarded {
					continue
				}
			} else if st != statusFilter {
				continue
			}
		}
		if !f.Since.IsZero() && rec.Timestamp.Before(f.Since) {
			continue
		}
		if !f.Until.IsZero() && rec.Timestamp.After(f.Until) {
			continue
		}
		if devFilter != "" && devFilter != "all" {
			if !strings.Contains(strings.ToLower(rec.DeviceName), devFilter) &&
				!strings.Contains(strings.ToLower(rec.DeviceID), devFilter) {
				continue
			}
		}
		if q != "" {
			if !strings.Contains(strings.ToLower(rec.DisplayName), q) &&
				!strings.Contains(strings.ToLower(rec.DeviceName), q) &&
				!strings.Contains(strings.ToLower(rec.SHA256), q) {
				continue
			}
		}

		if totalMatched >= offset && len(matched) < limit {
			matched = append(matched, rec)
		}
		totalMatched++
	}
	filePath := s.filePath
	s.mu.RUnlock()

	var storeSize int64
	if filePath != "" {
		if st, err := os.Stat(filePath); err == nil {
			storeSize = st.Size()
		}
	}

	return QueryResult{
		Records:        matched,
		Total:          totalMatched,
		Stats:          stats,
		StoreSizeBytes: storeSize,
	}
}

// Delete removes one history record by ID without deleting any user files on disk.
func (s *Store) Delete(id string) error {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("history: store closed")
	}
	kept := make([]Record, 0, len(s.records))
	found := false
	for _, r := range s.records {
		if r.ID == id {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		s.mu.Unlock()
		return nil
	}
	s.records = kept
	s.sortDescAndReindexLocked()
	s.mu.Unlock()

	done := make(chan error, 1)
	s.ops <- writeOp{kind: "rewrite", done: done}
	return <-done
}

// Clear removes all persisted history metadata without deleting any files on disk.
func (s *Store) Clear() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("history: store closed")
	}
	s.records = make([]Record, 0)
	s.dedupIndex = make(map[string]int)
	s.mu.Unlock()

	done := make(chan error, 1)
	s.ops <- writeOp{kind: "rewrite", done: done}
	return <-done
}

// ApplyRetention enforces age-based (maxAge > 0) and count-based (maxRecords > 0)
// pruning on history metadata without touching any transferred user files.
func (s *Store) ApplyRetention(maxAge time.Duration, maxRecords int) (int, error) {
	if s == nil {
		return 0, nil
	}
	if maxAge <= 0 && maxRecords <= 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	var cutoff time.Time
	if maxAge > 0 {
		cutoff = now.Add(-maxAge)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, errors.New("history: store closed")
	}
	before := len(s.records)
	kept := make([]Record, 0, before)
	for _, r := range s.records {
		if !cutoff.IsZero() && r.Timestamp.Before(cutoff) {
			continue
		}
		kept = append(kept, r)
	}
	// s.records is kept sorted newest-first, so preserving leading elements keeps newest.
	if maxRecords > 0 && len(kept) > maxRecords {
		kept = kept[:maxRecords]
	}
	removed := before - len(kept)
	if removed == 0 {
		s.mu.Unlock()
		return 0, nil
	}
	s.records = kept
	s.sortDescAndReindexLocked()
	s.mu.Unlock()

	done := make(chan error, 1)
	s.ops <- writeOp{kind: "rewrite", done: done}
	return removed, <-done
}

// Flush waits until all queued persistence operations finish.
func (s *Store) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return nil
	}
	done := make(chan error, 1)
	s.ops <- writeOp{kind: "flush", done: done}
	return <-done
}

// Close flushes pending writes and shuts down the background persistence worker.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.ops)
		s.mu.Unlock()
		<-s.workerDone
	})
	return nil
}
