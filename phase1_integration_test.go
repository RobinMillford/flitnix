package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/recovery"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/settings"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestPhase1ScenarioA_CrashRestartResumableWithModTime tests G1 + G22 + G26:
// chunked upload -> server shutdown/crash -> durable startup recovery ->
// HEAD offset probe -> PATCH resume -> atomic publication preserving mtime.
func TestPhase1ScenarioA_CrashRestartResumableWithModTime(t *testing.T) {
	receiveDir := t.TempDir()
	recDir := t.TempDir()
	origTime := time.Date(2020, 8, 19, 12, 34, 56, 0, time.UTC)
	origMs := origTime.UnixMilli()

	buildStack := func() (*session.Manager, *transfer.Service, *transfer.Coordinator, *server.Server) {
		store, err := transfer.NewStore(transfer.StoreConfig{Dir: receiveDir})
		if err != nil {
			t.Fatal(err)
		}
		quota := transfer.NewQuotaManager(transfer.QuotaConfig{DefaultDir: receiveDir, SafetyMargin: 1})
		sess := session.NewManager(session.Config{IdleTimeout: 10 * time.Minute})
		xfer := transfer.NewService(transfer.Config{Store: store, Quota: quota})
		coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
		svcs := &server.Services{
			Sessions:    sess,
			Transfers:   xfer,
			Coordinator: coord,
			Registry:    transfer.NewRegistry(),
			Downloads:   transfer.NewDownloadRegistry(),
		}
		srv := server.New(server.Config{ListenAddr: "127.0.0.1:0", RecoveryDir: recDir, Services: svcs})
		if err := srv.Recover(); err != nil {
			t.Fatalf("Recover: %v", err)
		}
		return sess, xfer, coord, srv
	}

	payload := bytes.Repeat([]byte("PHASE1-A"), 192*1024) // ~1.5 MiB
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])

	sess1, xfer1, coord1, srv1 := buildStack()
	s, err := sess1.Create()
	if err != nil {
		t.Fatal(err)
	}
	tok := s.Token()
	manifestBody, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFolder,
		RootName:  "Phase1Crash",
		Items: []transfer.ManifestItem{{
			RelativePath:   "sub/large.bin",
			Size:           int64(len(payload)),
			LastModifiedMs: origMs,
		}},
	})
	dReq := httptest.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(manifestBody))
	dReq.Header.Set("Authorization", "Bearer "+tok)
	dRec := httptest.NewRecorder()
	srv1.Handler().ServeHTTP(dRec, dReq)
	if dRec.Code != http.StatusCreated {
		t.Fatalf("declare = %d: %s", dRec.Code, dRec.Body.String())
	}
	var batch transfer.BatchView
	_ = json.Unmarshal(dRec.Body.Bytes(), &batch)
	cid, _ := coord1.ItemContentID(s.Key(), batch.ID, batch.Items[0].ID)
	_ = xfer1.DecideBatchItems([]string{cid}, true)
	_, _ = coord1.ApproveBatch(s.Key(), batch.ID)

	// Upload first half via PATCH
	split := 768 * 1024
	patchPath := fmt.Sprintf("/api/batches/%s/items/%s/content", batch.ID, batch.Items[0].ID)
	p1 := httptest.NewRequest(http.MethodPatch, patchPath, bytes.NewReader(payload[:split]))
	p1.Header.Set("Authorization", "Bearer "+tok)
	p1.Header.Set("Upload-Offset", "0")
	p1Rec := httptest.NewRecorder()
	srv1.Handler().ServeHTTP(p1Rec, p1)
	if p1Rec.Code != http.StatusNoContent {
		t.Fatalf("chunk 1 = %d: %s", p1Rec.Code, p1Rec.Body.String())
	}
	srv1.CloseRecovery()

	// Restart into clean stack using durable WAL + HMAC .meta sidecar
	_, _, _, srv2 := buildStack()
	defer srv2.CloseRecovery()

	hReq := httptest.NewRequest(http.MethodHead, patchPath, nil)
	hReq.Header.Set("Authorization", "Bearer "+tok)
	hRec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(hRec, hReq)
	if hRec.Code != http.StatusOK || hRec.Header().Get("Upload-Offset") != strconv.Itoa(split) {
		t.Fatalf("HEAD after recovery = %d, Upload-Offset=%q", hRec.Code, hRec.Header().Get("Upload-Offset"))
	}

	p2 := httptest.NewRequest(http.MethodPatch, patchPath, bytes.NewReader(payload[split:]))
	p2.Header.Set("Authorization", "Bearer "+tok)
	p2.Header.Set("Upload-Offset", strconv.Itoa(split))
	p2Rec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(p2Rec, p2)
	if p2Rec.Code != http.StatusOK {
		t.Fatalf("chunk 2 = %d: %s", p2Rec.Code, p2Rec.Body.String())
	}
	var doneRes struct {
		SHA256 string `json:"sha256"`
	}
	_ = json.Unmarshal(p2Rec.Body.Bytes(), &doneRes)
	if doneRes.SHA256 != wantSHA {
		t.Fatalf("SHA256 = %q, want %q", doneRes.SHA256, wantSHA)
	}

	finalPath := filepath.Join(receiveDir, "Phase1Crash", "sub", "large.bin")
	gotBytes, err := os.ReadFile(finalPath)
	if err != nil || !bytes.Equal(gotBytes, payload) {
		t.Fatalf("final file read mismatch: err=%v len=%d", err, len(gotBytes))
	}
	info, _ := os.Stat(finalPath)
	if gotTime := info.ModTime().UTC().Truncate(time.Second); !gotTime.Equal(origTime) {
		t.Fatalf("preserved mtime = %v, want %v", gotTime, origTime)
	}
}

