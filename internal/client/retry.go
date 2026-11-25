package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"
)

var (
	// ErrInsufficientStorage reports HTTP 507 when the desktop disk cannot hold the upload.
	ErrInsufficientStorage = errors.New("client: desktop storage is low")
)

// RetryPolicy controls automatic recovery after transient transport or Wi-Fi interruptions.
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	OnRetry    func(attempt, maxAttempts int, delay time.Duration, err error)
}

// DefaultRetryPolicy returns flitnix's default bounded retry schedule:
// 1s, 2s, 4s, 8s, 15s, 15s (with +/-20% jitter).
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries: 6,
		BaseDelay:  time.Second,
		MaxDelay:   15 * time.Second,
	}
}

func (p RetryPolicy) normalized() RetryPolicy {
	out := p
	if out.MaxRetries < 0 {
		out.MaxRetries = 0
	}
	if out.BaseDelay <= 0 {
		out.BaseDelay = time.Second
	}
	if out.MaxDelay <= 0 {
		out.MaxDelay = 15 * time.Second
	}
	return out
}

// HTTPStatusError preserves HTTP status and machine-readable error codes for retry decisions.
type HTTPStatusError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *HTTPStatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s: HTTP %d: %s", ErrServerFault, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s: HTTP %d", ErrServerFault, e.StatusCode)
}

func (e *HTTPStatusError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrBatchNotFound
	case http.StatusInsufficientStorage:
		return ErrInsufficientStorage
	default:
		return ErrServerFault
	}
}

// IsRetryableError classifies permanent protocol/auth/storage faults (false) vs
// transient transport/network/roaming failures (true).
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrSessionExpired) ||
		errors.Is(err, ErrRejected) || errors.Is(err, ErrBatchNotFound) ||
		errors.Is(err, ErrConflict) || errors.Is(err, ErrIntegrityMismatch) ||
		errors.Is(err, ErrSizeMismatch) || errors.Is(err, ErrInsufficientStorage) {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return false
	}

	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.Code {
		case "chunk_interrupted", "offset_mismatch", "stall":
			return true
		case "paused", "discarded", "quarantined", "session_revoked", "insufficient_storage":
			return false
		}
		switch statusErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}

	var uerr *url.Error
	if errors.As(err, &uerr) {
		return IsRetryableError(uerr.Err)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	return false
}

// BackoffDelay computes jittered (+/-20%) exponential backoff for 1-based attempt.
func BackoffDelay(attempt int, base, maxDelay time.Duration, randFloat func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if base <= 0 {
		base = time.Second
	}
	if maxDelay <= 0 {
		maxDelay = 15 * time.Second
	}
	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= maxDelay/2 {
			delay = maxDelay
			break
		}
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	r := 0.5
	if randFloat != nil {
		r = randFloat()
		if r < 0 {
			r = 0
		} else if r > 1 {
			r = 1
		}
	} else {
		r = rand.Float64()
	}
	// Apply +/-20% jitter cleanly.
	jitterFactor := 0.8 + 0.4*r
	return time.Duration(float64(delay) * jitterFactor)
}

func waitBackoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
