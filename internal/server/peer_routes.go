package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
)

type peerChallengeEntry struct {
	HostID       string
	ClientHostID string
	ClientPubHex string
	IssuedAt     time.Time
	Nonce        string
}

type peerChallengeStore struct {
	mu      sync.Mutex
	entries map[string]peerChallengeEntry
}

func newPeerChallengeStore() *peerChallengeStore {
	return &peerChallengeStore{
		entries: make(map[string]peerChallengeEntry),
	}
}

func (p *peerChallengeStore) issue(hostID, clientHostID, clientPubHex string) (string, int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UTC()
	cutoff := now.Add(-2 * time.Minute)
	for k, v := range p.entries {
		if v.IssuedAt.Before(cutoff) {
			delete(p.entries, k)
		}
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", 0, err
	}
	nonce := hex.EncodeToString(raw[:])
	p.entries[nonce] = peerChallengeEntry{
		HostID:       hostID,
		ClientHostID: clientHostID,
		ClientPubHex: clientPubHex,
		IssuedAt:     now,
		Nonce:        nonce,
	}
	return nonce, now.Unix(), nil
}

func (p *peerChallengeStore) consume(hostID, clientHostID, clientPubHex, nonce string, ts int64) error {
	if nonce == "" {
		return devices.ErrChallengeExpired
	}
	now := time.Now().UTC()
	diff := now.Sub(time.Unix(ts, 0).UTC())
	if diff < 0 {
		diff = -diff
	}
	if diff > 60*time.Second {
		return devices.ErrChallengeExpired
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[nonce]
	if !ok {
		return devices.ErrReplayDetected
	}
	delete(p.entries, nonce)
	if !strings.EqualFold(entry.HostID, hostID) ||
		!strings.EqualFold(entry.ClientHostID, clientHostID) ||
		!strings.EqualFold(entry.ClientPubHex, clientPubHex) {
		return devices.ErrInvalidSignature
	}
	if now.Sub(entry.IssuedAt) > 2*time.Minute {
		return devices.ErrChallengeExpired
	}
	return nil
}

// BuildPeerAuthPayload creates the deterministic Ed25519 challenge message for visible LAN peers.
func BuildPeerAuthPayload(hostID, clientHostID string, timestampUnix int64, nonce string) []byte {
	return []byte(fmt.Sprintf("flitnix-peer-auth-v1:%s:%s:%d:%s", hostID, clientHostID, timestampUnix, nonce))
}

func enforceOpenPeerVisibility(w http.ResponseWriter, svcs *Services) bool {
	if svcs == nil || svcs.Visibility == nil {
		writeErrorCode(w, http.StatusForbidden, "Open peer visibility is disabled.", "visibility_restricted")
		return false
	}
	vis := svcs.Visibility.Evaluate()
	switch vis.Mode {
	case devices.VisibilityEveryone, devices.VisibilityTemporaryEveryone:
		return true
	case devices.VisibilityHidden:
		writeErrorCode(w, http.StatusForbidden, "This computer is currently Hidden.", "visibility_hidden")
		return false
	default:
		writeErrorCode(w, http.StatusForbidden, "This computer only accepts paired Trusted Devices or a 6-digit connection code.", "visibility_trusted_only")
		return false
	}
}

func handlePeerChallenge(svcs *Services, limiter *deviceSessionLimiter, store *peerChallengeStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svcs == nil || svcs.HostIdentity == nil || svcs.Sessions == nil {
			writeError(w, http.StatusNotFound, "peer access unavailable")
			return
		}
		if !enforceOpenPeerVisibility(w, svcs) {
			return
		}
		clientIP := clientIPFromRequest(r)
		if ok, retry := limiter.checkAllowed(clientIP); !ok {
			writeRateLimit(w, retry)
			return
		}
		var req struct {
			ClientHostID    string `json:"clientHostId"`
			ClientPublicKey string `json:"clientPublicKey"`
		}
		if !decodeControlJSON(w, r, &req, "invalid peer challenge request", rejectUnknownFields) {
			return
		}
		pubBytes, err := hex.DecodeString(req.ClientPublicKey)
		if err != nil || len(pubBytes) != ed25519.PublicKeySize {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusBadRequest, "invalid clientPublicKey", "invalid_key")
			return
		}
		sum := sha256.Sum256(pubBytes)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), req.ClientHostID) {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusBadRequest, "clientHostId does not match clientPublicKey", "invalid_key")
			return
		}
		nonce, ts, err := store.issue(svcs.HostIdentity.HostID, strings.ToLower(req.ClientHostID), strings.ToLower(req.ClientPublicKey))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to issue peer challenge")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"hostId":        svcs.HostIdentity.HostID,
			"hostPublicKey": svcs.HostIdentity.PublicKey,
			"nonce":         nonce,
			"timestamp":     ts,
		})
	}
}

func handlePeerConnect(svcs *Services, limiter *deviceSessionLimiter, store *peerChallengeStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svcs == nil || svcs.HostIdentity == nil || svcs.Sessions == nil {
			writeError(w, http.StatusNotFound, "peer access unavailable")
			return
		}
		if !enforceOpenPeerVisibility(w, svcs) {
			return
		}
		clientIP := clientIPFromRequest(r)
		if ok, retry := limiter.checkAllowed(clientIP); !ok {
			writeRateLimit(w, retry)
			return
		}
		var req struct {
			HostID          string `json:"hostId"`
			ClientHostID    string `json:"clientHostId"`
			ClientPublicKey string `json:"clientPublicKey"`
			DeviceName      string `json:"deviceName"`
			Nonce           string `json:"nonce"`
			Timestamp       int64  `json:"timestamp"`
			Signature       string `json:"signature"`
		}
		if !decodeControlJSON(w, r, &req, "invalid peer connect request", rejectUnknownFields) {
			return
		}
		if !strings.EqualFold(req.HostID, svcs.HostIdentity.HostID) {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusUnauthorized, "host mismatch", "host_mismatch")
			return
		}
		pubBytes, err := hex.DecodeString(req.ClientPublicKey)
		if err != nil || len(pubBytes) != ed25519.PublicKeySize {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusBadRequest, "invalid clientPublicKey", "invalid_key")
			return
		}
		sigBytes, err := hex.DecodeString(req.Signature)
		if err != nil || len(sigBytes) != ed25519.SignatureSize {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusUnauthorized, "invalid signature", "invalid_signature")
			return
		}
		payload := BuildPeerAuthPayload(svcs.HostIdentity.HostID, strings.ToLower(req.ClientHostID), req.Timestamp, req.Nonce)
		if !ed25519.Verify(ed25519.PublicKey(pubBytes), payload, sigBytes) {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusUnauthorized, "signature verification failed", "invalid_signature")
			return
		}
		if err := store.consume(svcs.HostIdentity.HostID, req.ClientHostID, req.ClientPublicKey, req.Nonce, req.Timestamp); err != nil {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusUnauthorized, "peer challenge expired or invalid", "challenge_invalid")
			return
		}

		// Untrusted open-visibility peer gets an unprivileged session where every upload/batch requires manual approval.
		sess, err := svcs.Sessions.Create()
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "could not establish peer session")
			return
		}
		limiter.clear(clientIP)
		svcs.RecordContact("peer-connect")

		writeJSON(w, http.StatusOK, map[string]any{
			"token":       sess.Token(),
			"sessionKey":  sess.Key(),
			"expiresAt":   sess.ExpiresAt(),
			"hostId":      svcs.HostIdentity.HostID,
			"deviceName":  devices.SanitizeDeviceName(req.DeviceName),
			"autoApprove": false,
		})
	}
}
