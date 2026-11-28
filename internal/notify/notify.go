// Package notify provides sanitized, policy-governed, bounded native desktop
// notifications with single-use token-backed interactive action handling.
package notify

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	MaxTitleBytes    = 96
	MaxBodyBytes     = 240
	DefaultQueueSize = 64
	actionTokenTTL   = 15 * time.Minute
	maxActionTokens  = 256
	defaultCooldown  = 400 * time.Millisecond
	maxDedupEntries  = 512
)

// Category classifies notifications for per-category settings filtering.
type Category string

const (
	CatIncomingTransfer  Category = "incoming_transfer"
	CatTransferCompleted Category = "transfer_completed"
	CatTransferFailed    Category = "transfer_failed"
	CatPairingRequest    Category = "pairing_request"
	CatGuestRequest      Category = "guest_request"
	CatQuickText         Category = "quick_text"
)

// ActionKind names the validated action executed on notification interaction.
type ActionKind string

const (
	ActionOpenWindow         ActionKind = "open_window"
	ActionApproveBatch       ActionKind = "approve_batch"
	ActionDeclineBatch       ActionKind = "decline_batch"
	ActionApproveUpload      ActionKind = "approve_upload"
	ActionDeclineUpload      ActionKind = "decline_upload"
	ActionApproveGuest       ActionKind = "approve_guest"
	ActionDeclineGuest       ActionKind = "decline_guest"
	ActionApproveGuestUpload ActionKind = "approve_guest_upload"
	ActionDeclineGuestUpload ActionKind = "decline_guest_upload"
	ActionReviewPairing      ActionKind = "review_pairing"
	ActionRevealTransfer     ActionKind = "reveal_transfer"
)

// Action describes an interactive notification button or default click action.
type Action struct {
	Token    string
	Kind     ActionKind
	TargetID string
	Label    string
}

// Notification represents a sanitized notification request ready for OS dispatch.
type Notification struct {
	Key           string
	Category      Category
	Title         string
	Body          string
	DefaultAction Action
	Actions       []Action
}

// Policy controls which notifications are emitted and whether filenames are masked.
type Policy struct {
	Enabled         bool
	QuietMode       bool
	HideFilenames   bool
	Incoming        bool
	Completed       bool
	Failed          bool
	QuickText       bool
	PairingRequests bool
	GuestRequests   bool
}

// DefaultPolicy returns standard enabled notification defaults.
func DefaultPolicy() Policy {
	return Policy{
		Enabled:         true,
		QuietMode:       false,
		HideFilenames:   false,
		Incoming:        true,
		Completed:       true,
		Failed:          true,
		QuickText:       true,
		PairingRequests: true,
		GuestRequests:   true,
	}
}

// ActionInvocation carries the validated server-side metadata resolved from an opaque action token.
type ActionInvocation struct {
	Kind     ActionKind
	TargetID string
}

type storedToken struct {
	Kind      ActionKind
	TargetID  string
	ExpiresAt time.Time
}

type backend interface {
	Supported() bool
	Send(n dispatchedNotification) error
	Close()
}

type dispatchedAction struct {
	Token string
	Label string
}

type dispatchedNotification struct {
	Key          string
	Title        string
	Body         string
	DefaultToken string
	Actions      []dispatchedAction
}

// Manager serializes notification delivery over a bounded queue and validates
// interactive callback tokens before dispatching application handlers.
type Manager struct {
	mu      sync.Mutex
	policy  Policy
	backend backend
	onAct   func(ActionInvocation)
	tokens  map[string]storedToken
	lastAt  map[string]time.Time
	queue   chan dispatchedNotification
	done    chan struct{}
	closed  bool
	nowFn   func() time.Time
}

// NewManager creates a notification Manager using the native platform backend.
func NewManager(policy Policy, onAction func(ActionInvocation)) *Manager {
	b := newPlatformBackend()
	m := newManagerWithBackend(policy, b, onAction)
	if b != nil {
		if starter, ok := b.(interface{ SetHandler(func(string)) }); ok {
			starter.SetHandler(func(tok string) {
				m.InvokeToken(tok)
			})
		}
	}
	return m
}

