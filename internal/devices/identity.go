package devices

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrInvalidHostIdentity = errors.New("identity: invalid host identity format")
)

// HostIdentity manages the local desktop host's persistent Ed25519 keypair and HostID.
type HostIdentity struct {
	mu         sync.RWMutex
	filePath   string
	Version    int       `json:"version"`
	HostID     string    `json:"hostId"`
	PublicKey  string    `json:"publicKey"`  // 64-character lowercase hex (32 raw bytes)
	PrivateKey string    `json:"privateKey"` // 128-character lowercase hex (64 raw bytes)
	CreatedAt  time.Time `json:"createdAt"`

	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
}

// LoadOrCreateHostIdentity loads an existing host identity from filePath, or generates
// and persists a fresh Ed25519 keypair with strict 0600 file permissions if not present.
func LoadOrCreateHostIdentity(filePath string) (*HostIdentity, error) {
	if filePath == "" {
		// Ephemeral test identity
		pub, priv, err := GenerateKeyPair()
		if err != nil {
			return nil, err
		}
		id := Fingerprint(pub)
		return &HostIdentity{
			Version:    1,
			HostID:     id,
			PublicKey:  hex.EncodeToString(pub),
			PrivateKey: hex.EncodeToString(priv),
			CreatedAt:  time.Now().UTC(),
			pubKey:     pub,
			privKey:    priv,
		}, nil
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return nil, fmt.Errorf("identity: failed to create directory: %w", err)
	}

	data, err := os.ReadFile(filePath)
	if err == nil && len(data) > 0 {
		var h HostIdentity
		if err := json.Unmarshal(data, &h); err != nil {
			return nil, fmt.Errorf("identity: failed to unmarshal: %w", err)
		}
		h.filePath = filePath

		pubBytes, err := hex.DecodeString(h.PublicKey)
		if err != nil || len(pubBytes) != ed25519.PublicKeySize {
			return nil, ErrInvalidKey
		}
		privBytes, err := hex.DecodeString(h.PrivateKey)
		if err != nil || len(privBytes) != ed25519.PrivateKeySize {
			return nil, ErrInvalidKey
		}

		h.pubKey = ed25519.PublicKey(pubBytes)
		h.privKey = ed25519.PrivateKey(privBytes)
		computedID := Fingerprint(h.pubKey)
		if h.HostID == "" {
			h.HostID = computedID
		} else if h.HostID != computedID {
			return nil, fmt.Errorf("identity: HostID mismatch (stored %s != computed %s)", h.HostID, computedID)
		}
		return &h, nil
	}

	// Generate fresh identity
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("identity: failed to generate keypair: %w", err)
	}

	id := Fingerprint(pub)
	h := &HostIdentity{
		filePath:   filePath,
		Version:    1,
		HostID:     id,
		PublicKey:  hex.EncodeToString(pub),
		PrivateKey: hex.EncodeToString(priv),
		CreatedAt:  time.Now().UTC(),
		pubKey:     pub,
		privKey:    priv,
	}

	if err := h.Save(); err != nil {
		return nil, fmt.Errorf("identity: failed to save fresh identity: %w", err)
	}

	return h, nil
}

// Save persists the HostIdentity to disk with 0600 permissions atomically.
func (h *HostIdentity) Save() error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.filePath == "" {
		return nil
	}

	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := h.filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, h.filePath)
}

// PublicKeyBytes returns the raw 32-byte Ed25519 public key.
func (h *HostIdentity) PublicKeyBytes() []byte {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return []byte(h.pubKey)
}

// Sign signs message with the host's private key and returns lowercase hex.
func (h *HostIdentity) Sign(message []byte) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return SignMessage(h.privKey, message)
}

// SignHostProof signs the 54-byte HostProof buffer for a given transcript hash.
func (h *HostIdentity) SignHostProof(transcriptHash [32]byte) string {
	payload := BuildHostProofPayload(transcriptHash)
	return h.Sign(payload)
}

// VerifyHostProof verifies a 64-byte Ed25519 HostProof signature over the 54-byte HostProof buffer.
func (h *HostIdentity) VerifyHostProof(transcriptHash [32]byte, signatureHex string) (bool, error) {
	payload := BuildHostProofPayload(transcriptHash)
	sigBytes, err := hex.DecodeString(signatureHex)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false, ErrInvalidSignature
	}
	h.mu.RLock()
	pub := h.pubKey
	h.mu.RUnlock()
	return ed25519.Verify(pub, payload, sigBytes), nil
}
