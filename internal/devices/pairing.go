package devices

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrPairingInProgress   = errors.New("pairing: another pairing handshake is currently in progress")
	ErrPairingLocked       = errors.New("pairing: pairing locked due to repeated failed attempts across host. Desktop user must re-arm in settings")
	ErrPairingExpired      = errors.New("pairing: pairing challenge expired or not found")
	ErrPairingDenied       = errors.New("pairing: host operator denied pairing authorization")
	ErrWrongPairingState   = errors.New("pairing: handshake state mismatch")
	ErrSessionMismatch     = errors.New("pairing: session token binding mismatch")
	ErrStaleRequest        = errors.New("pairing: stale or invalid pairing request ID")
	ErrDecisionAlreadyMade = errors.New("pairing: approval decision already recorded for this request")
)

const (
	ChallengeTTL      = 30 * time.Second
	MaxFailedAttempts = 5
	FailureWindow     = 10 * time.Minute
	LockoutCooldown   = 15 * time.Minute
)

// PairingState tracks the phases of an active pairing handshake.
type PairingState int

const (
	StateNone PairingState = iota
	StateChallenged
	StateRevealed
	StatePendingApproval
	StateConsumed
)

// PendingPairing stores the ephemeral state of an in-flight mutual commitment exchange.
type PendingPairing struct {
	SessionKeyHash  []byte
	ClientPubKey    ed25519.PublicKey
	ClientID        []byte // 32 raw bytes
	ClientCommit    []byte // 32 raw bytes
	HostEphemeral   *EphemeralKeyPair
	HostSalt        []byte // 16 raw bytes
	HostCommit      [32]byte
	PairingNonce    []byte // 32 raw bytes
	PairingNonceHex string
	CreatedAt       time.Time
	ExpiresAt       time.Time

	// Revealed in Phase 2
	ClientEphemeral []byte
	ClientSalt      []byte
	KShared         []byte
	Transcript      []byte
	TranscriptHash  [32]byte
	SASNumeric      int
	SASDisplay      string
	HostProofHex    string

	// Pending Approval in Phase 3
	RequestID    string
	DeviceName   string
	HostApproved bool
	decisionMade bool
	decisionChan chan bool

	State PairingState
}

// PairingCoordinator coordinates the mutual commitment handshake, attempt limits,
// and host lockout mode.
type PairingCoordinator struct {
	mu       sync.Mutex
	identity *HostIdentity
	store    *Store
	active   *PendingPairing

	failureTimestamps []time.Time
	lockoutUntil      time.Time

	// OnPairingPrompt is an optional callback invoked when Bob's approval is requested.
	OnPairingPrompt func(deviceName, clientID, sasCode string, decisionChan chan<- bool)
}

// NewPairingCoordinator constructs a PairingCoordinator.
func NewPairingCoordinator(identity *HostIdentity, store *Store) *PairingCoordinator {
	return &PairingCoordinator{
		identity:          identity,
		store:             store,
		failureTimestamps: make([]time.Time, 0, MaxFailedAttempts),
	}
}

