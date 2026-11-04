// Package recovery stores the control-plane identity and coordinator state
// required to restore authenticated uploads after a process restart.
//
// # On-disk model
//
// The store is an append-only journal of authenticated lifecycle events. One
// event is one logical transition — session created, batch declared, approval
// changed, upload allocated, item completed, session revoked — never a byte,
// chunk, PATCH or progress percentage. Each frame is length-prefixed so a
// replay can distinguish an incomplete trailing write from corruption in the
// middle of the journal:
//
//	[uint32 big-endian payload length][JSON event]
//
// Every event carries a schema version, a monotonic sequence number, a stable
// event ID, a type, a timestamp, and an HMAC-SHA256 tag keyed by the
// installation key. Nothing secret is ever written: session records hold an
// HMAC verifier for the bearer token, never the token itself.
//
// # Failure policy
//
//   - A corrupt or unauthenticated frame before the end of the journal fails
//     the whole open. Replay never skips a bad record and continues.
//   - The only tolerated damage is a genuinely incomplete final frame, which
//     is preserved as diagnostic evidence and then truncated to the last
//     complete valid offset.
//   - A failing append is sticky: the store refuses every later append so a
//     sequence gap or a torn frame can never appear in the middle of history.
//   - A missing or malformed installation key fails closed whenever durable
//     state already exists. A replacement key is never generated over
//     existing trusted records.
//
// Lock order: callers hold their own runtime mutex (session or coordinator)
// before entering the store. The store never calls back into a caller, so the
// ordering is consistent and deadlock-free.
package recovery

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/secfile"
)

const (
	// schemaVersion is the on-disk journal schema. A record with any other
	// version fails the open rather than being guessed at.
	schemaVersion = 1

	// KeyID is the identifier recorded alongside HMAC tags so a deliberate
	// key rotation can be introduced later without ambiguity.
	KeyID = "install-key-v1"

	// KeyLen is the installation key length in bytes (256 bits).
	KeyLen = 32

	// maxFrameSize bounds one encoded event. It is deliberately far larger
	// than any legitimate lifecycle event and far smaller than a payload.
	maxFrameSize = 4 << 20

	// tailEvidenceLimit bounds the diagnostic copy kept of an incomplete
	// trailing frame.
	tailEvidenceLimit = 64 << 10

	journalName = "state.wal"
	keyFileName = "install-key-v1"
	lockName    = "server.lock"
)

var (
	// ErrCorrupt means durable state could not be trusted: bad framing, bad
	// authentication, a schema or sequence mismatch, or a malformed payload.
	ErrCorrupt = errors.New("recovery: corrupt state")

	// ErrKey means the installation key is missing or unusable while trusted
	// state exists, or is otherwise invalid.
	ErrKey = errors.New("recovery: installation key unavailable")

	// ErrLocked means another process owns the state directory.
	ErrLocked = errors.New("recovery: state directory is owned by another process")
)

// Event types. Each maps to one lifecycle transition.
const (
	EventSessionCreated    = "SessionCreated"
	EventSessionRevoked    = "SessionRevoked"
	EventSessionExpired    = "SessionExpired"
	EventBatchDeclared     = "BatchDeclared"
	EventApprovalChanged   = "BatchApprovalChanged"
	EventItemTransition    = "ItemTransition"
	EventUploadAllocated   = "UploadAllocated"
	EventBatchStateChanged = "BatchStateChanged"
	EventSessionClosed     = "SessionClosed"
)

// SessionRecord is the durable, non-secret description of one session.
//
// TokenVerifier is HMAC-SHA256(installation key, bearer token). The raw token
// is never persisted. SessionKey is the legacy stable owner key
// (hex SHA-256 of the token) that resumable metadata already binds to, so
// existing .meta ownership keeps working after a restart.
type SessionRecord struct {
	SessionID     string     `json:"sessionId"`
	SessionKey    string     `json:"sessionKey"`
	TokenVerifier string     `json:"tokenVerifier"`
	KeyID         string     `json:"keyId,omitempty"`
	Generation    uint64     `json:"generation"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"createdAt"`
	ExpiresAt     time.Time  `json:"expiresAt"`
	RevokedAt     *time.Time `json:"revokedAt,omitempty"`
	DeviceLabel   string     `json:"deviceLabel,omitempty"`
}

