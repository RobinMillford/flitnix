package server

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

const (
	grantLifetime   = 60 * time.Second
	maxGrantSources = 512
	grantCookieName = "flitnix-download-grant"
)

type archiveSource struct {
	Kind  transfer.SourceKind
	Name  string
	Items []transfer.SourceItem
	Empty []string
	File  transfer.SharedFile
}

type archiveEntry struct {
	Name string
	Path string
	Size int64
	Dir  bool
}

type downloadGrant struct {
	ID, Secret, Session, Filename, Progress string
	Entries                                 []archiveEntry
	Total                                   int64
	Archive                                 bool
	ExpiresAt                               time.Time
}

type archiveProgress struct {
	mu                        sync.Mutex
	ID, Session, State, Error string
	Total, Processed          int64
	Updated                   time.Time
}

// DownloadGrantStore holds deliberately short-lived, in-memory browser grants.
type DownloadGrantStore struct {
	mu       sync.Mutex
	grants   map[string]downloadGrant
	progress map[string]*archiveProgress
	active   map[string]int
}

func newGrantStore() *DownloadGrantStore {
	return &DownloadGrantStore{
		grants:   make(map[string]downloadGrant),
		progress: make(map[string]*archiveProgress),
		active:   make(map[string]int),
	}
}

func randomGrantValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *DownloadGrantStore) create(session string, sources []archiveSource, filename string) (downloadGrant, *archiveProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[session] != 0 {
		return downloadGrant{}, nil, errors.New("archive download already active")
	}
	entries, total, err := buildArchiveEntries(sources)
	if err != nil {
		return downloadGrant{}, nil, err
	}
	id, err := randomGrantValue()
	if err != nil {
		return downloadGrant{}, nil, err
	}
	secret, err := randomGrantValue()
	if err != nil {
		return downloadGrant{}, nil, err
	}
	progressID, err := randomGrantValue()
	if err != nil {
		return downloadGrant{}, nil, err
	}
	now := time.Now()
	grant := downloadGrant{
		ID: id, Secret: secret, Session: session, Filename: filename,
		Progress: progressID, Entries: entries, Total: total,
		Archive:   len(sources) != 1 || sources[0].Kind != transfer.SourceKindFile,
		ExpiresAt: now.Add(grantLifetime),
	}
	progress := &archiveProgress{ID: progressID, Session: session, Total: total, State: "pending", Updated: now}
	s.grants[id], s.progress[progressID] = grant, progress
	return grant, progress, nil
}

func (s *DownloadGrantStore) redeem(id, secret string) (downloadGrant, *archiveProgress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[id]
	if !ok || grant.Secret != secret || time.Now().After(grant.ExpiresAt) || s.active[grant.Session] != 0 {
		return downloadGrant{}, nil, false
	}
	delete(s.grants, id)
	s.active[grant.Session]++
	p := s.progress[grant.Progress]
	p.mu.Lock()
	p.State, p.Updated = "transferring", time.Now()
	p.mu.Unlock()
	return grant, p, true
}

func (s *DownloadGrantStore) finish(grant downloadGrant) {
	s.mu.Lock()
	if s.active[grant.Session] > 0 {
		s.active[grant.Session]--
	}
	s.mu.Unlock()
}

func (s *DownloadGrantStore) getProgress(id, session string) (archiveProgress, bool) {
	s.mu.Lock()
	p := s.progress[id]
	s.mu.Unlock()
	if p == nil {
		return archiveProgress{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Session != session {
		return archiveProgress{}, false
	}
	return archiveProgress{
		ID: p.ID, Session: p.Session, State: p.State, Error: p.Error,
		Total: p.Total, Processed: p.Processed, Updated: p.Updated,
	}, true
}

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
		filename := "Flitnix-download.zip"
		if len(sources) == 1 && sources[0].Kind == transfer.SourceKindFile {
			filename = sources[0].File.Name
		}
		grant, progress, err := grants.create(sess.Key(), sources, filename)
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

func archiveContentDisposition(name string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": name})
}

func streamArchive(w http.ResponseWriter, grant downloadGrant, progress *archiveProgress, stall time.Duration) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", archiveContentDisposition(grant.Filename))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	zw := zip.NewWriter(&stallWriter{w: w, rc: http.NewResponseController(w), stall: stall})
	for _, entry := range grant.Entries {
		header := &zip.FileHeader{Name: entry.Name, Method: zip.Store}
		if entry.Dir {
			header.SetMode(0o755 | os.ModeDir)
			if _, err := zw.CreateHeader(header); err != nil {
				setProgressError(progress, err)
				break
			}
			continue
		}
		f, err := openTrustedSource(entry.Path)
		if err == nil {
			var part io.Writer
			part, err = zw.CreateHeader(header)
			if err == nil {
				_, err = io.Copy(part, f)
			}
			_ = f.Close()
		}
		if err != nil {
			setProgressError(progress, err)
			break
		}
		updateProgress(progress, entry.Size)
	}
	if err := zw.Close(); err != nil {
		setProgressError(progress, err)
	}
	progress.mu.Lock()
	if progress.Error == "" {
		progress.State = "completed"
	} else {
		progress.State = "failed"
	}
	progress.Updated = time.Now()
	progress.mu.Unlock()
}

func updateProgress(p *archiveProgress, n int64) {
	p.mu.Lock()
	p.Processed += n
	p.Updated = time.Now()
	p.mu.Unlock()
}

func setProgressError(p *archiveProgress, err error) {
	p.mu.Lock()
	p.Error = err.Error()
	p.Updated = time.Now()
	p.mu.Unlock()
}

func openTrustedSource(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("source file is unavailable")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("source file is unavailable")
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("source file changed")
	}
	return f, nil
}

func serveArchiveDownload(svcs *Services, grants *DownloadGrantStore, stall time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(grantCookieName)
		if err != nil {
			http.Error(w, "download unavailable", http.StatusUnauthorized)
			return
		}
		grant, progress, ok := grants.redeem(r.PathValue("id"), cookie.Value)
		if !ok {
			http.Error(w, "download unavailable", http.StatusForbidden)
			return
		}
		defer grants.finish(grant)
		if !grant.Archive {
			entry := grant.Entries[0]
			f, err := openTrustedSource(entry.Path)
			if err != nil {
				setProgressError(progress, err)
				progress.mu.Lock()
				progress.State = "failed"
				progress.mu.Unlock()
				http.Error(w, "source unavailable", http.StatusGone)
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", archiveContentDisposition(grant.Filename))
			w.Header().Set("Cache-Control", "no-store")
			_, err = io.Copy(&stallWriter{w: w, rc: http.NewResponseController(w), stall: stall}, f)
			if err != nil {
				setProgressError(progress, err)
			}
			progress.mu.Lock()
			progress.Processed = entry.Size
			if err == nil {
				progress.State = "completed"
			} else {
				progress.State = "failed"
			}
			progress.mu.Unlock()
			return
		}
		streamArchive(w, grant, progress, stall)
	}
}
