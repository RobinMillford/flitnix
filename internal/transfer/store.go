package transfer

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const (
	// dirPerm is the receive-directory permission. Umask can only remove
	// bits, never add them, so directories created with 0700 stay private
	// to the user.
	dirPerm = 0o700

	// filePerm applies to received files and part files.
	filePerm = 0o600

	// partSuffixMarker separates the sanitized filename from the random
	// suffix of a part file, e.g. "photo.jpg.part.3f9c8a1b". Part files
	// are recognizable during forensics after a crash and are removed on
	// every regular failure or cancellation path.
	partSuffixMarker = ".part."

	// commitVariants bounds the collision enumeration for one commit.
	commitVariants = 10000
)

// DefaultReceiveSubpath is the receive directory relative to the user's
// home directory, per the project's storage policy.
const DefaultReceiveSubpath = "Downloads/Flitnix"

// Store holds filesystem operations for the receive directory. Its methods
// are safe for concurrent use; PartFile instances are used by exactly one
// streaming goroutine.
type Store struct {
	dir string
}

// StoreConfig configures a Store.
type StoreConfig struct {
	// Dir is the receive directory. Empty selects
	// DefaultReceiveSubpath under the user's home directory.
	Dir string
}

// NewStore resolves the receive directory. The directory itself is created
// lazily by ensureDir when the first upload is committed, per the "create
// only when first needed" requirement.
func NewStore(cfg StoreConfig) (*Store, error) {
	dir := cfg.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return nil, fmt.Errorf("transfer: cannot derive the default receive directory: %w", err)
		}
		dir = filepath.Join(home, filepath.FromSlash(DefaultReceiveSubpath))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("transfer: cannot resolve receive directory %q: %w", dir, err)
	}
	return &Store{dir: abs}, nil
}

// Dir returns the absolute receive directory.
func (s *Store) Dir() string { return s.dir }

// ensureDir creates the receive directory when first needed. os.MkdirAll
// is idempotent and tolerant of concurrent creators, so no extra locking is
// required.
func (s *Store) ensureDir() error {
	if err := os.MkdirAll(s.dir, dirPerm); err != nil {
		return fmt.Errorf("transfer: cannot create receive directory %s: %w", s.dir, err)
	}
	return nil
}

// randHex returns n bytes as lowercase hex characters.
func randHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("transfer: generating random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// newID returns a random 128-bit hex ID for uploads and shared files. IDs
// are opaque to every client.
func newID() (string, error) { return NewOpaqueID() }

// withinDir verifies that path is inside the store's directory. Defense in
// depth next to SanitizeName: even if a name slipped a separator through a
// future change, a joined path is refused here before any syscall.
func (s *Store) withinDir(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(s.dir, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// PartFile is an in-progress receive file inside the receive directory. It
// implements the contentSink interface (stream.go): Write while streaming,
// Commit on success, Abort on any failure or cancellation. A PartFile is
// used by exactly one goroutine at a time.
type PartFile struct {
	store    *Store
	f        *os.File
	path     string // absolute path of the part file
	name     string // sanitized final filename candidate
	finalDir string

	mu      sync.Mutex
	decided bool // commit or abort already decided the fate of the part file
}

// OpenPart creates a unique part file for the sanitized name in the receive
// directory, creating the directory when first needed. The name must be a
// bare base name as produced by SanitizeName; as defense in depth, anything
// containing a separator or escaping the base-name contract is refused.
func (s *Store) OpenPart(name string) (*PartFile, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("%w: %q is not a bare base name", ErrInvalidFilename, name)
	}
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 64; attempt++ {
		suffix, err := randHex(4)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(s.dir, name+partSuffixMarker+suffix)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
		if err != nil {
			if os.IsExist(err) {
				continue // virtually impossible; retry with a new suffix
			}
			return nil, fmt.Errorf("transfer: cannot create temporary file in %s: %w", s.dir, err)
		}
		return &PartFile{store: s, f: f, path: path, name: name, finalDir: s.dir}, nil
	}

	return nil, errors.New("transfer: could not allocate a unique temporary file after 64 attempts")
}

// AllocateFolderRoot reserves one collision-safe directory for an incoming
// folder batch. Mkdir is the atomic reservation, so concurrent batches never
// share a requested root.
func (s *Store) AllocateFolderRoot(raw string) (string, error) {
	root, err := sanitizeRootName(raw)
	if err != nil {
		return "", err
	}
	if err := s.ensureDir(); err != nil {
		return "", err
	}
	for n := 0; n < commitVariants; n++ {
		candidate := variantName(root, n)
		path := filepath.Join(s.dir, candidate)
		if !s.withinDir(path) {
			return "", fmt.Errorf("transfer: refusing folder root outside receive directory")
		}
		err := os.Mkdir(path, dirPerm)
		if err == nil {
			return candidate, nil
		}
		if os.IsExist(err) {
			continue
		}
		return "", fmt.Errorf("transfer: creating folder root: %w", err)
	}
	return "", fmt.Errorf("transfer: could not allocate a folder root for %q", root)
}

// OpenPartPath creates a part file beneath a server-resolved folder root and
// validated relative path. Existing path components must be real directories,
// never symlinks, before any file is opened.
func (s *Store) OpenPartPath(root, relativePath string) (*PartFile, error) {
	root, err := sanitizeRootName(root)
	if err != nil {
		return nil, err
	}
	relativePath, err = SanitizeRelativePath(relativePath)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(filepath.FromSlash(relativePath))
	dirPart := filepath.Dir(filepath.FromSlash(relativePath))
	base := filepath.Join(s.dir, root)
	if !s.withinDir(base) {
		return nil, fmt.Errorf("transfer: refusing folder root outside receive directory")
	}
	if err := ensureSafeDirectory(base, dirPerm); err != nil {
		return nil, err
	}
	dir := base
	if dirPart != "." {
		parts := strings.Split(filepath.ToSlash(dirPart), "/")
		for _, part := range parts {
			dir = filepath.Join(dir, part)
			if !s.withinDir(dir) {
				return nil, fmt.Errorf("transfer: refusing path outside receive directory")
			}
			if err := ensureSafeDirectory(dir, dirPerm); err != nil {
				return nil, err
			}
		}
	}
	for attempt := 0; attempt < 64; attempt++ {
		suffix, err := randHex(4)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, name+partSuffixMarker+suffix)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return nil, fmt.Errorf("transfer: cannot create temporary folder file: %w", err)
		}
		return &PartFile{store: s, f: f, path: path, name: name, finalDir: dir}, nil
	}
	return nil, errors.New("transfer: could not allocate a unique temporary folder file")
}

