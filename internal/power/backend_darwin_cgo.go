//go:build darwin && cgo

package power

/*
#cgo darwin LDFLAGS: -framework IOKit -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/pwr_mgt/IOPMLib.h>

static int flitnix_inhibit_sleep(const char *reason, IOPMAssertionID *assertion_id) {
	CFStringRef type = CFSTR("PreventUserIdleSystemSleep");
	CFStringRef name = CFStringCreateWithCString(kCFAllocatorDefault, reason, kCFStringEncodingUTF8);
	if (name == NULL) {
		return -1;
	}
	IOReturn ret = IOPMAssertionCreateWithName(type, kIOPMAssertionLevelOn, name, assertion_id);
	CFRelease(name);
	return (ret == kIOReturnSuccess) ? 0 : (int)ret;
}

static int flitnix_release_sleep(IOPMAssertionID assertion_id) {
	IOReturn ret = IOPMAssertionRelease(assertion_id);
	return (ret == kIOReturnSuccess) ? 0 : (int)ret;
}
*/
import "C"
import (
	"fmt"
	"sync"
	"unsafe"
)

type darwinBackend struct {
	mu          sync.Mutex
	assertionID C.IOPMAssertionID
	held        bool
}

func newNativeBackend() (backend, error) {
	return newDarwinBackend()
}

func newDarwinBackend() (*darwinBackend, error) {
	return &darwinBackend{}, nil
}

func (b *darwinBackend) Supported() bool {
	return true
}

func (b *darwinBackend) Inhibit(reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.held {
		return nil
	}

	cReason := C.CString(reason)
	defer C.free(unsafe.Pointer(cReason))

	var id C.IOPMAssertionID
	ret := C.flitnix_inhibit_sleep(cReason, &id)
	if ret != 0 {
		return fmt.Errorf("IOPMAssertionCreateWithName failed with code %d", int(ret))
	}

	b.assertionID = id
	b.held = true
	return nil
}

func (b *darwinBackend) Uninhibit() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.held {
		return nil
	}

	ret := C.flitnix_release_sleep(b.assertionID)
	b.held = false
	b.assertionID = 0

	if ret != 0 {
		return fmt.Errorf("IOPMAssertionRelease failed with code %d", int(ret))
	}
	return nil
}

func (b *darwinBackend) Close() error {
	return b.Uninhibit()
}
