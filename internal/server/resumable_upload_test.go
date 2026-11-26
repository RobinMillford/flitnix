package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func uploadResumableHTTP(t *testing.T, url, token string, data []byte) (int64, string) {
	t.Helper()
	hReq, _ := http.NewRequest(http.MethodHead, url, nil)
	hReq.Header.Set("Authorization", "Bearer "+token)
	hRes, err := http.DefaultClient.Do(hReq)
	if err != nil {
		t.Fatal(err)
	}
	hRes.Body.Close()
	if hRes.StatusCode != http.StatusOK {
		t.Fatalf("HEAD %s status = %d", url, hRes.StatusCode)
	}
	offset, err := strconv.ParseInt(hRes.Header.Get("Upload-Offset"), 10, 64)
	if err != nil {
		t.Fatalf("HEAD Upload-Offset = %q: %v", hRes.Header.Get("Upload-Offset"), err)
	}
	initialOffset := offset
	chunkSize := transfer.DefaultChunkSize
	if cs, cerr := strconv.ParseInt(hRes.Header.Get("Upload-Chunk-Size"), 10, 64); cerr == nil && cs > 0 {
		chunkSize = cs
	}

	var result uploadResultJSON
	total := int64(len(data))
	for offset < total || (total == 0 && offset == 0) {
		end := offset + chunkSize
		if end > total {
			end = total
		}
		chunk := data[offset:end]
		req, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(chunk))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		req.ContentLength = int64(len(chunk))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PATCH %s at %d: %v", url, offset, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			nextOff, _ := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
			offset = nextOff
			continue
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH %s at %d status=%d body=%s", url, offset, resp.StatusCode, string(body))
		}
		_ = json.Unmarshal(body, &result)
		return initialOffset, result.SHA256
	}
	return initialOffset, result.SHA256
}

func synthPattern(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = seed ^ byte(i*149+11)
	}
	return out
}

