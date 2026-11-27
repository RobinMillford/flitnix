package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

type mockBackend struct {
	supported   bool
	started     bool
	stopped     bool
	lastState   State
	lastMenu    []MenuItem
	triggerFunc func(string)
}

func (m *mockBackend) Start(initial State, initialMenu []MenuItem, onAction func(string)) bool {
	if !m.supported {
		return false
	}
	m.started = true
	m.lastState = initial
	m.lastMenu = initialMenu
	m.triggerFunc = onAction
	return true
}

func (m *mockBackend) Update(state State, menu []MenuItem) {
	m.lastState = state
	m.lastMenu = menu
}

func (m *mockBackend) Stop() {
	m.stopped = true
}

func TestServiceMinimumAndExtendedLifecycle(t *testing.T) {
	b := &mockBackend{supported: true}
	var opened, hidden, quit bool
	var visSet string
	var quietToggled bool

	svc := newServiceWithBackend(b, Callbacks{
		OnOpen: func() { opened = true },
		OnHide: func() { hidden = true },
		OnQuit: func() { quit = true },
	})

	if !svc.Start(State{WindowVisible: true, StatusLabel: "Flitnix Ready"}) {
		t.Fatal("expected Start=true")
	}
	if !svc.Supported() {
		t.Fatal("expected Supported=true")
	}

	// Verify minimum menu does not include visibility submenu yet
	for _, it := range b.lastMenu {
		if it.ID == ActionVisMenu {
			t.Fatal("did not expect visibility submenu before callback wired")
		}
	}

	b.triggerFunc(ActionOpen)
	b.triggerFunc(ActionHide)
	if !opened || !hidden {
		t.Fatalf("expected open and hide callbacks, got opened=%v hidden=%v", opened, hidden)
	}

	// Wire extended callbacks (Part 2 behavior)
	svc.UpdateCallbacks(Callbacks{
		OnOpen:            func() { opened = true },
		OnHide:            func() { hidden = true },
		OnSetVisibility:   func(mode string) { visSet = mode },
		OnToggleQuietMode: func() { quietToggled = true },
		OnQuit:            func() { quit = true },
	})
	svc.Update(State{WindowVisible: false, VisibilityMode: "trusted", QuietMode: true})

	var sawVis, sawQuiet bool
	for _, it := range b.lastMenu {
		if it.ID == ActionVisMenu && len(it.Children) == 4 {
			sawVis = true
		}
		if it.ID == ActionToggleQuiet && it.Checked {
			sawQuiet = true
		}
	}
	if !sawVis || !sawQuiet {
		t.Fatalf("expected visibility submenu and checked DND item, got vis=%v quiet=%v", sawVis, sawQuiet)
	}

	b.triggerFunc(ActionVisEveryone)
	b.triggerFunc(ActionToggleQuiet)
	b.triggerFunc(ActionQuit)
	if visSet != "everyone" || !quietToggled || !quit {
		t.Fatalf("expected callbacks fired, got vis=%q quiet=%v quit=%v", visSet, quietToggled, quit)
	}

	// Idempotent stop
	svc.Stop()
	svc.Stop()
	if !b.stopped || svc.Supported() {
		t.Fatal("expected backend stopped cleanly")
	}
}

func TestServiceGracefulFallbackWhenUnsupported(t *testing.T) {
	b := &mockBackend{supported: false}
	svc := newServiceWithBackend(b, Callbacks{})
	if svc.Start(State{}) {
		t.Fatal("expected Start=false for unsupported backend")
	}
	if svc.Supported() {
		t.Fatal("expected Supported=false")
	}
	// Updates and Stop must be safe no-ops
	svc.Update(State{StatusLabel: "Ignore"})
	svc.Stop()
	if b.stopped {
		t.Fatal("Stop should not call backend Stop when Start failed")
	}
}

func TestDecodePNGToARGB(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	img.Set(0, 0, color.NRGBA{R: 255, G: 0, B: 128, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	w, h, data := DecodePNGToARGB(buf.Bytes(), 32)
	if w != 32 || h != 32 || len(data) != 32*32*4 {
		t.Fatalf("unexpected pixmap size %dx%d len=%d", w, h, len(data))
	}
	// Pixel (0,0) in big-endian ARGB: A=255, R=255, G=0, B=128
	if data[0] != 255 || data[1] != 255 || data[2] != 0 || data[3] != 128 {
		t.Fatalf("unexpected ARGB bytes: %v", data[:4])
	}
}
