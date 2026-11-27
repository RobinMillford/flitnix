package server

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
)

type devSessionLimitEntry struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
	lastSeen    time.Time
}

type deviceSessionLimiter struct {
	mu          sync.Mutex
	maxAttempts int
	window      time.Duration
	lockout     time.Duration
	maxIPs      int
	now         func() time.Time
	entries     map[string]*devSessionLimitEntry
}

func newDeviceSessionLimiter() *deviceSessionLimiter {
	return &deviceSessionLimiter{
		maxAttempts: 12,
		window:      1 * time.Minute,
		lockout:     2 * time.Minute,
		maxIPs:      1024,
		now:         time.Now,
		entries:     make(map[string]*devSessionLimitEntry),
	}
}

func (l *deviceSessionLimiter) checkAllowed(ip string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[ip]
	if !ok {
		return true, 0
	}
	e.lastSeen = now
	if !e.lockedUntil.IsZero() {
		if now.Before(e.lockedUntil) {
			return false, e.lockedUntil.Sub(now)
		}
		e.lockedUntil = time.Time{}
		e.count = 0
		e.windowStart = time.Time{}
	}
	return true, 0
}

func (l *deviceSessionLimiter) recordFailure(ip string) {
	if l == nil {
		return
	}
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[ip]
	if !ok {
		if len(l.entries) >= l.maxIPs {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range l.entries {
				if oldestKey == "" || v.lastSeen.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.lastSeen
				}
			}
			if oldestKey != "" {
				delete(l.entries, oldestKey)
			}
		}
		e = &devSessionLimitEntry{lastSeen: now}
		l.entries[ip] = e
	}
	e.lastSeen = now
	if e.windowStart.IsZero() || now.Sub(e.windowStart) > l.window {
		e.windowStart = now
		e.count = 0
	}
	e.count++
	if e.count >= l.maxAttempts {
		e.lockedUntil = now.Add(l.lockout)
	}
}

func (l *deviceSessionLimiter) clear(ip string) {
	if l == nil || ip == "" {
		return
	}
	l.mu.Lock()
	delete(l.entries, ip)
	l.mu.Unlock()
}

func enforceDeviceReconnectVisibility(w http.ResponseWriter, svcs *Services) bool {
	if svcs == nil || svcs.Visibility == nil {
		return true
	}
	vis := svcs.Visibility.Evaluate()
	if vis.Mode == devices.VisibilityHidden {
		writeErrorCode(
			w,
			http.StatusForbidden,
			"This desktop is currently Hidden and blocks unsolicited reconnects. Scan a QR code or switch visibility to Trusted Only.",
			"visibility_hidden",
		)
		return false
	}
	return true
}

func writeRateLimit(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeErrorCode(w, http.StatusTooManyRequests, "Too many device reconnect attempts. Try again shortly.", "rate_limited")
}

func handleDeviceSessionChallenge(svcs *Services, limiter *deviceSessionLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svcs == nil || svcs.Devices == nil || svcs.HostIdentity == nil || svcs.Sessions == nil {
			writeError(w, http.StatusNotFound, "trusted devices unavailable")
			return
		}
		if !enforceDeviceReconnectVisibility(w, svcs) {
			return
		}
		clientIP := clientIPFromRequest(r)
		if ok, retry := limiter.checkAllowed(clientIP); !ok {
			writeRateLimit(w, retry)
			return
		}

		var req struct {
			DeviceID string `json:"deviceId"`
		}
		if !decodeControlJSON(w, r, &req, "invalid device challenge request", rejectUnknownFields) {
			return
		}
		req.DeviceID = strings.TrimSpace(req.DeviceID)
		if req.DeviceID == "" {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusBadRequest, "deviceId is required", "invalid_request")
			return
		}

		dev, exists := svcs.Devices.Get(req.DeviceID)
		if !exists {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusUnauthorized, "This device is no longer trusted on this desktop.", "device_revoked")
			return
		}

		nonce, ts, err := svcs.Devices.CreateTransferChallenge(svcs.HostIdentity.HostID, dev.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create device session challenge")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"hostId":    svcs.HostIdentity.HostID,
			"deviceId":  dev.ID,
			"nonce":     nonce,
			"timestamp": ts,
		})
	}
}

func handleDeviceSessionVerify(svcs *Services, limiter *deviceSessionLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svcs == nil || svcs.Devices == nil || svcs.HostIdentity == nil || svcs.Sessions == nil {
			writeError(w, http.StatusNotFound, "trusted devices unavailable")
			return
		}
		if !enforceDeviceReconnectVisibility(w, svcs) {
			return
		}
		clientIP := clientIPFromRequest(r)
		if ok, retry := limiter.checkAllowed(clientIP); !ok {
			writeRateLimit(w, retry)
			return
		}

		var req struct {
			DeviceID  string `json:"deviceId"`
			HostID    string `json:"hostId"`
			Nonce     string `json:"nonce"`
			Timestamp int64  `json:"timestamp"`
			Signature string `json:"signature"`
		}
		if !decodeControlJSON(w, r, &req, "invalid device session verification payload", rejectUnknownFields) {
			return
		}
		if req.HostID != svcs.HostIdentity.HostID {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusUnauthorized, "Host identity mismatch.", "host_mismatch")
			return
		}
		dev, exists := svcs.Devices.Get(req.DeviceID)
		if !exists {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusUnauthorized, "This device is no longer trusted on this desktop.", "device_revoked")
			return
		}

		payload := devices.BuildDeviceSessionAuthPayload(svcs.HostIdentity.HostID, dev.ID, req.Timestamp, req.Nonce)
		valid, sigErr := svcs.Devices.VerifySignature(dev.ID, payload, req.Signature)
		if sigErr != nil || !valid {
			limiter.recordFailure(clientIP)
			writeErrorCode(w, http.StatusUnauthorized, "Device signature verification failed.", "invalid_signature")
			return
		}

		if err := svcs.Devices.ValidateAndConsumeTransferChallenge(
			svcs.HostIdentity.HostID, dev.ID, req.Nonce, req.Timestamp, 60*time.Second,
		); err != nil {
			limiter.recordFailure(clientIP)
			code := "challenge_invalid"
			if errors.Is(err, devices.ErrReplayDetected) {
				code = "challenge_replayed"
			} else if errors.Is(err, devices.ErrChallengeExpired) {
				code = "challenge_expired"
			}
			writeErrorCode(w, http.StatusUnauthorized, "Device challenge expired or already used.", code)
			return
		}

		sess, err := svcs.Sessions.CreateForDevice(dev.ID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "could not establish trusted device session")
			return
		}
		limiter.clear(clientIP)
		svcs.RecordContact("device-reconnect")

		writeJSON(w, http.StatusOK, map[string]any{
			"token":       sess.Token(),
			"sessionKey":  sess.Key(),
			"expiresAt":   sess.ExpiresAt(),
			"hostId":      svcs.HostIdentity.HostID,
			"deviceId":    dev.ID,
			"deviceName":  dev.Name,
			"autoApprove": dev.AutoApprove,
		})
	}
}
