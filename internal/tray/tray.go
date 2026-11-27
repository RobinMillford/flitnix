// Package tray provides a cross-platform system tray / menu bar status icon
// and menu service for Flitnix, with safe fallback in headless environments.
package tray

import (
	"bytes"
	"image"
	_ "image/png"
	"strings"
	"sync"
)

// Menu action identifiers shared across platform backends.
const (
	ActionStatus         = "status"
	ActionToggleWindow   = "toggle_window"
	ActionOpen           = "open"
	ActionHide           = "hide"
	ActionVisMenu        = "visibility_menu"
	ActionVisHidden      = "vis_hidden"
	ActionVisTrusted     = "vis_trusted"
	ActionVisTempEveryone = "vis_temporary_everyone"
	ActionVisEveryone    = "vis_everyone"
	ActionToggleQuiet    = "toggle_quiet"
	ActionOpenSettings   = "open_settings"
	ActionQuit           = "quit"
)

// MenuItem describes a platform-agnostic tray menu entry.
type MenuItem struct {
	ID          string
	Label       string
	Enabled     bool
	Visible     bool
	IsSeparator bool
	Checkable   bool
	Radio       bool
	Checked     bool
	Children    []MenuItem
}

// State captures the desktop & transfer state rendered by the tray icon and menu.
type State struct {
	Tooltip          string
	StatusLabel      string
	Attention        bool
	WindowVisible    bool
	VisibilityMode   string
	QuietMode        bool
	HasActiveSession bool
	PendingCount     int
}

// Callbacks receives tray menu and icon interactions.
type Callbacks struct {
	OnOpen            func()
	OnHide            func()
	OnToggleWindow    func()
	OnSetVisibility   func(mode string)
	OnToggleQuietMode func()
	OnOpenSettings    func()
	OnQuit            func()
}

type backend interface {
	Start(initial State, initialMenu []MenuItem, onAction func(id string)) bool
	Update(state State, menu []MenuItem)
	Stop()
}

// Service manages the lifecycle and state synchronization of the tray icon.
type Service struct {
	mu        sync.Mutex
	backend   backend
	callbacks Callbacks
	state     State
	supported bool
	started   bool
	stopped   bool
	iconPNG   []byte
}

// NewService creates a tray Service using the platform's native tray backend.
func NewService(iconPNG []byte, cb Callbacks) *Service {
	cp := append([]byte(nil), iconPNG...)
	return &Service{
		backend:   newPlatformBackend(cp),
		callbacks: cb,
		iconPNG:   cp,
	}
}

func newServiceWithBackend(b backend, cb Callbacks) *Service {
	return &Service{
		backend:   b,
		callbacks: cb,
	}
}

// Start initializes the tray icon and menu. It returns false (never errors) if
// no tray environment is available so headless/CI sessions keep running.
func (s *Service) Start(initial State) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return false
	}
	if s.started {
		sup := s.supported
		s.mu.Unlock()
		return sup
	}
	normalized := normalizeState(initial)
	s.state = normalized
	menu := BuildMenu(normalized, s.callbacks)
	b := s.backend
	s.mu.Unlock()

	ok := b != nil && b.Start(normalized, menu, s.handleAction)

	s.mu.Lock()
	s.started = true
	s.supported = ok
	s.mu.Unlock()
	return ok
}

// Supported reports whether the tray backend initialized and is active.
func (s *Service) Supported() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.supported && !s.stopped
}

// Update refreshes the tray tooltip, icon status, and menu items.
func (s *Service) Update(state State) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped || !s.started || !s.supported || s.backend == nil {
		s.mu.Unlock()
		return
	}
	normalized := normalizeState(state)
	s.state = normalized
	menu := BuildMenu(normalized, s.callbacks)
	b := s.backend
	s.mu.Unlock()

	b.Update(normalized, menu)
}

// UpdateCallbacks atomically replaces tray callbacks and refreshes menu structure
// if extended capabilities were wired after minimum initialization.
func (s *Service) UpdateCallbacks(cb Callbacks) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.callbacks = cb
	active := s.started && s.supported && !s.stopped && s.backend != nil
	st := s.state
	var menu []MenuItem
	if active {
		menu = BuildMenu(st, cb)
	}
	b := s.backend
	s.mu.Unlock()

	if active {
		b.Update(st, menu)
	}
}

// Stop removes the tray icon cleanly. Safe to call multiple times.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	wasStarted := s.started && s.supported
	s.supported = false
	b := s.backend
	s.mu.Unlock()

	if wasStarted && b != nil {
		b.Stop()
	}
}

func (s *Service) handleAction(actionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	cb := s.callbacks
	s.mu.Unlock()

	switch actionID {
	case ActionOpen:
		if cb.OnOpen != nil {
			cb.OnOpen()
		}
	case ActionHide:
		if cb.OnHide != nil {
			cb.OnHide()
		}
	case ActionToggleWindow:
		if cb.OnToggleWindow != nil {
			cb.OnToggleWindow()
		} else if cb.OnOpen != nil {
			cb.OnOpen()
		}
	case ActionVisHidden:
		if cb.OnSetVisibility != nil {
			cb.OnSetVisibility("hidden")
		}
	case ActionVisTrusted:
		if cb.OnSetVisibility != nil {
			cb.OnSetVisibility("trusted")
		}
	case ActionVisTempEveryone:
		if cb.OnSetVisibility != nil {
			cb.OnSetVisibility("temporary_everyone")
		}
	case ActionVisEveryone:
		if cb.OnSetVisibility != nil {
			cb.OnSetVisibility("everyone")
		}
	case ActionToggleQuiet:
		if cb.OnToggleQuietMode != nil {
			cb.OnToggleQuietMode()
		}
	case ActionOpenSettings:
		if cb.OnOpenSettings != nil {
			cb.OnOpenSettings()
		}
	case ActionQuit:
		if cb.OnQuit != nil {
			cb.OnQuit()
		}
	}
}

