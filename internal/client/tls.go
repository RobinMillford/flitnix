package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/server"
)

var (
	ErrPeerHostKeyMismatch = errors.New("client: remote peer TLS public key or hostId does not match expected identity")
)

// PeerProbeInfo describes the cryptographic identity inspected over TLS 1.3.
type PeerProbeInfo struct {
	Address   string `json:"address"`
	HostID    string `json:"hostId"`
	PublicKey string `json:"publicKey"`
	TLSActive bool   `json:"tlsActive"`
	Reachable bool   `json:"reachable"`
}

func verifyLeafCertificateIdentity(rawCerts [][]byte, expectedHostID, expectedPubHex string) (hostID, pubHex string, err error) {
	if len(rawCerts) == 0 {
		return "", "", errors.New("client: peer presented no TLS certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return "", "", fmt.Errorf("client: parse peer certificate: %w", err)
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return "", "", fmt.Errorf("client: invalid self-signed peer certificate signature: %w", err)
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return "", "", errors.New("client: peer certificate must carry an Ed25519 key")
	}
	sum := sha256.Sum256(pub)
	hostID = hex.EncodeToString(sum[:])
	pubHex = hex.EncodeToString(pub)

	if expectedHostID != "" && !strings.EqualFold(hostID, expectedHostID) {
		return "", "", fmt.Errorf("%w: expected hostId=%s got=%s", ErrPeerHostKeyMismatch, expectedHostID, hostID)
	}
	if expectedPubHex != "" && !strings.EqualFold(pubHex, expectedPubHex) {
		return "", "", fmt.Errorf("%w: public key mismatch", ErrPeerHostKeyMismatch)
	}
	return hostID, pubHex, nil
}

// NewPeerHTTPClient builds an HTTP client enforcing TLS 1.3 and pinning expectedHostID/expectedPubHex.
func NewPeerHTTPClient(expectedHostID, expectedPubHex string, onVerified func(hostID, pubHex string)) *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       16,
		WriteBufferSize:       256 * 1024,
		ReadBufferSize:        256 * 1024,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, // Pin verification enforced below via VerifyPeerCertificate
			ClientSessionCache: tls.NewLRUClientSessionCache(64),
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				hid, pk, err := verifyLeafCertificateIdentity(rawCerts, expectedHostID, expectedPubHex)
				if err != nil {
					return err
				}
				if onVerified != nil {
					onVerified(hid, pk)
				}
				return nil
			},
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   0,
	}
}

// ProbePeerTLS connects to addr (host:port) over TLS 1.3 and verifies its Ed25519 certificate & /healthz.
func ProbePeerTLS(ctx context.Context, addr string) (PeerProbeInfo, error) {
	var verifiedID, verifiedPub string
	hc := NewPeerHTTPClient("", "", func(hid, pk string) {
		verifiedID = hid
		verifiedPub = pk
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/healthz", nil)
	if err != nil {
		return PeerProbeInfo{}, err
	}
	req.Header.Set("Host", addr)
	resp, err := hc.Do(req)
	if err != nil {
		return PeerProbeInfo{}, fmt.Errorf("client: TLS probe failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return PeerProbeInfo{}, fmt.Errorf("client: /healthz returned HTTP %d", resp.StatusCode)
	}
	return PeerProbeInfo{
		Address:   addr,
		HostID:    verifiedID,
		PublicKey: verifiedPub,
		TLSActive: true,
		Reachable: true,
	}, nil
}

// ConnectWithShortCode redeems a 6-digit connection code over httpClient (TLS 1.3) and returns a Target.
func ConnectWithShortCode(ctx context.Context, httpClient *http.Client, baseURL, hostAuthority, code string) (*Target, string, error) {
	if httpClient == nil {
		httpClient = NewClient(&Target{}).httpClient
	}
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/connect-code", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Host", hostAuthority)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("client: connect-code request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &errBody)
		if errBody.Error != "" {
			return nil, "", errors.New(errBody.Error)
		}
		return nil, "", fmt.Errorf("client: connect-code returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Token  string `json:"token"`
		HostID string `json:"hostId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return nil, "", errors.New("client: invalid connect-code response")
	}
	return &Target{BaseURL: baseURL, Host: hostAuthority, token: out.Token}, out.HostID, nil
}

// ConnectAsTrustedPeer authenticates using an already-enrolled HostIdentity / ClientIdentity via /api/device-sessions.
func ConnectAsTrustedPeer(ctx context.Context, httpClient *http.Client, baseURL, hostAuthority, deviceID string, privKey ed25519.PrivateKey) (*Target, error) {
	chalBody, _ := json.Marshal(map[string]string{"deviceId": deviceID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/device-sessions/challenge", bytes.NewReader(chalBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Host", hostAuthority)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: trusted session challenge failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &errBody)
		if errBody.Error != "" {
			return nil, errors.New(errBody.Error)
		}
		return nil, fmt.Errorf("client: trusted session challenge HTTP %d", resp.StatusCode)
	}
	var chal struct {
		HostID    string `json:"hostId"`
		DeviceID  string `json:"deviceId"`
		Nonce     string `json:"nonce"`
		Timestamp int64  `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &chal); err != nil {
		return nil, err
	}
	payload := devices.BuildDeviceSessionAuthPayload(chal.HostID, chal.DeviceID, chal.Timestamp, chal.Nonce)
	sig := devices.SignMessage(privKey, payload)
	verBody, _ := json.Marshal(map[string]any{
		"deviceId":  chal.DeviceID,
		"hostId":    chal.HostID,
		"nonce":     chal.Nonce,
		"timestamp": chal.Timestamp,
		"signature": sig,
	})
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/device-sessions/verify", bytes.NewReader(verBody))
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Host", hostAuthority)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := httpClient.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("client: trusted session verify failed: %w", err)
	}
	defer resp2.Body.Close()
	raw2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw2, &errBody)
		if errBody.Error != "" {
			return nil, errors.New(errBody.Error)
		}
		return nil, fmt.Errorf("client: trusted session verify HTTP %d", resp2.StatusCode)
	}
	var verified struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw2, &verified); err != nil || verified.Token == "" {
		return nil, errors.New("client: invalid trusted session verify response")
	}
	return &Target{BaseURL: baseURL, Host: hostAuthority, token: verified.Token}, nil
}

