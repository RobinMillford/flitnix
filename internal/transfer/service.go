package transfer

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// State is the lifecycle state of an incoming upload.
type State string

const (
	StatePending   State = "pending"   // metadata received, awaiting the desktop user's decision
	StateApproved  State = "approved"  // approved; exactly one content upload is now allowed
	StateRejected  State = "rejected"  // refused by the desktop user; no content will be uploaded
	StateUploading State = "uploading" // content stream in progress
	StateCompleted State = "completed" // received fully, published in the receive directory
	StateFailed    State = "failed"    // transfer terminated with an error
	StateCanceled  State = "canceled"  // canceled by the desktop (shutdown or session end)
)

var (
	// ErrUploadNotFound means no upload with that ID exists.
	ErrUploadNotFound = errors.New("transfer: upload not found")

	// ErrInvalidUpload means the upload metadata itself is invalid.
	ErrInvalidUpload = errors.New("transfer: invalid upload metadata")

	// ErrWrongSession means the upload belongs to a different session.
	ErrWrongSession = errors.New("transfer: upload belongs to another session")

	// ErrAlreadyDecided means a decision was already recorded (approving
	// or rejecting twice, or sending content after a rejection).
	ErrAlreadyDecided = errors.New("transfer: upload already has a decision")

	// ErrNotApproved means content was sent while the upload was still
	// pending approval.
	ErrNotApproved = errors.New("transfer: upload is not approved")

	// ErrUploadUnderway means content submission was attempted while an
	// earlier stream already owns (or owned) the body.
	ErrUploadUnderway = errors.New("transfer: upload already in progress")

	// ErrCanceled means the upload was canceled before or during its
	// content stream.
	ErrCanceled = errors.New("transfer: upload was canceled")

	// ErrNoCapacity means a concurrency limit blocked the operation.
	ErrNoCapacity = errors.New("transfer: too many concurrent uploads")

	// ErrTooLateToCancel means a cancel request lost the race with
	// publication: the received bytes are already (or are becoming) the
	// published file, so the cancellation has no authority left. The record
	// stays completed instead of being relabeled canceled, because a
	// canceled record next to a published file would be a lie about disk.
	ErrTooLateToCancel = errors.New("transfer: publication already decided")
)

// CancelAuthority is the outcome of Service.ReserveCancel: it says whether a
// cancellation can still decide an upload's fate, or whether publication (or
// the end of the upload) got there first.
type CancelAuthority int

const (
	// CancelWins means the caller now owns the outcome: publication is
	// refused from this point on, so the item can be canceled and no final
	// file can appear.
	CancelWins CancelAuthority = iota

	// CancelLostToPublication means the stream had already claimed
	// publication before the cancel arrived. The record ends completed and
	// the caller must leave every other layer describing it as completed.
	CancelLostToPublication

	// CancelLostToTerminal means the upload already ended some other way
	// (a failed stream, a rejection, or an earlier cancellation of the
	// attempt). The upload's own state is authoritative.
	CancelLostToTerminal
)

const (
	// defaultMaxPendingUploads bounds uploads awaiting approval at once.
	defaultMaxPendingUploads = 32

	// defaultMaxActiveUploads bounds uploads streaming at once. This is a
	// politeness cap for stuck peers; it is NOT a maximum file size and
	// no such size cap exists by design.
	defaultMaxActiveUploads = 8

	// defaultMaxPendingBatchItems bounds batch-linked uploads awaiting a
	// batch decision. Batch items are governed by their batch's single
	// decision rather than by one desktop click each, so they are bounded
	// separately from standalone uploads: two maximum-size manifests.
	defaultMaxPendingBatchItems = 2 * 5000

	// defaultTerminalHistory bounds how many finished records are kept
	// for the desktop status view.
	defaultTerminalHistory = 64
)

