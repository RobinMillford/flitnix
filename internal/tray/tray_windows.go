//go:build windows

package tray

import (
	"runtime"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmUser          = 0x0400
	wmTrayIcon      = wmUser + 101
	wmTrayQuit      = wmUser + 102
	wmTrayUpdate    = wmUser + 103
	wmCommand       = 0x0111
	wmDestroy       = 0x0002
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmContextMenu   = 0x007B

	nimAdd        = 0x00000000
	nimModify     = 0x00000001
	nimDelete     = 0x00000002
	nimSetVersion = 0x00000004
	notifyVersion = 4

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	mfString    = 0x00000000
	mfGrayed    = 0x00000001
	mfDisabled  = 0x00000002
	mfChecked   = 0x00000008
	mfPopup     = 0x00000010
	mfSeparator = 0x00000800

	tpmRightAlign  = 0x0008
	tpmBottomAlign = 0x0020
	tpmNonotify    = 0x0080
	tpmReturnCmd   = 0x0100

	idiApplication = 32512
	hwndMessage    = ^windows.Handle(2) // HWND_MESSAGE (-3)
)

var (
	user32                = windows.NewLazySystemDLL("user32.dll")
	shell32               = windows.NewLazySystemDLL("shell32.dll")
	kernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGetModuleHandleW  = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostMessageW      = user32.NewProc("PostMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procLoadIconW         = user32.NewProc("LoadIconW")
	procCreateIconFromRes = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon       = user32.NewProc("DestroyIcon")
	procCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	procAppendMenuW       = user32.NewProc("AppendMenuW")
	procDestroyMenu       = user32.NewProc("DestroyMenu")
	procTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	procSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	procGetCursorPos      = user32.NewProc("GetCursorPos")
	procShellNotifyIconW  = shell32.NewProc("Shell_NotifyIconW")
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type point struct {
	x int32
	y int32
}

type msg struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             windows.Handle
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            windows.Handle
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         guid
	hBalloonIcon     windows.Handle
}

var (
	winBackendsMu sync.Mutex
	winBackends   = map[windows.Handle]*windowsBackend{}
)

type windowsBackend struct {
	mu         sync.Mutex
	iconPNG    []byte
	hwnd       windows.Handle
	hIcon      windows.Handle
	ownsIcon   bool
	state      State
	menu       []MenuItem
	actionByID map[uint16]string
	onAction   func(string)
	done       chan struct{}
}

func newPlatformBackend(iconPNG []byte) backend {
	return &windowsBackend{
		iconPNG:    iconPNG,
		actionByID: make(map[uint16]string),
	}
}

func (b *windowsBackend) Start(initial State, initialMenu []MenuItem, onAction func(id string)) bool {
	b.mu.Lock()
	b.state = initial
	b.menu = initialMenu
	b.onAction = onAction
	b.done = make(chan struct{})
	b.mu.Unlock()

	ready := make(chan bool, 1)
	go b.runLoop(ready)
	return <-ready
}

func (b *windowsBackend) runLoop(ready chan<- bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(b.done)

	hInst, _, _ := procGetModuleHandleW.Call(0)
	clsName, _ := windows.UTF16PtrFromString("FlitnixTrayHiddenWindow")

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(winTrayWndProc),
		hInstance:     windows.Handle(hInst),
		lpszClassName: clsName,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwndRaw, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(clsName)),
		uintptr(unsafe.Pointer(clsName)),
		0,
		0, 0, 0, 0,
		uintptr(hwndMessage),
		0,
		hInst,
		0,
	)
	if hwndRaw == 0 {
		ready <- false
		return
	}
	hwnd := windows.Handle(hwndRaw)

	hIcon, owns := b.loadWinIcon()
	b.mu.Lock()
	b.hwnd = hwnd
	b.hIcon = hIcon
	b.ownsIcon = owns
	state := b.state
	b.mu.Unlock()

	winBackendsMu.Lock()
	winBackends[hwnd] = b
	winBackendsMu.Unlock()

	nid := b.makeNotifyIconData(hwnd, hIcon, state.Tooltip)
	ret, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if ret == 0 {
		winBackendsMu.Lock()
		delete(winBackends, hwnd)
		winBackendsMu.Unlock()
		procDestroyWindow.Call(uintptr(hwnd))
		if owns && hIcon != 0 {
			procDestroyIcon.Call(uintptr(hIcon))
		}
		ready <- false
		return
	}
	nid.uVersion = notifyVersion
	procShellNotifyIconW.Call(nimSetVersion, uintptr(unsafe.Pointer(&nid)))

	ready <- true

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	winBackendsMu.Lock()
	delete(winBackends, hwnd)
	winBackendsMu.Unlock()

	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	if owns && hIcon != 0 {
		procDestroyIcon.Call(uintptr(hIcon))
	}
}

