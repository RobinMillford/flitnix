package transfer

import (
	"sync"
	"testing"
)

func TestCopyBufferPoolAcquisitionAndRecycle(t *testing.T) {
	bufPtr := GetCopyBuffer()
	if bufPtr == nil || len(*bufPtr) != CopyBufferSize {
		t.Fatalf("unexpected buffer: %v", bufPtr)
	}
	buf := *bufPtr
	buf[0] = 0xAA
	buf[len(buf)-1] = 0xBB

	PutCopyBuffer(bufPtr)

	// Acquire again
	bufPtr2 := GetCopyBuffer()
	if bufPtr2 == nil || len(*bufPtr2) != CopyBufferSize {
		t.Fatalf("unexpected recycled buffer: %v", bufPtr2)
	}
	PutCopyBuffer(bufPtr2)

	// Test resliced buffer recovery
	reslicedPtr := GetCopyBuffer()
	*reslicedPtr = (*reslicedPtr)[:100] // truncate slice length
	PutCopyBuffer(reslicedPtr)
	recoveredPtr := GetCopyBuffer()
	if len(*recoveredPtr) != CopyBufferSize {
		t.Fatalf("expected recovered buffer len %d, got %d", CopyBufferSize, len(*recoveredPtr))
	}
	PutCopyBuffer(recoveredPtr)

	// Reject mismatched buffer
	wrongBuf := make([]byte, 1024)
	PutCopyBuffer(&wrongBuf) // should be safely ignored
}

func TestCopyBufferPoolConcurrentSafety(t *testing.T) {
	const goroutines = 32
	const iterations = 100
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				bPtr := GetCopyBuffer()
				b := *bPtr
				b[0] = byte(id)
				b[len(b)-1] = byte(j)
				PutCopyBuffer(bPtr)
			}
		}(i)
	}
	wg.Wait()
}
