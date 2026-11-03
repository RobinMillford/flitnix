// Durable pairing lockout state (F-05).
//
// The five-failure / fifteen-minute pairing lockout previously lived only in
// PairingCoordinator memory, so every desktop restart handed an attacker a
// fresh failure budget and silently dropped any active lockout. This file
// adds a small, bounded, control-plane record that survives restarts.
//
// # Why a dedicated file and not the recovery journal
//
// The internal/recovery journal is technically compatible, but it is enabled
// only when server.Config.RecoveryDir is set — and no production entry point
// sets it. Persisting pairing state solely through the recovery store would
// therefore produce a mechanism that never runs outside tests. Flitnix's own
// idiom for control-plane state is one small JSON file per domain
// (settings.json, devices.json, audit.jsonl, templates.json); this follows
// that idiom with recovery-grade durability (fsync + atomic rename + parent
// directory fsync). It is one bounded record, never a second database, and
// nothing here is touched by upload or download payload processing.
//
// # What is (not) persisted
//
// The record holds only wall-clock timestamps: the failure timestamps inside
// the rolling window and the lockout expiry. The failure counter is
// host-global by policy, so there is no per-client identity to key or
// pseudonymise — and therefore no bearer token, pairing secret, transcript
// material, or key material is ever written. The record is authenticated
// with HMAC-SHA256 keyed by a value derived from the host identity private
// key, so corruption or tampering is detected instead of trusted.
//
// # Failure policy (fail safe = fail closed)
//
//   - A missing record is a fresh installation: no state, no lockout.
//   - An unreadable, unparseable, unauthenticated, out-of-bounds, or
//     wrong-schema record degrades the coordinator: pairing is refused with
//     ErrPairingLocked until the operator re-arms. A corrupt security record
//     must never silently disable the lockout.
//   - A failed save leaves the in-memory enforcement in place and degrades
//     the coordinator the same way: pairing must not continue on the
//     assumption that the failure was durably recorded. Nothing in the
//     public API ever reports a lockout as persisted when the durable write
//     failed.
//   - ReArmPairing (the operator's documented recovery action) attempts to
//     write the cleared record; if that write succeeds the degraded state is
//     lifted, otherwise pairing stays locked.
package devices

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// pairingSecuritySchema is the on-disk record schema. A record with any
	// other version is treated as corrupt and fails closed.
	pairingSecuritySchema = 1

	// maxPersistedFailures bounds the failure list of one record. The real
	// threshold is MaxFailedAttempts; this generous cap only guarantees the
	// record cannot grow without limit even if a future policy raises the
	// threshold, and gives replay validation a hard ceiling.
	maxPersistedFailures = 64

	// pairingSecurityMACDomain separates this key derivation from any other
	// use of the host identity key.
	pairingSecurityMACDomain = "flitnix-pairing-security-v1"
)

// PairingSecurityRecord is the single durable lockout record. It contains no
// secret material: only timestamps.
type PairingSecurityRecord struct {
	SchemaVersion     int         `json:"schemaVersion"`
	FailureTimestamps []time.Time `json:"failureTimestamps,omitempty"`
	LockoutUntil      time.Time   `json:"lockoutUntil,omitempty"`
	UpdatedAt         time.Time   `json:"updatedAt"`
	MAC               string      `json:"mac"`
}

// pairingMACKey derives the record-MAC key from the host identity private
// key. The key never leaves memory and is never persisted.
func (h *HostIdentity) pairingMACKey() []byte {
	h.mu.RLock()
	defer h.mu.RUnlock()
	label := []byte(pairingSecurityMACDomain)
	buf := make([]byte, 0, len(label)+len(h.privKey))
	buf = append(buf, label...)
	buf = append(buf, h.privKey...)
	sum := sha256.Sum256(buf)
	return sum[:]
}

