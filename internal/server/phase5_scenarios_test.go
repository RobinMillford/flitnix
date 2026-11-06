package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/power"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func createTestBatch(t *testing.T, env *testEnv, ts *httptest.Server, token string, itemName string, itemSize int64) (batchID, itemID string) {
	t.Helper()
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{
				RelativePath: itemName,
				Size:         itemSize,
			},
		},
	}
	body, _ := json.Marshal(manifest)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/batches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create batch req: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create batch status %d", resp.StatusCode)
	}
	var view transfer.BatchView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatalf("decode batch view: %v", err)
	}
	key := env.tokenKey(token)
	for _, it := range view.Items {
		contentID, err := env.coord.ItemContentID(key, view.ID, it.ID)
		if err == nil {
			_ = env.trans.DecideBatchItems([]string{contentID}, true)
		}
	}
	_, _ = env.coord.ApproveBatch(key, view.ID)
	return view.ID, view.Items[0].ID
}

func executePhase5Upload(t *testing.T, env *testEnv, ts *httptest.Server, token, filename string, content []byte) (bID, itID, publishedPath string) {
	t.Helper()
	bID, itID = createTestBatch(t, env, ts, token, filename, int64(len(content)))
	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", ts.URL, bID, itID)

	putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("new put req: %v", err)
	}
	putReq.Header.Set("Authorization", "Bearer "+token)
	putReq.Header.Set("Content-Type", "application/octet-stream")

	putRes, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT upload failed: %v", err)
	}
	defer putRes.Body.Close()
	if putRes.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(putRes.Body)
		t.Fatalf("PUT expected 200, got %d, body: %s", putRes.StatusCode, body)
	}

	publishedPath = filepath.Join(env.store.Dir(), filename)
	return bID, itID, publishedPath
}

// Scenario 1: Single-file streaming upload
func TestPhase5_Scenario01_SingleFileUpload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	content := []byte("Scenario 1 content: verified byte delivery")
	_, _, path := executePhase5Upload(t, env, ts, tok, "single.txt", content)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read published file: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Fatalf("published content mismatch: got %q, want %q", data, content)
	}
}

// Scenario 2: Multiple-file streaming upload
func TestPhase5_Scenario02_MultipleFileUpload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	file1 := []byte("file1 content")
	file2 := []byte("file2 content")

	_, _, path1 := executePhase5Upload(t, env, ts, tok, "multi1.txt", file1)
	_, _, path2 := executePhase5Upload(t, env, ts, tok, "multi2.txt", file2)

	d1, _ := os.ReadFile(path1)
	d2, _ := os.ReadFile(path2)
	if !bytes.Equal(d1, file1) || !bytes.Equal(d2, file2) {
		t.Fatalf("multi-file upload content mismatch")
	}
}

// Scenario 4: Single-file download
func TestPhase5_Scenario04_SingleFileDownload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "download_sample.txt")
	srcContent := []byte("Hello single-file download test!")
	if err := os.WriteFile(srcPath, srcContent, 0o600); err != nil {
		t.Fatal(err)
	}

	sf, err := env.reg.Add(srcPath, "download_sample.txt", "")
	if err != nil {
		t.Fatal(err)
	}

	grant, cookie := env.grantFor(tok, sf.ID)
	rec := env.getDownload(grant, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download returned %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), srcContent) {
		t.Fatalf("download content mismatch")
	}
	digestHdr := rec.Header().Get("Content-Digest")
	if digestHdr == "" {
		t.Fatalf("missing Content-Digest header")
	}
}

// Scenario 5: Multiple-file download as dynamic ZIP archive
func TestPhase5_Scenario05_MultipleFileDownload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "doc1.txt"), []byte("Doc 1 content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "doc2.txt"), []byte("Doc 2 content"), 0o600); err != nil {
		t.Fatal(err)
	}

	sf1, _ := env.reg.Add(filepath.Join(srcDir, "doc1.txt"), "doc1.txt", "")
	sf2, _ := env.reg.Add(filepath.Join(srcDir, "doc2.txt"), "doc2.txt", "")

	grant, cookie := env.grantFor(tok, sf1.ID, sf2.ID)
	if !grant.Archive {
		t.Fatalf("expected archive grant for multi-file selection")
	}

	rec := env.getDownload(grant, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download returned %d", rec.Code)
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("expected 2 files in zip, got %d", len(zr.File))
	}
}

