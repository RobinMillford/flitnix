//go:build windows

package netenv

import (
	"context"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winMu        sync.Mutex
	winListeners = make(map[uintptr]chan<- struct{})
	winIDCounter uintptr
	winCbOnce    sync.Once
	winCbPtr     uintptr
)

func winNotificationCallback(callerCtx uintptr, row uintptr, notificationType uintptr) uintptr {
	winMu.Lock()
	ch := winListeners[callerCtx]
	winMu.Unlock()

	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return 0
}

func defaultNativeNotifier(ctx context.Context, notify chan<- struct{}) {
	mod := windows.NewLazySystemDLL("iphlpapi.dll")
	procIfChange := mod.NewProc("NotifyIpInterfaceChange")
	procAddrChange := mod.NewProc("NotifyUnicastIpAddressChange")
	procCancelNotify := mod.NewProc("CancelMibChangeNotify2")

	// Attempt modern Windows Vista+ IPv4/IPv6 notifications
	if procIfChange.Find() == nil && procAddrChange.Find() == nil && procCancelNotify.Find() == nil {
		if runModernNotifier(ctx, notify, procIfChange, procAddrChange, procCancelNotify) {
			return
		}
	}

	// Fallback to legacy NotifyAddrChange if modern APIs failed or are unavailable
	runLegacyNotifier(ctx, notify, mod)
}

func runModernNotifier(
	ctx context.Context,
	notify chan<- struct{},
	procIfChange *windows.LazyProc,
	procAddrChange *windows.LazyProc,
	procCancelNotify *windows.LazyProc,
) bool {
	winCbOnce.Do(func() {
		winCbPtr = windows.NewCallback(winNotificationCallback)
	})
	if winCbPtr == 0 {
		return false
	}

	id := atomic.AddUintptr(&winIDCounter, 1)

	winMu.Lock()
	winListeners[id] = notify
	winMu.Unlock()

	defer func() {
		winMu.Lock()
		delete(winListeners, id)
		winMu.Unlock()
	}()

	var hIf windows.Handle
	var hAddr windows.Handle

	// AF_UNSPEC (0) captures both IPv4 and IPv6 changes.
	// InitialNotification = 0 (false) since monitor runs an initial state evaluation on Start.
	r1, _, _ := procIfChange.Call(
		0,
		winCbPtr,
		id,
		0,
		uintptr(unsafe.Pointer(&hIf)),
	)
	if r1 != 0 {
		return false
	}
	defer func() {
		if hIf != 0 {
			_, _, _ = procCancelNotify.Call(uintptr(hIf))
		}
	}()

	r2, _, _ := procAddrChange.Call(
		0,
		winCbPtr,
		id,
		0,
		uintptr(unsafe.Pointer(&hAddr)),
	)
	if r2 != 0 {
		return false
	}
	defer func() {
		if hAddr != 0 {
			_, _, _ = procCancelNotify.Call(uintptr(hAddr))
		}
	}()

	// Wait for context cancellation. Deregistrations execute in defer outside of callback.
	<-ctx.Done()
	return true
}

func runLegacyNotifier(ctx context.Context, notify chan<- struct{}, mod *windows.LazyDLL) {
	proc := mod.NewProc("NotifyAddrChange")
	if proc.Find() != nil {
		return
	}

	for {
		if ctx.Err() != nil {
			return
		}

		event, err := windows.CreateEvent(nil, 0, 0, nil)
		if err != nil {
			return
		}

		var handle windows.Handle
		var ov windows.Overlapped
		ov.HEvent = event

		r1, _, _ := proc.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&ov)))
		// 0 is NO_ERROR, 997 is ERROR_IO_PENDING
		if r1 != 0 && r1 != 997 {
			windows.CloseHandle(event)
			return
		}

		signaled := false
		for !signaled {
			if ctx.Err() != nil {
				windows.CloseHandle(event)
				return
			}
			res, _ := windows.WaitForSingleObject(event, 500)
			if res == windows.WAIT_OBJECT_0 {
				signaled = true
			}
		}
		windows.CloseHandle(event)

		select {
		case <-ctx.Done():
			return
		case notify <- struct{}{}:
		default:
		}
	}
}
