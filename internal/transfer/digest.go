package transfer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

var (
	ErrInvalidDigestHeader   = errors.New("transfer: invalid Content-Digest header")
	ErrUnsupportedDigestAlgo = errors.New("transfer: unsupported digest algorithm (only sha-256 is supported)")
)

const digestCopyBufferSize = 64 * 1024

// FormatContentDigest formats a 64-character hex-encoded SHA-256 digest into
// a standard RFC 9530 Content-Digest header value using Structured Fields:
//
//	Content-Digest: sha-256=:BASE64_ENCODED_SHA256:
//
// If the input hex is invalid or not 32 bytes (64 hex characters), it returns an empty string.
func FormatContentDigest(sha256Hex string) string {
	clean := strings.TrimSpace(sha256Hex)
	if len(clean) != 64 {
		return ""
	}
	raw, err := hex.DecodeString(clean)
	if err != nil || len(raw) != sha256.Size {
		return ""
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	return "sha-256=:" + b64 + ":"
}

// ParseContentDigest parses an RFC 9530 Content-Digest header value and returns
// the extracted SHA-256 digest as a lowercase 64-character hex string.
//
// It accepts formats conforming to RFC 9530, e.g.:
//
//	sha-256=:X48E9qOokqqrvdts8nOJRJN3OWDUoyWxBf7kbu9DBPE=:
//
// If legacy Digest header format (e.g. "SHA-256=<base64>") is encountered, it
// is also handled as a fallback.
func ParseContentDigest(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", ErrInvalidDigestHeader
	}

	// Case 1: RFC 9530 Structured Field: sha-256=:BASE64:
	lower := strings.ToLower(header)
	if strings.Contains(lower, "sha-256=:") {
		idx := strings.Index(lower, "sha-256=:")
		sub := header[idx+len("sha-256=:"):]
		colonEnd := strings.Index(sub, ":")
		if colonEnd == -1 {
			return "", ErrInvalidDigestHeader
		}
		b64 := sub[:colonEnd]
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(raw) != sha256.Size {
			return "", fmt.Errorf("%w: invalid base64 byte sequence", ErrInvalidDigestHeader)
		}
		return hex.EncodeToString(raw), nil
	}

	// Case 2: Legacy RFC 3230/5843 Digest fallback: SHA-256=<base64>
	if strings.Contains(lower, "sha-256=") {
		idx := strings.Index(lower, "sha-256=")
		sub := header[idx+len("sha-256="):]
		end := strings.IndexAny(sub, ", ;")
		if end != -1 {
			sub = sub[:end]
		}
		sub = strings.Trim(sub, `"`)
		raw, err := base64.StdEncoding.DecodeString(sub)
		if err != nil || len(raw) != sha256.Size {
			return "", fmt.Errorf("%w: invalid legacy base64 digest", ErrInvalidDigestHeader)
		}
		return hex.EncodeToString(raw), nil
	}

	return "", ErrUnsupportedDigestAlgo
}

// ComputeReaderSHA256 streams r through sha256.New() in bounded 64 KB chunks,
// returning the resulting 64-char hex digest and total bytes read without buffering
// the stream in memory.
func ComputeReaderSHA256(r io.Reader) (string, int64, error) {
	hasher := sha256.New()
	bufPtr := GetCopyBuffer()
	defer PutCopyBuffer(bufPtr)
	buf := *bufPtr
	n, err := io.CopyBuffer(hasher, r, buf)
	if err != nil {
		return "", n, fmt.Errorf("transfer: error streaming for digest: %w", err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), n, nil
}

// ComputeFileSHA256 opens the regular file at path and calculates its SHA-256 digest
// using bounded 64 KB chunks. It verifies the file is regular and returns its hex digest
// and size in bytes.
func ComputeFileSHA256(path string) (string, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, fmt.Errorf("transfer: stat file for digest: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("transfer: cannot digest non-regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("transfer: open file for digest: %w", err)
	}
	defer f.Close()

	digest, n, err := ComputeReaderSHA256(f)
	if err != nil {
		return "", n, err
	}
	if n != info.Size() {
		return "", n, fmt.Errorf("transfer: file size changed during digest calculation (%d -> %d)", info.Size(), n)
	}
	return digest, n, nil
}
