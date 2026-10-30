package client

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
)

var (
	// ErrHostMismatch indicates that a remote host's public key differs from the pinned key in known_hosts.json (TOFU alert).
	ErrHostMismatch = errors.New("security alert: host identity key changed from pinned key (TOFU alert)")

	// ErrPairingAborted indicates that the local user declined the SAS verification prompt.
	ErrPairingAborted = errors.New("client: pairing verification rejected by user")
)

// ResolveDataDir resolves the canonical configuration and storage directory for Flitnix across platforms.
func ResolveDataDir() string {
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

// ClientIdentity manages the local client's persistent Ed25519 keypair and DeviceID.
type ClientIdentity struct {
	mu         sync.RWMutex
	filePath   string
	Version    int       `json:"version"`
	DeviceID   string    `json:"deviceId"`
	PublicKey  string    `json:"publicKey"`  // 64-character lowercase hex (32 raw bytes)
	PrivateKey string    `json:"privateKey"` // 128-character lowercase hex (64 raw bytes)
	CreatedAt  time.Time `json:"createdAt"`

	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
}

// LoadOrCreateClientIdentity loads an existing client identity from filePath, or generates
// and persists a fresh Ed25519 keypair with strict 0600 file permissions if not present.
func LoadOrCreateClientIdentity(filePath string) (*ClientIdentity, error) {
	if filePath == "" {
		filePath = filepath.Join(ResolveDataDir(), "client_identity.json")
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return nil, fmt.Errorf("identity: failed to create directory: %w", err)
	}

	data, err := os.ReadFile(filePath)
	if err == nil && len(data) > 0 {
		var c ClientIdentity
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("identity: failed to unmarshal: %w", err)
		}
		c.filePath = filePath

		pubBytes, err := hex.DecodeString(c.PublicKey)
		if err != nil || len(pubBytes) != ed25519.PublicKeySize {
			return nil, devices.ErrInvalidKey
		}
		privBytes, err := hex.DecodeString(c.PrivateKey)
		if err != nil || len(privBytes) != ed25519.PrivateKeySize {
			return nil, devices.ErrInvalidKey
		}

		c.pubKey = ed25519.PublicKey(pubBytes)
		c.privKey = ed25519.PrivateKey(privBytes)
		computedID := devices.Fingerprint(c.pubKey)
		if c.DeviceID == "" {
			c.DeviceID = computedID
		} else if c.DeviceID != computedID {
			return nil, fmt.Errorf("identity: DeviceID mismatch (stored %s != computed %s)", c.DeviceID, computedID)
		}
		return &c, nil
	}

	// Generate fresh identity
	pub, priv, err := devices.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("identity: failed to generate keypair: %w", err)
	}

	id := devices.Fingerprint(pub)
	c := &ClientIdentity{
		filePath:   filePath,
		Version:    1,
		DeviceID:   id,
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		CreatedAt:  time.Now().UTC(),
		pubKey:     pub,
		privKey:    priv,
	}

	if err := c.Save(); err != nil {
		return nil, fmt.Errorf("identity: failed to save fresh client identity: %w", err)
	}

	return c, nil
}

// Save persists the ClientIdentity to disk with 0600 permissions atomically.
func (c *ClientIdentity) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.filePath == "" {
		return nil
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := c.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, c.filePath)
}

// PublicKeyBytes returns the raw 32-byte Ed25519 public key.
func (c *ClientIdentity) PublicKeyBytes() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return []byte(c.pubKey)
}

// Sign signs message with the client's private key and returns lowercase hex.
func (c *ClientIdentity) Sign(message []byte) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return devices.SignMessage(c.privKey, message)
}

// SignClientProof signs the 56-byte ClientProof buffer for a given transcript hash.
func (c *ClientIdentity) SignClientProof(transcriptHash [32]byte) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return devices.SignClientProof(c.privKey, transcriptHash)
}

// SignAuthV2 signs the domain-separated manifest authentication payload:
// flitnix-auth-v2:<hostId>:<deviceId>:<timestampUnix>:<transferNonce>:<manifestSha256Hex>
func (c *ClientIdentity) SignAuthV2(hostID string, timestampUnix int64, transferNonce, manifestSha256Hex string) string {
	payload := devices.BuildAuthV2Payload(hostID, c.DeviceID, timestampUnix, transferNonce, manifestSha256Hex)
	return c.Sign(payload)
}

