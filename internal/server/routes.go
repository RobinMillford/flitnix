package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/textmsg"
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
	mux.HandleFunc("/ping", handleDiagnosticsPing(services))
	mux.HandleFunc("/static/", serveStatic(assetsRoot))

	guestIndex, err := pageAssets.ReadFile("assets/guest.html")
	if err == nil && len(guestIndex) > 0 {
		mux.HandleFunc("GET /guest", serveHome(guestIndex))
	} else {
		mux.HandleFunc("GET /guest", serveHome(index))
	}

	mux.HandleFunc("/", serveNotFound)

	if services == nil {
		// Stages before the transfer endpoints keep deterministic page-only
		// semantics: no API surface, everything under /api/ is a 404.
		mux.HandleFunc("/api/", serveNotFound)
		mux.HandleFunc("/guest-api/", serveNotFound)
	} else {
		// The whole API router sits behind one session check, so unknown
		// /api/* paths and method mismatches are answered only after
		// authentication; a token holder learns nothing about the
		// endpoint set without a live session either way.
		api := newAPIMux(services, stall, grants)
		mux.Handle("/api/", requireSession(services, api.ServeHTTP))
		mux.HandleFunc("GET /api/downloads/{id}/content", serveArchiveDownload(services, grants, downloadStall))

		if services.GuestManager != nil {
			guestAPI := newGuestAPIMux(services, stall)
			mux.Handle("/guest-api/", guestAPI)
		} else {
			mux.HandleFunc("/guest-api/", serveNotFound)
		}
	}
	return securityMiddleware(mux)
}

// parseHostPort safely parses an HTTP Host authority string into host and port.
// It strictly validates square bracket formatting for IPv6 literals and rejects
// malformed syntax (e.g. unclosed brackets, extra colons, invalid ports).
func parseHostPort(raw string) (host string, port string, isBracketed bool, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", false, errors.New("empty host")
	}

	// Case 1: Bracketed host (IPv6 literal, e.g. [2001:db8::1] or [2001:db8::1]:43821)
	if strings.HasPrefix(s, "[") {
		closeIdx := strings.Index(s, "]")
		if closeIdx == -1 {
			return "", "", true, errors.New("missing closing bracket in host")
		}
		if strings.Count(s, "[") != 1 || strings.Count(s, "]") != 1 {
			return "", "", true, errors.New("invalid bracket syntax in host")
		}
		inside := s[1:closeIdx]
		if inside == "" {
			return "", "", true, errors.New("empty bracketed host")
		}
		host = inside
		remainder := s[closeIdx+1:]
		if remainder != "" {
			if !strings.HasPrefix(remainder, ":") {
				return "", "", true, errors.New("unexpected characters after closing bracket")
			}
			portStr := remainder[1:]
			if portStr == "" {
				return "", "", true, errors.New("empty port after colon")
			}
			p, perr := strconv.Atoi(portStr)
			if perr != nil || p < 1 || p > 65535 {
				return "", "", true, fmt.Errorf("invalid port %q", portStr)
			}
			port = portStr
		}
		return host, port, true, nil
	}

	// Case 2: Unbracketed host. Must not contain rogue brackets.
	if strings.ContainsAny(s, "[]") {
		return "", "", false, errors.New("invalid bracket in unbracketed host")
	}

	// If it contains a colon, it could be IPv4 host:port or unbracketed IPv6
	if strings.Contains(s, ":") {
		h, p, splitErr := net.SplitHostPort(s)
		if splitErr == nil {
			if p != "" {
				portNum, perr := strconv.Atoi(p)
				if perr != nil || portNum < 1 || portNum > 65535 {
					return "", "", false, fmt.Errorf("invalid port %q", p)
				}
			}
			return h, p, false, nil
		}
		// If SplitHostPort fails, check if entire string is an unbracketed IPv6 literal
		if addr, perr := netip.ParseAddr(s); perr == nil && addr.Is6() {
			return s, "", false, nil
		}
		return "", "", false, splitErr
	}

	return s, "", false, nil
}

