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

	scenario43Payload = "scenario-43-simplified-streaming-recovery-payload"
)

// TestWp4Scenario43HTTPServerProcessRestart is the Scenario 43 acceptance test
// for the simplified streaming architecture:
// Server A establishes durable session, batch, and item approval, then crashes
// abruptly via SIGKILL. Server B restarts with the same recovery directory,
// validates startup recovery (Phases 1-5), authenticates the original bearer
// token, and streams the upload via single-pass HTTP PUT to byte-for-byte
// publication.
func TestWp4Scenario43HTTPServerProcessRestart(t *testing.T) {
	stateDir := t.TempDir()
	payload := []byte(scenario43Payload)
	expectedHash := sha256.Sum256(payload)

	childA := startScenario43HTTPChild(t, "serve-before-crash", stateDir, "")
	readyA := childA.ready(t)
	t.Cleanup(func() { _ = childA.kill() })

	// Server A crashes abruptly via SIGKILL while session & batch are durable.
	if killErr := childA.kill(); killErr == nil {
		t.Fatal("crashed server exited successfully; want abrupt termination")
	} else if !errors.Is(killErr, os.ErrProcessDone) && !scenario43AbruptTermination(killErr) {
		t.Fatalf("server A termination = %v, want abrupt termination", killErr)
	}

	// Server B restarts using the same durable state directory.
	childB := startScenario43HTTPChild(t, "serve-after-crash", stateDir, readyA.Token)
	readyB := childB.ready(t)
	t.Cleanup(func() { _ = childB.kill() })

	if readyB.Token != "" && readyB.Token != readyA.Token {
		t.Fatalf("server B reported unexpected token: %s", readyB.Token)
	}

	// Verify Server B authenticates the original restored token by creating an approved batch
	manifest := transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items:     []transfer.ManifestItem{{RelativePath: "scenario43.bin", Size: int64(len(payload))}},
	}
	body, _ := json.Marshal(manifest)
	batchReq, _ := http.NewRequest(http.MethodPost, readyB.URL+"/api/batches", bytes.NewReader(body))
	batchReq.Header.Set("Authorization", "Bearer "+readyA.Token)
	batchReq.Header.Set("Content-Type", "application/json")
	batchRes := doScenario43Request(t, http.DefaultClient, batchReq)
	if batchRes.StatusCode != http.StatusCreated {
		t.Fatalf("batch creation with restored session returned %d", batchRes.StatusCode)
	}
	var bView transfer.BatchView
	if err := json.NewDecoder(batchRes.Body).Decode(&bView); err != nil {
		batchRes.Body.Close()
		t.Fatalf("decode batch view: %v", err)
	}
	batchRes.Body.Close()

	// Stream file to Server B via single-pass HTTP PUT using the restored session and approved item.
	putURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", readyB.URL, bView.ID, bView.Items[0].ID)
	putReq, err := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	putReq.Header.Set("Authorization", "Bearer "+readyA.Token)
	putReq.Header.Set("Content-Type", "application/octet-stream")

	putRes := doScenario43Request(t, http.DefaultClient, putReq)
	defer putRes.Body.Close()
	if putRes.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(putRes.Body)
		t.Fatalf("streaming upload after restart returned status %d, body=%s", putRes.StatusCode, body)
	}

	var pub struct {
		Written int64              `json:"written"`
		SHA256  string             `json:"sha256"`
		Batch   transfer.BatchView `json:"batch"`
	}
	if err := json.NewDecoder(putRes.Body).Decode(&pub); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}

	if pub.SHA256 != hex.EncodeToString(expectedHash[:]) {
		t.Fatalf("published SHA-256 mismatch: got %s, want %x", pub.SHA256, expectedHash)
	}
	if pub.Written != int64(len(payload)) {
		t.Fatalf("written bytes mismatch: got %d, want %d", pub.Written, len(payload))
	}

	// Verify the published file on disk in stateDir
	data, err := os.ReadFile(filepath.Join(stateDir, "scenario43.bin"))
	if err != nil {
		t.Fatalf("read published file: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("published file content mismatch")
	}
}

