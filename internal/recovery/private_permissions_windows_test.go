//go:build windows

package recovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itsyamin/flitnix/internal/secfile/securetest"
)

// stateDirForTest returns a state directory that does not exist yet, so the
// store's own creation path is the one under test.
func stateDirForTest(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

// assertPrivatePath is the Windows arm of the shared permission assertion. The
// Unix arm (private_permissions_unix_test.go) checks the 0600/0700 modes the
// kernel enforces; here the mode bits are meaningless and the DACL is the only
// evidence of confidentiality, so it is inspected directly.
//
// Only the principal set and rights are asserted here. Whether a directory is
// additionally a protected root is asserted by the dedicated tests below,
// because that depends on who created it, not on this shared assertion.
func assertPrivatePath(t *testing.T, path string, _ os.FileInfo) {
	t.Helper()
	securetest.RequirePrivate(t, path)
}

// TestWindowsStateDirectoryIsProtected proves the directory that holds the
// installation key and the journal is restricted to the approved principals
// and that the restriction reaches objects created afterwards.
func TestWindowsStateDirectoryIsProtected(t *testing.T) {
	dir := stateDirForTest(t)
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	defer store.Close()

	securetest.RequireProtectedRoot(t, dir)

	// A child directory must inherit the approved ACEs without its own ACL
	// call; otherwise every later subdirectory would reopen the hole. The
	// inherited shape is asserted exactly: directory rights effective, file
	// rights present but inherit-only.
	child := filepath.Join(dir, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("create child directory: %v", err)
	}
	securetest.RequireInheritedContainer(t, child)

	// The installation key is the credential that authenticates the journal,
	// so it is the single most sensitive file this store writes.
	securetest.RequirePrivate(t, filepath.Join(dir, keyFileName))
}

// TestWindowsJournalAndLockInheritApprovedACL proves the journal, the lock file
// and the key all inherit the protected policy once frames have been appended.
func TestWindowsJournalAndLockInheritApprovedACL(t *testing.T) {
	dir := stateDirForTest(t)
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append: %v", err)
	}

	for _, name := range []string{journalName, lockName, keyFileName} {
		securetest.RequirePrivate(t, filepath.Join(dir, name))
	}
}

// TestWindowsJournalReopensAndAppendsAfterClose proves the journal is usable
// across the store's own close and reopen, which is the lifetime every restart
// depends on.
//
// Open journals with O_CREATE|O_RDWR|O_APPEND. Go turns that into
// GENERIC_READ plus FILE_APPEND_DATA|FILE_WRITE_ATTRIBUTES|FILE_WRITE_EA|
// READ_CONTROL|SYNCHRONIZE, and the access check requires every one of them.
// Only the second Open proves it: the first journal handle is created before
// any DACL is in effect on the file, so a mask that is missing an append right
// still succeeds once and fails on the next start.
func TestWindowsJournalReopensAndAppendsAfterClose(t *testing.T) {
	dir := stateDirForTest(t)

	first, err := Open(dir)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	if err := first.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append to the fresh journal: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen the protected state directory: %v", err)
	}
	defer reopened.Close()

	// The append is the write path: it needs the journal's inherited DACL to
	// grant every right the O_APPEND access set names.
	if err := reopened.Append(EventSessionCreated, sessionRecord("k2")); err != nil {
		t.Fatalf("append after reopen: %v", err)
	}

	keys := make(map[string]bool)
	for _, session := range reopened.State().Sessions {
		keys[session.SessionKey] = true
	}
	for _, want := range []string{"k1", "k2"} {
		if !keys[want] {
			t.Fatalf("session %q missing after reopen: the journal was not replayed into the appended file", want)
		}
	}

	for _, name := range []string{journalName, lockName, keyFileName} {
		securetest.RequirePrivate(t, filepath.Join(dir, name))
	}
}

// TestWindowsExistingStateDirectoryIsRehardened proves an application directory
// that already existed with broad access is corrected on open rather than
// trusted, and that the correction is observable.
func TestWindowsExistingStateDirectoryIsRehardened(t *testing.T) {
	dir := stateDirForTest(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("pre-create state directory: %v", err)
	}
	securetest.GrantEveryone(t, dir)

	if violations := securetest.Violations(t, dir); len(violations) == 0 {
		t.Fatal("fixture granted Everyone access but no violation was detected; the check would be vacuous")
	}

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open over a broadly accessible directory: %v", err)
	}
	defer store.Close()

	if violations := securetest.Violations(t, dir); len(violations) > 0 {
		t.Fatalf("open left the state directory broadly accessible:\n  %s",
			strings.Join(violations, "\n  "))
	}
	securetest.RequireProtectedRoot(t, dir)

	// The store must still be fully usable after being re-hardened.
	if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append after re-hardening: %v", err)
	}
}
