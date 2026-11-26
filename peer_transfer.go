package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	stdlibRuntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/client"
	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/netenv"
	"github.com/itsyamin/flitnix/internal/server"
	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
	"github.com/itsyamin/flitnix/internal/version"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// LocalPeerInfo describes this computer's identity & connectivity for Computer Sharing.
type LocalPeerInfo struct {
	HostID      string `json:"hostId"`
	PublicKey   string `json:"publicKey"`
	Fingerprint string `json:"fingerprint"`
	DeviceName  string `json:"deviceName"`
	OS          string `json:"os"`
	Version     string `json:"version"`
	Address     string `json:"address"`
	Port        int    `json:"port"`
}

// PeerTransferJob tracks one outbound desktop-to-desktop transfer in real time.
type PeerTransferJob struct {
	ID             string    `json:"id"`
	PeerHostID     string    `json:"peerHostId,omitempty"`
	PeerName       string    `json:"peerName"`
	PeerAddress    string    `json:"peerAddress"`
	Kind           string    `json:"kind"` // "files" | "folder"
	RootName       string    `json:"rootName,omitempty"`
	TotalItems     int       `json:"totalItems"`
	CompletedItems int       `json:"completedItems"`
	TotalBytes     int64     `json:"totalBytes"`
	SentBytes      int64     `json:"sentBytes"`
	Speed          float64   `json:"speed"`
	State          string    `json:"state"`
	CurrentFile    string    `json:"currentFile,omitempty"`
	TrustedPeer    bool      `json:"trustedPeer"`
	Verified       bool      `json:"verified"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	CompletedAt    time.Time `json:"completedAt,omitempty"`
}

// PeerSendRequest initiates a desktop-to-desktop transfer.
type PeerSendRequest struct {
	Address        string   `json:"address"`
	Port           int      `json:"port,omitempty"`
	ExpectedHostID string   `json:"expectedHostId,omitempty"`
	ExpectedPubKey string   `json:"expectedPubKey,omitempty"`
	PeerName       string   `json:"peerName,omitempty"`
	ShortCode      string   `json:"shortCode,omitempty"`
	PairingToken   string   `json:"pairingToken,omitempty"`
	Paths          []string `json:"paths"`
}

// PeerPairRequest initiates SAS pairing with another desktop over TLS 1.3.
type PeerPairRequest struct {
	Address        string `json:"address"`
	Port           int    `json:"port,omitempty"`
	ExpectedHostID string `json:"expectedHostId,omitempty"`
	ExpectedPubKey string `json:"expectedPubKey,omitempty"`
	PeerName       string `json:"peerName,omitempty"`
	ShortCode      string `json:"shortCode,omitempty"`
	PairingToken   string `json:"pairingToken,omitempty"`
}

// PeerPairSASPrompt is emitted to the sender desktop UI to confirm SAS with the peer.
type PeerPairSASPrompt struct {
	RequestID  string `json:"requestId"`
	PeerName   string `json:"peerName"`
	PeerHostID string `json:"peerHostId"`
	SAS        string `json:"sas"`
	IsPinned   bool   `json:"isPinned"`
}

type peerLinkRecord struct {
	HostID        string `json:"hostId"`
	DeviceID      string `json:"deviceId"`
	HostPublicKey string `json:"hostPublicKey"`
	PeerName      string `json:"peerName"`
	LastAddress   string `json:"lastAddress"`
}

type peerTransferManager struct {
	app      *App
	linksMu  sync.Mutex
	links    map[string]peerLinkRecord
	linksDir string

	jobsMu  sync.Mutex
	jobs    map[string]*PeerTransferJob
	cancels map[string]context.CancelFunc

	sasMu      sync.Mutex
	sasWaiters map[string]chan bool
}

func newPeerTransferManager(app *App, dataDir string) *peerTransferManager {
	m := &peerTransferManager{
		app:        app,
		links:      make(map[string]peerLinkRecord),
		linksDir:   dataDir,
		jobs:       make(map[string]*PeerTransferJob),
		cancels:    make(map[string]context.CancelFunc),
		sasWaiters: make(map[string]chan bool),
	}
	m.loadLinks()
	return m
}

func (m *peerTransferManager) linksPath() string {
	if m.linksDir == "" {
		return ""
	}
	return filepath.Join(m.linksDir, "peer_links.json")
}

func (m *peerTransferManager) loadLinks() {
	path := m.linksPath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var list []peerLinkRecord
	if err := json.Unmarshal(raw, &list); err != nil {
		return
	}
	m.linksMu.Lock()
	defer m.linksMu.Unlock()
	for _, rec := range list {
		if rec.HostID != "" {
			m.links[strings.ToLower(rec.HostID)] = rec
		}
	}
}

func (m *peerTransferManager) saveLink(rec peerLinkRecord) error {
	m.linksMu.Lock()
	m.links[strings.ToLower(rec.HostID)] = rec
	list := make([]peerLinkRecord, 0, len(m.links))
	for _, v := range m.links {
		list = append(list, v)
	}
	m.linksMu.Unlock()

	path := m.linksPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *peerTransferManager) getLink(hostID string) (peerLinkRecord, bool) {
	m.linksMu.Lock()
	defer m.linksMu.Unlock()
	rec, ok := m.links[strings.ToLower(hostID)]
	return rec, ok
}

func (m *peerTransferManager) upsertJob(j PeerTransferJob) {
	m.jobsMu.Lock()
	cp := j
	m.jobs[j.ID] = &cp
	m.jobsMu.Unlock()
	m.emitJob(cp)
}

func (m *peerTransferManager) updateJob(id string, fn func(*PeerTransferJob)) {
	m.jobsMu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.jobsMu.Unlock()
		return
	}
	fn(j)
	cp := *j
	m.jobsMu.Unlock()
	m.emitJob(cp)
}

func (m *peerTransferManager) emitJob(j PeerTransferJob) {
	if m.app == nil {
		return
	}
	m.app.startupMu.Lock()
	ctx := m.app.ctx
	m.app.startupMu.Unlock()
	if ctx != nil && ctx.Value("frontend") != nil {
		runtime.EventsEmit(ctx, "peer-transfer:updated", j)
	}
}

func (m *peerTransferManager) listJobs() []PeerTransferJob {
	m.jobsMu.Lock()
	out := make([]PeerTransferJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	m.jobsMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func formatAuthority(address string, port int) (string, error) {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return "", errors.New("peer address is required")
	}
	if strings.Contains(trimmed, "://") {
		if strings.HasPrefix(trimmed, "http://") {
			trimmed = strings.TrimPrefix(trimmed, "http://")
		} else if strings.HasPrefix(trimmed, "https://") {
			trimmed = strings.TrimPrefix(trimmed, "https://")
		}
	}
	trimmed = strings.SplitN(trimmed, "/", 2)[0]
	trimmed = strings.SplitN(trimmed, "#", 2)[0]
	if h, p, err := net.SplitHostPort(trimmed); err == nil {
		if h == "" {
			return "", errors.New("invalid host")
		}
		if port > 0 {
			return net.JoinHostPort(h, strconv.Itoa(port)), nil
		}
		return net.JoinHostPort(h, p), nil
	}
	if port <= 0 {
		port = server.DefaultPort
	}
	cleanHost := strings.Trim(trimmed, "[]")
	return net.JoinHostPort(cleanHost, strconv.Itoa(port)), nil
}

// GetLocalPeerInfo returns this desktop's cryptographic identity and local LAN address.
func (a *App) GetLocalPeerInfo() LocalPeerInfo {
	hostID := ""
	pubKey := ""
	fingerprint := ""
	if a.hostIdentity != nil {
		hostID = a.hostIdentity.HostID
		pubKey = a.hostIdentity.PublicKey
		if len(hostID) >= 12 {
			fingerprint = hostID[:6] + "·" + hostID[len(hostID)-6:]
		} else {
			fingerprint = hostID
		}
	}
	ep := a.discoveryLocalEndpoint()
	addr := a.ServerAddress()
	if addr == "" && ep.LocalIP != "" {
		addr = net.JoinHostPort(ep.LocalIP, strconv.Itoa(ep.Port))
	}
	return LocalPeerInfo{
		HostID:      hostID,
		PublicKey:   pubKey,
		Fingerprint: fingerprint,
		DeviceName:  ep.DeviceName,
		OS:          stdlibRuntime.GOOS,
		Version:     version.Get().Version,
		Address:     addr,
		Port:        ep.Port,
	}
}

// EnsurePeerServer ensures the local HTTP+TLS peer listener is bound on the active non-VPN LAN interface.
func (a *App) EnsurePeerServer() (string, error) {
	if addr := a.ServerAddress(); addr != "" {
		return addr, nil
	}
	view, err := a.resolveInterface("")
	if err != nil {
		return "", err
	}
	listen, err := netenv.ListenAddr(view.IP, server.DefaultPort)
	if err != nil {
		return "", err
	}
	if err := a.ensureServer(listen); err != nil {
		return "", err
	}
	if a.discovery != nil {
		a.discovery.NotifyEndpointChanged()
	}
	return a.ServerAddress(), nil
}

// EnsurePeerSessionShortCode creates or returns an active session's 6-digit connection code
// strictly upon explicit user action in the Computer Sharing UI.
func (a *App) EnsurePeerSessionShortCode() (session.ShortCodeView, error) {
	if _, err := a.EnsurePeerServer(); err != nil {
		return session.ShortCodeView{}, err
	}
	actives := a.sessions.ActiveSessions()
	var sessKey string
	for _, s := range actives {
		if s.Kind() != session.KindDevice {
			sessKey = s.Key()
			break
		}
	}
	if sessKey == "" {
		created, err := a.sessions.Create()
		if err != nil {
			return session.ShortCodeView{}, err
		}
		sessKey = created.Key()
	}
	return a.GetSessionShortCode(sessKey), nil
}

// InspectPeerTarget connects over TLS 1.3 to retrieve the remote desktop's verified Ed25519 HostID/PublicKey.
func (a *App) InspectPeerTarget(address string, port int) (client.PeerProbeInfo, error) {
	authority, err := formatAuthority(address, port)
	if err != nil {
		return client.PeerProbeInfo{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return client.ProbePeerTLS(ctx, authority)
}

// PickFilesForPeer opens a multi-file selector strictly for outbound Computer Sharing.
func (a *App) PickFilesForPeer() ([]string, error) {
	if a.ctx == nil {
		return nil, errors.New("flitnix: application context unavailable")
	}
	paths, err := runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select Files to Send to Computer",
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// PickFolderForPeer opens a directory selector strictly for outbound Computer Sharing.
func (a *App) PickFolderForPeer() (string, error) {
	if a.ctx == nil {
		return "", errors.New("flitnix: application context unavailable")
	}
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Select Folder to Send to Computer",
	})
}

// ListPeerTransfers returns active and recent desktop-to-desktop transfer jobs.
func (a *App) ListPeerTransfers() []PeerTransferJob {
	if a.peerMgr == nil {
		return []PeerTransferJob{}
	}
	return a.peerMgr.listJobs()
}

// CancelPeerTransfer cancels an active outbound peer transfer job.
func (a *App) CancelPeerTransfer(jobID string) error {
	if a.peerMgr == nil {
		return nil
	}
	a.peerMgr.jobsMu.Lock()
	cancelFn, ok := a.peerMgr.cancels[jobID]
	a.peerMgr.jobsMu.Unlock()
	if ok && cancelFn != nil {
		cancelFn()
	}
	return nil
}

// ClearCompletedPeerTransfers removes finished jobs from the Computer Sharing queue.
func (a *App) ClearCompletedPeerTransfers() {
	if a.peerMgr == nil {
		return
	}
	a.peerMgr.jobsMu.Lock()
	for id, j := range a.peerMgr.jobs {
		if j.State == "completed" || j.State == "failed" || j.State == "canceled" {
			delete(a.peerMgr.jobs, id)
		}
	}
	a.peerMgr.jobsMu.Unlock()
}

// ResolvePeerPairingSAS delivers the user's SAS visual verification decision for an outbound PairWithPeer call.
func (a *App) ResolvePeerPairingSAS(requestID string, approved bool) {
	if a.peerMgr == nil {
		return
	}
	a.peerMgr.sasMu.Lock()
	ch, ok := a.peerMgr.sasWaiters[requestID]
	if ok {
		delete(a.peerMgr.sasWaiters, requestID)
	}
	a.peerMgr.sasMu.Unlock()
	if ok && ch != nil {
		select {
		case ch <- approved:
		default:
		}
	}
}

func verifyHexEd25519(pubHex string, payload []byte, sigHex string) bool {
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return false
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pubBytes), payload, sigBytes)
}

func (a *App) clientIdentityFromHost() (*client.ClientIdentity, error) {
	if a.hostIdentity == nil {
		return nil, errors.New("host identity unavailable")
	}
	return client.NewClientIdentityFromKeys(a.hostIdentity.PublicKey, a.hostIdentity.PrivateKey)
}

func (a *App) peerAuthClientAndTarget(ctx context.Context, authority, expectedHostID, expectedPubKey, shortCode, pairingToken string) (*http.Client, *client.Target, string, bool, error) {
	kh, err := client.LoadOrCreateKnownHosts(filepath.Join(a.dataDir, "known_hosts.json"))
	if err != nil {
		return nil, nil, "", false, err
	}
	if expectedHostID != "" && expectedPubKey != "" {
		if verifyErr := kh.VerifyHost(expectedHostID, expectedPubKey); verifyErr != nil {
			return nil, nil, "", false, verifyErr
		}
	}

	var idMu sync.Mutex
	var cbHostID, cbPubHex string
	hc := client.NewPeerHTTPClient(expectedHostID, expectedPubKey, func(hid, pk string) {
		idMu.Lock()
		cbHostID = hid
		cbPubHex = pk
		idMu.Unlock()
	})
	baseURL := "https://" + authority

	// Probe TLS + verify host identity from the live TLS 1.3 leaf certificate BEFORE sending credentials.
	probeReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return nil, nil, "", false, err
	}
	probeReq.Header.Set("Host", authority)
	probeResp, err := hc.Do(probeReq)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("failed TLS 1.3 handshake with %s: %w", authority, err)
	}
	_ = probeResp.Body.Close()

	var peerHostID, peerPubHex string
	if probeResp.TLS != nil && len(probeResp.TLS.PeerCertificates) > 0 {
		if pub, ok := probeResp.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey); ok && len(pub) == ed25519.PublicKeySize {
			sum := sha256.Sum256(pub)
			peerHostID = hex.EncodeToString(sum[:])
			peerPubHex = hex.EncodeToString(pub)
		}
	}
	if peerHostID == "" {
		idMu.Lock()
		peerHostID = cbHostID
		peerPubHex = cbPubHex
		idMu.Unlock()
	}
	if peerHostID == "" || peerPubHex == "" {
		return nil, nil, "", false, errors.New("TLS peer presented no verified Ed25519 certificate")
	}

	// Enforce TOFU pinning against known_hosts.json and detect IP-address swap when caller
	// previously linked this address to a different host identity.
	if err := kh.VerifyHost(peerHostID, peerPubHex); err != nil {
		return nil, nil, "", false, err
	}
	if a.peerMgr != nil && expectedHostID == "" && strings.TrimSpace(shortCode) == "" && strings.TrimSpace(pairingToken) == "" {
		a.peerMgr.linksMu.Lock()
		for linkedID, rec := range a.peerMgr.links {
			if rec.LastAddress == authority && !strings.EqualFold(linkedID, peerHostID) {
				a.peerMgr.linksMu.Unlock()
				return nil, nil, "", false, fmt.Errorf("%w: %s previously paired to host %s, now presenting %s (verify identity or use 6-digit code)", client.ErrHostMismatch, authority, linkedID, peerHostID)
			}
		}
		a.peerMgr.linksMu.Unlock()
	}

	if pairingToken != "" {
		t, err := client.ParseTarget(baseURL+"/#"+strings.TrimSpace(pairingToken), pairingToken)
		return hc, t, peerHostID, false, err
	}
	if strings.TrimSpace(shortCode) != "" {
		t, _, err := client.ConnectWithShortCode(ctx, hc, baseURL, authority, shortCode)
		return hc, t, peerHostID, false, err
	}

	// Check if this computer already holds a trusted device link to peerHostID.
	if a.peerMgr != nil {
		if link, ok := a.peerMgr.getLink(peerHostID); ok && link.DeviceID != "" && a.hostIdentity != nil {
			privBytes, decErr := hex.DecodeString(a.hostIdentity.PrivateKey)
			if decErr == nil && len(privBytes) == ed25519.PrivateKeySize {
				t, trustedErr := client.ConnectAsTrustedPeer(ctx, hc, baseURL, authority, link.DeviceID, ed25519.PrivateKey(privBytes))
				if trustedErr == nil {
					return hc, t, peerHostID, true, nil
				}
				// Surface explicit revocation or hidden errors instead of masking them with fallback.
				if strings.Contains(strings.ToLower(trustedErr.Error()), "no longer trusted") ||
					strings.Contains(strings.ToLower(trustedErr.Error()), "hidden") {
					return nil, nil, "", false, trustedErr
				}
			}
		}
	}

	ep := a.discoveryLocalEndpoint()
	t, err := client.ConnectAsVisiblePeer(ctx, hc, baseURL, authority, ep.DeviceName, a.hostIdentity)
	return hc, t, peerHostID, false, err
}

// PairWithPeer runs mutual TLS 1.3 + X25519 SAS pairing with another Flitnix desktop.
func (a *App) PairWithPeer(req PeerPairRequest) (*client.PairResult, error) {
	if a.peerMgr == nil {
		return nil, errors.New("peer transfer manager unavailable")
	}
	authority, err := formatAuthority(req.Address, req.Port)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	hc, target, resolvedHostID, _, err := a.peerAuthClientAndTarget(ctx, authority, req.ExpectedHostID, req.ExpectedPubKey, req.ShortCode, req.PairingToken)
	if err != nil {
		return nil, err
	}
	id, err := a.clientIdentityFromHost()
	if err != nil {
		return nil, err
	}
	kh, err := client.LoadOrCreateKnownHosts(filepath.Join(a.dataDir, "known_hosts.json"))
	if err != nil {
		return nil, err
	}
	ep := a.discoveryLocalEndpoint()

	res, err := client.PairWithHTTPClient(ctx, hc, target, ep.DeviceName, id, kh, func(sas string, isPinned bool) (bool, error) {
		a.startupMu.Lock()
		uiCtx := a.ctx
		a.startupMu.Unlock()
		if uiCtx == nil || uiCtx.Value("frontend") == nil {
			return true, nil
		}
		reqID, _ := transfer.NewOpaqueID()
		ch := make(chan bool, 1)
		a.peerMgr.sasMu.Lock()
		a.peerMgr.sasWaiters[reqID] = ch
		a.peerMgr.sasMu.Unlock()

		runtime.EventsEmit(uiCtx, "peer-pair:sas", PeerPairSASPrompt{
			RequestID:  reqID,
			PeerName:   req.PeerName,
			PeerHostID: resolvedHostID,
			SAS:        sas,
			IsPinned:   isPinned,
		})

		select {
		case ok := <-ch:
			return ok, nil
		case <-ctx.Done():
			a.peerMgr.sasMu.Lock()
			delete(a.peerMgr.sasWaiters, reqID)
			a.peerMgr.sasMu.Unlock()
			return false, ctx.Err()
		}
	})
	if err != nil {
		return nil, err
	}

	_ = a.peerMgr.saveLink(peerLinkRecord{
		HostID:        res.HostID,
		DeviceID:      res.DeviceID,
		HostPublicKey: res.HostPublicKey,
		PeerName:      req.PeerName,
		LastAddress:   authority,
	})
	return res, nil
}

// ConnectAndSendToPeer starts an async desktop-to-desktop transfer job over TLS 1.3.
func (a *App) ConnectAndSendToPeer(req PeerSendRequest) (PeerTransferJob, error) {
	if a.peerMgr == nil {
		return PeerTransferJob{}, errors.New("peer transfer manager unavailable")
	}
	if len(req.Paths) == 0 {
		return PeerTransferJob{}, errors.New("select at least one file or folder to send")
	}
	authority, err := formatAuthority(req.Address, req.Port)
	if err != nil {
		return PeerTransferJob{}, err
	}

	lm, err := client.BuildManifest(req.Paths, "")
	if err != nil {
		return PeerTransferJob{}, err
	}

	jobID, err := transfer.NewOpaqueID()
	if err != nil {
		return PeerTransferJob{}, err
	}

	peerName := strings.TrimSpace(req.PeerName)
	if peerName == "" {
		peerName = authority
	}
	job := PeerTransferJob{
		ID:          jobID,
		PeerHostID:  req.ExpectedHostID,
		PeerName:    peerName,
		PeerAddress: authority,
		Kind:        string(lm.Manifest.Kind),
		RootName:    lm.Manifest.RootName,
		TotalItems:  len(lm.LocalItems),
		TotalBytes:  lm.TotalBytes,
		State:       "connecting",
		CreatedAt:   time.Now(),
	}

	jobCtx, cancelFn := context.WithCancel(context.Background())
	a.peerMgr.jobsMu.Lock()
	a.peerMgr.cancels[jobID] = cancelFn
	a.peerMgr.jobsMu.Unlock()
	a.peerMgr.upsertJob(job)

	go a.runPeerTransferJob(jobCtx, jobID, req, authority, lm)
	return job, nil
}

func (a *App) runPeerTransferJob(ctx context.Context, jobID string, req PeerSendRequest, authority string, lm *client.LocalManifest) {
	defer func() {
		a.peerMgr.jobsMu.Lock()
		delete(a.peerMgr.cancels, jobID)
		a.peerMgr.jobsMu.Unlock()
	}()

	failJob := func(cause error) {
		state := "failed"
		msg := cause.Error()
		if errors.Is(cause, context.Canceled) {
			state = "canceled"
			msg = "Transfer canceled"
		}
		a.peerMgr.updateJob(jobID, func(j *PeerTransferJob) {
			j.State = state
			j.Speed = 0
			j.Error = msg
			j.CompletedAt = time.Now()
		})
	}

	a.peerMgr.updateJob(jobID, func(j *PeerTransferJob) {
		j.State = "authenticating"
	})
	if err := lm.PreComputeDigests(); err != nil {
		failJob(err)
		return
	}

	hc, target, resolvedHostID, trustedPeer, err := a.peerAuthClientAndTarget(
		ctx, authority, req.ExpectedHostID, req.ExpectedPubKey, req.ShortCode, req.PairingToken,
	)
	if err != nil {
		failJob(err)
		return
	}
	a.peerMgr.updateJob(jobID, func(j *PeerTransferJob) {
		if j.PeerHostID == "" {
			j.PeerHostID = resolvedHostID
		}
		j.TrustedPeer = trustedPeer
	})

	c := client.NewClientWithHTTPClient(target, hc)
	var authHdrs *client.AuthHeaders
	if trustedPeer && a.hostIdentity != nil {
		if link, ok := a.peerMgr.getLink(resolvedHostID); ok && link.DeviceID != "" {
			if chalResp, chalErr := c.RequestAuthChallenge(ctx, link.DeviceID); chalErr == nil {
				// Verify host signature on the transfer challenge against the TLS-pinned public key.
				chalPayload := devices.BuildSrvChalPayload(chalResp.HostID, chalResp.DeviceID, chalResp.TransferNonce, chalResp.Timestamp)
				if verifyHexEd25519(link.HostPublicKey, chalPayload, chalResp.HostSignature) {
					if cManifest, cErr := transfer.CanonicalManifestFromBatch(lm.Manifest, nil); cErr == nil {
						if _, mHash, hErr := transfer.ComputeManifestSHA256(cManifest); hErr == nil {
							if privBytes, decErr := hex.DecodeString(a.hostIdentity.PrivateKey); decErr == nil {
								payload := devices.BuildAuthV2Payload(chalResp.HostID, link.DeviceID, chalResp.Timestamp, chalResp.TransferNonce, mHash)
								sig := devices.SignMessage(ed25519.PrivateKey(privBytes), payload)
								authHdrs = &client.AuthHeaders{
									DeviceID:  link.DeviceID,
									Nonce:     chalResp.TransferNonce,
									Timestamp: chalResp.Timestamp,
									Signature: sig,
								}
							}
						}
					}
				}
			}
		}
	}

	batch, err := c.DeclareBatchWithAuth(ctx, lm.Manifest, authHdrs)
	if err != nil {
		failJob(err)
		return
	}
	if batch.ApprovalState != transfer.ApprovalApproved {
		a.peerMgr.updateJob(jobID, func(j *PeerTransferJob) {
			j.State = "awaiting_approval"
		})
		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Minute)
		batch, err = c.WaitForApproval(waitCtx, batch.ID, 400*time.Millisecond)
		waitCancel()
		if err != nil {
			failJob(err)
			return
		}
	}

	var completedBaseBytes int64
	var lastUIEmit time.Time
	lastSpeedAt := time.Now()
	var lastSpeedBytes int64
	var emaSpeed float64
	for idx, localItem := range lm.LocalItems {
		if idx >= len(batch.Items) {
			break
		}
		remoteItem := batch.Items[idx]
		a.peerMgr.updateJob(jobID, func(j *PeerTransferJob) {
			j.State = "transferring"
			j.CurrentFile = localItem.ManifestItem.DisplayName
		})
		res, streamErr := c.StreamItemWithHint(ctx, batch.ID, remoteItem.ID, localItem.Path, remoteItem.Transferred, transfer.DefaultChunkSize, func(sent, _ int64) {
			now := time.Now()
			if !lastUIEmit.IsZero() && now.Sub(lastUIEmit) < 150*time.Millisecond {
				return
			}
			totalSent := completedBaseBytes + sent
			if dt := now.Sub(lastSpeedAt).Seconds(); (dt >= 0.05 || (emaSpeed <= 0 && dt > 0)) && totalSent > lastSpeedBytes {
				inst := float64(totalSent-lastSpeedBytes) / dt
				if emaSpeed <= 0 {
					emaSpeed = inst
				} else {
					emaSpeed = 0.7*emaSpeed + 0.3*inst
				}
				lastSpeedBytes = totalSent
				lastSpeedAt = now
			}
			lastUIEmit = now
			spd := emaSpeed
			a.peerMgr.updateJob(jobID, func(j *PeerTransferJob) {
				j.SentBytes = totalSent
				j.Speed = spd
			})
		})
		if streamErr != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				cCtx, cCancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = c.CancelBatch(cCtx, batch.ID, batch.Items)
				cCancel()
			}
			failJob(streamErr)
			return
		}
		completedBaseBytes += res.Written
		now := time.Now()
		if emaSpeed <= 0 && completedBaseBytes > lastSpeedBytes {
			if dt := now.Sub(lastSpeedAt).Seconds(); dt > 0 {
				emaSpeed = float64(completedBaseBytes-lastSpeedBytes) / dt
			}
		}
		lastUIEmit = now
		spd := emaSpeed
		a.peerMgr.updateJob(jobID, func(j *PeerTransferJob) {
			j.CompletedItems = idx + 1
			j.SentBytes = completedBaseBytes
			j.Speed = spd
		})
	}

	a.peerMgr.updateJob(jobID, func(j *PeerTransferJob) {
		j.State = "completed"
		j.CurrentFile = ""
		j.Verified = true
		j.Speed = 0
		j.CompletedAt = time.Now()
	})
}
