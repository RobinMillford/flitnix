package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

const (
	// defaultArchiveName names an arbitrary multi-source selection.
	defaultArchiveName = "Flitnix-download.zip"

	// progressFlushBytes and progressFlushEvery throttle progress updates
	// while streaming: at most one mutex-guarded update per 256 KiB or per
	// 250 ms, never one per write.
	progressFlushBytes = 256 * 1024
	progressFlushEvery = 250 * time.Millisecond
)

type phoneSourceJSON struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	RootName   string `json:"rootName,omitempty"`
	ItemCount  int    `json:"itemCount"`
	TotalBytes int64  `json:"totalBytes"`
}

func serveSourcesList(svcs *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sources := svcs.Registry.ListSources()
		seen := make(map[string]bool, len(sources))
		out := make([]phoneSourceJSON, 0, len(sources))
		for _, source := range sources {
			seen[source.ID] = true
			var total int64
			for _, item := range source.Items {
				total += item.Size
			}
			out = append(out, phoneSourceJSON{source.ID, string(source.Kind), source.Name, source.RootName, len(source.Items), total})
		}
		for _, file := range svcs.Registry.List() {
			if !seen[file.ID] {
				out = append(out, phoneSourceJSON{file.ID, string(transfer.SourceKindFile), file.Name, "", 1, file.Size})
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

type createGrantRequest struct {
	SourceIDs []string `json:"sourceIds"`
}
type createGrantResponse struct {
	DownloadURL string `json:"downloadUrl"`
	ProgressID  string `json:"progressId"`
	Archive     bool   `json:"archive"`
	Filename    string `json:"filename"`
}

func createDownloadGrant(svcs *Services, grants *DownloadGrantStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var req createGrantRequest
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&req) != nil || len(req.SourceIDs) == 0 || len(req.SourceIDs) > maxGrantSources {
			writeError(w, http.StatusBadRequest, "invalid source selection")
			return
		}
		sources := make([]archiveSource, 0, len(req.SourceIDs))
		seen := make(map[string]bool, len(req.SourceIDs))
		for _, id := range req.SourceIDs {
			if id == "" || seen[id] {
				writeError(w, http.StatusBadRequest, "invalid source selection")
				return
			}
			seen[id] = true
			if source, ok := svcs.Registry.GetSource(id); ok {
				planned := archiveSource{Kind: source.Kind, Name: source.Name, Items: source.Items, Empty: source.EmptyDirs}
				if source.Kind == transfer.SourceKindFile && len(source.Items) == 1 {
					planned.File = transfer.SharedFile{ID: source.ID, Name: source.Name, Path: source.Items[0].Path, Size: source.Items[0].Size}
				}
				sources = append(sources, planned)
			} else if file, ok := svcs.Registry.Get(id); ok {
				sources = append(sources, archiveSource{Kind: transfer.SourceKindFile, Name: file.Name, File: file})
			} else {
				writeError(w, http.StatusNotFound, "source not found")
				return
			}
		}
		grant, progress, err := grants.create(sess, sources, grantFilename(sources))
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: grantCookieName, Value: grant.Secret,
			Path: "/api/downloads/" + grant.ID + "/content", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, MaxAge: int(grantLifetime.Seconds()),
			Secure: r.TLS != nil,
		})
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusCreated, createGrantResponse{
			DownloadURL: "/api/downloads/" + grant.ID + "/content",
			ProgressID:  progress.ID, Archive: grant.Archive, Filename: grant.Filename,
		})
	}
}

func serveDownloadProgress(svcs *Services, grants *DownloadGrantStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFrom(r)
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		p, ok := grants.getProgress(r.PathValue("id"), sess.Key())
		if !ok {
			writeError(w, http.StatusNotFound, "progress not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": p.ID, "totalBytes": p.Total, "processedBytes": p.Processed,
			"state": p.State, "error": p.Error,
		})
	}
}

