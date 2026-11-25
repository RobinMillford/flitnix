package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		_ = store.Close()
		s.store = nil
		return fmt.Errorf("server: recovery phase 4 (sessions): %w", err)
	}

	// Phase 5.
	if err := installCoordinatorRecovery(store, svcs); err != nil {
		_ = store.Close()
		s.store = nil
		return fmt.Errorf("server: recovery phase 5 (coordinator): %w", err)
	}

	// Phases 6-10: resumable staging recovery, cross-validation, quarantine, and cleanup.
	stagingTTL := cfg.StagingTTL
	if stagingTTL <= 0 {
		stagingTTL = transfer.DefaultStagingTTL
	}
	if err := recoverResumableStaging(cfg.RecoveryDir, store, svcs, stagingTTL); err != nil {
		_ = store.Close()
		s.store = nil
		return fmt.Errorf("server: recovery phase 6-10 (staging): %w", err)
	}

	return nil
}

// Store exposes the opened durable store (nil when recovery is disabled).
func (s *Server) Store() *recovery.Store { return s.store }

// CloseRecovery detaches durable persistence hooks and closes the recovery
// store, releasing the single-instance lock cleanly when Serve finishes.
func (s *Server) CloseRecovery() error {
	svcs := s.cfg.Services
	if svcs != nil {
		if svcs.Sessions != nil {
			svcs.Sessions.SetCreateHook(nil)
			svcs.Sessions.SetRevokeHook(nil)
		}
		if svcs.Coordinator != nil {
			svcs.Coordinator.SetPersistHook(nil)
		}
	}
	if s.store == nil {
		return nil
	}
	err := s.store.Close()
	s.store = nil
	return err
}