func ensureSafeDirectory(path string, perm os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, perm); err != nil && !os.IsExist(err) {
			return fmt.Errorf("transfer: creating directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("transfer: inspecting directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("transfer: unsafe destination directory %q", path)
	}
	return nil
}

// Name returns the sanitized final filename candidate.
func (p *PartFile) Name() string { return p.name }

// PartPath returns the absolute path of the part file, for diagnostics and
// desktop UI only.
func (p *PartFile) PartPath() string { return p.path }

// Write appends bytes to the part file.
func (p *PartFile) Write(b []byte) (int, error) {
	return p.f.Write(b)
}

// Commit fsyncs the part file, closes it, then publishes it under a
// collision-safe final name using link+unlink, which is atomic and never
// overwrites an existing file (plain rename on POSIX silently replaces).
//
// If hard links are unavailable on the receiving filesystem (rare;
// network/overlay filesystems), the commit deliberately FAILS and keeps the
// part file in place instead of falling back to a (racy, overwrite-capable)
// rename: preserving "never overwrite, never half-publish" beats silently
// producing a risky result. The error tells the caller where the part file
// is.
func (p *PartFile) Commit() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.decided {
		return "", errors.New("transfer: part file already committed or aborted")
	}
	p.decided = true

	if err := p.f.Sync(); err != nil {
		p.f.Close()
		_ = os.Remove(p.path)
		return "", fmt.Errorf("transfer: fsync of received data failed: %w", err)
	}
	if err := p.f.Close(); err != nil {
		_ = os.Remove(p.path)
		return "", fmt.Errorf("transfer: closing received file failed: %w", err)
	}

	for n := 0; n < commitVariants; n++ {
		final := filepath.Join(p.finalDir, variantName(p.name, n))
		if !p.store.withinDir(final) {
			// Unreachable while OpenPart keeps enforcing the base-name
			// contract; kept as defense in depth against future changes.
			return "", fmt.Errorf("transfer: refusing to write outside the receive directory: %s", final)
		}
		err := os.Link(p.path, final)
		if err == nil {
			_ = os.Remove(p.path) // drop the temporary link; the data survives
			return final, nil
		}
		switch {
		case os.IsExist(err):
			continue // genuine collision: try the next variant
		case isLinkUnsupported(err):
			return "", fmt.Errorf(
				"transfer: atomic commit failed: the filesystem rejected hard links; the completed data is kept at %s",
				p.path,
			)
		default:
			return "", fmt.Errorf("transfer: committing file: %w", err)
		}
	}
	return "", fmt.Errorf("transfer: could not find a free filename for %q after %d variants", p.name, commitVariants)
}

// Abort closes and deletes the part file. It is idempotent and safe to call
// even when Commit already ran.
func (p *PartFile) Abort() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.decided {
		// Commit closed the file and consumed the link; nothing to clean.
		return nil
	}
	p.decided = true
	_ = p.f.Close()
	return os.Remove(p.path)
}

// isLinkUnsupported reports whether a link(2) failure means the filesystem
// refused the atomic hard-link operation rather than reporting a real
// resource error.
func isLinkUnsupported(err error) bool {
	var le *os.LinkError
	if !errors.As(err, &le) {
		return false
	}
	switch {
	case errors.Is(le.Err, syscall.ENOSYS), // no link(2) implementation
		errors.Is(le.Err, syscall.EOPNOTSUPP), // fs does not support the operation
		errors.Is(le.Err, syscall.ENOTSUP),
		errors.Is(le.Err, syscall.EINVAL), // some network/overlay filesystems
		errors.Is(le.Err, syscall.EPERM):  // some FUSE variants
		return true
	}
	return false
}