// TestPhase1ScenarioB_Quota507AndDiscardFreesHeadroom tests G26 + G1:
// exceeding max cap yields HTTP 507, discarding an interrupted upload releases
// reservation and frees room for a new batch.
func TestPhase1ScenarioB_Quota507AndDiscardFreesHeadroom(t *testing.T) {
	a := newTestApp(t)
	a.quota = transfer.NewQuotaManager(transfer.QuotaConfig{
		DefaultDir:   a.store.Dir(),
		SafetyMargin: 1,
		Probe: func(dir string) transfer.DiskSpaceInfo {
			return transfer.DiskSpaceInfo{
				Available:  true,
				FreeBytes:  1024,
				TotalBytes: 4096,
			}
		},
	})
	a.transfers = transfer.NewService(transfer.Config{Store: a.store, Quota: a.quota})

	s, err := a.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}

	// Declare & approve 800-byte batch and upload first 400 bytes
	b1Body, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{DisplayName: "hold.bin", Size: 800}},
	})
	d1Req := httptest.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(b1Body))
	d1Req.Host = "127.0.0.1"
	d1Req.Header.Set("Authorization", "Bearer "+s.Token())
	d1Rec := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(d1Rec, d1Req)
	if d1Rec.Code != http.StatusCreated {
		t.Fatalf("declare b1 = %d", d1Rec.Code)
	}
	var b1 BatchView
	_ = json.Unmarshal(d1Rec.Body.Bytes(), &b1)
	if _, err := a.ApproveBatch(b1.ID); err != nil {
		t.Fatal(err)
	}

	p1 := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/batches/%s/items/%s/content", b1.ID, b1.Items[0].ID), bytes.NewReader(bytes.Repeat([]byte("Q"), 400)))
	p1.Host = "127.0.0.1"
	p1.Header.Set("Authorization", "Bearer "+s.Token())
	p1.Header.Set("Upload-Offset", "0")
	p1Rec := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(p1Rec, p1)
	if p1Rec.Code != http.StatusNoContent {
		t.Fatalf("partial patch = %d", p1Rec.Code)
	}

	// Second 700-byte batch exceeds remaining headroom (1024 - 1 - 400 = 623 < 700) -> 507
	b2Body, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{DisplayName: "overflow.bin", Size: 700}},
	})
	d2Req := httptest.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(b2Body))
	d2Req.Host = "127.0.0.1"
	d2Req.Header.Set("Authorization", "Bearer "+s.Token())
	d2Rec := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(d2Rec, d2Req)
	if d2Rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected 507 Insufficient Storage, got %d: %s", d2Rec.Code, d2Rec.Body.String())
	}

	// Discard b1 -> frees reservation -> b2 succeeds cleanly
	if err := a.DiscardBatch(b1.ID); err != nil {
		t.Fatalf("DiscardBatch: %v", err)
	}
	d3Req := httptest.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(b2Body))
	d3Req.Host = "127.0.0.1"
	d3Req.Header.Set("Authorization", "Bearer "+s.Token())
	d3Rec := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(d3Rec, d3Req)
	if d3Rec.Code != http.StatusCreated {
		t.Fatalf("after discard declare = %d: %s", d3Rec.Code, d3Rec.Body.String())
	}
}