// installSessionRecovery restores sessions into the in-memory manager and
// wires durable creation, revocation and expiry. The verifier is installed
// before any record is restored so every live session uses the same
// installation-keyed derivation as the records it was restored from.
func installSessionRecovery(store *recovery.Store, svcs *Services) error {
	if svcs.Sessions == nil {
		return nil
	}
	if svcs.Sessions.Len() == 0 {
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
		b := batchFromRecord(record)
		if !svcs.Coordinator.HasBatch(b.ID) {
			if err := svcs.Coordinator.RestoreBatch(b); err != nil {
				return fmt.Errorf("restore batch %s: %w", record.ID, err)
			}
		}
		if svcs.Transfers != nil && svcs.Transfers.Quota() != nil &&
			b.Direction == transfer.DirectionPhoneToDesktop &&
			b.ApprovalState == transfer.ApprovalApproved &&
			b.State != transfer.BatchStateCompleted &&
			b.State != transfer.BatchStateCanceled &&
			b.State != transfer.BatchStateRejected &&
			b.State != transfer.BatchStateFailed {
			targetDir := ""
			if svcs.Transfers.Store() != nil {
				targetDir = svcs.Transfers.Store().Dir()
			}
			for _, item := range b.Items {
				if item.State == transfer.ItemStateCompleted || item.State == transfer.ItemStateCanceled ||
					item.State == transfer.ItemStateRejected || item.State == transfer.ItemStateFailed ||
					item.State == transfer.ItemStateDiscarded {
					continue
				}
				resID := item.ContentID
				if resID == "" {
					resID = item.ID
				}
				svcs.Transfers.Quota().RestoreReservation(resID, b.SessionKey, targetDir, item.DeclaredSize, item.Transferred)
			}
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

// recoverResumableStaging implements startup recovery Phases 6-10:
//   - 6: Installs the HMAC metadata subkey and scans resumable staging artifacts.
//   - 7-9: Cross-validates .meta sidecars and .part files against durable sessions
//     and coordinator batches, truncating torn tails and quarantining corrupt
//     uploads individually instead of failing global startup.
//   - 10: Reconstructs upload records + storage reservations and cleans up
//     expired or orphaned staging artifacts.
func recoverResumableStaging(recoveryDir string, store *recovery.Store, svcs *Services, stagingTTL time.Duration) error {
	if svcs == nil || svcs.Transfers == nil || svcs.Transfers.Store() == nil {
		return nil
	}
	tStore := svcs.Transfers.Store()
	if err := tStore.SetMetaKey(store.DeriveSubkey("staging-meta-v1")); err != nil {
		return err
	}

	quarantineDir := ""
	if recoveryDir != "" {
		quarantineDir = filepath.Join(recoveryDir, "quarantine")
	}

	artifacts, orphanParts, scanErr := tStore.ScanResumableMeta()
	if scanErr != nil {
		return scanErr
	}

	activeSessions := make(map[string]bool)
	if svcs.Sessions != nil {
		for _, sess := range svcs.Sessions.ActiveSessions() {
			activeSessions[sess.Key()] = true
		}
	}

	coordSnap := transfer.CoordinatorState{}
	if svcs.Coordinator != nil {
		coordSnap = svcs.Coordinator.Snapshot()
	}

	type itemRef struct {
		batch transfer.TransferBatch
		item  transfer.TransferItem
	}
	itemsByID := make(map[string]itemRef)
	itemsByContentID := make(map[string]itemRef)
	for _, b := range coordSnap.Batches {
		for _, it := range b.Items {
			ref := itemRef{batch: b, item: it}
			itemsByID[b.ID+":"+it.ID] = ref
			if it.ContentID != "" {
				itemsByContentID[it.ContentID] = ref
			}
		}
	}

	validMetaByContentID := make(map[string]transfer.StagedArtifact)
	quarantinedContentIDs := make(map[string]string)

	for _, art := range artifacts {
		if art.Err != nil {
			_ = transfer.QuarantineFiles(quarantineDir, art.PartPath, art.MetaPath, art.Err.Error())
			continue
		}
		meta := art.Meta
		if !activeSessions[meta.SessionKey] {
			// Phase 10: session revoked or expired -> clean up staging files.
			_ = os.Remove(art.PartPath)
			_ = os.Remove(art.MetaPath)
			if svcs.Transfers.Quota() != nil {
				svcs.Transfers.Quota().Release(meta.UploadID)
			}
			continue
		}
		if stagingTTL > 0 && !meta.UpdatedAt.IsZero() && time.Since(meta.UpdatedAt) > stagingTTL {
			_ = os.Remove(art.PartPath)
			_ = os.Remove(art.MetaPath)
			quarantinedContentIDs[meta.UploadID] = "staging retention expired"
			if svcs.Transfers.Quota() != nil {
				svcs.Transfers.Quota().Release(meta.UploadID)
			}
			continue
		}

		// Cross-validate against Coordinator record.
		ref, found := itemsByID[meta.BatchID+":"+meta.ItemID]
		if !found || ref.batch.SessionKey != meta.SessionKey ||
			ref.item.ContentID != meta.UploadID ||
			ref.item.DeclaredSize != meta.DeclaredSize ||
			ref.batch.Direction != transfer.DirectionPhoneToDesktop {
			_ = transfer.QuarantineFiles(quarantineDir, art.PartPath, art.MetaPath, "staging metadata mismatch against durable journal")
			quarantinedContentIDs[meta.UploadID] = "staging metadata mismatch"
			if svcs.Transfers.Quota() != nil {
				svcs.Transfers.Quota().Release(meta.UploadID)
			}
			continue
		}

		// If the batch/item already ended cleanly in the journal, remove stale staging.
		switch ref.item.State {
		case transfer.ItemStateCompleted, transfer.ItemStateCanceled, transfer.ItemStateRejected, transfer.ItemStateFailed, transfer.ItemStateDiscarded:
			_ = os.Remove(art.PartPath)
			_ = os.Remove(art.MetaPath)
			if svcs.Transfers.Quota() != nil {
				svcs.Transfers.Quota().Release(meta.UploadID)
			}
			continue
		}

		// Verify .part exists, is not a symlink, size >= CommittedOffset, and truncate torn tail.
		f, _, rerr := tStore.ReopenPartForAppend(art.PartPath, meta.CommittedOffset)
		if rerr != nil {
			_ = transfer.QuarantineFiles(quarantineDir, art.PartPath, art.MetaPath, rerr.Error())
			quarantinedContentIDs[meta.UploadID] = rerr.Error()
			if svcs.Transfers.Quota() != nil {
				svcs.Transfers.Quota().Release(meta.UploadID)
			}
			continue
		}
		_ = f.Close()

		validMetaByContentID[meta.UploadID] = art
	}

	// Reconstruct service upload records and reconcile coordinator + quota state.
	targetDir := tStore.Dir()
	for _, b := range coordSnap.Batches {
		if b.Direction != transfer.DirectionPhoneToDesktop || !activeSessions[b.SessionKey] {
			continue
		}
		for _, it := range b.Items {
			if it.ContentID == "" {
				continue
			}
			switch it.State {
			case transfer.ItemStateCompleted, transfer.ItemStateCanceled, transfer.ItemStateRejected, transfer.ItemStateFailed, transfer.ItemStateDiscarded:
				continue
			}
			if reason, qed := quarantinedContentIDs[it.ContentID]; qed {
				if svcs.Coordinator != nil {
					_ = svcs.Coordinator.ReconcileItemFromStaging(b.SessionKey, b.ID, it.ID, 0, false, true, reason)
				}
				_ = svcs.Transfers.RestoreApprovedUpload(transfer.RestoreUploadOpts{
					ID:          it.ContentID,
					SessionKey:  b.SessionKey,
					BatchID:     b.ID,
					ItemID:      it.ID,
					Name:        it.DisplayName,
					Size:        it.DeclaredSize,
					State:       transfer.StateFailed,
					Reason:      "quarantined: " + reason,
					BatchLinked: true,
					Quarantined: true,
					CreatedAt:   b.CreatedAt,
				})
				if svcs.Transfers.Quota() != nil {
					svcs.Transfers.Quota().Release(it.ContentID)
				}
				continue
			}

			if art, ok := validMetaByContentID[it.ContentID]; ok {
				paused := it.State == transfer.ItemStatePaused
				upState := transfer.StateApproved
				if paused {
					upState = transfer.StatePaused
				} else if art.Meta.CommittedOffset > 0 {
					upState = transfer.StateInterrupted
				}
				if svcs.Coordinator != nil {
					_ = svcs.Coordinator.ReconcileItemFromStaging(b.SessionKey, b.ID, it.ID, art.Meta.CommittedOffset, paused, false, "")
				}
				_ = svcs.Transfers.RestoreApprovedUpload(transfer.RestoreUploadOpts{
					ID:              it.ContentID,
					SessionKey:      b.SessionKey,
					BatchID:         b.ID,
					ItemID:          it.ID,
					Name:            art.Meta.FinalName,
					Root:            art.Meta.RootName,
					RelativePath:    art.Meta.RelativePath,
					Size:            it.DeclaredSize,
					ExpectedSHA256:  it.SHA256,
					State:           upState,
					PartPath:        art.PartPath,
					MetaPath:        art.MetaPath,
					FinalDir:        filepath.Dir(art.PartPath),
					CommittedOffset: art.Meta.CommittedOffset,
					ChunkSize:       art.Meta.ChunkSize,
					SHA256State:     art.Meta.SHA256State,
					CreatedAt:       b.CreatedAt,
					BatchLinked:     true,
				})
				if svcs.Transfers.Quota() != nil && b.ApprovalState == transfer.ApprovalApproved {
					svcs.Transfers.Quota().RestoreReservation(it.ContentID, b.SessionKey, targetDir, it.DeclaredSize, art.Meta.CommittedOffset)
				}
				continue
			}

			// Approved/pending item without .meta yet (offset 0).
			var root, relPath string
			if b.Kind == transfer.BatchKindFolder {
				root = b.RootName
				relPath = it.RelativePath
			}
			upState := transfer.StatePending
			if it.State == transfer.ItemStatePaused {
				upState = transfer.StatePaused
			} else if b.ApprovalState == transfer.ApprovalApproved {
				upState = transfer.StateApproved
			}
			if svcs.Coordinator != nil && it.State == transfer.ItemStateInterrupted {
				_ = svcs.Coordinator.ReconcileItemFromStaging(b.SessionKey, b.ID, it.ID, 0, false, false, "")
			}
			_ = svcs.Transfers.RestoreApprovedUpload(transfer.RestoreUploadOpts{
				ID:             it.ContentID,
				SessionKey:     b.SessionKey,
				BatchID:        b.ID,
				ItemID:         it.ID,
				Name:           it.DisplayName,
				Root:           root,
				RelativePath:   relPath,
				Size:           it.DeclaredSize,
				ExpectedSHA256: it.SHA256,
				State:          upState,
				CreatedAt:      b.CreatedAt,
				BatchLinked:    true,
			})
		}
	}

	// Phase 10: clean up orphan .part files with no matching valid .meta sidecar.
	for _, p := range orphanParts {
		_ = os.Remove(p)
	}
	return nil
}
