package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
)

var (
	ErrFirstContactSASRequired = errors.New("client: first-contact pairing requires interactive visual SAS verification")
	ErrInteractionRequired     = errors.New("client: interactive verification required for unpinned host")
)

// ConfirmSASFunc is invoked to verify the 6-digit SAS code.
// isPinned indicates whether the host public key was already verified in known_hosts.json.
type ConfirmSASFunc func(sas string, isPinned bool) (bool, error)

// PairResult contains the cryptographic details of an enrolled pairing.
type PairResult struct {
	HostID        string `json:"hostId"`
	DeviceID      string `json:"deviceId"`
	HostPublicKey string `json:"hostPublicKey"`
	SASDisplay    string `json:"sasDisplay"`
	HostName      string `json:"hostName"`
}

// Pair executes the full mutual commitment pairing handshake against a remote Flitnix host.
func Pair(ctx context.Context, target *Target, deviceName string, id *ClientIdentity, kh *KnownHosts, confirmSAS ConfirmSASFunc) (*PairResult, error) {
	if target == nil {
		return nil, errors.New("client: target is required")
	}

	if id == nil {
		var err error
		id, err = LoadOrCreateClientIdentity("")
		if err != nil {
			return nil, fmt.Errorf("client: failed to load client identity: %w", err)
		}
	}

	if kh == nil {
		var err error
		kh, err = LoadOrCreateKnownHosts("")
		if err != nil {
			return nil, fmt.Errorf("client: failed to load known hosts: %w", err)
		}
	}

	if deviceName == "" {
		hostname, _ := devices.DefaultDeviceName()
		if hostname == "" {
			hostname = "Flitnix CLI Peer"
		}
		deviceName = hostname
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	// 1. Generate client ephemeral keypair (ec, Ec) and opening salt rc
	clientEphemeral, err := devices.GenerateEphemeralKeyPair()
	if err != nil {
		return nil, fmt.Errorf("client: failed to generate ephemeral key: %w", err)
	}

	clientSalt, err := devices.GenerateOpeningSalt()
	if err != nil {
		return nil, fmt.Errorf("client: failed to generate opening salt: %w", err)
	}

	clientCommit, err := devices.ComputeCommitment(clientEphemeral.PublicKey, clientSalt)
	if err != nil {
		return nil, fmt.Errorf("client: failed to compute commitment: %w", err)
	}

	// 2. Phase 1: POST /api/pair/challenge
	chalReqBody, _ := json.Marshal(map[string]string{
		"clientPublicKey":  id.PublicKey,
		"clientCommitment": hex.EncodeToString(clientCommit[:]),
	})

	chalURL := target.BaseURL + "/api/pair/challenge"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chalURL, bytes.NewReader(chalReqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+target.Token())
	req.Header.Set("Host", target.Host)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: pair challenge request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, errors.New("client: pairing is temporarily locked or another attempt is in progress")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("client: pairing challenge rejected with HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var chalResp devices.ChallengeResponse
	if err := json.Unmarshal(bodyBytes, &chalResp); err != nil {
		return nil, fmt.Errorf("client: invalid challenge response JSON: %w", err)
	}

	// 3. TOFU Pinning Check: if host was seen before, assert its public key has not changed
	if err := kh.VerifyHost(chalResp.HostID, chalResp.HostPublicKey); err != nil {
		return nil, err
	}
	_, isPinned := kh.Get(chalResp.HostID)

	// 4. Phase 2: POST /api/pair/reveal
	revReqBody, _ := json.Marshal(map[string]string{
		"pairingNonce":       chalResp.PairingNonce,
		"clientEphemeralKey": hex.EncodeToString(clientEphemeral.PublicKey),
		"clientSalt":         hex.EncodeToString(clientSalt),
	})

	revURL := target.BaseURL + "/api/pair/reveal"
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, revURL, bytes.NewReader(revReqBody))
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Authorization", "Bearer "+target.Token())
	req2.Header.Set("Host", target.Host)
	req2.Header.Set("Content-Type", "application/json")

	resp2, err := httpClient.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("client: pair reveal request failed: %w", err)
	}
	defer resp2.Body.Close()

	bodyBytes2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("client: pairing reveal rejected with HTTP %d: %s", resp2.StatusCode, string(bodyBytes2))
	}

	var revResp devices.RevealResponse
	if err := json.Unmarshal(bodyBytes2, &revResp); err != nil {
		return nil, fmt.Errorf("client: invalid reveal response JSON: %w", err)
	}

	// 5. Verify Host Commitment: SHA-256(Eh || rh) == hostCommitment
	hostEphemeralBytes, err := hex.DecodeString(revResp.HostEphemeralKey)
	if err != nil || len(hostEphemeralBytes) != 32 {
		return nil, errors.New("client: invalid host ephemeral key in reveal response")
	}
	hostSaltBytes, err := hex.DecodeString(revResp.HostSalt)
	if err != nil || len(hostSaltBytes) != 16 {
		return nil, errors.New("client: invalid host salt in reveal response")
	}
	hostCommitBytes, err := hex.DecodeString(chalResp.HostCommitment)
	if err != nil || len(hostCommitBytes) != 32 {
		return nil, errors.New("client: invalid host commitment in challenge response")
	}

	if !devices.VerifyCommitment(hostCommitBytes, hostEphemeralBytes, hostSaltBytes) {
		return nil, errors.New("security alert: host commitment verification failed (ephemeral key tampered)")
	}

	// 6. Compute X25519 Shared Secret
	kShared, err := devices.ComputeSharedSecret(clientEphemeral.PrivateKey, hostEphemeralBytes)
	if err != nil {
		return nil, fmt.Errorf("client: key agreement failed: %w", err)
	}

	// 7. Construct 271-Byte Transcript T and compute hash
	hostPubBytes, err := hex.DecodeString(chalResp.HostPublicKey)
	if err != nil || len(hostPubBytes) != ed25519.PublicKeySize {
		return nil, devices.ErrInvalidKey
	}
	hostIDDigest := sha256.Sum256(hostPubBytes)
	clientIDDigest := sha256.Sum256(id.PublicKeyBytes())
	nonceBytes, err := hex.DecodeString(chalResp.PairingNonce)
	if err != nil || len(nonceBytes) != 32 {
		return nil, errors.New("client: invalid pairing nonce")
	}

	// Session key hash is SHA-256 of secret token
	tokenHash := sha256.Sum256([]byte(target.Token()))

	transcript, err := devices.BuildTranscript(devices.TranscriptParams{
		HostID:     hostIDDigest[:],
		ClientID:   clientIDDigest[:],
		CommitC:    clientCommit[:],
		CommitH:    hostCommitBytes,
		Ec:         clientEphemeral.PublicKey,
		Eh:         hostEphemeralBytes,
		Nh:         nonceBytes,
		SessionKey: tokenHash[:],
	})
	if err != nil {
		return nil, fmt.Errorf("client: transcript build failed: %w", err)
	}

	transcriptHash, err := devices.ComputeTranscriptHash(transcript)
	if err != nil {
		return nil, err
	}

	// 8. Verify HostProof signature over exact 54-byte buffer
	hostProofPayload := devices.BuildHostProofPayload(transcriptHash)
	hostProofSigBytes, err := hex.DecodeString(revResp.HostProof)
	if err != nil || len(hostProofSigBytes) != ed25519.SignatureSize {
		return nil, errors.New("client: invalid host proof signature encoding")
	}
	if !ed25519.Verify(ed25519.PublicKey(hostPubBytes), hostProofPayload, hostProofSigBytes) {
		return nil, errors.New("security alert: HostProof signature verification failed")
	}

	// 9. Derive SAS with unbiased rejection sampling
	_, sasDisplay, err := devices.DeriveSASFromTranscript(nonceBytes, kShared, transcriptHash)
	if err != nil {
		return nil, fmt.Errorf("client: SAS derivation failed: %w", err)
	}

	// 10. Interactive Out-of-Band Visual SAS Verification
	if confirmSAS != nil {
		confirmed, err := confirmSAS(sasDisplay, isPinned)
		if err != nil {
			return nil, err
		}
		if !confirmed {
			return nil, ErrPairingAborted
		}
	}

	// 11. Phase 3: Client confirms and signs ClientProof (56 bytes)
	clientProofSig := id.SignClientProof(transcriptHash)

	pairReqBody, _ := json.Marshal(map[string]string{
		"deviceName":      deviceName,
		"clientPublicKey": id.PublicKey,
		"pairingNonce":    chalResp.PairingNonce,
		"clientProof":     clientProofSig,
	})

	pairURL := target.BaseURL + "/api/pair/request"
	req3, err := http.NewRequestWithContext(ctx, http.MethodPost, pairURL, bytes.NewReader(pairReqBody))
	if err != nil {
		return nil, err
	}
	req3.Header.Set("Authorization", "Bearer "+target.Token())
	req3.Header.Set("Host", target.Host)
	req3.Header.Set("Content-Type", "application/json")

	resp3, err := httpClient.Do(req3)
	if err != nil {
		return nil, fmt.Errorf("client: pair confirmation request failed: %w", err)
	}
	defer resp3.Body.Close()

	bodyBytes3, _ := io.ReadAll(resp3.Body)
	if resp3.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("client: pairing confirmation rejected with HTTP %d: %s", resp3.StatusCode, string(bodyBytes3))
	}

	var enrollResp devices.EnrollResponse
	if err := json.Unmarshal(bodyBytes3, &enrollResp); err != nil {
		return nil, fmt.Errorf("client: invalid enroll response JSON: %w", err)
	}

	// 12. Pin Host in known_hosts.json
	if err := kh.Pin(chalResp.HostID, target.Host, chalResp.HostPublicKey); err != nil {
		return nil, fmt.Errorf("client: failed to pin host: %w", err)
	}

	return &PairResult{
		HostID:        chalResp.HostID,
		DeviceID:      enrollResp.DeviceID,
		HostPublicKey: chalResp.HostPublicKey,
		SASDisplay:    sasDisplay,
		HostName:      target.Host,
	}, nil
}
