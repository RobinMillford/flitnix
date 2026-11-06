package transfer

import (
	"io"
	"os"
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

func BenchmarkSimplifiedStreamingUpload(b *testing.B) {
	tempDir, err := os.MkdirTemp("", "bench-stream-*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	store, err := NewStore(StoreConfig{Dir: tempDir})
	if err != nil {
		b.Fatal(err)
	}

	const payloadSize = 256 * 1024 * 1024 // 256 MB per benchmark run

	b.SetBytes(payloadSize)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sink, err := newPartSink(store, "test.bin")
		if err != nil {
			b.Fatal(err)
		}
		zr := &zeroReader{remaining: payloadSize}
		b.StartTimer()

		_, err = streamBody(zr, sink, payloadSize, nil)
		if err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		_ = sink.Abort()
	}
}
