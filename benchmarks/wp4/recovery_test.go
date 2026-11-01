package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// makeTestFixture writes a deterministic, non-compressible fixture so recovery
// behaviour can be validated without touching the 1/5/20 GB WP4 artifacts.
func makeTestFixture(t *testing.T, size int64) FixtureMetadata {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("fixture_%d.bin", size))
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte((i*1664525 + 1013904223) & 0xff)
	}
	h := sha256.New()
	var written int64
	for written < size {
		n := int64(len(chunk))
		if size-written < n {
			n = size - written
		}
		if _, err := f.Write(chunk[:n]); err != nil {
			_ = f.Close()
			t.Fatalf("write fixture: %v", err)
		}
		h.Write(chunk[:n])
		written += n
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return FixtureMetadata{
		Name:      filepath.Base(path),
		Path:      path,
		SizeBytes: size,
		SHA256:    hex.EncodeToString(h.Sum(nil)),
	}
}

func startTestHarness(t *testing.T, runID string, isolated bool) (*BenchHarness, func()) {
	t.Helper()
	h, cleanup, err := StartBenchServer(t.TempDir())
	if err != nil {
		t.Fatalf("start bench server: %v", err)
	}
	h.runID = runID
	h.isolated = isolated
	return h, cleanup
}

// createTusUpload declares a tus upload for a batch item and returns its URL.
func createTusUpload(t *testing.T, h *BenchHarness, batchID, itemID string, size int64) string {
	t.Helper()
	tusURL := fmt.Sprintf("%s/api/batches/%s/items/%s/tus", h.serverURL, batchID, itemID)
	req, _ := http.NewRequest(http.MethodPost, tusURL, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.FormatInt(size, 10))
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("tus create: %v", err)
	}
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusCreated {
		t.Fatalf("tus create: unexpected HTTP %d", code)
	}
	return tusURL
}

// assertGenuineInterruption encodes the evidence a real interrupted HTTP request
// must leave behind. A clean partial tus PATCH cannot satisfy it: that path ends
// in state `active` with HTTP 204 and no client transport error.
func assertGenuineInterruption(t *testing.T, rec RecoveryRunResult, fix FixtureMetadata) {
	t.Helper()
	if rec.InterruptDeclaredLength != fix.SizeBytes {
		t.Fatalf("interrupted request declared Content-Length %d, expected the full upload length %d", rec.InterruptDeclaredLength, fix.SizeBytes)
	}
	if rec.InterruptBytesSent != rec.InterruptOffset {
		t.Fatalf("client body supplied %d bytes, expected the interruption target %d", rec.InterruptBytesSent, rec.InterruptOffset)
	}
	if rec.InterruptServerStatus == http.StatusNoContent {
		t.Fatal("server answered the interruption with 204 No Content: it saw a complete request, not truncation")
	}
	if !rec.ServerSawTruncation {
		t.Fatalf("server state after interruption was %q, expected %q", rec.InterruptedState, transfer.ResumableInterrupted)
	}
	if rec.ServerCommitted <= 0 {
		t.Fatalf("server committed no recoverable prefix (committed=%d)", rec.ServerCommitted)
	}
	if rec.ServerCommitted >= fix.SizeBytes {
		t.Fatalf("upload is not incomplete: server committed %d of %d bytes", rec.ServerCommitted, fix.SizeBytes)
	}
	if rec.ServerCommitted > rec.InterruptOffset {
		t.Fatalf("server committed %d beyond the %d bytes the client supplied", rec.ServerCommitted, rec.InterruptOffset)
	}
	wantRetransmit := rec.InterruptOffset - rec.ServerCommitted
	if wantRetransmit < 0 {
		wantRetransmit = 0
	}
	if rec.RetransmittedBytes != wantRetransmit {
		t.Fatalf("retransmitted accounting %d, expected max(0, target-committed)=%d", rec.RetransmittedBytes, wantRetransmit)
	}
}