// Limits bound how many uploads may exist in each phase concurrently. Zero
// fields select the defaults above. The caps exist so a stuck peer cannot
// accumulate unbounded state; they are deliberately generous for a one
// phone at a time app and impose no maximum file size.
type Limits struct {
	MaxPending int
	MaxActive  int
}

// Config configures a Service.
type Config struct {
	// Store resolves the receive directory. Required (pass NewStore's
	// result).
	Store *Store

	// Limits bounds concurrent uploads. Zero values select defaults.
	Limits Limits
}

// Service owns the upload approval machine. All methods are safe for
// concurrent use. Upload records are keyed by opaque IDs and bound to the
// session that created them (the session's non-secret hex digest), so a
// token holder can only ever see and extend its own uploads.
type Service struct {
	store  *Store
	limits Limits

	mu      sync.Mutex
	uploads map[string]*upload
	nextSeq int64
}

// NewService prepares the approval machine.
func NewService(cfg Config) *Service {
	limits := cfg.Limits
	if limits.MaxPending <= 0 {
		limits.MaxPending = defaultMaxPendingUploads
	}

	if limits.MaxActive <= 0 {
		limits.MaxActive = defaultMaxActiveUploads
	}

	return &Service{store: cfg.Store, limits: limits, uploads: make(map[string]*upload)}
}

// Store exposes the configured storage boundary to the server composition
// layer for batch-root allocation.
func (s *Service) Store() *Store { return s.store }

// Upload is a consistent copy of one upload record for callers outside the
// package (HTTP handlers, the desktop UI).
type Upload struct {
	ID        string    // opaque upload ID
	Name      string    // sanitized final filename candidate
	Session   string    // owning session's non-secret key (hex digest)
	Size      int64     // approved size in bytes
	Created   time.Time // when the metadata arrived
	State     State
	Reason    string  // non-empty on failed/canceled records: why the transfer ended
	Uploaded  int64   // bytes actually received so far (live)
	Speed     float64 // measured bytes per second while receiving
	Published string  // absolute final path; populated exactly in the completed state

	// BatchLinked is true for a record that belongs to a transfer batch. The
	// batch coordinator is its only approval authority: such a record is
	// never offered as an independent approval request.
	BatchLinked bool
}

// ContentResult describes a successfully received upload body.
type ContentResult struct {
	// Written is the exact byte count written to disk, guaranteed to
	// equal the approved size by the stream enforcement.
	Written int64

	// SHA256 is the lowercase hex digest of exactly the bytes that were
	// written to disk.
	SHA256 string

	// Path is the published absolute path inside the receive directory.
	Path string
}

// upload is the mutable record behind a snapshot. mu guards every mutable
// field; Uploaded stays atomic so the streaming goroutine's progress
// callback never blocks the desktop's polling reads.
type upload struct {
	id           string
	name         string
	root         string
	relativePath string
	session      string
	size         int64
	created      time.Time

	// managed marks a batch item whose approval is owned by the batch
	// coordinator. It is fixed at declaration and never changes.
	managed bool

	// seq is a monotonically increasing creation counter assigned under
	// the service lock. It orders even records whose Created timestamps
	// tie (time.Now can repeat within a single nanosecond resolution), so
	// list ordering and pruning are fully deterministic.
	seq int64

	mu        sync.Mutex
	state     State
	reason    string
	uploaded  atomic.Int64
	sink      contentSink // non-nil while a content stream owns the record
	published string
	speed     float64
	lastAt    time.Time
	lastBytes int64

	// pub records the single decision a finishing stream and a cancel
	// contend for. It is taken under mu before any publication work, so
	// exactly one of "the file is published" and "the upload is canceled"
	// is ever chosen, and every layer can read the winner afterwards.
	pub pubPhase
}

// pubPhase is the publication-versus-cancellation decision of one upload.
type pubPhase uint8

const (
	// pubOpen means neither side has decided yet; both may still win.
	pubOpen pubPhase = iota

	// pubPublishing means the stream won: it claimed publication before any
	// cancel could, so a later cancel can only report that it was too late.
	pubPublishing

	// pubCancelReserved means a cancel won: publication is refused from now
	// on, so no final file can exist for this attempt.
	pubCancelReserved
)