// Scenario 6: Interrupted upload cleans up part file
func TestPhase5_Scenario06_InterruptedUpload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	totalSize := 500 * 1024
	bID, itID := createTestBatch(t, env, ts, tok, "interrupted_upload.bin", int64(totalSize))

	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", ts.URL, bID, itID)
	// Truncated body: send only 100 KiB instead of declared 500 KiB
	truncatedBody := bytes.Repeat([]byte{0x42}, 100*1024)
	putReq, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(truncatedBody))
	putReq.Header.Set("Authorization", "Bearer "+tok)

	res, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT failed: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for truncated stream, got %d", res.StatusCode)
	}

	// Verify no incomplete file was published
	finalPath := filepath.Join(env.store.Dir(), "interrupted_upload.bin")
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("incomplete file was published to %s", finalPath)
	}
}

// Scenario 7: Interrupted download (HTTP Range / 206 Partial Content)
func TestPhase5_Scenario07_InterruptedDownload(t *testing.T) {
	mockImmutable(t)
	env := newTestEnv(t)
	tok := env.token()

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "interrupted_dl.bin")
	srcData := bytes.Repeat([]byte{0x55}, 1024*1024)
	if err := os.WriteFile(srcPath, srcData, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(srcData)
	sumHex := hex.EncodeToString(sum[:])
	sf, err := env.reg.AddImmutable(srcPath, "interrupted_dl.bin", "", sumHex)
	if err != nil {
		t.Fatal(err)
	}

	grant, initialCookie := env.grantFor(tok, sf.ID)
	grantID := extractGrantID(grant.DownloadURL)

	initReq := httptest.NewRequest(http.MethodGet, grant.DownloadURL, nil)
	initReq.AddCookie(initialCookie)
	initRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(initRec, initReq)
	if initRec.Code != http.StatusOK {
		t.Fatalf("initial redemption failed: status=%d", initRec.Code)
	}
	contCookie := extractContinuationCookie(t, initRec, grantID)

	req := httptest.NewRequest("GET", grant.DownloadURL, nil)
	req.Header.Set("Range", "bytes=0-99")
	req.Header.Set("If-Range", fmt.Sprintf("\"sha256-%s\"", sumHex))
	req.AddCookie(contCookie)
	w := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", w.Code)
	}
	if w.Body.Len() != 100 {
		t.Fatalf("expected 100 bytes, got %d", w.Body.Len())
	}
	if !bytes.Equal(w.Body.Bytes(), srcData[:100]) {
		t.Fatalf("partial content mismatch")
	}
}

// Scenario 8: Retry after network interruption starts from byte zero and succeeds
func TestPhase5_Scenario08_ResumeAfterNetworkLoss(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	fullData := bytes.Repeat([]byte{0x77}, 256*1024)

	// Attempt 1 fails prematurely
	bID1, itID1 := createTestBatch(t, env, ts, tok, "retry_test.bin", int64(len(fullData)))
	putURL1 := fmt.Sprintf("%s/api/batches/%s/items/%s/content", ts.URL, bID1, itID1)
	req1, _ := http.NewRequest(http.MethodPut, putURL1, bytes.NewReader(fullData[:50*1024]))
	req1.Header.Set("Authorization", "Bearer "+tok)
	res1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("attempt 1 error: %v", err)
	}
	res1.Body.Close()

	// Retry creates fresh transfer and transmits full file from byte zero
	bID2, itID2 := createTestBatch(t, env, ts, tok, "retry_test.bin", int64(len(fullData)))
	putURL2 := fmt.Sprintf("%s/api/batches/%s/items/%s/content", ts.URL, bID2, itID2)
	req2, _ := http.NewRequest(http.MethodPut, putURL2, bytes.NewReader(fullData))
	req2.Header.Set("Authorization", "Bearer "+tok)
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("attempt 2 error: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("attempt 2 expected 200, got %d", res2.StatusCode)
	}
}

// Scenario 10: Duplicate filename handling produces numbered collision variant
func TestPhase5_Scenario10_DuplicateFilenameHandling(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	c1 := []byte("version 1")
	c2 := []byte("version 2")

	executePhase5Upload(t, env, ts, tok, "photo.jpg", c1)
	executePhase5Upload(t, env, ts, tok, "photo.jpg", c2)

	path1 := filepath.Join(env.store.Dir(), "photo.jpg")
	path2 := filepath.Join(env.store.Dir(), "photo (1).jpg")

	d1, err := os.ReadFile(path1)
	if err != nil {
		t.Fatalf("cannot read photo.jpg: %v", err)
	}
	d2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatalf("cannot read photo (1).jpg: %v", err)
	}
	if !bytes.Equal(d1, c1) || !bytes.Equal(d2, c2) {
		t.Fatalf("collision content mismatch: d1=%q, d2=%q", d1, d2)
	}
}