// TestWp4Scenario43RevokedSessionCannotContinue proves a revocation is
// durable: the token must not work in a fresh process after restart.
func TestWp4Scenario43RevokedSessionCannotContinue(t *testing.T) {
	stateDir := t.TempDir()

	childA := startScenario43HTTPChild(t, "revoke-before-crash", stateDir, "")
	readyA := childA.ready(t)
	if err := childA.kill(); err == nil {
		t.Fatal("crashed server exited successfully")
	}

	childB := startScenario43HTTPChild(t, "serve-after-crash", stateDir, readyA.Token)
	readyB := childB.ready(t)
	t.Cleanup(func() { _ = childB.kill() })

	req, _ := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/api/batches/%s/items/%s/content", readyB.URL, readyA.BatchID, readyA.ItemID),
		bytes.NewReader([]byte("test")))
	req.Header.Set("Authorization", "Bearer "+readyA.Token)

	res := doScenario43Request(t, http.DefaultClient, req)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("PUT with a revoked token after restart = %d, want 401", res.StatusCode)
	}
}

// TestWp4Scenario43ExpiredSessionCannotContinue proves expiry is not undone by
// a restart: the durable ExpiresAt is authoritative.
func TestWp4Scenario43ExpiredSessionCannotContinue(t *testing.T) {
	stateDir := t.TempDir()

	childA := startScenario43HTTPChild(t, "expire-before-crash", stateDir, "")
	readyA := childA.ready(t)
	if err := childA.kill(); err == nil {
		t.Fatal("crashed server exited successfully")
	}

	childB := startScenario43HTTPChild(t, "serve-after-crash", stateDir, readyA.Token)
	readyB := childB.ready(t)
	t.Cleanup(func() { _ = childB.kill() })

	req, _ := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/api/batches/%s/items/%s/content", readyB.URL, readyA.BatchID, readyA.ItemID),
		bytes.NewReader([]byte("test")))
	req.Header.Set("Authorization", "Bearer "+readyA.Token)

	res := doScenario43Request(t, http.DefaultClient, req)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("PUT with an expired token after restart = %d, want 401", res.StatusCode)
	}
}

// TestWp4Scenario43InvalidJournalPreventsReadiness proves global journal
// corruption is fail-closed: the child exits without ever publishing readiness.
func TestWp4Scenario43InvalidJournalPreventsReadiness(t *testing.T) {
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
func (c *scenario43HTTPChild) kill() error {
	if c.cmd.Process == nil {
		return os.ErrProcessDone
	}
	signalErr := terminateScenario43Child(c.cmd.Process)
	if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
		return signalErr
	}
	select {
	case waitErr := <-c.exitErr:
		c.exitErr <- waitErr
		return waitErr
	case <-time.After(20 * time.Second):
		return errors.New("server child did not exit after termination")
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
	services.OnBatchPending = func(sessionKey string, batch transfer.BatchView, remoteAddr string) {
		for _, item := range batch.Items {
			contentID, err := coord.ItemContentID(sessionKey, batch.ID, item.ID)
			if err != nil {
				fmt.Fprintln(os.Stderr, "ItemContentID err:", err)
			} else {
				if dErr := transfers.DecideBatchItems([]string{contentID}, true); dErr != nil {
					fmt.Fprintln(os.Stderr, "DecideBatchItems err:", dErr)
				}
			}
		}
		if _, aErr := coord.ApproveBatch(sessionKey, batch.ID); aErr != nil {
			fmt.Fprintln(os.Stderr, "ApproveBatch err:", aErr)
		}
	}

	srv := New(Config{ListenAddr: "127.0.0.1:0", RecoveryDir: stateDir, Services: services})

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
		ready.Token = sess.Token()
		ready.SessionKey = sess.Key()
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