// snapshotLocked copies the record; the caller must already hold u.mu.
func (u *upload) snapshotLocked() Upload {
	return Upload{
		ID:        u.id,
		Name:      u.name,
		Session:   u.session,
		Size:      u.size,
		Created:   u.created,
		State:     u.state,
		Reason:    u.reason,
		Uploaded:  u.uploaded.Load(),
		Speed:     u.speed,
		Published: u.published,

		BatchLinked: u.managed,
	}
}

// snapshot copies the record under its lock.
func (u *upload) snapshot() Upload {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.snapshotLocked()
}

// stateOf reads a record's state safely from callers that already hold
// s.mu. The mutual exclusion matters because state transitions happen
// under u.mu without s.mu. The lock order in this package is always
// s.mu → u.mu, never reversed.
func stateOf(u *upload) State {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

func isTerminal(st State) bool {
	switch st {
	case StateRejected, StateCompleted, StateFailed, StateCanceled:
		return true
	}
	return false
}

// makeFailed marks the record failed with a reason. The caller must hold
// u.mu and the record must be non-terminal.
func (u *upload) makeFailedLocked(err error) {
	u.state = StateFailed
	u.reason = err.Error()
}

// newPartSink is the factory this package uses to open receive sinks. The
// service tests replace it with fakes to exercise write failures without
// touching the real disk.
var newPartSink = func(store *Store, name string) (contentSink, error) {
	return store.OpenPart(name)
}

// Declare records a pending upload for the given session key. The metadata
// is validated here, before anything is stored or trusted: the filename is
// sanitized, the size must be non-negative, and the limits are enforced.
func (s *Service) Declare(sessionKey, rawName string, size int64) (Upload, error) {
	return s.declare(sessionKey, rawName, "", "", size, false)
}

// DeclareBatchItem records a pending upload that belongs to a transfer batch.
// root and relativePath are empty for a flat file batch and both set for a
// folder batch (see DeclarePath). The record is excluded from the standalone
// approval list and cannot be decided one by one through the legacy approval
// surface; it is decided together with its batch by DecideBatchItems.
func (s *Service) DeclareBatchItem(sessionKey, root, relativePath, rawName string, size int64) (Upload, error) {
	if (root == "") != (relativePath == "") {
		return Upload{}, ErrInvalidUpload
	}
	return s.declare(sessionKey, rawName, root, relativePath, size, true)
}

// DeclarePath records a folder item whose server-selected root and validated
// relative path determine its eventual publication location.
func (s *Service) DeclarePath(sessionKey, root, relativePath, rawName string, size int64) (Upload, error) {
	if root == "" || relativePath == "" {
		return Upload{}, ErrInvalidUpload
	}
	return s.declare(sessionKey, rawName, root, relativePath, size, false)
}

func (s *Service) declare(sessionKey, rawName, root, relativePath string, size int64, managed bool) (Upload, error) {
	if sessionKey == "" {
		return Upload{}, ErrWrongSession
	}
	name, err := SanitizeName(rawName)
	if err != nil {
		return Upload{}, err
	}
	if size < 0 {
		return Upload{}, fmt.Errorf("%w: size must not be negative", ErrInvalidUpload)
	}
	if root != "" {
		if _, err := sanitizeRootName(root); err != nil {
			return Upload{}, err
		}
		if relativePath, err = SanitizeRelativePath(relativePath); err != nil {
			return Upload{}, err
		}
		if name != filepath.Base(filepath.FromSlash(relativePath)) {
			return Upload{}, fmt.Errorf("%w: display name does not match relative path", ErrInvalidUpload)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.capacityLocked(managed); err != nil {
		return Upload{}, err
	}
	id, err := newID()
	if err != nil {
		return Upload{}, err
	}
	u := &upload{
		id:           id,
		name:         name,
		root:         root,
		relativePath: relativePath,
		session:      sessionKey,
		size:         size,
		created:      time.Now(),
		managed:      managed,
		seq:          s.nextSeq,
		state:        StatePending,
	}
	s.nextSeq++
	s.uploads[id] = u
	s.pruneTerminalLocked()
	return u.snapshot(), nil
}

// capacityLocked enforces the limits. Callers must hold s.mu. Standalone
// uploads are bounded by MaxPending/MaxActive; batch items only by the
// batch-item cap, because one batch decision governs all of them and they
// stream strictly one at a time per session.
func (s *Service) capacityLocked(managed bool) error {
	var pending, active, pendingBatchItems int
	for _, u := range s.uploads {
		switch stateOf(u) {
		case StatePending:
			if u.managed {
				pendingBatchItems++
			} else {
				pending++
			}
		case StateUploading:
			if !u.managed {
				active++
			}
		}
	}
	if managed {
		if pendingBatchItems >= defaultMaxPendingBatchItems {
			return fmt.Errorf("%w: too many batch items are pending approval", ErrNoCapacity)
		}
		return nil
	}
	if pending >= s.limits.MaxPending {
		return fmt.Errorf("%w: too many uploads are pending approval", ErrNoCapacity)
	}
	if active >= s.limits.MaxActive {
		return fmt.Errorf("%w: too many transfers are already streaming", ErrNoCapacity)
	}
	return nil
}

// pruneTerminalLocked bounds the retained finished records so the status
// view never grows without bound. Callers must hold s.mu.
func (s *Service) pruneTerminalLocked() {
	var terminal []*upload
	for _, u := range s.uploads {
		if isTerminal(stateOf(u)) {
			terminal = append(terminal, u)
		}
	}
	if len(terminal) <= defaultTerminalHistory {
		return
	}
	// Oldest first by the monotonic creation sequence: deterministic even
	// when Created timestamps tie.
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].seq < terminal[j].seq })
	for _, u := range terminal[:len(terminal)-defaultTerminalHistory] {
		delete(s.uploads, u.id)
	}
}

