// Flitnix desktop application: the composition root.
//
// This file is the thin Wails adapter around the standalone Flitnix core.
// The core packages (session, transfer, server) know nothing about Wails;
// the desktop layer only wires them together, keeps the receive store
// alive, drives the HTTP server's lifecycle and projects records into
// views safe for the UI.
//
// Privacy rules carried into every view type:
//
//   - Session tokens are surfaced only by StartSession (the desktop needs
//     them for the QR flow) and never by any list view.
//   - Receive-directory paths of received files are never exposed; the UI
//     works with opaque upload IDs and states only.
//   - Shared-file local paths are never exposed to the UI; listings carry
//     opaque IDs.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/qr"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// SessionView is the UI projection of a pairing session. Token is populated
// only when the session has just been created (StartSession); every other
// producer leaves it empty so the raw secret is available exactly once, to
// the one code path that needs it.
type SessionView struct {
	Key             string    `json:"key"`
	Created         time.Time `json:"created"`
	ExpiresAt       time.Time `json:"expiresAt"`
	PinnedTransfers int32     `json:"pinnedTransfers"`
	Token           string    `json:"token,omitempty"`

	// InterfaceName and Address are populated by StartSession: the chosen
	// network interface and the machine's own IPv4 address used for the
	// session's URL (never a gateway address). Listings leave both empty.
	InterfaceName string `json:"interfaceName,omitempty"`
	Address       string `json:"address,omitempty"`
}

// UploadView is the UI projection of an incoming upload (pending approval,
// in flight or finished). Received file paths are deliberately not part of
// the projection: the UI names uploads by their opaque ID and state.
type UploadView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
	State    string    `json:"state"`
	Uploaded int64     `json:"uploaded"`
	Speed    float64   `json:"speed"`
	Reason   string    `json:"reason,omitempty"`
}

