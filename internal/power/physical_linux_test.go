//go:build linux

package power

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func checkSystemdInhibitContainsFlitnix() bool {
	out, err := exec.Command("systemd-inhibit", "--list").CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Flitnix") && strings.Contains(line, "sleep") && strings.Contains(line, "block") {
			return true
		}
	}
	return false
}

func TestPhysical_SystemdInhibitTransitions(t *testing.T) {
	// Native logind integration: requires real privileges polkit denies to
	// non-local CI sessions. Opt-in only (see native_optin_test.go).
	requireNativeTestsOptIn(t)

	mgr := New()
	defer mgr.Close()

	st := mgr.Status()
	if !st.Supported {
		t.Skip("systemd-logind D-Bus not supported in this environment, skipping physical verification")
	}

	// 1. Initial state: Flitnix must not be in systemd-inhibit
	if checkSystemdInhibitContainsFlitnix() {
		t.Fatal("pre-condition failed: Flitnix already present in systemd-inhibit")
	}

	// 2. Start Transfer 1
	l1, err := mgr.Acquire("Transfer 1: physical_test_archive.tar")
	if err != nil {
		t.Fatalf("Acquire 1 failed: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if !checkSystemdInhibitContainsFlitnix() {
		t.Fatal("systemd-inhibit does not show Flitnix during active transfer 1")
	}
	if st := mgr.Status(); st.Holders != 1 || !st.Active {
		t.Fatalf("expected holders = 1, active = true, got %+v", st)
	}

	// 3. Start Transfer 2 (concurrent)
	l2, err := mgr.Acquire("Transfer 2: physical_test_video.mp4")
	if err != nil {
		t.Fatalf("Acquire 2 failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if !checkSystemdInhibitContainsFlitnix() {
		t.Fatal("systemd-inhibit dropped Flitnix during transfer 2")
	}
	if st := mgr.Status(); st.Holders != 2 {
		t.Fatalf("expected holders = 2, got %d", st.Holders)
	}

	// 4. Release Transfer 1 (Transfer 2 still active)
	l1.Release()
	time.Sleep(100 * time.Millisecond)

	if !checkSystemdInhibitContainsFlitnix() {
		t.Fatal("systemd-inhibit dropped Flitnix prematurely when only 1 of 2 transfers completed")
	}
	if st := mgr.Status(); st.Holders != 1 || !st.Active {
		t.Fatalf("expected holders = 1, active = true, got %+v", st)
	}

	// 5. Release Transfer 2 (final active transfer)
	l2.Release()
	time.Sleep(150 * time.Millisecond)

	if checkSystemdInhibitContainsFlitnix() {
		t.Fatal("systemd-inhibit still holds Flitnix after all transfers released")
	}
	if st := mgr.Status(); st.Holders != 0 || st.Active {
		t.Fatalf("expected holders = 0, active = false, got %+v", st)
	}
}
