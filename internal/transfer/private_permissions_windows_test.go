//go:build windows

package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// TestWindowsPublicationSecurityFailureBlocksPublish drives a real resumable
// upload to the publication step and injects a failure at exactly the
// ProtectFile(partPath) call, proving publication refuses to proceed rather
// than renaming a file whose DACL could not be applied.
func TestWindowsPublicationSecurityFailureBlocksPublish(t *testing.T) {
	tmpDir := t.TempDir()
	destDir := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}

	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	payload := []byte("publication under an injected ACL failure")
	sum := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(sum[:])
	const transferID = "up-acl-fail"

	if _, err := rm.CreateUpload("sess-1", transferID, "b-1", "it-1", "doc.pdf", "", int64(len(payload)), destDir); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, state, err := rm.IngestStream(transferID, 0, bytes.NewReader(payload), nil); err != nil || state != ResumableAwaitingDigest {
		t.Fatalf("IngestStream: err=%v state=%s", err, state)
	}

	partPath := rm.partPath(transferID)
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf("staged part file missing before publication: %v", err)
	}

	// Fail only the publication-time policy call, so staging stays valid and
	// the failure is attributable to the step under test.
	secfile.SetProtectHook(func(path string) error {
		if path == partPath {
			return errors.New("injected: cannot protect the published file")
		}
		return nil
	})
	t.Cleanup(func() { secfile.SetProtectHook(nil) })

	if _, err := rm.FinalizeComplete(transferID, expectedHex); err == nil {
		t.Fatal("FinalizeComplete published a file whose DACL could not be applied")
	}

	if _, err := os.Stat(filepath.Join(destDir, "doc.pdf")); !os.IsNotExist(err) {
		t.Fatalf("target file exists despite the failure: %v", err)
	}
	if _, err := os.Stat(rm.publishedPath(transferID)); !os.IsNotExist(err) {
		t.Fatal("a durable publication record was written despite the failure")
	}
	// The staged bytes must survive so the existing retry design can resume.
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf("staged part file was discarded: %v", err)
	}
	upload, ok := rm.GetUpload(transferID)
	if !ok {
		t.Fatal("upload record was dropped after the failure")
	}
	if meta := upload.Meta(); meta.State == ResumableCompleted {
		t.Fatalf("upload reached %s although publication failed", meta.State)
	}

	// With the fault removed the same upload publishes, proving the failure
	// left recoverable state rather than a poisoned one.
	secfile.SetProtectHook(nil)
	pub, err := rm.FinalizeComplete(transferID, expectedHex)
	if err != nil {
		t.Fatalf("retry after the injected failure: %v", err)
	}
	securetest.RequirePrivate(t, pub.TargetPath)
}

// TestWindowsResumableUploadReopensAfterRestart proves a partially received
// upload survives a manager restart on a protected staging directory.
//
// The part file is never passed to ProtectFile while it is streaming: it
// inherits the file policy from the staging directory, which is the case a
// hand-written DACL gets wrong most easily. Every step after the restart goes
// through a fresh handle opened against that inherited DACL - the resume open
// (O_RDWR), the durable checkpoint's sync, the publication-time hash pass
// (O_RDONLY) and the final rename - so a mask missing FILE_READ_EA,
// FILE_WRITE_EA or FILE_WRITE_ATTRIBUTES fails here rather than on a user's
// machine.
func TestWindowsResumableUploadReopensAfterRestart(t *testing.T) {
	root := t.TempDir()
	destDir := filepath.Join(root, "out")
	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		t.Fatalf("create destination directory: %v", err)
	}

	store, err := NewStore(StoreConfig{Dir: root})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	payload := []byte("a restart must reopen the protected part file and continue")
	half := len(payload) / 2
	firstHalf, secondHalf := payload[:half], payload[half:]
	sum := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(sum[:])
	const transferID = "up-reopen"

	first := NewResumableManager(store, nil)
	if _, err := first.CreateUpload("sess-1", transferID, "b-1", "it-1", "reopened.bin", "", int64(len(payload)), destDir); err != nil {
		first.Close()
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, state, err := first.IngestStream(transferID, 0, bytes.NewReader(firstHalf), nil); err != nil {
		first.Close()
		t.Fatalf("first ingest: %v", err)
	} else if state != ResumableActive {
		first.Close()
		t.Fatalf("state after a partial ingest = %s, want %s", state, ResumableActive)
	}
	partPath := first.partPath(transferID)
	if _, err := os.Stat(partPath); err != nil {
		first.Close()
		t.Fatalf("staged part file missing: %v", err)
	}
	securetest.RequirePrivate(t, partPath)
	first.Close()

	// A second manager over the same store is the restart. Its constructor runs
	// RecoverOnStartup, which re-reads the staging directory through the
	// protected DACL.
	restarted := NewResumableManager(store, nil)
	defer restarted.Close()
	if _, ok := restarted.GetUpload(transferID); !ok {
		t.Fatal("restart did not restore the upload from the staging directory")
	}
	securetest.RequireProtectedRoot(t, restarted.StagingDir())

	committed, state, err := restarted.IngestStream(transferID, int64(len(firstHalf)), bytes.NewReader(secondHalf), nil)
	if err != nil {
		t.Fatalf("resume after restart: %v", err)
	}
	if state != ResumableAwaitingDigest {
		t.Fatalf("state after the resumed ingest = %s, want %s", state, ResumableAwaitingDigest)
	}
	if committed != int64(len(payload)) {
		t.Fatalf("committed offset = %d, want %d", committed, len(payload))
	}

	pub, err := restarted.FinalizeComplete(transferID, expectedHex)
	if err != nil {
		t.Fatalf("publish after restart: %v", err)
	}
	data, err := os.ReadFile(pub.TargetPath)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatal("published contents differ from the transferred payload")
	}
	securetest.RequirePrivate(t, pub.TargetPath)
}

// TestWindowsStagingDirectoryIsProtected proves the resumable staging directory
// - which Flitnix owns outright at a fixed name inside the receive root - is
// protected before it receives partially transferred bytes.
func TestWindowsStagingDirectoryIsProtected(t *testing.T) {
	st := newTestStore(t)
	rm := NewResumableManager(st, nil)
	defer rm.Close()

	securetest.RequireProtectedRoot(t, rm.StagingDir())

	partPath := rm.partPath("transfer-1")
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("create staging part file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close staging part file: %v", err)
	}
	securetest.RequirePrivate(t, partPath)
}