// ConnectAsVisiblePeer connects over TLS 1.3 to an open-visibility peer (/api/peer/challenge + /api/peer/connect).
func ConnectAsVisiblePeer(ctx context.Context, httpClient *http.Client, baseURL, hostAuthority, deviceName string, localHostID *devices.HostIdentity) (*Target, error) {
	if localHostID == nil {
		return nil, errors.New("client: local host identity required")
	}
	privBytes, err := hex.DecodeString(localHostID.PrivateKey)
	if err != nil || len(privBytes) != ed25519.PrivateKeySize {
		return nil, errors.New("client: invalid local private key")
	}
	priv := ed25519.PrivateKey(privBytes)

	chalBody, _ := json.Marshal(map[string]string{
		"clientHostId":    localHostID.HostID,
		"clientPublicKey": localHostID.PublicKey,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/peer/challenge", bytes.NewReader(chalBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Host", hostAuthority)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: peer challenge failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &errBody)
		if errBody.Error != "" {
			return nil, errors.New(errBody.Error)
		}
		return nil, fmt.Errorf("client: peer challenge HTTP %d", resp.StatusCode)
	}
	var chal struct {
		HostID    string `json:"hostId"`
		Nonce     string `json:"nonce"`
		Timestamp int64  `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &chal); err != nil {
		return nil, err
	}
	payload := server.BuildPeerAuthPayload(chal.HostID, localHostID.HostID, chal.Timestamp, chal.Nonce)
	sig := hex.EncodeToString(ed25519.Sign(priv, payload))
	connBody, _ := json.Marshal(map[string]any{
		"hostId":          chal.HostID,
		"clientHostId":    localHostID.HostID,
		"clientPublicKey": localHostID.PublicKey,
		"deviceName":      deviceName,
		"nonce":           chal.Nonce,
		"timestamp":       chal.Timestamp,
		"signature":       sig,
	})
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/peer/connect", bytes.NewReader(connBody))
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Host", hostAuthority)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := httpClient.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("client: peer connect failed: %w", err)
	}
	defer resp2.Body.Close()
	raw2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw2, &errBody)
		if errBody.Error != "" {
			return nil, errors.New(errBody.Error)
		}
		return nil, fmt.Errorf("client: peer connect HTTP %d", resp2.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw2, &out); err != nil || out.Token == "" {
		return nil, errors.New("client: invalid peer connect response")
	}
	return &Target{BaseURL: baseURL, Host: hostAuthority, token: out.Token}, nil
}