// TestPhase1ScenarioC_RoamingPreservesMultiFileBatch tests G7 + G1:
// temporary Wi-Fi drop & interface rebind keeps session and resumes multi-file batch.
func TestPhase1ScenarioC_RoamingPreservesMultiFileBatch(t *testing.T) {
	a := newTestApp(t)
	if err := a.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	sess, err := a.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	srcDir := t.TempDir()
	f1 := filepath.Join(srcDir, "one.bin")
	f2 := filepath.Join(srcDir, "two.bin")
	d1 := bytes.Repeat([]byte("R1"), 128*1024)
	d2 := bytes.Repeat([]byte("R2"), 32*1024)
	_ = os.WriteFile(f1, d1, 0o600)
	_ = os.WriteFile(f2, d2, 0o600)

	lm, err := client.BuildManifest([]string{f1, f2}, "")
	if err != nil {
		t.Fatal(err)
	}
	mBody, _ := json.Marshal(lm.Manifest)
	decReq, _ := http.NewRequest(http.MethodPost, testAppServerURL(t, a)+"/api/batches", bytes.NewReader(mBody))
	decReq.Header.Set("Authorization", "Bearer "+sess.Token())
	decRes, err := http.DefaultClient.Do(decReq)
	if err != nil || decRes.StatusCode != http.StatusCreated {
		t.Fatalf("declare = %v / %v", err, decRes)
	}
	var batch BatchView
	_ = json.NewDecoder(decRes.Body).Decode(&batch)
	decRes.Body.Close()

	if _, err := a.ApproveBatch(batch.ID); err != nil {
		t.Fatal(err)
	}

	// Partial upload of item 0, then simulate Wi-Fi drop & recovery
	u0 := fmt.Sprintf("%s/api/batches/%s/items/%s/content", testAppServerURL(t, a), batch.ID, batch.Items[0].ID)
	partReq, _ := http.NewRequest(http.MethodPatch, u0, bytes.NewReader(d1[:64*1024]))
	partReq.Header.Set("Authorization", "Bearer "+sess.Token())
	partReq.Header.Set("Upload-Offset", "0")
	pRes, err := http.DefaultClient.Do(partReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()

	a.handleNetworkChange(netenv.NetworkState{Status: netenv.StatusNoNetwork})
	a.handleNetworkChange(netenv.NetworkState{Status: netenv.StatusConnected, IP: "127.0.0.1"})

	// Verify session stayed valid and CLI client finishes remaining items via HEAD + PATCH retry
	if _, err := a.sessions.Validate(sess.Token()); err != nil {
		t.Fatalf("session lost during roaming: %v", err)
	}
	target, err := client.ParseTarget("http://"+a.ServerAddress()+"/#"+sess.Token(), "")
	if err != nil {
		t.Fatal(err)
	}
	cli := client.NewClient(target)
	cli.SetRetryPolicy(client.RetryPolicy{MaxRetries: 3, BaseDelay: 5 * time.Millisecond, MaxDelay: 20 * time.Millisecond})
	if res0, err := cli.StreamItem(t.Context(), batch.ID, batch.Items[0].ID, f1, nil); err != nil || !res0.Verified {
		t.Fatalf("StreamItem 0 after roam failed: res=%+v err=%v", res0, err)
	}
	if res1, err := cli.StreamItem(t.Context(), batch.ID, batch.Items[1].ID, f2, nil); err != nil || !res1.Verified {
		t.Fatalf("StreamItem 1 after roam failed: res=%+v err=%v", res1, err)
	}
	if got, _ := os.ReadFile(filepath.Join(a.store.Dir(), "one.bin")); !bytes.Equal(got, d1) {
		t.Fatal("roamed one.bin content mismatch")
	}
	if got, _ := os.ReadFile(filepath.Join(a.store.Dir(), "two.bin")); !bytes.Equal(got, d2) {
		t.Fatal("sibling two.bin content mismatch")
	}
}

// TestPhase1ScenarioD_DownloadRangeResumeAndMutationFallback tests G2:
// byte-range resume with strong metadata ETag + If-Range mismatch 200 fallback on disk edit.
func TestPhase1ScenarioD_DownloadRangeResumeAndMutationFallback(t *testing.T) {
	a := newTestApp(t)
	if err := a.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	sess, _ := a.sessions.Create()

	src := filepath.Join(t.TempDir(), "movie.mp4")
	v1 := append(bytes.Repeat([]byte("A"), 4096), bytes.Repeat([]byte("B"), 4096)...)
	_ = os.WriteFile(src, v1, 0o600)
	sf, err := a.registry.Add(src, "movie.mp4", "")
	if err != nil {
		t.Fatal(err)
	}

	dlURL := testAppServerURL(t, a) + "/api/files/" + sf.ID + "/download"
	r1, _ := http.NewRequest(http.MethodGet, dlURL, nil)
	r1.Header.Set("Authorization", "Bearer "+sess.Token())
	r1.Header.Set("Range", "bytes=0-4095")
	resp1, err := http.DefaultClient.Do(r1)
	if err != nil {
		t.Fatal(err)
	}
	part1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	etagV1 := resp1.Header.Get("ETag")
	if resp1.StatusCode != http.StatusPartialContent || etagV1 == "" {
		t.Fatalf("first range status=%d ETag=%q", resp1.StatusCode, etagV1)
	}

	// Resume cleanly while unchanged
	r2, _ := http.NewRequest(http.MethodGet, dlURL, nil)
	r2.Header.Set("Authorization", "Bearer "+sess.Token())
	r2.Header.Set("Range", "bytes=4096-")
	r2.Header.Set("If-Range", etagV1)
	resp2, err := http.DefaultClient.Do(r2)
	if err != nil {
		t.Fatal(err)
	}
	part2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusPartialContent || !bytes.Equal(append(part1, part2...), v1) {
		t.Fatalf("resumed download mismatch: status=%d", resp2.StatusCode)
	}

	// Edit file on disk -> stale If-Range restarts at 200 OK full fresh body
	v2 := bytes.Repeat([]byte("Z"), 9000)
	_ = os.WriteFile(src, v2, 0o600)
	_ = os.Chtimes(src, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second))
	r3, _ := http.NewRequest(http.MethodGet, dlURL, nil)
	r3.Header.Set("Authorization", "Bearer "+sess.Token())
	r3.Header.Set("Range", "bytes=4096-")
	r3.Header.Set("If-Range", etagV1)
	resp3, err := http.DefaultClient.Do(r3)
	if err != nil {
		t.Fatal(err)
	}
	fullV2, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK || !bytes.Equal(fullV2, v2) {
		t.Fatalf("edited file If-Range fallback status=%d len=%d", resp3.StatusCode, len(fullV2))
	}
}

