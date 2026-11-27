//go:build darwin && cgo

package tray

/*
#include <stdint.h>

// cgo copies the preamble of a file containing //export into two generated C
// translation units, so it must contain declarations only. The Objective-C
// class and tray helpers (which have external linkage) live in
// tray_darwin_objc_cgo.go so they are emitted exactly once.
//
// cgo generates this declaration from the //export below using char *,
// so the manual declaration must match it exactly.
extern void flitnixDarwinTrayAction(uintptr_t handle, char *actionID);
*/
import "C"
import (
	"runtime/cgo"
	"sync"
	"unsafe"
)

type darwinBackend struct {
	mu       sync.Mutex
	iconPNG  []byte
	handle   cgo.Handle
	ptr      unsafe.Pointer
	onAction func(string)
}

func newPlatformBackend(iconPNG []byte) backend {
	return &darwinBackend{iconPNG: iconPNG}
}

//export flitnixDarwinTrayAction
func flitnixDarwinTrayAction(h uintptr, action *C.char) {
	handle := cgo.Handle(h)
	b, ok := handle.Value().(*darwinBackend)
	if !ok || b == nil {
		return
	}
	act := C.GoString(action)
	b.mu.Lock()
	cb := b.onAction
	b.mu.Unlock()
	if cb != nil && act != "" {
		go cb(act)
	}
}
