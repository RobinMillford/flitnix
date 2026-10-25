// Package transfer implements Flitnix's file-transfer core: the upload
// approval state machine, streaming receive with inline SHA-256 hashing,
// collision-safe file storage under the receive directory, and the registry
// of files the desktop chose to share with the phone.
//
// The package depends only on the Go standard library and is independent of
// the Wails desktop layer and the HTTP layer, so both the approval machine
// and the storage layer are exercised directly under go test.
//
// Trust boundary: nothing received from the phone (filenames, declared
// sizes, request bodies) is trusted. Filenames are sanitized to a bare base
// name before they touch the filesystem; sizes are declared up front,
// approved by the desktop user, and enforced against the actual stream.
package transfer

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxFilenameBytes is the maximum accepted filename length in bytes
	// after sanitization. Linux filesystems typically cap one name
	// component at 255 bytes; the headroom absorbs the collision
	// suffixes (" (999)") appended later.
	MaxFilenameBytes = 230

	// MaxRelativePathBytes bounds a canonical folder path while leaving
	// room for destination and collision handling on common filesystems.
	MaxRelativePathBytes = 4096

	// MaxPathComponentBytes matches the conservative filename limit used
	// for every component of a folder-relative path.
	MaxPathComponentBytes = MaxFilenameBytes

	// MaxRelativePathDepth prevents manifests from creating unexpectedly
	// deep directory trees.
	MaxRelativePathDepth = 64
)

// ErrInvalidFilename reports a client-supplied filename that cannot be
// stored safely.
var ErrInvalidFilename = errors.New("transfer: invalid filename")

// ErrInvalidRelativePath reports a folder path that is not safe to
// canonicalize as a relative path. Validation is syntactic only; callers
// must still enforce destination confinement and filesystem policies.
var ErrInvalidRelativePath = errors.New("transfer: invalid relative path")

// SanitizeName turns a client-supplied filename into a safe, bare filename
// for the receive directory.
//
// Rules (all deliberately conservative):
//
//   - surrounding whitespace is trimmed, then emptiness enforced;
//   - path separators (both '/' and the Windows '\' defensively), NUL and
//     other control characters, and invalid UTF-8 are rejected;
//   - "." and ".." are rejected;
//   - anything over MaxFilenameBytes is rejected;
//   - Unicode is otherwise preserved as-is.
//
// A rejected name fails the upload request with a 400 from the HTTP layer;
// no sanitized variant is "guessed" on behalf of the sender, because
// silently mangling names is how files end up somewhere the user did not
// approve.
func SanitizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("%w: name is empty or whitespace", ErrInvalidFilename)
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("%w: name is a path component (%q)", ErrInvalidFilename, name)
	}
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: name is not valid UTF-8", ErrInvalidFilename)
	}
	if len(name) > MaxFilenameBytes {
		return "", fmt.Errorf("%w: name is %d bytes, want at most %d", ErrInvalidFilename, len(name), MaxFilenameBytes)
	}
	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			return "", fmt.Errorf("%w: name contains a path separator", ErrInvalidFilename)
		case r == 0 || unicode.IsControl(r):
			return "", fmt.Errorf("%w: name contains a control character", ErrInvalidFilename)
		}
	}
	return name, nil
}

// SanitizeRelativePath canonicalizes a client-supplied folder path using
// forward slashes. It never touches the filesystem.
//
// The cross-platform policy rejects absolute paths, drive/UNC syntax,
// traversal components, empty components, control characters, colons, and
// Windows device names. This intentionally rejects a small set of names
// that are valid on some Unix filesystems so a later Windows receiver does
// not receive an unrepresentable manifest.
func SanitizeRelativePath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: path is empty", ErrInvalidRelativePath)
	}
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("%w: path is not valid UTF-8", ErrInvalidRelativePath)
	}
	path := strings.ReplaceAll(raw, `\`, "/")
	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return "", fmt.Errorf("%w: path must contain non-empty relative components", ErrInvalidRelativePath)
	}
	if len(path) > MaxRelativePathBytes {
		return "", fmt.Errorf("%w: path is %d bytes, want at most %d", ErrInvalidRelativePath, len(path), MaxRelativePathBytes)
	}

	parts := strings.Split(path, "/")
	if len(parts) > MaxRelativePathDepth {
		return "", fmt.Errorf("%w: path has %d components, want at most %d", ErrInvalidRelativePath, len(parts), MaxRelativePathDepth)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: path contains an empty or traversal component", ErrInvalidRelativePath)
		}
		if len(part) > MaxPathComponentBytes {
			return "", fmt.Errorf("%w: component is %d bytes, want at most %d", ErrInvalidRelativePath, len(part), MaxPathComponentBytes)
		}
		if err := validateRelativePathComponent(part); err != nil {
			return "", err
		}
	}
	return strings.Join(parts, "/"), nil
}

func validateRelativePathComponent(component string) error {
	if strings.ContainsRune(component, ':') {
		return fmt.Errorf("%w: component contains a colon", ErrInvalidRelativePath)
	}
	if strings.HasSuffix(component, " ") || strings.HasSuffix(component, ".") {
		return fmt.Errorf("%w: component ends with a space or dot", ErrInvalidRelativePath)
	}
	for _, r := range component {
		if r == 0 || unicode.IsControl(r) {
			return fmt.Errorf("%w: component contains a control character", ErrInvalidRelativePath)
		}
	}
	if isWindowsReservedComponent(component) {
		return fmt.Errorf("%w: component uses a reserved Windows device name", ErrInvalidRelativePath)
	}
	return nil
}

func isWindowsReservedComponent(component string) bool {
	base := strings.ToUpper(strings.TrimRight(component, " ."))
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}

// splitExt splits a filename into stem and extension the way the collision
// variants are built. It deliberately differs from filepath.Ext for
// dotfiles: for ".hidden" the whole name is the stem (variants become
// ".hidden (1)" instead of " (1).hidden").
func splitExt(name string) (stem, ext string) {
	// The loop starts at the last byte and stops before index 0 so a
	// leading dot is never treated as an extension separator. '.' (0x2E)
	// can never appear inside a multi-byte UTF-8 sequence, so byte-wise
	// scanning is safe.
	for i := len(name) - 1; i > 0; i-- {
		if name[i] == '.' {
			return name[:i], name[i:]
		}
	}
	return name, ""
}

// variantName returns the nth collision variant of name: the name itself
// for n == 0, then "stem (n).ext" as specified by the project's collision
// policy.
func variantName(name string, n int) string {
	if n <= 0 {
		return name
	}
	stem, ext := splitExt(name)
	return stem + " (" + itoa(n) + ")" + ext
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
