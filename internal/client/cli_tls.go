package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	// ErrTLSRequired indicates that encrypted transport was required (by setting,
	// --require-tls, https:// URL, or prior pinning) and the peer could not be
	// verified over TLS 1.3.
	ErrTLSRequired = errors.New("client: encrypted TLS 1.3 peer transport is required and unavailable")
)

// CLITLSOptions controls TLS 1.3 negotiation, pinning, and downgrade protection for CLI peers.
type CLITLSOptions struct {
	KnownHosts     *KnownHosts
	ExpectedHostID string
	RequireTLS     bool
}

// CLITLSResolution describes the verified transport client and upgraded target.
type CLITLSResolution struct {
	HTTPClient *http.Client
	Target     *Target
	Probe      *PeerProbeInfo
	TLSActive  bool
}

// ResolveCLITargetAndClient enforces TLS 1.3 probing, certificate identity pinning,
// known-hosts verification, and no-silent-downgrade protection for CLI peer operations.
//
// If the target URL uses https://, or if RequireTLS / ExpectedHostID / prior
// known-hosts address pinning applies, any TLS failure or mismatch fails closed.
// If the target URL uses http:// and the peer supports Flitnix TLS 1.3 on the same
// port, the connection upgrades automatically to pinned https:// before any token
// or file payload is transmitted.
func ResolveCLITargetAndClient(ctx context.Context, target *Target, opts CLITLSOptions) (CLITLSResolution, error) {
	if target == nil {
		return CLITLSResolution{}, errors.New("client: target is required")
	}

	expectedHostID := strings.ToLower(strings.TrimSpace(opts.ExpectedHostID))
	strictTLS := opts.RequireTLS || target.Scheme() == "https" || expectedHostID != ""
	if !strictTLS && opts.KnownHosts != nil && opts.KnownHosts.HasPinnedAddress(target.Host) {
		strictTLS = true
	}

	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	probe, probeErr := ProbePeerTLS(probeCtx, target.Host)
	cancel()

	if probeErr == nil && probe.TLSActive && probe.HostID != "" {
		if expectedHostID != "" && !strings.EqualFold(probe.HostID, expectedHostID) {
			return CLITLSResolution{}, fmt.Errorf("%w: expected hostId=%s got=%s", ErrPeerHostKeyMismatch, expectedHostID, probe.HostID)
		}
		if opts.KnownHosts != nil {
			if err := opts.KnownHosts.VerifyHost(probe.HostID, probe.PublicKey); err != nil {
				return CLITLSResolution{}, err
			}
			if expectedHostID == "" {
				if err := opts.KnownHosts.VerifyAddressBinding(target.Host, probe.HostID); err != nil {
					return CLITLSResolution{}, err
				}
			}
		}

		hc := NewPeerHTTPClient(probe.HostID, probe.PublicKey, nil)
		upgradedTarget := target.WithScheme("https")
		pCopy := probe
		return CLITLSResolution{
			HTTPClient: hc,
			Target:     upgradedTarget,
			Probe:      &pCopy,
			TLSActive:  true,
		}, nil
	}

	// TLS probe did not succeed. Never downgrade when strict TLS applies.
	if strictTLS {
		if probeErr != nil {
			return CLITLSResolution{}, fmt.Errorf("%w: %v (refusing insecure plaintext fallback)", ErrTLSRequired, probeErr)
		}
		return CLITLSResolution{}, fmt.Errorf("%w: peer at %s did not present a verified Ed25519 TLS 1.3 identity", ErrTLSRequired, target.Host)
	}

	// Unencrypted peer when RequireTLS is disabled and target was not https:// or pinned.
	return CLITLSResolution{
		HTTPClient: nil,
		Target:     target,
		Probe:      nil,
		TLSActive:  false,
	}, nil
}
