package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/itsyamin/flitnix/internal/recovery"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// Recover runs the mandatory startup recovery phases in the order the
// approved architecture requires. It is idempotent and every failure is
// returned: there is no ignored recovery error on this path.
//
// The listener must not bind, and Ready must not close, until it returns nil.
//
//  1. Single-instance ownership of the state directory.
//  2. Protected state directory and installation key.
//  3. Authenticated journal replay.
//  4. Session restoration.
//  5. Coordinator restoration.
//  6. Resumable staging recovery (including publication replay and quota
//     reconstruction).
//     7-9. Cross-validation of durable identity against staging, with per-upload
//     quarantine instead of global refusal.
//  10. Expiry and cleanup.
//
// Global failures — lock ownership, key integrity, journal authentication,
// schema or sequence corruption, an unrecoverable storage error — return an
// error and therefore prevent readiness. A single corrupt upload never takes
// the application down; it is quarantined, cannot be published, and never
// reports successful recovery.
func (s *Server) Recover() error {
	s.recoverOne.Do(func() { s.recoverErr = s.recover() })
	return s.recoverErr
}

func (s *Server) recover() error {
	cfg := s.cfg
	if cfg.RecoveryDir == "" || cfg.Services == nil {
		return nil
	}
	svcs := cfg.Services

	// Phases 1-3.
	store, err := recovery.Open(cfg.RecoveryDir)
	if err != nil {
		return fmt.Errorf("server: recovery phase 1-3 (ownership, key, journal): %w", err)
	}
	s.store = store

	// Phase 4.
	if err := installSessionRecovery(store, svcs); err != nil {
		return fmt.Errorf("server: recovery phase 4 (sessions): %w", err)
	}

	// Phase 5.
	if err := installCoordinatorRecovery(store, svcs); err != nil {
		return fmt.Errorf("server: recovery phase 5 (coordinator): %w", err)
	}

	rm := svcs.Resumable
	if rm == nil {
		rm = svcs.EnsureResumable()
	}
	if rm == nil {
		return nil
	}

	// Phase 6.
	if err := rm.RecoverOnStartup(); err != nil {
		return fmt.Errorf("server: recovery phase 6 (resumable staging): %w", err)
	}

	// Phases 7-9. RecoverOnStartup has already replayed publication records and
	// rebuilt quota reservations from staging; authorization below decides
	// which of those uploads may stay registered, and quarantine releases the
	// reservation of every upload it removes.
	state := store.State()
	if _, err := rm.Reconcile(func(meta transfer.ResumableMeta) error {
		return authorizeRestoredUpload(state, meta)
	}); err != nil {
		return fmt.Errorf("server: recovery phase 7-9 (reconciliation): %w", err)
	}

	// Phase 10.
	rm.CleanupExpired()
	return nil
}

// Store exposes the opened durable store (nil when recovery is disabled).
func (s *Server) Store() *recovery.Store { return s.store }

// authorizeRestoredUpload is the trusted-ledger join for one restored upload.
//
// It never infers ownership from a filename, size, session key or
// client-supplied transfer ID. Everything must be provable from the durable
// journal: the batch record, its owner, its approval, the item record, the
// declared size, and a still-active owning session.
func authorizeRestoredUpload(state recovery.State, meta transfer.ResumableMeta) error {
	if meta.SessionKey == "" {
		return errors.New("upload has no recorded session owner")
	}
	if meta.BatchID == "" || meta.ItemID == "" {
		return errors.New("legacy upload has no durable batch/item association")
	}
	var batch *recovery.BatchRecord
	for i := range state.Batches {
		if state.Batches[i].ID == meta.BatchID {
			batch = &state.Batches[i]
			break
		}
	}
	if batch == nil {
		return fmt.Errorf("batch %s has no durable record", meta.BatchID)
	}
	if batch.SessionKey != meta.SessionKey {
		return errors.New("batch owner does not match the upload owner")
	}
	if batch.ApprovalState != string(transfer.ApprovalApproved) {
		return fmt.Errorf("batch is not approved (%s)", batch.ApprovalState)
	}
	var item *recovery.ItemRecord
	for i := range batch.Items {
		if batch.Items[i].ID == meta.ItemID {
			item = &batch.Items[i]
			break
		}
	}
	if item == nil {
		return fmt.Errorf("item %s has no durable record", meta.ItemID)
	}
	if item.BatchID != "" && item.BatchID != batch.ID {
		return errors.New("item belongs to a different batch")
	}
	if item.DeclaredSize != meta.TotalSize {
		return fmt.Errorf("upload size %d does not match the durable item size %d", meta.TotalSize, item.DeclaredSize)
	}
	if item.RelativePath != "" && meta.RelativePath != "" && item.RelativePath != meta.RelativePath {
		return errors.New("upload path does not match the durable item")
	}
	if !activeSession(state, meta.SessionKey) {
		return errors.New("owning session is not active")
	}
	return nil
}

