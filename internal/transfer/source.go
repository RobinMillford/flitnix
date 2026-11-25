package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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

// ErrInvalidBrowsePrefix reports an invalid folder exploration prefix,
// traversal attempt, or malformed pagination parameter.
var ErrInvalidBrowsePrefix = errors.New("transfer: invalid folder prefix")

const (
	DefaultSourceBrowseLimit = 200
	MaxSourceBrowseLimit     = 500
	MaxSelectableSourceItems = 10000
)

// SourceItem is trusted desktop metadata. Path is never serialized to a
// phone-facing or Wails public view.
type SourceItem struct {
	ID           string
	RelativePath string
	Size         int64
	ModTimeMs    int64
	Path         string
}

// SourceBrowseEntry describes one directory or file node inside a shared folder
// at a single hierarchy level without exposing any host filesystem path.
type SourceBrowseEntry struct {
	Kind       string `json:"kind"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Size       int64  `json:"size,omitempty"`
	ModTimeMs  int64  `json:"modTimeMs,omitempty"`
	ItemCount  int    `json:"itemCount,omitempty"`
	TotalBytes int64  `json:"totalBytes,omitempty"`
}

// SourceBrowsePage is one bounded page of directory children inside a shared source.
type SourceBrowsePage struct {
	SourceID     string              `json:"sourceId"`
	SourceName   string              `json:"sourceName"`
	Kind         SourceKind          `json:"kind"`
	Prefix       string              `json:"prefix"`
	Items        []SourceBrowseEntry `json:"items"`
	TotalEntries int                 `json:"totalEntries"`
	Cursor       int                 `json:"cursor"`
	Limit        int                 `json:"limit"`
	HasMore      bool                `json:"hasMore"`
	NextCursor   *int                `json:"nextCursor,omitempty"`
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
		itemID, idErr := newID()
		if idErr != nil {
			return idErr
		}
		items = append(items, SourceItem{
			ID:           itemID,
			RelativePath: canonical,
			Size:         st.Size(),
			ModTimeMs:    st.ModTime().UnixMilli(),
			Path:         path,
		})
		return nil
	})
	if err != nil {
		return "", nil, nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].RelativePath < items[j].RelativePath })
	sort.Strings(emptyDirs)
	return rootName, items, emptyDirs, nil
}

func canonicalBrowsePrefix(raw string) (string, error) {
	if raw == "" || raw == "/" {
		return "", nil
	}
	trimmed := strings.Trim(raw, "/")
	if trimmed == "" {
		return "", nil
	}
	if strings.ContainsAny(trimmed, "\\\x00") {
		return "", fmt.Errorf("%w: forbidden path character", ErrInvalidBrowsePrefix)
	}
	parts := strings.Split(trimmed, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%w: invalid path segment", ErrInvalidBrowsePrefix)
		}
	}
	canonical, err := SanitizeRelativePath(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidBrowsePrefix, err)
	}
	return canonical, nil
}

func withinSourceRoot(root, candidate string) bool {
	if root == "" || candidate == "" {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absCandidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if evalRoot, rErr := filepath.EvalSymlinks(absRoot); rErr == nil {
		if evalCand, cErr := filepath.EvalSymlinks(absCandidate); cErr == nil {
			evalRel, eErr := filepath.Rel(evalRoot, evalCand)
			if eErr != nil || evalRel == ".." || strings.HasPrefix(evalRel, ".."+string(filepath.Separator)) {
				return false
			}
		}
	}
	return true
}

func buildBrowseEntries(source SharedSource, prefix string, cursor, limit int) (SourceBrowsePage, error) {
	canonicalPrefix, err := canonicalBrowsePrefix(prefix)
	if err != nil {
		return SourceBrowsePage{}, err
	}
	if cursor < 0 || limit < 0 {
		return SourceBrowsePage{}, fmt.Errorf("%w: negative pagination parameter", ErrInvalidBrowsePrefix)
	}
	if limit == 0 {
		limit = DefaultSourceBrowseLimit
	}
	if limit > MaxSourceBrowseLimit {
		limit = MaxSourceBrowseLimit
	}
	if canonicalPrefix != "" && source.Path != "" {
		candidateDir := filepath.Join(source.Path, filepath.FromSlash(canonicalPrefix))
		if !withinSourceRoot(source.Path, candidateDir) {
			return SourceBrowsePage{}, fmt.Errorf("%w: path escapes shared folder root", ErrInvalidBrowsePrefix)
		}
		if st, lErr := os.Lstat(candidateDir); lErr == nil {
			if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
				return SourceBrowsePage{}, fmt.Errorf("%w: non-directory or symlink target", ErrInvalidBrowsePrefix)
			}
		}
	}

	dirs := make(map[string]*SourceBrowseEntry)
	files := make([]SourceBrowseEntry, 0)
	prefixSlash := ""
	if canonicalPrefix != "" {
		prefixSlash = canonicalPrefix + "/"
	}
	prefixMatched := canonicalPrefix == ""

	for _, item := range source.Items {
		rel := item.RelativePath
		var rest string
		if canonicalPrefix == "" {
			rest = rel
		} else if strings.HasPrefix(rel, prefixSlash) {
			prefixMatched = true
			rest = strings.TrimPrefix(rel, prefixSlash)
		} else {
			continue
		}
		if rest == "" {
			continue
		}
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			dirName := rest[:idx]
			entry, exists := dirs[dirName]
			if !exists {
				dirPath := dirName
				if canonicalPrefix != "" {
					dirPath = canonicalPrefix + "/" + dirName
				}
				entry = &SourceBrowseEntry{
					Kind: "dir",
					Name: dirName,
					Path: dirPath,
				}
				dirs[dirName] = entry
			}
			entry.ItemCount++
			entry.TotalBytes += item.Size
		} else {
			files = append(files, SourceBrowseEntry{
				Kind:      "file",
				ID:        item.ID,
				Name:      path.Base(rel),
				Path:      rel,
				Size:      item.Size,
				ModTimeMs: item.ModTimeMs,
			})
		}
	}

	for _, ed := range source.EmptyDirs {
		if canonicalPrefix == "" {
			rest := ed
			if idx := strings.IndexByte(rest, '/'); idx >= 0 {
				rest = rest[:idx]
			}
			if rest != "" {
				if _, exists := dirs[rest]; !exists {
					dirs[rest] = &SourceBrowseEntry{Kind: "dir", Name: rest, Path: rest}
				}
			}
			continue
		}
		if ed == canonicalPrefix {
			prefixMatched = true
			continue
		}
		if strings.HasPrefix(ed, prefixSlash) {
			prefixMatched = true
			rest := strings.TrimPrefix(ed, prefixSlash)
			if idx := strings.IndexByte(rest, '/'); idx >= 0 {
				rest = rest[:idx]
			}
			if rest != "" {
				if _, exists := dirs[rest]; !exists {
					dirs[rest] = &SourceBrowseEntry{
						Kind: "dir",
						Name: rest,
						Path: canonicalPrefix + "/" + rest,
					}
				}
			}
		}
	}

	if !prefixMatched {
		return SourceBrowsePage{}, ErrNotFound
	}

	dirEntries := make([]SourceBrowseEntry, 0, len(dirs))
	for _, d := range dirs {
		dirEntries = append(dirEntries, *d)
	}
	sort.Slice(dirEntries, func(i, j int) bool { return dirEntries[i].Name < dirEntries[j].Name })
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	combined := make([]SourceBrowseEntry, 0, len(dirEntries)+len(files))
	combined = append(combined, dirEntries...)
	combined = append(combined, files...)

	total := len(combined)
	if cursor > total {
		cursor = total
	}
	end := cursor + limit
	if end > total {
		end = total
	}
	pageItems := make([]SourceBrowseEntry, end-cursor)
	copy(pageItems, combined[cursor:end])

	var nextCursor *int
	hasMore := end < total
	if hasMore {
		next := end
		nextCursor = &next
	}
	return SourceBrowsePage{
		SourceID:     source.ID,
		SourceName:   source.Name,
		Kind:         source.Kind,
		Prefix:       canonicalPrefix,
		Items:        pageItems,
		TotalEntries: total,
		Cursor:       cursor,
		Limit:        limit,
		HasMore:      hasMore,
		NextCursor:   nextCursor,
	}, nil
}