func sha256HexBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestResumableBatchUploadMidChunkTCPRstAndResume(t *testing.T) {
	env := newTestEnv(t)
	srv, _, stop := startRealServer(t, Config{
		Services: &Services{
			Sessions:    env.sess,
			Transfers:   env.trans,
			Coordinator: env.coord,
			Registry:    env.reg,
			Downloads:   env.dls,
			Diagnose:    env.diagSink,
		},
	})
	_ = stop

	tok := env.token()
	key := env.tokenKey(tok)

	// 10 MiB item 0 + 64-byte sibling item 1.
	payload0 := synthPattern(10*1024*1024+123, 0x51)
	payload1 := []byte("sibling-file-must-survive-transient-interruption-0123456789abcde")
	b := env.filesBatch(tok, int64(len(payload0)), int64(len(payload1)))
	env.approveBatch(key, b)

	item0URL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", srv.URL(), b.ID, b.Items[0].ID)

	// 1. HEAD initial offset == 0
	hReq, _ := http.NewRequest(http.MethodHead, item0URL, nil)
	hReq.Header.Set("Authorization", "Bearer "+tok)
	hRes, err := http.DefaultClient.Do(hReq)
	if err != nil {
		t.Fatal(err)
	}
	hRes.Body.Close()
	if hRes.StatusCode != http.StatusOK || hRes.Header.Get("Upload-Offset") != "0" {
		t.Fatalf("initial HEAD = %d offset=%s", hRes.StatusCode, hRes.Header.Get("Upload-Offset"))
	}

	// 2. Send chunk 0 (4 MiB) cleanly -> 204 No Content with Upload-Offset: 4194304.
	chunkLen := int64(4 << 20)
	p1Req, _ := http.NewRequest(http.MethodPatch, item0URL, bytes.NewReader(payload0[:chunkLen]))
	p1Req.Header.Set("Authorization", "Bearer "+tok)
	p1Req.Header.Set("Content-Type", "application/offset+octet-stream")
	p1Req.Header.Set("Upload-Offset", "0")
	p1Req.ContentLength = chunkLen
	p1Res, err := http.DefaultClient.Do(p1Req)
	if err != nil {
		t.Fatal(err)
	}
	p1Res.Body.Close()
	if p1Res.StatusCode != http.StatusNoContent || p1Res.Header.Get("Upload-Offset") != strconv.FormatInt(chunkLen, 10) {
		t.Fatalf("chunk 1 PATCH = %d offset=%s", p1Res.StatusCode, p1Res.Header.Get("Upload-Offset"))
	}

	// 3. Open raw TCP connection for chunk 2, send 512 KiB of 4 MiB, then sever TCP mid-chunk!
	host := strings.TrimPrefix(srv.URL(), "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "PATCH /api/batches/%s/items/%s/content HTTP/1.1\r\n", b.ID, b.Items[0].ID)
	fmt.Fprintf(&raw, "Host: %s\r\n", host)
	fmt.Fprintf(&raw, "Authorization: Bearer %s\r\n", tok)
	fmt.Fprintf(&raw, "Content-Type: application/offset+octet-stream\r\n")
	fmt.Fprintf(&raw, "Upload-Offset: %d\r\n", chunkLen)
	fmt.Fprintf(&raw, "Content-Length: %d\r\n", chunkLen)
	fmt.Fprintf(&raw, "Connection: close\r\n\r\n")
	if _, err := conn.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(payload0[chunkLen : chunkLen+512*1024]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	_ = conn.Close()

	// Wait until coordinator marks item 0 interrupted at exact chunkLen (not canceled, sibling item 1 untouched!).
	waitFor(t, "item 0 interrupted", func() bool {
		v, err := env.coord.GetBatch(key, b.ID)
		return err == nil && v.State == transfer.BatchStateInterrupted && v.Items[0].State == transfer.ItemStateInterrupted
	})
	v, _ := env.coord.GetBatch(key, b.ID)
	if v.Items[0].Transferred != chunkLen {
		t.Fatalf("item 0 Transferred = %d, want %d", v.Items[0].Transferred, chunkLen)
	}
	if v.Items[1].State != transfer.ItemStateQueued {
		t.Fatalf("sibling item 1 state = %q, want Queued (must NOT be canceled on transient interruption)", v.Items[1].State)
	}

	// 4. HEAD confirms authoritative offset == chunkLen; resume using Go resumable client!
	hReq2, _ := http.NewRequest(http.MethodHead, item0URL, nil)
	hReq2.Header.Set("Authorization", "Bearer "+tok)
	hRes2, err := http.DefaultClient.Do(hReq2)
	if err != nil {
		t.Fatal(err)
	}
	hRes2.Body.Close()
	if hRes2.Header.Get("Upload-Offset") != strconv.FormatInt(chunkLen, 10) {
		t.Fatalf("HEAD after drop = %s, want %d", hRes2.Header.Get("Upload-Offset"), chunkLen)
	}

	// Wrong Upload-Offset produces 409 with authoritative Upload-Offset.
	badReq, _ := http.NewRequest(http.MethodPatch, item0URL, bytes.NewReader([]byte("x")))
	badReq.Header.Set("Authorization", "Bearer "+tok)
	badReq.Header.Set("Upload-Offset", "0")
	badReq.ContentLength = 1
	badRes, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	badRes.Body.Close()
	if badRes.StatusCode != http.StatusConflict || badRes.Header.Get("Upload-Offset") != strconv.FormatInt(chunkLen, 10) {
		t.Fatalf("mismatch PATCH = %d Upload-Offset=%s", badRes.StatusCode, badRes.Header.Get("Upload-Offset"))
	}

	// Resume rest of item 0 & item 1 cleanly from chunkLen!
	item1URL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", srv.URL(), b.ID, b.Items[1].ID)
	startOff0, gotSHA0 := uploadResumableHTTP(t, item0URL, tok, payload0)
	if startOff0 != chunkLen || gotSHA0 != sha256HexBytes(payload0) {
		t.Fatalf("resumed item 0 startOff=%d (want %d), sha=%s (want %s)", startOff0, chunkLen, gotSHA0, sha256HexBytes(payload0))
	}
	startOff1, gotSHA1 := uploadResumableHTTP(t, item1URL, tok, payload1)
	if startOff1 != 0 || gotSHA1 != sha256HexBytes(payload1) {
		t.Fatalf("sibling item 1 startOff=%d, sha=%s", startOff1, gotSHA1)
	}

	finalBatch, _ := env.coord.GetBatch(key, b.ID)
	if finalBatch.State != transfer.BatchStateCompleted {
		t.Fatalf("final batch state = %q, want completed", finalBatch.State)
	}
	if left := partFiles(t, env.store.Dir()); len(left) != 0 {
		t.Fatalf("staging files left after completion: %v", left)
	}
}

func TestStandaloneUploadHeadAndPatchResumption(t *testing.T) {
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	defer ts.Close()
	tok := env.token()

	payload := synthPattern(1500*1024+99, 0x29)
	meta, _ := json.Marshal(map[string]any{"name": "solo.bin", "size": len(payload)})
	dec := env.do(http.MethodPost, "/api/uploads", tok, meta)
	if dec.Code != http.StatusCreated {
		t.Fatalf("declare standalone: %d %s", dec.Code, dec.Body.String())
	}
	var up transfer.Upload
	_ = json.Unmarshal(dec.Body.Bytes(), &up)
	if _, err := env.trans.Decide(up.ID, true); err != nil {
		t.Fatal(err)
	}

	url := ts.URL + "/api/uploads/" + up.ID + "/content"
	split := 700 * 1024

	p1, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(payload[:split]))
	p1.Header.Set("Authorization", "Bearer "+tok)
	p1.Header.Set("Upload-Offset", "0")
	p1.ContentLength = int64(split)
	r1, err := http.DefaultClient.Do(p1)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusNoContent {
		t.Fatalf("standalone PATCH 1 = %d", r1.StatusCode)
	}

	// Cross-session HEAD must return 404.
	otherTok := env.token()
	hx, _ := http.NewRequest(http.MethodHead, url, nil)
	hx.Header.Set("Authorization", "Bearer "+otherTok)
	rx, _ := http.DefaultClient.Do(hx)
	rx.Body.Close()
	if rx.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-session HEAD = %d, want 404", rx.StatusCode)
	}

	// Owner HEAD must report split offset.
	h1, _ := http.NewRequest(http.MethodHead, url, nil)
	h1.Header.Set("Authorization", "Bearer "+tok)
	hr1, _ := http.DefaultClient.Do(h1)
	hr1.Body.Close()
	if hr1.Header.Get("Upload-Offset") != strconv.Itoa(split) {
		t.Fatalf("HEAD offset = %s, want %d", hr1.Header.Get("Upload-Offset"), split)
	}

	// Final PATCH completes and matches SHA-256.
	rem := int64(len(payload) - split)
	p2, _ := http.NewRequest(http.MethodPatch, url, bytes.NewReader(payload[split:]))
	p2.Header.Set("Authorization", "Bearer "+tok)
	p2.Header.Set("Upload-Offset", strconv.Itoa(split))
	p2.ContentLength = rem
	r2, err := http.DefaultClient.Do(p2)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("standalone PATCH 2 = %d", r2.StatusCode)
	}
	var out uploadResultJSON
	_ = json.NewDecoder(r2.Body).Decode(&out)
	if out.SHA256 != sha256HexBytes(payload) || out.Written != int64(len(payload)) {
		t.Fatalf("standalone result mismatch: %+v", out)
	}
}

