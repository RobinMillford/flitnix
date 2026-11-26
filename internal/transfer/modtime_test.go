package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateModTimeMsBounds(t *testing.T) {
	now := time.Now()
	validPast := now.Add(-48 * time.Hour).UnixMilli()
	if err := ValidateModTimeMs(0); err != nil {
		t.Fatalf("0 should be valid: %v", err)
	}
	if err := ValidateModTimeMs(validPast); err != nil {
		t.Fatalf("validPast failed: %v", err)
	}
	if err := ValidateModTimeMs(-1); err == nil {
		t.Fatal("negative lastModifiedMs must fail validation")
	}
	farFuture := now.Add(MaxModTimeFutureSkew + 24*time.Hour).UnixMilli()
	if err := ValidateModTimeMs(farFuture); err == nil {
		t.Fatal("far-future lastModifiedMs must fail validation")
	}

	mBadNeg := BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFiles,
		Items:     []ManifestItem{{DisplayName: "bad.jpg", Size: 5, LastModifiedMs: -50}},
	}
	if _, err := ValidateManifest(mBadNeg); err == nil {
		t.Fatal("expected ValidateManifest to reject negative lastModifiedMs")
	}
}

func TestStorePreserveModTimesPutAndToggle(t *testing.T) {
	origTime := time.Date(2022, 6, 15, 14, 30, 45, 0, time.UTC)
	origMs := origTime.UnixMilli()

	t.Run("preserve enabled (default)", func(t *testing.T) {
		svc, store := newTestService(t)
		if !store.PreserveModTimes() {
			t.Fatal("PreserveModTimes should default to true")
		}
		payload := []byte("single-pass content with date")
		up, err := svc.Declare("sess-1", "dated.txt", int64(len(payload)))
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.SetUploadModTime(up.ID, origMs); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Decide(up.ID, true); err != nil {
			t.Fatal(err)
		}
		res, err := svc.SubmitContentWithPublish(up.ID, "sess-1", bytes.NewReader(payload), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(res.Path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.ModTime().UTC().Truncate(time.Second); !got.Equal(origTime) {
			t.Fatalf("published mtime = %v, want %v", got, origTime)
		}
	})

	t.Run("preserve disabled uses receive time", func(t *testing.T) {
		svc, store := newTestService(t)
		store.SetPreserveModTimes(false)
		payload := []byte("content without preserved date")
		up, err := svc.Declare("sess-1", "recent.txt", int64(len(payload)))
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.SetUploadModTime(up.ID, origMs); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Decide(up.ID, true); err != nil {
			t.Fatal(err)
		}
		res, err := svc.SubmitContentWithPublish(up.ID, "sess-1", bytes.NewReader(payload), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(res.Path)
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(info.ModTime()) > time.Minute {
			t.Fatalf("expected receive-time mtime when disabled, got %v", info.ModTime())
		}
	})
}

func TestResumableChunkUploadPreservesModTimeAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x42}, 32)
	store, err := NewStore(StoreConfig{Dir: dir, MetaKey: key})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Config{Store: store})

	origTime := time.Date(2020, 1, 20, 8, 15, 0, 0, time.UTC)
	origMs := origTime.UnixMilli()

	payload := bytes.Repeat([]byte("M"), 256*1024)
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])

	up, err := svc.DeclareBatchItem("sess-1", "", "", "archive_photo.raw", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.SetUploadModTime(up.ID, origMs); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideBatchItems([]string{up.ID}, true); err != nil {
		t.Fatal(err)
	}

	split := 64 * 1024
	if _, err := svc.SubmitChunk(t.Context(), up.ID, "sess-1", 0, int64(split), bytes.NewReader(payload[:split]), nil, nil); err != nil {
		t.Fatalf("chunk 1: %v", err)
	}

	// Simulate fresh Service after restart using .meta on disk
	arts, _, err := store.ScanResumableMeta()
	if err != nil || len(arts) != 1 {
		t.Fatalf("ScanResumableMeta = (%d, %v)", len(arts), err)
	}
	if arts[0].Meta.LastModifiedMs != origMs {
		t.Fatalf("Meta.LastModifiedMs = %d, want %d", arts[0].Meta.LastModifiedMs, origMs)
	}

	store2, err := NewStore(StoreConfig{Dir: dir, MetaKey: key})
	if err != nil {
		t.Fatal(err)
	}
	svc2 := NewService(Config{Store: store2})
	if err := svc2.RestoreApprovedUpload(RestoreUploadOpts{
		ID:              arts[0].Meta.UploadID,
		SessionKey:      arts[0].Meta.SessionKey,
		Name:            arts[0].Meta.FinalName,
		Size:            arts[0].Meta.DeclaredSize,
		LastModifiedMs:  arts[0].Meta.LastModifiedMs,
		ExpectedSHA256:  sumHex,
		State:           StateInterrupted,
		PartPath:        arts[0].PartPath,
		MetaPath:        arts[0].MetaPath,
		FinalDir:        filepath.Dir(arts[0].PartPath),
		CommittedOffset: arts[0].Meta.CommittedOffset,
		ChunkSize:       arts[0].Meta.ChunkSize,
		SHA256State:     arts[0].Meta.SHA256State,
		BatchLinked:     true,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc2.SubmitChunk(t.Context(), up.ID, "sess-1", int64(split), int64(len(payload)-split), bytes.NewReader(payload[split:]), nil, nil)
	if err != nil {
		t.Fatalf("chunk 2: %v", err)
	}
	if !res.Completed {
		t.Fatal("expected completed upload after second chunk")
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.ModTime().UTC().Truncate(time.Second); !got.Equal(origTime) {
		t.Fatalf("resumed published mtime = %v, want %v", got, origTime)
	}
}
