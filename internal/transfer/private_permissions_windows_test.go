//go:build windows

package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itsyamin/flitnix/internal/secfile"
	"github.com/itsyamin/flitnix/internal/secfile/securetest"
)

// assertPrivatePath is the Windows arm of the shared permission assertion. The
// Unix arm (private_permissions_unix_test.go) checks the 0700 mode the kernel
// enforces; Windows ignores those bits, so the DACL is the only evidence a
// path is private and it is inspected directly.
//
// Only the principal set, the rights and the no-execute rule are asserted here.
// Whether a directory is additionally a protected root depends on who created
// it, and the dedicated tests below cover both cases explicitly.
func assertPrivatePath(t *testing.T, path string, _ os.FileInfo) {
	t.Helper()
	securetest.RequirePrivate(t, path)
}

// assertPrivateFile checks a file's DACL: files are granted the file policy
// directly and are never protected roots.
func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	securetest.RequirePrivate(t, path)
}

// TestWindowsReceiveRootCreatedByFlitnixIsProtected proves a receive directory
// Flitnix created is restricted to the approved principals and that the
// restriction propagates to children.
func TestWindowsReceiveRootCreatedByFlitnixIsProtected(t *testing.T) {
	st := newTestStore(t)
	part := mustOpenPart(t, st, "photo.jpg")
	defer part.Abort()

	securetest.RequireProtectedRoot(t, st.Dir())

	// A child directory must inherit the approved ACEs without its own ACL
	// call; otherwise every later subdirectory would reopen the hole. The
	// inherited shape is asserted exactly: directory rights effective, file
	// rights present but inherit-only.
	child := filepath.Join(st.Dir(), "child")
	if err := os.Mkdir(child, dirPerm); err != nil {
		t.Fatalf("create child directory: %v", err)
	}
	securetest.RequireInheritedContainer(t, child)

	securetest.RequirePrivate(t, part.PartPath())
}

// TestWindowsFilePolicyCarriesNoExecuteRight asserts the split between the
// directory and file masks at the bit level: directories keep FILE_TRAVERSE,
// files must never carry the same bit as FILE_EXECUTE.
func TestWindowsFilePolicyCarriesNoExecuteRight(t *testing.T) {
	st := newTestStore(t)
	part := mustOpenPart(t, st, "photo.jpg")
	defer part.Abort()

	user, err := secfile.CurrentUserSID()
	if err != nil {
		t.Fatalf("current user SID: %v", err)
	}

	if mask := securetest.EffectiveMask(t, st.Dir(), user); mask&securetest.FileExecute == 0 {
		t.Errorf("receive root lost FILE_TRAVERSE (%#x)", uint32(mask))
	}

	child := filepath.Join(st.Dir(), "child")
	if err := os.Mkdir(child, dirPerm); err != nil {
		t.Fatalf("create child directory: %v", err)
	}
	if mask := securetest.EffectiveMask(t, child, user); mask&securetest.FileExecute == 0 {
		t.Errorf("inherited child directory lost FILE_TRAVERSE (%#x)", uint32(mask))
	}

	inherited := filepath.Join(child, "file.bin")
	f, err := os.OpenFile(inherited, os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		t.Fatalf("create inherited file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close inherited file: %v", err)
	}
	if mask := securetest.EffectiveMask(t, inherited, user); mask&securetest.FileExecute != 0 {
		t.Errorf("inherited child file carries FILE_EXECUTE (%#x)", uint32(mask))
	}
	if mask := securetest.EffectiveMask(t, part.PartPath(), user); mask&securetest.FileExecute != 0 {
		t.Errorf("explicitly protected part file carries FILE_EXECUTE (%#x)", uint32(mask))
	}
}

// TestWindowsPreExistingReceiveRootKeepsItsACL proves Flitnix does not rewrite
// the DACL of a directory the user already had. A Downloads folder, a documents
// folder or a shared drive may be shared on purpose, and replacing its ACL
// would silently revoke access other users and other applications rely on.
func TestWindowsPreExistingReceiveRootKeepsItsACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Downloads")
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatalf("pre-create receive directory: %v", err)
	}
	securetest.GrantEveryone(t, dir)

	before := securetest.Violations(t, dir)
	if len(before) == 0 {
		t.Fatal("fixture granted Everyone access but no violation was detected; the check would be vacuous")
	}

	st, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	part := mustOpenPart(t, st, "photo.jpg")
	defer part.Abort()

	after := securetest.Violations(t, dir)
	if len(after) != len(before) {
		t.Fatalf("the user-selected receive directory's ACL was rewritten:\nbefore:\n  %s\nafter:\n  %s",
			strings.Join(before, "\n  "), strings.Join(after, "\n  "))
	}

	// The bytes Flitnix writes there must still be private, obtained from the
	// file's own DACL rather than from the parent's.
	securetest.RequirePrivate(t, part.PartPath())
}

// TestWindowsPartFileSecurityFailureBlocksUpload proves a failure to secure the
// temporary file fails the upload instead of silently writing received bytes
// into a file that keeps the parent's wider access.
func TestWindowsPartFileSecurityFailureBlocksUpload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Downloads")
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatalf("pre-create receive directory: %v", err)
	}
	securetest.GrantEveryone(t, dir)

	st, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	secfile.SetProtectHook(func(string) error { return errors.New("injected: cannot set a DACL") })
	t.Cleanup(func() { secfile.SetProtectHook(nil) })

	if part, err := st.OpenPart("photo.jpg"); err == nil {
		_ = part.Abort()
		t.Fatal("OpenPart succeeded although the temporary file could not be secured")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read receive directory: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), partSuffixMarker) {
			t.Fatalf("an insecure temporary file %q was left behind", e.Name())
		}
	}
}

// TestWindowsPublishedFileIsProtectedInsideUserSelectedRoot proves publication
// into a directory whose ACL Flitnix leaves alone still produces a private
// file, because the file carries a protected DACL of its own.
func TestWindowsPublishedFileIsProtectedInsideUserSelectedRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Documents")
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		t.Fatalf("pre-create destination directory: %v", err)
	}
	securetest.GrantEveryone(t, dir)

	st, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	part := mustOpenPart(t, st, "photo.jpg")
	writeAll(t, part, []byte("jpeg-data"))
	final, err := part.Commit()
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	securetest.RequirePrivate(t, final)
	if violations := securetest.Violations(t, dir); len(violations) == 0 {
		t.Fatal("publication rewrote the user-selected directory's ACL")
	}
}
