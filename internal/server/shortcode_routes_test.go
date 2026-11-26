package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
)

func TestConnectCodeRedeemAndRejectionPaths(t *testing.T) {
	h := newSecurityTestHarness(t)
	now := time.Now()
	shortCodes := session.NewShortCodeManager(session.ShortCodeConfig{
		TTL:             3 * time.Minute,
		MaxFailures:     3,
		LockoutDuration: 2 * time.Minute,
		Now:             func() time.Time { return now },
	})
	h.svcs.ShortCodes = shortCodes

	codeView, err := shortCodes.GetOrCreate(h.sess.Key())
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	// Wrong code returns 401 without revealing code.
	badRec := h.doJSON(http.MethodPost, "/api/connect-code", map[string]string{"code": "999999"}, "", nil)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code status = %d, want 401", badRec.Code)
	}
	if strings.Contains(badRec.Body.String(), codeView.Code) {
		t.Fatalf("error response leaked active code")
	}

	// Valid formatted code exchanges cleanly for a fresh Bearer token.
	okRec := h.doJSON(http.MethodPost, "/api/connect-code", map[string]string{
		"code": codeView.FormattedCode,
	}, "", nil)
	if okRec.Code != http.StatusOK {
		t.Fatalf("redeem status = %d, want 200 (%s)", okRec.Code, okRec.Body.String())
	}
	var tokenResp struct {
		Token      string `json:"token"`
		SessionKey string `json:"sessionKey"`
		HostID     string `json:"hostId"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("unmarshal token response: %v", err)
	}
	if tokenResp.Token == "" || tokenResp.Token == h.sess.Token() {
		t.Fatalf("expected fresh distinct Bearer token, got %q", tokenResp.Token)
	}
	if tokenResp.HostID != h.hostID.HostID {
		t.Fatalf("hostId = %q, want %q", tokenResp.HostID, h.hostID.HostID)
	}

	// Issued Bearer token works against authenticated transfer API endpoints.
	filesRec := h.doJSON(http.MethodGet, "/api/files", nil, tokenResp.Token, nil)
	if filesRec.Code != http.StatusOK {
		t.Fatalf("authenticated /api/files with redeemed token = %d, want 200", filesRec.Code)
	}

	// Reusing the same 6-digit code fails with 409 code_used.
	replayRec := h.doJSON(http.MethodPost, "/api/connect-code", map[string]string{"code": codeView.Code}, "", nil)
	if replayRec.Code != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409 (%s)", replayRec.Code, replayRec.Body.String())
	}

	// Repeated invalid attempts trigger 429 rate limiting with Retry-After.
	for i := 0; i < 2; i++ {
		_ = h.doJSON(http.MethodPost, "/api/connect-code", map[string]string{"code": "123456"}, "", nil)
	}
	limitRec := h.doJSON(http.MethodPost, "/api/connect-code", map[string]string{"code": "123456"}, "", nil)
	if limitRec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit status = %d, want 429 (%s)", limitRec.Code, limitRec.Body.String())
	}
	if limitRec.Header().Get("Retry-After") == "" {
		t.Fatalf("missing Retry-After header on 429 response")
	}
}