func newManagerWithBackend(policy Policy, b backend, onAction func(ActionInvocation)) *Manager {
	m := &Manager{
		policy:  policy,
		backend: b,
		onAct:   onAction,
		tokens:  make(map[string]storedToken),
		lastAt:  make(map[string]time.Time),
		queue:   make(chan dispatchedNotification, DefaultQueueSize),
		done:    make(chan struct{}),
		nowFn:   time.Now,
	}
	go m.worker()
	return m
}

func (m *Manager) worker() {
	defer close(m.done)
	for n := range m.queue {
		if m.backend != nil && m.backend.Supported() {
			_ = m.backend.Send(n)
		}
	}
}

// Supported reports whether native OS notifications are available on this host.
func (m *Manager) Supported() bool {
	if m == nil || m.backend == nil {
		return false
	}
	return m.backend.Supported()
}

// SetPolicy updates notification filtering and privacy rules dynamically.
func (m *Manager) SetPolicy(p Policy) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.policy = p
	m.mu.Unlock()
}

// Policy returns a snapshot of the active notification policy.
func (m *Manager) Policy() Policy {
	if m == nil {
		return Policy{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy
}

func (p Policy) allows(cat Category) bool {
	if !p.Enabled || p.QuietMode {
		return false
	}
	switch cat {
	case CatIncomingTransfer:
		return p.Incoming
	case CatTransferCompleted:
		return p.Completed
	case CatTransferFailed:
		return p.Failed
	case CatQuickText:
		return p.QuickText
	case CatPairingRequest:
		return p.PairingRequests
	case CatGuestRequest:
		return p.GuestRequests
	default:
		return false
	}
}

// Notify enqueues a notification if allowed by Policy and not duplicate-throttled.
func (m *Manager) Notify(n Notification) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed || !m.policy.allows(n.Category) || m.backend == nil || !m.backend.Supported() {
		m.mu.Unlock()
		return
	}
	now := m.nowFn()
	if n.Key != "" {
		if prev, ok := m.lastAt[n.Key]; ok && now.Sub(prev) < defaultCooldown {
			m.mu.Unlock()
			return
		}
		if len(m.lastAt) >= maxDedupEntries {
			m.pruneLastLocked(now)
		}
		m.lastAt[n.Key] = now
	}

	m.pruneTokensLocked(now)
	dn := dispatchedNotification{
		Key:   SanitizeText(n.Key, 64),
		Title: SanitizeText(n.Title, MaxTitleBytes),
		Body:  SanitizeText(n.Body, MaxBodyBytes),
	}
	if n.DefaultAction.Kind != "" {
		dn.DefaultToken = m.mintTokenLocked(n.DefaultAction.Kind, n.DefaultAction.TargetID, now)
	}
	for _, act := range n.Actions {
		if len(dn.Actions) >= 3 {
			break
		}
		lbl := SanitizeText(act.Label, 32)
		if lbl == "" || act.Kind == "" {
			continue
		}
		dn.Actions = append(dn.Actions, dispatchedAction{
			Token: m.mintTokenLocked(act.Kind, act.TargetID, now),
			Label: lbl,
		})
	}
	m.mu.Unlock()

	if dn.Title == "" && dn.Body == "" {
		return
	}

	select {
	case m.queue <- dn:
	default:
		// Drop newest low-priority item under extreme burst so hot paths never block.
	}
}

// InvokeToken validates an opaque action token and executes its registered callback at most once
// for stateful decisions (approvals/declines) or re-usable open/reveal targets until TTL.
func (m *Manager) InvokeToken(tok string) bool {
	if m == nil || tok == "" {
		return false
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	now := m.nowFn()
	rec, ok := m.tokens[tok]
	if !ok || now.After(rec.ExpiresAt) {
		delete(m.tokens, tok)
		m.mu.Unlock()
		return false
	}
	if isSingleUseDecision(rec.Kind) {
		delete(m.tokens, tok)
	}
	cb := m.onAct
	m.mu.Unlock()

	if cb != nil {
		cb(ActionInvocation{Kind: rec.Kind, TargetID: rec.TargetID})
	}
	return true
}

func isSingleUseDecision(k ActionKind) bool {
	switch k {
	case ActionApproveBatch, ActionDeclineBatch,
		ActionApproveUpload, ActionDeclineUpload,
		ActionApproveGuest, ActionDeclineGuest,
		ActionApproveGuestUpload, ActionDeclineGuestUpload:
		return true
	default:
		return false
	}
}

func (m *Manager) mintTokenLocked(kind ActionKind, targetID string, now time.Time) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	tok := hex.EncodeToString(raw[:])
	if len(m.tokens) >= maxActionTokens {
		m.pruneTokensLocked(now)
	}
	m.tokens[tok] = storedToken{
		Kind:      kind,
		TargetID:  targetID,
		ExpiresAt: now.Add(actionTokenTTL),
	}
	return tok
}

func (m *Manager) pruneTokensLocked(now time.Time) {
	for k, v := range m.tokens {
		if now.After(v.ExpiresAt) {
			delete(m.tokens, k)
		}
	}
	for len(m.tokens) >= maxActionTokens {
		for k := range m.tokens {
			delete(m.tokens, k)
			break
		}
	}
}

func (m *Manager) pruneLastLocked(now time.Time) {
	cutoff := now.Add(-10 * time.Second)
	for k, ts := range m.lastAt {
		if ts.Before(cutoff) {
			delete(m.lastAt, k)
		}
	}
	if len(m.lastAt) >= maxDedupEntries {
		clear(m.lastAt)
	}
}

// Close stops the notification queue and tears down native OS handles.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.queue)
	m.mu.Unlock()

	<-m.done
	if m.backend != nil {
		m.backend.Close()
	}
}

