package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// fileSnapshot captures immutable identity and metadata attributes of a file
// at the moment serving begins so mid-stream or cross-resume edits can be detected.
type fileSnapshot struct {
	Path    string
	Size    int64
	ModTime time.Time
	Info    os.FileInfo
	ETag    string
}

func strongFileETag(f *os.File, info os.FileInfo) string {
	if info == nil {
		return ""
	}
	return fmt.Sprintf("\"flit-%x-%x-%s\"", info.Size(), info.ModTime().UTC().UnixNano(), fileIdentityToken(f, info))
}

func captureFileSnapshot(f *os.File, path string, info os.FileInfo) fileSnapshot {
	return fileSnapshot{
		Path:    path,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Info:    info,
		ETag:    strongFileETag(f, info),
	}
}

func fileMatchesSnapshot(f *os.File, snap fileSnapshot) bool {
	if f == nil || snap.Info == nil {
		return false
	}
	cur, err := f.Stat()
	if err != nil || !cur.Mode().IsRegular() {
		return false
	}
	if !os.SameFile(snap.Info, cur) || cur.Size() != snap.Size || !cur.ModTime().Equal(snap.ModTime) {
		return false
	}
	if snap.Path != "" {
		linfo, lerr := os.Lstat(snap.Path)
		if lerr != nil || !linfo.Mode().IsRegular() || !os.SameFile(cur, linfo) {
			return false
		}
	}
	return true
}

// guardedRangeSeeker wraps an *os.File for http.ServeContent while enforcing
// session validity, context cancellation, progress accounting, and mid-stream
// file mutation detection.
type guardedRangeSeeker struct {
	f           *os.File
	snap        fileSnapshot
	ctx         context.Context
	sess        *session.Session
	activeCheck func() bool
	p           *archiveProgress
	downloads  *transfer.DownloadRegistry
	downloadID string

	pending    int64
	sinceCheck int64
	read       int64
	curOffset  int64
	rangeStart int64
	started    bool
	checked    bool
	lastFlush  time.Time
	lastCheck  time.Time
	err        error
}

func (s *guardedRangeSeeker) checkMutableAndSession() error {
	if s.ctx != nil && s.ctx.Err() != nil {
		return errDownloadCanceled
	}
	if s.sess != nil && s.sess.Status() == session.StatusRevoked {
		return errDownloadSession
	}
	if s.activeCheck != nil && !s.activeCheck() {
		return errDownloadSession
	}
	if !s.checked || s.sinceCheck >= progressFlushBytes || time.Since(s.lastCheck) >= progressFlushEvery {
		s.checked = true
		s.sinceCheck = 0
		s.lastCheck = time.Now()
		if s.snap.Info != nil && !fileMatchesSnapshot(s.f, s.snap) {
			return errSourceChanged
		}
	}
	return nil
}

func (s *guardedRangeSeeker) Read(p []byte) (int, error) {
	if err := s.checkMutableAndSession(); err != nil {
		s.err = err
		return 0, err
	}
	if !s.started {
		s.started = true
		s.rangeStart = s.curOffset
	}
	n, err := s.f.Read(p)
	if n > 0 {
		s.read += int64(n)
		s.curOffset += int64(n)
		s.pending += int64(n)
		s.sinceCheck += int64(n)
		if s.pending >= progressFlushBytes || time.Since(s.lastFlush) >= progressFlushEvery {
			s.flush()
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = errSourceUnavailable
		return n, errSourceUnavailable
	}
	if (errors.Is(err, io.EOF) || s.curOffset >= s.snap.Size) && s.snap.Info != nil && !fileMatchesSnapshot(s.f, s.snap) {
		s.err = errSourceChanged
		return n, errSourceChanged
	}
	return n, err
}

func (s *guardedRangeSeeker) Seek(offset int64, whence int) (int64, error) {
	pos, err := s.f.Seek(offset, whence)
	if err == nil {
		s.curOffset = pos
	}
	return pos, err
}

func (s *guardedRangeSeeker) flush() {
	if s.pending > 0 {
		if s.p != nil {
			updateProgress(s.p, s.pending)
		} else if s.downloads != nil && s.downloadID != "" {
			s.downloads.Progress(s.downloadID, s.read)
		}
		s.pending = 0
	}
	s.lastFlush = time.Now()
}

// shouldUseRangeServeContent decides whether http.ServeContent should handle a
// Range request directly (returning 206 or 416) rather than streaming a full 200
// representation with integrity trailers. Any weak or mismatching If-Range forces
// a full 200 fallback per RFC 9110 §13.1.5.
func shouldUseRangeServeContent(r *http.Request, etag string, modTime time.Time) bool {
	if r == nil || strings.TrimSpace(r.Header.Get("Range")) == "" {
		return false
	}
	ir := strings.TrimSpace(r.Header.Get("If-Range"))
	if ir == "" {
		return true
	}
	if strings.HasPrefix(ir, "W/") || strings.HasPrefix(ir, "w/") {
		return false
	}
	if strings.HasPrefix(ir, "\"") {
		return ir == etag
	}
	parsed, err := http.ParseTime(ir)
	if err != nil || modTime.IsZero() {
		return false
	}
	return modTime.Truncate(time.Second).Equal(parsed)
}

func progressProcessedAtLeast(p *archiveProgress, target int64) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Processed >= target
}