// activeSession reports whether the durable session record is active and
// unexpired. Revocation and expiry are durable, so a revoked or expired owner
// can never continue an upload after a restart.
func activeSession(state recovery.State, key string) bool {
	for _, rec := range state.Sessions {
		if rec.SessionKey != key {
			continue
		}
		if rec.Status != string(session.StatusActive) {
			return false
		}
		return rec.ExpiresAt.IsZero() || rec.ExpiresAt.After(time.Now())
	}
	return false
}

// installSessionRecovery restores sessions into the in-memory manager and
// wires durable creation, revocation and expiry. The verifier is installed
// before any record is restored so every live session uses the same
// installation-keyed derivation as the records it was restored from.
func installSessionRecovery(store *recovery.Store, svcs *Services) error {
	if svcs.Sessions == nil {
		return nil
	}
	if err := svcs.Sessions.SetVerifier(func(token string) [sha256.Size]byte {
		raw, err := hex.DecodeString(store.Verifier(token))
		if err != nil || len(raw) != sha256.Size {
			return [sha256.Size]byte{}
		}
		var out [sha256.Size]byte
		copy(out[:], raw)
		return out
	}); err != nil {
		return err
	}

	for _, record := range store.State().Sessions {
		verifier, err := hex.DecodeString(record.TokenVerifier)
		if err != nil || len(verifier) != sha256.Size {
			return fmt.Errorf("session %s has an invalid token verifier", record.SessionID)
		}
		var v [sha256.Size]byte
		copy(v[:], verifier)
		restored := session.SessionRecord{
			SessionID:  record.SessionID,
			SessionKey: record.SessionKey,
			Verifier:   v,
			Generation: record.Generation,
			Status:     session.Status(record.Status),
			CreatedAt:  record.CreatedAt,
			ExpiresAt:  record.ExpiresAt,
		}
		if record.RevokedAt != nil {
			restored.RevokedAt = *record.RevokedAt
		}
		if err := svcs.Sessions.Restore(restored); err != nil {
			return err
		}
	}

	svcs.Sessions.SetCreateHook(func(record session.SessionRecord) error {
		return store.Append(recovery.EventSessionCreated, durableSession(record))
	})
	svcs.Sessions.SetRevokeHook(func(record session.SessionRecord) error {
		kind := recovery.EventSessionRevoked
		if record.Status == session.StatusExpired {
			kind = recovery.EventSessionExpired
		}
		return store.Append(kind, durableSession(record))
	})
	return nil
}

func durableSession(record session.SessionRecord) recovery.SessionRecord {
	out := recovery.SessionRecord{
		SessionID:     record.SessionID,
		SessionKey:    record.SessionKey,
		TokenVerifier: hex.EncodeToString(record.Verifier[:]),
		KeyID:         recovery.KeyID,
		Generation:    record.Generation,
		Status:        string(record.Status),
		CreatedAt:     record.CreatedAt,
		ExpiresAt:     record.ExpiresAt,
	}
	if !record.RevokedAt.IsZero() {
		revoked := record.RevokedAt
		out.RevokedAt = &revoked
	}
	return out
}

