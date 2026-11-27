package devices

import (
	"strings"
	"testing"
	"time"
)

func TestVisibilityStateMachineAndTemporaryExpiry(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	var persisted []VisibilityState
	var changed []VisibilityState

	mgr := NewVisibilityManager(VisibilityState{}, clock)
	mgr.SetHooks(
		func(st VisibilityState) { changed = append(changed, st) },
		func(st VisibilityState) error {
			persisted = append(persisted, st)
			return nil
		},
	)

	// Default mode is trusted.
	if st := mgr.Evaluate(); st.Mode != VisibilityTrustedOnly {
		t.Fatalf("default mode = %q, want %q", st.Mode, VisibilityTrustedOnly)
	}

	// Switch to hidden first, then open temporary_everyone: it should remember "hidden".
	if _, err := mgr.SetMode(VisibilityHidden, 0); err != nil {
		t.Fatalf("SetMode(hidden): %v", err)
	}
	st, err := mgr.SetMode(VisibilityTemporaryEveryone, 10*time.Minute)
	if err != nil {
		t.Fatalf("SetMode(temporary_everyone): %v", err)
	}
	if st.Mode != VisibilityTemporaryEveryone || st.PreviousRestrictiveMode != VisibilityHidden {
		t.Fatalf("unexpected temporary state: %+v", st)
	}
	if st.RemainingSeconds != 600 {
		t.Fatalf("remainingSeconds = %d, want 600", st.RemainingSeconds)
	}

	// Advance 9m59s: still temporary_everyone.
	now = now.Add(9*time.Minute + 59*time.Second)
	st = mgr.Evaluate()
	if st.Mode != VisibilityTemporaryEveryone || st.RemainingSeconds != 1 {
		t.Fatalf("before expiry state = %+v", st)
	}

	// Advance past 10m: must revert automatically to PreviousRestrictiveMode (hidden).
	now = now.Add(2 * time.Second)
	st = mgr.Evaluate()
	if st.Mode != VisibilityHidden {
		t.Fatalf("after expiry mode = %q, want %q", st.Mode, VisibilityHidden)
	}
	if !st.TemporaryExpiresAt.IsZero() || st.RemainingSeconds != 0 {
		t.Fatalf("expected cleared expiry after revert, got %+v", st)
	}

	// Restart recovery rule: expired temporary_everyone must NEVER resurrect on startup.
	restored := NewVisibilityManager(VisibilityState{
		Mode:                    VisibilityTemporaryEveryone,
		PreviousRestrictiveMode: VisibilityTrustedOnly,
		TemporaryExpiresAt:      now.Add(-5 * time.Minute),
	}, clock)
	if got := restored.Evaluate(); got.Mode != VisibilityTrustedOnly {
		t.Fatalf("expired restart mode = %q, want %q", got.Mode, VisibilityTrustedOnly)
	}

	// Missing expiration timestamp on restart also fails closed to previous restrictive mode.
	malformed := NewVisibilityManager(VisibilityState{
		Mode:                    VisibilityTemporaryEveryone,
		PreviousRestrictiveMode: VisibilityHidden,
	}, clock)
	if got := malformed.Evaluate(); got.Mode != VisibilityHidden {
		t.Fatalf("malformed temporary mode = %q, want %q", got.Mode, VisibilityHidden)
	}

	// QR pause toggle works independently of discovery mode.
	paused, err := mgr.SetQRSharingPaused(true)
	if err != nil || !paused.QRSharingPaused {
		t.Fatalf("SetQRSharingPaused(true) failed: %+v err=%v", paused, err)
	}
	if len(persisted) == 0 || len(changed) == 0 {
		t.Fatalf("expected persist and change callbacks to fire")
	}
}

func TestEvaluateAutoApprovalRules(t *testing.T) {
	// Unknown / untrusted device must never auto-approve.
	if dec := EvaluateAutoApproval(nil, VisibilityEveryone, []ApprovalCandidate{{Name: "photo.jpg", Size: 1024}}); dec.AutoApproved {
		t.Fatalf("nil device must not auto-approve: %+v", dec)
	}

	dev := &Device{
		ID:          "dev-1",
		Name:        "Pixel 9",
		AutoApprove: false,
	}
	if dec := EvaluateAutoApproval(dev, VisibilityEveryone, []ApprovalCandidate{{Name: "photo.jpg", Size: 1024}}); dec.AutoApproved {
		t.Fatalf("AutoApprove=false must require manual approval: %+v", dec)
	}

	// Enable auto-approve with default 100 MiB cap.
	dev.AutoApprove = true
	if dec := EvaluateAutoApproval(dev, VisibilityTrustedOnly, []ApprovalCandidate{{Name: "photo.jpg", Size: 5 * 1024 * 1024}}); !dec.AutoApproved {
		t.Fatalf("safe file within default cap should auto-approve: %+v", dec)
	}

	// Exceeding default 100 MiB cap must require manual confirmation.
	oversized := EvaluateAutoApproval(dev, VisibilityTrustedOnly, []ApprovalCandidate{{Name: "movie.mp4", Size: DefaultMaxAutoApproveBytes + 1}})
	if oversized.AutoApproved || oversized.RequiresManualReason == "" {
		t.Fatalf("oversized file must require manual approval: %+v", oversized)
	}

	// Executables and scripts must require manual confirmation by default.
	exeDec := EvaluateAutoApproval(dev, VisibilityTrustedOnly, []ApprovalCandidate{{Name: "setup.exe", Size: 1024}})
	if exeDec.AutoApproved || len(exeDec.RiskyFiles) != 1 || !strings.Contains(exeDec.RiskyFiles[0], "setup.exe") {
		t.Fatalf("executable should block auto-approve: %+v", exeDec)
	}
	shDec := EvaluateAutoApproval(dev, VisibilityTrustedOnly, []ApprovalCandidate{{Name: "install.sh", Size: 512}})
	if shDec.AutoApproved || len(shDec.RiskyFiles) != 1 || !strings.Contains(shDec.RiskyFiles[0], "install.sh") {
		t.Fatalf("script should block auto-approve: %+v", shDec)
	}

	// .bin must NOT be treated as an OS executable so raw binary data/test blobs aren't disrupted.
	if binDec := EvaluateAutoApproval(dev, VisibilityTrustedOnly, []ApprovalCandidate{{Name: "backup.bin", Size: 512}}); !binDec.AutoApproved {
		t.Fatalf(".bin should remain RiskSafe: %+v", binDec)
	}

	// Explicit policy override allows executables / larger sizes.
	dev.AllowExecutables = true
	dev.MaxAutoApproveBytes = -1
	if dec := EvaluateAutoApproval(dev, VisibilityTrustedOnly, []ApprovalCandidate{
		{Name: "installer.dmg", Size: 500 * 1024 * 1024},
	}); !dec.AutoApproved {
		t.Fatalf("explicit executable+unlimited override should auto-approve: %+v", dec)
	}
}
