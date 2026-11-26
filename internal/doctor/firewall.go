package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	FirewallStateAllowed                 = "allowed"
	FirewallStateBlocked                 = "blocked"
	FirewallStateUnknown                 = "unknown"
	FirewallStateUnavailable             = "unavailable"
	FirewallStateInsufficientPermissions = "insufficient_permissions"

	defaultFirewallTimeout = 1500 * time.Millisecond
)

// CommandRunner abstracts read-only OS firewall inspection commands for testing.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, stderr string, err error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

func resolveAffectedPort(serverAddr string) int {
	if serverAddr != "" {
		if _, pStr, err := net.SplitHostPort(serverAddr); err == nil {
			if p, convErr := strconv.Atoi(pStr); convErr == nil && p > 0 {
				return p
			}
		}
	}
	return 43821
}

func checkFirewallStatus(p Params) DiagnosticItem {
	goos := strings.TrimSpace(p.GOOS)
	if goos == "" {
		goos = runtime.GOOS
	}
	runner := p.CommandRunner
	if runner == nil {
		runner = execCommandRunner{}
	}
	port := resolveAffectedPort(p.ServerAddr)
	exePath := strings.TrimSpace(p.ExecutablePath)
	if exePath == "" {
		if resolved, err := os.Executable(); err == nil {
			exePath = resolved
		} else {
			exePath = "flitnix"
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultFirewallTimeout)
	defer cancel()

	switch goos {
	case "windows":
		return checkWindowsFirewall(ctx, runner, port)
	case "linux":
		return checkLinuxFirewall(ctx, runner, port)
	case "darwin":
		return checkDarwinFirewall(ctx, runner, port, exePath)
	default:
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "Host Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateUnavailable,
			AffectedPort:  port,
			Message:       fmt.Sprintf("Automated firewall inspection is unsupported on %s. Ensure TCP port %d is open for local LAN connections.", goos, port),
			Remediation:   fmt.Sprintf("Allow inbound TCP traffic on port %d from your local Wi-Fi/LAN subnet.", port),
		}
	}
}

func isCommandNotFound(err error, combined string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	lower := strings.ToLower(combined + " " + err.Error())
	return strings.Contains(lower, "executable file not found") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "no such file or directory")
}

func isPermissionDenied(combined string, err error) bool {
	lower := strings.ToLower(combined)
	if err != nil {
		lower += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "you need to be root") ||
		strings.Contains(lower, "must be run as root") ||
		strings.Contains(lower, "requires elevation") ||
		strings.Contains(lower, "access is denied") ||
		strings.Contains(lower, "operation not permitted") ||
		strings.Contains(lower, "not authorized")
}

