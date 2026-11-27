package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/devices"
	"github.com/itsyamin/flitnix/internal/settings"
)

func TestAppVisibilityPersistenceAndRestartExpiry(t *testing.T) {
	dir := isolateAppStorage(t)
	app := newIsolatedAppIn(t, dir)
	t.Cleanup(func() { app.shutdown(context.Background()) })

	st, err := app.SetVisibilityMode(string(devices.VisibilityHidden))
	if err != nil || st.Mode != devices.VisibilityHidden {
		t.Fatalf("SetVisibilityMode(hidden): st=%+v err=%v", st, err)
	}

	st, err = app.SetVisibilityMode(string(devices.VisibilityTemporaryEveryone))
	if err != nil || st.Mode != devices.VisibilityTemporaryEveryone || st.PreviousRestrictiveMode != devices.VisibilityHidden {
		t.Fatalf("SetVisibilityMode(temporary_everyone): st=%+v err=%v", st, err)
	}

	paused, err := app.SetQRSharingPaused(true)
	if err != nil || !paused.QRSharingPaused {
		t.Fatalf("SetQRSharingPaused: st=%+v err=%v", paused, err)
	}

	// Write an already-expired temporary_everyone state into settings.json and
	// verify a restarted App reverts to PreviousRestrictiveMode instead of staying open.
	settingsPath := filepath.Join(resolveDataDir(), "settings.json")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var disk settings.Settings
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("unmarshal settings.json: %v", err)
	}
	disk.VisibilityMode = string(devices.VisibilityTemporaryEveryone)
	disk.VisibilityPreviousMode = string(devices.VisibilityHidden)
	disk.VisibilityExpiresAt = time.Now().UTC().Add(-2 * time.Minute)
	updated, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		t.Fatalf("marshal settings.json: %v", err)
	}
	if err := os.WriteFile(settingsPath, updated, 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	restarted := newIsolatedAppIn(t, dir)
	t.Cleanup(func() { restarted.shutdown(context.Background()) })

	got := restarted.GetVisibilityState()
	if got.Mode != devices.VisibilityHidden {
		t.Fatalf("restarted expired temporary mode = %q, want %q", got.Mode, devices.VisibilityHidden)
	}
}

func TestAppRemoveTrustedDeviceRevokesActiveDeviceSession(t *testing.T) {
	app := newIsolatedApp(t)
	t.Cleanup(func() { app.shutdown(context.Background()) })

	pub, _, err := devices.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	dev, err := app.devicesStore.Add("Phone A", pub, true)
	if err != nil {
		t.Fatalf("devicesStore.Add: %v", err)
	}

	devSess, err := app.sessions.CreateForDevice(dev.ID)
	if err != nil {
		t.Fatalf("CreateForDevice: %v", err)
	}
	if _, err := app.sessions.Validate(devSess.Token()); err != nil {
		t.Fatalf("device session should be valid before removal: %v", err)
	}

	if err := app.SetDevicePolicy(dev.ID, devices.DevicePolicy{
		AutoApprove:         true,
		MaxAutoApproveBytes: 4096,
	}); err != nil {
		t.Fatalf("SetDevicePolicy: %v", err)
	}
	updatedDev, ok := app.devicesStore.Get(dev.ID)
	if !ok || updatedDev.MaxAutoApproveBytes != 4096 {
		t.Fatalf("updated device policy mismatch: ok=%v dev=%+v", ok, updatedDev)
	}

	if err := app.RemoveTrustedDevice(dev.ID); err != nil {
		t.Fatalf("RemoveTrustedDevice: %v", err)
	}
	if _, err := app.sessions.Validate(devSess.Token()); err == nil {
		t.Fatalf("revoked device session must fail validation immediately")
	}
}
