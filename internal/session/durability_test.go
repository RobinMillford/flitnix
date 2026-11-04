package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func hmacVerifier(key []byte) func(string) [sha256.Size]byte {
	return func(token string) [sha256.Size]byte {
		m := hmac.New(sha256.New, key)
		_, _ = m.Write([]byte(token))
		var out [sha256.Size]byte
		copy(out[:], m.Sum(nil))
		return out
	}
}

func TestDurableTokenRestoration(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var persisted []SessionRecord

	m := NewManager(Config{
		Verifier: hmacVerifier(key),
		OnCreate: func(r SessionRecord) error {
			persisted = append(persisted, r)
			return nil
		},
	})
	created, err := m.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID() == "" {
		t.Fatal("created session has no stable identity")
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted %d records, want 1", len(persisted))
	}
	if persisted[0].Verifier == ([sha256.Size]byte{}) {
		t.Fatal("persisted record has no token verifier")
	}
	if persisted[0].SessionKey != created.Key() {
		t.Fatal("persisted session key does not match the stable owner key")
	}

	restored := NewManager(Config{Verifier: hmacVerifier(key)})
	for _, record := range persisted {
		if err := restored.Restore(record); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	got, err := restored.Validate(created.Token())
	if err != nil {
		t.Fatalf("validate restored token: %v", err)
	}
	if got.Key() != created.Key() {
		t.Fatal("restored session key changed")
	}
}

func TestRestoredTokenRejectsIncorrectToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var persisted []SessionRecord
	m := NewManager(Config{Verifier: hmacVerifier(key), OnCreate: func(r SessionRecord) error {
		persisted = append(persisted, r)
		return nil
	}})
	created, err := m.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	restored := NewManager(Config{Verifier: hmacVerifier(key)})
	for _, record := range persisted {
		if err := restored.Restore(record); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	if _, err := restored.Validate("not-the-original-token-value-aaaaaaaaaaaaaaa"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("validate with a wrong token = %v, want ErrInvalid", err)
	}

	// A verifier derived under a different installation key must not match
	// even for the correct token.
	foreign := NewManager(Config{Verifier: hmacVerifier([]byte("ffffffffffffffffffffffffffffffff"))})
	for _, record := range persisted {
		if err := foreign.Restore(record); err != nil {
			t.Fatalf("restore foreign: %v", err)
		}
	}
	if _, err := foreign.Validate(created.Token()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("validate under a foreign key = %v, want ErrInvalid", err)
	}
}

func TestRevocationSurvivesRestart(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var records []SessionRecord
	persist := func(r SessionRecord) error {
		for i := range records {
			if records[i].SessionKey == r.SessionKey {
				records[i] = r
				return nil
			}
		}
		records = append(records, r)
		return nil
	}

	m := NewManager(Config{Verifier: hmacVerifier(key), OnCreate: persist, OnRevoke: persist})
	created, err := m.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Revoke(created.Token()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	restored := NewManager(Config{Verifier: hmacVerifier(key)})
	for _, record := range records {
		if err := restored.Restore(record); err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	if _, err := restored.Validate(created.Token()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("validate revoked token after restart = %v, want ErrRevoked", err)
	}
	if restored.Len() != 0 {
		t.Fatalf("restored manager has %d active sessions, want 0", restored.Len())
	}
}

func TestRevocationIsNotAcknowledgedWhenNotDurable(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	m := NewManager(Config{
		Verifier: hmacVerifier(key),
		OnRevoke: func(SessionRecord) error { return errors.New("journal unavailable") },
	})
	created, err := m.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Revoke(created.Token()); err == nil {
		t.Fatal("revoke reported success without a durable record")
	}
	if _, err := m.Validate(created.Token()); err != nil {
		t.Fatalf("session was ended despite a failed durable revocation: %v", err)
	}
}

func TestExpirySurvivesRestart(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var persisted []SessionRecord
	origin := NewManager(Config{Verifier: hmacVerifier(key), OnCreate: func(r SessionRecord) error {
		persisted = append(persisted, r)
		return nil
	}})
	created, err := origin.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted %d records, want 1", len(persisted))
	}

	// The durable record still says active, but its absolute expiry has
	// passed. A restart must not revive it with a fresh idle window.
	record := persisted[0]
	if record.Status != StatusActive {
		t.Fatalf("create persisted status %q, want active", record.Status)
	}
	record.ExpiresAt = time.Now().Add(-time.Minute)

	restored := NewManager(Config{Verifier: hmacVerifier(key)})
	if err := restored.Restore(record); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Len() != 0 {
		t.Fatalf("expired session was restored as active")
	}
	if _, err := restored.Validate(created.Token()); !errors.Is(err, ErrExpired) {
		t.Fatalf("validate expired token after restart = %v, want ErrExpired", err)
	}
}

func TestShutdownPreservesEligibleSessions(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	var persisted []SessionRecord
	m := NewManager(Config{
		Verifier: hmacVerifier(key),
		OnCreate: func(r SessionRecord) error { persisted = append(persisted, r); return nil },
		OnRevoke: func(SessionRecord) error {
			t.Fatal("shutdown attempted a durable revocation")
			return nil
		},
	})
	created, err := m.Create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m.Shutdown()

	if len(persisted) != 1 || persisted[0].Status != StatusActive {
		t.Fatalf("durable record changed at shutdown: %+v", persisted)
	}
	restored := NewManager(Config{Verifier: hmacVerifier(key)})
	for _, record := range persisted {
		if err := restored.Restore(record); err != nil {
			t.Fatalf("restore after shutdown: %v", err)
		}
	}
	if _, err := restored.Validate(created.Token()); err != nil {
		t.Fatalf("session did not survive a graceful shutdown: %v", err)
	}
}

func TestVerifierCannotChangeWhileSessionsAreActive(t *testing.T) {
	m := NewManager(Config{})
	if _, err := m.Create(); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.SetVerifier(hmacVerifier([]byte("k"))); err == nil {
		t.Fatal("token verifier changed while a session was active")
	}
}
