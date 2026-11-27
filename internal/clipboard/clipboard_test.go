package clipboard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryBackend struct {
	mu      sync.Mutex
	snap    Snapshot
	writes  []string
	caps    Capabilities
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{
		caps: Capabilities{
			Supported:           true,
			BackendName:         "memory-test",
			ConcealedFilterSafe: true,
		},
	}
}

func (m *memoryBackend) Capabilities() Capabilities {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caps
}

func (m *memoryBackend) SetSnapshot(snap Snapshot) {
	m.mu.Lock()
	m.snap = snap
	m.mu.Unlock()
}

func (m *memoryBackend) Read(ctx context.Context) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap, nil
}

func (m *memoryBackend) WriteText(ctx context.Context, text string) error {
	m.mu.Lock()
	m.snap = Snapshot{Text: text}
	m.writes = append(m.writes, text)
	m.mu.Unlock()
	return nil
}

func (m *memoryBackend) WriteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.writes)
}

func TestClipboardOptInDefaultOffAndLifecycle(t *testing.T) {
	backend := newMemoryBackend()
	backend.SetSnapshot(Snapshot{Text: "pre-existing secret"})

	var localCount int
	coord := NewCoordinator(Config{
		Backend:      backend,
		PollInterval: 15 * time.Millisecond,
		HostDeviceID: "desktop-1",
		HostName:     "Desktop",
		OnLocalSync:  func(item SyncItem) { localCount++ },
	})
	defer coord.Close()

	// Default is OFF; polling returns ErrDisabled and does not capture pre-existing clipboard.
	if coord.Enabled() {
		t.Fatal("expected AutoClipboardSync to default to OFF")
	}
	if _, err := coord.PollOnce(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("PollOnce when disabled = %v; want ErrDisabled", err)
	}
	if st := coord.Status(); st.Watching || st.HasPendingItem {
		t.Fatalf("expected not watching when disabled: %+v", st)
	}

	// Enabling without authorized peer keeps watcher paused.
	coord.SetEnabled(true)
	if coord.Status().Watching {
		t.Fatal("watcher should not run without an active authorized peer")
	}

	// Activating an authorized peer starts watcher without broadcasting pre-existing clipboard.
	coord.SetActiveAuthorizedPeers(true)
	if !coord.Status().Watching {
		t.Fatal("expected watcher active with enabled=true and active authorized peer")
	}
	time.Sleep(35 * time.Millisecond)
	if localCount != 0 {
		t.Fatalf("pre-existing clipboard text was unexpectedly pushed (%d times)", localCount)
	}

	// New clipboard change triggers local sync once.
	backend.SetSnapshot(Snapshot{Text: "copied-after-opt-in"})
	changed, err := coord.PollOnce(context.Background())
	if err != nil || !changed {
		t.Fatalf("PollOnce on new text changed=%v err=%v; want true, nil", changed, err)
	}
	// Duplicate poll does not emit again.
	changedAgain, err := coord.PollOnce(context.Background())
	if err != nil || changedAgain {
		t.Fatalf("duplicate PollOnce changed=%v err=%v; want false, nil", changedAgain, err)
	}

	// Disabling stops watcher and wipes transient item.
	coord.SetEnabled(false)
	if st := coord.Status(); st.Watching || st.HasPendingItem {
		t.Fatalf("after disable: %+v", st)
	}
	if _, ok := coord.Latest(); ok {
		t.Fatal("expected Latest() cleared when disabled")
	}
}

func TestClipboardEchoLoopConcealedAndSizeBounds(t *testing.T) {
	backend := newMemoryBackend()
	var remoteItems []SyncItem
	var localItems []SyncItem

	coord := NewCoordinator(Config{
		Backend:      backend,
		HostDeviceID: "desktop-1",
		HostName:     "Desktop",
		OnLocalSync:  func(item SyncItem) { localItems = append(localItems, item) },
		OnRemoteSync: func(item SyncItem) { remoteItems = append(remoteItems, item) },
	})
	defer coord.Close()
	coord.SetEnabled(true)

	// 1. Oversized (> 64 KiB) rejected clearly.
	tooBig := strings.Repeat("x", MaxContentBytes+1)
	if _, _, err := coord.ReceiveRemote(context.Background(), "dev-phone", "Phone", tooBig); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("ReceiveRemote >64KiB = %v; want ErrPayloadTooLarge", err)
	}
	backend.SetSnapshot(Snapshot{Text: tooBig})
	if _, err := coord.PollOnce(context.Background()); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("PollOnce >64KiB = %v; want ErrPayloadTooLarge", err)
	}

	// 2. Invalid UTF-8 rejected.
	if _, _, err := coord.ReceiveRemote(context.Background(), "dev-phone", "Phone", string([]byte{0xff, 0xfe})); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("ReceiveRemote invalid UTF-8 = %v; want ErrInvalidUTF8", err)
	}

	// 3. Concealed password manager item skipped cleanly.
	backend.SetSnapshot(Snapshot{Text: "hunter2", Concealed: true, Reason: "password manager"})
	if changed, err := coord.PollOnce(context.Background()); changed || !errors.Is(err, ErrConcealedContent) {
		t.Fatalf("concealed item changed=%v err=%v; want false, ErrConcealedContent", changed, err)
	}

	// 4. Remote receive updates OS clipboard cleanly and MUST NOT echo back via PollOnce.
	item, applied, err := coord.ReceiveRemote(context.Background(), "dev-phone", "Phone", "hello from phone")
	if err != nil || !applied || item.Content != "hello from phone" {
		t.Fatalf("ReceiveRemote valid failed: item=%+v applied=%v err=%v", item, applied, err)
	}
	if backend.WriteCount() != 1 || len(remoteItems) != 1 {
		t.Fatalf("expected 1 backend write and 1 remote callback")
	}

	// Immediate local poll sees "hello from phone" on OS clipboard -> MUST suppress echo!
	changed, err := coord.PollOnce(context.Background())
	if err != nil || changed || len(localItems) != 0 {
		t.Fatalf("echo loop occurred! changed=%v err=%v localCount=%d", changed, err, len(localItems))
	}

	// Duplicate remote resend within echo window is deduplicated without double-writing.
	_, appliedDup, err := coord.ReceiveRemote(context.Background(), "dev-phone", "Phone", "hello from phone")
	if err != nil || appliedDup {
		t.Fatalf("expected duplicate remote clipboard text deduplicated: applied=%v err=%v", appliedDup, err)
	}
}
