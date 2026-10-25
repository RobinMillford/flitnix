package transfer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SourceKind identifies a desktop source selected for a future phone
// download. Folder sources are intentionally not exposed by the HTTP file
// listing until the archive-transfer phase is implemented.
type SourceKind string

const (
	SourceKindFile   SourceKind = "file"
	SourceKindFolder SourceKind = "folder"
)

const (
	MaxSourceItems     = 100000
	MaxSourceEmptyDirs = 100000
	MaxSourcePathBytes = 4096
)

// SourceItem is trusted desktop metadata. Path is never serialized to a
// phone-facing or Wails public view.
type SourceItem struct {
	RelativePath string
	Size         int64
	Path         string
}

// SharedSource is the internal representation used by the desktop source
// registry. A file source has one item; a folder source keeps its root and
// relative item paths so a later archive transfer need not rediscover them.
type SharedSource struct {
	ID        string
	Kind      SourceKind
	Name      string
	RootName  string
	Path      string
	Items     []SourceItem
	EmptyDirs []string
	Session   string
	Created   time.Time
}

// SourceView is safe for Wails/UI use. It deliberately contains no absolute
// paths, session keys, or item-level filesystem paths.
type SourceView struct {
	ID         string
	Kind       SourceKind
	Name       string
	RootName   string
	ItemCount  int
	TotalBytes int64
	Created    time.Time
}

func sourceView(source SharedSource) SourceView {
	var total int64
	for _, item := range source.Items {
		total += item.Size
	}
	return SourceView{
		ID: source.ID, Kind: source.Kind, Name: source.Name,
		RootName: source.RootName, ItemCount: len(source.Items),
		TotalBytes: total, Created: source.Created,
	}
}

func enumerateFolder(root string) (rootName string, items []SourceItem, emptyDirs []string, err error) {
	if root == "" {
		return "", nil, nil, fmt.Errorf("%w: folder path is empty", ErrMissingFile)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", nil, nil, fmt.Errorf("%w: resolve folder: %v", ErrMissingFile, err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, nil, fmt.Errorf("%w: %v", ErrMissingFile, err)
	}
	if !info.IsDir() {
		return "", nil, nil, fmt.Errorf("%w: %q", ErrNotRegularFile, root)
	}
	rootName, err = SanitizeName(filepath.Base(filepath.Clean(absolute)))
	if err != nil {
		return "", nil, nil, fmt.Errorf("transfer: invalid folder name: %w", err)
	}

	err = filepath.WalkDir(absolute, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("transfer: enumerate %q: %w", path, walkErr)
		}
		if path == absolute {
			return nil
		}
		relative, relErr := filepath.Rel(absolute, path)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("transfer: source path escaped selected folder")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			entries, readErr := os.ReadDir(path)
			if readErr != nil {
				return fmt.Errorf("transfer: inspect directory %q: %w", path, readErr)
			}
			if len(entries) == 0 {
				if len(emptyDirs) >= MaxSourceEmptyDirs {
					return fmt.Errorf("transfer: folder contains more than %d empty directories", MaxSourceEmptyDirs)
				}
				emptyDirs = append(emptyDirs, filepath.ToSlash(relative))
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		canonical, pathErr := SanitizeRelativePath(filepath.ToSlash(relative))
		if pathErr != nil {
			return fmt.Errorf("transfer: unsafe source path %q: %w", relative, pathErr)
		}
		if len(canonical) > MaxSourcePathBytes {
			return fmt.Errorf("transfer: source path exceeds %d bytes", MaxSourcePathBytes)
		}
		if len(items) >= MaxSourceItems {
			return fmt.Errorf("transfer: folder contains more than %d files", MaxSourceItems)
		}
		st, statErr := entry.Info()
		if statErr != nil {
			return fmt.Errorf("transfer: inspect %q: %w", path, statErr)
		}
		if st.Size() < 0 {
			return fmt.Errorf("transfer: negative size for %q", path)
		}
		items = append(items, SourceItem{RelativePath: canonical, Size: st.Size(), Path: path})
		return nil
	})
	if err != nil {
		return "", nil, nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].RelativePath < items[j].RelativePath })
	sort.Strings(emptyDirs)
	return rootName, items, emptyDirs, nil
}
