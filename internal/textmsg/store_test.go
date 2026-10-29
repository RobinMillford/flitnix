package textmsg

import (
	"strings"
	"testing"
)

func TestStoreMessageOperations(t *testing.T) {
	s := NewStore()
	sessKey := "test-session-key"

	// 1. Add valid desktop message
	msg1, err := s.Add(sessKey, "desktop", "Hello from desktop!")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if msg1.Sender != "desktop" || msg1.Content != "Hello from desktop!" || msg1.IsLink {
		t.Fatalf("unexpected message: %+v", msg1)
	}

	// 2. Add valid phone URL
	msg2, err := s.Add(sessKey, "phone", "https://github.com/RobinMillford/flitnix")
	if err != nil {
		t.Fatalf("Add URL: %v", err)
	}
	if !msg2.IsLink || msg2.URL != "https://github.com/RobinMillford/flitnix" {
		t.Fatalf("expected safe link, got %+v", msg2)
	}

	// 3. List messages
	msgs := s.List(sessKey)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	// 4. Test validation errors
	_, err = s.Add(sessKey, "invalid-sender", "text")
	if err != ErrInvalidSender {
		t.Fatalf("expected ErrInvalidSender, got %v", err)
	}

	_, err = s.Add(sessKey, "desktop", "   ")
	if err != ErrEmptyContent {
		t.Fatalf("expected ErrEmptyContent, got %v", err)
	}

	tooLarge := strings.Repeat("A", MaxContentBytes+1)
	_, err = s.Add(sessKey, "desktop", tooLarge)
	if err != ErrContentTooLarge {
		t.Fatalf("expected ErrContentTooLarge, got %v", err)
	}

	// 5. Clear session
	s.Clear(sessKey)
	if len(s.List(sessKey)) != 0 {
		t.Fatal("expected messages to be cleared")
	}
}

func TestIsSafeURL(t *testing.T) {
	cases := []struct {
		in     string
		isLink bool
		want   string
	}{
		{"https://flitnix.org", true, "https://flitnix.org"},
		{"http://192.168.1.5:43821/photo.jpg", true, "http://192.168.1.5:43821/photo.jpg"},
		{"javascript:alert(1)", false, ""},
		{"data:text/html,bad", false, ""},
		{"file:///etc/passwd", false, ""},
		{"not a link at all", false, ""},
		{"https://site.com with trailing text", false, ""},
	}

	for _, tc := range cases {
		isL, u := IsSafeURL(tc.in)
		if isL != tc.isLink || u != tc.want {
			t.Errorf("IsSafeURL(%q) = (%v, %q); want (%v, %q)", tc.in, isL, u, tc.isLink, tc.want)
		}
	}
}
