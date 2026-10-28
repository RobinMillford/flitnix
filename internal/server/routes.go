package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// newRouter builds the full HTTP router.
//
// Page routes (unauthenticated):
//
//	/             GET/HEAD  the embedded phone webpage
//	/healthz      GET/HEAD  JSON health response
//	/static/<f>   GET/HEAD  embedded stylesheet / script / other assets
//	anything else           404 JSON error
//
// API routes (Bearer authenticated, present only when services != nil):
//
//	POST /api/uploads                 phone declares an incoming file
//	GET  /api/uploads/{id}            phone polls one of its own uploads
//	PUT  /api/uploads/{id}/content    phone streams the approved body
//	POST /api/batches                phone declares an ordered batch
//	GET  /api/batches/{id}           phone polls its own batch
//	DELETE /api/batches/{id}/items/{itemID}
//	                                 phone withdraws one batch item
//	PUT  /api/batches/{batchID}/items/{itemID}/content
//	                                 phone streams one approved item
//	GET  /api/files                   lists the desktop-shared files
//	GET  /api/files/{id}/download     streams one shared file
//
// Method handling is delegated to ServeMux's method-prefixed patterns: a
// method mismatch on a registered path yields 405 with an Allow header, and
// a GET registration also serves HEAD (the body is stripped by net/http).
// Everything below /api/ that is unknown yields 404 within the API router.
// With a nil Services every /api/ path answers 404 (page-only server).
func newRouter(services *Services, stall, downloadStall time.Duration, grants *DownloadGrantStore) http.Handler {
	index, err := pageAssets.ReadFile("assets/index.html")
	if err != nil {
		panic("server: embedded assets missing: " + err.Error())
	}
	assetsRoot, err := fs.Sub(pageAssets, "assets")
	if err != nil {
		panic("server: embedded assets unusable: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", serveHome(index))
	mux.HandleFunc("/healthz", serveHealth)
	mux.HandleFunc("/static/", serveStatic(assetsRoot))
	mux.HandleFunc("/", serveNotFound)

	if services == nil {
		// Stages before the transfer endpoints keep deterministic page-only
		// semantics: no API surface, everything under /api/ is a 404.
		mux.HandleFunc("/api/", serveNotFound)
	} else {
		// The whole API router sits behind one session check, so unknown
		// /api/* paths and method mismatches are answered only after
		// authentication; a token holder learns nothing about the
		// endpoint set without a live session either way.
		api := newAPIMux(services, stall, grants)
		mux.Handle("/api/", requireSession(services.Sessions, api.ServeHTTP))
		mux.HandleFunc("GET /api/downloads/{id}/content", serveArchiveDownload(services, grants, downloadStall))
	}
	return mux
}

// newAPIMux registers the transfer endpoints. Authentication is applied at
// the /api/ boundary by newRouter, so nothing reaches these handlers
// without an authenticated session in the request context.
func newAPIMux(services *Services, stall time.Duration, grants *DownloadGrantStore) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/uploads", handleUploadDeclare(services, stall))
	mux.HandleFunc("GET /api/uploads/{id}", handleUploadStatus(services))
	mux.HandleFunc("PUT /api/uploads/{id}/content", handleUploadContent(services, stall))
	mux.HandleFunc("POST /api/batches", handleBatchCreate(services))
	mux.HandleFunc("GET /api/batches/{id}", handleBatchStatus(services))
	mux.HandleFunc("DELETE /api/batches/{id}/items/{itemID}", handleBatchItemCancel(services))
	mux.HandleFunc("PUT /api/batches/{batchID}/items/{itemID}/content", handleBatchContent(services, stall))
	mux.HandleFunc("GET /api/files", serveFilesList(services))
	mux.HandleFunc("GET /api/files/{id}/download", serveFileDownload(services, stall))
	mux.HandleFunc("GET /api/sources", serveSourcesList(services))
	mux.HandleFunc("POST /api/download-grants", createDownloadGrant(services, grants))
	mux.HandleFunc("GET /api/download-progress/{id}", serveDownloadProgress(services, grants))
	return mux
}

// ---- request authentication ----

// bearerPrefix is the required form of the Authorization header. The token
// value travels only between phone and desktop; it is never logged and
// never appears in URLs.
const bearerPrefix = "Bearer "

type sessCtxKey struct{}

