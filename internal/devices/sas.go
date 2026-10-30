package devices

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

var (
	ErrSASDerivationExhausted = errors.New("sas: all candidate integers exceeded rejection threshold; fail-closed abort mandated")
	ErrInvalidCommitment      = errors.New("sas: commitment opening verification failed")
	ErrInvalidTranscriptField = errors.New("sas: invalid transcript field size")
	ErrInvalidProofLength     = errors.New("sas: invalid proof payload length")
)

const (
	TranscriptTag             = "flitnix-sas-v1\x00"          // 15 raw bytes
	TranscriptLength          = 271                           // 15 + 8*32 = 271 raw bytes
	ExpandInfoTag             = "flitnix-sas-expand-v1\x00"   // 22 raw bytes
	HostProofTag              = "flitnix-host-proof-v1\x00"   // 22 raw bytes
	ClientProofTag            = "flitnix-client-proof-v1\x00" // 24 raw bytes
	RejectionThreshold uint32 = 4294000000
)

// EphemeralKeyPair wraps an X25519 private key and its raw 32-byte public key.
type EphemeralKeyPair struct {
	PrivateKey *ecdh.PrivateKey
	PublicKey  []byte // 32 raw bytes
}

// GenerateEphemeralKeyPair generates a fresh X25519 keypair for mutual commitment pairing.
func GenerateEphemeralKeyPair() (*EphemeralKeyPair, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sas: failed to generate X25519 key: %w", err)
	}
	return &EphemeralKeyPair{
		PrivateKey: priv,
		PublicKey:  priv.PublicKey().Bytes(),
	}, nil
}

// EphemeralKeyPairFromScalar constructs an EphemeralKeyPair from a fixed 32-byte private scalar (test-only).
func EphemeralKeyPairFromScalar(scalar []byte) (*EphemeralKeyPair, error) {
	if len(scalar) != 32 {
		return nil, errors.New("sas: scalar must be 32 bytes")
	}
	curve := ecdh.X25519()
	priv, err := curve.NewPrivateKey(scalar)
	if err != nil {
		return nil, err
	}
	return &EphemeralKeyPair{
		PrivateKey: priv,
		PublicKey:  priv.PublicKey().Bytes(),
	}, nil
}

// GenerateOpeningSalt generates a cryptographically secure 16-byte random opening salt.
func GenerateOpeningSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("sas: failed to generate salt: %w", err)
	}
	return salt, nil
}

