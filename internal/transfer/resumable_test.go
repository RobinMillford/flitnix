package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func deterministicPattern(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed ^ byte(i*131+7)
	}
	return out
}

type failingReader struct {
	data  []byte
	limit int
	pos   int
	err   error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= f.limit {
		return 0, f.err
	}
	remain := f.limit - f.pos
	if remain > len(p) {
		remain = len(p)
	}
	copy(p, f.data[f.pos:f.pos+remain])
	f.pos += remain
	if f.pos >= f.limit && f.err != nil {
		return remain, f.err
	}
	return remain, nil
}

func TestResumableMetaWriteReadAndTamperDetection(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	metaPath := filepath.Join(dir, "video.mp4.part.abcd1234.meta")
	meta := ResumableMeta{
		UploadID:        "up-1",
		BatchID:         "batch-1",
		ItemID:          "item-1",
		SessionKey:      "sess-key-1",
		FinalName:       "video.mp4",
		PartRelPath:     "video.mp4.part.abcd1234",
		DeclaredSize:    10 << 20,
		CommittedOffset: 4 << 20,
		ChunkSize:       DefaultChunkSize,
	}
	if err := WriteResumableMeta(metaPath, key, meta); err != nil {
		t.Fatalf("WriteResumableMeta: %v", err)
	}

	got, err := ReadResumableMeta(metaPath, key)
	if err != nil {
		t.Fatalf("ReadResumableMeta: %v", err)
	}
	if got.CommittedOffset != 4<<20 || got.UploadID != "up-1" {
		t.Fatalf("unexpected meta read: %+v", got)
	}

	// Wrong key fails authentication cleanly.
	badKey := make([]byte, 32)
	_, _ = rand.Read(badKey)
	if _, err := ReadResumableMeta(metaPath, badKey); !errors.Is(err, ErrStagingQuarantined) {
		t.Fatalf("ReadResumableMeta wrong key = %v, want ErrStagingQuarantined", err)
	}

	// Tampered file content fails HMAC check.
	raw, _ := os.ReadFile(metaPath)
	raw[len(raw)/2] ^= 0x55
	_ = os.WriteFile(metaPath, raw, 0o600)
	if _, err := ReadResumableMeta(metaPath, key); !errors.Is(err, ErrStagingQuarantined) {
		t.Fatalf("ReadResumableMeta tampered = %v, want ErrStagingQuarantined", err)
	}
}