const (
	g1CrashModeEnv    = "FLITNIX_G1_CRASH_MODE"
	g1CrashDirEnv     = "FLITNIX_G1_CRASH_DIR"
	g1CrashReceiveEnv = "FLITNIX_G1_RECEIVE_DIR"
)

func TestG1CrashHelperProcess(t *testing.T) {
	mode := os.Getenv(g1CrashModeEnv)
	if mode == "" {
		return
	}
	stateDir := os.Getenv(g1CrashDirEnv)
	recDir := os.Getenv(g1CrashReceiveEnv)
	if recDir == "" {
		recDir = stateDir
	}

	store, err := transfer.NewStore(transfer.StoreConfig{Dir: recDir})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	quota := transfer.NewQuotaManager(transfer.QuotaConfig{DefaultDir: store.Dir(), SafetyMargin: 1})
	sessions := session.NewManager(session.Config{IdleTimeout: 10 * time.Minute})
	transfers := transfer.NewService(transfer.Config{Store: store, Quota: quota})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	services := &Services{
		Sessions: sessions, Transfers: transfers, Coordinator: coord,
		Registry: transfer.NewRegistry(), Downloads: transfer.NewDownloadRegistry(),
	}
	services.OnBatchPending = func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
		var cids []string
		for _, item := range batch.Items {
			cid, err := coord.ItemContentID(sessionKey, batch.ID, item.ID)
			if err == nil && cid != "" {
				cids = append(cids, cid)
			}
		}
		if len(cids) > 0 {
			_ = transfers.DecideBatchItems(cids, true)
		}
		_, _ = coord.ApproveBatch(sessionKey, batch.ID)
	}

	srv := New(Config{ListenAddr: "127.0.0.1:0", RecoveryDir: stateDir, Services: services})
	ctx := t.Context()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ctx) }()

	select {
	case err := <-errc:
		fmt.Fprintln(os.Stderr, "serve failed:", err)
		os.Exit(3)
	case <-srv.Ready():
	}

	var token string
	if mode == "before-crash" {
		s, err := sessions.Create()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		token = s.Token()
	}

	info, _ := json.Marshal(struct {
		URL   string `json:"url"`
		Token string `json:"token,omitempty"`
	}{URL: srv.URL(), Token: token})
	fmt.Println(string(info))

	select {
	case err := <-errc:
		if err != nil {
			os.Exit(5)
		}
	}
}