// ComputeCommitment calculates SHA-256(ephemeralPubKey || salt).
// ephemeralPubKey must be 32 bytes and salt must be 16 bytes.
func ComputeCommitment(ephemeralPubKey, salt []byte) ([32]byte, error) {
	if len(ephemeralPubKey) != 32 {
		return [32]byte{}, fmt.Errorf("%w: ephemeral key must be 32 bytes", ErrInvalidTranscriptField)
	}
	if len(salt) != 16 {
		return [32]byte{}, fmt.Errorf("%w: opening salt must be 16 bytes", ErrInvalidTranscriptField)
	}

	h := sha256.New()
	h.Write(ephemeralPubKey)
	h.Write(salt)
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// VerifyCommitment verifies that SHA-256(ephemeralPubKey || salt) matches expectedCommitment.
func VerifyCommitment(expectedCommitment, ephemeralPubKey, salt []byte) bool {
	if len(expectedCommitment) != 32 || len(ephemeralPubKey) != 32 || len(salt) != 16 {
		return false
	}
	actual, err := ComputeCommitment(ephemeralPubKey, salt)
	if err != nil {
		return false
	}
	return hmac.Equal(actual[:], expectedCommitment)
}

// ComputeSharedSecret computes X25519(localPriv, remotePub).
func ComputeSharedSecret(localPriv *ecdh.PrivateKey, remotePub []byte) ([]byte, error) {
	curve := ecdh.X25519()
	remoteECDH, err := curve.NewPublicKey(remotePub)
	if err != nil {
		return nil, fmt.Errorf("sas: invalid remote X25519 public key: %w", err)
	}
	return localPriv.ECDH(remoteECDH)
}

// TranscriptParams bundles the inputs for constructing the 271-byte binary transcript.
type TranscriptParams struct {
	HostID     []byte // 32 bytes raw SHA-256 digest of Host public key
	ClientID   []byte // 32 bytes raw SHA-256 digest of Client public key
	CommitC    []byte // 32 bytes raw client commitment
	CommitH    []byte // 32 bytes raw host commitment
	Ec         []byte // 32 bytes raw client ephemeral public key
	Eh         []byte // 32 bytes raw host ephemeral public key
	Nh         []byte // 32 bytes raw server pairing nonce
	SessionKey []byte // 32 bytes raw session token digest
}

// BuildTranscript constructs the invariant 271-byte binary handshake transcript T.
func BuildTranscript(p TranscriptParams) ([]byte, error) {
	tag := []byte(TranscriptTag)
	if len(tag) != 15 {
		return nil, fmt.Errorf("%w: tag length %d != 15", ErrInvalidTranscriptField, len(tag))
	}
	fields := [][]byte{p.HostID, p.ClientID, p.CommitC, p.CommitH, p.Ec, p.Eh, p.Nh, p.SessionKey}
	for i, f := range fields {
		if len(f) != 32 {
			return nil, fmt.Errorf("%w: field index %d has length %d (must be 32)", ErrInvalidTranscriptField, i, len(f))
		}
	}

	buf := make([]byte, 0, TranscriptLength)
	buf = append(buf, tag...)
	for _, f := range fields {
		buf = append(buf, f...)
	}

	if len(buf) != TranscriptLength {
		return nil, fmt.Errorf("%w: built transcript length %d != %d", ErrInvalidTranscriptField, len(buf), TranscriptLength)
	}
	return buf, nil
}

// ComputeTranscriptHash returns SHA-256(transcript).
func ComputeTranscriptHash(transcript []byte) ([32]byte, error) {
	if len(transcript) != TranscriptLength {
		return [32]byte{}, fmt.Errorf("%w: expected %d bytes, got %d", ErrInvalidTranscriptField, TranscriptLength, len(transcript))
	}
	return sha256.Sum256(transcript), nil
}

// DeriveHKDF_PRK calculates PRK = HMAC-SHA256(salt=Nh, IKM=Kshared).
func DeriveHKDF_PRK(nh, kShared []byte) []byte {
	mac := hmac.New(sha256.New, nh)
	mac.Write(kShared)
	return mac.Sum(nil)
}

// BuildExpandInfo constructs the 54-byte domain-separated info buffer:
// info = "flitnix-sas-expand-v1\x00" (22B) || transcriptHash (32B).
func BuildExpandInfo(transcriptHash [32]byte) []byte {
	tag := []byte(ExpandInfoTag)
	buf := make([]byte, 0, len(tag)+len(transcriptHash))
	buf = append(buf, tag...)
	buf = append(buf, transcriptHash[:]...)
	return buf
}

// DeriveOKM calculates OKM = HMAC-SHA256(PRK, info || 0x01) (32 bytes).
func DeriveOKM(prk, info []byte) []byte {
	mac := hmac.New(sha256.New, prk)
	mac.Write(info)
	mac.Write([]byte{0x01})
	return mac.Sum(nil)
}

// DeriveSAS executes unbiased rejection sampling over the 32-byte OKM to produce
// the 6-digit integer and formatted string "XXX-YYY".
//
// If all 8 candidate 32-bit big-endian integers are >= 4,294,000,000, it returns
// ErrSASDerivationExhausted, mandating a fail-closed restart.
func DeriveSAS(okm []byte) (numeric int, display string, candidateIdx int, err error) {
	if len(okm) != 32 {
		return 0, "", -1, errors.New("sas: OKM must be 32 bytes")
	}

	for i := 0; i < 8; i++ {
		candidate := binary.BigEndian.Uint32(okm[i*4 : (i+1)*4])
		if candidate < RejectionThreshold {
			num := int(candidate % 1000000)
			str := fmt.Sprintf("%06d", num)
			disp := fmt.Sprintf("%s-%s", str[:3], str[3:])
			return num, disp, i, nil
		}
	}

	return 0, "", -1, ErrSASDerivationExhausted
}

// DeriveSASFromTranscript performs the complete HKDF extraction, expansion, and
// unbiased rejection sampling directly from Nh, Kshared, and transcriptHash.
func DeriveSASFromTranscript(nh, kShared []byte, transcriptHash [32]byte) (numeric int, display string, err error) {
	prk := DeriveHKDF_PRK(nh, kShared)
	info := BuildExpandInfo(transcriptHash)
	okm := DeriveOKM(prk, info)
	num, disp, _, err := DeriveSAS(okm)
	return num, disp, err
}

// BuildHostProofPayload constructs the 54-byte HostProof signed buffer:
// "flitnix-host-proof-v1\x00" (22B) || transcriptHash (32B).
func BuildHostProofPayload(transcriptHash [32]byte) []byte {
	tag := []byte(HostProofTag)
	buf := make([]byte, 0, len(tag)+len(transcriptHash))
	buf = append(buf, tag...)
	buf = append(buf, transcriptHash[:]...)
	return buf
}

// BuildClientProofPayload constructs the 56-byte ClientProof signed buffer:
// "flitnix-client-proof-v1\x00" (24B) || transcriptHash (32B).
func BuildClientProofPayload(transcriptHash [32]byte) []byte {
	tag := []byte(ClientProofTag)
	buf := make([]byte, 0, len(tag)+len(transcriptHash))
	buf = append(buf, tag...)
	buf = append(buf, transcriptHash[:]...)
	return buf
}

// SignClientProof signs the 56-byte ClientProof payload using clientPrivKey.
func SignClientProof(clientPrivKey ed25519.PrivateKey, transcriptHash [32]byte) string {
	payload := BuildClientProofPayload(transcriptHash)
	sig := ed25519.Sign(clientPrivKey, payload)
	return hex.EncodeToString(sig)
}

// VerifyClientProof verifies a 64-byte Ed25519 ClientProof signature over the 56-byte ClientProof payload.
func VerifyClientProof(clientPubKey ed25519.PublicKey, transcriptHash [32]byte, signatureHex string) (bool, error) {
	payload := BuildClientProofPayload(transcriptHash)
	if len(payload) != 56 {
		return false, ErrInvalidProofLength
	}
	sigBytes, err := hex.DecodeString(signatureHex)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false, ErrInvalidSignature
	}
	return ed25519.Verify(clientPubKey, payload, sigBytes), nil
}
