package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsAllowedHostMatrix(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		allowed bool
	}{
		// Valid IPv4 literals
		{"ipv4 bare", "192.168.1.42", true},
		{"ipv4 with port", "192.168.1.42:43821", true},
		{"loopback ipv4", "127.0.0.1:43821", true},
		{"loopback alternative", "127.0.0.9:43821", true},

		// Valid bracketed IPv6 literals
		{"ipv6 global bare", "[2001:db8::10]", true},
		{"ipv6 global with port", "[2001:db8::10]:43821", true},
		{"ipv6 ula bare", "[fd12:3456::10]", true},
		{"ipv6 ula with port", "[fd12:3456::10]:43821", true},
		{"ipv6 loopback bare", "[::1]", true},
		{"ipv6 loopback with port", "[::1]:43821", true},

		// Zone identifiers on IPv6
		{"ipv6 link-local with raw zone", "[fe80::1%eth0]", true},
		{"ipv6 link-local with url-encoded zone", "[fe80::1%25eth0]:43821", true},
		{"ipv4 with zone is rejected", "192.168.1.1%eth0", false},
		{"hostname with zone is rejected", "localhost%eth0", false},

		// Invalid bracket formatting
		{"missing closing bracket", "[2001:db8::1", false},
		{"missing opening bracket", "2001:db8::1]", false},
		{"nested brackets", "[[2001:db8::1]]", false},
		{"bracket on ipv4", "[192.168.1.1]", false},
		{"empty bracket", "[]", false},
		{"empty bracket with port", "[]:43821", false},
		{"bracketed trailing colon without port", "[2001:db8::1]:", false},
		{"bracketed with alphabetic port", "[2001:db8::1]:abc", false},
		{"bracketed with negative port", "[2001:db8::1]:-1", false},
		{"bracketed with out-of-range port", "[2001:db8::1]:70000", false},

		// Local/test hostnames
		{"localhost", "localhost", true},
		{"localhost with port", "localhost:43821", true},
		{"subdomain localhost", "dev.localhost:43821", true},
		{"example test domain", "example.com:43821", true},
		{"rfc 2606 test domain", "flitnix.test:43821", true},

		// Untrusted / DNS rebinding targets
		{"empty host", "", false},
		{"whitespace host", "   ", false},
		{"evil attacker domain", "evil.attacker.com", false},
		{"evil attacker with port", "evil.attacker.com:43821", false},
		{"spoofed localhost prefix", "localhost.evil.com", false},
		{"spoofed ip prefix", "192.168.1.1.attacker.com", false},
		{"arbitrary internet host", "google.com", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isAllowedHost(tc.host)
			if got != tc.allowed {
				t.Errorf("isAllowedHost(%q) = %v, want %v", tc.host, got, tc.allowed)
			}
		})
	}
}

func TestEqualHostPortNormalization(t *testing.T) {
	cases := []struct {
		name  string
		h1    string
		h2    string
		equal bool
	}{
		{"exact match ipv4", "192.168.1.10:43821", "192.168.1.10:43821", true},
		{"exact match ipv6", "[2001:db8::1]:43821", "[2001:db8::1]:43821", true},
		{"ipv6 case insensitivity", "[2001:DB8::A]:43821", "[2001:db8::a]:43821", true},
		{"ipv6 leading zero normalization", "[2001:0db8::0001]:43821", "[2001:db8::1]:43821", true},
		{"ipv6 port mismatch", "[2001:db8::1]:43821", "[2001:db8::1]:8080", false},
		{"ipv4 ip mismatch", "192.168.1.10:43821", "192.168.1.20:43821", false},
		{"ipv4 vs ipv6 mismatch", "192.168.1.10:43821", "[2001:db8::1]:43821", false},
		{"localhost case insensitivity", "LOCALHOST:43821", "localhost:43821", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := equalHostPort(tc.h1, tc.h2)
			if got != tc.equal {
				t.Errorf("equalHostPort(%q, %q) = %v, want %v", tc.h1, tc.h2, got, tc.equal)
			}
		})
	}
}

func TestSecurityMiddlewareRejectsDNSRebindingOverIPv6(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	// 1. Valid bracketed IPv6 host header accepted
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Host = "[2001:db8::10]:43821"
	rec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid IPv6 host header failed: code=%d %s", rec.Code, rec.Body.String())
	}

	// 2. DNS rebinding hostname rejected with 400 Bad Request
	reqRebind := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	reqRebind.Host = "rebind.attacker.internal:43821"
	recRebind := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(recRebind, reqRebind)
	if recRebind.Code != http.StatusBadRequest {
		t.Fatalf("DNS rebinding request must return 400, got %d", recRebind.Code)
	}

	// 3. Malformed brackets rejected with 400 Bad Request
	reqMalformed := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	reqMalformed.Host = "[2001:db8::10:43821"
	recMalformed := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(recMalformed, reqMalformed)
	if recMalformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed bracket host must return 400, got %d", recMalformed.Code)
	}

	// 4. Cross-origin CSRF on IPv6 authority rejected with 403 Forbidden
	reqCSRF := httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewReader([]byte("{}")))
	reqCSRF.Host = "[2001:db8::10]:43821"
	reqCSRF.Header.Set("Authorization", "Bearer "+tok)
	reqCSRF.Header.Set("Origin", "http://attacker.com")
	recCSRF := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(recCSRF, reqCSRF)
	if recCSRF.Code != http.StatusForbidden {
		t.Fatalf("cross-origin state changing request must return 403, got %d", recCSRF.Code)
	}

	// 5. Same-origin state-changing request over IPv6 accepted
	reqSameOrigin := httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewReader([]byte(`{"name":"test.bin","size":100}`)))
	reqSameOrigin.Host = "[2001:db8::10]:43821"
	reqSameOrigin.Header.Set("Authorization", "Bearer "+tok)
	reqSameOrigin.Header.Set("Origin", "http://[2001:db8::10]:43821")
	reqSameOrigin.Header.Set("Content-Type", "application/json")
	recSameOrigin := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(recSameOrigin, reqSameOrigin)
	if recSameOrigin.Code != http.StatusCreated {
		t.Fatalf("same-origin IPv6 request failed: code=%d %s", recSameOrigin.Code, recSameOrigin.Body.String())
	}
}