func checkWindowsFirewall(ctx context.Context, runner CommandRunner, port int) DiagnosticItem {
	cmdHint := fmt.Sprintf(`netsh advfirewall firewall add rule name="Flitnix LAN Transfer" dir=in action=allow protocol=TCP localport=%d profile=private`, port)

	stateOut, stateErrOut, stateErr := runner.Run(ctx, "netsh", "advfirewall", "show", "allprofiles", "state")
	if isCommandNotFound(stateErr, stateErrOut) {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "Windows Defender Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateUnavailable,
			AffectedPort:  port,
			Message:       "Could not locate Windows netsh utility to inspect firewall profiles.",
			Remediation:   fmt.Sprintf("Verify Windows Defender Firewall allows inbound TCP port %d on your private network.", port),
			Command:       cmdHint,
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "Windows Defender Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateUnknown,
			AffectedPort:  port,
			Message:       "Timed out checking Windows Firewall status.",
			Remediation:   fmt.Sprintf("Allow inbound TCP port %d in Windows Defender Firewall if phones cannot connect.", port),
			Command:       cmdHint,
		}
	}

	rulesOut, rulesErrOut, rulesErr := runner.Run(ctx, "netsh", "advfirewall", "firewall", "show", "rule", "name=all")
	combined := stateOut + "\n" + stateErrOut + "\n" + rulesOut + "\n" + rulesErrOut
	if isPermissionDenied(combined, rulesErr) && rulesOut == "" {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "Windows Defender Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateInsufficientPermissions,
			AffectedPort:  port,
			Message:       "Firewall rules could not be read due to restricted permissions.",
			Remediation:   fmt.Sprintf("Run the rule below from an Administrator PowerShell prompt to allow TCP port %d.", port),
			Command:       cmdHint,
		}
	}

	// If all active profiles report State OFF, firewall is inactive.
	if stateErr == nil && strings.Contains(strings.ToUpper(stateOut), "OFF") && !strings.Contains(strings.ToUpper(stateOut), "ON") {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "Windows Defender Firewall",
			Status:        StatusPass,
			FirewallState: FirewallStateAllowed,
			AffectedPort:  port,
			Message:       fmt.Sprintf("Windows Defender Firewall profiles are disabled; TCP port %d is reachable.", port),
		}
	}

	portStr := strconv.Itoa(port)
	lowerRules := strings.ToLower(rulesOut)
	hasAllow := (strings.Contains(lowerRules, "flitnix") || strings.Contains(lowerRules, portStr)) &&
		strings.Contains(lowerRules, "allow")
	hasExplicitBlock := (strings.Contains(lowerRules, "flitnix") || strings.Contains(lowerRules, portStr)) &&
		strings.Contains(lowerRules, "block") && !hasAllow

	if hasAllow {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "Windows Defender Firewall",
			Status:        StatusPass,
			FirewallState: FirewallStateAllowed,
			AffectedPort:  port,
			Message:       fmt.Sprintf("Windows Firewall allows Flitnix / TCP port %d.", port),
		}
	}
	if hasExplicitBlock || strings.Contains(strings.ToUpper(stateOut), "ON") {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "Windows Defender Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateBlocked,
			AffectedPort:  port,
			Message:       fmt.Sprintf("Windows Defender Firewall is active and no explicit inbound Allow rule was found for Flitnix or TCP port %d.", port),
			Remediation:   fmt.Sprintf("Add an inbound Private network firewall rule for TCP port %d if phones fail to connect.", port),
			Command:       cmdHint,
		}
	}

	return DiagnosticItem{
		ID:            "firewall_status",
		Title:         "Windows Defender Firewall",
		Status:        StatusWarn,
		FirewallState: FirewallStateUnknown,
		AffectedPort:  port,
		Message:       fmt.Sprintf("Unable to confirm whether TCP port %d is permitted by Windows Firewall.", port),
		Remediation:   fmt.Sprintf("Run the command below as Administrator if remote devices time out reaching port %d.", port),
		Command:       cmdHint,
	}
}

