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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	stdlibRuntime "runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/audit"
	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/doctor"
	"github.com/itsyamin/flitnix/internal/guest"
	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/qr"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/settings"
	"github.com/itsyamin/flitnix/internal/textmsg"
	"github.com/itsyamin/flitnix/internal/transfer"
	"github.com/wailsapp/wails/v2/pkg/options"
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

type GuestRoomView struct {
	ID              string                      `json:"id"`
	Date            string                      `json:"date"`
	DestinationRoot string                      `json:"destinationRoot"`
	RoomFolder      string                      `json:"roomFolder"`
	Title           string                      `json:"title"`
	Description     string                      `json:"description,omitempty"`
	Deadline        *time.Time                  `json:"deadline,omitempty"`
	CollisionPolicy string                      `json:"collisionPolicy"`
	State           string                      `json:"state"`
	DurationSeconds int                         `json:"durationSeconds"`
	CreatedAt       time.Time                   `json:"createdAt"`
	ExpiresAt       time.Time                   `json:"expiresAt"`
	InviteSecret    string                      `json:"inviteSecret,omitempty"`
	InviteURL       string                      `json:"inviteUrl,omitempty"`
	QRDataURL       string                      `json:"qrDataUrl,omitempty"`
	FormSchema      []guest.FormField           `json:"formSchema,omitempty"`
	Roster          []guest.ExpectedParticipant `json:"roster,omitempty"`
	HasRoster       bool                        `json:"hasRoster"`
}

type GuestRoomConfigView struct {
	DestinationDir       string                      `json:"destinationDir"`
	DurationMinutes      int                         `json:"durationMinutes"`
	Title                string                      `json:"title"`
	Description          string                      `json:"description,omitempty"`
	DeadlineRFC3339      string                      `json:"deadlineRFC3339,omitempty"`
	CollisionPolicy      string                      `json:"collisionPolicy"` // "preserve_both" or "reject"
	FormSchema           []guest.FormField           `json:"formSchema,omitempty"`
	ExpectedParticipants []guest.ExpectedParticipant `json:"expectedParticipants,omitempty"`
}

type GuestView struct {
	ID            string    `json:"id"`
	Nickname      string    `json:"nickname"`
	ParticipantID string    `json:"participantId,omitempty"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"createdAt"`
}

type GuestUploadView struct {
	ID          string            `json:"id"`
	GuestID     string            `json:"guestId"`
	GuestName   string            `json:"guestName"`
	FileName    string            `json:"fileName"`
	Size        int64             `json:"size"`
	State       string            `json:"state"`
	Transferred int64             `json:"transferred"`
	Speed       float64           `json:"speed"`
	CreatedAt   time.Time         `json:"createdAt"`
	CompletedAt time.Time         `json:"completedAt,omitempty"`
	SHA256      string            `json:"sha256,omitempty"`
	Error       string            `json:"error,omitempty"`
	FormValues  map[string]string `json:"formValues,omitempty"`
}

type GuestRoomStateView struct {
	Room             *GuestRoomView              `json:"room"`
	PendingGuests    []GuestView                 `json:"pendingGuests"`
	ApprovedGuests   []GuestView                 `json:"approvedGuests"`
	PendingUploads   []GuestUploadView           `json:"pendingUploads"`
	CompletedUploads []GuestUploadView           `json:"completedUploads"`
	RecentUploads    []GuestUploadView           `json:"recentUploads"`
	Roster           []guest.ExpectedParticipant `json:"roster,omitempty"`
}

type AuditListResult struct {
	Entries []audit.Entry `json:"entries"`
	Total   int           `json:"total"`
}