// installCoordinatorRecovery restores batches with their original IDs,
// ownership and approval, then wires the durable lifecycle sink.
//
// The sink propagates every error to the coordinator operation that required
// durability. Nothing here discards a journal write failure.
func installCoordinatorRecovery(store *recovery.Store, svcs *Services) error {
	if svcs.Coordinator == nil {
		return nil
	}
	for _, record := range store.State().Batches {
		if err := svcs.Coordinator.RestoreBatch(batchFromRecord(record)); err != nil {
			return fmt.Errorf("restore batch %s: %w", record.ID, err)
		}
	}
	svcs.Coordinator.SetPersistHook(func(ev transfer.LifecycleEvent) error {
		switch ev.Kind {
		case transfer.LifecycleBatchDeclared:
			if ev.Batch == nil {
				return errors.New("batch declaration without a batch record")
			}
			return store.Append(recovery.EventBatchDeclared, batchRecordFrom(*ev.Batch))
		case transfer.LifecycleApprovalChanged:
			return store.Append(recovery.EventApprovalChanged, recovery.ApprovalChange{
				BatchID:       ev.BatchID,
				ApprovalState: string(ev.ApprovalState),
				BatchState:    string(ev.BatchState),
			})
		case transfer.LifecycleItemTransition:
			if ev.Item == nil {
				return errors.New("item transition without an item record")
			}
			if err := store.Append(recovery.EventItemTransition, recovery.ItemChange{
				BatchID: ev.BatchID,
				Item:    itemRecordFrom(*ev.Item),
			}); err != nil {
				return err
			}
			if ev.BatchState == "" {
				return nil
			}
			return store.Append(recovery.EventBatchStateChanged, recovery.BatchStateChange{
				BatchID:          ev.BatchID,
				State:            string(ev.BatchState),
				Error:            ev.Error,
				CompletedAt:      ev.CompletedAt,
				TransferredBytes: ev.Transferred,
			})
		case transfer.LifecycleUploadAllocated:
			return store.Append(recovery.EventUploadAllocated, recovery.Allocation{
				BatchID:   ev.BatchID,
				ItemID:    ev.ItemID,
				ContentID: ev.ContentID,
			})
		case transfer.LifecycleBatchStateChange:
			return store.Append(recovery.EventBatchStateChanged, recovery.BatchStateChange{
				BatchID:          ev.BatchID,
				State:            string(ev.BatchState),
				Error:            ev.Error,
				CompletedAt:      ev.CompletedAt,
				TransferredBytes: ev.Transferred,
			})
		case transfer.LifecycleSessionClosed:
			closed := recovery.SessionClose{ClosedAt: time.Now().UTC()}
			for _, batch := range ev.Batches {
				closed.Batches = append(closed.Batches, batchRecordFrom(batch))
			}
			return store.Append(recovery.EventSessionClosed, closed)
		default:
			return fmt.Errorf("unknown lifecycle event %q", ev.Kind)
		}
	})
	return nil
}

func batchFromRecord(record recovery.BatchRecord) transfer.TransferBatch {
	batch := transfer.TransferBatch{
		ID:                 record.ID,
		SessionKey:         record.SessionKey,
		Direction:          transfer.BatchDirection(record.Direction),
		Kind:               transfer.BatchKind(record.Kind),
		RootName:           record.RootName,
		TotalItemCount:     record.TotalItemCount,
		DeclaredTotalBytes: record.DeclaredTotalBytes,
		TransferredBytes:   record.TransferredBytes,
		State:              transfer.BatchState(record.State),
		ApprovalState:      transfer.ApprovalState(record.ApprovalState),
		CreatedAt:          record.CreatedAt,
		StartedAt:          record.StartedAt,
		CompletedAt:        record.CompletedAt,
		Error:              record.Error,
	}
	if len(record.Items) > 0 {
		batch.Items = make([]transfer.TransferItem, 0, len(record.Items))
		for _, item := range record.Items {
			batch.Items = append(batch.Items, transfer.TransferItem{
				ID:           item.ID,
				BatchID:      item.BatchID,
				Sequence:     item.Sequence,
				RelativePath: item.RelativePath,
				DisplayName:  item.DisplayName,
				DeclaredSize: item.DeclaredSize,
				State:        transfer.ItemState(item.State),
				SHA256:       item.SHA256,
				ContentID:    item.ContentID,
				Error:        item.Error,
				Transferred:  item.Transferred,
			})
		}
	}
	return batch
}

func batchRecordFrom(batch transfer.TransferBatch) recovery.BatchRecord {
	record := recovery.BatchRecord{
		ID:                 batch.ID,
		SessionKey:         batch.SessionKey,
		Direction:          string(batch.Direction),
		Kind:               string(batch.Kind),
		RootName:           batch.RootName,
		TotalItemCount:     batch.TotalItemCount,
		DeclaredTotalBytes: batch.DeclaredTotalBytes,
		TransferredBytes:   batch.TransferredBytes,
		State:              string(batch.State),
		ApprovalState:      string(batch.ApprovalState),
		CreatedAt:          batch.CreatedAt,
		StartedAt:          batch.StartedAt,
		CompletedAt:        batch.CompletedAt,
		Error:              batch.Error,
	}
	record.Items = make([]recovery.ItemRecord, 0, len(batch.Items))
	for _, item := range batch.Items {
		record.Items = append(record.Items, itemRecordFrom(item))
	}
	return record
}

func itemRecordFrom(item transfer.TransferItem) recovery.ItemRecord {
	return recovery.ItemRecord{
		ID:           item.ID,
		BatchID:      item.BatchID,
		Sequence:     item.Sequence,
		RelativePath: item.RelativePath,
		DisplayName:  item.DisplayName,
		DeclaredSize: item.DeclaredSize,
		State:        string(item.State),
		SHA256:       item.SHA256,
		ContentID:    item.ContentID,
		Error:        item.Error,
		Transferred:  item.Transferred,
	}
}