func checkLinuxFirewall(ctx context.Context, runner CommandRunner, port int) DiagnosticItem {
	portStr := strconv.Itoa(port)
	ufwCmd := fmt.Sprintf("sudo ufw allow %d/tcp", port)
	fwdCmd := fmt.Sprintf("sudo firewall-cmd --add-port=%d/tcp --permanent && sudo firewall-cmd --reload", port)

	ufwOut, ufwErrOut, ufwErr := runner.Run(ctx, "ufw", "status")
	ufwCombined := ufwOut + "\n" + ufwErrOut
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "Linux Host Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateUnknown,
			AffectedPort:  port,
			Message:       "Timed out checking Linux firewall status.",
			Remediation:   fmt.Sprintf("Ensure TCP port %d is allowed in your Linux firewall.", port),
			Command:       ufwCmd,
		}
	}

	if !isCommandNotFound(ufwErr, ufwCombined) {
		if isPermissionDenied(ufwCombined, ufwErr) {
			return DiagnosticItem{
				ID:            "firewall_status",
				Title:         "Linux Firewall (UFW)",
				Status:        StatusWarn,
				FirewallState: FirewallStateInsufficientPermissions,
				AffectedPort:  port,
				Message:       "Checking UFW status requires elevated privileges on this host.",
				Remediation:   fmt.Sprintf("Check UFW status or allow TCP port %d using sudo.", port),
				Command:       ufwCmd,
			}
		}
		lower := strings.ToLower(ufwOut)
		if strings.Contains(lower, "status: inactive") {
			return DiagnosticItem{
				ID:            "firewall_status",
				Title:         "Linux Firewall (UFW)",
				Status:        StatusPass,
				FirewallState: FirewallStateAllowed,
				AffectedPort:  port,
				Message:       fmt.Sprintf("UFW firewall is inactive; TCP port %d is not blocked by UFW.", port),
			}
		}
		if strings.Contains(lower, "status: active") {
			if (strings.Contains(lower, portStr+"/tcp") || strings.Contains(lower, portStr)) && strings.Contains(lower, "allow") {
				return DiagnosticItem{
					ID:            "firewall_status",
					Title:         "Linux Firewall (UFW)",
					Status:        StatusPass,
					FirewallState: FirewallStateAllowed,
					AffectedPort:  port,
					Message:       fmt.Sprintf("UFW is active and allows inbound TCP port %d.", port),
				}
			}
			return DiagnosticItem{
				ID:            "firewall_status",
				Title:         "Linux Firewall (UFW)",
				Status:        StatusWarn,
				FirewallState: FirewallStateBlocked,
				AffectedPort:  port,
				Message:       fmt.Sprintf("UFW firewall is active and does not appear to allow TCP port %d.", port),
				Remediation:   fmt.Sprintf("Allow inbound TCP port %d through UFW so phones and other desktops can connect.", port),
				Command:       ufwCmd,
			}
		}
	}

	// Try firewalld (firewall-cmd)
	fwdStateOut, fwdStateErrOut, fwdStateErr := runner.Run(ctx, "firewall-cmd", "--state")
	fwdCombined := fwdStateOut + "\n" + fwdStateErrOut
	if !isCommandNotFound(fwdStateErr, fwdCombined) {
		if isPermissionDenied(fwdCombined, fwdStateErr) {
			return DiagnosticItem{
				ID:            "firewall_status",
				Title:         "Linux Firewall (firewalld)",
				Status:        StatusWarn,
				FirewallState: FirewallStateInsufficientPermissions,
				AffectedPort:  port,
				Message:       "Inspecting firewalld requires elevated permissions on this host.",
				Remediation:   fmt.Sprintf("Allow TCP port %d in firewalld using the command below.", port),
				Command:       fwdCmd,
			}
		}
		lowerState := strings.ToLower(strings.TrimSpace(fwdStateOut))
		if lowerState == "not running" {
			return DiagnosticItem{
				ID:            "firewall_status",
				Title:         "Linux Firewall (firewalld)",
				Status:        StatusPass,
				FirewallState: FirewallStateAllowed,
				AffectedPort:  port,
				Message:       fmt.Sprintf("firewalld is not running; TCP port %d is open locally.", port),
			}
		}
		if strings.Contains(lowerState, "running") {
			portsOut, _, _ := runner.Run(ctx, "firewall-cmd", "--list-ports")
			if strings.Contains(portsOut, portStr+"/tcp") {
				return DiagnosticItem{
					ID:            "firewall_status",
					Title:         "Linux Firewall (firewalld)",
					Status:        StatusPass,
					FirewallState: FirewallStateAllowed,
					AffectedPort:  port,
					Message:       fmt.Sprintf("firewalld is running and permits TCP port %d.", port),
				}
			}
			return DiagnosticItem{
				ID:            "firewall_status",
				Title:         "Linux Firewall (firewalld)",
				Status:        StatusWarn,
				FirewallState: FirewallStateBlocked,
				AffectedPort:  port,
				Message:       fmt.Sprintf("firewalld is running and TCP port %d was not found in active allowed ports.", port),
				Remediation:   fmt.Sprintf("Add TCP port %d to firewalld and reload.", port),
				Command:       fwdCmd,
			}
		}
	}

	return DiagnosticItem{
		ID:            "firewall_status",
		Title:         "Linux Host Firewall",
		Status:        StatusPass,
		FirewallState: FirewallStateUnavailable,
		AffectedPort:  port,
		Message:       fmt.Sprintf("Neither UFW nor firewalld appears active. If you use custom nftables/iptables rules, verify TCP port %d is allowed.", port),
		Command:       ufwCmd,
	}
}