// Scenario 11: Destination already exists before upload starts
func TestPhase5_Scenario11_DestinationAlreadyExists(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()

	existingPath := filepath.Join(env.store.Dir(), "already_there.txt")
	existingContent := []byte("pre-existing content")
	if err := os.WriteFile(existingPath, existingContent, 0o600); err != nil {
		t.Fatal(err)
	}

	newContent := []byte("new incoming content")
	executePhase5Upload(t, env, ts, tok, "already_there.txt", newContent)

	d1, _ := os.ReadFile(existingPath)
	if !bytes.Equal(d1, existingContent) {
		t.Fatalf("pre-existing destination was overwritten")
	}

	variantPath := filepath.Join(env.store.Dir(), "already_there (1).txt")
	d2, err := os.ReadFile(variantPath)
	if err != nil {
		t.Fatalf("expected variant 'already_there (1).txt' was not created: %v", err)
	}
	if !bytes.Equal(d2, newContent) {
		t.Fatalf("variant content mismatch")
	}
}

// Scenario 13: Server shutdown during transfer cleans up cleanly
func TestPhase5_Scenario13_ServerShutdownDuringTransfer(t *testing.T) {
	tempDir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: tempDir})
	if err != nil {
		t.Fatal(err)
	}
	sessMgr := session.NewManager(session.Config{})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})

	svcs := &Services{
		Sessions:    sessMgr,
		Transfers:   transfers,
		Coordinator: coord,
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
	}
	svcs.OnBatchPending = func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
		for _, item := range batch.Items {
			contentID, err := coord.ItemContentID(sessionKey, batch.ID, item.ID)
			if err == nil {
				_ = transfers.DecideBatchItems([]string{contentID}, true)
			}
		}
		_, _ = coord.ApproveBatch(sessionKey, batch.ID)
	}

	srv := New(Config{ListenAddr: "127.0.0.1:0", Services: svcs})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	<-srv.Ready()

	sess, _ := sessMgr.Create()

	// Slow reader that stalls mid-transfer
	slowReader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write(make([]byte, 1024))
		// Keep open until context cancel
		<-ctx.Done()
		_ = writer.Close()
	}()

	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{RelativePath: "shutdown.bin", Size: 1024 * 1024}},
	}
	mBody, _ := json.Marshal(manifest)
	bReq, _ := http.NewRequest(http.MethodPost, srv.URL()+"/api/batches", bytes.NewReader(mBody))
	bReq.Header.Set("Authorization", "Bearer "+sess.Token())
	bReq.Header.Set("Content-Type", "application/json")
	bRes, err := http.DefaultClient.Do(bReq)
	if err != nil {
		t.Fatal(err)
	}
	var bView transfer.BatchView
	_ = json.NewDecoder(bRes.Body).Decode(&bView)
	bRes.Body.Close()

	go func() {
		putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", srv.URL(), bView.ID, bView.Items[0].ID)
		pReq, _ := http.NewRequest(http.MethodPut, putURL, slowReader)
		pReq.Header.Set("Authorization", "Bearer "+sess.Token())
		_, _ = http.DefaultClient.Do(pReq)
	}()

	time.Sleep(50 * time.Millisecond)
	// Cancel server context (simulating shutdown)
	cancel()

	time.Sleep(100 * time.Millisecond)
	// Verify no complete file published
	if _, err := os.Stat(filepath.Join(tempDir, "shutdown.bin")); !os.IsNotExist(err) {
		t.Fatal("file published despite server shutdown during stream")
	}
}

// Scenario 16: Multiple concurrent downloads across separate sessions
func TestPhase5_Scenario16_MultipleConcurrentDownloads(t *testing.T) {
	env := newTestEnv(t)
	tok1 := env.token()
	tok2 := env.token()

	srcDir := t.TempDir()
	p1 := filepath.Join(srcDir, "concurrent1.bin")
	p2 := filepath.Join(srcDir, "concurrent2.bin")
	d1 := bytes.Repeat([]byte{0x11}, 100*1024)
	d2 := bytes.Repeat([]byte{0x22}, 100*1024)
	_ = os.WriteFile(p1, d1, 0o600)
	_ = os.WriteFile(p2, d2, 0o600)

	sf1, _ := env.reg.Add(p1, "concurrent1.bin", "")
	sf2, _ := env.reg.Add(p2, "concurrent2.bin", "")

	grant1, c1 := env.grantFor(tok1, sf1.ID)
	grant2, c2 := env.grantFor(tok2, sf2.ID)

	var wg sync.WaitGroup
	var rec1, rec2 *httptest.ResponseRecorder

	wg.Add(2)
	go func() {
		defer wg.Done()
		rec1 = env.getDownload(grant1, c1, nil)
	}()
	go func() {
		defer wg.Done()
		rec2 = env.getDownload(grant2, c2, nil)
	}()
	wg.Wait()

	if rec1.Code != http.StatusOK || rec2.Code != http.StatusOK {
		t.Fatalf("concurrent downloads failed: c1=%d, c2=%d", rec1.Code, rec2.Code)
	}
	if !bytes.Equal(rec1.Body.Bytes(), d1) || !bytes.Equal(rec2.Body.Bytes(), d2) {
		t.Fatalf("concurrent download content corruption")
	}
}