// ItemRecord is the durable projection of one coordinator item. Byte progress
// lives in resumable metadata, not here.
type ItemRecord struct {
	ID           string `json:"id"`
	BatchID      string `json:"batchId"`
	Sequence     int    `json:"sequence"`
	RelativePath string `json:"relativePath"`
	DisplayName  string `json:"displayName"`
	DeclaredSize int64  `json:"declaredSize"`
	State        string `json:"state"`
	SHA256       string `json:"sha256"`
	ContentID    string `json:"contentId"`
	Error        string `json:"error,omitempty"`
	Transferred  int64  `json:"transferred,omitempty"`
}

// BatchRecord is the durable projection of one coordinator batch.
type BatchRecord struct {
	ID                 string       `json:"id"`
	SessionKey         string       `json:"sessionKey"`
	Direction          string       `json:"direction"`
	Kind               string       `json:"kind"`
	RootName           string       `json:"rootName"`
	Items              []ItemRecord `json:"items"`
	TotalItemCount     int          `json:"totalItemCount"`
	DeclaredTotalBytes int64        `json:"declaredTotalBytes"`
	TransferredBytes   int64        `json:"transferredBytes"`
	State              string       `json:"state"`
	ApprovalState      string       `json:"approvalState"`
	CreatedAt          time.Time    `json:"createdAt"`
	StartedAt          time.Time    `json:"startedAt"`
	CompletedAt        time.Time    `json:"completedAt"`
	Error              string       `json:"error,omitempty"`
}

// State is the whole durable projection, rebuilt from the journal.
type State struct {
	Sessions []SessionRecord `json:"sessions"`
	Batches  []BatchRecord   `json:"batches"`
}

// ApprovalChange is the payload of EventApprovalChanged.
type ApprovalChange struct {
	BatchID       string `json:"batchId"`
	ApprovalState string `json:"approvalState"`
	BatchState    string `json:"batchState"`
}

// ItemChange is the payload of EventItemTransition.
type ItemChange struct {
	BatchID string     `json:"batchId"`
	Item    ItemRecord `json:"item"`
}

// Allocation is the payload of EventUploadAllocated.
type Allocation struct {
	BatchID   string `json:"batchId"`
	ItemID    string `json:"itemId"`
	ContentID string `json:"contentId"`
}

// BatchStateChange is the payload of EventBatchStateChanged.
type BatchStateChange struct {
	BatchID          string    `json:"batchId"`
	State            string    `json:"state"`
	Error            string    `json:"error,omitempty"`
	CompletedAt      time.Time `json:"completedAt,omitempty"`
	TransferredBytes int64     `json:"transferredBytes"`
}

// SessionClose is the payload of EventSessionClosed.
type SessionClose struct {
	SessionKey string        `json:"sessionKey"`
	ClosedAt   time.Time     `json:"closedAt"`
	Batches    []BatchRecord `json:"batches,omitempty"`
}

// Event is one journal frame.
type Event struct {
	SchemaVersion uint32          `json:"schemaVersion"`
	Sequence      uint64          `json:"sequence"`
	EventID       string          `json:"eventId"`
	Type          string          `json:"type"`
	OccurredAt    time.Time       `json:"occurredAt"`
	Payload       json.RawMessage `json:"payload"`
	MAC           string          `json:"mac"`
}

// Store owns the installation key, the single-instance lock and the journal.
type Store struct {
	mu     sync.Mutex
	dir    string
	wal    *os.File
	lock   *instanceLock
	key    []byte
	seq    uint64
	state  State
	closed bool

	// appendErr latches the first failed append. Further appends fail with
	// the same error so history can never contain a gap or a torn frame in
	// the middle of the journal.
	appendErr error

	// createdKey reports that Open generated a fresh key because no trusted
	// state existed yet.
	createdKey bool
}