// lookup returns the record for the ID, verifying session ownership when a
// session key is given. Records are returned without their per-record lock;
// the caller takes it before touching mutable fields.
func (s *Service) lookup(id, sessionKey string) (*upload, error) {
	s.mu.Lock()
	u, ok := s.uploads[id]
	s.mu.Unlock()
	if !ok {
		return nil, ErrUploadNotFound
	}
	if sessionKey != "" && u.session != sessionKey {
		return nil, ErrWrongSession
	}
	return u, nil
}

// Decide records the desktop user's approval decision for a pending
// upload. A second Decide (even with the same outcome) and a racing content
// submission are both rejected with precise errors.
func (s *Service) Decide(id string, approve bool) (Upload, error) {
	u, err := s.lookup(id, "") // decisions are desktop-initiated; no session check
	if err != nil {
		return Upload{}, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state != StatePending {
		return u.snapshotLocked(), fmt.Errorf("%w: current state is %q", ErrAlreadyDecided, u.state)
	}
	if approve {
		u.state = StateApproved
	} else {
		u.state = StateRejected
	}
	return u.snapshotLocked(), nil
}

// DecideBatchItems records one decision for every listed batch-linked upload
// atomically: either every record is pending and all of them take the
// decision, or none is touched and an error describes the first conflict.
// This is the only decision path for batch items; there is no state in which
// part of a batch is approved and the rest is not.
func (s *Service) DecideBatchItems(ids []string, approve bool) error {
	if len(ids) == 0 {
		return ErrInvalidUpload
	}
	seen := make(map[string]bool, len(ids))
	records := make([]*upload, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("%w: duplicate upload in decision", ErrInvalidUpload)
		}
		seen[id] = true
		u, err := s.lookup(id, "")
		if err != nil {
			return err
		}
		if !u.managed {
			return fmt.Errorf("%w: upload is not part of a batch", ErrInvalidUpload)
		}
		records = append(records, u)
	}
	// A fixed lock order makes concurrent decisions over overlapping sets
	// deadlock-free. No other path holds two record locks at once.
	sort.Slice(records, func(i, j int) bool { return records[i].id < records[j].id })
	for _, u := range records {
		u.mu.Lock()
	}
	defer func() {
		for _, u := range records {
			u.mu.Unlock()
		}
	}()
	for _, u := range records {
		if u.state != StatePending {
			return fmt.Errorf("%w: current state is %q", ErrAlreadyDecided, u.state)
		}
	}
	next := StateRejected
	if approve {
		next = StateApproved
	}
	for _, u := range records {
		u.state = next
	}
	return nil
}

