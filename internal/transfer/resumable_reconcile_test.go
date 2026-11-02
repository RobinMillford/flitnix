package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newReconcileFixture(t *testing.T) (*ResumableManager, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rm := NewResumableManager(store, NewQuotaManager(dir, DefaultSafetyMargin, DefaultMaxConcurrentPerSession))
	t.Cleanup(rm.Close)
	return rm, dir
}

func TestReconcileQuarantinesUploadWithoutDurableOwner(t *testing.T) {
	rm, dir := newReconcileFixture(t)
	if _, err := rm.CreateUpload("session-key", "tid-orphan", "b_orphan", "it_orphan", "f.bin", "f.bin", 4, dir); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	// Put real bytes in the staged payload so quarantine preservation is
	// observable rather than an empty placeholder.
	entries, err := os.ReadDir(rm.StagingDir())
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	wrotePayload := false
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".part") {
			continue
		}
		if err := os.WriteFile(filepath.Join(rm.StagingDir(), entry.Name()), []byte("abcd"), 0o600); err != nil {
			t.Fatalf("seed staged payload: %v", err)
		}
		wrotePayload = true
	}
	if !wrotePayload {
		t.Fatal("no staged .part payload was created for the orphan upload")
	}

	quarantined, err := rm.Reconcile(func(ResumableMeta) error {
		return errors.New("no durable owner record")
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if quarantined != 1 {
		t.Fatalf("quarantined = %d, want 1", quarantined)
	}
	if _, ok := rm.GetUpload("tid-orphan"); ok {
		t.Fatal("a quarantined upload is still registered and addressable")
	}
	if _, ok := rm.GetUploadByItem("b_orphan", "it_orphan"); ok {
		t.Fatal("a quarantined upload is still resolvable by batch/item")
	}

	entries, err = os.ReadDir(rm.StagingDir())
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	live := 0
	quarantinedParts := 0
	for _, entry := range entries {
		switch {
		case strings.Contains(entry.Name(), ".quarantine-") && strings.Contains(entry.Name(), ".part"):
			quarantinedParts++
			// The original bytes are preserved, not deleted.
			info, statErr := entry.Info()
			if statErr != nil {
				t.Fatalf("stat quarantined file: %v", statErr)
			}
			if info.Size() == 0 {
				t.Fatalf("quarantined file %q is empty; evidence was destroyed", entry.Name())
			}
		case entry.Name() == ".flitnix-part-tid-orphan.meta", entry.Name() == ".flitnix-part-tid-orphan.part":
			live++
		}
	}
	if live != 0 {
		t.Fatalf("%d live staging file(s) remain for a quarantined upload", live)
	}
	if quarantinedParts == 0 {
		t.Fatal("no quarantined payload evidence was preserved")
	}
}

func TestReconcileKeepsAuthorizedUploadAddressable(t *testing.T) {
	rm, dir := newReconcileFixture(t)
	if _, err := rm.CreateUpload("session-key", "tid-good", "b_good", "it_good", "g.bin", "g.bin", 4, dir); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	quarantined, err := rm.Reconcile(func(meta ResumableMeta) error {
		if meta.SessionKey != "session-key" || meta.BatchID != "b_good" || meta.ItemID != "it_good" {
			return errors.New("identity mismatch")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if quarantined != 0 {
		t.Fatalf("quarantined = %d, want 0", quarantined)
	}
	if _, ok := rm.GetUpload("tid-good"); !ok {
		t.Fatal("an authorized upload was removed")
	}
}

func TestReconcileRequiresAuthorizationFunction(t *testing.T) {
	rm, _ := newReconcileFixture(t)
	if _, err := rm.Reconcile(nil); err == nil {
		t.Fatal("Reconcile accepted a nil authorization function")
	}
}

// TestRecoverOnStartupLatchesFailure proves a staging recovery failure is not
// silently discarded: the first failure is retained and returned again so the
// startup gate can refuse readiness.
func TestRecoverOnStartupLatchesFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Replace the staging directory with a regular file so os.ReadDir fails
	// with a real error rather than ENOENT.
	if err := os.WriteFile(filepath.Join(dir, StagingDirName), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write staging placeholder: %v", err)
	}
	rm := NewResumableManager(store, nil)
	t.Cleanup(rm.Close)

	if err := rm.RecoverOnStartup(); err == nil {
		t.Fatal("staging recovery reported success against an unreadable staging path")
	}
	if err := rm.RecoverOnStartup(); err == nil {
		t.Fatal("the latched staging recovery failure was forgotten on a second call")
	}
}