// requireSession authenticates the request's Bearer token with the session
// manager and stores the authenticated *session.Session in the request
// context for the handler. Any failure (missing, malformed, unknown,
// expired, revoked) answers 401 with a deliberately generic message so the
// response never reveals why a token is dead. Only the authenticated handler
// sees the session; failures here never log any token material.
func requireSession(m *session.Manager, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		auth = strings.TrimSpace(auth)
		if !strings.HasPrefix(auth, bearerPrefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		token := auth[len(bearerPrefix):]
		token = strings.TrimSpace(token)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		sess, err := m.Validate(token)
		if err != nil {
			// The message stays generic. The machine-readable code lets the
			// phone page tell a session that really ended (expired or
			// revoked) from a token it never had; it exposes nothing about
			// the token itself and is only produced for a token that was
			// once valid.
			writeUnauthorized(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessCtxKey{}, sess)))
	})
}

// sessionFrom returns the authenticated session requireSession placed in the
// request context, or nil for unauthenticated requests.
func sessionFrom(r *http.Request) *session.Session {
	s, _ := r.Context().Value(sessCtxKey{}).(*session.Session)
	return s
}

// ---- transfer diagnostics ----

// diagnose is the nil-safe sink for transfer-lifecycle diagnostics. Call
// sites pass opaque short identifiers only; when no hook is configured the
// diagnostic is dropped.
func diagnose(svcs *Services, format string, args ...any) {
	if svcs == nil || svcs.Diagnose == nil {
		return
	}
	svcs.Diagnose(format, args...)
}

// shortKey bounds a session key to its leading prefix: keys are internal
// values, and they reach a log only in shortened form.
func shortKey(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}

// shortID bounds an opaque record identifier for diagnostics output.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// diagMilestoneMinBytes is the declared size above which quarter-mark
// progress lines are written. Smaller items are described by their
// claim/done lines alone, so a batch of many small files stays quiet.
const diagMilestoneMinBytes = 1024 * 1024

// classifyStreamFailure names the cause of a failed content stream for
// diagnostics. It deliberately never returns the raw error, which embeds
// the file's display name. A client disconnect surfaces through the
// request context once the server has observed the dropped connection;
// otherwise the body's own verdict (short, oversized, canceled) or a
// connection read-deadline firing names the cause.
func classifyStreamFailure(r *http.Request, err error) string {
	if r.Context().Err() != nil {
		return "client_disconnect"
	}
	switch {
	case errors.Is(err, transfer.ErrSizeExceeded):
		return "size_exceeded"
	case errors.Is(err, transfer.ErrTruncated):
		return "truncated"
	case errors.Is(err, transfer.ErrCanceled):
		return "canceled"
	case isReadTimeout(err):
		return "stall"
	default:
		return "server_error"
	}
}

// isReadTimeout reports whether err is a connection read deadline firing —
// the stall watchdog's cut, not an ordinary read fault.
func isReadTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// ---- transfer-stall deadlines ----

// stallBodyReader wraps an upload body with a rolling read deadline: every
// Read first extends the deadline by the stall window. A sender that keeps
// pushing bytes is never cut; one that stalls inside the request stream is.
// Setting the deadline is best effort (see the package timeout policy).
type stallBodyReader struct {
	io.ReadCloser
	rc    *http.ResponseController
	stall time.Duration // <= 0 disables
}

func (b *stallBodyReader) Read(p []byte) (int, error) {
	if b.stall > 0 {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.stall))
	}
	return b.ReadCloser.Read(p)
}

// stallWriter wraps a download response with a rolling write deadline: every
// Write first extends the deadline by the stall window. A receiver that
// drains at any speed is never cut; one that stops reading is.
type stallWriter struct {
	w     http.ResponseWriter
	rc    *http.ResponseController
	stall time.Duration // <= 0 disables
}

func (d *stallWriter) Write(p []byte) (int, error) {
	if d.stall > 0 {
		_ = d.rc.SetWriteDeadline(time.Now().Add(d.stall))
	}
	return d.w.Write(p)
}

// ---- incoming uploads ----

// maxMetadataBytes bounds the JSON upload-metadata request body. The body
// holds a filename and a byte count; the sanitizer clamps names far below
// this, so 4 KiB is a hugely generous bound and anything larger is plainly
// not metadata.
const maxMetadataBytes = 4 * 1024

