package notify

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type mockBackend struct {
	mu        sync.Mutex
	supported bool
	sent      []dispatchedNotification
	closed    bool
	notifyCh  chan dispatchedNotification
}

func (m *mockBackend) Supported() bool { return m.supported }
func (m *mockBackend) Send(n dispatchedNotification) error {
	m.mu.Lock()
	m.sent = append(m.sent, n)
	m.mu.Unlock()
	if m.notifyCh != nil {
		select {
		case m.notifyCh <- n:
		default:
		}
	}
	return nil
}
func (m *mockBackend) Close() { m.closed = true }

func TestSanitizeAndFormatPrivacy(t *testing.T) {
	dirty := "evil\x00\u202Egpj.exe\n\t<b>bold</b>"
	clean := SanitizeText(dirty, 80)
	if strings.ContainsRune(clean, '\u202E') || strings.ContainsRune(clean, '\x00') || strings.ContainsRune(clean, '\n') {
		t.Fatalf("unsafe characters leaked through SanitizeText: %q", clean)
	}
	if escaped := EscapeMarkup(clean); strings.Contains(escaped, "<b>") {
		t.Fatalf("markup was not escaped: %q", escaped)
	}

	title, body := FormatIncomingSummary(true, "My Phone", "secret_tax_2025.pdf", 2, "4.2 MB")
	if strings.Contains(title, "My Phone") || strings.Contains(body, "secret_tax_2025.pdf") {
		t.Fatalf("privacy mask failed to redact names: title=%q body=%q", title, body)
	}
}

func TestFormatCompletedSummaryWording(t *testing.T) {
	title, body := FormatCompletedSummary(false, "download", "report.pdf", 1)
	if title != "Transfer completed" {
		t.Fatalf("outgoing share title = %q", title)
	}
	if strings.Contains(title, "Download") || strings.Contains(body, "Download") || strings.Contains(body, "downloaded") {
		t.Fatalf("outgoing share notification is phone-centric: %q / %q", title, body)
	}
	if !strings.Contains(body, "report.pdf") || !strings.Contains(body, "shared") {
		t.Fatalf("unexpected outgoing share body: %q", body)
	}

	hiddenTitle, hiddenBody := FormatCompletedSummary(true, "download", "secret.pdf", 1)
	if hiddenTitle != "Transfer completed" || strings.Contains(hiddenBody, "secret.pdf") {
		t.Fatalf("privacy mask or wording failed: %q / %q", hiddenTitle, hiddenBody)
	}

	if inTitle, _ := FormatCompletedSummary(false, "upload", "photo.jpg", 1); inTitle != "File received" {
		t.Fatalf("incoming transfer wording changed: %q", inTitle)
	}
}

func TestManagerPolicyQuietModeDedupAndTokenSecurity(t *testing.T) {
	b := &mockBackend{supported: true, notifyCh: make(chan dispatchedNotification, 8)}
	var acts []ActionInvocation
	var actMu sync.Mutex

	mgr := newManagerWithBackend(DefaultPolicy(), b, func(inv ActionInvocation) {
		actMu.Lock()
		acts = append(acts, inv)
		actMu.Unlock()
	})
	defer mgr.Close()

	mgr.Notify(Notification{
		Key:      "batch:b1",
		Category: CatIncomingTransfer,
		Title:    "Incoming file",
		Body:     "photo.jpg",
		DefaultAction: Action{
			Kind: ActionOpenWindow,
		},
		Actions: []Action{
			{Kind: ActionApproveBatch, TargetID: "b1", Label: "Approve"},
			{Kind: ActionDeclineBatch, TargetID: "b1", Label: "Decline"},
		},
	})

	// Duplicate same-key within cooldown should be dropped cleanly
	mgr.Notify(Notification{
		Key:      "batch:b1",
		Category: CatIncomingTransfer,
		Title:    "Incoming file",
		Body:     "photo.jpg",
	})

	var dispatched dispatchedNotification
	select {
	case dispatched = <-b.notifyCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for dispatched notification")
	}

	if len(dispatched.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(dispatched.Actions))
	}

	// Forged token fails closed
	if mgr.InvokeToken("forged-token-123") {
		t.Fatal("expected forged token to fail")
	}

	// Valid approval token runs once and cannot be replayed
	approveTok := dispatched.Actions[0].Token
	if !mgr.InvokeToken(approveTok) {
		t.Fatal("expected valid approve token to succeed")
	}
	if mgr.InvokeToken(approveTok) {
		t.Fatal("expected single-use approve token replay to be rejected")
	}

	// Quiet mode suppresses further notifications
	p := mgr.Policy()
	p.QuietMode = true
	mgr.SetPolicy(p)
	mgr.Notify(Notification{
		Key:      "batch:b2",
		Category: CatIncomingTransfer,
		Title:    "Suppressed",
		Body:     "doc.pdf",
	})

	select {
	case extra := <-b.notifyCh:
		t.Fatalf("did not expect notification during quiet mode, got %+v", extra)
	case <-time.After(40 * time.Millisecond):
	}

	actMu.Lock()
	defer actMu.Unlock()
	if len(acts) != 1 || acts[0].Kind != ActionApproveBatch || acts[0].TargetID != "b1" {
		t.Fatalf("unexpected action invocations: %+v", acts)
	}
}
