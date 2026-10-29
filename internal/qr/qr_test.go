package qr

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestPayload(t *testing.T) {
	got, err := Payload("http://192.168.1.42:43821", "tok_v43")
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}
	if want := "http://192.168.1.42:43821/#tok_v43"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPayloadRejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name, base, token string
	}{
		{"empty base", "", "tok"},
		{"base with fragment", "http://a:1/#x", "tok"},
		{"base with query", "http://a:1/?q", "tok"},
		{"base with space", "http://a b:1", "tok"},
		{"empty token", "http://a:43821", ""},
		{"token with fragment", "http://a:43821", "a#b"},
		{"token with newline", "http://a:43821", "a\nb"},
	}
	for _, tc := range cases {
		if _, err := Payload(tc.base, tc.token); !errors.Is(err, ErrPayload) {
			t.Errorf("%s: err = %v, want ErrPayload", tc.name, err)
		}
	}
}

func TestPNG(t *testing.T) {
	payload, err := Payload("http://192.168.1.42:43821", "tok_v43")
	if err != nil {
		t.Fatalf("Payload: %v", err)
	}

	png, err := PNG(payload, DefaultSize)
	if err != nil {
		t.Fatalf("PNG: %v", err)
	}
	// PNG magic + IHDR chunk signature; cheap structural sanity without
	// pulling a decoder into the contract.
	if !bytes.HasPrefix(png, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		t.Fatal("output is not a PNG")
	}
	if len(png) < 200 {
		t.Fatalf("PNG suspiciously small (%d bytes)", len(png))
	}
	// Width/height live in the IHDR big-endian bytes: 8..15.
	w := int32(uint(png[16])<<24 | uint(png[17])<<16 | uint(png[18])<<8 | uint(png[19]))
	h := int32(uint(png[20])<<24 | uint(png[21])<<16 | uint(png[22])<<8 | uint(png[23]))
	if w != DefaultSize || h != DefaultSize {
		t.Fatalf("PNG dimensions %dx%d, want %d×%d", w, h, DefaultSize, DefaultSize)
	}

	small, err := PNG(payload, MinSize)
	if err != nil {
		t.Fatalf("PNG at min size: %v", err)
	}
	if len(small) == 0 || bytes.Equal(small, png) {
		t.Fatal("different sizes should produce different renders")
	}
}

func TestPNGRejectsBadSize(t *testing.T) {
	for _, size := range []int{0, -1, MinSize - 1, MaxSize + 1} {
		if _, err := PNG("http://a:1/#t", size); !errors.Is(err, ErrSize) {
			t.Errorf("size %d: err = %v, want ErrSize", size, err)
		}
	}
}

func TestPNGRejectsImpossiblePayload(t *testing.T) {
	// A payload longer than the QR capacity must surface as an encoding
	// error rather than a silent failure.
	payload := strings.Repeat("a", 10_000)
	if _, err := PNG(payload, DefaultSize); err == nil {
		t.Fatal("oversized payload must fail to encode")
	}
}

func TestTerminal(t *testing.T) {
	out, err := Terminal("http://192.168.1.50:43821/#abc123token")
	if err != nil {
		t.Fatalf("Terminal failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Terminal output is empty")
	}
}