// ChallengeResponse is returned to the client upon Phase 1 POST /api/pair/challenge.
type ChallengeResponse struct {
	HostID           string `json:"hostId"`
	HostPublicKey    string `json:"hostPublicKey"`
	HostCommitment   string `json:"hostCommitment"`
	PairingNonce     string `json:"pairingNonce"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

// InitChallenge initiates Phase 1 of mutual commitment pairing.
func (c *PairingCoordinator) InitChallenge(sessionKeyHash []byte, clientPubKeyHex, clientCommitHex string) (*ChallengeResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()

	// 1. Check Lockout
	if now.Before(c.lockoutUntil) {
		return nil, ErrPairingLocked
	}

	// 2. Handle active challenge & Single-Pending Invariant
	if c.active != nil {
		if now.Before(c.active.ExpiresAt) && c.active.State != StateConsumed {
			// An active, unexpired challenge is in flight. Reject without consuming attempt budget!
			return nil, ErrPairingInProgress
		}
		// Prior challenge expired or unconsumed -> counts as an abandoned/timed-out failure!
		if c.active.State != StateConsumed {
			c.recordFailureLocked(now)
			if now.Before(c.lockoutUntil) {
				c.active = nil
				return nil, ErrPairingLocked
			}
		}
		c.active = nil
	}

	// 3. Decode & validate client inputs
	clientPubBytes, err := hex.DecodeString(clientPubKeyHex)
	if err != nil || len(clientPubBytes) != ed25519.PublicKeySize {
		return nil, ErrInvalidKey
	}
	clientCommitBytes, err := hex.DecodeString(clientCommitHex)
	if err != nil || len(clientCommitBytes) != 32 {
		return nil, ErrInvalidCommitment
	}

	// 4. Generate Host Ephemeral Keypair (eh, Eh) and 16-byte opening salt rh
	hostPair, err := GenerateEphemeralKeyPair()
	if err != nil {
		return nil, err
	}
	hostSalt, err := GenerateOpeningSalt()
	if err != nil {
		return nil, err
	}
	hostCommit, err := ComputeCommitment(hostPair.PublicKey, hostSalt)
	if err != nil {
		return nil, err
	}

	// 5. Generate 32-byte cryptographically secure server pairing nonce Nh
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("pairing: nonce generation error: %w", err)
	}
	nonceHex := hex.EncodeToString(nonce)

	clientPub := ed25519.PublicKey(clientPubBytes)
	clientIDDigest := sha256.Sum256(clientPub)

	c.active = &PendingPairing{
		SessionKeyHash:  sessionKeyHash,
		ClientPubKey:    clientPub,
		ClientID:        clientIDDigest[:],
		ClientCommit:    clientCommitBytes,
		HostEphemeral:   hostPair,
		HostSalt:        hostSalt,
		HostCommit:      hostCommit,
		PairingNonce:    nonce,
		PairingNonceHex: nonceHex,
		CreatedAt:       now,
		ExpiresAt:       now.Add(ChallengeTTL),
		State:           StateChallenged,
	}

	return &ChallengeResponse{
		HostID:           c.identity.HostID,
		HostPublicKey:    c.identity.PublicKey,
		HostCommitment:   hex.EncodeToString(hostCommit[:]),
		PairingNonce:     nonceHex,
		ExpiresInSeconds: int(ChallengeTTL.Seconds()),
	}, nil
}

// RevealResponse is returned to the client upon Phase 2 POST /api/pair/reveal.
type RevealResponse struct {
	HostEphemeralKey string `json:"hostEphemeralKey"`
	HostSalt         string `json:"hostSalt"`
	HostProof        string `json:"hostProof"`
	SASCode          string `json:"sasCode,omitempty"` // For internal/testing visibility
}

// Reveal handles Phase 2 ephemeral key reveal and verification.
func (c *PairingCoordinator) Reveal(sessionKeyHash []byte, nonceHex, clientEphemeralHex, clientSaltHex string) (*RevealResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	if now.Before(c.lockoutUntil) {
		return nil, ErrPairingLocked
	}

	p := c.active
	if p == nil || now.After(p.ExpiresAt) || p.PairingNonceHex != nonceHex {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, ErrPairingExpired
	}

	if p.State != StateChallenged {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, ErrWrongPairingState
	}

	// Decode client ephemeral key and salt
	clientEphemeral, err := hex.DecodeString(clientEphemeralHex)
	if err != nil || len(clientEphemeral) != 32 {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, fmt.Errorf("%w: invalid client ephemeral key", ErrInvalidTranscriptField)
	}
	clientSalt, err := hex.DecodeString(clientSaltHex)
	if err != nil || len(clientSalt) != 16 {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, fmt.Errorf("%w: invalid client salt", ErrInvalidTranscriptField)
	}

	// Verify Client Commitment: SHA-256(Ec || rc) == Commit_c
	if !VerifyCommitment(p.ClientCommit, clientEphemeral, clientSalt) {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, ErrInvalidCommitment
	}

	// Compute X25519 Shared Secret
	kShared, err := ComputeSharedSecret(p.HostEphemeral.PrivateKey, clientEphemeral)
	if err != nil {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, fmt.Errorf("pairing: ECDH key agreement failed: %w", err)
	}

	// Construct 271-Byte Transcript T
	hostPub := c.identity.PublicKeyBytes()
	hostIDDigest := sha256.Sum256(hostPub)

	transcript, err := BuildTranscript(TranscriptParams{
		HostID:     hostIDDigest[:],
		ClientID:   p.ClientID,
		CommitC:    p.ClientCommit,
		CommitH:    p.HostCommit[:],
		Ec:         clientEphemeral,
		Eh:         p.HostEphemeral.PublicKey,
		Nh:         p.PairingNonce,
		SessionKey: sessionKeyHash,
	})
	if err != nil {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, fmt.Errorf("pairing: transcript construction error: %w", err)
	}

	transcriptHash, err := ComputeTranscriptHash(transcript)
	if err != nil {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, err
	}

	// Derive SAS with unbiased rejection sampling
	num, disp, err := DeriveSASFromTranscript(p.PairingNonce, kShared, transcriptHash)
	if err != nil {
		// Entropy limit exhausted -> fail-closed restart mandated
		c.active = nil
		return nil, err
	}

	// Generate HostProof (54 bytes)
	hostProofSig := c.identity.SignHostProof(transcriptHash)

	p.ClientEphemeral = clientEphemeral
	p.ClientSalt = clientSalt
	p.KShared = kShared
	p.Transcript = transcript
	p.TranscriptHash = transcriptHash
	p.SASNumeric = num
	p.SASDisplay = disp
	p.HostProofHex = hostProofSig
	p.State = StateRevealed

	return &RevealResponse{
		HostEphemeralKey: hex.EncodeToString(p.HostEphemeral.PublicKey),
		HostSalt:         hex.EncodeToString(p.HostSalt),
		HostProof:        hostProofSig,
		SASCode:          disp,
	}, nil
}

// EnrollResponse is returned upon Phase 3 completion.
type EnrollResponse struct {
	Status   string `json:"status"`
	DeviceID string `json:"deviceId"`
}

// PrepareApproval validates the client proof and enters StatePendingApproval.
// It returns the unique RequestID, a channel to wait for host decision, and expiration time.
func (c *PairingCoordinator) PrepareApproval(sessionKeyHash []byte, deviceName, clientPubKeyHex, nonceHex, clientProofHex string) (requestID string, decisionChan <-chan bool, expiresAt time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	if now.Before(c.lockoutUntil) {
		return "", nil, time.Time{}, ErrPairingLocked
	}

	p := c.active
	if p == nil || now.After(p.ExpiresAt) || p.PairingNonceHex != nonceHex {
		c.recordFailureLocked(now)
		c.active = nil
		return "", nil, time.Time{}, ErrPairingExpired
	}

	if p.State == StatePendingApproval {
		// A request is already in flight awaiting approval. Protect in-flight request without wiping state.
		return "", nil, time.Time{}, ErrPairingInProgress
	}

	if p.State != StateRevealed {
		c.recordFailureLocked(now)
		c.active = nil
		return "", nil, time.Time{}, ErrWrongPairingState
	}

	// Verify session binding
	if len(p.SessionKeyHash) > 0 && len(sessionKeyHash) > 0 && !hmac.Equal(p.SessionKeyHash, sessionKeyHash) {
		c.recordFailureLocked(now)
		c.active = nil
		return "", nil, time.Time{}, ErrSessionMismatch
	}

	// Check client public key matches
	if hex.EncodeToString(p.ClientPubKey) != clientPubKeyHex {
		c.recordFailureLocked(now)
		c.active = nil
		return "", nil, time.Time{}, fmt.Errorf("%w: client public key changed mid-handshake", ErrInvalidKey)
	}

	// Verify ClientProof signature over 56-byte buffer BEFORE exposing to desktop!
	valid, err := VerifyClientProof(p.ClientPubKey, p.TranscriptHash, clientProofHex)
	if err != nil || !valid {
		c.recordFailureLocked(now)
		c.active = nil
		return "", nil, time.Time{}, ErrInvalidSignature
	}

	// Generate unique 16-byte random request ID
	reqIDBytes := make([]byte, 16)
	if _, err := rand.Read(reqIDBytes); err != nil {
		c.recordFailureLocked(now)
		c.active = nil
		return "", nil, time.Time{}, fmt.Errorf("pairing: failed to generate request id: %w", err)
	}

	p.RequestID = hex.EncodeToString(reqIDBytes)
	p.DeviceName = deviceName
	p.decisionChan = make(chan bool, 1)
	p.decisionMade = false
	p.State = StatePendingApproval

	if c.OnPairingPrompt != nil {
		ch := p.decisionChan
		go c.OnPairingPrompt(deviceName, hex.EncodeToString(p.ClientID), p.SASDisplay, ch)
	}

	return p.RequestID, p.decisionChan, p.ExpiresAt, nil
}

// ApprovePairing records desktop operator approval for a pending pairing request.
func (c *PairingCoordinator) ApprovePairing(requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	p := c.active
	if p == nil || p.RequestID == "" || p.RequestID != requestID || p.State != StatePendingApproval {
		return ErrStaleRequest
	}

	if now.After(p.ExpiresAt) {
		c.recordFailureLocked(now)
		c.active = nil
		return ErrPairingExpired
	}

	if p.decisionMade {
		return ErrDecisionAlreadyMade
	}

	p.decisionMade = true
	p.HostApproved = true
	select {
	case p.decisionChan <- true:
	default:
	}
	return nil
}

// DenyPairing records desktop operator denial for a pending pairing request.
func (c *PairingCoordinator) DenyPairing(requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	p := c.active
	if p == nil || p.RequestID == "" || p.RequestID != requestID || p.State != StatePendingApproval {
		return ErrStaleRequest
	}

	if now.After(p.ExpiresAt) {
		c.recordFailureLocked(now)
		c.active = nil
		return ErrPairingExpired
	}

	if p.decisionMade {
		return ErrDecisionAlreadyMade
	}

	p.decisionMade = true
	p.HostApproved = false
	c.recordFailureLocked(now)
	select {
	case p.decisionChan <- false:
	default:
	}
	return nil
}

// CompleteEnrollment finalizes device persistence once a host decision is reached.
func (c *PairingCoordinator) CompleteEnrollment(requestID string, approved bool) (*EnrollResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	p := c.active
	if p == nil || p.RequestID != requestID {
		return nil, ErrStaleRequest
	}

	if !approved {
		// Ensure failure is recorded if not already recorded by DenyPairing
		if !p.decisionMade {
			c.recordFailureLocked(now)
		}
		c.active = nil
		return nil, ErrPairingDenied
	}

	// Persist device to store with AutoApprove = false
	dev, err := c.store.Add(p.DeviceName, p.ClientPubKey, false)
	if err != nil {
		c.recordFailureLocked(now)
		c.active = nil
		return nil, fmt.Errorf("pairing: failed to enroll device: %w", err)
	}

	p.State = StateConsumed
	c.active = nil

	return &EnrollResponse{
		Status:   "paired",
		DeviceID: dev.ID,
	}, nil
}

// CancelRequest cancels the pending pairing request upon client disconnect or timeout.
func (c *PairingCoordinator) CancelRequest(requestID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	p := c.active
	if p != nil && p.RequestID == requestID {
		if !p.decisionMade {
			c.recordFailureLocked(time.Now().UTC())
		}
		c.active = nil
	}
}

// ConfirmAndEnroll handles synchronous verification and enrollment (convenience and backward compatibility).
func (c *PairingCoordinator) ConfirmAndEnroll(sessionKeyHash []byte, deviceName, clientPubKeyHex, nonceHex, clientProofHex string, hostApproved bool) (*EnrollResponse, error) {
	reqID, _, _, err := c.PrepareApproval(sessionKeyHash, deviceName, clientPubKeyHex, nonceHex, clientProofHex)
	if err != nil {
		return nil, err
	}
	if hostApproved {
		if err := c.ApprovePairing(reqID); err != nil {
			return nil, err
		}
	} else {
		if err := c.DenyPairing(reqID); err != nil {
			return nil, err
		}
	}
	return c.CompleteEnrollment(reqID, hostApproved)
}

// CancelActive cancels the active challenge (e.g. user aborts or enters "n") and records a failure.
func (c *PairingCoordinator) CancelActive(nonceHex string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.active != nil && (nonceHex == "" || c.active.PairingNonceHex == nonceHex) {
		c.recordFailureLocked(time.Now().UTC())
		c.active = nil
	}
}

// ReArmPairing manually clears lockout mode and flushes the 10-minute failure buffer.
func (c *PairingCoordinator) ReArmPairing() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lockoutUntil = time.Time{}
	c.failureTimestamps = c.failureTimestamps[:0]
	c.active = nil
}

// IsLocked reports whether pairing is currently in lockout mode and the duration remaining.
func (c *PairingCoordinator) IsLocked() (bool, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	if now.Before(c.lockoutUntil) {
		return true, c.lockoutUntil.Sub(now)
	}
	return false, 0
}

// FailureCount returns the number of failures currently recorded in the rolling 10-minute window.
func (c *PairingCoordinator) FailureCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pruneFailuresLocked(time.Now().UTC())
	return len(c.failureTimestamps)
}

// PairingStatusView represents the status displayed in desktop GUI and Wails.
type PairingStatusView struct {
	Locked           bool    `json:"locked"`
	LockoutDuration  float64 `json:"lockoutDuration"`
	Active           bool    `json:"active"`
	RequiresApproval bool    `json:"requiresApproval"`
	RequestID        string  `json:"requestId,omitempty"`
	DeviceName       string  `json:"deviceName,omitempty"`
	ClientID         string  `json:"clientId,omitempty"`
	SAS              string  `json:"sas,omitempty"`
	RemainingSeconds int     `json:"remainingSeconds"`
	FailureCount     int     `json:"failureCount"`
}

// GetPairingStatus returns the current pairing state for desktop GUI monitoring.
func (c *PairingCoordinator) GetPairingStatus() PairingStatusView {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now().UTC()
	locked := false
	lockoutDur := 0.0
	if now.Before(c.lockoutUntil) {
		locked = true
		lockoutDur = c.lockoutUntil.Sub(now).Seconds()
	}

	c.pruneFailuresLocked(now)

	status := PairingStatusView{
		Locked:          locked,
		LockoutDuration: lockoutDur,
		FailureCount:    len(c.failureTimestamps),
	}

	p := c.active
	if p != nil && now.Before(p.ExpiresAt) && p.State != StateConsumed {
		status.Active = true
		status.RemainingSeconds = int(p.ExpiresAt.Sub(now).Seconds())
		status.SAS = p.SASDisplay
		if len(p.ClientID) == 32 {
			status.ClientID = hex.EncodeToString(p.ClientID)
		}
		if p.State == StatePendingApproval && p.RequestID != "" {
			status.RequiresApproval = true
			status.RequestID = p.RequestID
			status.DeviceName = p.DeviceName
		}
	}

	return status
}

// GetActiveSAS returns the displayed SAS code of the active pairing if in StateRevealed or StatePendingApproval.
func (c *PairingCoordinator) GetActiveSAS() (string, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.active != nil && (c.active.State == StateRevealed || c.active.State == StatePendingApproval) && time.Now().UTC().Before(c.active.ExpiresAt) {
		return c.active.SASDisplay, hex.EncodeToString(c.active.ClientID), true
	}
	return "", "", false
}

// recordFailureLocked registers a pairing attempt failure in the sliding 10-minute window.
func (c *PairingCoordinator) recordFailureLocked(now time.Time) {
	c.pruneFailuresLocked(now)
	c.failureTimestamps = append(c.failureTimestamps, now)
	if len(c.failureTimestamps) >= MaxFailedAttempts {
		c.lockoutUntil = now.Add(LockoutCooldown)
	}
}

// pruneFailuresLocked removes timestamps outside the rolling 10-minute window.
func (c *PairingCoordinator) pruneFailuresLocked(now time.Time) {
	cutoff := now.Add(-FailureWindow)
	valid := c.failureTimestamps[:0]
	for _, t := range c.failureTimestamps {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	c.failureTimestamps = valid
}