func buildArchiveEntries(sources []archiveSource) ([]archiveEntry, int64, error) {
	if len(sources) == 0 || len(sources) > maxGrantSources {
		return nil, 0, errors.New("invalid source selection")
	}
	used := make(map[string]bool)
	var entries []archiveEntry
	var total int64
	for _, source := range sources {
		if source.Kind == transfer.SourceKindFolder {
			root := uniqueName(source.Name, used)
			used[root+"/"] = true
			for _, dir := range source.Empty {
				name, err := safeArchivePath(root + "/" + dir + "/")
				if err != nil {
					return nil, 0, err
				}
				if !used[name] {
					used[name] = true
					entries = append(entries, archiveEntry{Name: name, Dir: true})
				}
			}
			for _, item := range source.Items {
				if item.Size < 0 || total > (int64(^uint64(0)>>1))-item.Size {
					return nil, 0, errors.New("archive size overflow")
				}
				name, err := safeArchivePath(root + "/" + item.RelativePath)
				if err != nil {
					return nil, 0, err
				}
				name = uniqueArchiveName(name, used)
				used[name] = true
				entries = append(entries, archiveEntry{Name: name, Path: item.Path, Size: item.Size})
				total += item.Size
			}
			if len(source.Items) == 0 && len(source.Empty) == 0 {
				entries = append(entries, archiveEntry{Name: root + "/", Dir: true})
			}
		} else {
			if source.File.Size < 0 || total > (int64(^uint64(0)>>1))-source.File.Size {
				return nil, 0, errors.New("archive size overflow")
			}
			name := uniqueArchiveName(source.File.Name, used)
			if err := validateArchiveName(name); err != nil {
				return nil, 0, err
			}
			used[name] = true
			entries = append(entries, archiveEntry{Name: name, Path: source.File.Path, Size: source.File.Size})
			total += source.File.Size
		}
		if len(entries) > transfer.MaxSourceItems+transfer.MaxSourceEmptyDirs {
			return nil, 0, errors.New("archive contains too many entries")
		}
	}
	return entries, total, nil
}

func uniqueName(name string, used map[string]bool) string {
	for i := 0; ; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)", name, i)
		}
		if !used[candidate+"/"] {
			return candidate
		}
	}
}

func uniqueArchiveName(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !used[candidate] {
			return candidate
		}
	}
}

func validateArchiveName(name string) error {
	if name == "" || len(name) > transfer.MaxRelativePathBytes || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
		return errors.New("invalid archive entry name")
	}
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if part == "" || part == "." || part == ".." || strings.Contains(part, ":") {
			return errors.New("invalid archive entry name")
		}
	}
	return nil
}

