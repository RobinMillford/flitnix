// Package server implements Flitnix's standalone HTTP server for the
// phone-facing webpage.
//
// The package depends only on the Go standard library and is deliberately
// independent of the Wails desktop layer: the desktop application constructs
// a server with New and runs it with Serve, and the routing can be exercised
// with plain [httptest] under go test.
//
// For this stage the server is loopback-only (default address
// 127.0.0.1:43821) and serves no file-transfer endpoints; those arrive in
// later stages. If the configured address is already in use, Serve returns a
// clear error instead of silently picking another port.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"
)

// DefaultListenAddr is the loopback-only address the server binds to unless
// configured otherwise. LAN binding is a later stage.
const DefaultListenAddr = "127.0.0.1:43821"

const (
	defaultShutdownTimeout = 5 * time.Second
	readHeaderTimeout      = 10 * time.Second
	readTimeout            = 30 * time.Second
	writeTimeout           = 60 * time.Second
	idleTimeout            = 2 * time.Minute
)

// Config controls server startup. Zero values are valid: ListenAddr
// defaults to DefaultListenAddr and ShutdownTimeout to a 5s drain window.
type Config struct {
	// ListenAddr is the TCP host:port to bind, e.g. "127.0.0.1:43821".
	ListenAddr string

	// ShutdownTimeout bounds the graceful-drain phase when a Serve context
	// is cancelled. After it elapses remaining connections are closed.
	ShutdownTimeout time.Duration
}

// normalize fills in zero-value defaults.
func (c *Config) normalize() {
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
}

// Server is the HTTP server for the phone page. A Server is created once
// and served once:
//
//	srv := server.New(server.Config{})
//	err := srv.Serve(ctx) // blocks until ctx is cancelled
type Server struct {
	cfg     Config
	http    *http.Server
	handler http.Handler

	once  sync.Once
	ready chan struct{}

	mu           sync.Mutex
	listener     net.Listener
	serveStarted bool
}

// New prepares a server. No network is touched until Serve is called, so
// construction cannot fail.
func New(cfg Config) *Server {
	cfg.normalize()
	handler := newRouter()
	return &Server{
		cfg: cfg,
		http: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
		},
		handler: handler,
		ready:   make(chan struct{}),
	}
}

// Handler returns the router the server runs. Exposed for tests and for
// later integration into other infrastructure.
func (s *Server) Handler() http.Handler { return s.handler }

// Serve binds the configured address, then serves HTTP until ctx is
// cancelled (graceful drain) or the accept loop fails. It must be called at
// most once per Server.
//
// On cancellation ListenAndServe-style draining is bounded by
// Config.ShutdownTimeout after which connections are force-closed; Serve
// returns nil for a clean stop and an error for anything else.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := s.bind()
	if err != nil {
		return err
	}
	if err := s.markStarted(); err != nil {
		return err
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(ln) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("server: serving on %s: %w", s.cfg.ListenAddr, err)
	case <-ctx.Done():
		return s.drain(serveErr)
	}
}

// bind opens the listener exactly once. A second call re-uses the stored
// listener. Port-occupied failures are reported with an actionable message.
func (s *Server) bind() (net.Listener, error) {
	var (
		ln      net.Listener
		bindErr error
	)
	s.once.Do(func() {
		ln, bindErr = net.Listen("tcp", s.cfg.ListenAddr)
		if bindErr != nil {
			return
		}
		s.mu.Lock()
		s.listener = ln
		s.mu.Unlock()
		close(s.ready)
	})
	if bindErr != nil {
		if errors.Is(bindErr, syscall.EADDRINUSE) {
			return nil, fmt.Errorf(
				"server: cannot listen on %s: address already in use; stop the program holding that port or choose a different address",
				s.cfg.ListenAddr,
			)
		}
		return nil, fmt.Errorf("server: cannot listen on %s: %w", s.cfg.ListenAddr, bindErr)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listener, nil
}

// markStarted rejects a second Serve call. Serve must be called once.
func (s *Server) markStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serveStarted {
		return errors.New("server: Serve already called on this server")
	}
	s.serveStarted = true
	return nil
}

// drain gracefully closes the server and waits for Serve to finish.
func (s *Server) drain(serveErr <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// Drain window elapsed; close remaining connections hard.
		_ = s.http.Close()
	}
	err := <-serveErr
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("server: during shutdown: %w", err)
}

// Addr returns the bound listener address, or nil before Ready closes.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Ready returns a channel that is closed once the listener is bound
// successfully. If binding fails, Serve returns the error and the channel
// is never closed. Ready lets callers learn the resolved address (e.g. an
// ephemeral port) from Addr or URL without guessing or polling.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// URL returns the HTTP URL of the bound listener, or "" before Ready closes.
// It is hardcoded to http in stage 2; HTTPS is a release requirement for
// later stages.
func (s *Server) URL() string {
	addr := s.Addr()
	if addr == nil {
		return ""
	}
	return "http://" + addr.String()
}