// Open opens (or initialises) the state directory and replays the journal.
//
// The returned error may wrap ErrKey, ErrCorrupt or ErrLocked. Any error
// means the caller must not serve: recovery is all-or-nothing for global
// state.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("recovery: empty state directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("recovery: create state directory: %w", err)
	}
	// The 0700 mode above is enforced only on the Unix family. Windows ignores
	// it, so an inherited DACL from a shared or redirected parent could let
	// another local account read the installation key or the journal.
	//
	// This directory is exclusively Flitnix's: it is the application's own
	// state, created by Flitnix and never a path the user chose to share, so
	// replacing its DACL is correct - and must also correct a directory left
	// over from an older install. Apply the policy before the key or any
	// journal frame exists, and refuse to open the store rather than serve
	// from a directory whose confidentiality is unproven.
	if err := secfile.ProtectDir(dir); err != nil {
		return nil, fmt.Errorf("recovery: secure state directory: %w", err)
	}

	lock, err := acquireInstanceLock(filepath.Join(dir, lockName))
	if err != nil {
		return nil, err
	}

	journalPath := filepath.Join(dir, journalName)
	trusted, err := hasTrustedState(journalPath)
	if err != nil {
		_ = lock.release()
		return nil, err
	}
	key, created, err := loadOrCreateKey(filepath.Join(dir, keyFileName), trusted)
	if err != nil {
		_ = lock.release()
		return nil, err
	}

	wal, err := os.OpenFile(journalPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("recovery: open journal: %w", err)
	}
	s := &Store{dir: dir, wal: wal, lock: lock, key: key, createdKey: created}
	if err := s.replay(); err != nil {
		_ = wal.Close()
		_ = lock.release()
		return nil, err
	}
	return s, nil
}

// hasTrustedState reports whether a non-empty journal already exists, which
// makes a missing installation key unrecoverable rather than a fresh install.
func hasTrustedState(journalPath string) (bool, error) {
	info, err := os.Stat(journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("recovery: inspect journal: %w", err)
	}
	return info.Size() > 0, nil
}

// loadOrCreateKey reads the installation key, creating it only for a fresh
// installation with no trusted state. A missing or malformed key alongside
// existing state fails closed: generating a replacement would silently
// restore sessions that no verifier can authenticate.
func loadOrCreateKey(path string, trusted bool) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != KeyLen {
			return nil, false, fmt.Errorf("%w: key %s has length %d, want %d", ErrKey, KeyID, len(data), KeyLen)
		}
		return data, false, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, false, fmt.Errorf("%w: read key: %v", ErrKey, err)
	}
	if trusted {
		return nil, false, fmt.Errorf("%w: key %s is missing while durable state exists; refusing to generate a replacement", ErrKey, KeyID)
	}
	key := make([]byte, KeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("recovery: generate installation key: %w", err)
	}
	if err := writeFileDurable(path, key); err != nil {
		return nil, false, err
	}
	return key, true, nil
}

