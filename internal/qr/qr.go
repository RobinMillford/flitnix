// Package qr renders the desktop's pairing QR code.
//
// It wraps a small, maintained Go QR library and exists so the rest of the
// application stays QR-agnostic: the adapter hands it a URL plus a pixel
// size and receives PNG bytes it can base64-embed into the desktop UI. No
// asset is fetched at runtime; the library is compiled in, so scanning
// never depends on internet access.
//
// The payload contract lives here too: the session token rides the URL
// fragment (never the query string, never a diagnostic log line), which is
// how the phone page picks it up without the server seeing it in URLs.
package qr

import (
	"errors"
	"fmt"
	"strings"

	"github.com/skip2/go-qrcode"
)

// DefaultSize is the QR image size the desktop uses, in pixels.
const DefaultSize = 288

// MinSize/MaxSize bound the requested PNG size.
const (
	MinSize = 128
	MaxSize = 1024
)

var (
	// ErrPayload means the base URL or token cannot form a QR payload.
	ErrPayload = errors.New("qr: cannot build the session payload")

	// ErrSize means the requested pixel size is out of bounds.
	ErrSize = errors.New("qr: requested QR size is out of bounds")
)

// Payload joins the reachable server URL with the session token as the
// QR text. The server URL must already be complete (scheme, host and
// port); the token only ever appears in the fragment, so the phone reads
// it locally and repeats it in Authorization headers.
func Payload(baseURL, token string) (string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", fmt.Errorf("%w: the server URL is empty", ErrPayload)
	}
	if strings.ContainsAny(baseURL, " #?") {
		return "", fmt.Errorf("%w: the server URL must be plain scheme://host:port without fragments or queries", ErrPayload)
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("%w: the session token is empty", ErrPayload)
	}
	for _, c := range token {
		if c == '#' || c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			return "", fmt.Errorf("%w: the session token must be URL-safe", ErrPayload)
		}
	}
	return baseURL + "/#" + token, nil
}

// PNG renders payload as a square QR PNG of the requested size.
func PNG(payload string, size int) ([]byte, error) {
	if size < MinSize || size > MaxSize {
		return nil, fmt.Errorf("%w: %d must be between %d and %d px", ErrSize, size, MinSize, MaxSize)
	}
	code, err := qrcode.New(payload, qrcode.Low)
	if err != nil {
		return nil, fmt.Errorf("qr: encoding payload: %w", err)
	}
	code.DisableBorder = false
	png, err := code.PNG(size)
	if err != nil {
		return nil, fmt.Errorf("qr: rendering PNG: %w", err)
	}
	return png, nil
}

// Terminal renders payload as an ASCII/Unicode string suitable for terminal display.
func Terminal(payload string) (string, error) {
	code, err := qrcode.New(payload, qrcode.Low)
	if err != nil {
		return "", fmt.Errorf("qr: encoding payload: %w", err)
	}
	code.DisableBorder = false
	return code.ToSmallString(false), nil
}
