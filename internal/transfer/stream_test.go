package transfer

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"sort"
	"testing"
)

// errDiskFull is a synthetic write failure used by the fake sink below, so
// disk-full/truncation handling is testable without a real full disk.
var errDiskFull = errors.New("fake: simulated write failure")

// errSink is a contentSink that fails writes deterministically. failAfter
// < 0 means never failing; otherwise the (failAfter+1)th byte triggers the
// synthetic failure. Both Commit and Abort record that they ran.
type errSink struct {
	failAfter  int64
	written    int64
	committed  bool
	aborted    bool
	commitPath string
}

func (s *errSink) Write(p []byte) (int, error) {
	if s.failAfter >= 0 && s.written+int64(len(p)) > s.failAfter {
		room := s.failAfter - s.written
		if room < 0 {
			return 0, errDiskFull
		}
		s.written += room
		return int(room), errDiskFull
	}
	s.written += int64(len(p))
	return len(p), nil
}

func (s *errSink) Commit() (string, error) {
	s.committed = true
	return s.commitPath, nil
}

func (s *errSink) Abort() error { s.aborted = true; return nil }

var _ contentSink = (*errSink)(nil)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return buf
}

func TestStreamBodyExactReceipt(t *testing.T) {
	st := newTestStore(t)
	data := randomBytes(t, 3<<20) // 3 MiB: hashing + multi-buffer streaming
	sink, err := st.OpenPart("exact.bin")
	if err != nil {
		t.Fatalf("OpenPart: %v", err)
	}

	want := sha256.Sum256(data)
	res, err := streamBody(bytes.NewReader(data), sink, int64(len(data)), nil)
	if err != nil {
		t.Fatalf("streamBody: %v", err)
	}
	if res.Written != int64(len(data)) {
		t.Errorf("written = %d, want %d", res.Written, len(data))
	}
	if res.SHA256 != want {
		t.Errorf("hash mismatch: got %x, want %x", res.SHA256, want)
	}

	final, err := sink.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("read %s: %v", final, err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("published bytes differ from the streamed bytes")
	}
}

func TestStreamBodyZeroSize(t *testing.T) {
	st := newTestStore(t)
	sink := mustOpenPart(t, st, "empty.bin")
	res, err := streamBody(bytes.NewReader(nil), sink, 0, nil)
	if err != nil {
		t.Fatalf("streamBody: %v", err)
	}
	if res.Written != 0 {
		t.Errorf("written = %d, want 0", res.Written)
	}
	want := sha256.Sum256(nil)
	if res.SHA256 != want {
		t.Errorf("empty hash = %x, want %x (of nothing)", res.SHA256, want)
	}
	if _, err := sink.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestStreamBodyRejectsOverrun(t *testing.T) {
	sink := &errSink{failAfter: -1}
	data := randomBytes(t, 1024)
	_, err := streamBody(bytes.NewReader(data), sink, int64(len(data)-1), nil)
	if !errors.Is(err, ErrSizeExceeded) {
		t.Fatalf("err = %v, want ErrSizeExceeded", err)
	}
	if sink.written > int64(len(data)-1) {
		t.Errorf("sink received %d bytes; excess bytes must never be written, approved was %d", sink.written, len(data)-1)
	}
	if sink.aborted {
		t.Error("streamBody must leave Abort to the caller")
	}
}

func TestStreamBodyRejectsTruncation(t *testing.T) {
	sink := &errSink{failAfter: -1}
	data := randomBytes(t, 512)
	_, err := streamBody(bytes.NewReader(data), sink, int64(len(data))+1, nil)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if sink.written != int64(len(data)) {
		t.Errorf("sink received %d, want the full partial stream %d", sink.written, len(data))
	}
}

func TestStreamBodyRejectsNegativeSize(t *testing.T) {
	sink := &errSink{failAfter: -1}
	_, err := streamBody(bytes.NewReader(nil), sink, -1, nil)
	if err == nil || errors.Is(err, ErrSizeExceeded) || errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want a non-size error for a negative declared size", err)
	}
}

func TestStreamBodyReaderErrorStopsTheStream(t *testing.T) {
	sink := &errSink{failAfter: -1}
	wantErr := errors.New("reader exploded")
	multi := io.MultiReader(bytes.NewReader(make([]byte, 64)), errReader{err: wantErr})
	if _, err := streamBody(multi, sink, 256, nil); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the wrapped reader error", err)
	}
	// The caller is responsible for aborting; the fake records writes so
	// a partial write count below the declared size is observable.
	if sink.written != 64 {
		t.Errorf("sink received %d bytes before the reader failed; want 64", sink.written)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func TestStreamBodyProgressReportsMonotonically(t *testing.T) {
	sink := &errSink{failAfter: -1}
	// progressStep is 1 MiB: a 2.5 MiB stream must report progress twice
	// plus a final flush.
	data := make([]byte, 2500*1024)
	reports := []int64{}
	if _, err := streamBody(bytes.NewReader(data), sink, int64(len(data)), func(n int64) {
		if len(reports) > 0 && n <= reports[len(reports)-1] {
			t.Errorf("progress not monotonic: %v", reports)
		}
		reports = append(reports, n)
	}); err != nil {
		t.Fatalf("streamBody: %v", err)
	}
	if len(reports) < 2 {
		t.Fatalf("progress reported %d times, want at least 2", len(reports))
	}
	if last := reports[len(reports)-1]; last != int64(len(data)) {
		t.Errorf("final progress = %d, want %d (the flush must cover the tail)", last, len(data))
	}
	if !sort.SliceIsSorted(reports, func(i, j int) bool { return reports[i] < reports[j] }) {
		t.Errorf("progress reports not sorted: %v", reports)
	}
}

func TestStreamBodyPropagatesSinkFailure(t *testing.T) {
	sink := &errSink{failAfter: 100}
	data := randomBytes(t, 4096)
	_, err := streamBody(bytes.NewReader(data), sink, int64(len(data)), nil)
	if !errors.Is(err, errDiskFull) {
		t.Fatalf("err = %v, want the wrapped fake write failure", err)
	}
	if sink.committed {
		t.Error("a failed stream must never be committed by streamBody; the caller owns finalization")
	}
}
