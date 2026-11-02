package recovery

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sessionRecord(key string) SessionRecord {
	return SessionRecord{
		SessionID:     "sess_" + key,
		SessionKey:    key,
		TokenVerifier: "verifier-" + key,
		KeyID:         KeyID,
		Generation:    1,
		Status:        "active",
		CreatedAt:     time.Now().UTC(),
		ExpiresAt:     time.Now().UTC().Add(time.Hour),
	}
}

// forgeFrame builds a length-framed event with a correct MAC under the given
// key. Tests use it to present structurally valid but logically hostile
// records that a mere byte flip could not express.
func forgeFrame(t *testing.T, key []byte, ev Event) []byte {
	t.Helper()
	m := hmac.New(sha256.New, key)
	var buf [12]byte
	binary.BigEndian.PutUint32(buf[:4], ev.SchemaVersion)
	binary.BigEndian.PutUint64(buf[4:], ev.Sequence)
	_, _ = m.Write(buf[:])
	_, _ = m.Write([]byte(ev.Type))
	_, _ = m.Write([]byte{0})
	_, _ = m.Write(ev.Payload)
	ev.MAC = hex.EncodeToString(m.Sum(nil))
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal forged frame: %v", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	return append(hdr[:], body...)
}

func readKey(t *testing.T, dir string) []byte {
	t.Helper()
	key, err := os.ReadFile(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("read installation key: %v", err)
	}
	return key
}

func journalSize(t *testing.T, dir string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, journalName))
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	return info.Size()
}

func appendBytes(t *testing.T, dir string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, journalName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open journal for append: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatalf("append journal bytes: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

func TestStoreFreshInstallCreatesKeyDurably(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open fresh store: %v", err)
	}
	if !store.CreatedKey() {
		t.Fatal("fresh install did not report creating the installation key")
	}
	defer store.Close()

	info, err := os.Stat(filepath.Join(dir, keyFileName))
	if err != nil {
		t.Fatalf("stat installation key: %v", err)
	}
	if info.Size() != KeyLen {
		t.Fatalf("installation key size = %d, want %d", info.Size(), KeyLen)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("installation key mode = %o, want 600", mode)
	}
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		t.Fatal("state directory is empty")
	}
}

func TestStoreMissingKeyWithExistingStateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, keyFileName)); err != nil {
		t.Fatalf("remove key: %v", err)
	}

	_, err = Open(dir)
	if !errors.Is(err, ErrKey) {
		t.Fatalf("open without installation key = %v, want ErrKey", err)
	}
	// The refused open must not have generated a replacement key.
	if _, statErr := os.Stat(filepath.Join(dir, keyFileName)); !os.IsNotExist(statErr) {
		t.Fatalf("a replacement installation key was created: %v", statErr)
	}
}

func TestStoreInvalidKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFileName), []byte("too-short"), 0o600); err != nil {
		t.Fatalf("corrupt key: %v", err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrKey) {
		t.Fatalf("open with invalid key = %v, want ErrKey", err)
	}
}

func TestStoreEnforcesSingleInstanceOwnership(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()

	if _, err := Open(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open = %v, want ErrLocked", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("open after release: %v", err)
	}
	_ = second.Close()
}

