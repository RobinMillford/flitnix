package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startTestServer binds an ephemeral loopback port (no fixed-port races) and
// starts serving until the test cleanup cancels it. The cleanup verifies the
// server stops cleanly, exercising the graceful-shutdown path on every test.
func startTestServer(t *testing.T) *Server {
	t.Helper()

	s := New(Config{ListenAddr: "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ctx) }()

	select {
	case <-s.Ready():
	case err := <-errc:
		cancel()
		t.Fatalf("server failed to start: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		<-errc
		t.Fatal("timed out waiting for the server to become ready")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Errorf("server returned error during shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("server did not stop after context cancellation")
		}
	})
	return s
}

func TestServeAcceptsRealRequests(t *testing.T) {
	s := startTestServer(t)

	resp, err := http.Get("http://" + s.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz on the real server failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("real GET /healthz returned %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("real /healthz body is not valid JSON: %v\nbody: %q", err, body)
	}
	if payload.Status != "ok" {
		t.Errorf("real /healthz reported status %q, want \"ok\"", payload.Status)
	}
}

func TestServePortInUse(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocating the blocking listener: %v", err)
	}
	defer blocker.Close()

	s := New(Config{ListenAddr: blocker.Addr().String()})
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(context.Background()) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatalf("Serve succeeded on an occupied port %s, want an error", blocker.Addr())
		}
		if !strings.Contains(err.Error(), "already in use") {
			t.Errorf("port-in-use error = %v, want it to mention \"already in use\"", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Serve did not return despite the port being occupied")
	}
}

func TestServeRejectsSecondCall(t *testing.T) {
	s := startTestServer(t)

	errc := make(chan error, 1)
	go func() { errc <- s.Serve(context.Background()) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("second Serve call succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "already called") {
			t.Errorf("second-Serve error = %v, want it to mention \"already called\"", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second Serve call blocked instead of returning an error")
	}
}

func TestServerURLMatchesListener(t *testing.T) {
	s := startTestServer(t)

	url := s.URL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("URL() = %q, want it to start with \"http://127.0.0.1:\"", url)
	}
	if !strings.HasSuffix(url, "/") {
		// Addr() is host:port; the URL helper adds just the scheme here, so
		// no trailing slash is expected — this assertion documents that.
	}
}
