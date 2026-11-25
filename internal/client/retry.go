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
	"strconv"
	"strings"
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
	// RetryAfter is the server-requested delay parsed from the Retry-After
	// header (delta seconds or an HTTP date); zero when absent or invalid.
	RetryAfter time.Duration
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
		case http.StatusRequestTimeout, http.StatusTooManyRequests,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
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

// MaxAutoRetryWait bounds the automatic wait when a server asks for a longer
// Retry-After delay than the configured backoff policy would use. Beyond it,
// retries stop and the requested delay is reported through OnRetry so callers
// can surface a deferred retry instead of retrying early.
const MaxAutoRetryWait = 5 * time.Minute

// MaxRetryAfterSeconds caps delta-seconds Retry-After values so huge or
// overflowing inputs cannot extend the wait beyond MaxAutoRetryWait.
const MaxRetryAfterSeconds = int64(86400)

// ParseRetryAfter parses an HTTP Retry-After header value.
//
// Supported formats:
//   - Delta seconds, e.g. "10" — a delay of at least 10 seconds.
//   - HTTP date, e.g. "Wed, 25 Nov 2026 15:30:00 GMT" — the remaining time
//     until that date; expired dates yield a zero delay.
//
// Malformed values, negative values and zero return false. Extremely large
// values are clamped to MaxAutoRetryWait. The second return reports whether a
// valid value was found.
func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs == 0 {
			return 0, true
		}
		if secs > MaxRetryAfterSeconds {
			return MaxAutoRetryWait, true
		}
		return time.Duration(secs) * time.Second, true
	} else if isAllDigits(v) {
		// Delta seconds too large for int64: clamp to the maximum wait.
		return MaxAutoRetryWait, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		if d > MaxAutoRetryWait {
			return MaxAutoRetryWait, true
		}
		return d, true
	}
	return 0, false
}

// isAllDigits reports whether s consists only of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// RetryAfterOf extracts the server-requested Retry-After from err, if any.
func RetryAfterOf(err error) (time.Duration, bool) {
	var se *HTTPStatusError
	if errors.As(err, &se) && se.RetryAfter > 0 {
		return se.RetryAfter, true
	}
	return 0, false
}

// RetryAfterHeader parses the Retry-After header of a response, returning 0
// when the header is absent or invalid.
func RetryAfterHeader(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	if d, ok := ParseRetryAfter(h.Get("Retry-After"), time.Now()); ok {
		return d
	}
	return 0
}

// RetryDelay returns the final wait for a retry after err: the jittered
// exponential backoff combined with any server-requested Retry-After. The
// result is never shorter than a valid server request.
func RetryDelay(err error, attempt int, policy RetryPolicy, randFloat func() float64) time.Duration {
	delay := BackoffDelay(attempt, policy.BaseDelay, policy.MaxDelay, randFloat)
	if ra, ok := RetryAfterOf(err); ok && ra > delay {
		delay = ra
	}
	return delay
}

// retryDelayOrDeferred returns the wait for the next retry after err. When the
// wait exceeds MaxAutoRetryWait the second return is non-nil and describes a
// deferred retry: callers must not wait and must not retry early.
func (c *Client) retryDelayOrDeferred(err error, attempt int, policy RetryPolicy) (time.Duration, error) {
	delay := RetryDelay(err, attempt, policy, nil)
	if delay > MaxAutoRetryWait {
		return delay, fmt.Errorf("%w: server requested retry after %s", err, delay)
	}
	return delay, nil
}
