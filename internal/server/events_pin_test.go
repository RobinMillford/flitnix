package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
)

func TestPersonalEventsRequirePINUnlock(t *testing.T) {
	srv, svcs, _ := startEventsTestServer(t)
	sess, err := svcs.Sessions.CreateWithPolicy(session.PolicyOptions{PinCode: "7391"})
	if err != nil {
		t.Fatal(err)
	}

	jar, _ := cookiejar.New(nil)
	cli := &http.Client{Jar: jar}

	probe := func() int {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL()+"/api/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+sess.Token())
		resp, err := cli.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := probe(); got != http.StatusForbidden {
		t.Fatalf("GET /api/events before PIN unlock = %d; want 403", got)
	}

	body, _ := json.Marshal(map[string]string{"pin": "7391"})
	req, err := http.NewRequest(http.MethodPost, srv.URL()+"/api/pin/verify", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sess.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PIN verify = %d; want 200", resp.StatusCode)
	}

	// An unlocked session must still be able to subscribe. Bound the probe so
	// a successful SSE stream cannot hang the test.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL()+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sess.Token())
	resp, err = cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/events after PIN unlock = %d; want 200", resp.StatusCode)
	}
}