// SubmitContent streams the body of an approved upload into the receive
// directory. It enforces the approved size against the actual byte stream
// and leaves the record in a terminal state: completed on an exact-size
// receipt, failed/canceled otherwise, with the part file cleaned up on
// every non-success path. onProgress, if non-nil, is invoked from the
// streaming path as bytes land on disk.
func (s *Service) SubmitContent(id, sessionKey string, body io.Reader, onProgress func(Upload)) (ContentResult, error) {
	return s.SubmitContentWithPublish(id, sessionKey, body, onProgress, nil)
}

// SubmitContentWithPublish streams the body of an approved upload into the
// receive directory. It invokes onPublish immediately after the publication
// decision is taken under the record lock, before filesystem commit I/O begins.
func (s *Service) SubmitContentWithPublish(id, sessionKey string, body io.Reader, onProgress func(Upload), onPublish func()) (ContentResult, error) {
	if sessionKey == "" || body == nil {
		return ContentResult{}, ErrWrongSession // content always requires a live session and a body
	}
	u, err := s.lookup(id, sessionKey)
	if err != nil {
		return ContentResult{}, err
	}

	// Claim the stream. Opening the receive file happens under the record
	// lock so a concurrent Decide or Cancel can never interleave with the
	// state claim; the create syscall is the only latency cost.
	u.mu.Lock()
	switch u.state {
	case StateCanceled:
		u.mu.Unlock()
		return ContentResult{}, ErrCanceled
	case StatePending:
		u.mu.Unlock()
		return ContentResult{}, ErrNotApproved
	case StateRejected:
		u.mu.Unlock()
		return ContentResult{}, ErrAlreadyDecided
	case StateUploading:
		u.mu.Unlock()
		return ContentResult{}, ErrUploadUnderway
	case StateCompleted, StateFailed:
		u.mu.Unlock()
		return ContentResult{}, fmt.Errorf("%w: state is %q", ErrAlreadyDecided, u.state)
	}
	// u.state == StateApproved: claim this submission for the stream.
	var sink contentSink
	var perr error
	if u.relativePath != "" {
		sink, perr = s.store.OpenPartPath(u.root, u.relativePath)
	} else {
		sink, perr = newPartSink(s.store, u.name)
	}
	if perr != nil {
		u.makeFailedLocked(perr)
		u.mu.Unlock()
		return ContentResult{}, fmt.Errorf("transfer: cannot prepare the receive file: %w", perr)
	}
	u.state = StateUploading
	u.sink = sink
	u.uploaded.Store(0)
	u.mu.Unlock()

	result, serr := streamBody(body, sink, u.size, func(written int64) {
		u.uploaded.Store(written)
		u.mu.Lock()
		now := time.Now()
		if elapsed := now.Sub(u.lastAt).Seconds(); elapsed > 0 {
			u.speed = float64(written-u.lastBytes) / elapsed
		}
		u.lastAt = now
		u.lastBytes = written
		u.mu.Unlock()
		if onProgress != nil {
			onProgress(u.snapshot())
		}
	})

	if serr != nil {
		_ = sink.Abort()
		u.mu.Lock()
		u.sink = nil
		u.reason = serr.Error()
		if u.state == StateCanceled {
			// Cancel decided the fate during streaming: keep the
			// canceled status honest rather than overwriting failed.
			u.mu.Unlock()
			return ContentResult{}, fmt.Errorf("%w: %v", ErrCanceled, serr)
		}
		u.state = StateFailed
		u.mu.Unlock()
		return ContentResult{}, fmt.Errorf("%s: %w", u.name, serr)
	}

	// The bytes are in. Publication and cancellation contend for exactly one
	// decision, and it is taken here, under the record lock, before any
	// publishing work: a cancel that already reserved the outcome refuses
	// publication (nothing is linked into place), while a stream that claims
	// it first makes every later cancel a pure "too late" report. Because
	// both sides take this decision under the same mutex, the record and the
	// filesystem cannot disagree afterwards.
	u.mu.Lock()
	switch u.pub {
	case pubCancelReserved:
		u.state = StateCanceled
		u.reason = "canceled"
		u.sink = nil
		u.mu.Unlock()
		_ = sink.Abort()
		return ContentResult{}, fmt.Errorf("%w: canceled before publication", ErrCanceled)
	case pubPublishing:
		// Unreachable: only the stream that wrote the body calls this, once.
		u.mu.Unlock()
		_ = sink.Abort()
		return ContentResult{}, fmt.Errorf("%w: publication already under way", ErrAlreadyDecided)
	}
	u.pub = pubPublishing
	u.mu.Unlock()

	if onPublish != nil {
		onPublish()
	}

	final, cerr := sink.Commit()
	u.mu.Lock()
	u.sink = nil
	if cerr != nil {
		// The data arrived intact but atomic publication failed; keep
		// the record failed and surface the store's precise error.
		u.reason = cerr.Error()
		if u.state == StateCanceled {
			// Cancel decided the fate during the commit (its abort won
			// the publication race): keep the canceled status honest
			// rather than overwriting failed.
			u.mu.Unlock()
			return ContentResult{}, fmt.Errorf("%w: %v", ErrCanceled, cerr)
		}
		u.state = StateFailed
		u.mu.Unlock()
		return ContentResult{}, fmt.Errorf("%s: %w", u.name, cerr)
	}
	u.state = StateCompleted
	u.published = final
	u.mu.Unlock()
	return ContentResult{Written: result.Written, SHA256: result.HexSHA256(), Path: final}, nil
}

