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
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	ID       string    `json:"id"`
	FileID   string    `json:"fileId"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	State    string    `json:"state"`
	Written  int64     `json:"written"`
	Speed    float64   `json:"speed"`
	Reason   string    `json:"reason,omitempty"`
}

// SharedFileView is the UI projection of one file currently shared with
// phones. The local filesystem path is deliberately not exposed.
type SharedFileView struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

// SharedSourceView describes a desktop selection without exposing its local
// path. Folder sources are prepared for a later archive-transfer phase and
// are not yet listed as downloadable phone files.
type SharedSourceView struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	RootName   string    `json:"rootName,omitempty"`
	ItemCount  int       `json:"itemCount"`
	TotalBytes int64     `json:"totalBytes"`
	Created    time.Time `json:"created"`
}

type BatchItemView struct {
	ID           string `json:"id"`
	BatchID      string `json:"batchId"`
	Sequence     int    `json:"sequence"`
	RelativePath string `json:"relativePath,omitempty"`
	DisplayName  string `json:"displayName"`
	DeclaredSize int64  `json:"declaredSize"`
	Transferred  int64  `json:"transferred"`
	State        string `json:"state"`
	SHA256       string `json:"sha256,omitempty"`
	Error        string `json:"error,omitempty"`
}

type BatchView struct {
	ID                 string          `json:"id"`
	Direction          string          `json:"direction"`
	Kind               string          `json:"kind"`
	RootName           string          `json:"rootName,omitempty"`
	Items              []BatchItemView `json:"items"`
	TotalItemCount     int             `json:"totalItemCount"`
	DeclaredTotalBytes int64           `json:"declaredTotalBytes"`
	TransferredBytes   int64           `json:"transferredBytes"`
	State              string          `json:"state"`
	ApprovalState      string          `json:"approvalState"`
	CreatedAt          time.Time       `json:"createdAt"`
	StartedAt          time.Time       `json:"startedAt,omitempty"`
	CompletedAt        time.Time       `json:"completedAt,omitempty"`
	Error              string          `json:"error,omitempty"`
}

// App owns the Flitnix core instances and the desktop lifecycle.
type App struct {
	ctx context.Context

	store       *transfer.Store
	sessions    *session.Manager
	transfers   *transfer.Service
	coordinator *transfer.Coordinator
	registry    *transfer.Registry
	downloads   *transfer.DownloadRegistry

	// source supplies live network-interface state for the session
	// address decisions; tests inject synthetic interfaces.
	source netenv.Provider

	// batchMu serializes batch decisions (approve, reject, cancel) so the
	// coordinator and the upload service always move together and a repeated
	// or concurrent decision observes a settled state.
	batchMu sync.Mutex

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
	a := &App{
		store:       store,
		transfers:   transfer.NewService(transfer.Config{Store: store}),
		coordinator: transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		registry:    transfer.NewRegistry(),
		downloads:   transfer.NewDownloadRegistry(),
		source:      netenv.DefaultSource(),
	}
	a.sessions = session.NewManager(session.Config{OnExpire: a.releaseSession, OnEnd: a.logSessionEnd})
	return a, nil
}

// releaseSession frees everything a session owned once it can no longer be
// used. EndSession does the same for explicit endings; this covers idle
// expiry, which previously left the session's pending and approved uploads
// and its batches behind. A session with a live transfer stream is pinned and
// never expires, so this can never fail a transfer that is making progress.
//
// It deliberately does not take batchMu: it runs from inside session-manager
// calls that batch decisions themselves make (finding a batch's session), and
// every step here is idempotent and safe against a concurrent decision, which
// fails closed and withdraws its own approvals.
func (a *App) releaseSession(key string) {
	a.transfers.CancelSession(key)
	_ = a.coordinator.CloseSession(key)
	a.discardEmptyRoots(key)
	a.downloads.CancelSession(key)
	for _, sf := range a.registry.ListSession(key) {
		a.registry.Remove(sf.ID)
	}
	for _, source := range a.registry.ListSourceSession(key) {
		a.registry.RemoveSource(source.ID)
	}
}

// startup records the Wails runtime context so bound methods can call the
// runtime (native file dialogs).
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// shortKey returns a bounded prefix of a session's non-secret key for log
// lines. Full keys are 64 hex characters; eight identify which session a
// line is about without resembling any secret (tokens are unrelated
// values), and nothing longer than the prefix is ever logged.
func shortKey(key string) string {
	if len(key) > 8 {
		return key[:8]
	}
	return key
}

// logSessionEnd records why a pairing session ended — idle expiry, revocation
// or shutdown — using the manager's OnEnd diagnostics. Unknown end reasons
// used to be reported only as "Session ended" on the phone; this line is the
// desktop-side counterpart that names the cause. Log contents stay within
// the privacy rules: a non-secret key prefix and a reason category, never
// tokens, pairing URLs, cookies or paths.
func (a *App) logSessionEnd(ev session.EndEvent) {
	fmt.Fprintf(os.Stderr, "flitnix: session %s ended: %s\n", shortKey(ev.Key), ev.Reason)
}

// logTransferDiagnostics is the server's Diagnose sink: the low-volume
// transfer-lifecycle lines (item claim, quarter marks, stream ending with
// its classified reason) go to stderr. The server guarantees opaque short
// identifiers; the format here only adds the stderr prefix.
func (a *App) logTransferDiagnostics(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "flitnix: "+format+"\n", args...)
}

// shutdown tears everything down in a safe order: cancel each session's
// in-flight or approved-but-unstreamed uploads, revoke every session,
// forget shared files and stop the HTTP server. Any upload streamed at that
// moment ends canceled (never half-published), matching the core's
// shutdown contract.
func (a *App) shutdown(ctx context.Context) {
	fmt.Fprintf(os.Stderr, "flitnix: the desktop is shutting down; ending every session: shutdown\n")
	for _, s := range a.sessions.ActiveSessions() {
		a.transfers.CancelSession(s.Key())
		_ = a.coordinator.CloseSession(s.Key())
		a.discardEmptyRoots(s.Key())
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
		fmt.Fprintf(os.Stderr, "flitnix: the listen address changed; the existing sessions cannot stay reachable: pairing_replacement\n")
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
			Sessions:    a.sessions,
			Transfers:   a.transfers,
			Coordinator: a.coordinator,
			Registry:    a.registry,
			Downloads:   a.downloads,
			Diagnose:    a.logTransferDiagnostics,
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
		_ = a.coordinator.CloseSession(key)
		a.downloads.CancelSession(key)
		for _, sf := range a.registry.ListSession(key) {
			a.registry.Remove(sf.ID)
		}
		for _, source := range a.registry.ListSourceSession(key) {
			a.registry.RemoveSource(source.ID)
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
// uploads are canceled, its held download records are canceled with it, its
// shared files unlisted and the token dies. When the last session ends, all
// remaining shared files are forgotten and the HTTP server stops.
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
	fmt.Fprintf(os.Stderr, "flitnix: session %s ending at the user's request: explicit_stop\n", shortKey(key))
	a.transfers.CancelSession(key)
	_ = a.coordinator.CloseSession(key)
	a.discardEmptyRoots(key)
	a.downloads.CancelSession(key)
	for _, sf := range a.registry.ListSession(key) {
		a.registry.Remove(sf.ID)
	}
	for _, source := range a.registry.ListSourceSession(key) {
		a.registry.RemoveSource(source.ID)
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

// RecentUploads returns every live and recently finished standalone upload,
// oldest first. The UI polls this to follow transfer progress. Batch items
// are intentionally absent: the batch coordinator is their only authority and
// they are shown through ListBatches, so no record is displayed twice with
// possibly diverging state.
func (a *App) RecentUploads() []UploadView {
	all := a.transfers.List()
	own := all[:0:0]
	for _, u := range all {
		if !u.BatchLinked {
			own = append(own, u)
		}
	}
	return uploadViews(own)
}

func (a *App) RecentDownloads() []DownloadView {
	return downloadViews(a.downloads.List())
}

// ApproveUpload accepts the pending upload with the given ID. The phone
// may then stream exactly its approved content.
func (a *App) ApproveUpload(id string) (UploadView, error) {
	if err := a.requireStandalone(id); err != nil {
		return UploadView{}, err
	}
	up, err := a.transfers.Decide(id, true)
	if err != nil {
		return UploadView{}, err
	}
	return uploadView(up), nil
}

// RejectUpload refuses the pending upload with the given ID.
func (a *App) RejectUpload(id string) (UploadView, error) {
	if err := a.requireStandalone(id); err != nil {
		return UploadView{}, err
	}
	up, err := a.transfers.Decide(id, false)
	if err != nil {
		return UploadView{}, err
	}
	return uploadView(up), nil
}

// CancelUpload aborts a pending, approved or streaming upload. A streaming
// receive file is closed immediately and the record ends canceled.
func (a *App) CancelUpload(id string) error {
	if err := a.requireStandalone(id); err != nil {
		return err
	}
	return a.transfers.Cancel(id)
}

// errBatchLinked refuses an individual decision on a batch item.
var errBatchLinked = errors.New("flitnix: this file belongs to a batch; decide on the batch instead")

// requireStandalone refuses the legacy single-upload surface for a record the
// batch coordinator owns, so one record never has two approval authorities.
func (a *App) requireStandalone(id string) error {
	if u, ok := a.transfers.Get(id); ok && u.BatchLinked {
		return errBatchLinked
	}
	return nil
}

// ListPendingBatches returns only batches still awaiting the desktop's
// decision. A batch leaves this list the moment it is approved, rejected,
// canceled or ended with its session.
func (a *App) ListPendingBatches() []BatchView {
	return a.listBatches(func(v transfer.BatchView) bool {
		return v.ApprovalState == transfer.ApprovalPending && v.State == transfer.BatchStatePending
	})
}

func (a *App) ListBatches() []BatchView {
	return a.listBatches(func(transfer.BatchView) bool { return true })
}

func (a *App) GetBatch(id string) (BatchView, error) {
	key, err := a.findBatchSession(id)
	if err != nil {
		return BatchView{}, err
	}
	v, err := a.coordinator.GetBatch(key, id)
	return batchView(v), err
}

// batchContentIDs returns the upload-service record of every item.
func (a *App) batchContentIDs(key string, v transfer.BatchView) ([]string, error) {
	ids := make([]string, 0, len(v.Items))
	for _, item := range v.Items {
		id, err := a.coordinator.ItemContentID(key, v.ID, item.ID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// batchApprovableContentIDs returns the upload records that still await the
// desktop decision. Items the phone already withdrew are skipped: a canceled
// record must not make the whole batch decision fail and strand the items that
// are still eligible.
func (a *App) batchApprovableContentIDs(key string, v transfer.BatchView) ([]string, error) {
	ids := make([]string, 0, len(v.Items))
	for _, item := range v.Items {
		if item.State != transfer.ItemStatePending {
			continue
		}
		id, err := a.coordinator.ItemContentID(key, v.ID, item.ID)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// cancelContent cancels the upload records of a batch whose fate was decided
// elsewhere. Canceling a record that already finished is a no-op.
func (a *App) cancelContent(ids []string) {
	for _, id := range ids {
		_ = a.transfers.Cancel(id)
	}
}

// discardEmptyRoot removes the receive directory a folder batch reserved for
// itself when that batch will never publish into it. AllocateFolderRoot
// creates the root with mkdir before the item records exist (that is what
// makes the reservation atomic and collision-safe), so a batch that is
// rejected, canceled or ended with its session used to leave an empty
// directory named after the sender's folder in the user's receive directory
// forever — the same abandoned-root leak that batch-creation failure already
// cleans up, one state later.
//
// os.Remove refuses a non-empty directory, so a root that received even one
// published file is left untouched; roots are reserved uniquely per batch, so
// this can never remove a directory another batch owns. A root that is
// already gone is not an error.
func (a *App) discardEmptyRoot(v transfer.BatchView) {
	if v.Kind != transfer.BatchKindFolder || v.RootName == "" {
		return
	}
	_ = a.store.DiscardFolderRoot(v.RootName)
}

// discardEmptyRoots applies discardEmptyRoot to every folder batch a session
// ever owned. It is the session-ending counterpart of discardEmptyRoot and
// must run after CloseSession, because a closed session's records are only
// reachable through the read-only history projection.
func (a *App) discardEmptyRoots(key string) {
	batches, ok := a.coordinator.SessionHistory(key)
	if !ok {
		return
	}
	for _, v := range batches {
		a.discardEmptyRoot(v)
	}
}

// ApproveBatch approves a pending batch exactly once. The batch is the only
// approval authority for its items: their upload records are decided together,
// atomically, and only then does the coordinator release the batch to the
// phone's queue. Repeating the call for a batch that is already approved is a
// no-op that returns the current state; approving a rejected or canceled
// batch is refused.
func (a *App) ApproveBatch(id string) (BatchView, error) {
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	key, err := a.findBatchSession(id)
	if err != nil {
		return BatchView{}, err
	}
	v, err := a.coordinator.GetBatch(key, id)
	if err != nil {
		return BatchView{}, err
	}
	switch {
	case v.ApprovalState == transfer.ApprovalApproved &&
		v.State != transfer.BatchStateCanceled && v.State != transfer.BatchStateRejected:
		return batchView(v), nil
	case v.ApprovalState != transfer.ApprovalPending || v.State != transfer.BatchStatePending:
		return BatchView{}, fmt.Errorf("%w: the batch is %s and can no longer be approved",
			transfer.ErrInvalidTransition, v.State)
	}
	ids, err := a.batchApprovableContentIDs(key, v)
	if err != nil {
		return BatchView{}, err
	}
	if len(ids) == 0 {
		// Every item was withdrawn before the decision; the coordinator has
		// already finished the batch, so there is nothing to approve.
		return BatchView{}, fmt.Errorf("%w: the batch has no items left to approve",
			transfer.ErrInvalidTransition)
	}
	if err := a.transfers.DecideBatchItems(ids, true); err != nil {
		// Some record was not pending (for example, it was canceled with its
		// session). Nothing was approved, and a batch that cannot be released
		// whole must not stay pending, so close it out in both subsystems.
		a.cancelContent(ids)
		_, _ = a.coordinator.CancelBatch(key, id)
		return BatchView{}, err
	}
	if _, err := a.coordinator.ApproveBatch(key, id); err != nil {
		// The coordinator refused (its state moved on, e.g. the session
		// closed); withdraw the upload-service approvals to match.
		a.cancelContent(ids)
		return BatchView{}, err
	}
	v, err = a.coordinator.GetBatch(key, id)
	return batchView(v), err
}

// RejectBatch declines a batch. Rejecting an already rejected batch returns
// its state; a completed or canceled batch cannot be rejected.
func (a *App) RejectBatch(id string) (BatchView, error) {
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	key, err := a.findBatchSession(id)
	if err != nil {
		return BatchView{}, err
	}
	before, err := a.coordinator.GetBatch(key, id)
	if err != nil {
		return BatchView{}, err
	}
	if before.State == transfer.BatchStateRejected {
		return batchView(before), nil
	}
	ids, idErr := a.batchContentIDs(key, before)
	v, err := a.coordinator.RejectBatch(key, id)
	if err != nil {
		return BatchView{}, err
	}
	if idErr == nil {
		// Pending records take the rejected state; anything already decided
		// or streaming is canceled below.
		_ = a.transfers.DecideBatchItems(ids, false)
		a.cancelContent(ids)
	}
	// The root was reserved before the desktop decided; a rejected batch
	// never publishes into it, so it must not stay on disk.
	a.discardEmptyRoot(v)
	return batchView(v), nil
}

// CancelBatch cancels every unfinished item of a batch in both subsystems.
func (a *App) CancelBatch(id string) error {
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	key, err := a.findBatchSession(id)
	if err != nil {
		return err
	}
	v, err := a.coordinator.CancelBatch(key, id)
	if err != nil {
		return err
	}
	if ids, idErr := a.batchContentIDs(key, v); idErr == nil {
		a.cancelContent(ids)
	}
	// Streams are aborted above, so a folder root that received nothing is
	// now empty and can be removed; one that published a file is non-empty
	// and os.Remove refuses it.
	a.discardEmptyRoot(v)
	return nil
}

// listBatches merges the coordinator's batch records over every session
// the coordinator has ever tracked — including sessions that already
// ended — and projects the ones passing filter. A session ending used to
// hide its completed, failed and canceled batches from the desktop; their
// terminal history now stays visible through SessionHistory's closed-session
// fallback, and the merged list is ordered oldest-first (with ID breaking
// equal timestamps) once across all sessions.
//
// Closed sessions are read-only for projections. Decisions keep failing
// closed: findBatchSession walks active sessions only, so a decision can
// never attach to a batch whose session has ended.
func (a *App) listBatches(filter func(transfer.BatchView) bool) []BatchView {
	// The slice is never nil: the bound methods marshal an empty slice to JS
	// as [], while a nil one becomes null and the UI dereferences .length and
	// .filter on it, which would abort every Svelte render flush and freeze
	// the desktop panel numbers mid-transfer.
	out := make([]BatchView, 0)
	seen := make(map[string]struct{})
	for _, key := range a.coordinator.SessionKeys() {
		items, err := a.coordinator.ListBatches(key)
		switch {
		case err == nil:
		case errors.Is(err, transfer.ErrSessionClosed):
			history, ok := a.coordinator.SessionHistory(key)
			if !ok {
				continue
			}
			items = history
		default:
			continue
		}
		for _, item := range items {
			if _, dup := seen[item.ID]; dup {
				continue
			}
			seen[item.ID] = struct{}{}
			if filter(item) {
				out = append(out, batchView(item))
			}
		}
	}
	sortBatchViews(out)
	return out
}

// sortBatchViews orders batch views oldest-first, breaking equal timestamps
// by ID so repeatably-ordered projection never depends on map iteration.
func sortBatchViews(views []BatchView) {
	sort.Slice(views, func(i, j int) bool {
		if !views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].CreatedAt.Before(views[j].CreatedAt)
		}
		return views[i].ID < views[j].ID
	})
}

func (a *App) findBatchSession(id string) (string, error) {
	for _, sess := range a.sessions.ActiveSessions() {
		if _, err := a.coordinator.GetBatch(sess.Key(), id); err == nil {
			return sess.Key(), nil
		}
	}
	return "", errors.New("flitnix: batch not found")
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
		source, addErr := a.registry.AddFileSource(p, filepath.Base(p), "")
		if addErr != nil {
			// Keep what succeeded so far; tell the user about the file
			// that could not be registered.
			return views, fmt.Errorf("%s: %w", filepath.Base(p), addErr)
		}
		views = append(views, SharedFileView{
			ID: source.ID, Name: source.Name, Size: source.Items[0].Size,
			Created: source.Created,
		})
	}
	return views, nil
}

// PickFolderForSharing opens the native directory picker and prepares one
// folder source. The source stays desktop-private until archive downloads are
// implemented. Canceling leaves existing selections unchanged.
func (a *App) PickFolderForSharing() (SharedSourceView, error) {
	path, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Choose a folder to share",
	})
	if err != nil || path == "" {
		return SharedSourceView{}, nil
	}
	source, err := a.registry.AddFolder(path, "")
	if err != nil {
		return SharedSourceView{}, err
	}
	return sharedSourceView(source), nil
}

// AddDroppedSources registers native paths delivered by Wails file-drop
// integration. Each path is handled independently so one invalid drop does
// not remove or invalidate existing selections.
func (a *App) AddDroppedSources(paths []string) ([]SharedSourceView, error) {
	views := make([]SharedSourceView, 0, len(paths))
	var failures []string
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			failures = append(failures, filepath.Base(path)+": "+err.Error())
			continue
		}
		var source transfer.SharedSource
		if info.IsDir() {
			source, err = a.registry.AddFolder(path, "")
		} else {
			source, err = a.registry.AddFileSource(path, filepath.Base(path), "")
		}
		if err != nil {
			failures = append(failures, filepath.Base(path)+": "+err.Error())
			continue
		}
		views = append(views, sharedSourceView(source))
	}
	if len(failures) > 0 {
		return views, fmt.Errorf("could not add dropped source(s): %s", strings.Join(failures, "; "))
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

// ListSharedSources returns desktop-private source selections. It is
// intentionally separate from ListSharedFiles until Phase 5 adds archive
// downloads for folders and collections.
func (a *App) ListSharedSources() []SharedSourceView {
	sources := a.registry.ListSources()
	views := make([]SharedSourceView, 0, len(sources))
	for _, source := range sources {
		views = append(views, sharedSourceView(source))
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

// RevokeSharedSource removes only the selected desktop source.
func (a *App) RevokeSharedSource(id string) error {
	if !a.registry.RemoveSource(id) {
		return errors.New("flitnix: that source is no longer selected")
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

func sharedSourceView(source transfer.SharedSource) SharedSourceView {
	var total int64
	for _, item := range source.Items {
		total += item.Size
	}
	return SharedSourceView{
		ID: source.ID, Kind: string(source.Kind), Name: source.Name,
		RootName: source.RootName, ItemCount: len(source.Items),
		TotalBytes: total, Created: source.Created,
	}
}

func batchView(b transfer.BatchView) BatchView {
	items := make([]BatchItemView, 0, len(b.Items))
	for _, item := range b.Items {
		items = append(items, BatchItemView{
			ID: item.ID, BatchID: item.BatchID, Sequence: item.Sequence,
			RelativePath: item.RelativePath, DisplayName: item.DisplayName,
			DeclaredSize: item.DeclaredSize, Transferred: item.Transferred,
			State: string(item.State), SHA256: item.SHA256, Error: item.Error,
		})
	}
	return BatchView{
		ID: b.ID, Direction: string(b.Direction), Kind: string(b.Kind),
		RootName: b.RootName, Items: items, TotalItemCount: b.TotalItemCount,
		DeclaredTotalBytes: b.DeclaredTotalBytes, TransferredBytes: b.TransferredBytes,
		State: string(b.State), ApprovalState: string(b.ApprovalState),
		CreatedAt: b.CreatedAt, StartedAt: b.StartedAt, CompletedAt: b.CompletedAt,
		Error: b.Error,
	}
}

func downloadView(d transfer.Download) DownloadView {
	return DownloadView{
		ID: d.ID, FileID: d.FileID, Name: d.Name, Kind: d.Kind, Size: d.Size,
		Created: d.Created, Started: d.Started, Finished: d.Finished,
		State: string(d.State), Written: d.Written,
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
