package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/recovery"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

const (
	scenario43HTTPModeEnv = "FLITNIX_SCENARIO43_HTTP_MODE"
	scenario43StateEnv    = "FLITNIX_SCENARIO43_STATE"
	scenario43TokenEnv    = "FLITNIX_SCENARIO43_TOKEN"

	scenario43Payload = "scenario-43-awaiting-digest-payload"
)

func scenario43Skip(t *testing.T) {
	t.Helper()
	if runtimeGOOS := os.Getenv("GOOS"); runtimeGOOS != "" && runtimeGOOS != "linux" {
		t.Skip("Scenario 43 abrupt-process coverage uses SIGKILL and is Linux-specific")
	}
	if syscall.SIGKILL == 0 {
		t.Skip("SIGKILL is unavailable on this platform")
	}
}

// TestWp4Scenario43HTTPServerProcessRestart is the full acceptance boundary:
// the original session authenticates, the original batch/item identity and
// approval are restored, HTTP HEAD is authoritative, completion publishes the
// exact bytes, repeated completion is idempotent, and the payload is never
// retransmitted. PATCH count is instrumented independently in both process
// lifetimes instead of being inferred from the request sequence.
func TestWp4Scenario43HTTPServerProcessRestart(t *testing.T) {
	scenario43Skip(t)

	stateDir := t.TempDir()
	payload := []byte(scenario43Payload)
	expected := sha256.Sum256(payload)

	counterA := &countingTransport{base: http.DefaultTransport}
	childA := startScenario43HTTPChild(t, "serve-before-crash", stateDir, "")
	readyA := childA.ready(t)
	t.Cleanup(func() { _ = childA.kill() })

	tusPath := fmt.Sprintf("/api/batches/%s/items/%s/tus", readyA.BatchID, readyA.ItemID)
	tusURL := readyA.URL + tusPath
	auth := "Bearer " + readyA.Token
	clientA := &http.Client{Transport: counterA}

	createReq, err := http.NewRequest(http.MethodPost, tusURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	createReq.Header.Set("Authorization", auth)
	createReq.Header.Set("Tus-Resumable", "1.0.0")
	createReq.Header.Set("Upload-Length", fmt.Sprint(len(payload)))
	createRes := doScenario43Request(t, clientA, createReq)
	if createRes.StatusCode != http.StatusCreated {
		t.Fatalf("Tus creation status = %d, want 201", createRes.StatusCode)
	}
	createRes.Body.Close()

	patchReq, err := http.NewRequest(http.MethodPatch, tusURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	patchReq.Header.Set("Authorization", auth)
	patchReq.Header.Set("Tus-Resumable", "1.0.0")
	patchReq.Header.Set("Content-Type", "application/offset+octet-stream")
	patchReq.Header.Set("Upload-Offset", "0")
	patchRes := doScenario43Request(t, clientA, patchReq)
	patchBody, _ := io.ReadAll(patchRes.Body)
	patchRes.Body.Close()
	if patchRes.StatusCode != http.StatusNoContent {
		t.Fatalf("final PATCH status = %d, body=%s, want 204", patchRes.StatusCode, patchBody)
	}
	if got := patchRes.Header.Get("Upload-Offset"); got != fmt.Sprint(len(payload)) {
		t.Fatalf("final PATCH offset = %q, want %d", got, len(payload))
	}
	if got := patchRes.Header.Get("Upload-State"); got != "awaiting_digest" {
		t.Fatalf("final PATCH state = %q, want awaiting_digest", got)
	}
	if got := patchRes.Header.Get("Upload-Prefix-Digest"); got != hex.EncodeToString(expected[:]) {
		t.Fatalf("final PATCH prefix digest = %q, want %x", got, expected)
	}

	// The response is fully consumed before SIGKILL. No graceful shutdown API
	// is called: this models a server crash at the durable awaiting_digest
	// boundary.
	if err := childA.kill(); err == nil {
		t.Fatal("crashed server exited successfully; want SIGKILL termination")
	} else if !errors.Is(err, os.ErrProcessDone) && !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("server A termination = %v, want SIGKILL", err)
	}
	if got := counterA.patches(); got != 1 {
		t.Fatalf("server A PATCH count = %d, want 1", got)
	}

	counterB := &countingTransport{base: http.DefaultTransport}
	childB := startScenario43HTTPChild(t, "serve-after-crash", stateDir, readyA.Token)
	readyB := childB.ready(t)
	t.Cleanup(func() { _ = childB.kill() })
	clientB := &http.Client{Transport: counterB}

	if readyB.Token != "" && readyB.Token != readyA.Token {
		t.Fatalf("server B reported a different session token")
	}

	headReq, err := http.NewRequest(http.MethodHead, readyB.URL+tusPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	headReq.Header.Set("Authorization", auth)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headRes := doScenario43Request(t, clientB, headReq)
	headBody, _ := io.ReadAll(headRes.Body)
	headRes.Body.Close()
	if headRes.StatusCode != http.StatusOK {
		t.Fatalf("server B HEAD status = %d, body=%s, want 200", headRes.StatusCode, headBody)
	}
	if got := headRes.Header.Get("Upload-Offset"); got != fmt.Sprint(len(payload)) {
		t.Fatalf("recovered HEAD offset = %q, want %d", got, len(payload))
	}
	if got := headRes.Header.Get("Upload-Length"); got != fmt.Sprint(len(payload)) {
		t.Fatalf("recovered HEAD length = %q, want %d", got, len(payload))
	}
	if got := headRes.Header.Get("Upload-State"); got != "awaiting_digest" {
		t.Fatalf("recovered HEAD state = %q, want awaiting_digest", got)
	}
	if got := headRes.Header.Get("Upload-Prefix-Digest"); got != hex.EncodeToString(expected[:]) {
		t.Fatalf("recovered HEAD prefix digest = %q, want %x", got, expected)
	}

	completeURL := fmt.Sprintf("%s/api/batches/%s/items/%s/complete", readyB.URL, readyA.BatchID, readyA.ItemID)
	completeReq, err := http.NewRequest(http.MethodPost, completeURL, strings.NewReader(fmt.Sprintf(`{"expectedSHA256":"%x"}`, expected)))
	if err != nil {
		t.Fatal(err)
	}
	completeReq.Header.Set("Authorization", auth)
	completeReq.Header.Set("Content-Type", "application/json")
	completeRes := doScenario43Request(t, clientB, completeReq)
	completeBody, _ := io.ReadAll(completeRes.Body)
	completeRes.Body.Close()
	if completeRes.StatusCode != http.StatusOK {
		t.Fatalf("recovered completion status = %d, body=%s, want 200", completeRes.StatusCode, completeBody)
	}
	var completion struct {
		Status string `json:"status"`
		SHA256 string `json:"sha256"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(completeBody, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.Status != "completed" || completion.SHA256 != hex.EncodeToString(expected[:]) || completion.Size != int64(len(payload)) {
		t.Fatalf("unexpected completion response: %+v", completion)
	}
	published, err := os.ReadFile(completion.Path)
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if !bytes.Equal(published, payload) {
		t.Fatal("published bytes differ from original payload")
	}

	repeatReq, err := http.NewRequest(http.MethodPost, completeURL, strings.NewReader(fmt.Sprintf(`{"expectedSHA256":"%x"}`, expected)))
	if err != nil {
		t.Fatal(err)
	}
	repeatReq.Header.Set("Authorization", auth)
	repeatReq.Header.Set("Content-Type", "application/json")
	repeatRes := doScenario43Request(t, clientB, repeatReq)
	repeatRes.Body.Close()
	if repeatRes.StatusCode != http.StatusOK {
		t.Fatalf("repeated completion status = %d, want 200", repeatRes.StatusCode)
	}

	if got := counterB.patches(); got != 0 {
		t.Fatalf("server B issued %d PATCH request(s) after restart, want 0", got)
	}
	t.Logf("server B recovered upload %s through HTTP HEAD and completion without retransmission", readyA.Transfer)
}

// TestWp4Scenario43RevokedSessionCannotContinue proves a revocation is
// durable: the token must not work in a fresh process.
func TestWp4Scenario43RevokedSessionCannotContinue(t *testing.T) {
	scenario43Skip(t)
	stateDir := t.TempDir()

	childA := startScenario43HTTPChild(t, "revoke-before-crash", stateDir, "")
	readyA := childA.ready(t)
	if err := childA.kill(); err == nil {
		t.Fatal("crashed server exited successfully")
	}

	childB := startScenario43HTTPChild(t, "serve-after-crash", stateDir, readyA.Token)
	readyB := childB.ready(t)
	t.Cleanup(func() { _ = childB.kill() })

	res := doScenario43Request(t, http.DefaultClient, mustRequest(t, http.MethodHead,
		fmt.Sprintf("%s/api/batches/%s/items/%s/tus", readyB.URL, readyA.BatchID, readyA.ItemID), readyA.Token))
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HEAD with a revoked token after restart = %d, want 401", res.StatusCode)
	}
}

// TestWp4Scenario43ExpiredSessionCannotContinue proves expiry is not undone by
// a restart: the durable ExpiresAt is authoritative even though no expiry
// event was appended before the crash.
func TestWp4Scenario43ExpiredSessionCannotContinue(t *testing.T) {
	scenario43Skip(t)
	stateDir := t.TempDir()

	childA := startScenario43HTTPChild(t, "expire-before-crash", stateDir, "")
	readyA := childA.ready(t)
	if err := childA.kill(); err == nil {
		t.Fatal("crashed server exited successfully")
	}

	childB := startScenario43HTTPChild(t, "serve-after-crash", stateDir, readyA.Token)
	readyB := childB.ready(t)
	t.Cleanup(func() { _ = childB.kill() })

	res := doScenario43Request(t, http.DefaultClient, mustRequest(t, http.MethodHead,
		fmt.Sprintf("%s/api/batches/%s/items/%s/tus", readyB.URL, readyA.BatchID, readyA.ItemID), readyA.Token))
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HEAD with an expired token after restart = %d, want 401", res.StatusCode)
	}
}

// TestWp4Scenario43InvalidJournalPreventsReadiness proves global journal
// corruption is fail-closed: the child exits without ever publishing readiness.
func TestWp4Scenario43InvalidJournalPreventsReadiness(t *testing.T) {
	scenario43Skip(t)
	stateDir := t.TempDir()
	seedRecoveryState(t, stateDir)

	path := filepath.Join(stateDir, "state.wal")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("seeded journal is empty")
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	child := startScenario43HTTPChild(t, "serve-after-crash", stateDir, "irrelevant")
	assertNoReadiness(t, child)
}

// TestWp4Scenario43MissingKeyPreventsReadiness proves a missing installation
// key alongside existing trusted state fails closed instead of silently
// generating a replacement and restoring old sessions.
func TestWp4Scenario43MissingKeyPreventsReadiness(t *testing.T) {
	scenario43Skip(t)
	stateDir := t.TempDir()
	seedRecoveryState(t, stateDir)
	if err := os.Remove(filepath.Join(stateDir, "install-key-v1")); err != nil {
		t.Fatal(err)
	}

	child := startScenario43HTTPChild(t, "serve-after-crash", stateDir, "irrelevant")
	assertNoReadiness(t, child)
	if _, err := os.Stat(filepath.Join(stateDir, "install-key-v1")); !os.IsNotExist(err) {
		t.Fatalf("a replacement installation key was created: %v", err)
	}
}

// TestWp4Scenario43ResumableRecoveryFailurePreventsReadiness proves a staging
// recovery failure blocks listener readiness instead of serving unknown state.
func TestWp4Scenario43ResumableRecoveryFailurePreventsReadiness(t *testing.T) {
	scenario43Skip(t)
	stateDir := t.TempDir()
	seedRecoveryState(t, stateDir)
	// A regular file where the staging directory belongs makes os.ReadDir fail
	// with a real (non-ENOENT) error.
	if err := os.WriteFile(filepath.Join(stateDir, transfer.StagingDirName), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	child := startScenario43HTTPChild(t, "serve-after-crash", stateDir, "irrelevant")
	assertNoReadiness(t, child)
}

// TestWp4Scenario43OrphanStagingCannotBePublished proves an upload with no
// durable batch/item/owner association is quarantined, is never addressable
// through HTTP, and is never published.
func TestWp4Scenario43OrphanStagingCannotBePublished(t *testing.T) {
	scenario43Skip(t)
	stateDir := t.TempDir()

	childA := startScenario43HTTPChild(t, "serve-before-crash", stateDir, "")
	readyA := childA.ready(t)
	if err := childA.kill(); err == nil {
		t.Fatal("crashed server exited successfully")
	}

	staging := filepath.Join(stateDir, transfer.StagingDirName)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	orphanMeta := transfer.ResumableMeta{
		Version: 2, TransferID: "orphan-transfer", BatchID: "b_orphan", ItemID: "it_orphan",
		SessionKey: readyA.SessionKey, SanitizedName: "orphan.bin", RelativePath: "orphan.bin",
		DestinationDir: stateDir, TotalSize: 4, CommittedOffset: 0,
		State: transfer.ResumableAwaitingDigest,
	}
	encoded, err := json.Marshal(&orphanMeta)
	if err != nil {
		t.Fatal(err)
	}
	// The staging files must use the manager's real on-disk names.
	metaName := fmt.Sprintf(".flitnix-part-%s.meta", orphanMeta.TransferID)
	partName := fmt.Sprintf(".flitnix-part-%s.part", orphanMeta.TransferID)
	if err := os.WriteFile(filepath.Join(staging, metaName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, partName), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	childB := startScenario43HTTPChild(t, "serve-after-crash", stateDir, readyA.Token)
	readyB := childB.ready(t)
	t.Cleanup(func() { _ = childB.kill() })

	res := doScenario43Request(t, http.DefaultClient, mustRequest(t, http.MethodHead,
		fmt.Sprintf("%s/api/batches/%s/items/%s/tus", readyB.URL, orphanMeta.BatchID, orphanMeta.ItemID), readyA.Token))
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD for a quarantined orphan upload = %d, want 404", res.StatusCode)
	}

	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	quarantined := false
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".quarantine-") {
			quarantined = true
		}
		if entry.Name() == metaName || entry.Name() == partName {
			t.Fatalf("quarantined upload file %q is still live", entry.Name())
		}
	}
	if !quarantined {
		t.Fatal("orphan upload was not preserved under a quarantine name")
	}
}

// seedRecoveryState creates a trusted state directory containing one session
// record and its installation key.
func seedRecoveryState(t *testing.T, stateDir string) {
	t.Helper()
	store, err := recovery.Open(stateDir)
	if err != nil {
		t.Fatalf("seed recovery state: %v", err)
	}
	record := recovery.SessionRecord{
		SessionID: "sess_seed", SessionKey: "seeded-key",
		TokenVerifier: "00", KeyID: recovery.KeyID, Generation: 1,
		Status: "active", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := store.Append(recovery.EventSessionCreated, record); err != nil {
		t.Fatalf("seed session record: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}
}

func mustRequest(t *testing.T, method, url, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	return req
}

// countingTransport counts PATCH requests independently per process lifetime.
type countingTransport struct {
	base  http.RoundTripper
	mu    sync.Mutex
	patch int
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPatch {
		c.mu.Lock()
		c.patch++
		c.mu.Unlock()
	}
	return c.base.RoundTrip(req)
}

func (c *countingTransport) patches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.patch
}

type scenario43HTTPChild struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	readyC  chan scenario43HTTPReadyResult
	exitErr chan error
}

type scenario43HTTPReadyResult struct {
	ready scenario43HTTPReady
	err   string
}

type scenario43HTTPReady struct {
	URL        string `json:"url"`
	Token      string `json:"token"`
	SessionKey string `json:"sessionKey"`
	BatchID    string `json:"batchID"`
	ItemID     string `json:"itemID"`
	Transfer   string `json:"transfer"`
}

func startScenario43HTTPChild(t *testing.T, mode, stateDir, token string) *scenario43HTTPChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestScenario43HTTPServerChild$")
	cmd.Env = append(os.Environ(),
		scenario43HTTPModeEnv+"="+mode,
		scenario43StateEnv+"="+stateDir,
		scenario43TokenEnv+"="+token,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	readyC := make(chan scenario43HTTPReadyResult, 1)
	exitErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		var ready scenario43HTTPReady
		if scanner.Scan() && json.Unmarshal(scanner.Bytes(), &ready) == nil && ready.URL != "" {
			readyC <- scenario43HTTPReadyResult{ready: ready}
			_, _ = io.Copy(io.Discard, stdout)
			exitErr <- cmd.Wait()
			return
		}
		errText, _ := io.ReadAll(stderr)
		exitErr <- cmd.Wait()
		readyC <- scenario43HTTPReadyResult{err: string(errText)}
	}()
	return &scenario43HTTPChild{cmd: cmd, stdout: stdout, stderr: stderr, readyC: readyC, exitErr: exitErr}
}

func (c *scenario43HTTPChild) ready(t *testing.T) scenario43HTTPReady {
	t.Helper()
	select {
	case result := <-c.readyC:
		if result.ready.URL == "" {
			t.Fatalf("server child did not publish readiness: %s", result.err)
		}
		return result.ready
	case <-time.After(20 * time.Second):
		_ = c.cmd.Process.Kill()
		t.Fatal("server child readiness timed out")
		return scenario43HTTPReady{}
	}
}

// assertNoReadiness requires the child to exit without publishing readiness.
func assertNoReadiness(t *testing.T, child *scenario43HTTPChild) {
	t.Helper()
	select {
	case result := <-child.readyC:
		if result.ready.URL != "" {
			_ = child.cmd.Process.Kill()
			t.Fatalf("server child published readiness despite a fatal recovery failure: %s", result.ready.URL)
		}
		if strings.TrimSpace(result.err) == "" {
			t.Fatal("server child failed without a diagnostic")
		}
		t.Logf("child refused readiness: %s", strings.TrimSpace(result.err))
	case <-time.After(20 * time.Second):
		_ = child.cmd.Process.Kill()
		t.Fatal("server child neither became ready nor failed")
	}
}

// kill terminates the child process and waits for its exit.
//
// The waiter goroutine started by startScenario43HTTPChild owns cmd.Wait and
// is the only goroutine allowed to call it; a second concurrent Wait races on
// os/exec's internal state. kill therefore signals and then observes exitErr,
// and is idempotent: the collected exit error is put back for later callers.
func (c *scenario43HTTPChild) kill() error {
	if c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	signalErr := c.cmd.Process.Signal(syscall.SIGKILL)
	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		return signalErr
	}
	select {
	case waitErr := <-c.exitErr:
		c.exitErr <- waitErr
		return waitErr
	case <-time.After(20 * time.Second):
		return errors.New("server child did not exit after SIGKILL")
	}
}

func doScenario43Request(t *testing.T, client *http.Client, req *http.Request) *http.Response {
	t.Helper()
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	return res
}

func TestScenario43HTTPServerChild(t *testing.T) {
	mode := os.Getenv(scenario43HTTPModeEnv)
	if mode == "" {
		return
	}
	stateDir := os.Getenv(scenario43StateEnv)
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: stateDir})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	idle := 10 * time.Minute
	if mode == "expire-before-crash" {
		idle = 2 * time.Second
	}
	sessions := session.NewManager(session.Config{IdleTimeout: idle})
	transfers := transfer.NewService(transfer.Config{Store: store})
	coord := transfer.NewCoordinator(transfer.CoordinatorConfig{})
	services := &Services{
		Sessions: sessions, Transfers: transfers, Coordinator: coord,
		Registry: transfer.NewRegistry(), Downloads: transfer.NewDownloadRegistry(),
	}
	services.Resumable = transfer.NewResumableManager(store,
		transfer.NewQuotaManager(stateDir, transfer.DefaultSafetyMargin, transfer.DefaultMaxConcurrentPerSession))

	srv := New(Config{ListenAddr: "127.0.0.1:0", RecoveryDir: stateDir, Services: services})

	// Recovery is explicit and must succeed before any session is created:
	// creation itself is a durable transition, so the hooks must be installed
	// first.
	if err := srv.Recover(); err != nil {
		fmt.Fprintln(os.Stderr, "recovery failed:", err)
		os.Exit(3)
	}

	ready := scenario43HTTPReady{}
	switch mode {
	case "serve-before-crash", "revoke-before-crash", "expire-before-crash":
		sess, err := sessions.Create()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		manifest := transfer.BatchManifest{
			Direction: transfer.DirectionPhoneToDesktop,
			Kind:      transfer.BatchKindFiles,
			Items:     []transfer.ManifestItem{{DisplayName: "scenario43.bin", Size: int64(len(scenario43Payload))}},
		}
		batch, err := coord.CreateBatch(sess.Key(), manifest)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if _, err := coord.ApproveBatch(sess.Key(), batch.ID); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		ready.Token, ready.BatchID, ready.ItemID = sess.Token(), batch.ID, batch.Items[0].ID
		ready.SessionKey = sess.Key()
		ready.Transfer = personalTransferID(batch.ID, batch.Items[0].ID)
	case "serve-after-crash":
		ready.Token = os.Getenv(scenario43TokenEnv)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()
	select {
	case <-srv.Ready():
	case err := <-serveErr:
		fmt.Fprintln(os.Stderr, "server did not become ready:", err)
		os.Exit(3)
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "server readiness timed out")
		os.Exit(2)
	}
	ready.URL = srv.URL()

	switch mode {
	case "revoke-before-crash":
		if err := sessions.Revoke(ready.Token); err != nil {
			fmt.Fprintln(os.Stderr, "revoke:", err)
			os.Exit(2)
		}
	case "expire-before-crash":
		time.Sleep(2500 * time.Millisecond)
	}

	encoded, _ := json.Marshal(ready)
	fmt.Println(string(encoded))
	for {
		time.Sleep(time.Second)
	}
}
