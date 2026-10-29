package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/itsyamin/flitnix/internal/server"
)

var (
	// ErrInvalidURL reports an unparseable or malformed recipient URL.
	ErrInvalidURL = errors.New("client: invalid recipient URL")

	// ErrMissingToken reports a URL or target missing the secret pairing token.
	ErrMissingToken = errors.New("client: missing pairing token in URL fragment (#<token>) or --token flag")

	// ErrUnsupportedIPv6 reports a link-local IPv6 address that cannot be used.
	ErrUnsupportedIPv6 = errors.New("client: link-local IPv6 addresses are unsupported for transfer")

	// ErrInvalidHost reports an invalid or empty host component.
	ErrInvalidHost = errors.New("client: invalid host in recipient URL")
)

// Target holds the sanitized connection information for the remote Flitnix instance.
// The pairing token is kept private and will never be serialized or printed.
type Target struct {
	BaseURL string // e.g. "http://192.168.1.50:43821" or "http://[2001:db8::1]:43821"
	Host    string // e.g. "192.168.1.50:43821" or "[2001:db8::1]:43821"
	token   string // 256-bit secret token; strictly unexported
}

// Token returns the unexported pairing token.
func (t *Target) Token() string {
	if t == nil {
		return ""
	}
	return t.token
}

// String implements fmt.Stringer, returning the BaseURL without any secret token.
func (t *Target) String() string {
	if t == nil {
		return "<nil>"
	}
	return t.BaseURL
}

// MarshalJSON implements json.Marshaler, ensuring the secret token is never serialized.
func (t *Target) MarshalJSON() ([]byte, error) {
	if t == nil {
		return []byte("null"), nil
	}
	return json.Marshal(map[string]string{
		"baseUrl": t.BaseURL,
		"host":    t.Host,
	})
}

// ParseTarget parses a raw URL or pairing link and extracts the target and token.
// If explicitToken is provided, it overrides any token in the URL fragment.
func ParseTarget(rawInput, explicitToken string) (*Target, error) {
	s := strings.TrimSpace(rawInput)
	if s == "" {
		return nil, fmt.Errorf("%w: target URL is empty", ErrInvalidURL)
	}

	// Default to http scheme if omitted
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}

	// Check for IPv6 zone identifier anywhere in input before url.Parse
	if strings.Contains(s, "%") {
		return nil, fmt.Errorf("%w: IPv6 zone identifiers are unsupported; recipient must use an IPv4 or Global/ULA IPv6 address", ErrUnsupportedIPv6)
	}

	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q (must be http or https)", ErrInvalidURL, u.Scheme)
	}

	if u.Host == "" {
		return nil, fmt.Errorf("%w: missing host", ErrInvalidHost)
	}

	// Parse host and port
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		// Could be host without port
		host = u.Host
		port = strconv.Itoa(server.DefaultPort)
		// Strip brackets if bracketed IPv6 without port, e.g. [2001:db8::1]
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
	}

	// Check for IPv6 zone identifier
	if strings.Contains(host, "%") {
		return nil, fmt.Errorf("%w: IPv6 zone identifiers are unsupported; recipient must use an IPv4 or Global/ULA IPv6 address", ErrUnsupportedIPv6)
	}

	// Validate IP address family if host is an IP literal
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
		if addr.IsLinkLocalUnicast() || strings.HasPrefix(strings.ToLower(host), "fe80:") {
			return nil, fmt.Errorf("%w: link-local address %s requires a zone index which is unsupported; recipient must use an IPv4 or Global/ULA IPv6 address", ErrUnsupportedIPv6, host)
		}
	}

	// Rebuild canonical host:port authority
	canonicalAuthority := net.JoinHostPort(host, port)
	baseURL := fmt.Sprintf("%s://%s", scheme, canonicalAuthority)

	// Extract token
	token := strings.TrimSpace(explicitToken)
	if token == "" && u.Fragment != "" {
		frag := strings.TrimSpace(u.Fragment)
		frag = strings.TrimPrefix(frag, "/")
		token = frag
	}

	if token == "" {
		return nil, ErrMissingToken
	}

	return &Target{
		BaseURL: baseURL,
		Host:    canonicalAuthority,
		token:   token,
	}, nil
}
