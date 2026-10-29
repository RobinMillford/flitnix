package devices

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDevicesStoreLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "devices.json")

	store, err := NewStore(storePath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}

	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	// 1. Add device
	dev, err := store.Add("Pixel 8 Pro", pub, true)
	if err != nil {
		t.Fatalf("store.Add failed: %v", err)
	}

	if dev.Name != "Pixel 8 Pro" {
		t.Errorf("got name %q, want 'Pixel 8 Pro'", dev.Name)
	}
	if !dev.AutoApprove {
		t.Errorf("got AutoApprove false, want true")
	}

	// 2. Get device
	got, ok := store.Get(dev.ID)
	if !ok || got.ID != dev.ID {
		t.Fatalf("store.Get failed or ID mismatch: %v", got)
	}

	// 3. List
	list := store.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 device, got %d", len(list))
	}

	// 4. Verify signature
	msg := []byte("Flitnix Pair Auth Challenge: " + time.Now().UTC().Format(time.RFC3339))
	sigHex := SignMessage(priv, msg)

	valid, err := store.VerifySignature(dev.ID, msg, sigHex)
	if err != nil || !valid {
		t.Fatalf("VerifySignature failed: valid=%v, err=%v", valid, err)
	}

	// Bad signature test
	badSigHex := sigHex[:len(sigHex)-4] + "0000"
	validBad, err := store.VerifySignature(dev.ID, msg, badSigHex)
	if validBad {
		t.Errorf("expected bad signature to fail verification")
	}

	// 5. Toggle AutoApprove
	if err := store.SetAutoApprove(dev.ID, false); err != nil {
		t.Fatalf("SetAutoApprove failed: %v", err)
	}
	gotUpdated, _ := store.Get(dev.ID)
	if gotUpdated.AutoApprove {
		t.Errorf("expected AutoApprove to be false after toggle")
	}

	// 6. Persistence reload test
	store2, err := NewStore(storePath)
	if err != nil {
		t.Fatalf("reloading store failed: %v", err)
	}
	if len(store2.List()) != 1 {
		t.Fatalf("expected 1 reloaded device, got %d", len(store2.List()))
	}

	// 7. Remove device
	if err := store2.Remove(dev.ID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if len(store2.List()) != 0 {
		t.Fatalf("expected 0 devices after removal, got %d", len(store2.List()))
	}
}