func startG1Child(t *testing.T, mode, stateDir, recDir string) (url, token string, kill func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestG1CrashHelperProcess$")
	cmd.Env = append(os.Environ(),
		g1CrashModeEnv+"="+mode,
		g1CrashDirEnv+"="+stateDir,
		g1CrashReceiveEnv+"="+recDir,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lineCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			txt := sc.Text()
			if strings.HasPrefix(txt, "{") {
				lineCh <- txt
				return
			}
		}
		close(lineCh)
	}()
	select {
	case line, ok := <-lineCh:
		if !ok {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("child process did not emit ready JSON")
		}
		var ready struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(line), &ready); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("invalid ready JSON %q: %v", line, err)
		}
		return ready.URL, ready.Token, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("timeout waiting for child server")
		return "", "", nil
	}
}

func TestG1SubprocessSIGKILLCrashAndOffsetRecovery(t *testing.T) {
	stateDir := t.TempDir()
	recDir := t.TempDir()

	payload := synthPattern(5*1024*1024+4096, 0x6B)
	chunk1 := int64(2 << 20)

	// 1. Start Child A, declare approved batch, upload chunk 1 (2 MiB), write torn tail into .part, then SIGKILL!
	urlA, tok, killA := startG1Child(t, "before-crash", stateDir, recDir)
	defer killA()

	batchBody, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "crash-resume.bin", Size: int64(len(payload)), SHA256: sha256HexBytes(payload)},
		},
	})
	decReq, _ := http.NewRequest(http.MethodPost, urlA+"/api/batches", bytes.NewReader(batchBody))
	decReq.Header.Set("Authorization", "Bearer "+tok)
	decReq.Header.Set("Content-Type", "application/json")
	decRes, err := http.DefaultClient.Do(decReq)
	if err != nil {
		t.Fatal(err)
	}
	if decRes.StatusCode != http.StatusCreated {
		decRes.Body.Close()
		t.Fatalf("declare batch status = %d", decRes.StatusCode)
	}
	var bView transfer.BatchView
	_ = json.NewDecoder(decRes.Body).Decode(&bView)
	decRes.Body.Close()

	patchURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", urlA, bView.ID, bView.Items[0].ID)
	req, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(payload[:chunk1]))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Upload-Offset", "0")
	req.ContentLength = chunk1
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("chunk 1 status = %d", res.StatusCode)
	}

	// Simulate an uncommitted torn write inside the .part file right before SIGKILL!
	parts := partFiles(t, recDir)
	var dataPart string
	for _, p := range parts {
		if !strings.HasSuffix(p, transfer.MetaSuffix) {
			dataPart = p
			break
		}
	}
	if dataPart == "" {
		t.Fatal("expected staged .part file on disk")
	}
	f, err := os.OpenFile(dataPart, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("UNCOMMITTED_TORN_TAIL_GARBAGE"))
	_ = f.Close()

	// Kill Server A abruptly via SIGKILL!
	killA()

	// 2. Restart Child B with the same stateDir and recDir -> Phases 1-10 run!
	urlB, _, killB := startG1Child(t, "after-crash", stateDir, recDir)
	defer killB()

	// Torn tail must already have been truncated back to chunk1 by Phases 6-9.
	info, err := os.Stat(dataPart)
	if err != nil || info.Size() != chunk1 {
		t.Fatalf("after recovery, part size = %v (err=%v), want %d", info.Size(), err, chunk1)
	}

	// Probe HEAD with original token and pre-crash batch/item IDs -> must report chunk1!
	headURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", urlB, bView.ID, bView.Items[0].ID)
	hReq, _ := http.NewRequest(http.MethodHead, headURL, nil)
	hReq.Header.Set("Authorization", "Bearer "+tok)
	hRes, err := http.DefaultClient.Do(hReq)
	if err != nil {
		t.Fatal(err)
	}
	hRes.Body.Close()
	if hRes.StatusCode != http.StatusOK || hRes.Header.Get("Upload-Offset") != strconv.FormatInt(chunk1, 10) {
		t.Fatalf("post-crash HEAD = %d offset=%s, want 200 & %d", hRes.StatusCode, hRes.Header.Get("Upload-Offset"), chunk1)
	}

	// Stream ONLY the remaining bytes from chunk1!
	resumedFrom, gotSHA := uploadResumableHTTP(t, headURL, tok, payload)
	if resumedFrom != chunk1 {
		t.Fatalf("expected upload to resume from offset %d, started from %d", chunk1, resumedFrom)
	}
	if gotSHA != sha256HexBytes(payload) {
		t.Fatalf("post-crash SHA256 mismatch: got %s, want %s", gotSHA, sha256HexBytes(payload))
	}

	finalData, err := os.ReadFile(filepath.Join(recDir, "crash-resume.bin"))
	if err != nil || !bytes.Equal(finalData, payload) {
		t.Fatalf("final published file mismatch (err=%v)", err)
	}
}

