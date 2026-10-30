package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

var (
	// ErrUnauthorized indicates that the session token was rejected by the server.
	ErrUnauthorized = errors.New("client: unauthorized (token invalid, expired, or revoked)")

	// ErrSessionExpired indicates that the remote session has explicitly expired.
	ErrSessionExpired = errors.New("client: remote session has expired")

	// ErrRejected indicates that the recipient declined the transfer request.
	ErrRejected = errors.New("client: transfer was rejected by recipient")

	// ErrApprovalTimeout indicates that the recipient did not approve the transfer within the allowed time.
	ErrApprovalTimeout = errors.New("client: approval timed out waiting for recipient")

	// ErrBatchNotFound indicates that the requested batch was not found on the server.
	ErrBatchNotFound = errors.New("client: batch not found on server")

	// ErrIntegrityMismatch indicates that the server's received SHA-256 digest does not match the local digest.
	ErrIntegrityMismatch = errors.New("client: file integrity check failed (SHA-256 mismatch)")

	// ErrSizeMismatch indicates that bytes written on server do not match the local file size.
	ErrSizeMismatch = errors.New("client: file size mismatch")

	// ErrServerFault indicates an unexpected HTTP error from the server.
	ErrServerFault = errors.New("client: server error")

	// ErrConflict indicates an item or batch could not be modified due to its current state.
	ErrConflict = errors.New("client: item cannot be canceled (already completed or terminal)")
)

// ItemResult holds the verification details for a completed item transfer.
type ItemResult struct {
	Written     int64
	SHA256      string
	LocalSHA256 string
	Verified    bool
	Batch       transfer.BatchView
}

// Client executes outbound transfers against a remote Flitnix HTTP server.
type Client struct {
	target     *Target
	httpClient *http.Client
}

// NewClient constructs an outbound transfer client for the given target.
func NewClient(target *Target) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &Client{
		target: target,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   0, // Streaming requests use context deadlines rather than a blanket client timeout
		},
	}
}

// Target returns the connection target.
func (c *Client) Target() *Target {
	return c.target
}

// Ping performs a lightweight reachability test against the server's /ping endpoint.
func (c *Client) Ping(ctx context.Context) error {
	reqURL := c.target.BaseURL + "/ping"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Host", c.target.Host)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("client: cannot connect to recipient at %s: %w", c.target.Host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("client: recipient responded with HTTP %d to ping", resp.StatusCode)
	}
	return nil
}

// AuthHeaders holds the headers for authenticating a transfer manifest.
type AuthHeaders struct {
	DeviceID  string
	Nonce     string
	Timestamp int64
	Signature string
}

// TransferChallengeResponse holds the response from POST /api/auth/challenge.
type TransferChallengeResponse struct {
	HostID        string `json:"hostId"`
	DeviceID      string `json:"deviceId"`
	TransferNonce string `json:"transferNonce"`
	Timestamp     int64  `json:"timestamp"`
	HostSignature string `json:"hostSignature"`
}

// RequestAuthChallenge requests a fresh transfer challenge from POST /api/auth/challenge.
func (c *Client) RequestAuthChallenge(ctx context.Context, deviceID string) (*TransferChallengeResponse, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"deviceId": deviceID,
	})
	reqURL := c.target.BaseURL + "/api/auth/challenge"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.target.Token())
	req.Header.Set("Host", c.target.Host)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: auth challenge request failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("client: device not enrolled on host")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("client: auth challenge returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res TransferChallengeResponse
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return nil, fmt.Errorf("client: invalid challenge response JSON: %w", err)
	}
	return &res, nil
}

// DeclareBatch submits a transfer manifest to POST /api/batches.
func (c *Client) DeclareBatch(ctx context.Context, manifest transfer.BatchManifest) (transfer.BatchView, error) {
	return c.DeclareBatchWithAuth(ctx, manifest, nil)
}

// DeclareBatchWithAuth submits a transfer manifest to POST /api/batches with optional trusted-device auth headers.
func (c *Client) DeclareBatchWithAuth(ctx context.Context, manifest transfer.BatchManifest, auth *AuthHeaders) (transfer.BatchView, error) {
	bodyBytes, err := json.Marshal(manifest)
	if err != nil {
		return transfer.BatchView{}, fmt.Errorf("client: failed to marshal manifest: %w", err)
	}

	reqURL := c.target.BaseURL + "/api/batches"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return transfer.BatchView{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.target.Token())
	req.Header.Set("Host", c.target.Host)
	req.Header.Set("Content-Type", "application/json")

	if auth != nil {
		req.Header.Set("X-Flitnix-Device-ID", auth.DeviceID)
		req.Header.Set("X-Flitnix-Nonce", auth.Nonce)
		req.Header.Set("X-Flitnix-Timestamp", strconv.FormatInt(auth.Timestamp, 10))
		req.Header.Set("X-Flitnix-Signature", auth.Signature)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return transfer.BatchView{}, fmt.Errorf("client: declare batch request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		if strings.Contains(string(respBody), "expired") {
			return transfer.BatchView{}, ErrSessionExpired
		}
		return transfer.BatchView{}, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusCreated {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &errResp)
		msg := errResp.Error
		if msg == "" {
			msg = string(respBody)
		}
		return transfer.BatchView{}, fmt.Errorf("%w: HTTP %d: %s", ErrServerFault, resp.StatusCode, msg)
	}

	var view transfer.BatchView
	if err := json.Unmarshal(respBody, &view); err != nil {
		return transfer.BatchView{}, fmt.Errorf("client: invalid batch view response JSON: %w", err)
	}
	return view, nil
}

