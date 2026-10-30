package transfer

import "sync"

// CopyBufferSize is the standard buffer size for sequential file streaming.
// 256 KiB balances high throughput with bounded per-stream memory footprint.
const CopyBufferSize = 256 * 1024

var copyBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, CopyBufferSize)
		return &b
	},
}

// GetCopyBuffer acquires a 256 KiB buffer from the pool.
func GetCopyBuffer() *[]byte {
	return copyBufferPool.Get().(*[]byte)
}

// PutCopyBuffer returns a 256 KiB buffer to the pool if it has the standard capacity.
// It resets the slice length to CopyBufferSize before recycling.
func PutCopyBuffer(b *[]byte) {
	if b != nil && cap(*b) == CopyBufferSize {
		*b = (*b)[:CopyBufferSize]
		copyBufferPool.Put(b)
	}
}