func TestRecoveryQuarantinesTruncatedUploadWithoutBlockingSiblings(t *testing.T) {
	stateDir := t.TempDir()
	recDir := t.TempDir()

	// Phase A: create session, 2-item batch, stage 1 chunk for both items.
	storeA, _ := transfer.NewStore(transfer.StoreConfig{Dir: recDir})
	quotaA := transfer.NewQuotaManager(transfer.QuotaConfig{DefaultDir: recDir, SafetyMargin: 1})
	sessA := session.NewManager(session.Config{IdleTimeout: 10 * time.Minute})
	transA := transfer.NewService(transfer.Config{Store: storeA, Quota: quotaA})
	coordA := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	svcsA := &Services{Sessions: sessA, Transfers: transA, Coordinator: coordA, Registry: transfer.NewRegistry(), Downloads: transfer.NewDownloadRegistry()}
	srvA := New(Config{ListenAddr: "127.0.0.1:0", RecoveryDir: stateDir, Services: svcsA})
	if err := srvA.Recover(); err != nil {
		t.Fatal(err)
	}

	s, _ := sessA.Create()
	p0 := synthPattern(1024*1024, 0x11)
	p1 := synthPattern(1024*1024, 0x22)
	reqBody, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "bad.bin", Size: int64(len(p0))},
			{DisplayName: "good.bin", Size: int64(len(p1))},
		},
	})
	rec := httptest.NewRecorder()
	r, _ := http.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(reqBody))
	r.Host = "127.0.0.1"
	r.Header.Set("Authorization", "Bearer "+s.Token())
	srvA.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("batch create: %d %s", rec.Code, rec.Body.String())
	}
	var batch transfer.BatchView
	_ = json.Unmarshal(rec.Body.Bytes(), &batch)

	cids := make([]string, 2)
	for i, it := range batch.Items {
		cids[i], _ = coordA.ItemContentID(s.Key(), batch.ID, it.ID)
	}
	_ = transA.DecideBatchItems(cids, true)
	_, _ = coordA.ApproveBatch(s.Key(), batch.ID)

	// Upload half of item 0; pause/interrupt item 0; NO, let's complete item 0 half, then corrupt bad.bin's .part!
	patch0, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("/api/batches/%s/items/%s/content", batch.ID, batch.Items[0].ID), bytes.NewReader(p0[:512*1024]))
	patch0.Host = "127.0.0.1"
	patch0.Header.Set("Authorization", "Bearer "+s.Token())
	patch0.Header.Set("Upload-Offset", "0")
	patch0.ContentLength = 512 * 1024
	r0 := httptest.NewRecorder()
	srvA.Handler().ServeHTTP(r0, patch0)
	if r0.Code != http.StatusNoContent {
		t.Fatalf("patch0: %d", r0.Code)
	}
	_ = srvA.CloseRecovery()

	// Corrupt bad.bin's .part by truncating it below CommittedOffset!
	for _, p := range partFiles(t, recDir) {
		if strings.HasPrefix(filepath.Base(p), "bad.bin.part.") && !strings.HasSuffix(p, transfer.MetaSuffix) {
			if err := os.Truncate(p, 100); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Phase B: restart Server -> must succeed globally, quarantine item 0, keep item 1 healthy!
	storeB, _ := transfer.NewStore(transfer.StoreConfig{Dir: recDir})
	quotaB := transfer.NewQuotaManager(transfer.QuotaConfig{DefaultDir: recDir, SafetyMargin: 1})
	sessB := session.NewManager(session.Config{IdleTimeout: 10 * time.Minute})
	transB := transfer.NewService(transfer.Config{Store: storeB, Quota: quotaB})
	coordB := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	svcsB := &Services{Sessions: sessB, Transfers: transB, Coordinator: coordB, Registry: transfer.NewRegistry(), Downloads: transfer.NewDownloadRegistry()}
	srvB := New(Config{ListenAddr: "127.0.0.1:0", RecoveryDir: stateDir, Services: svcsB})
	if err := srvB.Recover(); err != nil {
		t.Fatalf("global recovery must not fail on single corrupt upload: %v", err)
	}
	defer func() { _ = srvB.CloseRecovery() }()

	bView, _ := coordB.GetBatch(s.Key(), batch.ID)
	if bView.Items[0].State != transfer.ItemStateFailed {
		t.Fatalf("corrupted item 0 state = %q, want failed (quarantined)", bView.Items[0].State)
	}
	if bView.Items[1].State != transfer.ItemStateQueued {
		t.Fatalf("healthy sibling item 1 state = %q, want queued", bView.Items[1].State)
	}

	// Verify quarantine dir contains evidence & item 1 still uploads cleanly!
	qEntries, _ := os.ReadDir(filepath.Join(stateDir, "quarantine"))
	if len(qEntries) == 0 {
		t.Fatalf("expected quarantined files inside %s/quarantine", stateDir)
	}
	patch1, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("/api/batches/%s/items/%s/content", batch.ID, batch.Items[1].ID), bytes.NewReader(p1))
	patch1.Host = "127.0.0.1"
	patch1.Header.Set("Authorization", "Bearer "+s.Token())
	patch1.Header.Set("Upload-Offset", "0")
	patch1.ContentLength = int64(len(p1))
	r1 := httptest.NewRecorder()
	srvB.Handler().ServeHTTP(r1, patch1)
	if r1.Code != http.StatusOK {
		t.Fatalf("sibling good.bin PATCH = %d body=%s", r1.Code, r1.Body.String())
	}
}