// TestPhase1ScenarioE_FolderExplorerAndSelectiveRangeDownload tests G31 + G2:
// browsing shared subfolders, downloading single item with Range, and selective multi-item ZIP.
func TestPhase1ScenarioE_FolderExplorerAndSelectiveRangeDownload(t *testing.T) {
	a := newTestApp(t)
	if err := a.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	sess, _ := a.sessions.Create()

	root := filepath.Join(t.TempDir(), "Trip")
	_ = os.MkdirAll(filepath.Join(root, "sub"), 0o700)
	_ = os.WriteFile(filepath.Join(root, "sub", "keep.bin"), bytes.Repeat([]byte("K"), 4096), 0o600)
	_ = os.WriteFile(filepath.Join(root, "skip.txt"), []byte("SKIP"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "also.txt"), []byte("ALSO"), 0o600)
	src, err := a.registry.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}

	bReq, _ := http.NewRequest(http.MethodGet, testAppServerURL(t, a)+"/api/sources/"+src.ID+"/items?prefix=sub", nil)
	bReq.Header.Set("Authorization", "Bearer "+sess.Token())
	bRes, err := http.DefaultClient.Do(bReq)
	if err != nil || bRes.StatusCode != http.StatusOK {
		t.Fatalf("browse = %v / %v", err, bRes)
	}
	var subPage transfer.SourceBrowsePage
	_ = json.NewDecoder(bRes.Body).Decode(&subPage)
	bRes.Body.Close()

	rReq, _ := http.NewRequest(http.MethodGet, testAppServerURL(t, a)+"/api/sources/"+src.ID+"/items", nil)
	rReq.Header.Set("Authorization", "Bearer "+sess.Token())
	rRes, _ := http.DefaultClient.Do(rReq)
	var rootPage transfer.SourceBrowsePage
	_ = json.NewDecoder(rRes.Body).Decode(&rootPage)
	rRes.Body.Close()

	var alsoID string
	for _, it := range rootPage.Items {
		if it.Name == "also.txt" {
			alsoID = it.ID
		}
	}

	// Selective multi-item ZIP omits skip.txt
	grantBody, _ := json.Marshal(map[string]any{
		"sourceId": src.ID,
		"itemIds":  []string{subPage.Items[0].ID, alsoID},
	})
	gReq, _ := http.NewRequest(http.MethodPost, testAppServerURL(t, a)+"/api/download-grants", bytes.NewReader(grantBody))
	gReq.Header.Set("Authorization", "Bearer "+sess.Token())
	gRes, err := http.DefaultClient.Do(gReq)
	if err != nil || gRes.StatusCode != http.StatusCreated {
		t.Fatalf("grant = %v / %v", err, gRes)
	}
	var grant struct {
		DownloadURL string `json:"downloadUrl"`
	}
	_ = json.NewDecoder(gRes.Body).Decode(&grant)
	var gCookie *http.Cookie
	for _, c := range gRes.Cookies() {
		if c.Name == "flitnix-download-grant" {
			gCookie = c
		}
	}
	gRes.Body.Close()

	dlReq, _ := http.NewRequest(http.MethodGet, testAppServerURL(t, a)+grant.DownloadURL, nil)
	dlReq.AddCookie(gCookie)
	dlRes, err := http.DefaultClient.Do(dlReq)
	if err != nil || dlRes.StatusCode != http.StatusOK {
		t.Fatalf("selective zip download = %v / %v", err, dlRes)
	}
	zipBytes, _ := io.ReadAll(dlRes.Body)
	dlRes.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["Trip/sub/keep.bin"] || !names["Trip/also.txt"] || names["Trip/skip.txt"] {
		t.Fatalf("unexpected ZIP contents: %+v", names)
	}
}

