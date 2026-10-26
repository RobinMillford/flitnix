package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

const (
	grantLifetime   = 60 * time.Second
	maxGrantSources = 512
	grantCookieName = "flitnix-download-grant"

	// progressRetention is how long a finished (completed, failed or
	// expired) progress record stays pollable. The phone stops polling at the
	// first terminal answer, so a couple of minutes is ample and keeps the
	// store bounded.
	progressRetention = 2 * time.Minute

	// maxGrantsPerSession bounds unredeemed grants a session may hold, and
	// maxProgressPerSession bounds the progress records (live plus recently
	// finished) it may hold. Both stop an authenticated peer from growing
	// server memory by requesting grants in a loop.
	maxGrantsPerSession   = 8
	maxProgressPerSession = 32
)

// Download progress states reported to the phone.
const (
	progressPending      = "pending"
	progressTransferring = "transferring"
	progressCompleted    = "completed"
	progressFailed       = "failed"
	progressExpired      = "expired"
)

// Redemption failures. The HTTP layer maps each to a distinct status; none of
// them says anything about why a secret did not match.
var (
	errGrantInvalid = errors.New("download grant is not valid")
	errGrantExpired = errors.New("download grant expired")
	errGrantBusy    = errors.New("another download is already active for this session")
	errGrantSession = errors.New("session is no longer active")
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

// downloadGrant is the immutable, server-built plan one browser navigation
// may redeem once. The plan is fixed when the grant is created from
// server-side source metadata; nothing the browser sends later (a save-as
// name, a query string) can alter which files it covers or what it is called.
type downloadGrant struct {
	ID, Secret, Session, Filename, Progress string
	Sess                                    *session.Session
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

	// clock stamps Updated; it is the owning store's clock so retention is
	// judged against one time source.
	clock func() time.Time
}

func (p *archiveProgress) now() time.Time {
	if p.clock != nil {
		return p.clock()
	}
	return time.Now()
}

// DownloadGrantStore holds deliberately short-lived, in-memory browser grants
// and the progress records that follow them.
type DownloadGrantStore struct {
	mu       sync.Mutex
	now      func() time.Time
	grants   map[string]downloadGrant
	progress map[string]*archiveProgress
	active   map[string]int
}

func newGrantStore() *DownloadGrantStore {
	return &DownloadGrantStore{
		now:      time.Now,
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

// pruneLocked drops expired unredeemed grants (marking their progress record
// expired so a polling phone learns the download will never start) and
// forgets finished progress records after progressRetention. Callers hold
// s.mu. It is invoked on every store operation, so cleanup needs no timer and
// the store is bounded whenever it is used at all.
func (s *DownloadGrantStore) pruneLocked(now time.Time) {
	for id, g := range s.grants {
		if now.After(g.ExpiresAt) {
			delete(s.grants, id)
			if p := s.progress[g.Progress]; p != nil {
				p.mu.Lock()
				if p.State == progressPending {
					p.State, p.Error, p.Updated = progressExpired, "the download was not started in time", now
				}
				p.mu.Unlock()
			}
		}
	}
	for id, p := range s.progress {
		p.mu.Lock()
		finished := p.State == progressCompleted || p.State == progressFailed || p.State == progressExpired
		old := now.Sub(p.Updated) > progressRetention
		p.mu.Unlock()
		if finished && old {
			delete(s.progress, id)
		}
	}
}

func (s *DownloadGrantStore) create(sess *session.Session, sources []archiveSource, filename string) (downloadGrant, *archiveProgress, error) {
	key := sess.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.pruneLocked(now)
	if s.active[key] != 0 {
		return downloadGrant{}, nil, errors.New("archive download already active")
	}
	var grants, records int
	for _, g := range s.grants {
		if g.Session == key {
			grants++
		}
	}
	for _, p := range s.progress {
		if p.Session == key {
			records++
		}
	}
	if grants >= maxGrantsPerSession || records >= maxProgressPerSession {
		return downloadGrant{}, nil, errors.New("too many pending downloads for this session")
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
	grant := downloadGrant{
		ID: id, Secret: secret, Session: key, Sess: sess, Filename: filename,
		Progress: progressID, Entries: entries, Total: total,
		Archive:   len(sources) != 1 || sources[0].Kind != transfer.SourceKindFile,
		ExpiresAt: now.Add(grantLifetime),
	}
	progress := &archiveProgress{ID: progressID, Session: key, Total: total, State: progressPending, Updated: now, clock: s.now}
	s.grants[id], s.progress[progressID] = grant, progress
	return grant, progress, nil
}

// redeem consumes a grant. Success is one-shot: the grant is deleted, so a
// replay (a second navigation, a browser retry, a resumed download) finds
// nothing. Failures that do not prove the caller holds the secret reveal
// nothing; a busy session leaves the grant intact for a retry inside its
// lifetime.
func (s *DownloadGrantStore) redeem(id, secret string) (downloadGrant, *archiveProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	grant, ok := s.grants[id]
	if !ok || subtle.ConstantTimeCompare([]byte(grant.Secret), []byte(secret)) != 1 {
		s.pruneLocked(now)
		return downloadGrant{}, nil, errGrantInvalid
	}
	if now.After(grant.ExpiresAt) {
		s.pruneLocked(now)
		return downloadGrant{}, nil, errGrantExpired
	}
	if grant.Sess == nil || grant.Sess.Status() != session.StatusActive {
		delete(s.grants, id)
		if p := s.progress[grant.Progress]; p != nil {
			setProgressFailed(p, "the session ended")
		}
		return downloadGrant{}, nil, errGrantSession
	}
	if s.active[grant.Session] != 0 {
		return downloadGrant{}, nil, errGrantBusy
	}
	delete(s.grants, id)
	s.active[grant.Session]++
	p := s.progress[grant.Progress]
	p.mu.Lock()
	p.State, p.Updated = progressTransferring, now
	p.mu.Unlock()
	s.pruneLocked(now)
	return grant, p, nil
}

func (s *DownloadGrantStore) finish(grant downloadGrant) {
	s.mu.Lock()
	if s.active[grant.Session] > 0 {
		s.active[grant.Session]--
	}
	if s.active[grant.Session] == 0 {
		delete(s.active, grant.Session)
	}
	s.mu.Unlock()
}

func (s *DownloadGrantStore) getProgress(id, session string) (archiveProgress, bool) {
	s.mu.Lock()
	s.pruneLocked(s.now())
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

// size reports how many grants, progress records and active streams the
// store currently holds. Tests use it to prove cleanup is bounded.
func (s *DownloadGrantStore) size() (grants, progress, active int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.active {
		active += n
	}
	return len(s.grants), len(s.progress), active
}