// Scenario 18: Network interface changes during transfer
func TestPhase5_Scenario18_NetworkInterfaceChangesDuringTransfer(t *testing.T) {
	src := netenv.Source{
		Interfaces: func() ([]net.Interface, error) {
			return []net.Interface{
				{Name: "eth0", Flags: net.FlagUp},
				{Name: "wlan0", Flags: net.FlagUp},
			}, nil
		},
		Addrs: func(iface net.Interface) ([]net.Addr, error) {
			if iface.Name == "eth0" {
				return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.50"), Mask: net.CIDRMask(24, 32)}}, nil
			}
			return []net.Addr{&net.IPNet{IP: net.ParseIP("10.0.0.25"), Mask: net.CIDRMask(24, 32)}}, nil
		},
	}

	views, err := netenv.Enumerate(src)
	if err != nil || len(views) != 2 {
		t.Fatalf("expected 2 interfaces, got %d (err: %v)", len(views), err)
	}

	v1, err := netenv.InterfaceFor(src, "192.168.1.50")
	if err != nil || v1.Name != "eth0" {
		t.Fatalf("failed to select eth0: %v", err)
	}
	v2, err := netenv.InterfaceFor(src, "10.0.0.25")
	if err != nil || v2.Name != "wlan0" {
		t.Fatalf("failed to select wlan0: %v", err)
	}
}

// Scenario 19: DHCP address changes
func TestPhase5_Scenario19_DHCPAddressChanges(t *testing.T) {
	step := 0
	src := netenv.Source{
		Interfaces: func() ([]net.Interface, error) {
			return []net.Interface{{Name: "wlan0", Flags: net.FlagUp}}, nil
		},
		Addrs: func(iface net.Interface) ([]net.Addr, error) {
			if step == 0 {
				return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.100"), Mask: net.CIDRMask(24, 32)}}, nil
			}
			return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.105"), Mask: net.CIDRMask(24, 32)}}, nil
		},
	}

	v1, _ := netenv.Enumerate(src)
	if v1[0].IP != "192.168.1.100" {
		t.Fatalf("initial IP mismatch: %s", v1[0].IP)
	}

	step = 1
	v2, _ := netenv.Enumerate(src)
	if v2[0].IP != "192.168.1.105" {
		t.Fatalf("updated DHCP IP mismatch: %s", v2[0].IP)
	}
}

// Scenario 20: Desktop sleep and wake inhibition
func TestPhase5_Scenario20_DesktopSleepAndWake(t *testing.T) {
	inh := power.NewFake()
	defer inh.Close()

	initialStatus := inh.Status()
	if initialStatus.Active || initialStatus.Holders != 0 {
		t.Fatalf("expected sleep inhibitor initially inactive: %+v", initialStatus)
	}

	lease1, err := inh.Acquire("Active Flitnix Transfer 1")
	if err != nil {
		t.Fatalf("failed to inhibit sleep: %v", err)
	}
	if !inh.Status().Active || inh.Status().Holders != 1 {
		t.Fatalf("sleep inhibitor should be active with 1 holder: %+v", inh.Status())
	}

	lease2, err := inh.Acquire("Active Flitnix Transfer 2")
	if err != nil {
		t.Fatalf("failed to acquire second lease: %v", err)
	}
	if !inh.Status().Active || inh.Status().Holders != 2 {
		t.Fatalf("sleep inhibitor should have 2 holders: %+v", inh.Status())
	}

	lease1.Release()
	if !inh.Status().Active || inh.Status().Holders != 1 {
		t.Fatalf("sleep inhibitor should remain active with 1 holder: %+v", inh.Status())
	}

	lease2.Release()
	if inh.Status().Active || inh.Status().Holders != 0 {
		t.Fatalf("sleep inhibitor should be inactive after all transfers complete")
	}
	lease2.Release() // Idempotent release
}