// writeFileDurable writes a 0600 file atomically and synchronises both the
// file and its parent directory, so a crash cannot leave a half-installed key
// or a key whose directory entry is not durable.
func writeFileDurable(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("recovery: create %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("recovery: write %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("recovery: sync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("recovery: close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("recovery: install %s: %w", filepath.Base(path), err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("recovery: sync state directory after installing %s: %w", filepath.Base(path), err)
	}
	return nil
}

// CreatedKey reports whether Open generated the installation key. It is true
// only for a genuinely fresh installation.
func (s *Store) CreatedKey() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createdKey
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

// State returns a deep copy of the replayed projection.
func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

// Verifier derives the installation-keyed HMAC-SHA256 verifier for a bearer
// token, as hex.
func (s *Store) Verifier(token string) string {
	s.mu.Lock()
	key := append([]byte(nil), s.key...)
	s.mu.Unlock()
	return hex.EncodeToString(hmacToken(key, token))
}

// VerifyToken compares a presented token against a stored verifier in
// constant time.
func (s *Store) VerifyToken(token, verifier string) bool {
	return hmac.Equal([]byte(s.Verifier(token)), []byte(verifier))
}

func hmacToken(key []byte, token string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(token))
	return m.Sum(nil)
}

// Append durably commits one lifecycle event and only then applies it to the
// in-memory projection.
//
// The caller must treat an error as a failed transition: the operation that
// required durability must not be reported as successful, and no later
// append will be accepted from this store.
func (s *Store) Append(kind string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("recovery: encode %s payload: %w", kind, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	if s.closed || s.wal == nil {
		return errors.New("recovery: store is closed")
	}
	id, err := newEventID()
	if err != nil {
		return err
	}
	ev := Event{
		SchemaVersion: schemaVersion,
		Sequence:      s.seq + 1,
		EventID:       id,
		Type:          kind,
		OccurredAt:    time.Now().UTC(),
		Payload:       raw,
	}
	ev.MAC = hex.EncodeToString(s.mac(ev.SchemaVersion, ev.Sequence, ev.Type, ev.Payload))

	// Validate and apply before the write so a malformed payload is rejected
	// without touching the journal. The projection is allowed to lead the
	// journal only until the write fails; from then on the store is latched
	// closed by appendErr.
	if err := applyEvent(&s.state, ev); err != nil {
		return fmt.Errorf("recovery: apply %s: %w", kind, err)
	}
	if err := s.writeFrame(ev); err != nil {
		s.appendErr = err
		return err
	}
	s.seq = ev.Sequence
	return nil
}

func (s *Store) writeFrame(ev Event) error {
	encoded, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("recovery: encode journal event: %w", err)
	}
	if len(encoded) > maxFrameSize {
		return fmt.Errorf("recovery: lifecycle event %s exceeds the %d byte journal frame limit", ev.Type, maxFrameSize)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(encoded)))
	if _, err := s.wal.Write(hdr[:]); err != nil {
		return fmt.Errorf("recovery: append journal length: %w", err)
	}
	if _, err := s.wal.Write(encoded); err != nil {
		return fmt.Errorf("recovery: append journal event: %w", err)
	}
	if err := s.wal.Sync(); err != nil {
		return fmt.Errorf("recovery: sync journal: %w", err)
	}
	return nil
}

// Close releases the journal and the single-instance lock. It performs no
// revocation and writes nothing: a graceful shutdown must leave sessions that
// are meant to survive a restart untouched.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.wal != nil {
		err = s.wal.Close()
		s.wal = nil
	}
	if lockErr := s.lock.release(); err == nil {
		err = lockErr
	}
	s.lock = nil
	return err
}

// replay reconstructs the projection, rejecting any corruption before the
// physical end of the journal.
func (s *Store) replay() error {
	if _, err := s.wal.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("recovery: seek journal: %w", err)
	}
	var last uint64
	var validEnd int64
	for {
		var hdr [4]byte
		n, err := io.ReadFull(s.wal, hdr[:])
		if err != nil {
			if errors.Is(err, io.EOF) && n == 0 {
				break
			}
			return s.recoverTail(validEnd, last, fmt.Sprintf("incomplete trailing frame length (%d of 4 bytes)", n))
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size == 0 || size > maxFrameSize {
			return fmt.Errorf("%w: invalid frame length %d at journal offset %d", ErrCorrupt, size, validEnd)
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(s.wal, data); err != nil {
			return s.recoverTail(validEnd, last, "incomplete trailing frame body")
		}
		var ev Event
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("%w: malformed frame at journal offset %d: %v", ErrCorrupt, validEnd, err)
		}
		if ev.SchemaVersion != schemaVersion {
			return fmt.Errorf("%w: unsupported schema version %d at journal offset %d", ErrCorrupt, ev.SchemaVersion, validEnd)
		}
		if ev.Sequence != last+1 {
			return fmt.Errorf("%w: journal sequence %d at offset %d, want %d", ErrCorrupt, ev.Sequence, validEnd, last+1)
		}
		if ev.EventID == "" {
			return fmt.Errorf("%w: journal event %d has no event id", ErrCorrupt, ev.Sequence)
		}
		expected := hex.EncodeToString(s.mac(ev.SchemaVersion, ev.Sequence, ev.Type, ev.Payload))
		if !hmac.Equal([]byte(expected), []byte(ev.MAC)) {
			return fmt.Errorf("%w: journal authentication failed at sequence %d (offset %d)", ErrCorrupt, ev.Sequence, validEnd)
		}
		if err := applyEvent(&s.state, ev); err != nil {
			return fmt.Errorf("%w: malformed %s payload at sequence %d: %v", ErrCorrupt, ev.Type, ev.Sequence, err)
		}
		last = ev.Sequence
		validEnd += 4 + int64(size)
	}
	s.seq = last
	if _, err := s.wal.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("recovery: seek journal end: %w", err)
	}
	return nil
}

