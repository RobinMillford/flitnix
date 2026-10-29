package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/textmsg"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestTextMessagingRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	store, _ := transfer.NewStore(transfer.StoreConfig{Dir: tmpDir})
	sessMgr := session.NewManager(session.Config{})
	textStore := textmsg.NewStore()
	transfers := transfer.NewService(transfer.Config{Store: store})
	downloads := transfer.NewDownloadRegistry()

	services := &Services{
		Sessions:     sessMgr,
		Transfers:    transfers,
		Downloads:    downloads,
		TextMessages: textStore,
	}

	grants := newGrantStore(downloads)
	handler := newRouter(services, 5*time.Second, 5*time.Second, grants)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Create session
	sess, err := sessMgr.Create()
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}

	// 1. Initial list is empty
	reqList, _ := http.NewRequest("GET", ts.URL+"/api/text", nil)
	reqList.Header.Set("Authorization", "Bearer "+sess.Token())
	respList, err := http.DefaultClient.Do(reqList)
	if err != nil {
		t.Fatalf("GET /api/text: %v", err)
	}
	defer respList.Body.Close()
	if respList.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", respList.StatusCode)
	}

	// 2. Send text message from phone
	sendPayload, _ := json.Marshal(map[string]string{
		"content": "Check out https://github.com/RobinMillford/flitnix for updates!",
	})
	reqSend, _ := http.NewRequest("POST", ts.URL+"/api/text", bytes.NewReader(sendPayload))
	reqSend.Header.Set("Authorization", "Bearer "+sess.Token())
	reqSend.Header.Set("Content-Type", "application/json")
	respSend, err := http.DefaultClient.Do(reqSend)
	if err != nil {
		t.Fatalf("POST /api/text: %v", err)
	}
	defer respSend.Body.Close()
	if respSend.StatusCode != http.StatusCreated {
		t.Fatalf("POST status = %d; want 201", respSend.StatusCode)
	}

	var msgRes textmsg.Message
	_ = json.NewDecoder(respSend.Body).Decode(&msgRes)
	if msgRes.Sender != "phone" || !bytes.Contains([]byte(msgRes.Content), []byte("Check out")) {
		t.Fatalf("unexpected message result: %+v", msgRes)
	}

	// 3. List messages returns the new message
	respList2, err := http.DefaultClient.Do(reqList)
	if err != nil {
		t.Fatalf("GET /api/text: %v", err)
	}
	defer respList2.Body.Close()
	var listRes struct {
		Messages []textmsg.Message `json:"messages"`
	}
	_ = json.NewDecoder(respList2.Body).Decode(&listRes)
	if len(listRes.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(listRes.Messages))
	}
}
