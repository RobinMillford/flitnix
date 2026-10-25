package transfer

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrCoordinatorClosed = errors.New("transfer: batch coordinator is closed")
	ErrSessionClosed     = errors.New("transfer: session is closed")
	ErrBatchNotFound     = errors.New("transfer: batch not found")
	ErrItemNotFound      = errors.New("transfer: item not found")
	ErrQueuePaused       = errors.New("transfer: session queue is paused")
	ErrNoEligibleItem    = errors.New("transfer: no eligible item")
	ErrLeaseInvalid      = errors.New("transfer: invalid or stale item lease")
	ErrInvalidTransition = errors.New("transfer: invalid batch transition")
	ErrProgressInvalid   = errors.New("transfer: invalid item progress")
)

// CoordinatorConfig configures the in-memory batch coordinator.
type CoordinatorConfig struct {
	// Now is injectable so lifecycle timestamps can be deterministic in tests.
	// A nil clock uses time.Now.
	Now func() time.Time
}

// TransferLease identifies one exclusive ownership claim for an item.
// LeaseID changes for every retry, so an obsolete stream cannot complete a
// later attempt of the same item.
type TransferLease struct {
	LeaseID      string
	BatchID      string
	ItemID       string
	Sequence     int
	DeclaredSize int64
	Item         ItemView
}

// QueueSnapshot is an immutable session-scoped queue projection.
type QueueSnapshot struct {
	Closed        bool
	Paused        bool
	ActiveLeaseID string
	Batches       []BatchView
}

type coordinatorSession struct {
	closed  bool
	paused  bool
	active  *activeLease
	batches []string
}

type activeLease struct {
	TransferLease
	cancel context.CancelFunc
}

type coordinatorBatch struct {
	batch TransferBatch
}

// Coordinator owns in-memory batch state. It does not perform file or network
// I/O and is independent of Wails, HTTP, the frontend, and the OS.
type Coordinator struct {
	mu       sync.Mutex
	now      func() time.Time
	closed   bool
	batches  map[string]*coordinatorBatch
	sessions map[string]*coordinatorSession
}

// NewCoordinator creates an empty in-memory coordinator.
func NewCoordinator(cfg CoordinatorConfig) *Coordinator {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Coordinator{
		now:      now,
		batches:  make(map[string]*coordinatorBatch),
		sessions: make(map[string]*coordinatorSession),
	}
}