func safeArchivePath(name string) (string, error) {
	name = strings.ReplaceAll(name, `\`, "/")
	if err := validateArchiveName(name); err != nil {
		return "", err
	}
	return name, nil
}

// ---- download filenames ----

// grantFilename chooses the browser's suggested save name from trusted,
// server-side source metadata only:
//
//   - one file: that file's name;
//   - one folder: the folder's own name plus ".zip";
//   - any other selection: defaultArchiveName.
//
// The phone never supplies it, and a name the user edits in a save dialog
// stays purely local.
func grantFilename(sources []archiveSource) string {
	if len(sources) == 1 {
		switch source := sources[0]; source.Kind {
		case transfer.SourceKindFile:
			if name := cleanDownloadName(source.File.Name); name != "" {
				return name
			}
			return "download"
		case transfer.SourceKindFolder:
			if name := zipFilename(source.Name); name != "" {
				return name
			}
		}
	}
	return defaultArchiveName
}

// zipFilename turns a folder name into "<name>.zip", clamped so the result
// stays within the filename length limit used everywhere else.
func zipFilename(base string) string {
	const ext = ".zip"
	name := cleanDownloadName(base)
	if name == "" {
		return ""
	}
	if limit := transfer.MaxFilenameBytes - len(ext); len(name) > limit {
		name = strings.TrimSpace(truncateUTF8(name, limit))
		if name == "" {
			return ""
		}
	}
	return name + ext
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cleanDownloadName makes a display name safe to place in a header and to
// hand a browser as a save name: invalid UTF-8 is repaired, control
// characters and path separators become '_', surrounding space is trimmed,
// and names that are empty or only dots are rejected (returned as "").
func cleanDownloadName(raw string) string {
	raw = strings.ToValidUTF8(raw, "_")
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r == '/' || r == '\\' || unicode.IsControl(r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	name := strings.TrimSpace(b.String())
	if strings.Trim(name, ".") == "" {
		return ""
	}
	return name
}

// contentDisposition builds an attachment header for a display name. It
// never embeds the raw name: the name is cleaned first, then emitted as a
// quoted ASCII fallback (via mime.FormatMediaType, which handles quoting)
// plus, whenever the fallback is lossy, an RFC 5987/6266 filename* parameter
// carrying the exact UTF-8 name. Modern browsers use filename*; old ones use
// the fallback.
func contentDisposition(name string) string {
	name = cleanDownloadName(name)
	if name == "" {
		name = "download"
	}
	fallback := asciiFallback(name)
	value := mime.FormatMediaType("attachment", map[string]string{"filename": fallback})
	if value == "" {
		value = "attachment"
	}
	if fallback != name {
		value += "; filename*=UTF-8''" + encodeRFC5987(name)
	}
	return value
}

func asciiFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r > 0x7e, r == '"', r == '\\', r == '/', r == '%':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// encodeRFC5987 percent-encodes every byte outside the RFC 5987 attr-char set.
func encodeRFC5987(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte("!#$&+-.^_`|~", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// ---- streaming ----

// Public, path-free reasons recorded on a failed download. Errors from the
// filesystem carry absolute paths, so they are never stored or sent.
var (
	errDownloadCanceled  = errors.New("the download was canceled")
	errDownloadSession   = errors.New("the session ended during the download")
	errSourceUnavailable = errors.New("source file is unavailable")
	errSourceChanged     = errors.New("source file changed")
)

// writeFailure marks an error that came from writing to the browser (the
// peer went away or stalled) rather than from reading a source.
type writeFailure struct{ err error }

func (e writeFailure) Error() string { return "the connection ended before the download finished" }
func (e writeFailure) Unwrap() error { return e.err }

// failWriter tags every write error as a writeFailure.
type failWriter struct{ w io.Writer }

func (f failWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		err = writeFailure{err}
	}
	return n, err
}

// publicDownloadError maps any streaming error to a message that is safe to
// show the phone.
func publicDownloadError(err error) string {
	var wf writeFailure
	switch {
	case errors.As(err, &wf):
		return wf.Error()
	case errors.Is(err, errDownloadCanceled), errors.Is(err, errDownloadSession),
		errors.Is(err, errSourceUnavailable), errors.Is(err, errSourceChanged):
		for _, known := range []error{errDownloadCanceled, errDownloadSession, errSourceUnavailable, errSourceChanged} {
			if errors.Is(err, known) {
				return known.Error()
			}
		}
	}
	return "the download could not be completed"
}

// guardedReader sits between a source file and the response. Each Read it
// stops the stream if the client disconnected or the session was revoked,
// converts raw filesystem errors into path-free ones, and reports progress
// in throttled batches (never per write).
type guardedReader struct {
	r       io.Reader
	ctx     context.Context
	sess    *session.Session
	p       *archiveProgress
	pending int64
	last    time.Time
}

func (g *guardedReader) Read(b []byte) (int, error) {
	if g.ctx.Err() != nil {
		return 0, errDownloadCanceled
	}
	if g.sess != nil && g.sess.Status() == session.StatusRevoked {
		return 0, errDownloadSession
	}
	n, err := g.r.Read(b)
	if err != nil && err != io.EOF {
		err = errSourceUnavailable
	}
	if n > 0 {
		g.pending += int64(n)
		if g.pending >= progressFlushBytes || time.Since(g.last) >= progressFlushEvery {
			g.flush()
		}
	}
	return n, err
}

func (g *guardedReader) flush() {
	if g.pending > 0 {
		updateProgress(g.p, g.pending)
		g.pending = 0
	}
	g.last = time.Now()
}

func updateProgress(p *archiveProgress, n int64) {
	p.mu.Lock()
	p.Processed += n
	p.Updated = p.now()
	p.mu.Unlock()
}

func setProgressFailed(p *archiveProgress, reason string) {
	p.mu.Lock()
	p.State, p.Error, p.Updated = progressFailed, reason, p.now()
	p.mu.Unlock()
}

// finishProgress records the terminal state. A nil err is "completed" and is
// only ever passed after the whole body, including the ZIP central directory,
// was written and flushed to the connection.
func finishProgress(p *archiveProgress, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Updated = p.now()
	if err == nil {
		p.State, p.Error = progressCompleted, ""
		return
	}
	p.State, p.Error = progressFailed, publicDownloadError(err)
}

// flushResponse pushes buffered bytes to the connection so a write failure in
// the tail of the body is observed here instead of after the handler claims
// success.
func flushResponse(w http.ResponseWriter) error {
	if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return writeFailure{err}
	}
	return nil
}

// writeArchive streams the grant's immutable plan as an uncompressed ZIP
// directly into out. It never buffers the archive. On any error it returns
// WITHOUT closing the zip writer: a central directory written after a failure
// would make a truncated archive look complete.
func writeArchive(ctx context.Context, out io.Writer, grant downloadGrant, progress *archiveProgress) error {
	zw := zip.NewWriter(out)
	buf := make([]byte, downloadCopySize)
	for _, entry := range grant.Entries {
		if ctx.Err() != nil {
			return errDownloadCanceled
		}
		header := &zip.FileHeader{Name: entry.Name, Method: zip.Store}
		if entry.Dir {
			header.SetMode(0o755 | os.ModeDir)
			if _, err := zw.CreateHeader(header); err != nil {
				return err
			}
			continue
		}
		if err := writeArchiveFile(ctx, zw, header, entry, grant.Sess, progress, buf); err != nil {
			return err
		}
	}
	return zw.Close()
}