func checkDarwinFirewall(ctx context.Context, runner CommandRunner, port int, exePath string) DiagnosticItem {
	const sfw = "/usr/libexec/ApplicationFirewall/socketfilterfw"
	unblockCmd := fmt.Sprintf("sudo %s --add %q --unblockapp %q", sfw, exePath, exePath)
	blockAllCmd := fmt.Sprintf("sudo %s --setblockall off", sfw)

	globalOut, globalErrOut, globalErr := runner.Run(ctx, sfw, "--getglobalstate")
	combined := globalOut + "\n" + globalErrOut
	if isCommandNotFound(globalErr, combined) {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "macOS Application Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateUnavailable,
			AffectedPort:  port,
			Message:       "Could not locate macOS socketfilterfw to inspect firewall status.",
			Remediation:   "Verify System Settings -> Network -> Firewall allows Flitnix incoming connections.",
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "macOS Application Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateUnknown,
			AffectedPort:  port,
			Message:       "Timed out checking macOS Application Firewall.",
			Remediation:   "Check System Settings -> Network -> Firewall and allow incoming connections for Flitnix.",
			Command:       unblockCmd,
		}
	}
	if isPermissionDenied(combined, globalErr) {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "macOS Application Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateInsufficientPermissions,
			AffectedPort:  port,
			Message:       "Permission denied querying macOS Application Firewall.",
			Remediation:   "Verify Flitnix is allowed under System Settings -> Network -> Firewall, or run the command below.",
			Command:       unblockCmd,
		}
	}

	lowerGlobal := strings.ToLower(globalOut)
	if strings.Contains(lowerGlobal, "disabled") {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "macOS Application Firewall",
			Status:        StatusPass,
			FirewallState: FirewallStateAllowed,
			AffectedPort:  port,
			Message:       fmt.Sprintf("macOS Application Firewall is disabled; TCP port %d is reachable.", port),
		}
	}

	blockAllOut, _, _ := runner.Run(ctx, sfw, "--getblockall")
	if strings.Contains(strings.ToLower(blockAllOut), "enabled") && !strings.Contains(strings.ToLower(blockAllOut), "disabled") {
		return DiagnosticItem{
			ID:            "firewall_status",
			Title:         "macOS Application Firewall",
			Status:        StatusWarn,
			FirewallState: FirewallStateBlocked,
			AffectedPort:  port,
			Message:       "macOS Application Firewall is set to 'Block all incoming connections', which stops phones and other computers from reaching Flitnix.",
			Remediation:   "Disable 'Block all incoming connections' in System Settings -> Network -> Firewall -> Options.",
			Command:       blockAllCmd,
		}
	}

	listOut, _, _ := runner.Run(ctx, sfw, "--listapps")
	lowerList := strings.ToLower(listOut)
	for _, line := range strings.Split(lowerList, "\n") {
		if strings.Contains(line, "flitnix") {
			if strings.Contains(line, "block") {
				return DiagnosticItem{
					ID:            "firewall_status",
					Title:         "macOS Application Firewall",
					Status:        StatusWarn,
					FirewallState: FirewallStateBlocked,
					AffectedPort:  port,
					Message:       "macOS Application Firewall has blocked incoming connections for Flitnix.",
					Remediation:   "Allow Flitnix under System Settings -> Network -> Firewall -> Options or run the command below.",
					Command:       unblockCmd,
				}
			}
			if strings.Contains(line, "allow") {
				return DiagnosticItem{
					ID:            "firewall_status",
					Title:         "macOS Application Firewall",
					Status:        StatusPass,
					FirewallState: FirewallStateAllowed,
					AffectedPort:  port,
					Message:       fmt.Sprintf("macOS Application Firewall allows Flitnix incoming connections on port %d.", port),
				}
			}
		}
	}

	return DiagnosticItem{
		ID:            "firewall_status",
		Title:         "macOS Application Firewall",
		Status:        StatusWarn,
		FirewallState: FirewallStateUnknown,
		AffectedPort:  port,
		Message:       "macOS Application Firewall is enabled and Flitnix was not explicitly found in the allowed applications list.",
		Remediation:   "Click 'Allow' if macOS prompts for incoming network connections, or add Flitnix manually.",
		Command:       unblockCmd,
	}
}
