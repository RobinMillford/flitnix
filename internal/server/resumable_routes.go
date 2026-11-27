package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

func setResumableHeaders(w http.ResponseWriter, offset, total, chunkSize int64, state string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
	if total >= 0 {
		w.Header().Set("Upload-Length", strconv.FormatInt(total, 10))
	}
	if chunkSize > 0 {
		w.Header().Set("Upload-Chunk-Size", strconv.FormatInt(chunkSize, 10))
	}
	if state != "" {
		w.Header().Set("X-Upload-State", state)
	}
}

func parseUploadOffsetHeader(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.Header.Get("Upload-Offset"))
	if raw == "" {
		return 0, errors.New("missing Upload-Offset header")
	}
	val, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || val < 0 {
		return 0, errors.New("invalid Upload-Offset header")
	}
	return val, nil
}

// handleUploadHead serves HEAD /api/uploads/{id}/content for authoritative
// resumable offset discovery on standalone uploads.
func handleUploadHead(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		id := r.PathValue("id")
		head, err := svcs.Transfers.HeadUpload(id, sess.Key())
		if err != nil {
			if errors.Is(err, transfer.ErrUploadNotFound) || errors.Is(err, transfer.ErrWrongSession) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		setResumableHeaders(w, head.CommittedOffset, head.DeclaredSize, head.ChunkSize, string(head.State))
		if head.Quarantined {
			w.Header().Set("X-Upload-Quarantined", "true")
			w.WriteHeader(http.StatusConflict)
			return
		}
		switch head.State {
		case transfer.StatePending, transfer.StatePaused, transfer.StateCanceled,
			transfer.StateRejected, transfer.StateFailed, transfer.StateDiscarded:
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// handleUploadPatch serves PATCH /api/uploads/{id}/content for resumable
// chunked standalone uploads.
func handleUploadPatch(svcs *Services, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		id := r.PathValue("id")
		offset, err := parseUploadOffsetHeader(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if r.ContentLength < 0 {
			writeError(w, http.StatusLengthRequired, "Content-Length header required for resumable chunk")
			return
		}
		var devID string
		if upRec, ok := svcs.Transfers.Get(id); ok {
			devID = upRec.DeviceID
			if devID != "" && svcs.Devices != nil {
				if _, exists := svcs.Devices.Get(devID); !exists {
					writeError(w, http.StatusUnauthorized, "device revoked")
					return
				}
			}
		}

		sess.Begin()
		defer sess.End()
		if svcs.Inhibitor != nil {
			pLease, pErr := svcs.Inhibitor.Acquire("Receiving chunked upload " + id)
			if pErr == nil && pLease != nil {
				defer pLease.Release()
			}
		}

		body := &stallBodyReader{
			ReadCloser: r.Body,
			rc:         http.NewResponseController(w),
			stall:      stall,
		}
		res, serr := svcs.Transfers.SubmitChunk(r.Context(), id, sess.Key(), offset, r.ContentLength, body, nil, func() error {
			if devID != "" && svcs.Devices != nil {
				if _, exists := svcs.Devices.Get(devID); !exists {
					return errors.New("device revoked mid-transfer")
				}
			}
			return nil
		})
		if serr != nil {
			writeChunkStreamError(w, r, serr, res.CommittedOffset)
			return
		}
		setResumableHeaders(w, res.CommittedOffset, -1, 0, "")
		if !res.Completed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, uploadResultJSON{Written: res.Written, SHA256: res.SHA256})
	}
}

// handleBatchHead serves HEAD /api/batches/{batchID}/items/{itemID}/content for
// authoritative resumable offset discovery before starting or resuming PATCH chunks.
func handleBatchHead(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil || svcs.Coordinator == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		batchID, itemID := r.PathValue("batchID"), r.PathValue("itemID")
		batch, err := svcs.Coordinator.GetBatch(sess.Key(), batchID)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var itemView *transfer.ItemView
		for i := range batch.Items {
			if batch.Items[i].ID == itemID {
				itemView = &batch.Items[i]
				break
			}
		}
		if itemView == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		contentID, err := svcs.Coordinator.ItemContentID(sess.Key(), batchID, itemID)
		if err != nil {
			setResumableHeaders(w, itemView.Transferred, itemView.DeclaredSize, transfer.DefaultChunkSize, string(itemView.State))
			w.WriteHeader(http.StatusConflict)
			return
		}
		head, herr := svcs.Transfers.HeadUpload(contentID, sess.Key())
		if herr != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		stateStr := string(itemView.State)
		if head.Quarantined {
			stateStr = "quarantined"
			w.Header().Set("X-Upload-Quarantined", "true")
		}
		setResumableHeaders(w, head.CommittedOffset, head.DeclaredSize, head.ChunkSize, stateStr)
		if head.Quarantined || batch.ApprovalState == transfer.ApprovalPending {
			w.WriteHeader(http.StatusConflict)
			return
		}
		switch itemView.State {
		case transfer.ItemStatePending, transfer.ItemStatePaused, transfer.ItemStateCanceled,
			transfer.ItemStateRejected, transfer.ItemStateFailed, transfer.ItemStateDiscarded:
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// handleBatchPatch serves PATCH /api/batches/{batchID}/items/{itemID}/content for
// resumable chunked uploads. Transient mid-chunk interruptions preserve durable
// committed offsets and leave sibling batch items ready to continue.
func handleBatchPatch(svcs *Services, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil || svcs.Coordinator == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		offset, err := parseUploadOffsetHeader(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if r.ContentLength < 0 {
			writeError(w, http.StatusLengthRequired, "Content-Length header required for resumable chunk")
			return
		}
		batchID, itemID := r.PathValue("batchID"), r.PathValue("itemID")

		sess.Begin()
		defer sess.End()

		lease, err := svcs.Coordinator.ClaimChunk(sess.Key(), batchID, itemID)
		if err != nil {
			if errors.Is(err, transfer.ErrUploadPaused) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "paused", "error": "upload is paused"})
				return
			}
			if errors.Is(err, transfer.ErrUploadDiscarded) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "discarded", "error": "partial upload was discarded"})
				return
			}
			writeBatchError(w, err)
			return
		}

		contentID, err := svcs.Coordinator.ItemContentID(sess.Key(), batchID, itemID)
		if err != nil {
			_, _ = svcs.Coordinator.FailBatch(sess.Key(), lease, err)
			writeBatchError(w, err)
			return
		}

		var devID string
		if upRec, ok := svcs.Transfers.Get(contentID); ok {
			devID = upRec.DeviceID
			if devID != "" && svcs.Devices != nil {
				if _, exists := svcs.Devices.Get(devID); !exists {
					head, _ := svcs.Transfers.HeadUpload(contentID, sess.Key())
					_, _ = svcs.Coordinator.InterruptItem(sess.Key(), lease, head.CommittedOffset, errors.New("device revoked"))
					writeError(w, http.StatusUnauthorized, "device revoked")
					return
				}
			}
		}

		if err := svcs.Coordinator.RegisterCancellation(sess.Key(), lease, func() { _ = svcs.Transfers.Cancel(contentID) }); err != nil {
			_ = svcs.Transfers.Cancel(contentID)
			writeBatchError(w, err)
			return
		}

		if svcs.Inhibitor != nil {
			pLease, pErr := svcs.Inhibitor.Acquire(fmt.Sprintf("Receiving chunk for item %s in batch %s", itemID, batchID))
			if pErr == nil && pLease != nil {
				defer pLease.Release()
			}
		}

		body := &stallBodyReader{
			ReadCloser: r.Body,
			rc:         http.NewResponseController(w),
			stall:      stall,
		}
		res, chunkErr := svcs.Transfers.SubmitChunk(
			r.Context(),
			contentID,
			sess.Key(),
			offset,
			r.ContentLength,
			body,
			func(up transfer.Upload) {
				_ = svcs.Coordinator.UpdateItemProgress(sess.Key(), lease, up.Uploaded)
			},
			func() error {
				if devID != "" && svcs.Devices != nil {
					if _, exists := svcs.Devices.Get(devID); !exists {
						return errors.New("device revoked mid-transfer")
					}
				}
				return svcs.Coordinator.MarkItemPublishing(sess.Key(), lease)
			},
		)

		if chunkErr != nil {
			var mm *transfer.OffsetMismatchError
			if errors.As(chunkErr, &mm) {
				_ = svcs.Coordinator.ReleaseChunkLease(sess.Key(), lease, mm.Expected)
				w.Header().Set("Upload-Offset", strconv.FormatInt(mm.Expected, 10))
				w.Header().Set("Upload-Length", strconv.FormatInt(lease.DeclaredSize, 10))
				writeJSON(w, http.StatusConflict, map[string]any{
					"code":           "offset_mismatch",
					"error":          "upload offset mismatch",
					"expectedOffset": mm.Expected,
				})
				return
			}
			if errors.Is(chunkErr, transfer.ErrCanceled) {
				_ = svcs.Coordinator.CancelItem(sess.Key(), batchID, itemID)
				writeChunkStreamError(w, r, chunkErr, res.CommittedOffset)
				return
			}
			if errors.Is(chunkErr, transfer.ErrUploadPaused) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "paused", "error": "upload paused"})
				return
			}
			if errors.Is(chunkErr, transfer.ErrUploadDiscarded) {
				writeJSON(w, http.StatusConflict, map[string]string{"code": "discarded", "error": "partial upload discarded"})
				return
			}
			if errors.Is(chunkErr, transfer.ErrSizeExceeded) || errors.Is(chunkErr, transfer.ErrDigestMismatch) || errors.Is(chunkErr, transfer.ErrStagingQuarantined) {
				failBatch(svcs, sess.Key(), lease, chunkErr)
				writeChunkStreamError(w, r, chunkErr, 0)
				return
			}
			// Transient interruption, stall, or insufficient storage: preserve durable committed bytes & siblings!
			head, herr := svcs.Transfers.HeadUpload(contentID, sess.Key())
			committed := res.CommittedOffset
			if herr == nil {
				committed = head.CommittedOffset
			}
			_, _ = svcs.Coordinator.InterruptItem(sess.Key(), lease, committed, chunkErr)
			diagnose(svcs, "batch chunk interrupted session=%s item=%s committed=%d reason=%s",
				shortKey(sess.Key()), shortID(itemID), committed, classifyStreamFailure(r, chunkErr))
			writeChunkStreamError(w, r, chunkErr, committed)
			return
		}

		if !res.Completed {
			_ = svcs.Coordinator.ReleaseChunkLease(sess.Key(), lease, res.CommittedOffset)
			setResumableHeaders(w, res.CommittedOffset, lease.DeclaredSize, transfer.DefaultChunkSize, string(transfer.ItemStateTransferring))
			w.WriteHeader(http.StatusNoContent)
			return
		}

		diagnose(svcs, "batch item done (resumable) session=%s item=%s bytes=%d",
			shortKey(sess.Key()), shortID(itemID), res.Written)
		var (
			view        transfer.BatchView
			completeErr error
		)
		if r.Header.Get("X-Flitnix-Compact-Ack") == "1" {
			view, completeErr = svcs.Coordinator.CompleteItemCompact(sess.Key(), lease, res.SHA256)
		} else {
			view, completeErr = svcs.Coordinator.CompleteItem(sess.Key(), lease, res.SHA256)
		}
		if completeErr != nil {
			writeBatchError(w, completeErr)
			return
		}
		setResumableHeaders(w, res.CommittedOffset, lease.DeclaredSize, transfer.DefaultChunkSize, string(transfer.ItemStateCompleted))
		writeJSON(w, http.StatusOK, struct {
			Written int64              `json:"written"`
			SHA256  string             `json:"sha256"`
			Batch   transfer.BatchView `json:"batch"`
		}{res.Written, res.SHA256, view})
	}
}

