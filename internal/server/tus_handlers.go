package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/audit"
	"github.com/itsyamin/flitnix/internal/transfer"
)

const (
	tusVersion        = "1.0.0"
	tusSupportedVers  = "1.0.0"
	tusExtensions     = "creation,termination,expiration"
	tusMaxSize        = "107374182400" // 100 GB
	tusExposeHeaders  = "Tus-Resumable,Tus-Version,Tus-Extension,Tus-Max-Size,Upload-Offset,Upload-Length,Upload-Expires,Upload-State,Upload-Prefix-Digest"
	tusAllowedHeaders = "Tus-Resumable,Upload-Length,Upload-Metadata,Upload-Offset,Content-Type,X-HTTP-Method-Override"
	tusAllowedMethods = "OPTIONS,POST,HEAD,PATCH,DELETE"

	statusLocked           = 423
	statusChecksumMismatch = 460
)

// checkTusResumable checks the Tus-Resumable header for data requests.
func checkTusResumable(w http.ResponseWriter, r *http.Request) bool {
	resumable := strings.TrimSpace(r.Header.Get("Tus-Resumable"))
	if resumable != tusVersion {
		w.Header().Set("Tus-Version", tusSupportedVers)
		http.Error(w, "unsupported tus version", http.StatusPreconditionFailed)
		return false
	}
	return true
}

func writeTusOptionsHeaders(w http.ResponseWriter) {
	w.Header().Set("Tus-Resumable", tusVersion)
	w.Header().Set("Tus-Version", tusSupportedVers)
	w.Header().Set("Tus-Extension", tusExtensions)
	w.Header().Set("Tus-Max-Size", tusMaxSize)
	w.Header().Set("Access-Control-Expose-Headers", tusExposeHeaders)
	w.Header().Set("Access-Control-Allow-Methods", tusAllowedMethods)
	w.Header().Set("Access-Control-Allow-Headers", tusAllowedHeaders)
}

func personalTransferID(batchID, itemID string) string {
	return fmt.Sprintf("batch-%s-%s", batchID, itemID)
}

func guestTransferID(uploadID string) string {
	return fmt.Sprintf("guest-%s", uploadID)
}

