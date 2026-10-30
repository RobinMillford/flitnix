package transfer

import (
	"sync"
	"sync/atomic"
	"time"
)

// DownloadState describes the desktop's view of a phone-initiated download.
type DownloadState string

const (
	DownloadStarting     DownloadState = "starting"
	DownloadTransferring DownloadState = "transferring"
	DownloadFinalizing   DownloadState = "finalizing"
	DownloadSent         DownloadState = "sent"
	DownloadFailed       DownloadState = "failed"
	DownloadCanceled     DownloadState = "canceled"
)

// Download is a safe projection of bytes successfully written to a client.
// It contains no local path or session token.
type Download struct {
	ID       string
	FileID   string
	Name     string
	Kind     string
	Size     int64
	Session  string
	Created  time.Time
	Started  time.Time
	Finished time.Time
	State    DownloadState
	Written  int64
	Speed    float64
	SHA256   string
	Reason   string
}

type downloadRecord struct {
	Download
	mu          sync.Mutex
	lastAt      time.Time
	lastWritten int64
	written     atomic.Int64
}

// DownloadRegistry retains active and recent outgoing download telemetry.
type DownloadRegistry struct {
	mu      sync.Mutex
	records map[string]*downloadRecord
	order   []string
}

const downloadHistoryLimit = 64

func NewDownloadRegistry() *DownloadRegistry {
	return &DownloadRegistry{records: make(map[string]*downloadRecord)}
}

func (r *DownloadRegistry) Start(fileID, name, session string, size int64) (Download, error) {
	return r.StartKind(fileID, name, session, size, "file")
}

// StartKind records an outbound file or archive without coupling the registry
// to the browser implementation that requested it.
func (r *DownloadRegistry) StartKind(fileID, name, session string, size int64, kind string) (Download, error) {
	id, err := newID()
	if err != nil {
		return Download{}, err
	}
	now := time.Now()
	d := &downloadRecord{Download: Download{
		ID: id, FileID: fileID, Name: name, Kind: kind, Size: size, Session: session,
		Created: now, State: DownloadStarting,
	}, lastAt: now}
	r.mu.Lock()
	r.records[id] = d
	r.order = append(r.order, id)
	r.pruneLocked()
	r.mu.Unlock()
	return d.snapshot(), nil
}

func (r *DownloadRegistry) pruneLocked() {
	for len(r.order) > downloadHistoryLimit {
		id := r.order[0]
		d := r.records[id]
		if d == nil {
			r.order = r.order[1:]
			continue
		}
		d.mu.Lock()
		terminal := d.State == DownloadSent || d.State == DownloadFailed || d.State == DownloadCanceled
		d.mu.Unlock()
		if !terminal {
			break
		}
		delete(r.records, id)
		r.order = r.order[1:]
	}
}

func (r *DownloadRegistry) Progress(id string, written int64) {
	r.mu.Lock()
	d := r.records[id]
	r.mu.Unlock()
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.State != DownloadStarting && d.State != DownloadTransferring && d.State != DownloadFinalizing {
		return
	}
	if written < 0 {
		written = 0
	}
	if written < d.Written {
		written = d.Written
	}
	if written < 0 {
		written = 0
	}
	if written > d.Size {
		written = d.Size
	}
	now := time.Now()
	if elapsed := now.Sub(d.lastAt).Seconds(); elapsed > 0 {
		d.Speed = float64(written-d.lastWritten) / elapsed
	}
	d.Written = written
	d.written.Store(written)
	d.lastWritten = written
	d.lastAt = now
}

func (r *DownloadRegistry) Finish(id string, written int64) {
	r.finish(id, DownloadSent, "", written, "")
}

func (r *DownloadRegistry) FinishWithDigest(id string, written int64, sha256 string) {
	r.finish(id, DownloadSent, "", written, sha256)
}

func (r *DownloadRegistry) Begin(id string) {
	r.setState(id, DownloadTransferring, "")
}

func (r *DownloadRegistry) Finalize(id string) {
	r.setState(id, DownloadFinalizing, "")
}

func (r *DownloadRegistry) Fail(id string, reason string, written int64) {
	r.finish(id, DownloadFailed, reason, written, "")
}

func (r *DownloadRegistry) CancelSession(session string) {
	r.mu.Lock()
	records := make([]*downloadRecord, 0)
	for _, d := range r.records {
		if d.Session == session {
			records = append(records, d)
		}
	}
	r.mu.Unlock()
	for _, d := range records {
		d.mu.Lock()
		if d.State != DownloadSent && d.State != DownloadFailed && d.State != DownloadCanceled {
			d.State = DownloadCanceled
			d.Finished = time.Now()
			d.Reason = "session ended"
		}
		d.mu.Unlock()
	}
}

func (r *DownloadRegistry) List() []Download {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Download, 0, len(r.order))
	for _, id := range r.order {
		if d := r.records[id]; d != nil {
			out = append(out, d.snapshot())
		}
	}
	return out
}

func (r *DownloadRegistry) finish(id string, state DownloadState, reason string, written int64, sha256 string) {
	r.mu.Lock()
	d := r.records[id]
	r.mu.Unlock()
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.State != DownloadStarting && d.State != DownloadTransferring && d.State != DownloadFinalizing {
		return
	}

	if written > d.Size {
		written = d.Size
	}
	d.Written = written
	d.written.Store(written)
	now := time.Now()
	if elapsed := now.Sub(d.lastAt).Seconds(); elapsed > 0 {
		d.Speed = float64(written-d.lastWritten) / elapsed
	}
	d.State = state
	d.Reason = reason
	if sha256 != "" {
		d.SHA256 = sha256
	}
	d.Finished = now
}

func (r *DownloadRegistry) setState(id string, state DownloadState, reason string) {
	r.mu.Lock()
	d := r.records[id]
	r.mu.Unlock()
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.State == DownloadSent || d.State == DownloadFailed || d.State == DownloadCanceled {
		return
	}
	if state == DownloadTransferring && d.Started.IsZero() {
		d.Started = time.Now()
	}
	d.State, d.Reason = state, reason
}

func (d *downloadRecord) snapshot() Download {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.Download
	out.Written = d.written.Load()
	return out
}