// CreateBatch validates a manifest, assigns server-owned IDs and appends the
// batch to the owning session's FIFO queue. Phone-originated batches wait for
// explicit approval; desktop-originated batches are already approved.
func (c *Coordinator) CreateBatch(sessionKey string, manifest BatchManifest) (BatchView, error) {
	if sessionKey == "" {
		return BatchView{}, ErrWrongSession
	}
	validated, err := ValidateManifest(manifest)
	if err != nil {
		return BatchView{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return BatchView{}, ErrCoordinatorClosed
	}
	session := c.sessionLocked(sessionKey)
	if session.closed {
		return BatchView{}, ErrSessionClosed
	}

	batchID, err := NewOpaqueID()
	if err != nil {
		return BatchView{}, err
	}
	state := BatchStatePending
	approval := ApprovalPending
	itemState := ItemStatePending
	if manifest.Direction == DirectionDesktopToPhone {
		state = BatchStateQueued
		approval = ApprovalApproved
		itemState = ItemStateQueued
	}
	now := c.now()
	batch := TransferBatch{
		ID: batchID, SessionKey: sessionKey, Direction: manifest.Direction,
		Kind: manifest.Kind, RootName: validated.RootName,
		TotalItemCount: len(validated.Items), DeclaredTotalBytes: validated.TotalBytes,
		State: state, ApprovalState: approval, CreatedAt: now,
	}
	batch.Items = make([]TransferItem, 0, len(validated.Items))
	for sequence, item := range validated.Items {
		itemID, err := NewOpaqueID()
		if err != nil {
			return BatchView{}, err
		}
		batch.Items = append(batch.Items, TransferItem{
			ID: itemID, BatchID: batchID, Sequence: sequence,
			RelativePath: item.RelativePath, DisplayName: item.DisplayName,
			DeclaredSize: item.Size, State: itemState,
		})
	}
	c.batches[batchID] = &coordinatorBatch{batch: batch}
	session.batches = append(session.batches, batchID)
	return batch.View(), nil
}

// ApproveBatch moves a pending incoming batch into its session queue.
func (c *Coordinator) ApproveBatch(sessionKey, batchID string) (BatchView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	batch, err := c.ownedBatchLocked(sessionKey, batchID)
	if err != nil {
		return BatchView{}, err
	}

	if batch.batch.ApprovalState != ApprovalPending || batch.batch.State != BatchStatePending {
		return BatchView{}, ErrInvalidTransition
	}
	batch.batch.ApprovalState = ApprovalApproved
	batch.batch.State = BatchStateQueued
	for i := range batch.batch.Items {
		batch.batch.Items[i].State = ItemStateQueued
	}
	return batch.batch.View(), nil
}

// SetItemContentID associates a coordinator item with the legacy streaming
// service record that owns its bytes. The ID is internal and never appears
// in ItemView.
func (c *Coordinator) SetItemContentID(sessionKey, batchID, itemID, contentID string) error {
	if contentID == "" {
		return ErrInvalidBatchID
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	batch, err := c.ownedBatchLocked(sessionKey, batchID)
	if err != nil {
		return err
	}
	item := findItem(&batch.batch, itemID)
	if item == nil {
		return ErrItemNotFound
	}
	if item.ContentID != "" {
		return ErrInvalidTransition
	}
	item.ContentID = contentID
	return nil
}

// ItemContentID returns the internal streaming-service record ID for an item.
func (c *Coordinator) ItemContentID(sessionKey, batchID, itemID string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	batch, err := c.ownedBatchLocked(sessionKey, batchID)
	if err != nil {
		return "", err
	}
	item := findItem(&batch.batch, itemID)
	if item == nil {
		return "", ErrItemNotFound
	}
	if item.ContentID == "" {
		return "", ErrInvalidTransition
	}
	return item.ContentID, nil
}

// RejectBatch permanently rejects a batch that has not completed or already
// been canceled. Any active cancellation callback is invoked after unlocking.
func (c *Coordinator) RejectBatch(sessionKey, batchID string) (BatchView, error) {
	return c.endBatch(sessionKey, batchID, BatchStateRejected, ApprovalRejected, ItemStateRejected)
}

// PeekNext returns the next item that would be claimed without changing state.
func (c *Coordinator) PeekNext(sessionKey string) (ItemView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return ItemView{}, err
	}
	if session.paused {
		return ItemView{}, ErrQueuePaused
	}
	if session.active != nil {
		return ItemView{}, ErrNoEligibleItem
	}
	item, _, err := c.nextItemLocked(session)
	if err != nil {
		return ItemView{}, err
	}
	return item.View(), nil
}

// ClaimNext exclusively leases the next eligible item in a session.
func (c *Coordinator) ClaimNext(sessionKey string) (TransferLease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return TransferLease{}, err
	}
	if session.active != nil {
		return TransferLease{}, ErrNoEligibleItem
	}
	if session.paused {
		return TransferLease{}, ErrQueuePaused
	}
	item, batch, err := c.nextItemLocked(session)
	if err != nil {
		return TransferLease{}, err
	}
	leaseID, err := NewOpaqueID()
	if err != nil {
		return TransferLease{}, err
	}
	item.State = ItemStateTransferring
	batch.batch.State = BatchStateTransferring
	if batch.batch.StartedAt.IsZero() {
		batch.batch.StartedAt = c.now()
	}
	lease := TransferLease{
		LeaseID: leaseID, BatchID: batch.batch.ID, ItemID: item.ID,
		Sequence: item.Sequence, DeclaredSize: item.DeclaredSize, Item: item.View(),
	}
	session.active = &activeLease{TransferLease: lease}
	return lease, nil
}

// RegisterCancellation attaches the future stream's cancellation function to
// a live lease. It is safe to call after ClaimNext returns.
func (c *Coordinator) RegisterCancellation(sessionKey string, lease TransferLease, cancel context.CancelFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return err
	}
	if !sameLease(session.active, lease) {
		return ErrLeaseInvalid
	}
	session.active.cancel = cancel
	return nil
}