type DownloadView struct {
	ID      string    `json:"id"`
	FileID  string    `json:"fileId"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	State   string    `json:"state"`
	Written int64     `json:"written"`
	Speed   float64   `json:"speed"`
	Reason  string    `json:"reason,omitempty"`
}

// SharedFileView is the UI projection of one file currently shared with
// phones. The local filesystem path is deliberately not exposed.
type SharedFileView struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

// App owns the Flitnix core instances and the desktop lifecycle.
type App struct {
	ctx context.Context

	store     *transfer.Store
	sessions  *session.Manager
	transfers *transfer.Service
	registry  *transfer.Registry
	downloads *transfer.DownloadRegistry

	// source supplies live network-interface state for the session
	// address decisions; tests inject synthetic interfaces.
	source netenv.Source

	srvMu       sync.Mutex
	srv         *server.Server
	serveCancel context.CancelFunc
	serveDone   chan error
}

// NewApp constructs the application: receive store on the default directory
// (~/Downloads/Flitnix), a fresh session manager, transfer service and
// shared-file registry. The HTTP server is not started here; it starts
// lazily when the first session is created and stops again when the last
// session ends.
func NewApp() (*App, error) {
	store, err := transfer.NewStore(transfer.StoreConfig{})
	if err != nil {
		return nil, err
	}
	return &App{
		store:     store,
		sessions:  session.NewManager(session.Config{}),
		transfers: transfer.NewService(transfer.Config{Store: store}),
		registry:  transfer.NewRegistry(),
		downloads: transfer.NewDownloadRegistry(),
		source:    netenv.DefaultSource(),
	}, nil
}

// startup records the Wails runtime context so bound methods can call the
// runtime (native file dialogs).
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// shutdown tears everything down in a safe order: cancel each session's
// in-flight or approved-but-unstreamed uploads, revoke every session,
// forget shared files and stop the HTTP server. Any upload streamed at that
// moment ends canceled (never half-published), matching the core's
// shutdown contract.
func (a *App) shutdown(ctx context.Context) {
	for _, s := range a.sessions.ActiveSessions() {
		a.transfers.CancelSession(s.Key())
		a.downloads.CancelSession(s.Key())
	}
	a.sessions.Close()
	a.registry.Clear()
	a.stopServer()
}

// ---- HTTP server lifecycle (lazy start, address-aware reuse) ----

// ensureServer makes the phone-facing server reachable on listen (an
// explicit ip:port address; never 0.0.0.0). A running server bound to
// exactly the requested address is reused, so an unchanged network never
// interrupts live transfers. Any other request — including a different
// address, which means the user's network changed since the server was
// bound — ends every active session first (their URLs pointed at the old
// network's address and can no longer be reached), stops the old server
// and binds the new one. A busy port fails the call with an actionable
// error; no random port is ever substituted.
func (a *App) ensureServer(listen string) error {
	a.srvMu.Lock()
	defer a.srvMu.Unlock()

	if a.srv != nil {
		if got := a.srv.Addr(); got != nil && got.String() == listen {
			return nil
		}
		a.endAllSessions()
		a.stopServerLocked()
	}
	return a.startServerLocked(listen)
}

// startServerLocked constructs and serves a new server on listen and
// waits (bounded) for the listener to bind, so StartSession never hands
// out a token before the server is reachable. Callers must hold srvMu.
func (a *App) startServerLocked(listen string) error {
	srv := server.New(server.Config{
		ListenAddr: listen,
		Services: &server.Services{
			Sessions:  a.sessions,
			Transfers: a.transfers,
			Registry:  a.registry,
			Downloads: a.downloads,
		},
	})
	serveCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(serveCtx) }()

	select {
	case err := <-done:
		cancel()
		return fmt.Errorf("flitnix: the local server could not start on %s: %w", listen, err)
	case <-srv.Ready():
		a.srv, a.serveCancel, a.serveDone = srv, cancel, done
		return nil
	case <-time.After(3 * time.Second):
		cancel()
		<-done // bounded by the server's own drain window
		return errors.New("flitnix: the local server did not bind its address in time")
	}
}

// stopServer cancels Serve and waits (bounded) for it to return. Safe to
// call repeatedly and before any session ever existed.
func (a *App) stopServer() {
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	a.stopServerLocked()
}

// stopServerLocked is stopServer's body. Callers must hold srvMu.
func (a *App) stopServerLocked() {
	if a.serveCancel == nil {
		return
	}
	a.serveCancel()
	select {
	case <-a.serveDone:
	case <-time.After(5 * time.Second):
		// Bounded only by the server's drain window; a longer wait would
		// mean an already-stranded Serve, which nothing here can fix.
	}
	a.srv, a.serveCancel, a.serveDone = nil, nil, nil
}

// endAllSessions ends every active session, mirroring EndSession for all
// sessions at once: in-flight or approved-but-unstreamed uploads are
// canceled, per-session shared files are unlisted and the tokens die.
// It is used when the network the server was bound to can no longer serve
// the old sessions, so nothing can reach them anyway. Callers must hold
// srvMu (it runs between stopServerLocked and startServerLocked).
func (a *App) endAllSessions() {
	for _, s := range a.sessions.ActiveSessions() {
		key := s.Key()
		a.transfers.CancelSession(key)
		a.downloads.CancelSession(key)
		a.downloads.CancelSession(key)
		for _, sf := range a.registry.ListSession(key) {
			a.registry.Remove(sf.ID)
		}
		a.sessions.RevokeSession(s)
	}
	if a.sessions.Len() == 0 {
		a.registry.Clear()
	}
}

// ---- Bound methods: sessions ----

// StartSession creates a new pairing session reachable on the requested
// interface and starts (or reuses) the HTTP server as needed.
//
// The ip argument must be the machine's own IPv4 address on the network
// the phone will connect through (as offered by ListNetworkInterfaces);
// an empty ip auto-selects when exactly one usable interface exists and
// fails with a choose-one error when several do. The selection is
// revalidated against live interface state, so a hotspot disconnect or a
// DHCP address change stops a stale address from starting a session the
// phone could never reach. The returned view carries the raw token for
// the QR flow plus the interface and address the QR encodes; nothing else
// ever sees the token.
func (a *App) StartSession(ip string) (SessionView, error) {
	if a.ctx == nil {
		return SessionView{}, errors.New("flitnix: the app is still starting")
	}
	view, err := a.resolveInterface(ip)
	if err != nil {
		return SessionView{}, err
	}
	listen, err := netenv.ListenAddr(view.IP, server.DefaultPort)
	if err != nil {
		return SessionView{}, fmt.Errorf("flitnix: cannot form the session address: %w", err)
	}
	if err := a.ensureServer(listen); err != nil {
		return SessionView{}, err
	}
	s, err := a.sessions.Create()
	if err != nil {
		// No session was created: keep the no-session-without-server
		// invariant unless another session is still active.
		if a.sessions.Len() == 0 {
			a.stopServer()
		}
		return SessionView{}, err
	}
	out := sessionView(s, s.Token())
	out.InterfaceName = view.Name
	out.Address = view.IP
	return out, nil
}

// resolveInterface turns the user's (possibly empty) selection into a
// revalidated interface view. The call must happen immediately before the
// server binds: a network change between choosing and starting must fail
// loudly instead of producing an unreachable QR.
func (a *App) resolveInterface(ip string) (netenv.View, error) {
	views, err := netenv.Enumerate(a.source)
	if err != nil {
		return netenv.View{}, fmt.Errorf("flitnix: could not read the network interfaces: %w", err)
	}
	if ip == "" {
		view, pickErr := netenv.PickDefault(views)
		switch {
		case errors.Is(pickErr, netenv.ErrAmbiguous):
			return netenv.View{}, fmt.Errorf("flitnix: choose which network interface connects to the phone: %w", pickErr)
		case pickErr != nil:
			return netenv.View{}, fmt.Errorf("flitnix: %w", pickErr)
		}
		return view, nil
	}
	view, pickErr := netenv.InterfaceFor(a.source, ip)
	switch {
	case errors.Is(pickErr, netenv.ErrGone):
		return netenv.View{}, fmt.Errorf("flitnix: the selected network is no longer reachable: %w", pickErr)
	case pickErr != nil:
		return netenv.View{}, fmt.Errorf("flitnix: %w", pickErr)
	}
	return view, nil
}

// ListNetworkInterfaces returns the interface addresses a session may
// bind: real IPv4 on running non-loopback, non-point-to-point interfaces.
// Read-only: nothing touches firewall configuration or system state.
func (a *App) ListNetworkInterfaces() ([]netenv.View, error) {
	views, err := netenv.Enumerate(a.source)
	if err != nil {
		return nil, fmt.Errorf("flitnix: could not read the network interfaces: %w", err)
	}
	return views, nil
}

// ServerAddress returns the running server's listening address as
// "host:port", or an empty string when no server is running. It is the
// base-URL part (host:port only) the desktop shows next to the QR code;
// the session token never appears in any view but the QR itself.
func (a *App) ServerAddress() string {
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	if a.srv == nil {
		return ""
	}
	return a.srv.Addr().String()
}

// SessionQR renders the pairing QR code for the active session with the
// given key, as a PNG data URL for the desktop <img>. The payload is the
// bound server address plus the session token in its URL fragment; the
// token appears only inside this method and StartSession and is never
// logged. It must be called after the server is visibly ready: no QR is
// ever rendered for a server that cannot accept connections.
func (a *App) SessionQR(key string) (string, error) {
	a.srvMu.Lock()
	srv := a.srv
	a.srvMu.Unlock()
	if srv == nil {
		return "", errors.New("flitnix: no session is running; start one first")
	}
	var token string
	for _, s := range a.sessions.ActiveSessions() {
		if s.Key() == key {
			token = s.Token()
			break
		}
	}
	if token == "" {
		return "", errors.New("flitnix: no active session matches that identifier")
	}
	payload, err := qr.Payload(srv.URL(), token)
	if err != nil {
		return "", fmt.Errorf("flitnix: %w", err)
	}
	png, err := qr.PNG(payload, qr.DefaultSize)
	if err != nil {
		return "", fmt.Errorf("flitnix: rendering the session QR: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// EndSession revokes the session identified by its non-secret key: its
// uploads are canceled, its shared files unlisted and the token dies. When
// the last session ends, all remaining shared files are forgotten and the
// HTTP server stops.
func (a *App) EndSession(key string) error {
	var target *session.Session
	for _, s := range a.sessions.ActiveSessions() {
		if s.Key() == key {
			target = s
			break
		}
	}
	if target == nil {
		return errors.New("flitnix: no active session matches that identifier")
	}
	a.transfers.CancelSession(key)
	for _, sf := range a.registry.ListSession(key) {
		a.registry.Remove(sf.ID)
	}
	a.sessions.RevokeSession(target)
	if a.sessions.Len() == 0 {
		a.registry.Clear()
		a.stopServer()
	}
	return nil
}

// ListSessions returns the active sessions. The raw token is never part of
// a listing.
func (a *App) ListSessions() []SessionView {
	actives := a.sessions.ActiveSessions()
	views := make([]SessionView, 0, len(actives))
	for _, s := range actives {
		views = append(views, sessionView(s, ""))
	}
	return views
}

// ---- Bound methods: incoming uploads ----

// ListPendingUploads returns the uploads awaiting the desktop's decision.
func (a *App) ListPendingUploads() []UploadView {
	return uploadViews(a.transfers.ListPending())
}

// RecentUploads returns every live and recently finished upload, oldest
// first. The UI polls this to follow transfer progress.
func (a *App) RecentUploads() []UploadView {
	return uploadViews(a.transfers.List())
}

func (a *App) RecentDownloads() []DownloadView {
	return downloadViews(a.downloads.List())
}

// ApproveUpload accepts the pending upload with the given ID. The phone
// may then stream exactly its approved content.
func (a *App) ApproveUpload(id string) (UploadView, error) {
	up, err := a.transfers.Decide(id, true)
	if err != nil {
		return UploadView{}, err
	}
	return uploadView(up), nil
}

// RejectUpload refuses the pending upload with the given ID.
func (a *App) RejectUpload(id string) (UploadView, error) {
	up, err := a.transfers.Decide(id, false)
	if err != nil {
		return UploadView{}, err
	}
	return uploadView(up), nil
}

// CancelUpload aborts a pending, approved or streaming upload. A streaming
// receive file is closed immediately and the record ends canceled.
func (a *App) CancelUpload(id string) error {
	return a.transfers.Cancel(id)
}

// ---- Bound methods: shared files ----

// PickFilesForSharing opens the native multi-file picker and registers the
// selected regular files as shareable. Cancelling the dialog returns an
// empty list with no error.
func (a *App) PickFilesForSharing() ([]SharedFileView, error) {
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Choose files to share",
	})
	// Some platforms report a cancelled file dialog as an error; selection
	// of nothing is not a failure of the user's intent, so both paths yield
	// an empty, successful result.
	if err != nil || len(paths) == 0 {
		return []SharedFileView{}, nil
	}
	views := make([]SharedFileView, 0, len(paths))
	for _, p := range paths {
		sf, addErr := a.registry.Add(p, filepath.Base(p), "")
		if addErr != nil {
			// Keep what succeeded so far; tell the user about the file
			// that could not be registered.
			return views, fmt.Errorf("%s: %w", filepath.Base(p), addErr)
		}
		views = append(views, sharedFileView(sf))
	}
	return views, nil
}

// ListSharedFiles returns the files currently shared with phones.
func (a *App) ListSharedFiles() []SharedFileView {
	sfs := a.registry.List()
	views := make([]SharedFileView, 0, len(sfs))
	for _, sf := range sfs {
		views = append(views, sharedFileView(sf))
	}
	return views
}

// RevokeSharedFile unlists the shared file with the given ID. The file on
// disk is never touched.
func (a *App) RevokeSharedFile(id string) error {
	if !a.registry.Remove(id) {
		return errors.New("flitnix: that file is no longer shared")
	}
	return nil
}

// ReceiveDirectory returns the absolute receive directory for received
// files.
func (a *App) ReceiveDirectory() string { return a.store.Dir() }

// ---- Projections ----

func sessionView(s *session.Session, token string) SessionView {
	return SessionView{
		Key:             s.Key(),
		Created:         s.Created(),
		ExpiresAt:       s.ExpiresAt(),
		PinnedTransfers: s.PinnedTransfers(),
		Token:           token,
	}
}

func uploadView(u transfer.Upload) UploadView {
	return UploadView{
		ID:       u.ID,
		Name:     u.Name,
		Size:     u.Size,
		Created:  u.Created,
		State:    string(u.State),
		Uploaded: u.Uploaded,
		Speed:    u.Speed,
		Reason:   u.Reason,
	}
}

func uploadViews(list []transfer.Upload) []UploadView {
	out := make([]UploadView, 0, len(list))
	for _, u := range list {
		out = append(out, uploadView(u))
	}
	return out
}

func sharedFileView(sf transfer.SharedFile) SharedFileView {
	return SharedFileView{
		ID:      sf.ID,
		Name:    sf.Name,
		Size:    sf.Size,
		Created: sf.Created,
	}
}

func downloadView(d transfer.Download) DownloadView {
	return DownloadView{
		ID: d.ID, FileID: d.FileID, Name: d.Name, Size: d.Size,
		Created: d.Created, State: string(d.State), Written: d.Written,
		Speed: d.Speed, Reason: d.Reason,
	}
}

func downloadViews(list []transfer.Download) []DownloadView {
	out := make([]DownloadView, 0, len(list))
	for _, d := range list {
		out = append(out, downloadView(d))
	}
	return out
}
