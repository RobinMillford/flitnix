package doctor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

type mockCmdResult struct {
	stdout string
	stderr string
	err    error
}

type mockRunner struct {
	responses map[string]mockCmdResult
	calls     []string
}

func (m *mockRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	key := name + " " + strings.Join(args, " ")
	m.calls = append(m.calls, key)
	if res, ok := m.responses[key]; ok {
		return res.stdout, res.stderr, res.err
	}
	if res, ok := m.responses[name]; ok {
		return res.stdout, res.stderr, res.err
	}
	return "", "not found", exec.ErrNotFound
}

func TestFirewallDiagnosticsWindows(t *testing.T) {
	// Blocked / active without allow rule
	blocked := checkFirewallStatus(Params{
		GOOS:       "windows",
		ServerAddr: "192.168.1.50:43821",
		CommandRunner: &mockRunner{
			responses: map[string]mockCmdResult{
				"netsh advfirewall show allprofiles state":      {stdout: "Private Profile Settings:\r\nState                                 ON\r\n"},
				"netsh advfirewall firewall show rule name=all": {stdout: "Rule Name: Something Else\r\nDirection: In\r\nAction: Allow\r\n"},
			},
		},
	})
	if blocked.FirewallState != FirewallStateBlocked || blocked.AffectedPort != 43821 || !strings.Contains(blocked.Command, "43821") {
		t.Fatalf("unexpected Windows blocked result: %+v", blocked)
	}

	// Allowed rule present
	allowed := checkFirewallStatus(Params{
		GOOS:       "windows",
		ServerAddr: "192.168.1.50:43821",
		CommandRunner: &mockRunner{
			responses: map[string]mockCmdResult{
				"netsh advfirewall show allprofiles state":      {stdout: "State ON\r\n"},
				"netsh advfirewall firewall show rule name=all": {stdout: "Rule Name: Flitnix LAN Transfer\r\nEnabled: Yes\r\nAction: Allow\r\nLocalPort: 43821\r\n"},
			},
		},
	})
	if allowed.FirewallState != FirewallStateAllowed || allowed.Status != StatusPass {
		t.Fatalf("unexpected Windows allowed result: %+v", allowed)
	}
}

func TestFirewallDiagnosticsLinuxAndMacOS(t *testing.T) {
	// Linux UFW active without port
	ufwBlocked := checkFirewallStatus(Params{
		GOOS:       "linux",
		ServerAddr: "192.168.1.50:43821",
		CommandRunner: &mockRunner{
			responses: map[string]mockCmdResult{
				"ufw status": {stdout: "Status: active\n\nTo Action From\n22/tcp ALLOW Anywhere\n"},
			},
		},
	})
	if ufwBlocked.FirewallState != FirewallStateBlocked || !strings.Contains(ufwBlocked.Command, "ufw allow 43821/tcp") {
		t.Fatalf("unexpected UFW blocked result: %+v", ufwBlocked)
	}

	// Linux UFW permission denied
	ufwPerm := checkFirewallStatus(Params{
		GOOS:       "linux",
		ServerAddr: "192.168.1.50:43821",
		CommandRunner: &mockRunner{
			responses: map[string]mockCmdResult{
				"ufw status": {stderr: "ERROR: You need to be root to run this script", err: errors.New("exit status 1")},
			},
		},
	})
	if ufwPerm.FirewallState != FirewallStateInsufficientPermissions {
		t.Fatalf("expected insufficient_permissions, got %+v", ufwPerm)
	}

	// Linux firewalld running with port allowed
	fwdAllowed := checkFirewallStatus(Params{
		GOOS:       "linux",
		ServerAddr: "192.168.1.50:43821",
		CommandRunner: &mockRunner{
			responses: map[string]mockCmdResult{
				"firewall-cmd --state":      {stdout: "running\n"},
				"firewall-cmd --list-ports": {stdout: "43821/tcp\n"},
			},
		},
	})
	if fwdAllowed.FirewallState != FirewallStateAllowed || fwdAllowed.Status != StatusPass {
		t.Fatalf("expected firewalld allowed, got %+v", fwdAllowed)
	}

	// macOS block-all mode
	sfw := "/usr/libexec/ApplicationFirewall/socketfilterfw"
	macBlocked := checkFirewallStatus(Params{
		GOOS:           "darwin",
		ServerAddr:     "192.168.1.50:43821",
		ExecutablePath: "/Applications/Flitnix.app/Contents/MacOS/flitnix",
		CommandRunner: &mockRunner{
			responses: map[string]mockCmdResult{
				sfw + " --getglobalstate": {stdout: "Firewall is enabled. (State = 1)"},
				sfw + " --getblockall":    {stdout: "Block all non-essential incoming connections is enabled."},
			},
		},
	})
	if macBlocked.FirewallState != FirewallStateBlocked || !strings.Contains(macBlocked.Command, "--setblockall off") {
		t.Fatalf("unexpected macOS blockall result: %+v", macBlocked)
	}

	// Missing utility fallback
	missing := checkFirewallStatus(Params{
		GOOS:          "linux",
		ServerAddr:    "192.168.1.50:43821",
		CommandRunner: &mockRunner{responses: map[string]mockCmdResult{}},
	})
	if missing.FirewallState != FirewallStateUnavailable {
		t.Fatalf("expected unavailable fallback, got %+v", missing)
	}
}