type TextMessageView struct {
	ID        string    `json:"id"`
	Sender    string    `json:"sender"`
	Content   string    `json:"content"`
	IsLink    bool      `json:"isLink"`
	URL       string    `json:"url,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
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
	SHA256   string    `json:"sha256,omitempty"`
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
	SHA256   string    `json:"sha256,omitempty"`
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

// SettingsView is the UI projection of persistent user settings.
type SettingsView struct {
	Version            int    `json:"version"`
	Theme              string `json:"theme"`
	Locale             string `json:"locale"`
	DownloadDir        string `json:"downloadDir"`
	EffectiveDir       string `json:"effectiveDir"`
	AutoStartSession   bool   `json:"autoStartSession"`
	PreferredInterface string `json:"preferredInterface"`
}

// App owns the Flitnix core instances and the desktop lifecycle.
type App struct {
	ctx context.Context

	store         *transfer.Store
	settingsStore *settings.Store
	sessions      *session.Manager
	transfers     *transfer.Service
	coordinator   *transfer.Coordinator
	registry      *transfer.Registry
	downloads     *transfer.DownloadRegistry
	guestMgr      *guest.Manager
	textStore     *textmsg.Store
	auditStore    *audit.Store
	templateStore *guest.TemplateStore
	devicesStore  *devices.Store
	hostIdentity  *devices.HostIdentity
	pairing       *devices.PairingCoordinator
	startupMu     sync.Mutex
	startupDone   bool
	initialShares []string

	phoneContactMu   sync.Mutex
	lastPhoneContact *time.Time

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

	bgCancel     context.CancelFunc
	bgWg         sync.WaitGroup
	shutdownOnce sync.Once
}

func resolveDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return filepath.Join(os.TempDir(), "flitnix")
		}
		return filepath.Join(home, ".config", "flitnix")
	}
	return filepath.Join(dir, "flitnix")
}

// NewApp constructs the application: receive store configured from persistent
// settings (or default ~/Downloads/Flitnix), a fresh session manager, transfer
// service and shared-file registry. The HTTP server is not started here; it starts
// lazily when the first session is created and stops again when the last
// session ends.
func NewApp() (*App, error) {
	dataDir := resolveDataDir()
	settingsStore, err := settings.NewStore(filepath.Join(dataDir, "settings.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "flitnix: settings store initialization warning: %v\n", err)
		settingsStore, _ = settings.NewStore("")
	}
	initialDir := ""
	if settingsStore != nil {
		initialDir = settingsStore.Get().DownloadDir
	}
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: initialDir})
	if err != nil {
		return nil, err
	}
	auditStore, err := audit.NewStore(filepath.Join(dataDir, "audit.jsonl"), 10000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "flitnix: audit store initialization warning: %v\n", err)
	}
	templateStore, err := guest.NewTemplateStore(filepath.Join(dataDir, "templates.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "flitnix: template store initialization warning: %v\n", err)
	}
	devicesStore, err := devices.NewStore(filepath.Join(dataDir, "devices.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "flitnix: devices store initialization warning: %v\n", err)
	}
	hostIdentity, err := devices.LoadOrCreateHostIdentity(filepath.Join(dataDir, "host_identity.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "flitnix: host identity initialization warning: %v\n", err)
	}
	pairingCoord := devices.NewPairingCoordinator(hostIdentity, devicesStore)

	a := &App{
		store:         store,
		settingsStore: settingsStore,
		transfers:     transfer.NewService(transfer.Config{Store: store}),
		coordinator:   transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		registry:      transfer.NewRegistry(),
		downloads:     transfer.NewDownloadRegistry(),
		guestMgr:      guest.NewManager(),
		textStore:     textmsg.NewStore(),
		auditStore:    auditStore,
		templateStore: templateStore,
		devicesStore:  devicesStore,
		hostIdentity:  hostIdentity,
		pairing:       pairingCoord,
		source:        netenv.DefaultSource(),
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
	a.textStore.Clear(key)
}

// startup records the Wails runtime context so bound methods can call the
// runtime (native file dialogs).
func (a *App) startup(ctx context.Context) {
	a.startupMu.Lock()
	a.ctx = ctx
	initial := a.initialShares
	a.startupDone = true
	bgCtx, bgCancel := context.WithCancel(context.Background())
	a.bgCancel = bgCancel
	a.startupMu.Unlock()

	ensureLinuxNautilusScript()

	if len(initial) > 0 {
		_, _ = a.AddDroppedSources(initial)
	}

	if a.settingsStore != nil && a.settingsStore.Get().AutoStartSession {
		a.bgWg.Add(1)
		go func() {
			defer a.bgWg.Done()
			select {
			case <-bgCtx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			if a.sessions.Len() == 0 {
				_, _ = a.StartSession("")
			}
		}()
	}
}

// ensureLinuxNautilusScript ensures the "Share with Flitnix" Nautilus script
// is installed in ~/.local/share/nautilus/scripts if GNOME/Nautilus is present.
func ensureLinuxNautilusScript() {
	if stdlibRuntime.GOOS != "linux" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	scriptsDir := filepath.Join(home, ".local", "share", "nautilus", "scripts")
	nautilusDir := filepath.Join(home, ".local", "share", "nautilus")
	desktopEnv := strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP"))
	// Only auto-provision if Nautilus data directory exists or user is in a GNOME desktop session
	if _, err := os.Stat(nautilusDir); os.IsNotExist(err) && !strings.Contains(desktopEnv, "gnome") {
		return
	}
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return
	}
	scriptPath := filepath.Join(scriptsDir, "Share with Flitnix")
	if _, err := os.Stat(scriptPath); err == nil {
		return // already present
	}
	content := `#!/usr/bin/env bash
# Share with Flitnix - GNOME Files (Nautilus) script
# Passes selected files and folders, or current folder if none selected.

if [ "$#" -gt 0 ]; then
    exec flitnix "$@"
elif [ -n "${NAUTILUS_SCRIPT_SELECTED_FILE_PATHS:-}" ]; then
    mapfile -t paths <<< "${NAUTILUS_SCRIPT_SELECTED_FILE_PATHS}"
    exec flitnix "${paths[@]}"
elif [ -n "${NAUTILUS_SCRIPT_CURRENT_URI:-}" ]; then
    exec flitnix "${NAUTILUS_SCRIPT_CURRENT_URI}"
else
    exec flitnix
fi
`
	_ = os.WriteFile(scriptPath, []byte(content), 0o755)
}

// onSecondInstanceLaunch is invoked when a second instance of Flitnix launches.
// It extracts file and folder arguments from the second instance, normalizes them
// relative to the second instance's working directory, queues them into the share
// registry, and restores/shows the application window.
func (a *App) onSecondInstanceLaunch(data options.SecondInstanceData) {
	normPaths := ExtractSharePaths(data.Args, data.WorkingDirectory, func(raw string, err error) {
		fmt.Fprintf(os.Stderr, "flitnix: second-instance invalid path %q: %v\n", raw, err)
	})

	if len(normPaths) > 0 {
		a.startupMu.Lock()
		if !a.startupDone {
			a.initialShares = append(a.initialShares, normPaths...)
			a.startupMu.Unlock()
			a.bringToFront()
			return
		}
		a.startupMu.Unlock()

		_, _ = a.AddDroppedSources(normPaths)
	}

	a.bringToFront()
}

// onMacFileOpen is invoked when macOS opens files with the running application.
func (a *App) onMacFileOpen(filePath string) {
	normPaths := ExtractSharePaths([]string{filePath}, "", func(raw string, err error) {
		fmt.Fprintf(os.Stderr, "flitnix: mac open-file invalid path %q: %v\n", raw, err)
	})

	if len(normPaths) > 0 {
		a.startupMu.Lock()
		if !a.startupDone {
			a.initialShares = append(a.initialShares, normPaths...)
			a.startupMu.Unlock()
			a.bringToFront()
			return
		}
		a.startupMu.Unlock()

		_, _ = a.AddDroppedSources(normPaths)
	}

	a.bringToFront()
}

// bringToFront restores a minimized window and brings it to the foreground.
func (a *App) bringToFront() {
	a.startupMu.Lock()
	ctx := a.ctx
	a.startupMu.Unlock()

	if ctx != nil && ctx.Value("frontend") != nil {
		runtime.WindowUnminimise(ctx)
		runtime.WindowShow(ctx)
	}
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
	a.shutdownOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "flitnix: the desktop is shutting down; ending every session: shutdown\n")
		a.startupMu.Lock()
		if a.bgCancel != nil {
			a.bgCancel()
		}
		a.startupMu.Unlock()
		a.bgWg.Wait()

		for _, s := range a.sessions.ActiveSessions() {
			a.transfers.CancelSession(s.Key())
			_ = a.coordinator.CloseSession(s.Key())
			a.discardEmptyRoots(s.Key())
			a.downloads.CancelSession(s.Key())
		}
		a.sessions.Close()
		a.registry.Clear()
		a.guestMgr.Close()
		if a.auditStore != nil {
			a.auditStore.Close()
		}
		a.stopServer()
	})
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
			Sessions:       a.sessions,
			Transfers:      a.transfers,
			Coordinator:    a.coordinator,
			Registry:       a.registry,
			Downloads:      a.downloads,
			GuestManager:   a.guestMgr,
			TextMessages:   a.textStore,
			Audit:          a.auditStore,
			Devices:        a.devicesStore,
			HostIdentity:   a.hostIdentity,
			Pairing:        a.pairing,
			OnPhoneContact: a.recordPhoneContact,
			Diagnose:       a.logTransferDiagnostics,
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
		if a.settingsStore != nil {
			pref := a.settingsStore.Get().PreferredInterface
			if pref != "" {
				var match *netenv.View
				for _, v := range views {
					if v.Name == pref {
						if v.Family == "IPv4" || match == nil {
							vCopy := v
							match = &vCopy
						}
					}
				}
				if match != nil {
					return *match, nil
				}
				// If preferred interface is not currently available, fall back
				// safely to PickDefault below without hardcoded addresses or interfaces.
			}
		}
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
	if pickErr != nil {
		// If caller passed an interface name instead of an IP literal, match by name.
		for _, v := range views {
			if v.Name == ip {
				return v, nil
			}
		}
	}
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
		if !a.isGuestRoomActive() {
			a.stopServer()
		}
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
	if v.State == transfer.BatchStateTransferring {
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
	if before, bErr := a.coordinator.GetBatch(key, id); bErr == nil {
		for _, item := range before.Items {
			if item.State == transfer.ItemStateCompleted || item.State == transfer.ItemStateCanceled ||
				item.State == transfer.ItemStateRejected || item.State == transfer.ItemStateFailed {
				continue
			}
			if contentID, idErr := a.coordinator.ItemContentID(key, id, item.ID); idErr == nil && contentID != "" {
				_, _ = a.transfers.ReserveCancel(contentID)
			}
		}
	}
	v, err := a.coordinator.CancelBatch(key, id)
	if err != nil {
		return err
	}
	for _, item := range v.Items {
		if item.State != transfer.ItemStateCanceled {
			continue
		}
		if contentID, idErr := a.coordinator.ItemContentID(key, v.ID, item.ID); idErr == nil && contentID != "" {
			_ = a.transfers.Cancel(contentID)
		}
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
// integration or external launches. Each path is handled independently so one invalid drop does
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

// GetSettings returns the user settings and current effective download directory.
func (a *App) GetSettings() SettingsView {
	if a.settingsStore == nil {
		return SettingsView{
			Version:      settings.CurrentSchemaVersion,
			Theme:        settings.ThemeSystem,
			Locale:       "system",
			EffectiveDir: a.store.Dir(),
		}
	}
	s := a.settingsStore.Get()
	return SettingsView{
		Version:            s.Version,
		Theme:              s.Theme,
		Locale:             s.Locale,
		DownloadDir:        s.DownloadDir,
		EffectiveDir:       a.store.Dir(),
		AutoStartSession:   s.AutoStartSession,
		PreferredInterface: s.PreferredInterface,
	}
}

// hasActiveTransfers reports whether any transfers (standalone or batches) are currently
// in progress, transferring, or pending.
func (a *App) hasActiveTransfers() bool {
	if a.transfers != nil {
		counts := a.transfers.Counts()
		if counts.Uploading > 0 {
			return true
		}
		for _, u := range a.transfers.List() {
			if u.State == transfer.StateUploading || u.State == transfer.StateApproved {
				return true
			}
		}
	}
	if a.coordinator != nil {
		for _, key := range a.coordinator.SessionKeys() {
			batches, err := a.coordinator.ListBatches(key)
			if err == nil {
				for _, b := range batches {
					if b.State == transfer.BatchStateTransferring ||
						b.State == transfer.BatchStateQueued ||
						b.State == transfer.BatchStateApproved {
						return true
					}
				}
			}
		}
	}
	return false
}

// SaveSettings updates the persistent settings and applies them to active subsystems.
func (a *App) SaveSettings(view SettingsView) error {
	if a.settingsStore == nil {
		return errors.New("flitnix: settings store not available")
	}
	theme := strings.ToLower(strings.TrimSpace(view.Theme))
	if theme != "" && theme != settings.ThemeSystem && theme != settings.ThemeDark && theme != settings.ThemeLight {
		return settings.ErrInvalidTheme
	}
	if view.DownloadDir != "" {
		info, err := os.Stat(view.DownloadDir)
		if err == nil && !info.IsDir() {
			return errors.New("flitnix: download path cannot be a regular file")
		}
	}
	if a.settingsStore != nil {
		currentDir := a.settingsStore.Get().DownloadDir
		if view.DownloadDir != currentDir && a.hasActiveTransfers() {
			return errors.New("flitnix: cannot change receive directory while transfers are active")
		}
	}
	s := settings.Settings{
		Version:            view.Version,
		Theme:              view.Theme,
		Locale:             view.Locale,
		DownloadDir:        view.DownloadDir,
		AutoStartSession:   view.AutoStartSession,
		PreferredInterface: view.PreferredInterface,
	}
	if err := a.settingsStore.Save(s); err != nil {
		return err
	}
	if a.store != nil {
		if err := a.store.SetDir(s.DownloadDir); err != nil {
			return err
		}
	}
	return nil
}

// ResetSettings resets settings to baseline defaults and re-applies default paths.
func (a *App) ResetSettings() (SettingsView, error) {
	if a.settingsStore == nil {
		return SettingsView{}, errors.New("flitnix: settings store not available")
	}
	if a.hasActiveTransfers() {
		if a.settingsStore != nil && a.settingsStore.Get().DownloadDir != "" {
			return SettingsView{}, errors.New("flitnix: cannot reset receive directory while transfers are active")
		}
	}
	if _, err := a.settingsStore.Reset(); err != nil {
		return SettingsView{}, err
	}
	if a.store != nil {
		_ = a.store.SetDir("")
	}
	return a.GetSettings(), nil
}

// SelectDownloadDirectory opens the native directory dialog for choosing the download directory.
func (a *App) SelectDownloadDirectory() (string, error) {
	if a.ctx == nil {
		return "", errors.New("flitnix: application context not initialized")
	}
	defaultDir := ""
	if a.store != nil {
		defaultDir = a.store.Dir()
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Select Download Directory",
		DefaultDirectory: defaultDir,
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// OpenDownloadFolder opens the active download directory in the native file manager.
func (a *App) OpenDownloadFolder() error {
	dir := a.store.Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("flitnix: cannot create download directory: %w", err)
	}
	return openFolderInOS(dir)
}

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
		SHA256:   u.SHA256,
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
		Speed: d.Speed, SHA256: d.SHA256, Reason: d.Reason,
	}
}

func downloadViews(list []transfer.Download) []DownloadView {
	out := make([]DownloadView, 0, len(list))
	for _, d := range list {
		out = append(out, downloadView(d))
	}
	return out
}

// ---- Guest Drop Box methods ----

func (a *App) StartGuestRoom(destinationDir string, durationMinutes int) (GuestRoomView, error) {
	return a.StartGuestRoomWithConfig(GuestRoomConfigView{
		DestinationDir:  destinationDir,
		DurationMinutes: durationMinutes,
		CollisionPolicy: "preserve_both",
	})
}

func (a *App) StartGuestRoomWithConfig(cfg GuestRoomConfigView) (GuestRoomView, error) {
	if cfg.DestinationDir == "" {
		cfg.DestinationDir = a.store.Dir()
	}

	a.srvMu.Lock()
	var listen string
	if a.srv != nil && a.srv.Addr() != nil {
		listen = a.srv.Addr().String()
	}
	a.srvMu.Unlock()

	if listen == "" {
		ifaces, err := a.ListNetworkInterfaces()
		if err != nil || len(ifaces) == 0 {
			return GuestRoomView{}, errors.New("flitnix: no usable network interface found; connect to a network first")
		}
		addr, err := netenv.ListenAddr(ifaces[0].IP, server.DefaultPort)
		if err != nil {
			return GuestRoomView{}, fmt.Errorf("flitnix: cannot form the guest room address: %w", err)
		}
		listen = addr
	}

	if err := a.ensureServer(listen); err != nil {
		return GuestRoomView{}, err
	}

	duration := time.Duration(cfg.DurationMinutes) * time.Minute
	if duration <= 0 {
		duration = 1 * time.Hour
	}

	policy := guest.CollisionPolicy(cfg.CollisionPolicy)
	if policy != guest.CollisionReject {
		policy = guest.CollisionPreserveBoth
	}

	var deadline *time.Time
	if cfg.DeadlineRFC3339 != "" {
		parsed, err := time.Parse(time.RFC3339, cfg.DeadlineRFC3339)
		if err == nil {
			deadline = &parsed
		}
	}

	room, err := a.guestMgr.CreateRoomWithConfig(guest.RoomConfig{
		DestinationRoot:      cfg.DestinationDir,
		Duration:             duration,
		Title:                cfg.Title,
		Description:          cfg.Description,
		Deadline:             deadline,
		CollisionPolicy:      policy,
		FormSchema:           cfg.FormSchema,
		ExpectedParticipants: cfg.ExpectedParticipants,
	})
	if err != nil {
		return GuestRoomView{}, err
	}

	if a.auditStore != nil {
		a.auditStore.Record(audit.Entry{
			EventType: audit.EventRoomCreated,
			RoomID:    room.ID,
			Outcome:   audit.OutcomeSuccess,
			Details:   fmt.Sprintf("title: %s, policy: %s", room.Title, room.CollisionPolicy),
		})
	}

	a.srvMu.Lock()
	srvURL := a.srv.URL()
	a.srvMu.Unlock()

	inviteURL := fmt.Sprintf("%s/guest#%s", srvURL, room.InviteSecret)
	qrPNG, err := qr.PNG(inviteURL, qr.DefaultSize)
	var qrDataURL string
	if err == nil {
		qrDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(qrPNG)
	}

	v := room.View()
	return GuestRoomView{
		ID:              v.ID,
		Date:            v.Date,
		DestinationRoot: v.DestinationRoot,
		RoomFolder:      v.RoomFolder,
		Title:           v.Title,
		Description:     v.Description,
		Deadline:        v.Deadline,
		CollisionPolicy: string(v.CollisionPolicy),
		State:           string(v.State),
		DurationSeconds: v.DurationSeconds,
		CreatedAt:       v.CreatedAt,
		ExpiresAt:       v.ExpiresAt,
		InviteSecret:    v.InviteSecret,
		InviteURL:       inviteURL,
		QRDataURL:       qrDataURL,
		FormSchema:      v.FormSchema,
		Roster:          v.Roster,
		HasRoster:       v.HasRoster,
	}, nil
}

func (a *App) GetGuestRoomState() (GuestRoomStateView, error) {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return GuestRoomStateView{}, nil
	}

	a.srvMu.Lock()
	var srvURL string
	if a.srv != nil {
		srvURL = a.srv.URL()
	}
	a.srvMu.Unlock()

	var qrDataURL string
	inviteURL := ""
	if srvURL != "" {
		inviteURL = fmt.Sprintf("%s/guest#%s", srvURL, room.InviteSecret)
		qrPNG, err := qr.PNG(inviteURL, qr.DefaultSize)
		if err == nil {
			qrDataURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(qrPNG)
		}
	}

	rv := room.View()
	roomView := &GuestRoomView{
		ID:              rv.ID,
		Date:            rv.Date,
		DestinationRoot: rv.DestinationRoot,
		RoomFolder:      rv.RoomFolder,
		Title:           rv.Title,
		Description:     rv.Description,
		Deadline:        rv.Deadline,
		CollisionPolicy: string(rv.CollisionPolicy),
		State:           string(rv.State),
		DurationSeconds: rv.DurationSeconds,
		CreatedAt:       rv.CreatedAt,
		ExpiresAt:       rv.ExpiresAt,
		InviteSecret:    rv.InviteSecret,
		InviteURL:       inviteURL,
		QRDataURL:       qrDataURL,
		FormSchema:      rv.FormSchema,
		Roster:          rv.Roster,
		HasRoster:       rv.HasRoster,
	}

	var pendingGuests []GuestView
	var approvedGuests []GuestView
	for _, g := range room.ListGuests() {
		gv := GuestView{
			ID:            g.ID,
			Nickname:      g.Nickname,
			ParticipantID: g.ParticipantID,
			State:         string(g.State),
			CreatedAt:     g.CreatedAt,
		}
		if g.State == guest.GuestPending {
			pendingGuests = append(pendingGuests, gv)
		} else if g.State == guest.GuestApproved {
			approvedGuests = append(approvedGuests, gv)
		}
	}

	var pendingUploads []GuestUploadView
	var completedUploads []GuestUploadView
	var recentUploads []GuestUploadView
	for _, u := range room.ListUploads() {
		uv := GuestUploadView{
			ID:          u.ID,
			GuestID:     u.GuestID,
			GuestName:   u.GuestName,
			FileName:    u.FileName,
			Size:        u.Size,
			State:       string(u.State),
			Transferred: u.Transferred,
			Speed:       u.Speed,
			CreatedAt:   u.CreatedAt,
			CompletedAt: u.CompletedAt,
			SHA256:      u.SHA256,
			Error:       u.Error,
			FormValues:  u.FormValues,
		}
		recentUploads = append(recentUploads, uv)
		if u.State == guest.UploadPendingApproval {
			pendingUploads = append(pendingUploads, uv)
		} else if u.State == guest.UploadCompleted {
			completedUploads = append(completedUploads, uv)
		}
	}

	sort.Slice(recentUploads, func(i, j int) bool {
		return recentUploads[i].CreatedAt.After(recentUploads[j].CreatedAt)
	})

	return GuestRoomStateView{
		Room:             roomView,
		PendingGuests:    pendingGuests,
		ApprovedGuests:   approvedGuests,
		PendingUploads:   pendingUploads,
		CompletedUploads: completedUploads,
		RecentUploads:    recentUploads,
		Roster:           rv.Roster,
	}, nil
}

func (a *App) ApproveGuest(guestID string) error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	err := room.ApproveGuest(guestID)
	if err == nil && a.auditStore != nil {
		a.auditStore.Record(audit.Entry{
			EventType: audit.EventGuestApproved,
			RoomID:    room.ID,
			GuestID:   guestID,
			Outcome:   audit.OutcomeSuccess,
		})
	}
	return err
}

func (a *App) RejectGuest(guestID string) error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	err := room.RejectGuest(guestID)
	if err == nil && a.auditStore != nil {
		a.auditStore.Record(audit.Entry{
			EventType: audit.EventGuestRejected,
			RoomID:    room.ID,
			GuestID:   guestID,
			Outcome:   audit.OutcomeRejected,
		})
	}
	return err
}

func (a *App) RevokeGuest(guestID string) error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	err := room.RevokeGuest(guestID)
	if err == nil && a.auditStore != nil {
		a.auditStore.Record(audit.Entry{
			EventType: audit.EventGuestRevoked,
			RoomID:    room.ID,
			GuestID:   guestID,
			Outcome:   audit.OutcomeCanceled,
		})
	}
	return err
}

func (a *App) ApproveGuestUpload(uploadID string) error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	err := room.ApproveUpload(uploadID)
	if err == nil && a.auditStore != nil {
		a.auditStore.Record(audit.Entry{
			EventType: audit.EventUploadApproved,
			RoomID:    room.ID,
			Details:   fmt.Sprintf("upload_id: %s", uploadID),
			Outcome:   audit.OutcomeSuccess,
		})
	}
	return err
}

func (a *App) RejectGuestUpload(uploadID string) error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	err := room.RejectUpload(uploadID)
	if err == nil && a.auditStore != nil {
		a.auditStore.Record(audit.Entry{
			EventType: audit.EventUploadRejected,
			RoomID:    room.ID,
			Details:   fmt.Sprintf("upload_id: %s", uploadID),
			Outcome:   audit.OutcomeRejected,
		})
	}
	return err
}

func (a *App) isGuestRoomActive() bool {
	if a.guestMgr == nil {
		return false
	}
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return false
	}
	room.CheckExpiry()
	v := room.View()
	return v.State == guest.RoomActive || v.State == guest.RoomExpiring
}

func (a *App) EndGuestRoom() error {
	room := a.guestMgr.ActiveRoom()
	var roomID string
	if room != nil {
		roomID = room.ID
	}
	err := a.guestMgr.EndRoom()
	if a.sessions.Len() == 0 {
		a.stopServer()
	}
	if a.auditStore != nil && roomID != "" {
		a.auditStore.Record(audit.Entry{
			EventType: audit.EventRoomEnded,
			RoomID:    roomID,
			Outcome:   audit.OutcomeSuccess,
		})
	}
	return err
}

func (a *App) PickGuestDestination() (string, error) {
	if a.ctx == nil {
		return a.store.Dir(), nil
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Choose Guest Drop Box Destination",
	})
	if err != nil {
		return "", err
	}
	if dir == "" {
		return a.store.Dir(), nil
	}
	return dir, nil
}

func openFolderInOS(folder string) error {
	var cmd *exec.Cmd
	switch stdlibRuntime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", folder)
	case "darwin":
		cmd = exec.Command("open", folder)
	default:
		cmd = exec.Command("xdg-open", folder)
	}
	return cmd.Start()
}

func (a *App) OpenGuestFolder() error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	folder := room.RoomFolder
	_ = os.MkdirAll(folder, 0700)
	return openFolderInOS(folder)
}

// ---- Expected Participants (Roster) methods ----

func (a *App) AddExpectedParticipant(name, email, identifier string) error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	idVal := identifier
	if idVal == "" {
		idVal = email
	}
	_, err := room.AddParticipant(name, idVal)
	return err
}

func (a *App) RemoveExpectedParticipant(participantID string) error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	return room.RemoveParticipant(participantID)
}

func (a *App) UpdateExpectedParticipant(participant guest.ExpectedParticipant) error {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return errors.New("flitnix: no active guest room")
	}
	return room.UpdateParticipant(participant.ID, participant.Name, participant.Identifier)
}

// ---- Guest Room Templates & Presets ----

func (a *App) ListGuestPresets() []guest.Preset {
	return guest.DefaultPresets()
}

func (a *App) ListGuestTemplates() []guest.Template {
	if a.templateStore == nil {
		return nil
	}
	return a.templateStore.List()
}

func (a *App) SaveGuestTemplate(t guest.Template) error {
	if a.templateStore == nil {
		return errors.New("flitnix: template store not initialized")
	}
	return a.templateStore.Save(t)
}

func (a *App) DeleteGuestTemplate(id string) error {
	if a.templateStore == nil {
		return errors.New("flitnix: template store not initialized")
	}
	return a.templateStore.Delete(id)
}

func (a *App) DuplicateGuestTemplate(id string, newName string) (guest.Template, error) {
	if a.templateStore == nil {
		return guest.Template{}, errors.New("flitnix: template store not initialized")
	}
	tpl, err := a.templateStore.Duplicate(id, newName)
	if err != nil {
		return guest.Template{}, err
	}
	return *tpl, nil
}

func (a *App) StartGuestRoomFromTemplate(templateID string, destinationOverride string) (GuestRoomView, error) {
	if a.templateStore == nil {
		return GuestRoomView{}, errors.New("flitnix: template store not initialized")
	}
	tpl, ok := a.templateStore.Get(templateID)
	if !ok {
		return GuestRoomView{}, errors.New("flitnix: template not found")
	}
	dest := tpl.DestinationRoot
	if destinationOverride != "" {
		dest = destinationOverride
	}
	return a.StartGuestRoomWithConfig(GuestRoomConfigView{
		DestinationDir:       dest,
		DurationMinutes:      tpl.DurationMinutes,
		Title:                tpl.Title,
		Description:          tpl.Description,
		CollisionPolicy:      string(tpl.CollisionPolicy),
		FormSchema:           tpl.FormSchema,
		ExpectedParticipants: tpl.ExpectedParticipants,
	})
}

// ---- Collection ZIP Export ----

func (a *App) PickExportDestination() (string, error) {
	room := a.guestMgr.ActiveRoom()
	defaultName := "flitnix-submissions.zip"
	if room != nil && room.Title != "" {
		clean := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, room.Title)
		clean = strings.Trim(clean, "_")
		if clean != "" {
			defaultName = fmt.Sprintf("%s-submissions.zip", clean)
		}
	}

	if a.ctx == nil {
		return filepath.Join(a.store.Dir(), defaultName), nil
	}

	target, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "Export Collection Archive",
		DefaultFilename: defaultName,
		Filters: []runtime.FileFilter{
			{DisplayName: "ZIP Archives (*.zip)", Pattern: "*.zip"},
		},
	})
	if err != nil {
		return "", err
	}
	return target, nil
}

func (a *App) ExportGuestCollection(targetZipPath string) (guest.ExportResult, error) {
	room := a.guestMgr.ActiveRoom()
	if room == nil {
		return guest.ExportResult{}, errors.New("flitnix: no active guest room")
	}
	if targetZipPath == "" {
		return guest.ExportResult{}, errors.New("flitnix: export target path cannot be empty")
	}

	res, err := guest.ExportRoomToZip(room, targetZipPath)
	if err != nil {
		if a.auditStore != nil {
			a.auditStore.Record(audit.Entry{
				EventType: audit.EventExportFailed,
				RoomID:    room.ID,
				Outcome:   audit.OutcomeFailed,
				Details:   fmt.Sprintf("error: %s, target: %s", err.Error(), targetZipPath),
			})
		}
		return guest.ExportResult{}, err
	}

	if a.auditStore != nil {
		a.auditStore.Record(audit.Entry{
			EventType: audit.EventExportCompleted,
			RoomID:    room.ID,
			Outcome:   audit.OutcomeSuccess,
			Details:   fmt.Sprintf("target: %s, files: %d, bytes: %d", targetZipPath, res.TotalFiles, res.TotalBytes),
		})
	}

	return *res, nil
}

// ---- Searchable Local Audit History ----

func (a *App) ListAuditEvents(filter audit.Filter) (AuditListResult, error) {
	if a.auditStore == nil {
		return AuditListResult{}, nil
	}
	entries, total := a.auditStore.Query(filter)
	return AuditListResult{
		Entries: entries,
		Total:   total,
	}, nil
}

func (a *App) ClearAuditHistory() error {
	if a.auditStore == nil {
		return nil
	}
	return a.auditStore.Clear()
}

// ---- Connection Doctor methods ----

func (a *App) recordPhoneContact(source string) {
	a.phoneContactMu.Lock()
	now := time.Now()
	a.lastPhoneContact = &now
	a.phoneContactMu.Unlock()
}

func (a *App) RunConnectionDiagnostics() doctor.DiagnosticReport {
	a.srvMu.Lock()
	serverRunning := a.srv != nil
	var serverAddr string
	if a.srv != nil && a.srv.Addr() != nil {
		serverAddr = a.srv.Addr().String()
	}
	a.srvMu.Unlock()

	var activeIP string
	var activeIface string
	for _, s := range a.ListSessions() {
		if s.Address != "" {
			activeIP = s.Address
		}
		if s.InterfaceName != "" {
			activeIface = s.InterfaceName
		}
	}
	if activeIP == "" && serverAddr != "" {
		host, _, err := net.SplitHostPort(serverAddr)
		if err == nil {
			activeIP = host
		}
	}

	a.phoneContactMu.Lock()
	lastContact := a.lastPhoneContact
	a.phoneContactMu.Unlock()

	return doctor.RunDiagnostics(doctor.Params{
		NetSource:        a.source,
		ServerRunning:    serverRunning,
		ServerAddr:       serverAddr,
		ActiveIP:         activeIP,
		ActiveIface:      activeIface,
		LastPhoneContact: lastContact,
	})
}

// ---- Quick Text & Link Transfer methods ----

func (a *App) SendTextMessage(sessionKey, content string) (TextMessageView, error) {
	if sessionKey == "" {
		sessions := a.sessions.ActiveSessions()
		if len(sessions) == 0 {
			return TextMessageView{}, errors.New("flitnix: no active pairing session; start one first")
		}
		sessionKey = sessions[0].Key()
	} else {
		found := false
		for _, s := range a.sessions.ActiveSessions() {
			if s.Key() == sessionKey {
				found = true
				break
			}
		}
		if !found {
			return TextMessageView{}, errors.New("flitnix: no active pairing session matches that identifier")
		}
	}

	msg, err := a.textStore.Add(sessionKey, "desktop", content)
	if err != nil {
		return TextMessageView{}, err
	}
	return TextMessageView{
		ID:        msg.ID,
		Sender:    msg.Sender,
		Content:   msg.Content,
		IsLink:    msg.IsLink,
		URL:       msg.URL,
		CreatedAt: msg.CreatedAt,
	}, nil
}

func (a *App) ListTextMessages(sessionKey string) []TextMessageView {
	if sessionKey == "" {
		sessions := a.sessions.ActiveSessions()
		if len(sessions) == 0 {
			return []TextMessageView{}
		}
		sessionKey = sessions[0].Key()
	} else {
		found := false
		for _, s := range a.sessions.ActiveSessions() {
			if s.Key() == sessionKey {
				found = true
				break
			}
		}
		if !found {
			return []TextMessageView{}
		}
	}

	msgs := a.textStore.List(sessionKey)
	res := make([]TextMessageView, len(msgs))
	for i, m := range msgs {
		res[i] = TextMessageView{
			ID:        m.ID,
			Sender:    m.Sender,
			Content:   m.Content,
			IsLink:    m.IsLink,
			URL:       m.URL,
			CreatedAt: m.CreatedAt,
		}
	}
	return res
}

func (a *App) ClearTextMessages(sessionKey string) error {
	if sessionKey == "" {
		sessions := a.sessions.ActiveSessions()
		if len(sessions) > 0 {
			sessionKey = sessions[0].Key()
		}
	}
	if sessionKey != "" {
		a.textStore.Clear(sessionKey)
	}
	return nil
}

// ---- Cryptographically Trusted Devices ----

func (a *App) ListTrustedDevices() []devices.Device {
	if a.devicesStore == nil {
		return nil
	}
	return a.devicesStore.List()
}

func (a *App) RemoveTrustedDevice(id string) error {
	if a.devicesStore == nil {
		return nil
	}
	return a.devicesStore.Remove(id)
}

func (a *App) SetDeviceAutoApprove(id string, autoApprove bool) error {
	if a.devicesStore == nil {
		return nil
	}
	return a.devicesStore.SetAutoApprove(id, autoApprove)
}

func (a *App) ApprovePairing(requestID string) error {
	if a.pairing == nil {
		return errors.New("pairing coordinator unavailable")
	}
	return a.pairing.ApprovePairing(requestID)
}

func (a *App) DenyPairing(requestID string) error {
	if a.pairing == nil {
		return errors.New("pairing coordinator unavailable")
	}
	return a.pairing.DenyPairing(requestID)
}

func (a *App) ReArmPairing() {
	if a.pairing != nil {
		a.pairing.ReArmPairing()
	}
}

func (a *App) GetPairingStatus() devices.PairingStatusView {
	if a.pairing == nil {
		return devices.PairingStatusView{}
	}
	return a.pairing.GetPairingStatus()
}
