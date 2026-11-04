package transfer

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

type zeroReader struct {
	remaining int64
}

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining <= 0 {
		return 0, io.EOF
	}
	toRead := int64(len(p))
	if toRead > z.remaining {
		toRead = z.remaining
	}
	z.remaining -= toRead
	return int(toRead), nil
}

func BenchmarkIngestStream(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "bench-ingest-*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewStore(StoreConfig{Dir: tempDir})
	if err != nil {
		b.Fatal(err)
	}
	rm := NewResumableManager(store, nil)
	defer rm.Close()

	const payloadSize = 256 * 1024 * 1024 // 256 MB per benchmark run

	b.SetBytes(payloadSize)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		transferID := filepath.Join(tempDir, "bench_up")
		_ = os.Remove(transferID)
		meta, err := rm.CreateUpload("sess-1", "up-bench", "batch-1", "item-1", "test.bin", "", payloadSize, "")
		if err != nil {
			b.Fatal(err)
		}
		zr := &zeroReader{remaining: payloadSize}
		b.StartTimer()

		_, _, err = rm.IngestStream(meta.TransferID, 0, zr, nil)
		if err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		_ = os.Remove(rm.partPath(meta.TransferID))
		_ = os.Remove(rm.metaPath(meta.TransferID))
		rm.mu.Lock()
		delete(rm.uploads, meta.TransferID)
		rm.mu.Unlock()
	}
}
