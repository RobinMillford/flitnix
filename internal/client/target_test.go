package client

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		explicitToken string
		wantBase      string
		wantHost      string
		wantToken     string
		wantErr       error
	}{
		{
			name:      "standard IPv4 pairing link",
			raw:       "http://192.168.1.50:43821/#secret-token-123",
			wantBase:  "http://192.168.1.50:43821",
			wantHost:  "192.168.1.50:43821",
			wantToken: "secret-token-123",
		},
		{
			name:      "IPv4 without port defaults to 43821",
			raw:       "http://192.168.1.50/#secret-token-123",
			wantBase:  "http://192.168.1.50:43821",
			wantHost:  "192.168.1.50:43821",
			wantToken: "secret-token-123",
		},
		{
			name:      "standard bracketed IPv6 pairing link",
			raw:       "http://[2001:db8::1]:43821/#secret-token-123",
			wantBase:  "http://[2001:db8::1]:43821",
			wantHost:  "[2001:db8::1]:43821",
			wantToken: "secret-token-123",
		},
		{
			name:      "bracketed IPv6 without port defaults to 43821",
			raw:       "http://[2001:db8::1]/#secret-token-123",
			wantBase:  "http://[2001:db8::1]:43821",
			wantHost:  "[2001:db8::1]:43821",
			wantToken: "secret-token-123",
		},
		{
			name:      "ULA IPv6 address",
			raw:       "http://[fd00::1]:43821/#secret-token-123",
			wantBase:  "http://[fd00::1]:43821",
			wantHost:  "[fd00::1]:43821",
			wantToken: "secret-token-123",
		},
		{
			name:          "explicit token overrides fragment",
			raw:           "http://192.168.1.50:43821/#old-token",
			explicitToken: "override-token-456",
			wantBase:      "http://192.168.1.50:43821",
			wantHost:      "192.168.1.50:43821",
			wantToken:     "override-token-456",
		},
		{
			name:          "URL without fragment but with explicit token",
			raw:           "http://192.168.1.50:43821",
			explicitToken: "override-token-456",
			wantBase:      "http://192.168.1.50:43821",
			wantHost:      "192.168.1.50:43821",
			wantToken:     "override-token-456",
		},
		{
			name:    "missing token returns error",
			raw:     "http://192.168.1.50:43821",
			wantErr: ErrMissingToken,
		},
		{
			name:    "link-local IPv6 address is rejected",
			raw:     "http://[fe80::1]:43821/#secret-token",
			wantErr: ErrUnsupportedIPv6,
		},
		{
			name:    "zone identifier is rejected",
			raw:     "http://fe80::1%eth0:43821/#secret-token",
			wantErr: ErrUnsupportedIPv6,
		},
		{
			name:    "unsupported scheme rejected",
			raw:     "ftp://192.168.1.50:43821/#token",
			wantErr: ErrInvalidURL,
		},
		{
			name:    "empty target rejected",
			raw:     "   ",
			wantErr: ErrInvalidURL,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target, err := ParseTarget(tc.raw, tc.explicitToken)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error %v, got nil", tc.wantErr)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error wrapping %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if target.BaseURL != tc.wantBase {
				t.Errorf("BaseURL = %q, want %q", target.BaseURL, tc.wantBase)
			}
			if target.Host != tc.wantHost {
				t.Errorf("Host = %q, want %q", target.Host, tc.wantHost)
			}
			if target.Token() != tc.wantToken {
				t.Errorf("Token = %q, want %q", target.Token(), tc.wantToken)
			}

			// Verify secret token is never exposed in String() or JSON
			str := target.String()
			if strings.Contains(str, tc.wantToken) {
				t.Fatalf("Target.String() leaked secret token: %s", str)
			}

			jsonBytes, err := json.Marshal(target)
			if err != nil {
				t.Fatalf("json.Marshal failed: %v", err)
			}
			if strings.Contains(string(jsonBytes), tc.wantToken) {
				t.Fatalf("Target JSON marshaling leaked secret token: %s", string(jsonBytes))
			}
		})
	}
}