func TestRecoveryPhase10CleansExpiredAndOrphanStaging(t *testing.T) {
	stateDir := t.TempDir()
	recDir := t.TempDir()

	// Seed an orphan .part file directly inside recDir.
	orphanPath := filepath.Join(recDir, "orphan.bin.part.deadbeef")
	_ = os.WriteFile(orphanPath, []byte("orphan"), 0o600)

	storeA, _ := transfer.NewStore(transfer.StoreConfig{Dir: recDir})
	sessA := session.NewManager(session.Config{})
	transA := transfer.NewService(transfer.Config{Store: storeA})
	coordA := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	svcsA := &Services{Sessions: sessA, Transfers: transA, Coordinator: coordA, Registry: transfer.NewRegistry(), Downloads: transfer.NewDownloadRegistry()}
	srvA := New(Config{ListenAddr: "127.0.0.1:0", RecoveryDir: stateDir, Services: svcsA})
	_ = srvA.Recover()

	s, _ := sessA.Create()
	payload := synthPattern(512*1024, 0x44)
	meta, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{DisplayName: "stale.bin", Size: int64(len(payload))}},
	})
	rec := httptest.NewRecorder()
	r, _ := http.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(meta))
	r.Host = "127.0.0.1"
	r.Header.Set("Authorization", "Bearer "+s.Token())
	srvA.Handler().ServeHTTP(rec, r)
	var b transfer.BatchView
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	cid, _ := coordA.ItemContentID(s.Key(), b.ID, b.Items[0].ID)
	_ = transA.DecideBatchItems([]string{cid}, true)
	_, _ = coordA.ApproveBatch(s.Key(), b.ID)

	patch, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("/api/batches/%s/items/%s/content", b.ID, b.Items[0].ID), bytes.NewReader(payload[:256*1024]))
	patch.Host = "127.0.0.1"
	patch.Header.Set("Authorization", "Bearer "+s.Token())
	patch.Header.Set("Upload-Offset", "0")
	patch.ContentLength = 256 * 1024
	pr := httptest.NewRecorder()
	srvA.Handler().ServeHTTP(pr, patch)
	if pr.Code != http.StatusNoContent {
		t.Fatalf("seed chunk: %d", pr.Code)
	}
	_ = srvA.CloseRecovery()

	time.Sleep(30 * time.Millisecond)

	storeB, _ := transfer.NewStore(transfer.StoreConfig{Dir: recDir})
	sessB := session.NewManager(session.Config{})
	transB := transfer.NewService(transfer.Config{Store: storeB})
	coordB := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	svcsB := &Services{Sessions: sessB, Transfers: transB, Coordinator: coordB, Registry: transfer.NewRegistry(), Downloads: transfer.NewDownloadRegistry()}
	srvB := New(Config{
		ListenAddr:  "127.0.0.1:0",
		RecoveryDir: stateDir,
		StagingTTL:  10 * time.Millisecond,
		Services:    svcsB,
	})
	if err := srvB.Recover(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srvB.CloseRecovery() }()

	if _, err := os.Stat(orphanPath); !errorsIsNotExist(err) {
		t.Fatalf("orphan part file survived Phase 10 cleanup (err=%v)", err)
	}
	if left := partFiles(t, recDir); len(left) != 0 {
		t.Fatalf("expired staging files survived Phase 10 cleanup: %v", left)
	}
}

