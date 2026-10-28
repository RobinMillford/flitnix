package server

import (
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

// TestRevokedSessionStopsAnInFlightDirectDownload pins the cancellation policy
// for GET /api/files/{id}/download: the body is an authenticated transfer, so
// revoking the session while it streams must end it. The archive path already
// does this through guardedReader (errDownloadSession); this endpoint used to
// keep sending every remaining byte and then record the transfer as `sent`,
// which made a dead session look like a completed download.
//
// The status line is committed before the first byte, so the only honest
// ending is a cut connection: the client must not receive a whole,
// complete-looking file.
func TestRevokedSessionStopsAnInFlightDirectDownload(t *testing.T) {
	live := newLiveEnv(t)
	tok := live.token()
	p := filepath.Join(t.TempDir(), "big.bin")
	const size = 32 << 20
	sparseFile(t, p, size)
	sf, err := live.reg.Add(p, "big.bin", "")
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, live.ts.URL+"/api/files/"+sf.ID+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != size {
		t.Fatalf("download = %d with length %d, want 200 with %d", resp.StatusCode, resp.ContentLength, size)
	}

	// Read one chunk, then revoke. The body is far larger than any socket
	// buffer, so the server cannot have finished writing yet.
	buf := make([]byte, downloadCopySize)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	sess, err := live.sess.Validate(tok)
	if err != nil {
		t.Fatal(err)
	}
	live.sess.RevokeSession(sess)
	if got := sess.Status(); got != session.StatusRevoked {
		t.Fatalf("session status after revoke = %q", got)
	}

	rest, _ := io.Copy(io.Discard, resp.Body)
	if total := int64(len(buf)) + rest; total >= size {
		t.Fatalf("revoked session still received the whole file (%d of %d bytes)", total, size)
	}

	waitFor(t, "the download record to reach its terminal state", func() bool {
		for _, d := range live.dls.List() {
			if d.State == transfer.DownloadFailed || d.State == transfer.DownloadSent {
				return true
			}
		}
		return false
	})
	list := live.dls.List()
	if len(list) != 1 {
		t.Fatalf("download records = %d, want 1", len(list))
	}
	d := list[0]
	if d.State != transfer.DownloadFailed {
		t.Fatalf("desktop download state = %q, want failed: a revoked session must not be recorded as sent", d.State)
	}
	if d.Reason != errDownloadSession.Error() {
		t.Errorf("failure reason = %q, want %q", d.Reason, errDownloadSession.Error())
	}
	if d.Written >= size {
		t.Errorf("recorded %d written bytes of %d: the stream was not cut", d.Written, size)
	}
}