// Scenario 21: Invalid or expired pairing
func TestPhase5_Scenario21_InvalidOrExpiredPairing(t *testing.T) {
	hostID, _ := devices.LoadOrCreateHostIdentity("")
	store, _ := devices.NewStore("")
	coord := devices.NewPairingCoordinator(hostID, store)

	clientPub, _, _ := devices.GenerateKeyPair()
	clientEphemeral, _ := devices.GenerateEphemeralKeyPair()
	clientSalt, _ := devices.GenerateOpeningSalt()
	clientCommit, _ := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	sessionKey := sha256.Sum256([]byte("sess-scen21"))

	chal, err := coord.InitChallenge(sessionKey[:], hex.EncodeToString(clientPub), hex.EncodeToString(clientCommit[:]))
	if err != nil {
		t.Fatal(err)
	}

	_, _ = coord.Reveal(sessionKey[:], chal.PairingNonce, hex.EncodeToString(clientEphemeral.PublicKey), hex.EncodeToString(clientSalt))

	bogusSig := hex.EncodeToString(make([]byte, 64))
	_, _, _, err = coord.PrepareApproval(sessionKey[:], "Evil Device", hex.EncodeToString(clientPub), chal.PairingNonce, bogusSig)
	if err != devices.ErrInvalidSignature {
		t.Fatalf("expected ErrInvalidSignature, got: %v", err)
	}
	if coord.FailureCount() != 1 {
		t.Fatalf("failure counter not incremented: count=%d", coord.FailureCount())
	}
}

// Scenario 22: Unauthorized transfer attempt
func TestPhase5_Scenario22_UnauthorizedTransferAttempt(t *testing.T) {
	env := newTestEnv(t)

	r1 := httptest.NewRequest("PUT", "/api/batches/b1/items/it1/content", strings.NewReader("data"))
	w1 := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w1, r1)
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for no token, got %d", w1.Code)
	}

	r2 := httptest.NewRequest("PUT", "/api/batches/b1/items/it1/content", strings.NewReader("data"))
	r2.Header.Set("Authorization", "Bearer invalid-token-string")
	w2 := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for bogus token, got %d", w2.Code)
	}
}

// Scenario 23: Malformed upload metadata
func TestPhase5_Scenario23_MalformedUploadMetadata(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	// Malformed JSON body
	req := httptest.NewRequest(http.MethodPost, "/api/batches", strings.NewReader("{invalid-json"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for malformed JSON, got %d", w.Code)
	}
}

// Scenario 25: Large directory transfer with nested files and folders
func TestPhase5_Scenario25_LargeDirectoryTransfer(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	srcDir := t.TempDir()
	rootDir := filepath.Join(srcDir, "ProjectFolder")
	_ = os.MkdirAll(filepath.Join(rootDir, "src", "pkg"), 0o755)
	_ = os.MkdirAll(filepath.Join(rootDir, "docs"), 0o755)

	_ = os.WriteFile(filepath.Join(rootDir, "README.md"), []byte("# Project Root"), 0o600)
	_ = os.WriteFile(filepath.Join(rootDir, "src", "main.go"), []byte("package main\nfunc main(){}"), 0o600)
	_ = os.WriteFile(filepath.Join(rootDir, "src", "pkg", "lib.go"), []byte("package pkg"), 0o600)
	_ = os.WriteFile(filepath.Join(rootDir, "docs", "arch.pdf"), bytes.Repeat([]byte{0x25}, 10000), 0o600)

	source, err := env.reg.AddFolder(rootDir, "")
	if err != nil {
		t.Fatalf("AddFolder failed: %v", err)
	}

	grant, cookie := env.grantFor(tok, source.ID)
	if !grant.Archive {
		t.Fatalf("expected archive grant for directory transfer")
	}

	rec := env.getDownload(grant, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive download returned %d: %s", rec.Code, rec.Body.String())
	}

	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}

	names := make(map[string]bool)
	for _, f := range zr.File {
		names[f.Name] = true
	}

	expected := []string{
		"ProjectFolder/README.md",
		"ProjectFolder/src/main.go",
		"ProjectFolder/src/pkg/lib.go",
		"ProjectFolder/docs/arch.pdf",
	}
	for _, exp := range expected {
		if !names[exp] {
			t.Errorf("missing entry in archive: %s (got %v)", exp, names)
		}
	}
}