func TestStoreReplayRestoresProjection(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append session: %v", err)
	}
	declared := BatchRecord{
		ID: "b1", SessionKey: "k1", Direction: "phone_to_desktop", Kind: "files",
		RootName: "Downloads", TotalItemCount: 1, DeclaredTotalBytes: 10,
		State: "pending", ApprovalState: "pending", CreatedAt: time.Now().UTC(),
		Items: []ItemRecord{{ID: "i1", BatchID: "b1", Sequence: 0, DeclaredSize: 10, State: "pending"}},
	}
	if err := store.Append(EventBatchDeclared, declared); err != nil {
		t.Fatalf("append batch: %v", err)
	}
	if err := store.Append(EventApprovalChanged, ApprovalChange{BatchID: "b1", ApprovalState: "approved", BatchState: "queued"}); err != nil {
		t.Fatalf("append approval: %v", err)
	}
	if err := store.Append(EventUploadAllocated, Allocation{BatchID: "b1", ItemID: "i1", ContentID: "c1"}); err != nil {
		t.Fatalf("append allocation: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	state := reopened.State()
	if len(state.Sessions) != 1 || state.Sessions[0].SessionKey != "k1" {
		t.Fatalf("sessions = %+v", state.Sessions)
	}
	if len(state.Batches) != 1 {
		t.Fatalf("batches = %+v", state.Batches)
	}
	if state.Batches[0].ApprovalState != "approved" || state.Batches[0].State != "queued" {
		t.Fatalf("approval not replayed: %+v", state.Batches[0])
	}
	if state.Batches[0].Items[0].ContentID != "c1" {
		t.Fatalf("allocation not replayed: %+v", state.Batches[0].Items[0])
	}
}

func TestStoreIncompleteTailTruncatesToExactValidSize(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	valid := journalSize(t, dir)
	if valid == 0 {
		t.Fatal("journal is empty after a successful append")
	}

	// A torn final frame: a plausible length header and a partial body.
	appendBytes(t, dir, []byte{0, 0, 0, 20, '{'})
	if got := journalSize(t, dir); got <= valid {
		t.Fatalf("journal size = %d, expected a torn tail after %d valid bytes", got, valid)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen with incomplete tail: %v", err)
	}
	defer reopened.Close()

	if got := journalSize(t, dir); got != valid {
		t.Fatalf("journal size after tail repair = %d, want exactly %d", got, valid)
	}
	if got := reopened.State().Sessions; len(got) != 1 || got[0].SessionKey != "k1" {
		t.Fatalf("valid history lost: %+v", got)
	}
	// Recovered state must still be appendable at the next sequence.
	if err := reopened.Append(EventSessionCreated, sessionRecord("k2")); err != nil {
		t.Fatalf("append after tail repair: %v", err)
	}
}

func TestStoreRejectsMidJournalCorruptionWithoutTruncation(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := store.Append(EventSessionCreated, sessionRecord("k2")); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	key := readKey(t, dir)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	path := filepath.Join(dir, journalName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	// Corrupt one byte inside the second frame's payload, then append a
	// structurally valid third frame so the damage is provably not a tail.
	first := int(binary.BigEndian.Uint32(data[:4]))
	secondStart := 4 + first
	second := int(binary.BigEndian.Uint32(data[secondStart : secondStart+4]))
	data[secondStart+4+second/2] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write corrupted journal: %v", err)
	}
	appendBytes(t, dir, forgeFrame(t, key, Event{
		SchemaVersion: schemaVersion, Sequence: 3, EventID: "evt_late",
		Type: EventSessionCreated, OccurredAt: time.Now().UTC(),
		Payload: mustJSON(t, sessionRecord("k3")),
	}))
	before := journalSize(t, dir)

	_, err = Open(dir)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("open with mid-journal corruption = %v, want ErrCorrupt", err)
	}
	if got := journalSize(t, dir); got != before {
		t.Fatalf("corrupt journal was truncated from %d to %d bytes", before, got)
	}
}

func TestStoreRejectsInvalidAuthentication(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append: %v", err)
	}
	key := readKey(t, dir)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Correct framing, correct sequence, wrong MAC.
	body := forgeFrame(t, key, Event{
		SchemaVersion: schemaVersion, Sequence: 2, EventID: "evt_bad",
		Type: EventSessionCreated, OccurredAt: time.Now().UTC(),
		Payload: mustJSON(t, sessionRecord("k2")),
	})
	raw, err := json.Marshal(Event{
		SchemaVersion: schemaVersion, Sequence: 2, EventID: "evt_bad",
		Type: EventSessionCreated, OccurredAt: time.Now().UTC(),
		Payload: mustJSON(t, sessionRecord("k2")), MAC: "deadbeef",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_ = body
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(raw)))
	appendBytes(t, dir, append(hdr[:], raw...))

	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("open with forged MAC = %v, want ErrCorrupt", err)
	}
}