func TestReopenPartForAppendTruncatesTornTailAndDetectsShortPart(t *testing.T) {
	st := newTestStore(t)
	pf, err := st.OpenPart("clip.bin")
	if err != nil {
		t.Fatal(err)
	}
	partPath := pf.PartPath()
	// Simulate 1024 committed bytes + 300 uncommitted torn bytes before crash.
	if _, err := pf.Write(make([]byte, 1324)); err != nil {
		t.Fatal(err)
	}
	_ = pf.f.Close()

	f, truncated, err := st.ReopenPartForAppend(partPath, 1024)
	if err != nil {
		t.Fatalf("ReopenPartForAppend: %v", err)
	}
	_ = f.Close()
	if !truncated {
		t.Fatalf("expected truncated=true when disk size exceeded committed offset")
	}
	info, err := os.Stat(partPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1024 {
		t.Fatalf("part size = %d, want 1024", info.Size())
	}

	// File smaller than CommittedOffset must quarantine rather than pad holes.
	if _, _, err := st.ReopenPartForAppend(partPath, 2048); !errors.Is(err, ErrStagingQuarantined) {
		t.Fatalf("short part ReopenPartForAppend = %v, want ErrStagingQuarantined", err)
	}
}

func TestSubmitChunkResumableWithMidChunkInterruptionAndSHA256Resume(t *testing.T) {
	st := newTestStore(t)
	svc := NewService(Config{Store: st})

	payload := deterministicPattern(3*1024*1024+777, 0x3A)
	sum := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(sum[:])

	up, err := svc.DeclareWithDigest("sess-a", "archive.bin", int64(len(payload)), expectedHex, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(up.ID, true); err != nil {
		t.Fatal(err)
	}

	// Chunk 1: 1 MiB succeeds durably.
	chunk1Len := int64(1024 * 1024)
	res1, err := svc.SubmitChunk(context.Background(), up.ID, "sess-a", 0, chunk1Len, bytes.NewReader(payload[:chunk1Len]), nil, nil)
	if err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if res1.Completed || res1.CommittedOffset != chunk1Len {
		t.Fatalf("unexpected chunk 1 result: %+v", res1)
	}

	// Verify .part and .meta exist on disk.
	head, err := svc.HeadUpload(up.ID, "sess-a")
	if err != nil || head.CommittedOffset != chunk1Len {
		t.Fatalf("HeadUpload = %+v, err=%v", head, err)
	}

	// Offset mismatch returns OffsetMismatchError with expected=chunk1Len.
	_, mmErr := svc.SubmitChunk(context.Background(), up.ID, "sess-a", 0, chunk1Len, bytes.NewReader(payload[:chunk1Len]), nil, nil)
	var mm *OffsetMismatchError
	if !errors.As(mmErr, &mm) || mm.Expected != chunk1Len || mm.Got != 0 {
		t.Fatalf("expected OffsetMismatchError(expected=%d), got %v", chunk1Len, mmErr)
	}

	// Chunk 2: drops mid-chunk after 256 KiB -> rolls back .part to chunk1Len, preserves state Interrupted!
	chunk2Len := int64(1024 * 1024)
	fr := &failingReader{data: payload[chunk1Len : chunk1Len+chunk2Len], limit: 256 * 1024, err: io.ErrUnexpectedEOF}
	res2, err2 := svc.SubmitChunk(context.Background(), up.ID, "sess-a", chunk1Len, chunk2Len, fr, nil, nil)
	if !errors.Is(err2, ErrChunkInterrupted) || res2.CommittedOffset != chunk1Len {
		t.Fatalf("interrupted chunk 2 res=%+v err=%v", res2, err2)
	}
	if gotU, _ := svc.Get(up.ID); gotU.State != StateInterrupted || gotU.Uploaded != chunk1Len {
		t.Fatalf("expected StateInterrupted at %d, got %+v", chunk1Len, gotU)
	}

	// Retry Chunk 2 cleanly from chunk1Len, then Chunk 3 (remainder).
	res2b, err2b := svc.SubmitChunk(context.Background(), up.ID, "sess-a", chunk1Len, chunk2Len, bytes.NewReader(payload[chunk1Len:chunk1Len+chunk2Len]), nil, nil)
	if err2b != nil || res2b.CommittedOffset != chunk1Len+chunk2Len {
		t.Fatalf("chunk 2 retry: res=%+v err=%v", res2b, err2b)
	}
	remOffset := chunk1Len + chunk2Len
	remLen := int64(len(payload)) - remOffset
	res3, err3 := svc.SubmitChunk(context.Background(), up.ID, "sess-a", remOffset, remLen, bytes.NewReader(payload[remOffset:]), nil, nil)
	if err3 != nil || !res3.Completed || res3.SHA256 != expectedHex {
		t.Fatalf("final chunk: res=%+v err=%v", res3, err3)
	}

	// Published file must match byte-for-byte, and no .part / .meta files remain.
	published, err := os.ReadFile(res3.Path)
	if err != nil || !bytes.Equal(published, payload) {
		t.Fatalf("published bytes mismatch (err=%v)", err)
	}
	entries, _ := os.ReadDir(st.Dir())
	for _, e := range entries {
		if strings.Contains(e.Name(), partSuffixMarker) {
			t.Fatalf("leftover staging file after completion: %s", e.Name())
		}
	}
}

func TestPauseResumeAndDiscardCleanupWithQuota(t *testing.T) {
	st := newTestStore(t)
	quota := NewQuotaManager(QuotaConfig{DefaultDir: st.Dir(), SafetyMargin: 1})
	svc := NewService(Config{Store: st, Quota: quota})
	coord := NewCoordinator(CoordinatorConfig{})

	manifest := BatchManifest{
		Direction: DirectionPhoneToDesktop,
		Kind:      BatchKindFiles,
		Items: []ManifestItem{
			{DisplayName: "first.bin", Size: 2 << 20},
			{DisplayName: "second.bin", Size: 2 << 20},
		},
	}
	b, err := coord.CreateBatch("sess-p", manifest)
	if err != nil {
		t.Fatal(err)
	}
	var cids []string
	for _, it := range b.Items {
		u, err := svc.DeclareBatchItem("sess-p", "", "", it.DisplayName, it.DeclaredSize)
		if err != nil {
			t.Fatal(err)
		}
		svc.SetBatchContext(u.ID, b.ID, it.ID)
		_ = coord.SetItemContentID("sess-p", b.ID, it.ID, u.ID)
		cids = append(cids, u.ID)
	}
	if err := svc.DecideBatchItems(cids, true); err != nil {
		t.Fatal(err)
	}
	if _, err := coord.ApproveBatch("sess-p", b.ID); err != nil {
		t.Fatal(err)
	}

	lease, err := coord.ClaimChunk("sess-p", b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	chunk := deterministicPattern(512*1024, 0x7F)
	res, err := svc.SubmitChunk(context.Background(), cids[0], "sess-p", 0, int64(len(chunk)), bytes.NewReader(chunk), nil, nil)
	if err != nil || res.CommittedOffset != int64(len(chunk)) {
		t.Fatalf("chunk 1: %+v, err=%v", res, err)
	}
	_ = coord.ReleaseChunkLease("sess-p", lease, res.CommittedOffset)

	// Interrupt item 0: sibling item 1 MUST remain Queued!
	lease2, err := coord.ClaimChunk("sess-p", b.ID, b.Items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	bView, err := coord.InterruptItem("sess-p", lease2, res.CommittedOffset, errors.New("wifi drop"))
	if err != nil {
		t.Fatal(err)
	}
	if bView.State != BatchStateInterrupted || bView.Items[0].State != ItemStateInterrupted || bView.Items[1].State != ItemStateQueued {
		t.Fatalf("InterruptItem broke sibling or lost state: %+v", bView)
	}

	// Pause refuses chunks until resumed.
	pView, pCIDs, err := coord.PauseBatch("sess-p", b.ID)
	if err != nil || pView.State != BatchStatePaused {
		t.Fatalf("PauseBatch = %+v, err=%v", pView, err)
	}
	for _, cid := range pCIDs {
		_ = svc.Pause(cid)
	}
	if _, err := coord.ClaimChunk("sess-p", b.ID, b.Items[0].ID); !errors.Is(err, ErrUploadPaused) {
		t.Fatalf("ClaimChunk while paused = %v, want ErrUploadPaused", err)
	}

	// Resume restores Interrupted state cleanly.
	rView, rCIDs, err := coord.ResumeBatch("sess-p", b.ID)
	if err != nil || rView.State != BatchStateInterrupted || rView.Items[0].State != ItemStateInterrupted {
		t.Fatalf("ResumeBatch = %+v, err=%v", rView, err)
	}
	for _, cid := range rCIDs {
		_ = svc.Resume(cid)
	}

	// Discard deletes staging files and releases all reserved quota.
	dView, dCIDs, err := coord.DiscardBatch("sess-p", b.ID)
	if err != nil || dView.Items[0].State != ItemStateDiscarded {
		t.Fatalf("DiscardBatch = %+v, err=%v", dView, err)
	}
	for _, cid := range dCIDs {
		_ = svc.Discard(cid)
	}
	status := quota.Status(st.Dir())
	if status.ReservedBytes != 0 {
		t.Fatalf("ReservedBytes after DiscardBatch = %d, want 0", status.ReservedBytes)
	}
	entries, _ := os.ReadDir(st.Dir())
	if len(entries) != 0 {
		t.Fatalf("DiscardBatch left files on disk: %+v", entries)
	}
}