// PinnedHost represents a host that has been verified and pinned in known_hosts.json.
type PinnedHost struct {
	Name      string    `json:"name"`
	PublicKey string    `json:"publicKey"` // 64-char lowercase hex
	PairedAt  time.Time `json:"pairedAt"`
	LastSeen  time.Time `json:"lastSeen"`
}

// KnownHosts manages trusted host public keys and TOFU pinning.
type KnownHosts struct {
	mu       sync.RWMutex
	filePath string
	Version  int                   `json:"version"`
	Hosts    map[string]PinnedHost `json:"hosts"` // keyed by HostID
}

// LoadOrCreateKnownHosts loads known_hosts.json from filePath or initializes a fresh store.
func LoadOrCreateKnownHosts(filePath string) (*KnownHosts, error) {
	if filePath == "" {
		filePath = filepath.Join(ResolveDataDir(), "known_hosts.json")
	}

	kh := &KnownHosts{
		filePath: filePath,
		Version:  1,
		Hosts:    make(map[string]PinnedHost),
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return nil, fmt.Errorf("known_hosts: failed to create directory: %w", err)
	}

	data, err := os.ReadFile(filePath)
	if err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, kh); err != nil {
			return nil, fmt.Errorf("known_hosts: failed to unmarshal: %w", err)
		}
		if kh.Hosts == nil {
			kh.Hosts = make(map[string]PinnedHost)
		}
	}

	return kh, nil
}

// Save persists the known hosts to disk with 0600 permissions atomically.
func (k *KnownHosts) Save() error {
	k.mu.RLock()
	defer k.mu.RUnlock()

	if k.filePath == "" {
		return nil
	}

	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := k.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, k.filePath)
}

// Get returns the pinned host for a given HostID if present.
func (k *KnownHosts) Get(hostID string) (PinnedHost, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	h, ok := k.Hosts[hostID]
	return h, ok
}

// Pin registers or updates a host identity pinning in known_hosts.json.
func (k *KnownHosts) Pin(hostID, name, publicKeyHex string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	now := time.Now().UTC()
	existing, ok := k.Hosts[hostID]
	pairedAt := now
	if ok && !existing.PairedAt.IsZero() {
		pairedAt = existing.PairedAt
	}

	k.Hosts[hostID] = PinnedHost{
		Name:      name,
		PublicKey: publicKeyHex,
		PairedAt:  pairedAt,
		LastSeen:  now,
	}

	return k.saveLocked()
}

// Remove unpins a host from known_hosts.json.
func (k *KnownHosts) Remove(hostID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	delete(k.Hosts, hostID)
	return k.saveLocked()
}

// VerifyHost verifies that if hostID is already known, the provided publicKeyHex matches the pinned key.
// If the key has changed, it returns ErrHostMismatch (TOFU violation).
func (k *KnownHosts) VerifyHost(hostID, publicKeyHex string) error {
	k.mu.RLock()
	defer k.mu.RUnlock()

	pinned, ok := k.Hosts[hostID]
	if !ok {
		// New host; passes TOFU check
		return nil
	}

	if pinned.PublicKey != publicKeyHex {
		return fmt.Errorf("%w: pinned key %s != remote key %s", ErrHostMismatch, pinned.PublicKey, publicKeyHex)
	}

	return nil
}

// Touch updates the LastSeen timestamp for a pinned host.
func (k *KnownHosts) Touch(hostID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	h, ok := k.Hosts[hostID]
	if !ok {
		return nil
	}
	h.LastSeen = time.Now().UTC()
	k.Hosts[hostID] = h
	return k.saveLocked()
}

func (k *KnownHosts) saveLocked() error {
	if k.filePath == "" {
		return nil
	}

	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := k.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, k.filePath)
}

// HostFingerprint computes the hex SHA-256 fingerprint of a raw 32-byte public key.
func HostFingerprint(pubKey ed25519.PublicKey) string {
	sum := sha256.Sum256(pubKey)
	return hex.EncodeToString(sum[:])
}