// Test A — a genuine HTTP request interruption. The request declares the full
// upload length but the body dies after the interruption target, so the server
// must observe a truncated body and retain a recoverable prefix.
func TestGenuineInterruptionProducesTruncatedRequest(t *testing.T) {
	fix := makeTestFixture(t, 12<<20)

	h, cleanup := startTestHarness(t, "test-interrupt", true)
	defer cleanup()

	rec, err := h.RunConfig6_InterruptionRecovery(fix, 50)
	if err != nil {
		t.Fatalf("genuine interruption recovery failed: %v (notes: %s)", err, rec.Notes)
	}
	assertGenuineInterruption(t, rec, fix)
	if !rec.Valid {
		t.Fatalf("recovery record is not valid: %+v", rec)
	}

	t.Logf("interrupt evidence: declared=%d sent=%d server_status=%d server_state=%q committed=%d settle=%.1fms attempts=%d retransmitted=%d",
		rec.InterruptDeclaredLength, rec.InterruptBytesSent, rec.InterruptServerStatus,
		rec.InterruptedState, rec.ServerCommitted, rec.SettleWaitMs, rec.ResumeAttempts, rec.RetransmittedBytes)
}

// Test A (contrast) — a *complete* partial tus PATCH (Content-Length equal to
// the bytes supplied) must NOT look like an interruption. This is the previous
// benchmark's defect: it declared exactly the prefix, so the server saw a
// well-formed request and answered 204.
func TestCompletePartialPatchIsNotAnInterruption(t *testing.T) {
	fix := makeTestFixture(t, 4<<20)

	h, cleanup := startTestHarness(t, "test-contrast", true)
	defer cleanup()

	bID, itID, err := h.createBatch("contrast.bin", fix.SizeBytes)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	tusURL := createTusUpload(t, h, bID, itID, fix.SizeBytes)

	prefix := fix.SizeBytes / 4
	f, err := os.Open(fix.Path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	req, _ := http.NewRequest(http.MethodPatch, tusURL, io.LimitReader(f, prefix))
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Offset", "0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.ContentLength = prefix // a COMPLETE request for `prefix` bytes

	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("complete partial PATCH failed: %v", err)
	}
	code := resp.StatusCode
	resp.Body.Close()
	if code != http.StatusNoContent {
		t.Fatalf("clean partial PATCH returned HTTP %d, expected 204", code)
	}

	up, ok := h.services.EnsureResumable().GetUploadByItem(bID, itID)
	if !ok {
		t.Fatal("upload not found")
	}
	m := up.Meta()
	if m.State == transfer.ResumableInterrupted {
		t.Fatalf("a complete partial PATCH was recorded as %q; the classifier would wrongly accept it", m.State)
	}
	if m.State != transfer.ResumableActive {
		t.Logf("partial PATCH left state %q (not %q); still not an interruption", m.State, transfer.ResumableInterrupted)
	}
	if m.CommittedOffset != prefix {
		t.Fatalf("clean partial PATCH committed %d, expected %d", m.CommittedOffset, prefix)
	}
}

