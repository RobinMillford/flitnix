package devices

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	ErrDeviceNotFound    = errors.New("devices: device not found")
	ErrInvalidKey        = errors.New("devices: invalid ed25519 public key")
	ErrInvalidSignature  = errors.New("devices: invalid signature")
	ErrReplayDetected    = errors.New("devices: authentication challenge replay detected")
	ErrChallengeExpired  = errors.New("devices: authentication challenge timestamp expired or clock skew")
	ErrChallengeNotFound = errors.New("devices: transfer challenge not found")
	ErrChallengeConsumed = errors.New("devices: transfer challenge already consumed")
)

// TransferChallenge represents a server-issued challenge nonce for transfer authorization.
type TransferChallenge struct {
	HostID   string
	DeviceID string
	Nonce    string
	IssuedAt time.Time
	Consumed bool
}

// Device represents a cryptographically paired personal device.
type Device struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	PublicKey   string    `json:"publicKey"` // hex-encoded Ed25519 public key
	PairedAt    time.Time `json:"pairedAt"`
	LastSeen    time.Time `json:"lastSeen"`
	AutoApprove bool      `json:"autoApprove"`
}

// Store manages persistent trusted devices.
type Store struct {
	mu           sync.RWMutex
	filePath     string
	devices      map[string]Device
	noncesMu     sync.Mutex
	usedNonces   map[string]time.Time
	challengesMu sync.Mutex
	challenges   map[string]*TransferChallenge
}

// NewStore initializes a persistent Store from the specified JSON file path.
func NewStore(filePath string) (*Store, error) {
	s := &Store{
		filePath:   filePath,
		devices:    make(map[string]Device),
		usedNonces: make(map[string]time.Time),
		challenges: make(map[string]*TransferChallenge),
	}

	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
			return nil, fmt.Errorf("devices: failed to create directory: %w", err)
		}
		if data, err := os.ReadFile(filePath); err == nil && len(data) > 0 {
			var list []Device
			if err := json.Unmarshal(data, &list); err == nil {
				for _, d := range list {
					s.devices[d.ID] = d
				}
			}
		}
	}

	return s, nil
}

// Fingerprint returns the hex SHA-256 fingerprint of a public key.
func Fingerprint(pubKey ed25519.PublicKey) string {
	sum := sha256.Sum256(pubKey)
	return hex.EncodeToString(sum[:])
}

// Add registers a new trusted device with its Ed25519 public key.
func (s *Store) Add(name string, pubKey ed25519.PublicKey, autoApprove bool) (Device, error) {
	if len(pubKey) != ed25519.PublicKeySize {
		return Device{}, ErrInvalidKey
	}

	id := Fingerprint(pubKey)
	now := time.Now().UTC()

	dev := Device{
		ID:          id,
		Name:        name,
		PublicKey:   hex.EncodeToString(pubKey),
		PairedAt:    now,
		LastSeen:    now,
		AutoApprove: autoApprove,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.devices[id] = dev
	if err := s.saveLocked(); err != nil {
		return Device{}, err
	}
	return dev, nil
}

// List returns all registered devices sorted by pairing time.
func (s *Store) List() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].PairedAt.Before(out[j].PairedAt)
	})
	return out
}

// Get finds a device by its ID fingerprint.
func (s *Store) Get(id string) (Device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	d, ok := s.devices[id]
	return d, ok
}

// Remove unpairs and deletes a trusted device.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.devices[id]; !ok {
		return ErrDeviceNotFound
	}
	delete(s.devices, id)
	return s.saveLocked()
}

// Touch updates the LastSeen timestamp of a device.
func (s *Store) Touch(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return ErrDeviceNotFound
	}
	d.LastSeen = time.Now().UTC()
	s.devices[id] = d
	return s.saveLocked()
}

// SetAutoApprove toggles the automatic transfer approval for a device.
func (s *Store) SetAutoApprove(id string, autoApprove bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return ErrDeviceNotFound
	}
	d.AutoApprove = autoApprove
	s.devices[id] = d
	return s.saveLocked()
}

// VerifySignature verifies that signatureHex over message was signed by deviceID's public key.
func (s *Store) VerifySignature(deviceID string, message []byte, signatureHex string) (bool, error) {
	s.mu.RLock()
	d, ok := s.devices[deviceID]
	s.mu.RUnlock()

	if !ok {
		return false, ErrDeviceNotFound
	}

	pubBytes, err := hex.DecodeString(d.PublicKey)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return false, ErrInvalidKey
	}

	sigBytes, err := hex.DecodeString(signatureHex)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false, ErrInvalidSignature
	}

	valid := ed25519.Verify(ed25519.PublicKey(pubBytes), message, sigBytes)
	if valid {
		_ = s.Touch(deviceID)
	}
	return valid, nil
}

// saveLocked persists the store to disk. Caller must hold s.mu.
func (s *Store) saveLocked() error {
	if s.filePath == "" {
		return nil
	}

	list := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		list = append(list, d)
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := s.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.filePath)
}

// ChallengePayload builds a deterministic challenge message for device authentication.
func ChallengePayload(deviceID, nonce string, timestamp time.Time) []byte {
	return []byte(fmt.Sprintf("flitnix-auth-v1:%s:%d:%s", deviceID, timestamp.Unix(), nonce))
}