const maxBatchMetadataBytes = 1024 * 1024

// uploadJSON is the phone-visible view of an upload record. It deliberately
// omits the session key and the server-side absolute paths: the phone needs
// the record's identity and progress, not disk layout.
type uploadJSON struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	State   string    `json:"state"`
	Reason  string    `json:"reason,omitempty"`
	// Uploaded is the live byte counter; it stays zero until the content
	// stream starts.
	Uploaded int64 `json:"uploaded"`
}

func toUploadJSON(u transfer.Upload) uploadJSON {
	return uploadJSON{
		ID:       u.ID,
		Name:     u.Name,
		Size:     u.Size,
		Created:  u.Created,
		State:    string(u.State),
		Reason:   u.Reason,
		Uploaded: u.Uploaded,
	}
}

// handleUploadDeclare serves POST /api/uploads: phase one of the two-phase
// receive protocol. The phone submits metadata (filename, size) and gets a
// pending record; only after the desktop user approves may it stream the
// body. The metadata body is bounded and parsed strictly (known fields
// only); anything larger or malformed is rejected before touching the
// approval machine.
func handleUploadDeclare(svcs *Services, _ time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			// requireSession guarantees this never happens; defensive.
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMetadataBytes))
		if err != nil {
			if isMaxBytes(err) || errors.Is(err, io.ErrUnexpectedEOF) {
				writeError(w, http.StatusRequestEntityTooLarge,
					"metadata body too large")
				return
			}
			writeError(w, http.StatusBadRequest, "reading metadata body")
			return
		}
		if len(body) > maxMetadataBytes {
			// A MaxBytesReader only errors once the excess write happens;
			// for readers that stop exactly at the line the length check
			// is the backstop.
			writeError(w, http.StatusRequestEntityTooLarge, "metadata body too large")
			return
		}

		var meta struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&meta); err != nil {
			writeError(w, http.StatusBadRequest, "invalid upload metadata JSON")
			return
		}

		up, err := svcs.Transfers.Declare(sess.Key(), meta.Name, meta.Size)
		switch {
		case err == nil:
		case errors.Is(err, transfer.ErrInvalidUpload),
			errors.Is(err, transfer.ErrInvalidFilename):
			writeError(w, http.StatusBadRequest, "invalid filename or size")
			return
		case errors.Is(err, transfer.ErrNoCapacity):
			writeError(w, http.StatusServiceUnavailable,
				"too many pending uploads; wait for the desktop to decide on the existing ones")
			return
		default:
			writeError(w, http.StatusInternalServerError, "upload declaration failed")
			return
		}

		writeJSON(w, http.StatusCreated, toUploadJSON(up))
	}
}

// isMaxBytes bigotry-free detection of *http.MaxBytesError (Go 1.19+).
func isMaxBytes(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// handleUploadStatus serves GET /api/uploads/{id}: the phone polls its own
// upload record. Records of other sessions are indistinguishable from
// unknown ones (404), so a token holder can never probe another session's
// business or even learn that an ID exists.
func handleUploadStatus(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		id := r.PathValue("id")
		u, ok := svcs.Transfers.Get(id)
		if !ok || u.Session != sess.Key() {
			writeError(w, http.StatusNotFound, "upload not found")
			return
		}
		writeJSON(w, http.StatusOK, toUploadJSON(u))
	}
}

// uploadResultJSON is the success body of PUT content. It reports the exact
// byte count and digest of what landed on disk; the absolute server path
// stays private to the desktop.
type uploadResultJSON struct {
	Written int64  `json:"written"`
	SHA256  string `json:"sha256"`
}

// handleUploadContent serves PUT /api/uploads/{id}/content: phase two of
// the receive protocol. The stream is bounded by the approved size and
// hashed while written; truncation, overrun, disk failures and desktop
// cancellation each end with an honest error. The session is pinned for the
// stream's duration so the idle timer can never abort an in-flight large
// transfer, and the rolling stall deadline detects senders that stop
// making progress.
func handleUploadContent(svcs *Services, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		id := r.PathValue("id")
		sess.Begin()
		defer sess.End()
		body := &stallBodyReader{
			ReadCloser: r.Body,
			rc:         http.NewResponseController(w),
			stall:      stall,
		}
		res, err := svcs.Transfers.SubmitContent(id, sess.Key(), body, nil)
		if err != nil {
			writeUploadStreamError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, uploadResultJSON{Written: res.Written, SHA256: res.SHA256})
	}
}

