package client

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	target      *Target
	httpClient  *http.Client
	retryPolicy RetryPolicy
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
		retryPolicy: DefaultRetryPolicy(),
	}
}

// SetRetryPolicy overrides the automatic chunk retry policy on transient network failures.
func (c *Client) SetRetryPolicy(policy RetryPolicy) {
	c.retryPolicy = policy.normalized()
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

// StreamItem streams the payload for one approved item. It queries HEAD for
// resumable PATCH chunk support first (resuming from the server's durable
// Upload-Offset when available), and falls back cleanly to single-pass PUT if
// the endpoint does not advertise HEAD/PATCH support.
// SHA-256 is verified over the complete local file against the server's digest.
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

	reqURL := fmt.Sprintf("%s/api/batches/%s/items/%s/content", c.target.BaseURL, url.PathEscape(batchID), url.PathEscape(itemID))
	policy := c.retryPolicy.normalized()
	for attempt := 0; ; attempt++ {
		offset, chunkSize, supportsResumable, headErr := c.probeHead(ctx, reqURL, declaredSize)
		if headErr != nil {
			if !IsRetryableError(headErr) || attempt >= policy.MaxRetries {
				return ItemResult{}, headErr
			}
			delay, derr := c.retryDelayOrDeferred(headErr, attempt+1, policy)
			if derr != nil {
				return ItemResult{}, derr
			}
			if policy.OnRetry != nil {
				policy.OnRetry(attempt+1, policy.MaxRetries, delay, headErr)
			}
			if werr := waitBackoff(ctx, delay); werr != nil {
				return ItemResult{}, werr
			}
			continue
		}
		if supportsResumable {
			return c.streamItemChunks(ctx, f, localPath, reqURL, declaredSize, offset, chunkSize, onProgress)
		}
		return c.streamItemPut(ctx, f, localPath, reqURL, declaredSize, onProgress)
	}
}

func (c *Client) probeHead(ctx context.Context, reqURL string, declaredSize int64) (offset int64, chunkSize int64, supported bool, err error) {
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, reqURL, nil)
	if err != nil {
		return 0, 0, false, err
	}
	headReq.Header.Set("Authorization", "Bearer "+c.target.Token())
	headReq.Header.Set("Host", c.target.Host)
	headResp, herr := c.httpClient.Do(headReq)
	if herr != nil {
		return 0, 0, false, herr
	}
	headResp.Body.Close()

	if headResp.StatusCode == http.StatusUnauthorized {
		return 0, 0, false, ErrUnauthorized
	}
	if headResp.StatusCode == http.StatusNotFound || headResp.StatusCode == http.StatusMethodNotAllowed {
		return 0, 0, false, nil
	}
	if headResp.StatusCode == http.StatusOK {
		parsedOff, oerr := strconv.ParseInt(strings.TrimSpace(headResp.Header.Get("Upload-Offset")), 10, 64)
		if oerr == nil && parsedOff >= 0 && parsedOff <= declaredSize {
			cs := transfer.DefaultChunkSize
			if v, cerr := strconv.ParseInt(strings.TrimSpace(headResp.Header.Get("Upload-Chunk-Size")), 10, 64); cerr == nil && v > 0 {
				cs = v
			}
			return parsedOff, cs, true, nil
		}
		return 0, 0, false, nil
	}
	return 0, 0, false, &HTTPStatusError{
		StatusCode: headResp.StatusCode,
		RetryAfter: RetryAfterHeader(headResp.Header),
		Message:    fmt.Sprintf("HEAD returned HTTP %d", headResp.StatusCode),
	}
}

func computeFilePrefixHash(f *os.File, offset int64) (h io.Writer, sumFunc func() []byte, err error) {
	hasher := sha256.New()
	if _, serr := f.Seek(0, io.SeekStart); serr != nil {
		return nil, nil, serr
	}
	if offset > 0 {
		if _, cerr := io.CopyN(hasher, f, offset); cerr != nil {
			return nil, nil, cerr
		}
	}
	return hasher, func() []byte { return hasher.Sum(nil) }, nil
}