func (b *windowsBackend) loadWinIcon() (windows.Handle, bool) {
	if len(b.iconPNG) > 0 {
		h, _, _ := procCreateIconFromRes.Call(
			uintptr(unsafe.Pointer(&b.iconPNG[0])),
			uintptr(len(b.iconPNG)),
			1,
			0x00030000,
			32,
			32,
			0,
		)
		if h != 0 {
			return windows.Handle(h), true
		}
	}
	h, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))
	return windows.Handle(h), false
}

func (b *windowsBackend) makeNotifyIconData(hwnd, hIcon windows.Handle, tooltip string) notifyIconDataW {
	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = wmTrayIcon
	nid.hIcon = hIcon
	tip16 := utf16.Encode([]rune(tooltip))
	n := min(len(tip16), len(nid.szTip)-1)
	copy(nid.szTip[:n], tip16[:n])
	return nid
}

func (b *windowsBackend) Update(state State, menu []MenuItem) {
	b.mu.Lock()
	b.state = state
	b.menu = menu
	hwnd := b.hwnd
	b.mu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(uintptr(hwnd), wmTrayUpdate, 0, 0)
	}
}

func (b *windowsBackend) Stop() {
	b.mu.Lock()
	hwnd := b.hwnd
	done := b.done
	b.hwnd = 0
	b.mu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(uintptr(hwnd), wmTrayQuit, 0, 0)
	}
	if done != nil {
		<-done
	}
}

func winTrayWndProc(hwnd windows.Handle, uMsg uint32, wParam, lParam uintptr) uintptr {
	winBackendsMu.Lock()
	b := winBackends[hwnd]
	winBackendsMu.Unlock()

	if b == nil {
		r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(uMsg), wParam, lParam)
		return r
	}

	switch uMsg {
	case wmTrayUpdate:
		b.mu.Lock()
		nid := b.makeNotifyIconData(hwnd, b.hIcon, b.state.Tooltip)
		b.mu.Unlock()
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
		return 0

	case wmTrayQuit:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0

	case wmTrayIcon:
		ev := uint32(lParam & 0xFFFF)
		switch ev {
		case wmLButtonUp, wmLButtonDblClk:
			b.trigger(ActionToggleWindow)
		case wmRButtonUp, wmContextMenu:
			b.showContextMenu(hwnd)
		}
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(uMsg), wParam, lParam)
	return r
}

func (b *windowsBackend) showContextMenu(hwnd windows.Handle) {
	b.mu.Lock()
	menuItems := b.menu
	onAction := b.onAction
	b.mu.Unlock()

	hMenuRaw, _, _ := procCreatePopupMenu.Call()
	if hMenuRaw == 0 {
		return
	}
	hMenu := windows.Handle(hMenuRaw)
	defer procDestroyMenu.Call(uintptr(hMenu))

	actionMap := make(map[uint16]string)
	nextCmd := uint16(100)
	var submenus []windows.Handle

	var appendItems func(parent windows.Handle, items []MenuItem)
	appendItems = func(parent windows.Handle, items []MenuItem) {
		for _, item := range items {
			if !item.Visible {
				continue
			}
			if item.IsSeparator {
				procAppendMenuW.Call(uintptr(parent), mfSeparator, 0, 0)
				continue
			}
			labelPtr, _ := windows.UTF16PtrFromString(item.Label)
			if len(item.Children) > 0 {
				subRaw, _, _ := procCreatePopupMenu.Call()
				if subRaw != 0 {
					sub := windows.Handle(subRaw)
					submenus = append(submenus, sub)
					appendItems(sub, item.Children)
					flags := uintptr(mfPopup)
					if !item.Enabled {
						flags |= mfGrayed | mfDisabled
					}
					procAppendMenuW.Call(uintptr(parent), flags, uintptr(sub), uintptr(unsafe.Pointer(labelPtr)))
				}
				continue
			}
			cmdID := nextCmd
			nextCmd++
			actionMap[cmdID] = item.ID
			flags := uintptr(mfString)
			if !item.Enabled {
				flags |= mfGrayed | mfDisabled
			}
			if item.Checked {
				flags |= mfChecked
			}
			procAppendMenuW.Call(uintptr(parent), flags, uintptr(cmdID), uintptr(unsafe.Pointer(labelPtr)))
		}
	}
	appendItems(hMenu, menuItems)
	defer func() {
		for _, sub := range submenus {
			procDestroyMenu.Call(uintptr(sub))
		}
	}()

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWin.Call(uintptr(hwnd))
	chosen, _, _ := procTrackPopupMenu.Call(
		uintptr(hMenu),
		tpmRightAlign|tpmBottomAlign|tpmReturnCmd|tpmNonotify,
		uintptr(pt.x),
		uintptr(pt.y),
		0,
		uintptr(hwnd),
		0,
	)
	if chosen != 0 && onAction != nil {
		if act, ok := actionMap[uint16(chosen)]; ok && act != "" {
			go onAction(act)
		}
	}
}

func (b *windowsBackend) trigger(action string) {
	b.mu.Lock()
	cb := b.onAction
	b.mu.Unlock()
	if cb != nil {
		go cb(action)
	}
}
