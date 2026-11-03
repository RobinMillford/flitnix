package power

import (
	"os"
	"testing"
)

// nativeTestsEnv is the explicit opt-in for tests that drive the real
// platform power API (systemd/logind on Linux, SetThreadExecutionState on
// Windows, IOPMAssertionCreateWithDescription on macOS).
//
// These are integration tests: they need actual privileges. GitHub-hosted
// CI runners expose a system D-Bus, so the Linux backend reports itself as
// supported, but polkit denies the logind Inhibit call for non-local
// sessions ("Permission denied"). Merely seeing a reachable bus does not
// mean inhibition may be exercised, so these tests are skipped by default
// and run only when an operator opts in on a machine that can actually
// inhibit sleep:
//
//	FLITNIX_POWER_NATIVE_TESTS=1 go test ./internal/power/
//
// When opted in, the tests run unmodified and a real acquisition failure
// remains a test failure. Ordinary mock-backend unit tests in this package
// never consult this variable and always run.
const nativeTestsEnv = "FLITNIX_POWER_NATIVE_TESTS"

// requireNativeTestsOptIn skips the calling test unless the operator
// explicitly opted in via nativeTestsEnv. The skip message names the exact
// variable so CI logs and local runs stay self-explanatory.
func requireNativeTestsOptIn(t *testing.T) {
	t.Helper()
	if os.Getenv(nativeTestsEnv) != "1" {
		t.Skipf("native power integration test disabled in restricted environments (logind Inhibit requires privileges CI runners do not have); set %s=1 to run against the real platform power API", nativeTestsEnv)
	}
}
