package client

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/itsyamin/flitnix/internal/transfer"
)

var (
	// ErrEmptyInput indicates that no file or folder paths were provided.
	ErrEmptyInput = errors.New("client: no files or directories specified")

	// ErrPathNotFound indicates that a specified path does not exist.
	ErrPathNotFound = errors.New("client: path not found")

	// ErrEmptyDirectory indicates that a folder contains no readable regular files.
	ErrEmptyDirectory = errors.New("client: directory contains no readable files")

	// ErrSpecialFile indicates a FIFO, device, socket, or unsupported file mode.
	ErrSpecialFile = errors.New("client: unsupported special file")

	// ErrBrokenSymlink indicates a symlink whose target does not exist.
	ErrBrokenSymlink = errors.New("client: broken symlink")

	// ErrSymlinkEscape indicates a directory symlink that points outside the root tree.
	ErrSymlinkEscape = errors.New("client: directory symlink targets outside root directory")

	// ErrMixedDirectory indicates mixing folder and file arguments in a single send command.
	ErrMixedDirectory = errors.New("client: cannot mix directories with other files; send directories individually")

	// ErrDuplicateName indicates multiple files with identical display names in a flat batch.
	ErrDuplicateName = errors.New("client: duplicate file name in transfer list")
)

// LocalItem maps an item in the batch manifest to its absolute path on the local filesystem.
type LocalItem struct {
	Path         string
	ManifestItem transfer.ManifestItem
}

// LocalManifest bundles a validated batch manifest with local filesystem mappings.
type LocalManifest struct {
	Manifest   transfer.BatchManifest
	LocalItems []LocalItem
	TotalBytes int64
}

// BuildManifest validates local paths and constructs a canonical transfer.BatchManifest.
// If paths contains a single directory, a folder batch is created.
// If paths contains one or more files, a flat files batch is created.
func BuildManifest(paths []string, nameOverride string) (*LocalManifest, error) {
	if len(paths) == 0 {
		return nil, ErrEmptyInput
	}

	// Case 1: Exactly one path provided, which may be a directory
	if len(paths) == 1 {
		absPath, err := filepath.Abs(paths[0])
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPathNotFound, err)
		}
		info, err := os.Stat(absPath)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot access %s: %v", ErrPathNotFound, paths[0], err)
		}

		if info.IsDir() {
			return buildFolderManifest(absPath, nameOverride)
		}
	}

	// Case 2: One or more flat files
	return buildFilesManifest(paths)
}

func buildFolderManifest(dirRoot, nameOverride string) (*LocalManifest, error) {
	rootName := strings.TrimSpace(nameOverride)
	if rootName == "" {
		rootName = filepath.Base(dirRoot)
	}

	sanitizedRoot, err := transfer.SanitizeName(rootName)
	if err != nil {
		return nil, fmt.Errorf("client: invalid folder root name %q: %w", rootName, err)
	}

	var manifestItems []transfer.ManifestItem
	var localItems []LocalItem
	var totalBytes int64

	err = filepath.WalkDir(dirRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == dirRoot {
			return nil
		}

		// Inspect symlinks
		if d.Type()&fs.ModeSymlink != 0 {
			target, evalErr := filepath.EvalSymlinks(p)
			if evalErr != nil {
				return fmt.Errorf("%w: %s (%v)", ErrBrokenSymlink, p, evalErr)
			}
			targetInfo, statErr := os.Stat(target)
			if statErr != nil {
				return fmt.Errorf("%w: cannot stat %s: %v", ErrBrokenSymlink, target, statErr)
			}

			// Disallow directory symlinks pointing outside the tree
			if targetInfo.IsDir() {
				rel, relErr := filepath.Rel(dirRoot, target)
				if relErr != nil || strings.HasPrefix(rel, "..") {
					return fmt.Errorf("%w: %s -> %s", ErrSymlinkEscape, p, target)
				}
				// Skip directory symlinks to prevent traversal loops
				return fs.SkipDir
			}
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("client: cannot read metadata for %s: %w", p, err)
		}

		if info.IsDir() {
			return nil
		}

		// Reject special files (FIFOs, sockets, device nodes)
		mode := info.Mode()
		if mode&(fs.ModeDevice|fs.ModeNamedPipe|fs.ModeSocket) != 0 {
			return fmt.Errorf("%w: %s", ErrSpecialFile, p)
		}

		// Verify file is readable
		f, openErr := os.Open(p)
		if openErr != nil {
			return fmt.Errorf("client: file not readable: %s: %w", p, openErr)
		}
		_ = f.Close()

		rel, relErr := filepath.Rel(dirRoot, p)
		if relErr != nil {
			return relErr
		}
		slashRel := filepath.ToSlash(rel)

		item := transfer.ManifestItem{
			RelativePath: slashRel,
			DisplayName:  filepath.Base(slashRel),
			Size:         info.Size(),
		}

		manifestItems = append(manifestItems, item)
		localItems = append(localItems, LocalItem{Path: p, ManifestItem: item})
		totalBytes += info.Size()
		return nil
	})

	if err != nil {
		return nil, err
	}

	if len(manifestItems) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrEmptyDirectory, dirRoot)
	}

	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFolder,
		RootName:  sanitizedRoot,
		Items:     manifestItems,
	}

	if _, err := transfer.ValidateManifest(manifest); err != nil {
		return nil, fmt.Errorf("client: manifest validation failed: %w", err)
	}

	return &LocalManifest{
		Manifest:   manifest,
		LocalItems: localItems,
		TotalBytes: totalBytes,
	}, nil
}

func buildFilesManifest(paths []string) (*LocalManifest, error) {
	var manifestItems []transfer.ManifestItem
	var localItems []LocalItem
	var totalBytes int64
	seenNames := make(map[string]string)

	for _, p := range paths {
		absPath, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("%w: %s (%v)", ErrPathNotFound, p, err)
		}

		info, err := os.Stat(absPath)
		if err != nil {
			return nil, fmt.Errorf("%w: %s (%v)", ErrPathNotFound, p, err)
		}

		if info.IsDir() {
			return nil, fmt.Errorf("%w: found directory %q in file list", ErrMixedDirectory, p)
		}

		mode := info.Mode()
		if mode&(fs.ModeDevice|fs.ModeNamedPipe|fs.ModeSocket) != 0 {
			return nil, fmt.Errorf("%w: %s", ErrSpecialFile, p)
		}

		// Verify file is readable
		f, openErr := os.Open(absPath)
		if openErr != nil {
			return nil, fmt.Errorf("client: file not readable: %s: %w", p, openErr)
		}
		_ = f.Close()

		baseName := filepath.Base(absPath)
		sanitized, err := transfer.SanitizeName(baseName)
		if err != nil {
			return nil, fmt.Errorf("client: invalid filename %q: %w", baseName, err)
		}

		if prevPath, dup := seenNames[sanitized]; dup {
			return nil, fmt.Errorf("%w: %q conflicts between %s and %s", ErrDuplicateName, sanitized, prevPath, p)
		}
		seenNames[sanitized] = p

		item := transfer.ManifestItem{
			DisplayName: sanitized,
			Size:        info.Size(),
		}

		manifestItems = append(manifestItems, item)
		localItems = append(localItems, LocalItem{Path: absPath, ManifestItem: item})
		totalBytes += info.Size()
	}

	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     manifestItems,
	}

	if _, err := transfer.ValidateManifest(manifest); err != nil {
		return nil, fmt.Errorf("client: manifest validation failed: %w", err)
	}

	return &LocalManifest{
		Manifest:   manifest,
		LocalItems: localItems,
		TotalBytes: totalBytes,
	}, nil
}