func handleBatchCreate(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil || svcs.Coordinator == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBatchMetadataBytes))
		if err != nil || len(body) > maxBatchMetadataBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "batch manifest too large")
			return
		}
		var manifest transfer.BatchManifest
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&manifest); err != nil {
			writeError(w, http.StatusBadRequest, "invalid batch manifest JSON")
			return
		}
		if manifest.Kind == transfer.BatchKindFiles {
			for i := range manifest.Items {
				if manifest.Items[i].DisplayName == "" {
					name, nameErr := transfer.SanitizeRelativePath(manifest.Items[i].RelativePath)
					if nameErr != nil || strings.Contains(name, "/") {
						writeError(w, http.StatusBadRequest, "invalid batch filename")
						return
					}
					manifest.Items[i].DisplayName = name
				}
				manifest.Items[i].RelativePath = ""
			}
		}
		validated, err := transfer.ValidateManifest(manifest)
		if err != nil {
			writeBatchError(w, err)
			return
		}
		manifest = validated.BatchManifest
		var resolvedRoot string
		if manifest.Kind == transfer.BatchKindFolder {
			resolvedRoot, err = svcs.Transfers.Store().AllocateFolderRoot(manifest.RootName)
			if err != nil {
				writeBatchError(w, err)
				return
			}
		}
		view, err := svcs.Coordinator.CreateBatch(sess.Key(), manifest)
		if err != nil {
			// Undo the on-disk root reservation: the batch will never
			// exist, so the empty directory must not linger on the
			// desktop's receive disk.
			_ = svcs.Transfers.Store().DiscardFolderRoot(resolvedRoot)
			writeBatchError(w, err)
			return
		}
		// Every item gets a batch-linked upload record. They are owned by the
		// batch decision, so they never show up as standalone approval
		// requests. A failure part-way must leave nothing behind: cancel the
		// records already declared as well as the batch itself.
		declared := make([]string, 0, len(view.Items))
		abort := func(cause error) {
			// A failed reserve loop cannot have streamed content in the
			// pre-approval window (content needs an approved item), so
			// the discard sees an empty directory; nothing is removed
			// if anything ever grew inside it.
			if resolvedRoot != "" {
				_ = svcs.Transfers.Store().DiscardFolderRoot(resolvedRoot)
			}
			for _, id := range declared {
				_ = svcs.Transfers.Cancel(id)
			}
			_, _ = svcs.Coordinator.CancelBatch(sess.Key(), view.ID)
			writeBatchError(w, cause)
		}
		for _, item := range view.Items {
			var root, relative string
			if manifest.Kind == transfer.BatchKindFolder {
				root, relative = resolvedRoot, item.RelativePath
			}
			up, declareErr := svcs.Transfers.DeclareBatchItem(sess.Key(), root, relative, item.DisplayName, item.DeclaredSize)
			if declareErr != nil {
				abort(declareErr)
				return
			}
			declared = append(declared, up.ID)
			if mapErr := svcs.Coordinator.SetItemContentID(sess.Key(), view.ID, item.ID, up.ID); mapErr != nil {
				abort(mapErr)
				return
			}
		}
		writeJSON(w, http.StatusCreated, view)
	}
}

