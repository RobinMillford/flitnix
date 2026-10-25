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
)

// ErrInvalidFilename reports a client-supplied filename that cannot be
// stored safely.
var ErrInvalidFilename = errors.New("transfer: invalid filename")

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
