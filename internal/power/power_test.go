package power

import (
	"errors"
	"sync"
	"testing"
)

type mockBackend struct {
	mu             sync.Mutex
	supported      bool
	inhibitErr     error
	uninhibitErr   error
	closeErr       error
	inhibitCalls   int
	uninhibitCalls int
	closeCalls     int
	lastReason     string
}

func (m *mockBackend) Supported() bool {
	return m.supported
}

func (m *mockBackend) Inhibit(reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inhibitCalls++
	m.lastReason = reason
	return m.inhibitErr
}

func (m *mockBackend) Uninhibit() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uninhibitCalls++
	return m.uninhibitErr
}

func (m *mockBackend) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCalls++
	return m.closeErr
}

func TestManager_FirstAcquisition(t *testing.T) {
	mock := &mockBackend{supported: true}
	mgr := NewWithBackend(mock)

	st := mgr.Status()
	if st.Active || st.Holders != 0 || !st.Supported {
		t.Fatalf("unexpected initial status: %+v", st)
	}

	lease, err := mgr.Acquire("upload 1")
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}
	if lease == nil {
		t.Fatal("expected non-nil lease")
	}

	mock.mu.Lock()
	if mock.inhibitCalls != 1 || mock.lastReason != "upload 1" {
		t.Errorf("mock inhibit calls = %d, reason = %q; want 1, 'upload 1'", mock.inhibitCalls, mock.lastReason)
	}
	mock.mu.Unlock()

	st = mgr.Status()
	if !st.Active || st.Holders != 1 || st.Reason != "upload 1" {
		t.Errorf("unexpected status after first acquire: %+v", st)
	}

	lease.Release()

	mock.mu.Lock()
	if mock.uninhibitCalls != 1 {
		t.Errorf("mock uninhibit calls = %d; want 1", mock.uninhibitCalls)
	}
	mock.mu.Unlock()

	st = mgr.Status()
	if st.Active || st.Holders != 0 {
		t.Errorf("unexpected status after release: %+v", st)
	}
}

func TestManager_MultipleAcquisitionsAndPartialRelease(t *testing.T) {
	mock := &mockBackend{supported: true}
	mgr := NewWithBackend(mock)

	l1, err := mgr.Acquire("download A")
	if err != nil {
		t.Fatalf("acquire l1: %v", err)
	}
	l2, err := mgr.Acquire("upload B")
	if err != nil {
		t.Fatalf("acquire l2: %v", err)
	}

	mock.mu.Lock()
	if mock.inhibitCalls != 1 {
		t.Errorf("expected exactly 1 native inhibit call, got %d", mock.inhibitCalls)
	}
	mock.mu.Unlock()

	if st := mgr.Status(); st.Holders != 2 || !st.Active {
		t.Errorf("status holders = %d, active = %v; want 2, true", st.Holders, st.Active)
	}

	// Partial release: release l1
	l1.Release()

	mock.mu.Lock()
	if mock.uninhibitCalls != 0 {
		t.Errorf("uninhibit called prematurely on partial release: %d", mock.uninhibitCalls)
	}
	mock.mu.Unlock()

	if st := mgr.Status(); st.Holders != 1 || !st.Active {
		t.Errorf("status holders = %d, active = %v; want 1, true", st.Holders, st.Active)
	}

	// Final release: release l2
	l2.Release()

	mock.mu.Lock()
	if mock.uninhibitCalls != 1 {
		t.Errorf("uninhibit not called on final release: %d", mock.uninhibitCalls)
	}
	mock.mu.Unlock()

	if st := mgr.Status(); st.Holders != 0 || st.Active {
		t.Errorf("status holders = %d, active = %v; want 0, false", st.Holders, st.Active)
	}
}

// TestManager_FullReleaseThenReacquire covers the transition the native
// physical test exercises: a complete release must uninhibit, and the next
// acquire after an idle period must re-inhibit with the new reason.
func TestManager_FullReleaseThenReacquire(t *testing.T) {
	mock := &mockBackend{supported: true}
	mgr := NewWithBackend(mock)

	l1, err := mgr.Acquire("transfer 1")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	l1.Release()

	mock.mu.Lock()
	if mock.inhibitCalls != 1 || mock.uninhibitCalls != 1 {
		mock.mu.Unlock()
		t.Fatalf("after first cycle: inhibit = %d, uninhibit = %d; want 1, 1", mock.inhibitCalls, mock.uninhibitCalls)
	}
	mock.mu.Unlock()

	if st := mgr.Status(); st.Active || st.Holders != 0 || st.Reason != "" {
		t.Errorf("status after full release: %+v; want inactive with empty reason", st)
	}

	l2, err := mgr.Acquire("transfer 2")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	defer l2.Release()

	mock.mu.Lock()
	calls, reason := mock.inhibitCalls, mock.lastReason
	mock.mu.Unlock()
	if calls != 2 {
		t.Errorf("re-acquire inhibit calls = %d; want 2", calls)
	}
	if reason != "transfer 2" {
		t.Errorf("re-acquire reason = %q; want %q", reason, "transfer 2")
	}

	if st := mgr.Status(); !st.Active || st.Holders != 1 || st.Reason != "transfer 2" {
		t.Errorf("status after re-acquire: %+v", st)
	}
}

