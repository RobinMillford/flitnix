package transfer

import (
	"testing"
	"time"
)

// TestResumableManager_GetUploadByItemReadsMetaUnderLock is the regression test
// for a real data race found by `go test -race ./...` while WP4's recovery
// harness polled committed state during an in-flight PATCH.
//
// GetUploadByItem walked the upload map and compared u.meta.BatchID / u.meta.ItemID
// directly, while executeDurableCheckpoint replaces the entire ResumableMeta
// struct through UpdateMeta under metaMu. The lookup now takes a Meta() snapshot
// (RLock) instead. Without that fix this test reports a DATA RACE under -race.
func TestResumableManager_GetUploadByItemReadsMetaUnderLock(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore(StoreConfig{Dir: tmpDir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	qm := NewQuotaManager(tmpDir, 0, 8)
	rm := NewResumableManager(store, qm)
	t.Cleanup(rm.Close)

	const (
		batchID    = "b-race-1"
		itemID     = "it-race-1"
		transferID = "up-race-1"
	)
	if _, err := rm.CreateUpload("sess-race", transferID, batchID, itemID, "race.bin", "", 4096, tmpDir); err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}

	upload, ok := rm.GetUpload(transferID)
	if !ok {
		t.Fatal("upload was not registered")
	}

	const iterations = 300
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		// Mirrors executeDurableCheckpoint: whole-struct replacement under metaMu.
		for i := 0; i < iterations; i++ {
			offset := int64(i * 16)
			upload.UpdateMeta(func(m *ResumableMeta) {
				m.CommittedOffset = offset
				m.PrefixDigest = "0123456789abcdef"
				m.UpdatedAt = time.Now()
			})
		}
	}()

	for i := 0; i < iterations; i++ {
		u, found := rm.GetUploadByItem(batchID, itemID)
		if !found || u == nil {
			t.Fatalf("GetUploadByItem failed at iteration %d while a checkpoint was in flight", i)
		}
	}
	<-writerDone

	// Identity fields must still resolve after concurrent rewrites of the struct.
	u, found := rm.GetUploadByItem(batchID, itemID)
	if !found {
		t.Fatal("upload no longer resolvable by batch/item after concurrent checkpoints")
	}
	if meta := u.Meta(); meta.BatchID != batchID || meta.ItemID != itemID {
		t.Fatalf("identity mutated: batchID=%q itemID=%q", meta.BatchID, meta.ItemID)
	}
	if _, found := rm.GetUploadByItem(batchID, "no-such-item"); found {
		t.Fatal("lookup matched an item that does not exist")
	}
}
