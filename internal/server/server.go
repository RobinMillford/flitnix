// Package server implements Flitnix's standalone HTTP server for the
// phone-facing webpage.
//
// The package depends only on the Go standard library, the session manager
// and the transfer service, and is deliberately independent of the Wails
// desktop layer: the desktop application (the composition root) constructs
// a server with New — wiring Config.Services when the transfer endpoints
// should exist — and runs it with Serve. The routing can be exercised with
// plain [httptest] under go test.
//
// For this stage the server is loopback-only (default address
// 127.0.0.1:43821). If Config.Services is nil the server serves only the
// page routes and every /api/ path 404s; otherwise /api/* carries the
// file-transfer endpoints behind Bearer-token authentication. If the
// configured address is already in use, Serve returns a clear error instead
// of silently picking another port.
//
// # Timeout policy
//
// A file-transfer server cannot use blanket ReadTimeout/WriteTimeout on
// http.Server: those are absolute per-connection bounds that would kill a
// legitimately large transfer that simply needs longer than the bound, and
// a laptop-sized network hiccup would abort hours of upload. The policy
// here is deliberately generous for throughput and strict for stalls:
//
//   - ReadHeaderTimeout (10s) bounds how long a fresh connection may take
//     to deliver its request headers; this is the classic baseline defence
//     against slowloris clients.
//   - Transfer-stall timeout (Config.TransferStall, default 30s; negative
//     disables it) bounds inactivity, not duration: on /api/* request endpoints
//     a rolling per-request read/write deadline is refreshed before every
//     body read and every response write via http.NewResponseController.
//     A sender that pushes a gigabyte non-stop never trips it; a sender that
//     stops making progress (or a client that stops draining a download)
//     is cut after the stall window. The sessions' own idle-expiry never
//     aborts an in-flight transfer because the handlers pin the session
//     for the duration of streaming.
//   - No absolute cap exists on transfer duration or file size by design.
//   - Shutdown: Serve's graceful drain uses Config.ShutdownTimeout; when
//     the drain window elapses the remaining connections are force-closed,
//     which promptly fails any transfer still in flight (its record ends
//     failed, not silently 200).
//
// The transfer-stall deadline is best effort: ResponseController is
// unsupported in some testing sinks, and a failure to set it is ignored
// rather than failing an otherwise good request.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// DefaultPort is the fixed TCP port the phone-facing server uses. It must
// never change: QR codes opened from earlier sessions would stop working
// and firewalls whitelisted for the port would drift.
const DefaultPort = 43821

// DefaultListenAddr is the loopback-only address the server binds to
// unless configured otherwise. The desktop adapter binds
// interface-specific LAN addresses by passing an explicit Config.ListenAddr
// (e.g. "192.168.1.42:DefaultPort"); it never binds 0.0.0.0, so the server
// stays unreachable from interfaces the user has not chosen. The default
// remains loopback so a bare New(Config{}) is safe for tests.
var DefaultListenAddr = "127.0.0.1:" + strconv.Itoa(DefaultPort)

const (
	defaultShutdownTimeout = 5 * time.Second
	readHeaderTimeout      = 10 * time.Second
	idleTimeout            = 2 * time.Minute

	// defaultTransferStall is the default inactivity window for /api/*
	// request bodies and response writes.
	defaultTransferStall = 30 * time.Second
)

// Config controls server startup. Zero values are valid: ListenAddr
// defaults to DefaultListenAddr, ShutdownTimeout to a 5s drain window and
// TransferStall to 30s.
type Config struct {
	// ListenAddr is the TCP host:port to bind, e.g. "127.0.0.1:43821".
	ListenAddr string

	// ShutdownTimeout bounds the graceful-drain phase when a Serve context
	// is cancelled. After it elapses remaining connections are closed.
	ShutdownTimeout time.Duration

	// TransferStall bounds inactivity (not duration) on the /api/*
	// transfer endpoints: a rolling read/write deadline is refreshed
	// before every body read and every response write, so slow-but-alive
	// transfers of any size pass while stuck ones are cut after the
	// window. 0 selects the default (30s); a negative value disables the
	// per-request deadlines entirely.
	TransferStall time.Duration

	// Services wires the session manager and transfer layer behind the
	// /api/ endpoints. A nil Services keeps the server page-only: every
	// /api/ path answers 404. It must be fully populated otherwise.
	Services *Services
}

// Services bundles the collaborators the API endpoints need. The desktop
// application is the composition root: it owns these instances (and the
// receive Store inside the transfer Service) and handed to Config.
type Services struct {
	Sessions  *session.Manager
	Transfers *transfer.Service
	Registry  *transfer.Registry
	Downloads *transfer.DownloadRegistry
}

// normalize fills in zero-value defaults.
func (c *Config) normalize() {
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = defaultShutdownTimeout
	}
	if c.TransferStall == 0 {
		c.TransferStall = defaultTransferStall
	}
	// A negative TransferStall is kept: it disables per-request deadlines.
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
// construction cannot fail. A fully populated Services yields a server with
// authenticated transfer endpoints; a nil Services keeps the page-only
// behaviour (all /api/ paths 404).
func New(cfg Config) *Server {
	cfg.normalize()
	handler := newRouter(cfg.Services, cfg.TransferStall)
	return &Server{
		cfg: cfg,
		http: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			// ReadTimeout/WriteTimeout are deliberately NOT set: they
			// are absolute per-connection bounds that cannot express
			// "any size goes through, stalls do not". The /api/*
			// handlers enforce rolling per-request deadlines instead
			// (see the package timeout policy); the page routes need
			// no body handling.
			IdleTimeout: idleTimeout,
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