// recoverTail preserves evidence of an incomplete trailing frame and then
// truncates the journal to the last complete valid offset. It is only ever
// reached for a tear at the physical end of the file; corruption with valid
// frames after it is reported as ErrCorrupt and never truncated away.
func (s *Store) recoverTail(validEnd int64, last uint64, reason string) error {
	s.preserveTailEvidence(validEnd)
	if err := s.repairJournal(validEnd); err != nil {
		return fmt.Errorf("%w: %s: truncate trailing frame: %v", ErrCorrupt, reason, err)
	}
	s.seq = last
	return nil
}

// repairJournal closes the append-mode replay handle before truncating and
// reopens it only after the repaired length is durable. Windows can reject
// SetEndOfFile through an append-mode handle even when the file was opened
// read/write, so recovery uses a dedicated non-append handle for this repair.
func (s *Store) repairJournal(validEnd int64) error {
	if err := s.wal.Close(); err != nil {
		return fmt.Errorf("close journal before repair: %w", err)
	}
	s.wal = nil

	journalPath := filepath.Join(s.dir, journalName)
	repair, err := os.OpenFile(journalPath, os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open journal for repair: %w", err)
	}
	if err := repair.Truncate(validEnd); err != nil {
		_ = repair.Close()
		return err
	}
	if err := repair.Sync(); err != nil {
		_ = repair.Close()
		return fmt.Errorf("sync truncated journal: %w", err)
	}
	if err := repair.Close(); err != nil {
		return fmt.Errorf("close repaired journal: %w", err)
	}

	wal, err := os.OpenFile(journalPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("reopen repaired journal: %w", err)
	}
	s.wal = wal
	if _, err := s.wal.Seek(0, io.SeekEnd); err != nil {
		_ = s.wal.Close()
		s.wal = nil
		return fmt.Errorf("seek repaired journal end: %w", err)
	}
	return nil
}

// preserveTailEvidence copies the incomplete trailing bytes aside so a tear
// can still be diagnosed after the journal is repaired. Failure to write the
// diagnostic copy must not block recovery of the valid history.
func (s *Store) preserveTailEvidence(validEnd int64) {
	f, err := os.Open(filepath.Join(s.dir, journalName))
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(validEnd, io.SeekStart); err != nil {
		return
	}
	tail, err := io.ReadAll(io.LimitReader(f, tailEvidenceLimit))
	if err != nil || len(tail) == 0 {
		return
	}
	name := fmt.Sprintf("%s.tail-%d.bin", journalName, time.Now().UTC().UnixNano())
	_ = os.WriteFile(filepath.Join(s.dir, name), tail, 0o600)
}

// mac authenticates the framing fields and the payload together, so a record
// cannot be re-typed, re-sequenced or edited without detection.
func (s *Store) mac(version uint32, sequence uint64, kind string, payload []byte) []byte {
	h := hmac.New(sha256.New, s.key)
	var buf [12]byte
	binary.BigEndian.PutUint32(buf[:4], version)
	binary.BigEndian.PutUint64(buf[4:], sequence)
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(kind))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(payload)
	return h.Sum(nil)
}

func newEventID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("recovery: generate event id: %w", err)
	}
	return "evt_" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// applyEvent is deterministic and idempotent per sequence: replaying the same