func normalizeState(s State) State {
	s.Tooltip = strings.TrimSpace(s.Tooltip)
	if s.Tooltip == "" {
		s.Tooltip = "Flitnix — Always-Ready Local Sharing"
	}
	s.StatusLabel = strings.TrimSpace(s.StatusLabel)
	if s.StatusLabel == "" {
		s.StatusLabel = "Flitnix Ready"
	}
	return s
}

// BuildMenu constructs a deterministic tray menu for the current state and callbacks.
func BuildMenu(state State, cb Callbacks) []MenuItem {
	items := []MenuItem{
		{
			ID:      ActionStatus,
			Label:   state.StatusLabel,
			Enabled: false,
			Visible: true,
		},
		{IsSeparator: true, Visible: true},
	}

	if state.WindowVisible {
		items = append(items, MenuItem{
			ID:      ActionOpen,
			Label:   "Bring Flitnix to Front",
			Enabled: true,
			Visible: true,
		}, MenuItem{
			ID:      ActionHide,
			Label:   "Hide Window to Tray",
			Enabled: true,
			Visible: true,
		})
	} else {
		items = append(items, MenuItem{
			ID:      ActionOpen,
			Label:   "Open Flitnix",
			Enabled: true,
			Visible: true,
		})
	}

	if cb.OnSetVisibility != nil {
		items = append(items,
			MenuItem{IsSeparator: true, Visible: true},
			MenuItem{
				ID:      ActionVisMenu,
				Label:   "Visibility Mode",
				Enabled: true,
				Visible: true,
				Children: []MenuItem{
					{
						ID:        ActionVisHidden,
						Label:     "Hidden (Offline)",
						Enabled:   true,
						Visible:   true,
						Checkable: true,
						Radio:     true,
						Checked:   state.VisibilityMode == "hidden",
					},
					{
						ID:        ActionVisTrusted,
						Label:     "Trusted Devices Only",
						Enabled:   true,
						Visible:   true,
						Checkable: true,
						Radio:     true,
						Checked:   state.VisibilityMode == "trusted" || state.VisibilityMode == "",
					},
					{
						ID:        ActionVisTempEveryone,
						Label:     "Open to Everyone (10 min)",
						Enabled:   true,
						Visible:   true,
						Checkable: true,
						Radio:     true,
						Checked:   state.VisibilityMode == "temporary_everyone",
					},
					{
						ID:        ActionVisEveryone,
						Label:     "Everyone on LAN",
						Enabled:   true,
						Visible:   true,
						Checkable: true,
						Radio:     true,
						Checked:   state.VisibilityMode == "everyone",
					},
				},
			},
		)
	}

	if cb.OnToggleQuietMode != nil || cb.OnOpenSettings != nil {
		items = append(items, MenuItem{IsSeparator: true, Visible: true})
		if cb.OnToggleQuietMode != nil {
			items = append(items, MenuItem{
				ID:        ActionToggleQuiet,
				Label:     "Pause Notifications (Do Not Disturb)",
				Enabled:   true,
				Visible:   true,
				Checkable: true,
				Checked:   state.QuietMode,
			})
		}
		if cb.OnOpenSettings != nil {
			items = append(items, MenuItem{
				ID:      ActionOpenSettings,
				Label:   "Settings…",
				Enabled: true,
				Visible: true,
			})
		}
	}

	items = append(items,
		MenuItem{IsSeparator: true, Visible: true},
		MenuItem{
			ID:      ActionQuit,
			Label:   "Quit Flitnix",
			Enabled: true,
			Visible: true,
		},
	)
	return items
}

// DecodePNGToARGB converts raw PNG bytes into a big-endian ARGB32 pixmap sized
// up to maxDim×maxDim, matching the freedesktop StatusNotifierItem IconPixmap format.
func DecodePNGToARGB(pngBytes []byte, maxDim int) (int32, int32, []byte) {
	if len(pngBytes) == 0 {
		return 0, 0, nil
	}
	img, _, err := image.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return 0, 0, nil
	}
	b := img.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return 0, 0, nil
	}
	dstW, dstH := srcW, srcH
	if maxDim > 0 && (srcW > maxDim || srcH > maxDim) {
		if srcW >= srcH {
			dstW = maxDim
			dstH = max(1, (srcH*maxDim)/srcW)
		} else {
			dstH = maxDim
			dstW = max(1, (srcW*maxDim)/srcH)
		}
	}
	out := make([]byte, dstW*dstH*4)
	for y := 0; y < dstH; y++ {
		sy := b.Min.Y + (y*srcH)/dstH
		for x := 0; x < dstW; x++ {
			sx := b.Min.X + (x*srcW)/dstW
			r16, g16, b16, a16 := img.At(sx, sy).RGBA()
			a := uint8(a16 >> 8)
			var r, g, bl uint8
			if a16 > 0 {
				// Convert alpha-premultiplied RGBA to straight ARGB bytes.
				r = uint8(min(255, (r16*255)/a16))
				g = uint8(min(255, (g16*255)/a16))
				bl = uint8(min(255, (b16*255)/a16))
			}
			off := (y*dstW + x) * 4
			out[off] = a
			out[off+1] = r
			out[off+2] = g
			out[off+3] = bl
		}
	}
	return int32(dstW), int32(dstH), out
}