// ReserveCancel decides one upload's contest between cancellation and
// publication without publishing or aborting anything. It must be called
// before a cancellation is propagated to the batch coordinator: the decision
// is what tells the caller whether the item may still be described as
// canceled, and it is taken under the same record lock the finishing stream
// uses to claim publication, so no window is left between the two.
//
// CancelWins means the caller owns the outcome: from now on publication is
// refused, so the item must be canceled and Cancel should finish the abort.
// The other authorities mean the upload already ended; the caller must not
// relabel it, and the upload's own state is what every layer should show.
func (s *Service) ReserveCancel(id string) (CancelAuthority, error) {
	u, err := s.lookup(id, "")
	if err != nil {
		return CancelLostToTerminal, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	publishing := u.pub == pubPublishing
	switch u.state {
	case StateCompleted:
		return CancelLostToPublication, fmt.Errorf("%w: the file is already published", ErrTooLateToCancel)
	case StateFailed, StateRejected:
		return CancelLostToTerminal, fmt.Errorf("%w: the upload already ended", ErrTooLateToCancel)
	case StateCanceled:
		// Reading canceled is not enough on its own: a cancel that landed
		// inside the publishing window set this state while the stream was
		// already linking the file into place, and the record is about to
		// read completed. Only a cancel taken before publication decided
		// anything owns the outcome.
		if publishing {
			return CancelLostToPublication, fmt.Errorf("%w: publication is already under way", ErrTooLateToCancel)
		}
		return CancelWins, nil
	}
	if publishing {
		return CancelLostToPublication, fmt.Errorf("%w: publication is already under way", ErrTooLateToCancel)
	}
	if u.pub == pubCancelReserved {
		// A concurrent cancel already reserved the outcome; asserting it
		// again is a safe no-op.
		return CancelWins, nil
	}
	u.pub = pubCancelReserved
	return CancelWins, nil
}

// Cancel terminates a pending, approved, or streaming upload. An approved
// upload the phone never streamed can be aborted this way before any file
// exists on disk. A streaming upload's receive file is closed immediately,
// which makes the in-flight stream fail fast on its next write. Cancelling
// from the terminal states is a no-op.
func (s *Service) Cancel(id string) error {
	u, err := s.lookup(id, "")
	if err != nil {
		return err
	}
	u.mu.Lock()
	switch u.state {
	case StatePending, StateApproved:
		u.state = StateCanceled
		u.mu.Unlock()
	case StateUploading:
		sink := u.sink
		u.state = StateCanceled
		u.mu.Unlock()
		if sink != nil {
			_ = sink.Abort()
		}
	default:
		u.mu.Unlock()
	}
	return nil
}

// CancelSession cancels every pending, approved, or streaming upload owned
// by the given session (matched by the session's non-secret key). The
// desktop calls it when stopping or revoking a session so no half-received
// file lingers on disk and no orphaned approved record outlives the
// connection.
func (s *Service) CancelSession(sessionKey string) {
	if sessionKey == "" {
		return
	}
	s.mu.Lock()
	var targets []*upload
	for _, u := range s.uploads {
		if u.session == sessionKey {
			switch stateOf(u) {
			case StatePending, StateApproved, StateUploading:
				targets = append(targets, u)
			}
		}
	}
	s.mu.Unlock()
	for _, u := range targets {
		u.mu.Lock()
		if u.pub == pubPublishing {
			u.mu.Unlock()
			continue
		}
		switch u.state {
		case StatePending, StateApproved:
			u.pub = pubCancelReserved
			u.state = StateCanceled
			u.mu.Unlock()
		case StateUploading:
			sink := u.sink
			u.pub = pubCancelReserved
			u.state = StateCanceled
			u.mu.Unlock()
			if sink != nil {
				_ = sink.Abort()
			}
		default:
			u.mu.Unlock()
		}
	}
}

// Get returns a consistent copy of one upload record.
func (s *Service) Get(id string) (Upload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok {
		return Upload{}, false
	}
	return u.snapshot(), true
}

// List returns every upload snapshot sorted oldest first (in creation
// order): live uploads plus a bounded recent history of finished ones.
func (s *Service) List() []Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	type entry struct {
		snap Upload
		seq  int64
	}
	entries := make([]entry, 0, len(s.uploads))
	for _, u := range s.uploads {
		entries = append(entries, entry{snap: u.snapshot(), seq: u.seq})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	out := make([]Upload, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.snap)
	}
	return out
}

// ListPending returns the standalone uploads currently awaiting a decision,
// oldest first (in creation order). Batch-linked items are deliberately not
// listed: their batch is the unit of approval. The desktop approval view is
// the caller.
func (s *Service) ListPending() []Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	type entry struct {
		snap Upload
		seq  int64
	}
	var entries []entry
	for _, u := range s.uploads {
		if !u.managed && stateOf(u) == StatePending {
			entries = append(entries, entry{snap: u.snapshot(), seq: u.seq})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	out := make([]Upload, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.snap)
	}
	return out
}

// Counts summarizes the state of the service for status reports.
type Counts struct {
	Pending   int
	Uploading int
}

// Counts returns the current numbers of pending and streaming uploads.
func (s *Service) Counts() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Counts
	for _, u := range s.uploads {
		switch stateOf(u) {
		case StatePending:
			if !u.managed {
				c.Pending++
			}
		case StateUploading:
			c.Uploading++
		}
	}
	return c
}