// isAllowedHost validates that the request Host header matches an IPv4/IPv6
// address, localhost, or RFC 2606 test domains (for httptest). Any arbitrary
// DNS hostname is rejected to prevent DNS rebinding attacks.
func isAllowedHost(hostHeader string) bool {
	host, _, isBracketed, err := parseHostPort(hostHeader)
	if err != nil || host == "" {
		return false
	}

	// Strip IPv6 zone index if present (e.g. fe80::1%eth0 or fe80::1%25eth0)
	cleanHost := host
	hasZone := false
	if zoneIdx := strings.Index(cleanHost, "%"); zoneIdx != -1 {
		hasZone = true
		cleanHost = cleanHost[:zoneIdx]
	}

	// Check if valid IP literal
	if addr, perr := netip.ParseAddr(cleanHost); perr == nil {
		if isBracketed && !addr.Is6() {
			// RFC 3986 specifies brackets only for IPv6 literals
			return false
		}
		if hasZone && !addr.Is6() {
			// Zone identifier is only valid on IPv6
			return false
		}
		return true
	}

	// Hostnames cannot be bracketed or contain zone identifiers
	if isBracketed || hasZone {
		return false
	}

	// Allowed test/local domain names for development and automated testing
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return true
	}
	if lower == "example.com" || strings.HasSuffix(lower, ".example.com") || strings.HasSuffix(lower, ".test") {
		return true
	}

	return false
}

// equalHostPort compares two Host authority strings (e.g. from Origin and Host headers),
// accounting for IPv6 normalization, bracket syntax, and case-insensitivity.
func equalHostPort(h1, h2 string) bool {
	host1, port1, _, err1 := parseHostPort(h1)
	host2, port2, _, err2 := parseHostPort(h2)
	if err1 != nil || err2 != nil {
		return strings.EqualFold(h1, h2)
	}

	if port1 != port2 {
		return false
	}

	c1 := host1
	if idx := strings.Index(c1, "%"); idx != -1 {
		c1 = c1[:idx]
	}
	c2 := host2
	if idx := strings.Index(c2, "%"); idx != -1 {
		c2 = c2[:idx]
	}

	ip1, err1 := netip.ParseAddr(c1)
	ip2, err2 := netip.ParseAddr(c2)
	if err1 == nil && err2 == nil {
		if ip1.Is4In6() {
			ip1 = ip1.Unmap()
		}
		if ip2.Is4In6() {
			ip2 = ip2.Unmap()
		}
		return ip1 == ip2
	}

	return strings.EqualFold(host1, host2)
}

// securityMiddleware rejects requests with untrusted Host headers (DNS rebinding)
// and rejects cross-origin state-changing requests (CSRF).
func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedHost(r.Host) {
			writeError(w, http.StatusBadRequest, "invalid host header")
			return
		}

		// Cross-origin defense for state-changing operations
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				writeError(w, http.StatusForbidden, "cross-origin request forbidden")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				if origin == "null" {
					writeError(w, http.StatusForbidden, "cross-origin request forbidden")
					return
				}
				u, err := url.Parse(origin)
				if err != nil || u.Host == "" || !equalHostPort(u.Host, r.Host) {
					writeError(w, http.StatusForbidden, "cross-origin request forbidden")
					return
				}
			}
		}

		next.ServeHTTP(w, r)
	})
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
	mux.HandleFunc("POST /api/text", handleTextMessageSend(services))
	mux.HandleFunc("GET /api/text", handleTextMessageList(services))
	mux.HandleFunc("GET /api/diagnostics/ping", handleDiagnosticsPing(services))
	mux.HandleFunc("POST /api/diagnostics/ping", handleDiagnosticsPing(services))
	return mux
}

