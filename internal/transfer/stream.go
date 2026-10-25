package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// contentSink receives the streamed upload body. Bytes are appended with
// Write while the stream is in progress; Commit publishes the received data
// under its final, collision-safe filename and Abort discards it. PartFile
// implements the interface; service tests replace the package sink factory
// with fakes to exercise write failures without touching the disk.
//
// Exactly one goroutine may use a contentSink at a time.
type contentSink interface {
	io.Writer

	// Commit finalizes the received data and returns its published
	// absolute path.
	Commit() (finalPath string, err error)

	// Abort discards the received data. It must be safe to call after a
	// successful or failed Commit (make it idempotent).
	Abort() error
}

var (
	// ErrSizeExceeded means the sender pushed more bytes than the
	// approved size.
	ErrSizeExceeded = errors.New("transfer: upload exceeds the approved size")

	// ErrTruncated means the sender's stream ended before the approved
	// size was reached (dropped connection, disk-full on the phone, or a
	// lying Content-Length).
	ErrTruncated = errors.New("transfer: upload is shorter than the approved size")
)

// PartFile is the production contentSink.
var _ contentSink = (*PartFile)(nil)

// progressStep is the byte interval between progress callbacks during
// streaming. 1 MiB keeps polling cheap for readers while giving the desktop
// UI a smooth progress indicator even for multi-gigabyte files.
const progressStep = 1024 * 1024

// streamCopySize is the fixed buffer used for streaming. Bounded memory is a
// deliberate property: a multi-gigabyte upload must never inflate RAM.
const streamCopySize = 64 * 1024

// StreamResult describes a fully received upload body. The checksum covers
// exactly the bytes written to the sink.
type StreamResult struct {
	Written int64             // actual bytes written to the sink
	SHA256  [sha256.Size]byte // digest of the written bytes
}

// HexSHA256 returns the digest as lowercase hex (64 characters).
func (r StreamResult) HexSHA256() string { return hex.EncodeToString(r.SHA256[:]) }

// streamBody copies exactly declared bytes from r into sink, computes their
// SHA-256 digest inline, and reports progress. It enforces the approved
// size aggressively: more, fewer, or malformed data abort the transfer
// before anything else happens; the caller decides exactly how to fail.
//
// The function never finalizes the sink: on success the caller invokes
// Commit; on any error the caller invokes Abort. This separation keeps
// cancellation (server shutdown, session end) fully under the service's
// control.
//
// Size bounds: declared must be >= 0 (negative sizes are rejected here so
// even a corrupted metadata record cannot desynchronize the accounting).
func streamBody(r io.Reader, sink contentSink, declared int64, onProgress func(written int64)) (StreamResult, error) {
	if declared < 0 {
		return StreamResult{}, fmt.Errorf("transfer: approved size is negative (%d)", declared)
	}

	hasher := sha256.New()
	buf := make([]byte, streamCopySize)
	var res StreamResult
	lastReported := int64(-1)

	for res.Written < declared {
		n, rerr := r.Read(buf)
		if n > 0 {
			if remaining := declared - res.Written; int64(n) > remaining {
				// More data than approved: reject the whole transfer.
				// Writing the excess would publish a file the user
				// never saw a size for.
				return res, fmt.Errorf("%w: approved %d, got %d+%d", ErrSizeExceeded, declared, res.Written, n)
			}
			wn, werr := sink.Write(buf[:n])
			if werr == nil && wn < n {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				return res, fmt.Errorf("transfer: writing received data: %w", werr)
			}
			hasher.Write(buf[:n])
			res.Written += int64(n)
			if onProgress != nil && res.Written-lastReported >= progressStep {
				onProgress(res.Written)
				lastReported = res.Written
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) && res.Written == declared {
				break
			}
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return res, fmt.Errorf("%w: received %d of %d approved bytes", ErrTruncated, res.Written, declared)
			}
			return res, fmt.Errorf("transfer: reading upload body: %w", rerr)
		}
	}
	if onProgress != nil && lastReported != res.Written {
		onProgress(res.Written)
	}
	hasher.Sum(res.SHA256[:0])
	return res, nil
}