func writeChunkStreamError(w http.ResponseWriter, r *http.Request, err error, committedOffset int64) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
	var mm *transfer.OffsetMismatchError
	if errors.As(err, &mm) {
		w.Header().Set("Upload-Offset", strconv.FormatInt(mm.Expected, 10))
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":           "offset_mismatch",
			"error":          "upload offset mismatch",
			"expectedOffset": mm.Expected,
		})
		return
	}
	switch {
	case errors.Is(err, transfer.ErrUploadPaused):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "paused", "error": "upload is paused"})
	case errors.Is(err, transfer.ErrUploadDiscarded):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "discarded", "error": "partial upload was discarded"})
	case errors.Is(err, transfer.ErrStagingQuarantined):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "quarantined", "error": "staged upload failed integrity check and was quarantined"})
	case errors.Is(err, transfer.ErrChunkInterrupted):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":            "chunk_interrupted",
			"error":           "chunk transfer interrupted; resume from Upload-Offset",
			"committedOffset": committedOffset,
		})
	case errors.Is(err, transfer.ErrInvalidChunk):
		writeError(w, http.StatusBadRequest, "invalid chunk offset or length")
	case isReadTimeout(err):
		writeJSON(w, http.StatusRequestTimeout, map[string]any{
			"code":            "stall",
			"error":           "upload chunk timed out; resume from Upload-Offset",
			"committedOffset": committedOffset,
		})
	default:
		writeUploadStreamError(w, err)
	}
}