// UpdateItemProgress records an absolute byte count. Repeating the same count
// is idempotent; regressions and counts beyond the declaration are rejected.
func (c *Coordinator) UpdateItemProgress(sessionKey string, lease TransferLease, transferred int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return err
	}
	if !sameLease(session.active, lease) {
		return ErrLeaseInvalid
	}
	item, batch, err := c.itemForLeaseLocked(lease)
	if err != nil {
		return err
	}
	if transferred < item.Transferred || transferred < 0 || transferred > item.DeclaredSize {
		return ErrProgressInvalid
	}
	delta := transferred - item.Transferred
	item.Transferred = transferred
	batch.batch.TransferredBytes += delta
	return nil
}

// CompleteItem finalizes a successful lease. A stream must have reported the
// complete declared size first.
func (c *Coordinator) CompleteItem(sessionKey string, lease TransferLease, sha256 string) (BatchView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return BatchView{}, err
	}
	if !sameLease(session.active, lease) {
		return BatchView{}, ErrLeaseInvalid
	}
	item, batch, err := c.itemForLeaseLocked(lease)
	if err != nil {
		return BatchView{}, err
	}
	if item.Transferred != item.DeclaredSize {
		return BatchView{}, ErrProgressInvalid
	}
	item.State = ItemStateCompleted
	item.SHA256 = sha256
	session.active = nil
	if allItemsCompleted(batch.batch.Items) {
		batch.batch.State = BatchStateCompleted
		batch.batch.CompletedAt = c.now()
	} else {
		batch.batch.State = BatchStateQueued
	}
	return batch.batch.View(), nil
}

// FailItem records a failed active attempt and pauses the entire session.
func (c *Coordinator) FailItem(sessionKey string, lease TransferLease, transferErr error) (BatchView, error) {
	if transferErr == nil {
		transferErr = errors.New("transfer: item failed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return BatchView{}, err
	}
	if !sameLease(session.active, lease) {
		return BatchView{}, ErrLeaseInvalid
	}
	item, batch, err := c.itemForLeaseLocked(lease)
	if err != nil {
		return BatchView{}, err
	}
	item.State = ItemStateFailed
	item.Error = transferErr.Error()
	session.active = nil
	session.paused = true
	batch.batch.State = BatchStateFailed
	batch.batch.Error = item.Error
	return batch.batch.View(), nil
}

// CancelItem cancels a queued or active item. The active cancellation callback
// is deliberately invoked after releasing the coordinator mutex.
func (c *Coordinator) CancelItem(sessionKey, batchID, itemID string) error {
	var cancel context.CancelFunc
	c.mu.Lock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err == nil {
		batch, batchErr := c.ownedBatchLocked(sessionKey, batchID)
		if batchErr != nil {
			err = batchErr
		} else if item := findItem(&batch.batch, itemID); item == nil {
			err = ErrItemNotFound
		} else if item.State == ItemStateCompleted || item.State == ItemStateRejected || item.State == ItemStateCanceled {
			err = ErrInvalidTransition
		} else {
			if session.active != nil && session.active.ItemID == itemID {
				cancel = session.active.cancel
				session.active = nil
			}
			if item.Transferred > 0 {
				batch.batch.TransferredBytes -= item.Transferred
				item.Transferred = 0
			}
			item.State = ItemStateCanceled
			item.Error = "canceled"
			if session.paused {
				session.paused = false
			}
			c.recomputeBatchTerminalStateLocked(batch)
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return err
}

// CancelBatch cancels all unfinished items in a batch.
func (c *Coordinator) CancelBatch(sessionKey, batchID string) (BatchView, error) {
	return c.endBatch(sessionKey, batchID, BatchStateCanceled, "", ItemStateCanceled)
}

// RetryItem resets a failed item and places it ahead of all later work in its
// batch. Retrying is the explicit action that resumes a paused session queue.
func (c *Coordinator) RetryItem(sessionKey, batchID, itemID string) (BatchView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return BatchView{}, err
	}
	batch, err := c.ownedBatchLocked(sessionKey, batchID)
	if err != nil {
		return BatchView{}, err
	}
	item := findItem(&batch.batch, itemID)
	if item == nil {
		return BatchView{}, ErrItemNotFound
	}
	if item.State != ItemStateFailed || session.active != nil {
		return BatchView{}, ErrInvalidTransition
	}
	batch.batch.TransferredBytes -= item.Transferred
	item.Transferred = 0
	item.State = ItemStateQueued
	item.Error = ""
	batch.batch.Error = ""
	batch.batch.State = BatchStateQueued
	session.paused = false
	return batch.batch.View(), nil
}

// GetBatch returns a deep, phone-safe snapshot for the owning session.
func (c *Coordinator) GetBatch(sessionKey, batchID string) (BatchView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	batch, err := c.ownedBatchLocked(sessionKey, batchID)
	if err != nil {
		return BatchView{}, err
	}
	return batch.batch.View(), nil
}

// ListBatches returns snapshots in registration order for one session.
func (c *Coordinator) ListBatches(sessionKey string) ([]BatchView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return nil, err
	}
	out := make([]BatchView, 0, len(session.batches))
	for _, id := range session.batches {
		if batch, ok := c.batches[id]; ok {
			out = append(out, batch.batch.View())
		}
	}
	return out, nil
}

// QueueStatus returns queue state and immutable batch snapshots.
func (c *Coordinator) QueueStatus(sessionKey string) (QueueSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return QueueSnapshot{}, err
	}
	out := QueueSnapshot{Closed: session.closed, Paused: session.paused}
	if session.active != nil {
		out.ActiveLeaseID = session.active.LeaseID
	}
	for _, id := range session.batches {
		if batch, ok := c.batches[id]; ok {
			out.Batches = append(out.Batches, batch.batch.View())
		}
	}
	return out, nil
}