func TestManager_DoubleReleaseIdempotent(t *testing.T) {
	mock := &mockBackend{supported: true}
	mgr := NewWithBackend(mock)

	lease, err := mgr.Acquire("transfer")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	for i := 0; i < 5; i++ {
		lease.Release()
	}

	mock.mu.Lock()
	if mock.uninhibitCalls != 1 {
		t.Errorf("uninhibit called %d times on repeated release; want 1", mock.uninhibitCalls)
	}
	mock.mu.Unlock()

	if st := mgr.Status(); st.Holders != 0 || st.Active {
		t.Errorf("status holders = %d; want 0", st.Holders)
	}
}

func TestManager_ConcurrentAcquireAndRelease(t *testing.T) {
	mock := &mockBackend{supported: true}
	mgr := NewWithBackend(mock)

	const goroutines = 40
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			lease, err := mgr.Acquire("concurrent task")
			if err != nil {
				t.Errorf("concurrent acquire failed: %v", err)
				return
			}
			// Immediately or shortly release
			lease.Release()
		}(i)
	}

	wg.Wait()

	mock.mu.Lock()
	inh := mock.inhibitCalls
	uninh := mock.uninhibitCalls
	mock.mu.Unlock()

	if inh != uninh {
		t.Errorf("mismatch: inhibit calls = %d, uninhibit calls = %d", inh, uninh)
	}

	if st := mgr.Status(); st.Holders != 0 || st.Active {
		t.Errorf("final status holders = %d, active = %v; want 0, false", st.Holders, st.Active)
	}
}

func TestManager_FailedNativeAcquisition(t *testing.T) {
	expectedErr := errors.New("dbus connection timeout")
	mock := &mockBackend{
		supported:  true,
		inhibitErr: expectedErr,
	}
	mgr := NewWithBackend(mock)

	lease, err := mgr.Acquire("failing transfer")
	if err == nil {
		t.Fatal("expected error on failed native inhibit, got nil")
	}
	if lease != nil {
		t.Fatal("expected nil lease on error")
	}
	if !errors.Is(err, ErrInhibitionFailed) {
		t.Errorf("error = %v; want wrapping ErrInhibitionFailed", err)
	}

	st := mgr.Status()
	if st.Active || st.Holders != 0 {
		t.Errorf("status active = %v, holders = %d; want false, 0", st.Active, st.Holders)
	}
	if st.Error == "" {
		t.Error("expected non-empty error in status after failure")
	}
}

func TestManager_ShutdownWithActiveLeases(t *testing.T) {
	mock := &mockBackend{supported: true}
	mgr := NewWithBackend(mock)

	l1, _ := mgr.Acquire("task 1")
	l2, _ := mgr.Acquire("task 2")

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	mock.mu.Lock()
	if mock.uninhibitCalls != 1 || mock.closeCalls != 1 {
		t.Errorf("uninhibit = %d, close = %d; want 1, 1", mock.uninhibitCalls, mock.closeCalls)
	}
	mock.mu.Unlock()

	// Subsequent acquire must fail with ErrClosed
	_, err := mgr.Acquire("task 3")
	if !errors.Is(err, ErrClosed) {
		t.Errorf("expected ErrClosed, got %v", err)
	}

	// Releasing old leases after close must not panic or cause extra uninhibit
	l1.Release()
	l2.Release()

	mock.mu.Lock()
	if mock.uninhibitCalls != 1 {
		t.Errorf("uninhibit calls after close release = %d; want 1", mock.uninhibitCalls)
	}
	mock.mu.Unlock()
}

func TestManager_UnsupportedPlatform(t *testing.T) {
	mock := &mockBackend{supported: false}
	mgr := NewWithBackend(mock)

	_, err := mgr.Acquire("task")
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("expected ErrUnsupportedPlatform, got %v", err)
	}

	st := mgr.Status()
	if st.Supported || st.Active {
		t.Errorf("status supported = %v, active = %v; want false, false", st.Supported, st.Active)
	}
}

func TestManager_RealNativeSmoke(t *testing.T) {
	// Native platform integration: requires real privileges (logind Inhibit
	// on Linux) polkit denies to non-local CI sessions. Opt-in only (see
	// native_optin_test.go).
	requireNativeTestsOptIn(t)

	mgr := New()
	defer mgr.Close()

	st := mgr.Status()
	if !st.Supported {
		t.Logf("Native power inhibition not supported in this environment: %s", st.Error)
		return
	}

	lease, err := mgr.Acquire("smoke test transfer")
	if err != nil {
		t.Fatalf("native acquire failed: %v", err)
	}
	if lease == nil {
		t.Fatal("expected non-nil lease")
	}

	st = mgr.Status()
	if !st.Active || st.Holders != 1 {
		t.Errorf("status after acquire: %+v", st)
	}

	lease.Release()

	st = mgr.Status()
	if st.Active || st.Holders != 0 {
		t.Errorf("status after release: %+v", st)
	}
}