// event twice produces the same projection.
func applyEvent(state *State, ev Event) error {
	switch ev.Type {
	case EventSessionCreated, EventSessionRevoked, EventSessionExpired:
		var rec SessionRecord
		if err := json.Unmarshal(ev.Payload, &rec); err != nil {
			return err
		}
		if rec.SessionKey == "" {
			return errors.New("session record has no session key")
		}
		upsertSession(state, rec)
		return nil
	case EventBatchDeclared:
		var rec BatchRecord
		if err := json.Unmarshal(ev.Payload, &rec); err != nil {
			return err
		}
		if rec.ID == "" || rec.SessionKey == "" || len(rec.Items) == 0 {
			return errors.New("batch record is incomplete")
		}
		state.Batches = append(state.Batches, cloneBatch(rec))
		return nil
	case EventApprovalChanged:
		var ch ApprovalChange
		if err := json.Unmarshal(ev.Payload, &ch); err != nil {
			return err
		}
		batch := findBatch(state, ch.BatchID)
		if batch == nil {
			return fmt.Errorf("approval change for unknown batch %q", ch.BatchID)
		}
		if ch.ApprovalState != "" {
			batch.ApprovalState = ch.ApprovalState
		}
		if ch.BatchState != "" {
			batch.State = ch.BatchState
		}
		return nil
	case EventItemTransition:
		var ch ItemChange
		if err := json.Unmarshal(ev.Payload, &ch); err != nil {
			return err
		}
		batch := findBatch(state, ch.BatchID)
		if batch == nil {
			return fmt.Errorf("item transition for unknown batch %q", ch.BatchID)
		}
		for i := range batch.Items {
			if batch.Items[i].ID == ch.Item.ID {
				batch.Items[i] = ch.Item
				return nil
			}
		}
		return fmt.Errorf("item transition for unknown item %q", ch.Item.ID)
	case EventUploadAllocated:
		var ch Allocation
		if err := json.Unmarshal(ev.Payload, &ch); err != nil {
			return err
		}
		batch := findBatch(state, ch.BatchID)
		if batch == nil {
			return fmt.Errorf("allocation for unknown batch %q", ch.BatchID)
		}
		for i := range batch.Items {
			if batch.Items[i].ID == ch.ItemID {
				batch.Items[i].ContentID = ch.ContentID
				return nil
			}
		}
		return fmt.Errorf("allocation for unknown item %q", ch.ItemID)
	case EventBatchStateChanged:
		var ch BatchStateChange
		if err := json.Unmarshal(ev.Payload, &ch); err != nil {
			return err
		}
		batch := findBatch(state, ch.BatchID)
		if batch == nil {
			return fmt.Errorf("state change for unknown batch %q", ch.BatchID)
		}
		batch.State = ch.State
		batch.Error = ch.Error
		batch.CompletedAt = ch.CompletedAt
		batch.TransferredBytes = ch.TransferredBytes
		return nil
	case EventSessionClosed:
		var ch SessionClose
		if err := json.Unmarshal(ev.Payload, &ch); err != nil {
			return err
		}
		for _, rec := range ch.Batches {
			if batch := findBatch(state, rec.ID); batch != nil {
				*batch = cloneBatch(rec)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown event type %q", ev.Type)
	}
}

func upsertSession(state *State, rec SessionRecord) {
	for i := range state.Sessions {
		if state.Sessions[i].SessionKey == rec.SessionKey {
			state.Sessions[i] = rec
			return
		}
	}
	state.Sessions = append(state.Sessions, rec)
}

func findBatch(state *State, id string) *BatchRecord {
	if id == "" {
		return nil
	}
	for i := range state.Batches {
		if state.Batches[i].ID == id {
			return &state.Batches[i]
		}
	}
	return nil
}

func cloneState(in State) State {
	out := State{Sessions: append([]SessionRecord(nil), in.Sessions...)}
	out.Batches = make([]BatchRecord, len(in.Batches))
	for i := range in.Batches {
		out.Batches[i] = cloneBatch(in.Batches[i])
	}
	return out
}

func cloneBatch(in BatchRecord) BatchRecord {
	out := in
	out.Items = append([]ItemRecord(nil), in.Items...)
	return out
}