// Test B — recovery at 10%, 50% and 99% of a deterministic fixture, asserting
// the full chain: interruption evidence, stable offset, HEAD consistency,
// mandatory prefix verification, authoritative resume offset, completion and
// final SHA-256 integrity.
func TestConfig6_RecoveryCompletesAt10_50_99(t *testing.T) {
	fix := makeTestFixture(t, 8<<20)

	for _, pct := range []int{10, 50, 99} {
		pct := pct
		t.Run(fmt.Sprintf("interrupt_%dpct", pct), func(t *testing.T) {
			h, cleanup := startTestHarness(t, "test-config6", true)
			defer cleanup()

			rec, err := h.RunConfig6_InterruptionRecovery(fix, pct)
			if err != nil {
				t.Fatalf("recovery at %d%%: %v (notes: %s)", pct, err, rec.Notes)
			}
			assertGenuineInterruption(t, rec, fix)
			if !rec.Valid || !rec.IntegrityPass {
				t.Fatalf("recovery at %d%% not valid: %+v", pct, rec)
			}
			if rec.SettleWaitMs <= 0 || rec.SettleObservations < 2 {
				t.Fatalf("settlement not evidenced: %.1f ms over %d observations", rec.SettleWaitMs, rec.SettleObservations)
			}
			if rec.ResumeStartOffset != rec.ServerCommitted {
				t.Fatalf("resume started at %d, HEAD reported committed %d", rec.ResumeStartOffset, rec.ServerCommitted)
			}
			if !rec.PrefixVerified {
				t.Fatal("prefix digest was not verified against the server's Upload-Prefix-Digest")
			}
			if rec.PrefixDigestServer == "" || rec.PrefixDigestServer != rec.PrefixDigestLocal {
				t.Fatalf("prefix digests do not agree: server=%q local=%q", rec.PrefixDigestServer, rec.PrefixDigestLocal)
			}
			if rec.FinalOffset != fix.SizeBytes {
				t.Fatalf("final offset %d, expected %d", rec.FinalOffset, fix.SizeBytes)
			}
			if rec.UploadState != string(transfer.ResumableAwaitingDigest) {
				t.Fatalf("resumed PATCH state %q, expected awaiting_digest", rec.UploadState)
			}
			if rec.AuthoritativeStatus != string(transfer.ResumableCompleted) {
				t.Fatalf("authoritative status %q, expected completed", rec.AuthoritativeStatus)
			}
			if rec.ResumeAttempts > 1 {
				t.Logf("resume needed %d attempts (transient refusal retried from the server-reported offset)", rec.ResumeAttempts)
			}
			t.Logf("%d%%: committed=%d target=%d retransmitted=%d settle=%.1fms head=%.1fms prefixRebuild=%.1fms total=%.1fms",
				pct, rec.ServerCommitted, rec.InterruptOffset, rec.RetransmittedBytes,
				rec.SettleWaitMs, rec.HeadTimeMs, rec.PrefixRebuildMs, rec.TotalCompletionMs)
		})
	}
}