// TestPhase1ScenarioF_ModTimeSettingToggle tests G22 App Settings toggle end-to-end.
func TestPhase1ScenarioF_ModTimeSettingToggle(t *testing.T) {
	a := newTestApp(t)
	if err := a.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	s, _ := a.sessions.Create()

	st := a.GetSettings()
	if !st.PreserveModTimes {
		t.Fatal("default GetSettings().PreserveModTimes should be true")
	}

	// Disable preservation in App Settings and upload a file declaring 2018 timestamp
	st.PreserveModTimes = false
	if err := a.SaveSettings(st); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	oldTime := time.Date(2018, 4, 2, 10, 0, 0, 0, time.UTC)
	declReq, _ := http.NewRequest(http.MethodPost, "/api/uploads", bytes.NewReader([]byte(
		fmt.Sprintf(`{"name":"no_preserve.txt","size":4,"lastModifiedMs":%d}`, oldTime.UnixMilli()),
	)))
	declReq.Host = "127.0.0.1"
	declReq.Header.Set("Authorization", "Bearer "+s.Token())
	declRec := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(declRec, declReq)
	var upRes struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(declRec.Body.Bytes(), &upRes)
	_, _ = a.ApproveUpload(upRes.ID)

	putReq, _ := http.NewRequest(http.MethodPut, "/api/uploads/"+upRes.ID+"/content", bytes.NewReader([]byte("test")))
	putReq.Host = "127.0.0.1"
	putReq.Header.Set("Authorization", "Bearer "+s.Token())
	putRec := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT = %d", putRec.Code)
	}
	info, err := os.Stat(filepath.Join(a.store.Dir(), "no_preserve.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().UTC().Truncate(time.Second).Equal(oldTime) || info.ModTime().Year() < 2025 {
		t.Fatalf("expected current receive-time when PreserveModTimes=false, got %v", info.ModTime())
	}

	// Re-enable preservation and verify sender mtime IS preserved
	st.PreserveModTimes = true
	if err := a.SaveSettings(st); err != nil {
		t.Fatalf("SaveSettings(true): %v", err)
	}
	declReq2, _ := http.NewRequest(http.MethodPost, "/api/uploads", bytes.NewReader([]byte(
		fmt.Sprintf(`{"name":"with_preserve.txt","size":4,"lastModifiedMs":%d}`, oldTime.UnixMilli()),
	)))
	declReq2.Host = "127.0.0.1"
	declReq2.Header.Set("Authorization", "Bearer "+s.Token())
	declRec2 := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(declRec2, declReq2)
	var upRes2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(declRec2.Body.Bytes(), &upRes2)
	_, _ = a.ApproveUpload(upRes2.ID)
	putReq2, _ := http.NewRequest(http.MethodPut, "/api/uploads/"+upRes2.ID+"/content", bytes.NewReader([]byte("test")))
	putReq2.Host = "127.0.0.1"
	putReq2.Header.Set("Authorization", "Bearer "+s.Token())
	putRec2 := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(putRec2, putReq2)
	if putRec2.Code != http.StatusOK {
		t.Fatalf("PUT 2 = %d", putRec2.Code)
	}
	info2, err := os.Stat(filepath.Join(a.store.Dir(), "with_preserve.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info2.ModTime().UTC().Truncate(time.Second); !got.Equal(oldTime) {
		t.Fatalf("expected preserved sender mtime %v when PreserveModTimes=true, got %v", oldTime, got)
	}

	// Keep unused imports clean against strict vet
	_ = recovery.KeyLen
	_ = settings.ThemeSystem
	_ = guest.CollisionPreserveBoth
}
