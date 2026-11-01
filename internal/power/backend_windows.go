//go:build windows

package power

import (
	"fmt"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

const (
	esSystemRequired = 0x00000001
	esContinuous     = 0x80000000
)

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

type winOp int

const (
	opInhibit winOp = iota
	opUninhibit
	opClose
)

type winReq struct {
	op   winOp
	resp chan error
}

type windowsBackend struct {
	reqChan   chan winReq
	closeOnce sync.Once
	initErr   error
}

func newNativeBackend() (backend, error) {
	return newWindowsBackend()
}

func newWindowsBackend() (*windowsBackend, error) {
	if err := kernel32.Load(); err != nil {
		return &windowsBackend{initErr: fmt.Errorf("kernel32.dll unavailable: %w", err)}, nil
	}
	if err := procSetThreadExecutionState.Find(); err != nil {
		return &windowsBackend{initErr: fmt.Errorf("SetThreadExecutionState procedure not found: %w", err)}, nil
	}

	b := &windowsBackend{
		reqChan: make(chan winReq),
	}

	ready := make(chan struct{})
	go b.worker(ready)
	<-ready

	return b, nil
}

func (b *windowsBackend) worker(ready chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	close(ready)

	for req := range b.reqChan {
		switch req.op {
		case opInhibit:
			r1, _, err := procSetThreadExecutionState.Call(uintptr(esContinuous | esSystemRequired))
			if r1 == 0 {
				req.resp <- fmt.Errorf("SetThreadExecutionState failed: %w", err)
			} else {
				req.resp <- nil
			}
		case opUninhibit:
			r1, _, err := procSetThreadExecutionState.Call(uintptr(esContinuous))
			if r1 == 0 {
				req.resp <- fmt.Errorf("SetThreadExecutionState restore failed: %w", err)
			} else {
				req.resp <- nil
			}
		case opClose:
			procSetThreadExecutionState.Call(uintptr(esContinuous))
			req.resp <- nil
			return
		}
	}
}

func (b *windowsBackend) Supported() bool {
	return b.initErr == nil && b.reqChan != nil
}

func (b *windowsBackend) Inhibit(reason string) error {
	if !b.Supported() {
		if b.initErr != nil {
			return b.initErr
		}
		return ErrUnsupportedPlatform
	}
	resp := make(chan error, 1)
	b.reqChan <- winReq{op: opInhibit, resp: resp}
	return <-resp
}

func (b *windowsBackend) Uninhibit() error {
	if !b.Supported() {
		return nil
	}
	resp := make(chan error, 1)
	b.reqChan <- winReq{op: opUninhibit, resp: resp}
	return <-resp
}

func (b *windowsBackend) Close() error {
	var err error
	b.closeOnce.Do(func() {
		if b.Supported() {
			resp := make(chan error, 1)
			b.reqChan <- winReq{op: opClose, resp: resp}
			err = <-resp
		}
	})
	return err
}