func handleBatchStatus(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil || svcs.Coordinator == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		view, err := svcs.Coordinator.GetBatch(sess.Key(), r.PathValue("id"))
		if err != nil {
			writeBatchError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

// handleBatchItemCancel serves DELETE /api/batches/{id}/items/{itemID}: the
// phone withdraws one item it declared, without canceling the rest of the
// batch. The authenticated session is the only authority: another session's
// (or an unknown) batch/item is answered 404, exactly like a missing one.
// An item whose state cannot be canceled answers 409. Repeating a cancel for
// an already canceled item succeeds, so a retry after a lost response is safe.
//
// The success body is the authoritative batch snapshot, so the phone never has
// to guess whether the item or the batch moved. The matching upload record is
// withdrawn after the coordinator lock is released: a queued record has no
// part file, and an active stream's sink is aborted by the cancellation
// callback, leaving no pin, record or temporary file behind.
func handleBatchItemCancel(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil || svcs.Coordinator == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		batchID, itemID := r.PathValue("id"), r.PathValue("itemID")
		if err := svcs.Coordinator.CancelItem(sess.Key(), batchID, itemID); err != nil {
			writeBatchError(w, err)
			return
		}
		if contentID, idErr := svcs.Coordinator.ItemContentID(sess.Key(), batchID, itemID); idErr == nil {
			_ = svcs.Transfers.Cancel(contentID)
		}
		view, err := svcs.Coordinator.GetBatch(sess.Key(), batchID)
		if err != nil {
			writeBatchError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}
}

func handleBatchContent(svcs *Services, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil || svcs.Coordinator == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		batchID, itemID := r.PathValue("batchID"), r.PathValue("itemID")
		sess.Begin()
		defer sess.End()
		// Claim exactly the named item, and only if it is the queue's next
		// one. A stray request therefore never leaves another item leased.
		lease, err := svcs.Coordinator.ClaimItem(sess.Key(), batchID, itemID)
		if err != nil {
			writeBatchError(w, err)
			return
		}
		diagnose(svcs, "batch item claim session=%s batch=%s item=%s bytes=%d",
			shortKey(sess.Key()), shortID(batchID), shortID(itemID), lease.DeclaredSize)
		contentID, err := svcs.Coordinator.ItemContentID(sess.Key(), batchID, itemID)
		if err != nil {
			failBatch(svcs, sess.Key(), lease, err)
			writeBatchError(w, err)
			return
		}
		if err := svcs.Coordinator.RegisterCancellation(sess.Key(), lease, func() { _ = svcs.Transfers.Cancel(contentID) }); err != nil {
			// A cancel won the race between the claim and this registration.
			// The cancel is authoritative: withdraw the record instead of
			// publishing bytes for an item the phone already withdrew.
			_ = svcs.Transfers.Cancel(contentID)
			writeBatchError(w, err)
			return
		}
		body := &stallBodyReader{ReadCloser: r.Body, rc: http.NewResponseController(w), stall: stall}
		start := time.Now()
		// Quarter marks of large streams are the only mid-stream
		// diagnostics lines; small streams are described by their
		// claim/done lines alone. Both counters live in this handler's
		// closure, so nothing about the protocol changes.
		milestoneBytes := int64(0)
		if svcs.Diagnose != nil && lease.DeclaredSize >= diagMilestoneMinBytes {
			milestoneBytes = lease.DeclaredSize / 4
		}
		lastMilestone := int64(0)
		res, streamErr := svcs.Transfers.SubmitContent(contentID, sess.Key(), body, func(up transfer.Upload) {
			_ = svcs.Coordinator.UpdateItemProgress(sess.Key(), lease, up.Uploaded)
			for lastMilestone < 3 && milestoneBytes > 0 && up.Uploaded >= (lastMilestone+1)*milestoneBytes {
				lastMilestone++
				diagnose(svcs, "batch item progress session=%s item=%s at=%d%%",
					shortKey(sess.Key()), shortID(itemID), lastMilestone*25)
			}
		})
		if streamErr != nil {
			failBatch(svcs, sess.Key(), lease, streamErr)
			diagnose(svcs, "batch item failed session=%s item=%s bytes=%d reason=%s elapsed=%s",
				shortKey(sess.Key()), shortID(itemID), res.Written,
				classifyStreamFailure(r, streamErr), time.Since(start))
			writeUploadStreamError(w, streamErr)
			return
		}
		diagnose(svcs, "batch item done session=%s item=%s bytes=%d elapsed=%s",
			shortKey(sess.Key()), shortID(itemID), res.Written, time.Since(start))
		view, completeErr := svcs.Coordinator.CompleteItem(sess.Key(), lease, res.SHA256)
		if completeErr != nil {
			writeBatchError(w, completeErr)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Written int64              `json:"written"`
			SHA256  string             `json:"sha256"`
			Batch   transfer.BatchView `json:"batch"`
		}{res.Written, res.SHA256, view})
	}
}

