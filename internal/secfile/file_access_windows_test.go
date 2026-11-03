//go:build windows

package secfile_test

// These tests are the only evidence that the DACL this package writes is
// compatible with how Go actually opens files. They inspect no security
// descriptors and assert no masks: they create the objects, close the handles
// that existed before the policy was applied, and then go through the real os
// entry points. A mask that is missing one right the Windows access check
// requires - FILE_READ_EA for every generic read, FILE_WRITE_EA,
// FILE_WRITE_ATTRIBUTES and FILE_APPEND_DATA for every generic write or append
// - fails here with ACCESS_DENIED, which is exactly how it would fail in the
// shipped application.
//
// Cross-compiling proves none of this: the access check runs in the kernel.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/secfile"
	"github.com/itsyamin/flitnix/internal/secfile/securetest"
)

// newProtectedRoot creates a directory Flitnix owns and applies the directory
// policy to it, the way the receive root and the recovery state directory are
// secured.
func newProtectedRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := secfile.ProtectDir(dir); err != nil {
		t.Fatalf("ProtectDir(%s): %v", dir, err)
	}
	return dir
}

// mustWrite creates or truncates path with the given contents and closes it.
func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mustRead returns the whole contents of path through a fresh read handle.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestProtectedDirectoryOpensThroughGo proves a protected directory stays
// usable through os.ReadDir at every level of the tree, not only on the handle
// that existed when the DACL was set.
func TestProtectedDirectoryOpensThroughGo(t *testing.T) {
	root := newProtectedRoot(t)
	securetest.RequireProtectedRoot(t, root)

	if _, err := os.ReadDir(root); err != nil {
		t.Fatalf("os.ReadDir on the protected root: %v", err)
	}

	// An explicit os.Open of a directory is a different code path (it relies
	// on FILE_FLAG_BACKUP_SEMANTICS) and is what filepath.WalkDir uses.
	d, err := os.Open(root)
	if err != nil {
		t.Fatalf("os.Open on the protected root: %v", err)
	}
	// An empty listing ends with io.EOF; anything else here is an access
	// failure.
	if _, err := d.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Readdirnames on the protected root: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close protected root: %v", err)
	}

	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("create child directory: %v", err)
	}
	securetest.RequireInheritedContainer(t, child)
	if _, err := os.ReadDir(child); err != nil {
		t.Fatalf("os.ReadDir on an inherited child directory: %v", err)
	}

	grandchild := filepath.Join(child, "grandchild")
	if err := os.Mkdir(grandchild, 0o700); err != nil {
		t.Fatalf("create grandchild directory: %v", err)
	}
	securetest.RequireInheritedContainer(t, grandchild)
	if _, err := os.ReadDir(grandchild); err != nil {
		t.Fatalf("os.ReadDir on an inherited grandchild directory: %v", err)
	}

	// Removal is part of ordinary operation, and needs DELETE on the entry
	// plus FILE_DELETE_CHILD (or DELETE) on the parent.
	if err := os.Remove(grandchild); err != nil {
		t.Fatalf("remove grandchild directory: %v", err)
	}
}