func writeArchiveFile(ctx context.Context, zw *zip.Writer, header *zip.FileHeader, entry archiveEntry, sess *session.Session, progress *archiveProgress, buf []byte) error {
	f, err := openTrustedSource(entry.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr == nil {
		header.Modified = info.ModTime()
	}
	part, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	// The plan fixed this entry's size. Reading no more than that keeps a
	// growing file from changing the archive; reading less is a changed file.
	src := &guardedReader{r: io.LimitReader(f, entry.Size), ctx: ctx, sess: sess, p: progress}
	n, err := io.CopyBuffer(part, src, buf)
	src.flush()
	if err != nil {
		return err
	}
	if n != entry.Size {
		return errSourceChanged
	}
	return nil
}

// abortResponse tears the connection down without a clean end-of-body, so the
// browser reports the download as failed instead of saving a truncated file.
func abortResponse() { panic(http.ErrAbortHandler) }

func serveArchiveDownload(svcs *Services, grants *DownloadGrantStore, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only GET redeems a grant. HEAD (or a probe) must not consume the
		// one-use grant the real navigation needs.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cookie, err := r.Cookie(grantCookieName)
		if err != nil {
			http.Error(w, "download unavailable", http.StatusUnauthorized)
			return
		}
		grant, progress, err := grants.redeem(r.PathValue("id"), cookie.Value)
		if err != nil {
			switch {
			case errors.Is(err, errGrantExpired):
				http.Error(w, "download expired", http.StatusGone)
			case errors.Is(err, errGrantBusy):
				http.Error(w, "another download is active", http.StatusConflict)
			case errors.Is(err, errGrantSession):
				http.Error(w, "session ended", http.StatusUnauthorized)
			default:
				http.Error(w, "download unavailable", http.StatusForbidden)
			}
			return
		}
		defer grants.finish(grant)
		// The stream is an authenticated transfer: pin the session so idle
		// expiry cannot cut it, and restart the idle window when it ends.
		grant.Sess.Begin()
		defer grant.Sess.End()

		out := failWriter{&stallWriter{w: w, rc: http.NewResponseController(w), stall: stall}}
		h := w.Header()
		h.Set("Content-Disposition", contentDisposition(grant.Filename))
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Accept-Ranges", "none")

		started := true
		if !grant.Archive {
			started, err = streamSingleFile(r.Context(), w, out, grant, progress)
		} else {
			h.Set("Content-Type", "application/zip")
			w.WriteHeader(http.StatusOK)
			err = writeArchive(r.Context(), out, grant, progress)
			if err == nil {
				err = flushResponse(w)
			}
		}
		finishProgress(progress, err)
		if err != nil && started {
			abortResponse()
		}
	}
}

// streamSingleFile serves one regular file with a Content-Length so a short
// body is detectable by the browser. started reports whether the status line
// was committed; when false an error response was already sent and the
// connection can end normally.
func streamSingleFile(ctx context.Context, w http.ResponseWriter, out io.Writer, grant downloadGrant, progress *archiveProgress) (started bool, err error) {
	entry := grant.Entries[0]
	f, err := openTrustedSource(entry.Path)
	if err != nil {
		http.Error(w, "source unavailable", http.StatusGone)
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "source unavailable", http.StatusGone)
		return false, errSourceUnavailable
	}
	size := info.Size()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	src := &guardedReader{r: io.LimitReader(f, size), ctx: ctx, sess: grant.Sess, p: progress}
	n, err := io.CopyBuffer(out, src, make([]byte, downloadCopySize))
	src.flush()
	if err != nil {
		return true, err
	}
	if n != size {
		return true, errSourceChanged
	}
	return true, flushResponse(w)
}

// openTrustedSource opens a planned source for reading. It refuses anything
// that is not a regular file at the moment of opening (symlinks, devices,
// sockets) and re-checks the opened handle against the lstat result so a swap
// between the two is caught. Errors carry no filesystem path.
func openTrustedSource(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errSourceUnavailable
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errSourceUnavailable
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		_ = f.Close()
		return nil, errSourceChanged
	}
	return f, nil
}