// failBatch ends a batch whose content stream failed or was interrupted. The
// item that failed keeps its error, the unstarted items are canceled in both
// the coordinator and the upload service (so no approved record lingers), and
// the session queue stays usable for a fresh batch. If the desktop already
// canceled the batch the lease is gone and nothing further is changed.
func failBatch(svcs *Services, sessionKey string, lease transfer.TransferLease, cause error) {
	view, err := svcs.Coordinator.FailBatch(sessionKey, lease, cause)
	if err != nil {
		return
	}
	for _, item := range view.Items {
		if item.State != transfer.ItemStateCanceled {
			continue
		}
		if contentID, idErr := svcs.Coordinator.ItemContentID(sessionKey, view.ID, item.ID); idErr == nil {
			_ = svcs.Transfers.Cancel(contentID)
		}
	}
}

func writeBatchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, transfer.ErrBatchNotFound), errors.Is(err, transfer.ErrItemNotFound),
		errors.Is(err, transfer.ErrWrongSession):
		writeError(w, http.StatusNotFound, "batch not found")
	case errors.Is(err, transfer.ErrQueuePaused), errors.Is(err, transfer.ErrNoEligibleItem),
		errors.Is(err, transfer.ErrInvalidTransition), errors.Is(err, transfer.ErrLeaseInvalid):
		writeError(w, http.StatusConflict, "batch item is not eligible")
	case errors.Is(err, transfer.ErrInvalidBatch), errors.Is(err, transfer.ErrInvalidFilename),
		errors.Is(err, transfer.ErrInvalidRelativePath):
		writeError(w, http.StatusBadRequest, "invalid batch manifest")
	case errors.Is(err, transfer.ErrNoCapacity):
		writeError(w, http.StatusServiceUnavailable, "transfer capacity unavailable")
	default:
		writeError(w, http.StatusInternalServerError, "batch operation failed")
	}
}

// writeUploadStreamError maps a failed content submission to an honest HTTP
// error. Once streaming begins the response has not been committed yet, so
// precise codes stay possible: sender-caused stream faults are the client's
// fault (400), state-machine refusals are conflicts (409 or 404), capacity
// caps are the server's load condition (503), disk trouble is ours (500).
func writeUploadStreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, transfer.ErrUploadNotFound), errors.Is(err, transfer.ErrWrongSession):
		writeError(w, http.StatusNotFound, "upload not found")
	case errors.Is(err, transfer.ErrNotApproved):
		writeError(w, http.StatusConflict, "upload awaits desktop approval")
	case errors.Is(err, transfer.ErrAlreadyDecided),
		errors.Is(err, transfer.ErrUploadUnderway):
		writeError(w, http.StatusConflict, "upload is no longer in the content-accepting state")
	case errors.Is(err, transfer.ErrCanceled):
		writeError(w, http.StatusConflict, "upload was canceled by the desktop")
	case errors.Is(err, transfer.ErrNoCapacity):
		writeError(w, http.StatusServiceUnavailable, "too many concurrent uploads")
	case errors.Is(err, transfer.ErrSizeExceeded):
		writeError(w, http.StatusBadRequest, "body exceeds the approved size")
	case errors.Is(err, transfer.ErrTruncated):
		writeError(w, http.StatusBadRequest, "body ended before the approved size")
	default:
		writeError(w, http.StatusInternalServerError, "receiving the upload failed")
	}
}

// ---- desktop-shared files ----

// fileJSON is the phone-visible view of a shared file. The disk path stays
// private; the ID is opaque and keys the download endpoint.
type fileJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func toFileJSON(sf transfer.SharedFile) fileJSON {
	return fileJSON{ID: sf.ID, Name: sf.Name, Size: sf.Size}
}