// PollBatch polls GET /api/batches/{id} to inspect the live batch and approval state.
func (c *Client) PollBatch(ctx context.Context, batchID string) (transfer.BatchView, error) {
	reqURL := fmt.Sprintf("%s/api/batches/%s", c.target.BaseURL, url.PathEscape(batchID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return transfer.BatchView{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.target.Token())
	req.Header.Set("Host", c.target.Host)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return transfer.BatchView{}, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		if strings.Contains(string(respBody), "expired") {
			return transfer.BatchView{}, ErrSessionExpired
		}
		return transfer.BatchView{}, ErrUnauthorized
	}
	if resp.StatusCode == http.StatusNotFound {
		return transfer.BatchView{}, ErrBatchNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return transfer.BatchView{}, fmt.Errorf("%w: HTTP %d", ErrServerFault, resp.StatusCode)
	}

	var view transfer.BatchView
	if err := json.Unmarshal(respBody, &view); err != nil {
		return transfer.BatchView{}, fmt.Errorf("client: invalid batch response JSON: %w", err)
	}
	return view, nil
}

// WaitForApproval polls GET /api/batches/{id} until the recipient approves or rejects the transfer.
func (c *Client) WaitForApproval(ctx context.Context, batchID string, pollInterval time.Duration) (transfer.BatchView, error) {
	if pollInterval <= 0 {
		pollInterval = 400 * time.Millisecond
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		view, err := c.PollBatch(ctx, batchID)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
				return transfer.BatchView{}, ErrApprovalTimeout
			}
			return transfer.BatchView{}, err
		}

		if view.ApprovalState == transfer.ApprovalApproved || view.State == transfer.BatchStateApproved || view.State == transfer.BatchStateQueued {
			return view, nil
		}
		if view.ApprovalState == transfer.ApprovalRejected || view.State == transfer.BatchStateRejected {
			return view, ErrRejected
		}
		if view.State == transfer.BatchStateCanceled {
			return view, errors.New("client: transfer was canceled")
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return view, ErrApprovalTimeout
			}
			return view, ctx.Err()
		case <-ticker.C:
		}
	}
}

// StreamItem streams the payload for one approved item via PUT /api/batches/{batchID}/items/{itemID}/content.
// Memory use is bounded by streaming chunks directly from the local filesystem.
// SHA-256 is calculated inline and compared against the server's returned digest.
func (c *Client) StreamItem(ctx context.Context, batchID, itemID, localPath string, onProgress func(written, total int64)) (ItemResult, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return ItemResult{}, fmt.Errorf("client: cannot open local file %s: %w", localPath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ItemResult{}, fmt.Errorf("client: cannot stat local file %s: %w", localPath, err)
	}
	declaredSize := info.Size()

	hashingReader := NewHashingReader(f, declaredSize, onProgress)

	reqURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", c.target.BaseURL, url.PathEscape(batchID), url.PathEscape(itemID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, hashingReader)
	if err != nil {
		return ItemResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.target.Token())
	req.Header.Set("Host", c.target.Host)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = declaredSize

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ItemResult{}, fmt.Errorf("client: streaming failed for %s: %w", filepath.Base(localPath), err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return ItemResult{}, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &errResp)
		msg := errResp.Error
		if msg == "" {
			msg = string(respBody)
		}
		return ItemResult{}, fmt.Errorf("%w: HTTP %d: %s", ErrServerFault, resp.StatusCode, msg)
	}

	var res struct {
		Written int64              `json:"written"`
		SHA256  string             `json:"sha256"`
		Batch   transfer.BatchView `json:"batch"`
	}
	if err := json.Unmarshal(respBody, &res); err != nil {
		return ItemResult{}, fmt.Errorf("client: invalid stream result JSON: %w", err)
	}

	localSHA := hashingReader.SHA256()

	// Verify byte count
	if res.Written != declaredSize {
		return ItemResult{}, fmt.Errorf("%w: %s written %d bytes, expected %d", ErrSizeMismatch, filepath.Base(localPath), res.Written, declaredSize)
	}

	// Verify SHA-256 digest
	if !strings.EqualFold(localSHA, res.SHA256) {
		return ItemResult{}, fmt.Errorf("%w: %s local=%s remote=%s", ErrIntegrityMismatch, filepath.Base(localPath), localSHA, res.SHA256)
	}

	return ItemResult{
		Written:     res.Written,
		SHA256:      res.SHA256,
		LocalSHA256: localSHA,
		Verified:    true,
		Batch:       res.Batch,
	}, nil
}

// CancelItem withdraws one declared item via DELETE /api/batches/{id}/items/{itemID}.
func (c *Client) CancelItem(ctx context.Context, batchID, itemID string) error {
	reqURL := fmt.Sprintf("%s/api/batches/%s/items/%s", c.target.BaseURL, url.PathEscape(batchID), url.PathEscape(itemID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.target.Token())
	req.Header.Set("Host", c.target.Host)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusConflict {
		return ErrConflict
	}
	return fmt.Errorf("unexpected status canceling item: %d", resp.StatusCode)
}

// CancelBatch cancels all unfinished items in a batch, transitioning the batch to canceled on the server.
func (c *Client) CancelBatch(ctx context.Context, batchID string, items []transfer.ItemView) error {
	var firstErr error
	for _, item := range items {
		if item.State == transfer.ItemStateCompleted || item.State == transfer.ItemStateCanceled {
			continue
		}
		if err := c.CancelItem(ctx, batchID, item.ID); err != nil && !errors.Is(err, ErrConflict) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