// TestExplicitlyProtectedFileReopensThroughGo covers the file the transfer
// stack secures directly: a part file created inside a user-selected receive
// directory that keeps its own wider ACL.
//
// The handle opened before ProtectFile is kept open on purpose. Streaming
// writes through that handle after the DACL changed, which is what the real
// upload path does, and only then are the reopen paths exercised.
func TestExplicitlyProtectedFileReopensThroughGo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "received.bin")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("create part file: %v", err)
	}
	if _, err := f.Write([]byte("head")); err != nil {
		t.Fatalf("write before protecting: %v", err)
	}
	if err := secfile.ProtectFile(path); err != nil {
		t.Fatalf("ProtectFile(%s): %v", path, err)
	}
	// The live upload continues streaming on the original handle.
	if _, err := f.Write([]byte("-mid")); err != nil {
		t.Fatalf("write through the handle that predates the DACL change: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync before close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close part file: %v", err)
	}

	if got := mustRead(t, path); got != "head-mid" {
		t.Fatalf("contents after the streaming writes = %q, want %q", got, "head-mid")
	}

	t.Run("os.Open", func(t *testing.T) {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("os.Open: %v", err)
		}
		buf := make([]byte, 4)
		if _, err := f.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if string(buf) != "head" {
			t.Fatalf("read %q, want %q", string(buf), "head")
		}
	})

	t.Run("O_RDONLY", func(t *testing.T) {
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile O_RDONLY: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})

	t.Run("O_WRONLY", func(t *testing.T) {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile O_WRONLY: %v", err)
		}
		if _, err := f.Write([]byte("HEAD")); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := mustRead(t, path); got != "HEAD-mid" {
			t.Fatalf("contents = %q, want %q", got, "HEAD-mid")
		}
	})

	t.Run("O_RDWR", func(t *testing.T) {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("OpenFile O_RDWR: %v", err)
		}
		buf := make([]byte, 4)
		if _, err := f.ReadAt(buf, 0); err != nil {
			t.Fatalf("read at: %v", err)
		}
		if _, err := f.WriteAt([]byte("head"), 0); err != nil {
			t.Fatalf("write at: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})

	// O_APPEND is the resume path: committed bytes are kept and the rest is
	// appended. Go maps it to an explicit set of rights rather than
	// GENERIC_WRITE, so it is checked on its own.
	t.Run("O_WRONLY_O_APPEND", func(t *testing.T) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("OpenFile O_WRONLY|O_APPEND: %v", err)
		}
		if _, err := f.Write([]byte("-tail")); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("sync appended data: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if got := mustRead(t, path); got != "head-mid-tail" {
			t.Fatalf("contents = %q, want %q", got, "head-mid-tail")
		}
	})

	// Publication reopens the staged file to hash it, then renames it into
	// place. Both need the DACL to hold after the fact.
	t.Run("hash and publish after reopen", func(t *testing.T) {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat staged file: %v", err)
		}
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("reopen for hashing: %v", err)
		}
		if _, err := f.Stat(); err != nil {
			t.Fatalf("stat through the reopened handle: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		final := filepath.Join(dir, "published.bin")
		if err := os.Rename(path, final); err != nil {
			t.Fatalf("publish rename: %v", err)
		}
		securetest.RequirePrivate(t, final)
		if got := mustRead(t, final); got != "head-mid-tail" {
			t.Fatalf("published contents = %q, want %q", got, "head-mid-tail")
		}
	})
}

// TestInheritedFileReopensThroughGo covers files that were never passed to
// ProtectFile: they inherit the file policy from the protected root, or from
// the inherit-only ACE a subdirectory carries. Those two shapes reach a file
// by different inheritance paths, so both are exercised.
func TestInheritedFileReopensThroughGo(t *testing.T) {
	root := newProtectedRoot(t)

	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("create child directory: %v", err)
	}
	grandchild := filepath.Join(child, "grandchild")
	if err := os.Mkdir(grandchild, 0o700); err != nil {
		t.Fatalf("create grandchild directory: %v", err)
	}

	for name, dir := range map[string]string{
		"protected root": root,
		"child":          child,
		"grandchild":     grandchild,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "inherited.bin")
			mustWrite(t, path, []byte("inherited"))
			// The write handle is closed before every reopen below, so the
			// access check runs against the file's own inherited DACL.
			securetest.RequirePrivate(t, path)

			if got := mustRead(t, path); got != "inherited" {
				t.Fatalf("contents = %q, want %q", got, "inherited")
			}
			for _, tc := range []struct {
				label string
				flag  int
			}{
				{"O_RDONLY", os.O_RDONLY},
				{"O_WRONLY", os.O_WRONLY},
				{"O_RDWR", os.O_RDWR},
				{"O_WRONLY|O_APPEND", os.O_WRONLY | os.O_APPEND},
			} {
				f, err := os.OpenFile(path, tc.flag, 0)
				if err != nil {
					t.Fatalf("OpenFile %s on an inherited file: %v", tc.label, err)
				}
				if err := f.Sync(); err != nil && tc.flag != os.O_RDONLY {
					t.Fatalf("sync with %s: %v", tc.label, err)
				}
				if err := f.Close(); err != nil {
					t.Fatalf("close with %s: %v", tc.label, err)
				}
			}

			// Attribute changes are a separate right from data writes; the
			// recovery path relies on them when it normalises timestamps.
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatalf("os.Chmod on an inherited file: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatalf("os.Remove on an inherited file: %v", err)
			}
		})
	}
}
