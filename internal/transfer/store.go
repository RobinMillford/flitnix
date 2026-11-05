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

	"github.com/itsyamin/flitnix/internal/secfile"
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
	mu   sync.RWMutex
	dir  string
	link func(oldname, newname string) error
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
func (s *Store) Dir() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dir
}

// SetDir updates the receive directory. Empty resets to DefaultReceiveSubpath.
func (s *Store) SetDir(dir string) error {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return fmt.Errorf("transfer: cannot derive the default receive directory: %w", err)
		}
		dir = filepath.Join(home, filepath.FromSlash(DefaultReceiveSubpath))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("transfer: cannot resolve receive directory %q: %w", dir, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dir = abs
	return nil
}

// SetLinkHook installs a fault-injection seam for the atomic publish step.
// Production never calls it: Commit uses os.Link whenever the hook is nil.
// It exists so tests can make publication fail deterministically on every
// platform, including Windows, where a permission fixture (chmod) cannot deny
// a write.
func (s *Store) SetLinkHook(fn func(oldname, newname string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.link = fn
}

// linkFile publishes oldname under newname through the injected hook when one
// is installed. Hard links are the production primitive: atomic, and they
// never overwrite an existing file.
func (s *Store) linkFile(oldname, newname string) error {
	s.mu.RLock()
	hook := s.link
	s.mu.RUnlock()
	if hook != nil {
		return hook(oldname, newname)
	}
	return os.Link(oldname, newname)
}

// ensureDir creates the receive directory when first needed.
//
// Ownership is decided by the filesystem operation that created the directory,
// never by a remembered path. os.Mkdir is atomic, so exactly one caller - in
// this process or another - can report success for a given directory, and only
// that caller treats it as Flitnix's to secure. A stat-then-create race, or a
// directory deleted and recreated later, therefore cannot make Flitnix replace
// the ACL of a directory it did not create.
func (s *Store) ensureDir() error {
	dir := s.Dir()
	created, err := mkdirLeaf(dir, dirPerm)
	if err != nil {
		return fmt.Errorf("transfer: cannot create receive directory %s: %w", dir, err)
	}
	if err := verifyCreatedDir(dir, created); err != nil {
		return fmt.Errorf("transfer: receive directory %s: %w", dir, err)
	}
	return nil
}

// mkdirLeaf creates path, reporting whether this call is the one that created
// the exact leaf directory.
//
// os.Mkdir is the only ownership decision: it fails with a "exists" error when
// anyone else got there first, so the reported flag is never a guess made
// before the creation attempt.
func mkdirLeaf(path string, perm os.FileMode) (bool, error) {
	err := os.Mkdir(path, perm)
	switch {
	case err == nil:
		return true, nil
	case os.IsExist(err):
		return false, nil
	case os.IsNotExist(err):
		// The parent chain is missing: build it, then let os.Mkdir decide
		// ownership of the leaf exactly as above.
		if perr := os.MkdirAll(filepath.Dir(path), perm); perr != nil {
			return false, perr
		}
		if err := os.Mkdir(path, perm); err != nil {
			if os.IsExist(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	default:
		return false, err
	}
}

// verifyCreatedDir validates a directory Flitnix is about to use, and applies
// the protected policy only when this process created it.
//
// A symlink or reparse point is never treated as a directory Flitnix created:
// replacing the DACL of whatever it points at would be an escalation. The
// target is only validated as a directory and its access is left exactly as
// the user configured it.
func verifyCreatedDir(path string, created bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, terr := os.Stat(path)
		if terr != nil || !target.IsDir() {
			return fmt.Errorf("unsafe destination directory %q", path)
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("unsafe destination directory %q", path)
	}
	if !created {
		// Pre-existing and user-selected: its ACL is not ours to replace.
		return nil
	}
	// dirPerm is enforced only by the kernel on the Unix family. Windows
	// ignores it, so a directory Flitnix just created would otherwise inherit
	// whatever its parent grants and expose received bytes.
	if err := secfile.ProtectDir(path); err != nil {
		return fmt.Errorf("secure: %w", err)
	}
	return nil
}

// protectPartFile secures a temporary file Flitnix just created, once per file
// creation and never per chunk.
//
// It runs unconditionally rather than depending on the receive directory being
// Flitnix's own: a pre-existing shared receive root keeps its ACL, so the part
// file - and, through the hard link Commit creates, the published file - needs
// a protected DACL of its own. Applying it to a file that already inherited the
// same policy is idempotent, and confidentiality then never rests on any
// bookkeeping about the parent path.
func (s *Store) protectPartFile(f *os.File, path string) error {
	if err := secfile.ProtectFile(path); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("transfer: cannot secure temporary file %s: %w", path, err)
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
	rel, err := filepath.Rel(s.Dir(), abs)
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
	if isWindowsReservedComponent(name) {
		return nil, fmt.Errorf("%w: %q uses a reserved Windows device name", ErrInvalidFilename, name)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return nil, fmt.Errorf("%w: %q ends with a space or dot", ErrInvalidFilename, name)
	}
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	dir := s.Dir()
	for attempt := 0; attempt < 64; attempt++ {
		suffix, err := randHex(4)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, name+partSuffixMarker+suffix)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
		if err != nil {
			if os.IsExist(err) {
				continue // virtually impossible; retry with a new suffix
			}
			return nil, fmt.Errorf("transfer: cannot create temporary file in %s: %w", dir, err)
		}
		if err := s.protectPartFile(f, path); err != nil {
			return nil, err
		}
		return &PartFile{store: s, f: f, path: path, name: name, finalDir: dir}, nil
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
	dir := s.Dir()
	for n := 0; n < commitVariants; n++ {
		candidate := variantName(root, n)
		path := filepath.Join(dir, candidate)
		if !s.withinDir(path) {
			return "", fmt.Errorf("transfer: refusing folder root outside receive directory")
		}
		err := os.Mkdir(path, dirPerm)
		if err == nil {
			// This call created the root, so it is Flitnix's to secure. A root
			// created inside a user-selected directory would otherwise inherit
			// that directory's wider access; secure the root, never the
			// directory around it.
			if perr := secfile.ProtectDir(path); perr != nil {
				_ = os.Remove(path)
				return "", fmt.Errorf("transfer: secure folder root: %w", perr)
			}
			return candidate, nil
		}
		if os.IsExist(err) {
			continue
		}
		return "", fmt.Errorf("transfer: creating folder root: %w", err)
	}
	return "", fmt.Errorf("transfer: could not allocate a folder root for %q", root)
}

// DiscardFolderRoot removes a folder root reserved by AllocateFolderRoot
// when the surrounding batch could not be created. It only ever deletes an
// empty directory (os.Remove refuses otherwise), so a root that received
// data cannot be harmed, and a missing root is already-discarded success.
// The store owns receive paths, so the routes never join filesystem names.
func (s *Store) DiscardFolderRoot(raw string) error {
	root, err := sanitizeRootName(raw)
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir(), root)
	if !s.withinDir(path) {
		return fmt.Errorf("transfer: refusing folder root outside receive directory")
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("transfer: discarding folder root: %w", err)
	}
	return nil
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
	base := filepath.Join(s.Dir(), root)
	if !s.withinDir(base) {
		return nil, fmt.Errorf("transfer: refusing folder root outside receive directory")
	}
	if err := s.prepareDir(base, dirPerm); err != nil {
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
			if err := s.prepareDir(dir, dirPerm); err != nil {
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
		if err := s.protectPartFile(f, path); err != nil {
			return nil, err
		}
		return &PartFile{store: s, f: f, path: path, name: name, finalDir: dir}, nil
	}
	return nil, errors.New("transfer: could not allocate a unique temporary folder file")
}

// prepareDir creates or validates one directory beneath the receive root.
//
// Ownership comes from mkdirLeaf's atomic creation, exactly as for the receive
// root: a directory this call created is Flitnix's to secure, and one that was
// already there is only validated. A directory Flitnix created inside a
// user-selected root would otherwise inherit that root's wider DACL.
func (s *Store) prepareDir(path string, perm os.FileMode) error {
	created, err := mkdirLeaf(path, perm)
	if err != nil {
		return fmt.Errorf("transfer: creating directory: %w", err)
	}
	if err := verifyCreatedDir(path, created); err != nil {
		return fmt.Errorf("transfer: %s: %w", path, err)
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
		err := p.store.linkFile(p.path, final)
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