func prefixDigestForMeta(meta *transfer.ResumableMeta) string {
	if meta.PrefixDigest != "" {
		return meta.PrefixDigest
	}
	if meta.CommittedOffset == 0 {
		return "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	}
	return ""
}

// ---- Personal Transfer tus Handlers ----

// handleBatchTusOptions serves OPTIONS /api/batches/{batchID}/items/{itemID}/tus.
func handleBatchTusOptions() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeTusOptionsHeaders(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleBatchTusCreate serves POST /api/batches/{batchID}/items/{itemID}/tus.
func handleBatchTusCreate(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkTusResumable(w, r) {
			return
		}
		sess := sessionFrom(r)
		if sess == nil || svcs.Coordinator == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		batchID := r.PathValue("batchID")
		itemID := r.PathValue("itemID")

		// Query batch to verify item and declared size
		view, err := svcs.Coordinator.GetBatch(sess.Key(), batchID)
		if err != nil {
			writeBatchError(w, err)
			return
		}

		var targetItem *transfer.ItemView
		for i := range view.Items {
			if view.Items[i].ID == itemID {
				targetItem = &view.Items[i]
				break
			}
		}
		if targetItem == nil {
			writeError(w, http.StatusNotFound, "batch item not found")
			return
		}

		// Desktop consent gates resumable uploads exactly as it gates the
		// legacy PUT path: an unapproved batch must not accept content, or
		// a rogue client could stream files before the operator decides.
		if view.ApprovalState != transfer.ApprovalApproved {
			writeError(w, http.StatusConflict, "upload awaits desktop approval")
			return
		}

		lengthHeader := strings.TrimSpace(r.Header.Get("Upload-Length"))
		if lengthHeader == "" {
			writeError(w, http.StatusBadRequest, "missing Upload-Length header")
			return
		}
		declaredLength, err := strconv.ParseInt(lengthHeader, 10, 64)
		if err != nil || declaredLength < 0 {
			writeError(w, http.StatusBadRequest, "invalid Upload-Length header")
			return
		}
		if declaredLength != targetItem.DeclaredSize {
			writeError(w, http.StatusBadRequest, "Upload-Length does not match declared item size")
			return
		}

		transferID := personalTransferID(batchID, itemID)
		meta, err := resumable.CreateUpload(sess.Key(), transferID, batchID, itemID, targetItem.DisplayName, targetItem.RelativePath, declaredLength, "")
		if err != nil {
			if errors.Is(err, transfer.ErrCrossFilesystem) {
				writeError(w, http.StatusBadRequest, "cross-filesystem destination not permitted")
				return
			}
			if errors.Is(err, transfer.ErrInsufficientStorage) {
				writeError(w, http.StatusInsufficientStorage, "insufficient storage space")
				return
			}
			if errors.Is(err, transfer.ErrQuotaExceeded) {
				writeError(w, http.StatusTooManyRequests, "upload quota exceeded")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.Header().Set("Tus-Resumable", tusVersion)
		w.Header().Set("Location", r.URL.Path)
		w.Header().Set("Upload-Offset", strconv.FormatInt(meta.CommittedOffset, 10))
		w.Header().Set("Upload-Expires", meta.ExpiresAt.UTC().Format(http.TimeFormat))
		if pd := prefixDigestForMeta(meta); pd != "" {
			w.Header().Set("Upload-Prefix-Digest", pd)
		}
		w.WriteHeader(http.StatusCreated)
	}
}

// handleBatchTusHead serves HEAD /api/batches/{batchID}/items/{itemID}/tus.
func handleBatchTusHead(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkTusResumable(w, r) {
			return
		}
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		batchID := r.PathValue("batchID")
		itemID := r.PathValue("itemID")
		transferID := personalTransferID(batchID, itemID)

		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != sess.Key() {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		meta := upload.Meta()
		w.Header().Set("Tus-Resumable", tusVersion)
		w.Header().Set("Upload-Offset", strconv.FormatInt(meta.CommittedOffset, 10))
		w.Header().Set("Upload-Length", strconv.FormatInt(meta.TotalSize, 10))
		w.Header().Set("Upload-State", string(meta.State))
		w.Header().Set("Upload-Expires", meta.ExpiresAt.UTC().Format(http.TimeFormat))
		if pd := prefixDigestForMeta(&meta); pd != "" {
			w.Header().Set("Upload-Prefix-Digest", pd)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	}
}

// handleBatchTusPatch serves PATCH /api/batches/{batchID}/items/{itemID}/tus.
func handleBatchTusPatch(svcs *Services, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkTusResumable(w, r) {
			return
		}
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		batchID := r.PathValue("batchID")
		itemID := r.PathValue("itemID")
		transferID := personalTransferID(batchID, itemID)

		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != sess.Key() {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
		if contentType != "application/offset+octet-stream" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/offset+octet-stream")
			return
		}

		offsetHeader := strings.TrimSpace(r.Header.Get("Upload-Offset"))
		if offsetHeader == "" {
			writeError(w, http.StatusBadRequest, "missing Upload-Offset header")
			return
		}
		clientOffset, err := strconv.ParseInt(offsetHeader, 10, 64)
		if err != nil || clientOffset < 0 {
			writeError(w, http.StatusBadRequest, "invalid Upload-Offset header")
			return
		}

		body := io.Reader(r.Body)
		if rc := http.NewResponseController(w); rc != nil && stall > 0 {
			body = &stallBodyReader{ReadCloser: r.Body, rc: rc, stall: stall}
		}

		if svcs.Inhibitor != nil {
			pLease, pErr := svcs.Inhibitor.Acquire(fmt.Sprintf("Receiving item %s in batch %s", itemID, batchID))
			if pErr == nil && pLease != nil {
				defer pLease.Release()
			}
		}

		committedOffset, state, streamErr := resumable.IngestStream(transferID, clientOffset, body, func(current int64) {
			// Update coordinator item progress live
			_ = svcs.Coordinator.UpdateItemProgress(sess.Key(), transfer.TransferLease{BatchID: batchID, ItemID: itemID}, current)
		})

		if errors.Is(streamErr, transfer.ErrUploadLocked) {
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(statusLocked)
			_, _ = w.Write([]byte("Upload resource is actively locked by another operation\n"))
			return
		}

		if errors.Is(streamErr, transfer.ErrOffsetMismatch) {
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
			w.WriteHeader(http.StatusConflict)
			return
		}

		if errors.Is(streamErr, transfer.ErrInsufficientStorage) {
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
			w.Header().Set("Upload-State", "paused_storage_full")
			writeError(w, http.StatusInsufficientStorage, "insufficient storage space")
			return
		}

		if streamErr != nil {
			// Interrupted stream, read error, or sync failure: last committed checkpoint was flushed; do NOT return 204
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
			curMeta := upload.Meta()
			if pd := prefixDigestForMeta(&curMeta); pd != "" {
				w.Header().Set("Upload-Prefix-Digest", pd)
			}
			if errors.Is(streamErr, io.ErrUnexpectedEOF) || errors.Is(streamErr, io.ErrClosedPipe) {
				writeError(w, http.StatusBadRequest, "request body truncated")
				return
			}
			writeError(w, http.StatusInternalServerError, streamErr.Error())
			return
		}

		w.Header().Set("Tus-Resumable", tusVersion)
		w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
		w.Header().Set("Upload-State", string(state))
		curMeta := upload.Meta()
		if pd := prefixDigestForMeta(&curMeta); pd != "" {
			w.Header().Set("Upload-Prefix-Digest", pd)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleBatchTusDelete serves DELETE /api/batches/{batchID}/items/{itemID}/tus.
func handleBatchTusDelete(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkTusResumable(w, r) {
			return
		}
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		batchID := r.PathValue("batchID")
		itemID := r.PathValue("itemID")
		transferID := personalTransferID(batchID, itemID)

		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != sess.Key() {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		err := resumable.CancelUpload(transferID)
		if errors.Is(err, transfer.ErrUploadLocked) {
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(statusLocked)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Cancel coordinator item only; leaves all other batch items unaffected!
		_ = svcs.Coordinator.CancelItem(sess.Key(), batchID, itemID)

		w.Header().Set("Tus-Resumable", tusVersion)
		w.WriteHeader(http.StatusNoContent)
	}
}

type completeRequest struct {
	ExpectedSHA256 string `json:"expectedSHA256"`
}

// handleBatchComplete serves POST /api/batches/{batchID}/items/{itemID}/complete.
func handleBatchComplete(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		batchID := r.PathValue("batchID")
		itemID := r.PathValue("itemID")
		transferID := personalTransferID(batchID, itemID)

		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != sess.Key() {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		var req completeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid complete request JSON")
			return
		}

		// FinalizeComplete publishes the file into the receive directory, so
		// desktop consent must be verified first: completing an upload on a
		// pending or rejected batch would place content on disk without the
		// operator's approval.
		batchView, batchErr := svcs.Coordinator.GetBatch(sess.Key(), batchID)
		if batchErr != nil {
			writeBatchError(w, batchErr)
			return
		}
		if batchView.ApprovalState != transfer.ApprovalApproved {
			writeError(w, http.StatusConflict, "upload awaits desktop approval")
			return
		}

		pub, err := resumable.FinalizeComplete(transferID, req.ExpectedSHA256)
		if errors.Is(err, transfer.ErrCrossFilesystem) {
			writeError(w, http.StatusBadRequest, "cross-filesystem destination not permitted")
			return
		}
		if errors.Is(err, transfer.ErrUploadLocked) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(statusLocked)
			return
		}
		if errors.Is(err, transfer.ErrInvalidDigest) {
			writeError(w, http.StatusBadRequest, "invalid expectedSHA256: must be 64-char lowercase hex")
			return
		}
		if errors.Is(err, transfer.ErrChecksumMismatch) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusChecksumMismatch)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "failed",
				"error":  "checksum mismatch",
			})
			return
		}
		if errors.Is(err, transfer.ErrConflict) {
			writeError(w, http.StatusConflict, "conflicting digest for completed upload")
			return
		}
		if errors.Is(err, transfer.ErrInvalidState) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Settle the coordinator item. FinalizeComplete has published and
		// verified the file, so the coordinator must agree the item
		// completed: claim the item (it is the next eligible one), record
		// the verified size, and complete it. A repeat complete finds the
		// item already completed and returns the publication directly
		// (idempotent per Scenario 44).
		viewNow, viewErr := svcs.Coordinator.GetBatch(sess.Key(), batchID)
		if viewErr != nil {
			writeBatchError(w, viewErr)
			return
		}
		alreadyCompleted := false
		for i := range viewNow.Items {
			if viewNow.Items[i].ID == itemID && viewNow.Items[i].State == transfer.ItemStateCompleted {
				alreadyCompleted = true
				break
			}
		}
		if !alreadyCompleted {
			realLease, claimErr := svcs.Coordinator.ClaimItem(sess.Key(), batchID, itemID)
			if claimErr != nil {
				writeBatchError(w, claimErr)
				return
			}
			if progErr := svcs.Coordinator.UpdateItemProgress(sess.Key(), realLease, pub.Size); progErr != nil {
				writeBatchError(w, progErr)
				return
			}
			if _, completeErr := svcs.Coordinator.CompleteItem(sess.Key(), realLease, pub.ExpectedSHA256); completeErr != nil {
				writeBatchError(w, completeErr)
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status": "completed",
			"sha256": pub.ExpectedSHA256,
			"path":   pub.TargetPath,
			"size":   pub.Size,
		})
	}
}

// handleBatchStatus serves GET /api/batches/{batchID}/items/{itemID}/status.
func handleBatchItemStatus(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		batchID := r.PathValue("batchID")
		itemID := r.PathValue("itemID")
		transferID := personalTransferID(batchID, itemID)

		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != sess.Key() {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		meta := upload.Meta()
		writeJSON(w, http.StatusOK, map[string]any{
			"status": meta.State,
			"offset": meta.CommittedOffset,
			"size":   meta.TotalSize,
			"sha256": meta.PrefixDigest,
		})
	}
}

// ---- Guest Drop Box tus Handlers ----

// handleGuestTusOptions serves OPTIONS /guest-api/uploads/{uploadID}/tus.
func handleGuestTusOptions() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeTusOptionsHeaders(w)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleGuestTusCreate serves POST /guest-api/uploads/{uploadID}/tus.
func handleGuestTusCreate(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkTusResumable(w, r) {
			return
		}
		token := guestTokenFromRequest(r)
		if token == "" || svcs.GuestManager == nil {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}
		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		uploadID := r.PathValue("uploadID")
		up, err := room.GetUpload(uploadID)
		if err != nil || up.GuestID != g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		lengthHeader := strings.TrimSpace(r.Header.Get("Upload-Length"))
		if lengthHeader == "" {
			writeError(w, http.StatusBadRequest, "missing Upload-Length header")
			return
		}
		declaredLength, err := strconv.ParseInt(lengthHeader, 10, 64)
		if err != nil || declaredLength < 0 {
			writeError(w, http.StatusBadRequest, "invalid Upload-Length header")
			return
		}
		if declaredLength != up.Size {
			writeError(w, http.StatusBadRequest, "Upload-Length does not match declared upload size")
			return
		}

		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		transferID := guestTransferID(uploadID)
		guestDir := room.GuestDirectory(g.ID, g.SafeSlug)
		meta, err := resumable.CreateUpload("guest-"+g.ID, transferID, "", uploadID, up.FileName, "", declaredLength, guestDir)
		if err != nil {
			if errors.Is(err, transfer.ErrCrossFilesystem) {
				writeError(w, http.StatusBadRequest, "cross-filesystem destination not permitted")
				return
			}
			if errors.Is(err, transfer.ErrInsufficientStorage) {
				writeError(w, http.StatusInsufficientStorage, "insufficient storage space")
				return
			}
			if errors.Is(err, transfer.ErrQuotaExceeded) {
				writeError(w, http.StatusTooManyRequests, "upload quota exceeded")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		w.Header().Set("Tus-Resumable", tusVersion)
		w.Header().Set("Location", r.URL.Path)
		w.Header().Set("Upload-Offset", strconv.FormatInt(meta.CommittedOffset, 10))
		w.Header().Set("Upload-Expires", meta.ExpiresAt.UTC().Format(http.TimeFormat))
		if pd := prefixDigestForMeta(meta); pd != "" {
			w.Header().Set("Upload-Prefix-Digest", pd)
		}
		w.WriteHeader(http.StatusCreated)
	}
}

// handleGuestTusHead serves HEAD /guest-api/uploads/{uploadID}/tus.
func handleGuestTusHead(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkTusResumable(w, r) {
			return
		}
		token := guestTokenFromRequest(r)
		if token == "" || svcs.GuestManager == nil {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}
		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		uploadID := r.PathValue("uploadID")
		up, err := room.GetUpload(uploadID)
		if err != nil || up.GuestID != g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		transferID := guestTransferID(uploadID)
		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != "guest-"+g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		meta := upload.Meta()
		w.Header().Set("Tus-Resumable", tusVersion)
		w.Header().Set("Upload-Offset", strconv.FormatInt(meta.CommittedOffset, 10))
		w.Header().Set("Upload-Length", strconv.FormatInt(meta.TotalSize, 10))
		w.Header().Set("Upload-State", string(meta.State))
		w.Header().Set("Upload-Expires", meta.ExpiresAt.UTC().Format(http.TimeFormat))
		if pd := prefixDigestForMeta(&meta); pd != "" {
			w.Header().Set("Upload-Prefix-Digest", pd)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	}
}

// handleGuestTusPatch serves PATCH /guest-api/uploads/{uploadID}/tus.
func handleGuestTusPatch(svcs *Services, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkTusResumable(w, r) {
			return
		}
		token := guestTokenFromRequest(r)
		if token == "" || svcs.GuestManager == nil {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}
		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		uploadID := r.PathValue("uploadID")
		up, err := room.GetUpload(uploadID)
		if err != nil || up.GuestID != g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
		if contentType != "application/offset+octet-stream" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/offset+octet-stream")
			return
		}

		offsetHeader := strings.TrimSpace(r.Header.Get("Upload-Offset"))
		if offsetHeader == "" {
			writeError(w, http.StatusBadRequest, "missing Upload-Offset header")
			return
		}
		clientOffset, err := strconv.ParseInt(offsetHeader, 10, 64)
		if err != nil || clientOffset < 0 {
			writeError(w, http.StatusBadRequest, "invalid Upload-Offset header")
			return
		}

		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		transferID := guestTransferID(uploadID)
		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != "guest-"+g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		body := io.Reader(r.Body)
		if rc := http.NewResponseController(w); rc != nil && stall > 0 {
			body = &stallBodyReader{ReadCloser: r.Body, rc: rc, stall: stall}
		}

		if svcs.Inhibitor != nil {
			pLease, pErr := svcs.Inhibitor.Acquire("Receiving guest upload " + up.FileName)
			if pErr == nil && pLease != nil {
				defer pLease.Release()
			}
		}

		committedOffset, state, streamErr := resumable.IngestStream(transferID, clientOffset, body, func(current int64) {
			room.UpdateUploadProgress(uploadID, current)
		})

		if errors.Is(streamErr, transfer.ErrUploadLocked) {
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(statusLocked)
			_, _ = w.Write([]byte("Upload resource is actively locked by another operation\n"))
			return
		}

		if errors.Is(streamErr, transfer.ErrOffsetMismatch) {
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
			w.WriteHeader(http.StatusConflict)
			return
		}

		if errors.Is(streamErr, transfer.ErrInsufficientStorage) {
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
			w.Header().Set("Upload-State", "paused_storage_full")
			writeError(w, http.StatusInsufficientStorage, "insufficient storage space")
			return
		}

		if streamErr != nil {
			// Interrupted stream, read error, or sync failure: last committed checkpoint was flushed; do NOT return 204
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
			curMeta := upload.Meta()
			if pd := prefixDigestForMeta(&curMeta); pd != "" {
				w.Header().Set("Upload-Prefix-Digest", pd)
			}
			if errors.Is(streamErr, io.ErrUnexpectedEOF) || errors.Is(streamErr, io.ErrClosedPipe) {
				writeError(w, http.StatusBadRequest, "request body truncated")
				return
			}
			writeError(w, http.StatusInternalServerError, streamErr.Error())
			return
		}

		w.Header().Set("Tus-Resumable", tusVersion)
		w.Header().Set("Upload-Offset", strconv.FormatInt(committedOffset, 10))
		w.Header().Set("Upload-State", string(state))
		curMeta := upload.Meta()
		if pd := prefixDigestForMeta(&curMeta); pd != "" {
			w.Header().Set("Upload-Prefix-Digest", pd)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleGuestTusDelete serves DELETE /guest-api/uploads/{uploadID}/tus.
func handleGuestTusDelete(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkTusResumable(w, r) {
			return
		}
		token := guestTokenFromRequest(r)
		if token == "" || svcs.GuestManager == nil {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}
		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		uploadID := r.PathValue("uploadID")
		up, err := room.GetUpload(uploadID)
		if err != nil || up.GuestID != g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		transferID := guestTransferID(uploadID)
		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != "guest-"+g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		err = resumable.CancelUpload(transferID)
		if errors.Is(err, transfer.ErrUploadLocked) {
			w.Header().Set("Tus-Resumable", tusVersion)
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(statusLocked)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		room.CancelUpload(uploadID)

		w.Header().Set("Tus-Resumable", tusVersion)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleGuestComplete serves POST /guest-api/uploads/{uploadID}/complete.
func handleGuestComplete(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := guestTokenFromRequest(r)
		if token == "" || svcs.GuestManager == nil {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}
		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		uploadID := r.PathValue("uploadID")
		up, err := room.GetUpload(uploadID)
		if err != nil || up.GuestID != g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		var req completeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid complete request JSON")
			return
		}

		transferID := guestTransferID(uploadID)
		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != "guest-"+g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		pub, err := resumable.FinalizeComplete(transferID, req.ExpectedSHA256)
		if errors.Is(err, transfer.ErrCrossFilesystem) {
			writeError(w, http.StatusBadRequest, "cross-filesystem destination not permitted")
			return
		}
		if errors.Is(err, transfer.ErrUploadLocked) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(statusLocked)
			return
		}
		if errors.Is(err, transfer.ErrInvalidDigest) {
			writeError(w, http.StatusBadRequest, "invalid expectedSHA256: must be 64-char lowercase hex")
			return
		}
		if errors.Is(err, transfer.ErrChecksumMismatch) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusChecksumMismatch)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "failed",
				"error":  "checksum mismatch",
			})
			return
		}
		if errors.Is(err, transfer.ErrConflict) {
			writeError(w, http.StatusConflict, "conflicting digest for completed upload")
			return
		}
		if errors.Is(err, transfer.ErrInvalidState) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Mark completed in guest room
		room.CompleteUpload(uploadID, pub.ExpectedSHA256, pub.TargetPath, pub.Size)

		if svcs.Audit != nil {
			svcs.Audit.Record(audit.Entry{
				EventType: audit.EventUploadCompleted,
				RoomID:    room.ID,
				GuestID:   g.ID,
				GuestName: g.Nickname,
				FileName:  up.FileName,
				FileSize:  pub.Size,
				SHA256:    pub.ExpectedSHA256,
				Outcome:   audit.OutcomeSuccess,
				Details:   fmt.Sprintf("Resumable upload completed: %s (%d bytes)", up.FileName, pub.Size),
			})
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status": "completed",
			"sha256": pub.ExpectedSHA256,
			"path":   pub.TargetPath,
			"size":   pub.Size,
		})
	}
}

// handleGuestUploadStatus serves GET /guest-api/uploads/{uploadID}/status.
func handleGuestResumableStatus(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := guestTokenFromRequest(r)
		if token == "" || svcs.GuestManager == nil {
			writeError(w, http.StatusUnauthorized, "missing guest credential")
			return
		}
		room, g, err := svcs.GuestManager.ValidateGuest(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		uploadID := r.PathValue("uploadID")
		up, err := room.GetUpload(uploadID)
		if err != nil || up.GuestID != g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		resumable := svcs.EnsureResumable()
		if resumable == nil {
			writeError(w, http.StatusInternalServerError, "resumable storage unavailable")
			return
		}

		transferID := guestTransferID(uploadID)
		upload, ok := resumable.GetUpload(transferID)
		if !ok || upload.Meta().SessionKey != "guest-"+g.ID {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}

		meta := upload.Meta()
		writeJSON(w, http.StatusOK, map[string]any{
			"status": meta.State,
			"offset": meta.CommittedOffset,
			"size":   meta.TotalSize,
			"sha256": meta.PrefixDigest,
		})
	}
}