func handleDiagnosticsPing(services *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if services != nil {
			services.RecordContact("ping")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    "ok",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	}
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
func requireSession(svcs *Services, next http.HandlerFunc) http.Handler {
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
		sess, err := svcs.Sessions.Validate(token)
		if err != nil {
			// The message stays generic. The machine-readable code lets the
			// phone page tell a session that really ended (expired or
			// revoked) from a token it never had; it exposes nothing about
			// the token itself and is only produced for a token that was
			// once valid.
			writeUnauthorized(w, err)
			return
		}
		svcs.RecordContact("api")
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
//
// A withdrawal can arrive while the item's bytes are being published. The
// upload record is the only place where those two decisions are ordered, so
// the request asks it first (ReserveCancel) and only cancels the coordinator
// item if the cancel actually won. An item that was already published keeps
// its completed state, its completed record and its file, and the phone is
// answered 409 so it refreshes into that authoritative snapshot instead of
// showing a cancellation that the receive folder contradicts.
func handleBatchItemCancel(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil || svcs.Coordinator == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		batchID, itemID := r.PathValue("id"), r.PathValue("itemID")
		var contentID string
		if id, idErr := svcs.Coordinator.ItemContentID(sess.Key(), batchID, itemID); idErr == nil {
			contentID = id
		}
		if contentID != "" {
			authority, err := svcs.Transfers.ReserveCancel(contentID)
			switch {
			case errors.Is(err, transfer.ErrUploadNotFound):
				// The record was already pruned, so nothing of its own is
				// publishing: the batch item is the only authority left.
			case authority != transfer.CancelWins:
				// Publication (or the end of the upload) already decided this
				// item. Leave every layer describing that outcome: rewriting
				// the coordinator item now would contradict the file.
				writeError(w, http.StatusConflict, cancelRefusal(authority, err))
				return
			case err != nil:
				writeBatchError(w, err)
				return
			}
		}
		if err := svcs.Coordinator.CancelItem(sess.Key(), batchID, itemID); err != nil {
			writeBatchError(w, err)
			return
		}
		if contentID != "" {
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

// cancelRefusal words the 409 that a withdrawal arriving after its item's
// bytes were already published (or after the upload ended another way) gets.
// Both cases use the existing error body shape, so the phone's existing
// "refresh and show the authoritative snapshot" path handles them unchanged.
func cancelRefusal(authority transfer.CancelAuthority, err error) string {
	if authority == transfer.CancelLostToPublication {
		return "the file was already received; it cannot be canceled"
	}
	if err != nil {
		return "the upload has already ended"
	}
	return "batch item is not eligible"
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
		res, streamErr := svcs.Transfers.SubmitContentWithPublish(contentID, sess.Key(), body, func(up transfer.Upload) {
			_ = svcs.Coordinator.UpdateItemProgress(sess.Key(), lease, up.Uploaded)
			for lastMilestone < 3 && milestoneBytes > 0 && up.Uploaded >= (lastMilestone+1)*milestoneBytes {
				lastMilestone++
				diagnose(svcs, "batch item progress session=%s item=%s at=%d%%",
					shortKey(sess.Key()), shortID(itemID), lastMilestone*25)
			}
		}, func() error {
			return svcs.Coordinator.MarkItemPublishing(sess.Key(), lease)
		})
		if errors.Is(streamErr, transfer.ErrCanceled) {
			// The upload record was withdrawn while this stream owned it, so
			// the cancellation is the intended outcome rather than a fault:
			// keep the item canceled instead of failing the whole batch. If
			// the withdrawal already reached the coordinator this is the
			// idempotent no-op; if this path gets there first, it performs the
			// cancellation the request asked for (and that later request then
			// finds the item already canceled).
			_ = svcs.Coordinator.CancelItem(sess.Key(), batchID, itemID)
			diagnose(svcs, "batch item canceled session=%s item=%s bytes=%d elapsed=%s",
				shortKey(sess.Key()), shortID(itemID), res.Written, time.Since(start))
			writeUploadStreamError(w, streamErr)
			return
		}
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

		f, err := openTrustedSource(sf.Path)
		if err != nil {
			writeError(w, http.StatusNotFound, "shared file no longer available")
			return
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			writeError(w, http.StatusNotFound, "shared file no longer available")
			return
		}

		size := info.Size()

		// Pass 1: Compute the SHA-256 digest of the actual source file using a bounded-memory streaming read
		hasher := sha256.New()
		buf := make([]byte, downloadCopySize)
		nPass1, err := io.CopyBuffer(hasher, f, buf)
		if err != nil || nPass1 != size {
			writeError(w, http.StatusGone, "shared file changed or unreadable")
			return
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rewind file stream")
			return
		}
		reopenedInfo, err := f.Stat()
		if err != nil || reopenedInfo.Size() != size || reopenedInfo.ModTime() != info.ModTime() {
			writeError(w, http.StatusGone, "shared file changed during preparation")
			return
		}
		expectedSHA256 := hex.EncodeToString(hasher.Sum(nil))

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
		if digestHdr := transfer.FormatContentDigest(expectedSHA256); digestHdr != "" {
			w.Header().Set("Content-Digest", digestHdr)
		}
		w.Header().Set("X-Content-SHA256", expectedSHA256)
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

		streamHasher := sha256.New()
		var written int64
		for written < size {
			// A direct download is an authenticated transfer, so a session
			// that was revoked while the body was streaming must stop here
			// — the same policy the archive path applies through
			// guardedReader (errDownloadSession). Without this check a
			// revoked phone kept receiving every remaining byte and the
			// desktop recorded the transfer as `sent`, which made a dead
			// session look like a completed one. The response status line
			// is already committed, so the connection is cut instead of
			// sealed and the client sees a short body, not a whole file.
			if sess.Status() == session.StatusRevoked {
				_ = forceClose(w, f)
				if svcs.Downloads != nil {
					svcs.Downloads.Fail(downloadID, errDownloadSession.Error(), written)
				}
				return
			}
			n, rerr := f.Read(buf)
			if n > 0 {
				streamHasher.Write(buf[:n])
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
					break
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

		actualSHA256 := hex.EncodeToString(streamHasher.Sum(nil))
		if actualSHA256 != expectedSHA256 {
			_ = forceClose(w, f)
			if svcs.Downloads != nil {
				svcs.Downloads.Fail(downloadID, "file checksum changed while it was being sent", written)
			}
			return
		}

		if svcs.Downloads != nil {
			svcs.Downloads.FinishWithDigest(downloadID, written, expectedSHA256)
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

type textSendRequest struct {
	Content string `json:"content"`
}

func handleTextMessageSend(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if svcs.TextMessages == nil {
			writeError(w, http.StatusNotFound, "text messaging unavailable")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, textmsg.MaxContentBytes+1024))
		if err != nil {
			writeError(w, http.StatusBadRequest, "reading message body")
			return
		}

		if !utf8.Valid(body) {
			writeError(w, http.StatusBadRequest, "invalid UTF-8 content")
			return
		}

		var req textSendRequest
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid message JSON")
			return
		}

		msg, err := svcs.TextMessages.Add(sess.Key(), "phone", req.Content)
		if err != nil {
			switch {
			case errors.Is(err, textmsg.ErrEmptyContent):
				writeError(w, http.StatusBadRequest, "empty message")
			case errors.Is(err, textmsg.ErrContentTooLarge):
				writeError(w, http.StatusRequestEntityTooLarge, "message exceeds 64 KiB")
			case errors.Is(err, textmsg.ErrInvalidUTF8):
				writeError(w, http.StatusBadRequest, "invalid UTF-8 content")
			default:
				writeError(w, http.StatusInternalServerError, "failed to save message")
			}
			return
		}

		writeJSON(w, http.StatusCreated, msg)
	}
}

func handleTextMessageList(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if svcs.TextMessages == nil {
			writeJSON(w, http.StatusOK, map[string]any{"messages": []textmsg.Message{}})
			return
		}

		msgs := svcs.TextMessages.List(sess.Key())
		writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
	}
}