// CloseSession cancels and closes a session. It is idempotent and prevents
// later claims or retries. The active cancellation callback runs unlocked.
func (c *Coordinator) CloseSession(sessionKey string) error {
	var cancel context.CancelFunc
	c.mu.Lock()
	session, ok := c.sessions[sessionKey]
	if !ok {
		c.mu.Unlock()
		return ErrSessionClosed
	}
	if session.closed {
		c.mu.Unlock()
		return nil
	}
	session.closed = true
	if session.active != nil {
		cancel = session.active.cancel
		session.active = nil
	}
	for _, id := range session.batches {
		if batch := c.batches[id]; batch != nil {
			for i := range batch.batch.Items {
				item := &batch.batch.Items[i]
				if item.State != ItemStateCompleted && item.State != ItemStateRejected && item.State != ItemStateCanceled {
					if item.Transferred > 0 {
						batch.batch.TransferredBytes -= item.Transferred
						item.Transferred = 0
					}
					item.State = ItemStateCanceled
					item.Error = "session closed"
				}
			}
			if batch.batch.State != BatchStateCompleted && batch.batch.State != BatchStateRejected {
				batch.batch.State = BatchStateCanceled
				batch.batch.Error = "session closed"
			}
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// Close stops the coordinator and closes every active session.
func (c *Coordinator) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	var cancels []context.CancelFunc
	for _, session := range c.sessions {
		if session.closed {
			continue
		}
		session.closed = true
		if session.active != nil && session.active.cancel != nil {
			cancels = append(cancels, session.active.cancel)
		}
		session.active = nil
		for _, id := range session.batches {
			if batch := c.batches[id]; batch != nil {
				for i := range batch.batch.Items {
					item := &batch.batch.Items[i]
					if item.State != ItemStateCompleted && item.State != ItemStateRejected && item.State != ItemStateCanceled {
						batch.batch.TransferredBytes -= item.Transferred
						item.State = ItemStateCanceled
						item.Transferred = 0
						item.Error = "coordinator closed"
					}
				}
				if batch.batch.State != BatchStateCompleted && batch.batch.State != BatchStateRejected {
					batch.batch.State = BatchStateCanceled
					batch.batch.Error = "coordinator closed"
				}
			}
		}
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (c *Coordinator) endBatch(sessionKey, batchID string, state BatchState, approval ApprovalState, itemState ItemState) (BatchView, error) {
	var cancel context.CancelFunc
	var view BatchView
	c.mu.Lock()
	batch, err := c.ownedBatchLocked(sessionKey, batchID)
	if err == nil {
		if batch.batch.State == BatchStateCompleted || batch.batch.State == BatchStateRejected || batch.batch.State == BatchStateCanceled {
			err = ErrInvalidTransition
		} else {
			session := c.sessions[sessionKey]
			for i := range batch.batch.Items {
				item := &batch.batch.Items[i]
				if session.active != nil && session.active.ItemID == item.ID {
					cancel = session.active.cancel
					session.active = nil
				}
				if item.State != ItemStateCompleted {
					if item.Transferred > 0 {
						batch.batch.TransferredBytes -= item.Transferred
						item.Transferred = 0
					}
					item.State = itemState
					item.Error = string(state)
				}
			}
			batch.batch.State = state
			if approval != "" {
				batch.batch.ApprovalState = approval
			}
			batch.batch.Error = string(state)
			if session.paused {
				session.paused = false
			}
			view = batch.batch.View()
		}
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err != nil {
		return BatchView{}, err
	}
	return view, nil
}

func (c *Coordinator) sessionLocked(sessionKey string) *coordinatorSession {
	session := c.sessions[sessionKey]
	if session == nil {
		session = &coordinatorSession{}
		c.sessions[sessionKey] = session
	}
	return session
}

func (c *Coordinator) sessionForOperationLocked(sessionKey string) (*coordinatorSession, error) {
	if c.closed {
		return nil, ErrCoordinatorClosed
	}
	session := c.sessions[sessionKey]
	if session == nil {
		return nil, ErrWrongSession
	}
	if session.closed {
		return nil, ErrSessionClosed
	}
	return session, nil
}

func (c *Coordinator) ownedBatchLocked(sessionKey, batchID string) (*coordinatorBatch, error) {
	session, err := c.sessionForOperationLocked(sessionKey)
	if err != nil {
		return nil, err
	}
	batch, ok := c.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	if batch.batch.SessionKey != sessionKey {
		return nil, ErrBatchNotFound
	}
	if session == nil {
		return nil, ErrWrongSession
	}
	return batch, nil
}

func (c *Coordinator) nextItemLocked(session *coordinatorSession) (*TransferItem, *coordinatorBatch, error) {
	for _, batchID := range session.batches {
		batch := c.batches[batchID]
		if batch == nil {
			continue
		}
		if batch.batch.State == BatchStatePending || batch.batch.State == BatchStateRejected || batch.batch.State == BatchStateCanceled || batch.batch.State == BatchStateCompleted {
			continue
		}
		for i := range batch.batch.Items {
			item := &batch.batch.Items[i]
			switch item.State {
			case ItemStateQueued:
				return item, batch, nil
			case ItemStateFailed:
				return nil, nil, ErrQueuePaused
			case ItemStatePending, ItemStateApproved:
				return nil, nil, ErrNoEligibleItem
			}
		}
	}
	return nil, nil, ErrNoEligibleItem
}

func (c *Coordinator) itemForLeaseLocked(lease TransferLease) (*TransferItem, *coordinatorBatch, error) {
	batch := c.batches[lease.BatchID]
	if batch == nil || batch.batch.SessionKey == "" {
		return nil, nil, ErrLeaseInvalid
	}
	item := findItem(&batch.batch, lease.ItemID)
	if item == nil {
		return nil, nil, ErrLeaseInvalid
	}
	if item.State != ItemStateTransferring {
		return nil, nil, ErrLeaseInvalid
	}
	return item, batch, nil
}

func findItem(batch *TransferBatch, itemID string) *TransferItem {
	for i := range batch.Items {
		if batch.Items[i].ID == itemID {
			return &batch.Items[i]
		}
	}
	return nil
}

func sameLease(active *activeLease, lease TransferLease) bool {
	return active != nil && active.LeaseID == lease.LeaseID &&
		active.BatchID == lease.BatchID && active.ItemID == lease.ItemID
}

func allItemsCompleted(items []TransferItem) bool {
	for _, item := range items {
		if item.State != ItemStateCompleted {
			return false
		}
	}
	return true
}

func (c *Coordinator) recomputeBatchTerminalStateLocked(batch *coordinatorBatch) {
	allTerminal := true
	for _, item := range batch.batch.Items {
		if item.State != ItemStateCompleted && item.State != ItemStateCanceled && item.State != ItemStateRejected {
			allTerminal = false
			break
		}
	}
	if allTerminal {
		batch.batch.State = BatchStateCanceled
	} else {
		batch.batch.State = BatchStateQueued
	}
}