// serveFilesList serves GET /api/files: everything the desktop currently
// shares. Desktop-owned files are shared through explicit desktop actions
// and are visible to any authenticated phone in this stage.
func serveFilesList(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		files := svcs.Registry.List()
		out := make([]fileJSON, len(files))
		for i, sf := range files {
			out[i] = toFileJSON(sf)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// downloadCopySize bounds each chunk the download loop moves between disk
// and network. Small and fixed: bounded memory, progress-friendly.
const downloadCopySize = 64 * 1024

// serveFileDownload serves GET /api/files/{id}/download: streams one file
// with Content-Length and a Content-Disposition carrying the sanitized
// filename. The size reported is the file's size at stream time (the
// registry's Add-time snapshot stays untouched); a file that vanished or
// stopped being a regular file answers 404. The session is pinned so the
// idle timer never aborts a long download.
func serveFileDownload(svcs *Services, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		sf, ok := svcs.Registry.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "shared file not found")
			return
		}

		info, err := os.Stat(sf.Path)
		if err != nil || !info.Mode().IsRegular() {
			writeError(w, http.StatusNotFound, "shared file no longer available")
			return
		}
		f, err := os.Open(sf.Path)
		if err != nil {
			writeError(w, http.StatusNotFound, "shared file no longer available")
			return
		}
		defer f.Close()

		size := info.Size()
		var downloadID string
		if svcs.Downloads != nil {
			dl, trackErr := svcs.Downloads.Start(sf.ID, sf.Name, sess.Key(), size)
			if trackErr != nil {
				writeError(w, http.StatusInternalServerError, "could not start download tracking")
				return
			}
			downloadID = dl.ID
			svcs.Downloads.Begin(downloadID)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Accept-Ranges", "none")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", contentDisposition(sf.Name))
		w.WriteHeader(http.StatusOK)

		// Pin for the streaming duration, then stream. Writes carry a
		// rolling stall deadline: a receiver that stops draining a live
		// download is cut after the stall window.
		sess.Begin()
		defer sess.End()

		out := &stallWriter{
			w:     w,
			rc:    http.NewResponseController(w),
			stall: stall,
		}

		buf := make([]byte, downloadCopySize)
		var written int64
		for written < size {
			n, rerr := f.Read(buf)
			if n > 0 {
				wn, werr := out.Write(buf[:n])
				written += int64(wn)
				if svcs.Downloads != nil {
					svcs.Downloads.Progress(downloadID, written)
				}
				if werr != nil || wn != n {
					// Body is committed; the client sees the cut stream.
					if svcs.Downloads != nil {
						svcs.Downloads.Fail(downloadID, "connection ended before the file was sent", written)
					}
					return
				}
			}
			if rerr != nil {
				if rerr == io.EOF && written == size {
					return
				}
				// Short file (shrank mid-stream) or a disk read error:
				// the headers already declared `size`, so abort the
				// connection rather than letting the client read a
				// complete-appearing but truncated body.
				_ = forceClose(w, f)
				if svcs.Downloads != nil {
					svcs.Downloads.Fail(downloadID, "file changed while it was being sent", written)
				}
				return
			}
		}
		if svcs.Downloads != nil {
			svcs.Downloads.Finish(downloadID, written)
		}
	}
}

// forceClose drops the underlying connection so a shorter-than-declared
// download is never mistaken for a complete one. Best effort.
func forceClose(w http.ResponseWriter, f *os.File) error {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("server: cannot force-close: not a hijacker")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return fmt.Errorf("server: cannot force-close: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// Discard any buffered body chunks; the response was already cut.
	_ = buf.Flush()
	return nil
}

// ---- page routes ----

// serveHome serves the embedded index page.
func serveHome(index []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireGetHead(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(index)
	}
}

// serveHealth reports server liveness as a small JSON document. It carries
// no request-derived data, so there is nothing to leak into logs or output.
func serveHealth(w http.ResponseWriter, r *http.Request) {
	if !requireGetHead(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// serveStatic serves files embedded under /static/<name>. The directory
// listing of /static/ itself is deliberately 404 so the asset tree is not
// exposed.
func serveStatic(assetsRoot fs.FS) http.HandlerFunc {
	files := http.StripPrefix("/static", http.FileServer(http.FS(assetsRoot)))
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireGetHead(w, r) {
			return
		}
		if r.URL.Path == "/static/" || r.URL.Path == "/static" {
			serveNotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	}
}

// serveNotFound answers unknown paths with a JSON 404.
func serveNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

// requireGetHead allows GET and HEAD; anything else gets a 405 response
// with an Allow header, and the caller must stop handling the request.
func requireGetHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET")
	return false
}

// writeJSON writes a JSON response body.
func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// writeUnauthorized answers 401 with a stable code for the session outcome.
func writeUnauthorized(w http.ResponseWriter, err error) {
	code := "invalid_token"
	switch {
	case errors.Is(err, session.ErrExpired):
		code = "session_expired"
	case errors.Is(err, session.ErrRevoked):
		code = "session_revoked"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized", "code": code})
}

// writeError writes a JSON error response akin to http.Error, with nosniff
// protection included.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