func (r PairingSecurityRecord) sealed(macKey []byte) (PairingSecurityRecord, error) {
	r.SchemaVersion = pairingSecuritySchema
	r.MAC = ""
	payload, err := json.Marshal(r)
	if err != nil {
		return PairingSecurityRecord{}, fmt.Errorf("devices: encode pairing security record: %w", err)
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write(payload)
	r.MAC = hex.EncodeToString(mac.Sum(nil))
	return r, nil
}

// validate rejects records that are structurally out of bounds. A record that
// fails validation is treated as corrupt by the caller (fail closed).
func (r PairingSecurityRecord) validate() error {
	if r.SchemaVersion != pairingSecuritySchema {
		return fmt.Errorf("schema version %d, want %d", r.SchemaVersion, pairingSecuritySchema)
	}
	if len(r.FailureTimestamps) > maxPersistedFailures {
		return fmt.Errorf("%d failure timestamps exceeds the %d record bound", len(r.FailureTimestamps), maxPersistedFailures)
	}
	for i, t := range r.FailureTimestamps {
		if t.IsZero() {
			return fmt.Errorf("failure timestamp %d is zero", i)
		}
	}
	return nil
}

// pairingSecurityStore persists exactly one sealed record at path.
type pairingSecurityStore struct {
	path   string
	macKey []byte
}

// load returns the stored record, or nil when no record exists yet. Any
// read, parse, authentication, or validation failure is an error: the caller
// must fail closed.
func (s *pairingSecurityStore) load() (*PairingSecurityRecord, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("devices: read pairing security record: %w", err)
	}
	var rec PairingSecurityRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("devices: pairing security record is corrupt: %w", err)
	}
	if !rec.verifyMAC(s.macKey) {
		return nil, errors.New("devices: pairing security record failed authentication")
	}
	if err := rec.validate(); err != nil {
		return nil, fmt.Errorf("devices: pairing security record is invalid: %v", err)
	}
	return &rec, nil
}

func (r PairingSecurityRecord) verifyMAC(macKey []byte) bool {
	expected := r.MAC
	if expected == "" {
		return false
	}
	r.MAC = ""
	payload, err := json.Marshal(r)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write(payload)
	return hmac.Equal([]byte(expected), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// save durably writes the record: temp file, fsync, atomic rename, then a
// parent-directory fsync, mirroring the recovery store's write discipline.
func (s *pairingSecurityStore) save(rec PairingSecurityRecord) error {
	sealed, err := rec.sealed(s.macKey)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(sealed, "", "  ")
	if err != nil {
		return fmt.Errorf("devices: encode pairing security record: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("devices: create pairing security directory: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("devices: create pairing security temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("devices: write pairing security temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("devices: sync pairing security temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("devices: close pairing security temp file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("devices: install pairing security record: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// EnableDurableLockout makes the failure counter and lockout survive restarts
// by persisting them to filePath (a small, single-record, HMAC-authenticated
// control-plane file). Call it once at startup, before any pairing traffic.
//
// A missing file starts clean. A corrupt or unauthenticated file fails
// closed: the coordinator reports the error and refuses pairing (with
// ErrPairingLocked) until the operator re-arms, which rewrites the record.
func (c *PairingCoordinator) EnableDurableLockout(filePath string) error {
	if c.identity == nil {
		// Fail closed: a coordinator without a host identity cannot derive the
		// record-MAC key, so durable enforcement is impossible. Marking it
		// degraded here guarantees the production construction path can never
		// silently fall back to legacy in-memory-only protection.
		c.mu.Lock()
		if c.degraded == nil {
			c.degraded = errors.New("devices: pairing coordinator has no host identity")
		}
		c.mu.Unlock()
		return c.degraded
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	sec := &pairingSecurityStore{path: filePath, macKey: c.identity.pairingMACKey()}
	rec, err := sec.load()
	c.security = sec
	if err != nil {
		// Fail closed: a security record we cannot trust must never disable
		// the lockout it protects.
		if c.degraded == nil {
			c.degraded = err
		}
		return err
	}
	if rec == nil {
		return nil
	}

	now := time.Now().UTC()
	for _, t := range rec.FailureTimestamps {
		if t.After(now.Add(-FailureWindow)) {
			c.failureTimestamps = append(c.failureTimestamps, t)
		}
	}
	if rec.LockoutUntil.After(now) {
		// Active lockouts keep their original expiry across the restart.
		c.lockoutUntil = rec.LockoutUntil
	}
	return nil
}

// persistSecurityLocked durably records the current lockout state. Callers
// hold c.mu. A save failure degrades the coordinator (fail closed) but never
// rolls back the in-memory enforcement and is never reported as a persisted
// lockout.
func (c *PairingCoordinator) persistSecurityLocked(now time.Time) {
	if c.security == nil {
		return
	}
	rec := PairingSecurityRecord{
		FailureTimestamps: append([]time.Time(nil), c.failureTimestamps...),
		LockoutUntil:      c.lockoutUntil,
		UpdatedAt:         now,
	}
	if err := c.security.save(rec); err != nil {
		if c.degraded == nil {
			c.degraded = err
		}
	}
}

// persistenceBlockedLocked reports whether durable-state trouble currently
// forbids pairing. Callers hold c.mu. Any degraded state blocks pairing, even
// when the security store itself never got wired: a coordinator whose durable
// dependency is missing (for example a nil host identity) must fail closed
// rather than silently continue with in-memory-only enforcement.
func (c *PairingCoordinator) persistenceBlockedLocked() bool {
	return c.degraded != nil
}