// VerifyAuthChallenge verifies an Ed25519 signature over a time-bound challenge and prevents replay attacks.
func (s *Store) VerifyAuthChallenge(deviceID, nonce string, timestamp time.Time, signatureHex string, maxDrift time.Duration) (bool, error) {
	if nonce == "" {
		return false, errors.New("devices: empty nonce")
	}
	if maxDrift <= 0 {
		maxDrift = 2 * time.Minute
	}

	now := time.Now().UTC()
	diff := now.Sub(timestamp)
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDrift {
		return false, ErrChallengeExpired
	}

	s.noncesMu.Lock()
	if s.usedNonces == nil {
		s.usedNonces = make(map[string]time.Time)
	}
	cutoff := now.Add(-2 * maxDrift)
	for n, t := range s.usedNonces {
		if t.Before(cutoff) {
			delete(s.usedNonces, n)
		}
	}
	if _, exists := s.usedNonces[nonce]; exists {
		s.noncesMu.Unlock()
		return false, ErrReplayDetected
	}
	s.noncesMu.Unlock()

	payload := ChallengePayload(deviceID, nonce, timestamp)
	valid, err := s.VerifySignature(deviceID, payload, signatureHex)
	if err != nil || !valid {
		return false, err
	}

	s.noncesMu.Lock()
	s.usedNonces[nonce] = now
	s.noncesMu.Unlock()

	return true, nil
}

// GenerateKeyPair generates an Ed25519 keypair for pairing.
func GenerateKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(nil)
}

// SignMessage signs message with private key and returns lowercase hex signature.
func SignMessage(privKey ed25519.PrivateKey, message []byte) string {
	sig := ed25519.Sign(privKey, message)
	return hex.EncodeToString(sig)
}

// BuildSrvChalPayload builds the server challenge string to sign in POST /api/auth/challenge:
// flitnix-srv-chal:<hostId>:<clientId>:<transferNonce>:<timestampUnix>
func BuildSrvChalPayload(hostID, clientID, transferNonce string, timestampUnix int64) []byte {
	return []byte(fmt.Sprintf("flitnix-srv-chal:%s:%s:%s:%d", hostID, clientID, transferNonce, timestampUnix))
}

// BuildAuthV2Payload builds the manifest authorization payload:
// flitnix-auth-v2:<hostId>:<deviceId>:<timestampUnix>:<transferNonce>:<manifestSha256Hex>
func BuildAuthV2Payload(hostID, deviceID string, timestampUnix int64, transferNonce, manifestSha256Hex string) []byte {
	return []byte(fmt.Sprintf("flitnix-auth-v2:%s:%s:%d:%s:%s", hostID, deviceID, timestampUnix, transferNonce, manifestSha256Hex))
}

// CreateTransferChallenge generates a 32-byte secure random nonce, records it, and returns the nonce and timestamp.
func (s *Store) CreateTransferChallenge(hostID, deviceID string) (string, int64, error) {
	s.challengesMu.Lock()
	defer s.challengesMu.Unlock()

	now := time.Now().UTC()
	cutoff := now.Add(-2 * time.Minute)
	for n, c := range s.challenges {
		if c.IssuedAt.Before(cutoff) {
			delete(s.challenges, n)
		}
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", 0, fmt.Errorf("devices: failed to generate challenge nonce: %w", err)
	}
	nonce := hex.EncodeToString(b)
	ts := now.Unix()

	s.challenges[nonce] = &TransferChallenge{
		HostID:   hostID,
		DeviceID: deviceID,
		Nonce:    nonce,
		IssuedAt: now,
		Consumed: false,
	}

	return nonce, ts, nil
}

// ValidateAndConsumeTransferChallenge verifies that the challenge nonce was issued by this server instance,
// matches hostID and deviceID, has not expired (<= maxDrift), and has not been consumed yet.
// If valid, it atomically marks the challenge as consumed.
func (s *Store) ValidateAndConsumeTransferChallenge(hostID, deviceID, nonce string, timestampUnix int64, maxDrift time.Duration) error {
	if nonce == "" {
		return errors.New("devices: empty challenge nonce")
	}
	if maxDrift <= 0 {
		maxDrift = 60 * time.Second
	}

	now := time.Now().UTC()
	claimedTime := time.Unix(timestampUnix, 0).UTC()
	diff := now.Sub(claimedTime)
	if diff < 0 {
		diff = -diff
	}
	if diff > maxDrift {
		return ErrChallengeExpired
	}

	s.challengesMu.Lock()
	defer s.challengesMu.Unlock()

	cutoff := now.Add(-2 * maxDrift)
	for n, c := range s.challenges {
		if c.IssuedAt.Before(cutoff) {
			delete(s.challenges, n)
		}
	}

	chal, exists := s.challenges[nonce]
	if !exists {
		return ErrChallengeNotFound
	}

	if chal.HostID != hostID || chal.DeviceID != deviceID {
		return errors.New("devices: challenge binding mismatch")
	}

	if chal.Consumed {
		return ErrChallengeConsumed
	}

	chal.Consumed = true
	return nil
}

// DefaultDeviceName returns a readable default device name using the OS hostname.
func DefaultDeviceName() (string, error) {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "Flitnix Peer", nil
	}
	return name, nil
}
