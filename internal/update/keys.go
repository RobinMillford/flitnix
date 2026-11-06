package update

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
)

// DefaultReleasePublicKeyHex is the official Ed25519 public key used to verify
// Flitnix release checksum signatures (SHA256SUMS.txt.sig).
// Generated specifically for Flitnix release integrity verification.
const DefaultReleasePublicKeyHex = "d872c011985396a843e990c74f514d7a783709ec557bc7a61d15264b63004e0e"

// VerifySignature verifies that the detached signature is a valid Ed25519 signature
// over the data using the provided or default public key.
func VerifySignature(data []byte, signatureHex string, publicKeyHex string) error {
	if publicKeyHex == "" {
		publicKeyHex = DefaultReleasePublicKeyHex
	}

	pubBytes, err := hex.DecodeString(publicKeyHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid ed25519 public key: %w", err)
	}

	sigBytes, err := hex.DecodeString(signatureHex)
	if err != nil {
		return fmt.Errorf("invalid signature hex encoding: %w", err)
	}

	if len(sigBytes) != ed25519.SignatureSize {
		return errors.New("signature length mismatch for ed25519")
	}

	if !ed25519.Verify(pubBytes, data, sigBytes) {
		return errors.New("ed25519 signature verification failed: signature does not match release data")
	}

	return nil
}
