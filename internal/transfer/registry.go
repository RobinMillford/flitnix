package transfer

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// registryMaxFiles bounds how many files may be shared at once. Sharing is
// an explicit desktop action, so this is a backstop rather than a live
// attack surface.
const registryMaxFiles = 512

var (
	// ErrNotFound means no shared file with that ID exists.
	ErrNotFound = errors.New("transfer: shared file not found")

	// ErrMissingFile means the file to share does not exist on disk.
	ErrMissingFile = errors.New("transfer: file to share does not exist")

	// ErrNotRegularFile means the file to share is not a regular file
	// (directories, sockets, devices are never shared).
	ErrNotRegularFile = errors.New("transfer: not a regular file")
)

// SharedFile is one file the desktop chose to expose to the phone. The ID
// is a fresh opaque value namespaced inside the registry: the phone never
// learns the path (it is only surfaced in HTTP headers like
// Content-Disposition's sanitized filename) and the name never doubles as a
// filesystem lookup key.
type SharedFile struct {
	ID      string    // opaque ID for /api/files/{id}/download
	Name    string    // sanitized display name
	Size    int64     // regular-file size in bytes
	Path    string    // absolute local path; never sent to the phone
	Session string    // owning session's non-secret key ("" while desktop-owned)
	Created time.Time // when the file was registered
}

// Registry keeps the files currently shared with phones. All methods are
// safe for concurrent use. Revoking a file only unlists it; the original
// file stays on the user's disk untouched.
type Registry struct {
	mu    sync.Mutex
	next  int64
	files map[string]SharedFile
	order []string // insertion order for stable listing
}

// NewRegistry prepares an empty registry.
func NewRegistry() *Registry {
	return &Registry{files: make(map[string]SharedFile)}
}

// Add registers an existing regular file as shared. The name is sanitized
// with the same rules as received filenames (so a shared file's
// Content-Disposition can never inject something), and the on-disk size is
// taken from an lstat to rule out directory tricks, because clients are not
// trusted to report sizes correctly.
func (r *Registry) Add(path, rawName, sessionKey string) (SharedFile, error) {
	if path == "" {
		return SharedFile{}, ErrMissingFile
	}
	name, err := SanitizeName(rawName)
	if err != nil {
		return SharedFile{}, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return SharedFile{}, fmt.Errorf("%w: %v", ErrMissingFile, err)
	}
	if !st.Mode().IsRegular() {
		return SharedFile{}, fmt.Errorf("%w: %q", ErrNotRegularFile, path)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.files) >= registryMaxFiles {
		return SharedFile{}, fmt.Errorf("%w: too many files are shared", ErrNoCapacity)
	}
	id, err := newID()
	if err != nil {
		return SharedFile{}, err
	}
	sf := SharedFile{
		ID:      id,
		Name:    name,
		Size:    st.Size(),
		Path:    path,
		Session: sessionKey,
		Created: time.Now(),
	}
	r.files[id] = sf
	r.order = append(r.order, id)
	return sf, nil
}

// Remove unlists a file by ID. It reports whether the ID existed. The
// underlying file is untouched.
func (r *Registry) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.files[id]; !ok {
		return false
	}
	delete(r.files, id)
	for i, cur := range r.order {
		if cur == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	return true
}

// Get returns the shared file for an ID.
func (r *Registry) Get(id string) (SharedFile, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sf, ok := r.files[id]
	return sf, ok
}

// List returns every shared file in insertion order.
func (r *Registry) List() []SharedFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SharedFile, 0, len(r.files))
	for _, id := range r.order {
		if sf, ok := r.files[id]; ok {
			out = append(out, sf)
		}
	}
	// order can only shrink here; sort defensively anyway.
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// ListSession returns the shared files registered for one session key.
func (r *Registry) ListSession(sessionKey string) []SharedFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SharedFile
	for _, id := range r.order {
		if sf, ok := r.files[id]; ok && sf.Session == sessionKey {
			out = append(out, sf)
		}
	}
	return out
}

// Clear drops every registration. Called on session end: any previously
// handed-out download URL dies with the session (downloads also always
// require a valid live session, so this clearing is a belt-and-braces
// cleanup rather than the security boundary).
func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files = make(map[string]SharedFile)
	r.order = nil
}

// Len returns the number of currently shared files.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.files)
}
