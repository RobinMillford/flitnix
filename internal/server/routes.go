package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
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
//	GET  /api/files                   lists the desktop-shared files
//	GET  /api/files/{id}/download     streams one shared file
//
// Method handling is delegated to ServeMux's method-prefixed patterns: a
// method mismatch on a registered path yields 405 with an Allow header, and
// a GET registration also serves HEAD (the body is stripped by net/http).
// Everything below /api/ that is unknown yields 404 within the API router.
// With a nil Services every /api/ path answers 404 (page-only server).
func newRouter(services *Services, stall time.Duration) http.Handler {
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
		api := newAPIMux(services, stall)
		mux.Handle("/api/", requireSession(services.Sessions, api.ServeHTTP))
	}
	return mux
}

// newAPIMux registers the transfer endpoints. Authentication is applied at
// the /api/ boundary by newRouter, so nothing reaches these handlers
// without an authenticated session in the request context.
func newAPIMux(services *Services, stall time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/uploads", handleUploadDeclare(services, stall))
	mux.HandleFunc("GET /api/uploads/{id}", handleUploadStatus(services))
	mux.HandleFunc("PUT /api/uploads/{id}/content", handleUploadContent(services, stall))
	mux.HandleFunc("GET /api/files", serveFilesList(services))
	mux.HandleFunc("GET /api/files/{id}/download", serveFileDownload(services, stall))
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
			// Missing vs expired vs revoked all collapse here; nothing
			// session-specific is recorded or reported.
			writeError(w, http.StatusUnauthorized, "unauthorized")
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

		// Pin the session: an idle-expiry while a multi-gigabyte file
		// streams in would otherwise abort it.
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
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Accept-Ranges", "none")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", mime.FormatMediaType(
			"attachment", map[string]string{"filename": sf.Name}))
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
				if werr != nil || wn != n {
					// Body is committed; the client sees the cut stream.
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
				return
			}
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

// writeError writes a JSON error response akin to http.Error, with nosniff
// protection included.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