func (c *Client) streamItemChunks(ctx context.Context, f *os.File, localPath, reqURL string, declaredSize, offset, chunkSize int64, onProgress func(written, total int64)) (ItemResult, error) {
	hasher, sumBytes, err := computeFilePrefixHash(f, offset)
	if err != nil {
		return ItemResult{}, fmt.Errorf("client: hashing resumed prefix of %s: %w", localPath, err)
	}
	if onProgress != nil {
		onProgress(offset, declaredSize)
	}

	var lastResult struct {
		Written int64              `json:"written"`
		SHA256  string             `json:"sha256"`
		Batch   transfer.BatchView `json:"batch"`
	}
	policy := c.retryPolicy.normalized()
	consecutiveRetries := 0

	retryWithHead := func(cause error) (bool, error) {
		if !IsRetryableError(cause) || consecutiveRetries >= policy.MaxRetries {
			return false, cause
		}
		consecutiveRetries++
		delay, derr := c.retryDelayOrDeferred(cause, consecutiveRetries, policy)
		if derr != nil {
			return false, derr
		}
		if policy.OnRetry != nil {
			policy.OnRetry(consecutiveRetries, policy.MaxRetries, delay, cause)
		}
		if werr := waitBackoff(ctx, delay); werr != nil {
			return false, werr
		}
		// Reachability probe before resuming chunk stream
		if perr := c.Ping(ctx); perr != nil {
			return true, nil // stay in retry loop; next attempt will backoff again
		}
		srvOff, srvChunk, _, herr := c.probeHead(ctx, reqURL, declaredSize)
		if herr != nil {
			return true, nil
		}
		if srvChunk > 0 {
			chunkSize = srvChunk
		}
		if srvOff != offset {
			offset = srvOff
		}
		newHasher, newSum, rerr := computeFilePrefixHash(f, offset)
		if rerr != nil {
			return false, rerr
		}
		hasher, sumBytes = newHasher, newSum
		if onProgress != nil {
			onProgress(offset, declaredSize)
		}
		return true, nil
	}

	for offset < declaredSize || (declaredSize == 0 && offset == 0) {
		lenChunk := chunkSize
		if declaredSize-offset < lenChunk {
			lenChunk = declaredSize - offset
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return ItemResult{}, err
		}
		// Compute chunk into request without mutating file prefix hasher on failed partial send:
		// we hash only committed bytes once accepted (or hash prefix cleanly on retry).
		chunkReader := io.LimitReader(f, lenChunk)
		pr := io.TeeReader(chunkReader, hasher)
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, reqURL, &chunkProgressReader{
			r:          pr,
			base:       offset,
			total:      declaredSize,
			onProgress: onProgress,
		})
		if err != nil {
			return ItemResult{}, err
		}
		req.Header.Set("Authorization", "Bearer "+c.target.Token())
		req.Header.Set("Host", c.target.Host)
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		req.ContentLength = lenChunk

		resp, err := c.httpClient.Do(req)
		if err != nil {
			cont, rerr := retryWithHead(fmt.Errorf("client: chunk streaming failed for %s at offset %d: %w", filepath.Base(localPath), offset, err))
			if cont {
				// Recompute prefix hash at confirmed offset so hasher has no partial dirty chunk bytes.
				if newHasher, newSum, herr := computeFilePrefixHash(f, offset); herr == nil {
					hasher, sumBytes = newHasher, newSum
				}
				continue
			}
			return ItemResult{}, rerr
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized {
			return ItemResult{}, ErrUnauthorized
		}
		if resp.StatusCode == http.StatusNoContent {
			consecutiveRetries = 0
			nextOffset := offset + lenChunk
			if srvOff, serr := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Upload-Offset")), 10, 64); serr == nil && srvOff >= 0 {
				nextOffset = srvOff
			}
			offset = nextOffset
			if onProgress != nil {
				onProgress(offset, declaredSize)
			}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			if len(respBody) > 0 {
				if err := json.Unmarshal(respBody, &lastResult); err != nil {
					return ItemResult{}, fmt.Errorf("client: invalid stream result JSON: %w", err)
				}
			}
			offset = declaredSize
			break
		}

		var errResp struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &errResp)
		msg := errResp.Error
		if msg == "" {
			msg = string(respBody)
		}
		httpErr := &HTTPStatusError{
			StatusCode: resp.StatusCode,
			Code:       errResp.Code,
			RetryAfter: RetryAfterHeader(resp.Header),
			Message:    msg,
		}
		if errResp.Code == "offset_mismatch" {
			if srvOff, serr := strconv.ParseInt(strings.TrimSpace(resp.Header.Get("Upload-Offset")), 10, 64); serr == nil && srvOff >= 0 && srvOff <= declaredSize {
				offset = srvOff
			}
		}
		cont, rerr := retryWithHead(httpErr)
		if cont {
			if newHasher, newSum, herr := computeFilePrefixHash(f, offset); herr == nil {
				hasher, sumBytes = newHasher, newSum
			}
			continue
		}
		return ItemResult{}, rerr
	}

	if declaredSize == 0 && lastResult.SHA256 == "" {
		return ItemResult{}, fmt.Errorf("%w: empty upload completed without digest", ErrServerFault)
	}

	localSHA := fmt.Sprintf("%x", sumBytes())
	if lastResult.Written != declaredSize {
		return ItemResult{}, fmt.Errorf("%w: %s written %d bytes, expected %d", ErrSizeMismatch, filepath.Base(localPath), lastResult.Written, declaredSize)
	}
	if !strings.EqualFold(localSHA, lastResult.SHA256) {
		return ItemResult{}, fmt.Errorf("%w: %s local=%s remote=%s", ErrIntegrityMismatch, filepath.Base(localPath), localSHA, lastResult.SHA256)
	}
	return ItemResult{
		Written:     lastResult.Written,
		SHA256:      lastResult.SHA256,
		LocalSHA256: localSHA,
		Verified:    true,
		Batch:       lastResult.Batch,
	}, nil
}

type chunkProgressReader struct {
	r          io.Reader
	base       int64
	read       int64
	total      int64
	onProgress func(written, total int64)
}

func (c *chunkProgressReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.read += int64(n)
		if c.onProgress != nil {
			c.onProgress(c.base+c.read, c.total)
		}
	}
	return n, err
}

func (c *Client) streamItemPut(ctx context.Context, f *os.File, localPath, reqURL string, declaredSize int64, onProgress func(written, total int64)) (ItemResult, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ItemResult{}, err
	}
	hashingReader := NewHashingReader(f, declaredSize, onProgress)

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