func errorsIsNotExist(err error) bool { return os.IsNotExist(err) }

func TestCompactAckOmitsItemsWhileUpdatingBatchSummary(t *testing.T) {
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	sessMgr := session.NewManager(session.Config{})
	trans := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	svcs := &Services{
		Sessions:    sessMgr,
		Transfers:   trans,
		Coordinator: coord,
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
	}
	srv := New(Config{ListenAddr: "127.0.0.1:0", Services: svcs})
	s, _ := sessMgr.Create()

	payload := synthPattern(32*1024, 0x55)
	items := make([]transfer.ManifestItem, 5)
	for i := range items {
		items[i] = transfer.ManifestItem{
			DisplayName: fmt.Sprintf("f%d.bin", i),
			Size:        int64(len(payload)),
		}
	}
	meta, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     items,
	})
	rec := httptest.NewRecorder()
	r, _ := http.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(meta))
	r.Host = "127.0.0.1"
	r.Header.Set("Authorization", "Bearer "+s.Token())
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create batch = %d", rec.Code)
	}
	var b transfer.BatchView
	_ = json.Unmarshal(rec.Body.Bytes(), &b)

	cids := make([]string, len(b.Items))
	for i, it := range b.Items {
		cid, _ := coord.ItemContentID(s.Key(), b.ID, it.ID)
		cids[i] = cid
	}
	_ = trans.DecideBatchItems(cids, true)
	_, _ = coord.ApproveBatch(s.Key(), b.ID)

	for i, it := range b.Items {
		patch, _ := http.NewRequest(http.MethodPatch, fmt.Sprintf("/api/batches/%s/items/%s/content", b.ID, it.ID), bytes.NewReader(payload))
		patch.Host = "127.0.0.1"
		patch.Header.Set("Authorization", "Bearer "+s.Token())
		patch.Header.Set("Upload-Offset", "0")
		patch.Header.Set("X-Flitnix-Compact-Ack", "1")
		patch.ContentLength = int64(len(payload))
		pr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(pr, patch)
		if pr.Code != http.StatusOK {
			t.Fatalf("item %d PATCH = %d body=%s", i, pr.Code, pr.Body.String())
		}
		var ack struct {
			Written int64              `json:"written"`
			Batch   transfer.BatchView `json:"batch"`
		}
		if err := json.Unmarshal(pr.Body.Bytes(), &ack); err != nil {
			t.Fatal(err)
		}
		if len(ack.Batch.Items) != 0 {
			t.Fatalf("compact ACK item %d returned %d items, want 0", i, len(ack.Batch.Items))
		}
		if ack.Batch.TotalItemCount != 5 || ack.Batch.TransferredBytes != int64(i+1)*int64(len(payload)) {
			t.Fatalf("compact ACK item %d summary = %+v", i, ack.Batch)
		}
		if i == len(b.Items)-1 && ack.Batch.State != transfer.BatchStateCompleted {
			t.Fatalf("final compact ACK batch state = %q, want completed", ack.Batch.State)
		}
	}
}
