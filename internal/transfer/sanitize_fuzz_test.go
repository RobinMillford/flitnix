package transfer

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzSanitizeRelativePath asserts the invariants every downstream consumer
// depends on: whatever the phone supplies, a successful canonicalization can
// never describe a path that escapes the batch root, names a Windows device,
// embeds a separator, or exceeds the documented bounds. Rejected input is a
// valid outcome; an accepted input that breaks an invariant is a bug.
func FuzzSanitizeRelativePath(f *testing.F) {
	seeds := []string{
		"a/b.txt",
		"Photos/2026/image.jpg",
		`a\b.txt`,
		"../etc/passwd",
		"../../../../etc/shadow",
		"/etc/passwd",
		"C:/Windows/win.ini",
		`C:\Windows\win.ini`,
		`\\server\share\file.txt`,
		"a/./b.txt",
		"a//b.txt",
		"a/",
		"/a",
		".",
		"..",
		"a\x00b",
		"a\nb",
		"a/b.",
		"a/b ",
		"CON",
		"con.txt",
		"LPT1/x",
		"a/b:c.txt",
		"données/été.txt",
		"日本語/ファイル.txt",
		strings.Repeat("a/", MaxRelativePathDepth) + "f.txt",
		strings.Repeat("a", MaxPathComponentBytes+1),
		strings.Repeat("a", MaxRelativePathBytes+1),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		path, err := SanitizeRelativePath(raw)
		if err != nil {
			return
		}
		if path == "" {
			t.Fatalf("accepted %q as an empty canonical path", raw)
		}
		if strings.HasPrefix(path, "/") {
			t.Fatalf("accepted absolute path %q", path)
		}
		if strings.Contains(path, `\`) {
			t.Fatalf("accepted path with a backslash: %q", path)
		}
		if !utf8.ValidString(path) {
			t.Fatalf("accepted path with invalid UTF-8: %q", path)
		}
		if len(path) > MaxRelativePathBytes {
			t.Fatalf("accepted %d-byte path (limit %d)", len(path), MaxRelativePathBytes)
		}
		parts := strings.Split(path, "/")
		if len(parts) > MaxRelativePathDepth {
			t.Fatalf("accepted %d components (limit %d)", len(parts), MaxRelativePathDepth)
		}
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				t.Fatalf("accepted traversal or empty component in %q", path)
			}
			if len(part) > MaxPathComponentBytes {
				t.Fatalf("accepted %d-byte component (limit %d)", len(part), MaxPathComponentBytes)
			}
			if strings.ContainsRune(part, ':') {
				t.Fatalf("accepted component with a colon: %q", part)
			}
			if strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") {
				t.Fatalf("accepted component with a trailing space or dot: %q", part)
			}
			if isWindowsReservedComponent(part) {
				t.Fatalf("accepted Windows device name %q", part)
			}
			for _, r := range part {
				if r == 0 || unicode.IsControl(r) {
					t.Fatalf("accepted control character in %q", part)
				}
			}
		}
		// Canonicalization is idempotent: re-sanitizing the canonical form
		// must succeed and return the same value, or a caller that stores
		// then re-validates a path would disagree with itself.
		again, err := SanitizeRelativePath(path)
		if err != nil {
			t.Fatalf("canonical path %q was rejected on re-validation: %v", path, err)
		}
		if again != path {
			t.Fatalf("canonicalization is not idempotent: %q -> %q", path, again)
		}
	})
}

// FuzzSanitizeName asserts the bare-base-name contract for client-supplied
// file names: no separators, no control characters, no traversal names, and
// a length within MaxFilenameBytes.
func FuzzSanitizeName(f *testing.F) {
	seeds := []string{
		"photo.jpg",
		"a b.txt",
		"données.txt",
		"日本語.txt",
		"a/b",
		`a\b`,
		"..",
		".",
		"",
		"   ",
		"a\x00b",
		"a\tb",
		"a\rb",
		".hidden",
		"CON",
		strings.Repeat("a", MaxFilenameBytes),
		strings.Repeat("a", MaxFilenameBytes+1),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		name, err := SanitizeName(raw)
		if err != nil {
			return
		}
		if name == "" || name == "." || name == ".." {
			t.Fatalf("accepted unsafe bare name %q", name)
		}
		if strings.ContainsAny(name, `/\`) {
			t.Fatalf("accepted name with a separator: %q", name)
		}
		if !utf8.ValidString(name) {
			t.Fatalf("accepted name with invalid UTF-8: %q", name)
		}
		if len(name) > MaxFilenameBytes {
			t.Fatalf("accepted %d-byte name (limit %d)", len(name), MaxFilenameBytes)
		}
		if strings.TrimSpace(name) != name {
			t.Fatalf("accepted name with untrimmed space: %q", name)
		}
		for _, r := range name {
			if r == 0 || unicode.IsControl(r) {
				t.Fatalf("accepted control character in %q", name)
			}
		}
	})
}
