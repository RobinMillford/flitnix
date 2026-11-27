package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestAppWiFiRoamingKeepsSessionAndResumesMultiFileBatch(t *testing.T) {
	a := newTestApp(t)
	ip2 := loopTestAltIP(t)
	probeLANPort(t, ip2)

	if err := a.ensureServer("127.0.0.1:0"); err != nil {
		t.Fatalf("ensureServer: %v", err)
	}
	sess, err := a.sessions.Create()
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	tok := sess.Token()

	// Build a 2-file batch: 6 MiB item 0 + small sibling item 1.
	payload0 := make([]byte, 6*1024*1024+13)
	for i := range payload0 {
		payload0[i] = byte(i*37 + 3)
	}
	payload1 := []byte("sibling-survived-roaming")

	manifest, _ := json.Marshal(transfer.BatchManifest{
		Direction: transfer.DirectionPhoneToDesktop,
		Kind:      transfer.BatchKindFiles,
		Items: []transfer.ManifestItem{
			{DisplayName: "roam0.bin", Size: int64(len(payload0))},
			{DisplayName: "roam1.txt", Size: int64(len(payload1))},
		},
	})
	decReq, _ := http.NewRequest(http.MethodPost, "/api/batches", bytes.NewReader(manifest))
	decReq.Host = "127.0.0.1"
	decReq.Header.Set("Authorization", "Bearer "+tok)
	decRec := httptest.NewRecorder()
	testAppHandler(t, a).ServeHTTP(decRec, decReq)
	if decRec.Code != http.StatusCreated {
		t.Fatalf("declare batch = %d %s", decRec.Code, decRec.Body.String())
	}
	var batch BatchView
	_ = json.Unmarshal(decRec.Body.Bytes(), &batch)
	if _, err := a.ApproveBatch(batch.ID); err != nil {
		t.Fatalf("ApproveBatch: %v", err)
	}

	// Upload chunk 0 (4 MiB) before Wi-Fi drops.
	chunk1 := int64(4 << 20)
	pReq, _ := http.NewRequest(http.MethodPatch,
		testAppServerURL(t, a)+"/api/batches/"+batch.ID+"/items/"+batch.Items[0].ID+"/content",
		bytes.NewReader(payload0[:chunk1]))
	pReq.Header.Set("Authorization", "Bearer "+tok)
	pReq.Header.Set("Upload-Offset", "0")
	pReq.ContentLength = chunk1
	pRes, err := http.DefaultClient.Do(pReq)
	if err != nil {
		t.Fatal(err)
	}
	pRes.Body.Close()
	if pRes.StatusCode != http.StatusNoContent {
		t.Fatalf("pre-roam PATCH = %d", pRes.StatusCode)
	}

	// Simulate Wi-Fi drop then roam onto alternate IP (ip2).
	a.handleNetworkChange(netenv.NetworkState{Status: netenv.StatusNoNetwork})
	time.Sleep(50 * time.Millisecond)
	a.handleNetworkChange(netenv.NetworkState{
		Status:        netenv.StatusConnected,
		IP:            ip2,
		InterfaceName: "wlan0",
	})

	if _, err := a.sessions.Validate(tok); err != nil {
		t.Fatalf("session token lost during roaming: %v", err)
	}

	// Reconnect on new URL and finish item 0 + sibling item 1!
	tmpDir := t.TempDir()
	f0 := filepath.Join(tmpDir, "roam0.bin")
	f1 := filepath.Join(tmpDir, "roam1.txt")
	_ = os.WriteFile(f0, payload0, 0o600)
	_ = os.WriteFile(f1, payload1, 0o600)

	target2, err := client.ParseTarget("http://"+a.ServerAddress()+"/#"+tok, "")
	if err != nil {
		t.Fatal(err)
	}
	cli := client.NewClient(target2)
	cli.SetRetryPolicy(client.RetryPolicy{MaxRetries: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond})

	res0, err := cli.StreamItem(t.Context(), batch.ID, batch.Items[0].ID, f0, nil)
	if err != nil || !res0.Verified {
		t.Fatalf("resumed item 0 after roam = %+v, err=%v", res0, err)
	}
	res1, err := cli.StreamItem(t.Context(), batch.ID, batch.Items[1].ID, f1, nil)
	if err != nil || !res1.Verified {
		t.Fatalf("sibling item 1 after roam = %+v, err=%v", res1, err)
	}
}