// SanitizeText strips ASCII control chars and Unicode BIDI overrides, collapses
// whitespace, and truncates safely on rune boundaries.
func SanitizeText(s string, maxBytes int) string {
	var b strings.Builder
	b.Grow(min(len(s), maxBytes+4))
	lastSpace := false
	for _, r := range s {
		if isUnsafeControlOrBidi(r) {
			continue
		}
		if unicode.IsSpace(r) {
			if b.Len() > 0 && !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		if b.Len()+len(string(r)) > maxBytes {
			if maxBytes >= 3 {
				out := strings.TrimSpace(b.String())
				if len(out)+3 <= maxBytes {
					return out + "…"
				}
			}
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// EscapeMarkup neutralizes XML/Pango markup characters for D-Bus notification servers.
func EscapeMarkup(s string) string {
	return html.EscapeString(s)
}

func isUnsafeControlOrBidi(r rune) bool {
	if r < 0x20 || r == 0x7f {
		return !unicode.IsSpace(r)
	}
	// Strip Unicode BIDI embedding/override/isolate characters (filename spoofing protection).
	if (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F {
		return true
	}
	return false
}

// FormatIncomingSummary builds privacy-aware notification text for incoming files/batches.
func FormatIncomingSummary(hideNames bool, sender, itemName string, itemCount int, sizeText string) (string, string) {
	if hideNames {
		if itemCount > 1 {
			return "Incoming transfer request", fmt.Sprintf("%d files (%s) waiting for your approval", itemCount, sizeText)
		}
		return "Incoming file transfer", fmt.Sprintf("1 file (%s) waiting for your approval", sizeText)
	}
	src := SanitizeText(sender, 40)
	if src == "" {
		src = "Connected device"
	}
	name := SanitizeText(itemName, 64)
	if name == "" {
		name = "files"
	}
	if itemCount > 1 {
		return fmt.Sprintf("Incoming transfer from %s", src), fmt.Sprintf("%s (+%d more, %s)", name, itemCount-1, sizeText)
	}
	return fmt.Sprintf("Incoming file from %s", src), fmt.Sprintf("%s (%s)", name, sizeText)
}

// FormatCompletedSummary builds privacy-aware notification text for completed transfers.
// Direction is from the phone's point of view: "download" means the phone finished
// downloading a file shared from the desktop, so the text stays desktop-centric.
func FormatCompletedSummary(hideNames bool, direction, itemName string, itemCount int) (string, string) {
	if hideNames {
		if direction == "download" {
			return "Transfer completed", "Shared file finished downloading"
		}
		if itemCount > 1 {
			return "Transfer received", fmt.Sprintf("%d files saved to Flitnix folder", itemCount)
		}
		return "File received", "1 file saved to Flitnix folder"
	}
	name := SanitizeText(itemName, 64)
	if name == "" {
		name = "transfer"
	}
	if direction == "download" {
		return "Transfer completed", fmt.Sprintf("%s was shared successfully", name)
	}
	if itemCount > 1 {
		return "Files received", fmt.Sprintf("%s (+%d more) saved", name, itemCount-1)
	}
	return "File received", fmt.Sprintf("%s saved", name)
}