// Test C — settling cannot return while the upload lifecycle lock is held.
func TestSettlementWaitsForLifecycleLock(t *testing.T) {
	h, cleanup := startTestHarness(t, "test-settle", false)
	defer cleanup()

	fix := makeTestFixture(t, 1<<20)
	bID, itID, err := h.createBatch("settle.bin", fix.SizeBytes)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	createTusUpload(t, h, bID, itID, fix.SizeBytes)

	up, ok := h.services.EnsureResumable().GetUploadByItem(bID, itID)
	if !ok {
		t.Fatal("resumable upload not found after tus create")
	}

	up.Lock()
	holdFor := 80 * time.Millisecond
	go func() {
		time.Sleep(holdFor)
		up.Unlock()
	}()

	start := time.Now()
	offset, waited, err := h.waitForUploadSettled(bID, itID, 2*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if elapsed < holdFor {
		t.Fatalf("settlement returned after %v while the lifecycle lock was held for %v", elapsed, holdFor)
	}
	if waited > elapsed {
		t.Fatalf("reported settle wait %v exceeds observed %v", waited, elapsed)
	}
	if offset != 0 {
		t.Fatalf("expected committed offset 0 before any PATCH, got %d", offset)
	}
}

// Test C2 — the recovery-specific settlement gate must not accept a freshly
// created upload that has no interruption yet, even when its lock is free.
func TestInterruptedSettlementRejectsFreshUpload(t *testing.T) {
	h, cleanup := startTestHarness(t, "test-settle-fresh", false)
	defer cleanup()

	fix := makeTestFixture(t, 1<<20)
	bID, itID, err := h.createBatch("fresh.bin", fix.SizeBytes)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	createTusUpload(t, h, bID, itID, fix.SizeBytes)

	start := time.Now()
	_, _, waited, _, err := h.waitForInterruptedSettlement(bID, itID, 250*time.Millisecond, fix.SizeBytes)
	if err == nil {
		t.Fatal("settlement accepted an upload that was never interrupted")
	}
	if waited < 250*time.Millisecond {
		t.Fatalf("settlement gave up after %v, before its bounded timeout; it must not short-circuit", waited)
	}
	t.Logf("correctly refused after %v: %v", time.Since(start), err)
}

// Test D — the resumed PATCH must use the offset HEAD reports, not the offset
// the client last wrote. A stale offset is refused with 409 carrying the
// authoritative Upload-Offset.
func TestResumeUsesAuthoritativeHeadOffset(t *testing.T) {
	fix := makeTestFixture(t, 4<<20)

	h, cleanup := startTestHarness(t, "test-head-offset", true)
	defer cleanup()

	bID, itID, err := h.createBatch("head-offset.bin", fix.SizeBytes)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	tusURL := createTusUpload(t, h, bID, itID, fix.SizeBytes)

	committed := fix.SizeBytes / 4
	f, err := os.Open(fix.Path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	patch, _ := http.NewRequest(http.MethodPatch, tusURL, io.LimitReader(f, committed))
	patch.Header.Set("Authorization", "Bearer "+h.token)
	patch.Header.Set("Tus-Resumable", "1.0.0")
	patch.Header.Set("Upload-Offset", "0")
	patch.Header.Set("Content-Type", "application/offset+octet-stream")
	patch.ContentLength = committed
	resp, err := h.client.Do(patch)
	if err != nil {
		t.Fatalf("partial PATCH: %v", err)
	}
	resp.Body.Close()
	f.Close()

	// HEAD is the discovery step: it must report the durably committed offset.
	headReq, _ := http.NewRequest(http.MethodHead, tusURL, nil)
	headReq.Header.Set("Authorization", "Bearer "+h.token)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headResp, err := h.client.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	headOffset := headResp.Header.Get("Upload-Offset")
	headResp.Body.Close()
	if headOffset != strconv.FormatInt(committed, 10) {
		t.Fatalf("HEAD reported offset %s, expected %d", headOffset, committed)
	}

	// A resume from a stale (deliberately wrong) offset must be refused with the
	// authoritative offset, never silently accepted.
	stale := committed / 2
	staleReq, _ := http.NewRequest(http.MethodPatch, tusURL, io.LimitReader(f, 0))
	staleReq.Header.Set("Authorization", "Bearer "+h.token)
	staleReq.Header.Set("Tus-Resumable", "1.0.0")
	staleReq.Header.Set("Upload-Offset", strconv.FormatInt(stale, 10))
	staleReq.Header.Set("Content-Type", "application/offset+octet-stream")
	staleReq.ContentLength = 0
	staleResp, err := h.client.Do(staleReq)
	if err != nil {
		t.Fatalf("stale-offset PATCH: %v", err)
	}
	staleCode := staleResp.StatusCode
	staleOffset := staleResp.Header.Get("Upload-Offset")
	staleResp.Body.Close()
	if staleCode != http.StatusConflict {
		t.Fatalf("stale-offset PATCH returned HTTP %d, expected 409", staleCode)
	}
	if staleOffset != strconv.FormatInt(committed, 10) {
		t.Fatalf("409 reported Upload-Offset %q, expected the authoritative %d", staleOffset, committed)
	}

	// Resuming from HEAD's offset completes the upload.
	remaining, err := os.Open(fix.Path)
	if err != nil {
		t.Fatalf("reopen fixture: %v", err)
	}
	defer remaining.Close()
	if _, err := remaining.Seek(committed, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	resumeReq, _ := http.NewRequest(http.MethodPatch, tusURL, remaining)
	resumeReq.Header.Set("Authorization", "Bearer "+h.token)
	resumeReq.Header.Set("Tus-Resumable", "1.0.0")
	resumeReq.Header.Set("Upload-Offset", headOffset)
	resumeReq.Header.Set("Content-Type", "application/offset+octet-stream")
	resumeReq.ContentLength = fix.SizeBytes - committed
	resumeResp, err := h.client.Do(resumeReq)
	if err != nil {
		t.Fatalf("resume PATCH: %v", err)
	}
	resumeCode := resumeResp.StatusCode
	finalOffset := resumeResp.Header.Get("Upload-Offset")
	resumeResp.Body.Close()
	if resumeCode != http.StatusNoContent {
		t.Fatalf("resume from HEAD offset returned HTTP %d", resumeCode)
	}
	if finalOffset != strconv.FormatInt(fix.SizeBytes, 10) {
		t.Fatalf("resume ended at %s, expected %d", finalOffset, fix.SizeBytes)
	}
}

// Test E — a nonzero committed offset with a missing, malformed or incorrect
// server prefix digest must invalidate recovery.
func TestPrefixDigestVerificationIsStrict(t *testing.T) {
	const good = "3a7bd3e2360a3d29eea436fcfb7e44c735d117c42d1c1835420b6b9942dd4f1b"
	const other = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	cases := []struct {
		name   string
		offset int64
		server string
		want   bool
	}{
		{"matching nonzero offset", 4096, good, true},
		{"missing at nonzero offset", 4096, "", false},
		{"malformed at nonzero offset", 4096, "not-a-digest", false},
		{"uppercase at nonzero offset", 4096, "3A7BD3E2360A3D29EEA436FCFB7E44C735D117C42D1C1835420B6B9942DD4F1B", false},
		{"mismatched at nonzero offset", 4096, other, false},
		{"zero offset needs no digest", 0, "", true},
	}
	for _, tc := range cases {
		err := checkPrefixDigest(tc.offset, tc.server, good)
		if tc.want && err != nil {
			t.Errorf("%s: expected acceptance, got %v", tc.name, err)
		}
		if !tc.want && err == nil {
			t.Errorf("%s: expected rejection, got acceptance", tc.name)
		}
	}
}

// Test E2 — end to end: when the server loses its prefix digest, HEAD stops
// advertising one and the harness's verification must fail rather than treating
// the absent header as success.
func TestMissingPrefixDigestFailsRecovery(t *testing.T) {
	fix := makeTestFixture(t, 4<<20)

	h, cleanup := startTestHarness(t, "test-prefix", true)
	defer cleanup()

	bID, itID, err := h.createBatch("prefix.bin", fix.SizeBytes)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	tusURL := createTusUpload(t, h, bID, itID, fix.SizeBytes)

	committed := fix.SizeBytes / 4
	f, err := os.Open(fix.Path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	patch, _ := http.NewRequest(http.MethodPatch, tusURL, io.LimitReader(f, committed))
	patch.Header.Set("Authorization", "Bearer "+h.token)
	patch.Header.Set("Tus-Resumable", "1.0.0")
	patch.Header.Set("Upload-Offset", "0")
	patch.Header.Set("Content-Type", "application/offset+octet-stream")
	patch.ContentLength = committed
	resp, err := h.client.Do(patch)
	if err != nil {
		t.Fatalf("partial PATCH: %v", err)
	}
	resp.Body.Close()
	f.Close()

	up, ok := h.services.EnsureResumable().GetUploadByItem(bID, itID)
	if !ok {
		t.Fatal("upload not found")
	}
	original := up.Meta().PrefixDigest
	if original == "" {
		t.Fatal("precondition failed: server did not record a prefix digest")
	}

	headDigest := func() (string, int64) {
		req, _ := http.NewRequest(http.MethodHead, tusURL, nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		req.Header.Set("Tus-Resumable", "1.0.0")
		r, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("HEAD: %v", err)
		}
		d := r.Header.Get("Upload-Prefix-Digest")
		off, _ := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
		r.Body.Close()
		return d, off
	}

	digest, off := headDigest()
	if off != committed || digest != original {
		t.Fatalf("precondition: HEAD offset=%d digest=%q", off, digest)
	}
	local, err := hashPrefix(fix.Path, off)
	if err != nil {
		t.Fatalf("hash prefix: %v", err)
	}
	localHex := hex.EncodeToString(local.Sum(nil))
	if err := checkPrefixDigest(off, digest, localHex); err != nil {
		t.Fatalf("precondition: matching digest rejected: %v", err)
	}

	// Simulate a server that lost the hash state for the committed prefix.
	up.UpdateMeta(func(m *transfer.ResumableMeta) { m.PrefixDigest = "" })
	digest, off = headDigest()
	if off != committed {
		t.Fatalf("offset changed to %d; expected %d", off, committed)
	}
	if digest != "" {
		t.Fatalf("server still advertised digest %q after losing it", digest)
	}
	if err := checkPrefixDigest(off, digest, localHex); err == nil {
		t.Fatal("missing prefix digest was accepted as verification success")
	}

	// Restore, then confirm a mismatching digest is rejected too.
	up.UpdateMeta(func(m *transfer.ResumableMeta) { m.PrefixDigest = original })
	digest, off = headDigest()
	if err := checkPrefixDigest(off, digest, hex.EncodeToString([]byte("different"))); err == nil {
		t.Fatal("mismatched prefix digest was accepted")
	}
}

// Test F — a recovery run must not contaminate the throughput session.
//
// Root cause (verified in the previous run): ClaimItem only leases the next
// ELIGIBLE item in the coordinator queue, so an unfinished queued tus item left
// by a recovery batch blocks later legacy PUT claims in the same session.
// Isolating each recovery experiment in its own server/session prevents that.
func TestIsolatedRecoveryDoesNotPoisonThroughputQueue(t *testing.T) {
	fix := makeTestFixture(t, 4<<20)

	throughput, cleanupThroughput := startTestHarness(t, "test-throughput", false)
	defer cleanupThroughput()

	before, err := throughput.RunConfig1_LegacyPUT(fix, 1)
	if err != nil || !before.Valid {
		t.Fatalf("baseline legacy PUT failed before recovery: %v (notes: %s)", err, before.Notes)
	}

	recovery, cleanupRecovery := startTestHarness(t, "test-recovery", true)
	rec, recErr := recovery.RunConfig6_InterruptionRecovery(fix, 50)
	cleanupRecovery()
	if recErr != nil {
		t.Fatalf("isolated recovery failed: %v (notes: %s)", recErr, rec.Notes)
	}

	after, err := throughput.RunConfig1_LegacyPUT(fix, 2)
	if err != nil || !after.Valid {
		t.Fatalf("throughput session was poisoned by a recovery run: %v (notes: %s)", err, after.Notes)
	}
	if !after.IntegrityPass {
		t.Fatalf("post-recovery throughput run failed integrity: %+v", after)
	}
}

// Test G — several recoveries in sequence must all complete in the session they
// share, proving an expected interruption does not wedge that session.
func TestRepeatedRecoveryDoesNotPauseSession(t *testing.T) {
	fix := makeTestFixture(t, 4<<20)

	h, cleanup := startTestHarness(t, "test-repeat", false)
	defer cleanup()

	for _, pct := range []int{10, 50, 99} {
		rec, err := h.RunConfig6_InterruptionRecovery(fix, pct)
		if err != nil {
			t.Fatalf("recovery iteration at %d%%: %v (notes: %s)", pct, err, rec.Notes)
		}
		assertGenuineInterruption(t, rec, fix)
		if !rec.Valid {
			t.Fatalf("recovery at %d%% reported invalid: %+v", pct, rec)
		}
	}
}