func TestStoreRejectsInvalidSequenceAndSchema(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   Event
	}{
		{"duplicate sequence", Event{SchemaVersion: schemaVersion, Sequence: 1, EventID: "evt_dup", Type: EventSessionCreated}},
		{"skipped sequence", Event{SchemaVersion: schemaVersion, Sequence: 5, EventID: "evt_skip", Type: EventSessionCreated}},
		{"unsupported schema", Event{SchemaVersion: schemaVersion + 1, Sequence: 2, EventID: "evt_v2", Type: EventSessionCreated}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
				t.Fatalf("append: %v", err)
			}
			key := readKey(t, dir)
			if err := store.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			ev := tc.ev
			ev.OccurredAt = time.Now().UTC()
			ev.Payload = mustJSON(t, sessionRecord("k2"))
			appendBytes(t, dir, forgeFrame(t, key, ev))
			if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("open = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestStoreRejectsMalformedPayloadWithValidAuthentication(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	key := readKey(t, dir)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A valid MAC over an unparseable payload. The frame must be assembled by
	// hand because json.Marshal refuses to encode invalid raw JSON.
	payload := []byte("{")
	m := hmac.New(sha256.New, key)
	var buf [12]byte
	binary.BigEndian.PutUint32(buf[:4], schemaVersion)
	binary.BigEndian.PutUint64(buf[4:], 1)
	_, _ = m.Write(buf[:])
	_, _ = m.Write([]byte(EventSessionCreated))
	_, _ = m.Write([]byte{0})
	_, _ = m.Write(payload)
	body := fmt.Sprintf(
		`{"schemaVersion":%d,"sequence":1,"eventId":"evt_broken","type":%q,"occurredAt":"2026-11-02T00:00:00Z","payload":%s,"mac":%q}`,
		schemaVersion, EventSessionCreated, string(payload), hex.EncodeToString(m.Sum(nil)),
	)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	appendBytes(t, dir, append(hdr[:], []byte(body)...))
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("open with malformed payload = %v, want ErrCorrupt", err)
	}
}

func TestStoreRejectsUnknownEventType(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	key := readKey(t, dir)
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	appendBytes(t, dir, forgeFrame(t, key, Event{
		SchemaVersion: schemaVersion, Sequence: 1, EventID: "evt_unknown",
		Type: "SomethingElse", OccurredAt: time.Now().UTC(),
		Payload: json.RawMessage(`{}`),
	}))
	if _, err := Open(dir); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("open with unknown event type = %v, want ErrCorrupt", err)
	}
}

func TestStoreAppendFailureIsSticky(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// An event whose payload cannot be applied is rejected before the write,
	// so it must not consume a sequence number or poison the journal.
	err = store.Append(EventApprovalChanged, ApprovalChange{BatchID: "missing"})
	if err == nil {
		t.Fatal("append of an unappliable event succeeded")
	}
	if store.appendErr != nil {
		t.Fatalf("rejected event latched the journal: %v", store.appendErr)
	}
	if err := store.Append(EventSessionCreated, sessionRecord("k1")); err != nil {
		t.Fatalf("append after a rejected event: %v", err)
	}
	if got := store.State().Sessions; len(got) != 1 {
		t.Fatalf("sessions = %+v, want one", got)
	}
}

func TestStoreVerifierIsKeyed(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()

	token := "a-bearer-token-value"
	if !a.VerifyToken(token, a.Verifier(token)) {
		t.Fatal("verifier does not accept its own token")
	}
	if a.Verifier(token) == b.Verifier(token) {
		t.Fatal("two installations derived the same verifier for one token")
	}
	if a.VerifyToken(token, b.Verifier(token)) {
		t.Fatal("a foreign installation's verifier was accepted")
	}
	if a.Verifier(token) == token {
		t.Fatal("verifier equals the raw token")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
