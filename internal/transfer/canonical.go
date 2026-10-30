package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

var (
	ErrInvalidDecimalSize = errors.New("canonical: invalid decimal size string (must match ^[0-9]+$ with no leading zeros unless '0')")
	sizeRegex             = regexp.MustCompile(`^[0-9]+$`)
)

// ValidateDecimalSize verifies that a size string complies with RFC 8785 precision rules:
// - Non-empty string of ASCII digits [0-9]
// - No leading zeros unless the string is exactly "0"
func ValidateDecimalSize(sizeStr string) error {
	if !sizeRegex.MatchString(sizeStr) {
		return ErrInvalidDecimalSize
	}
	if len(sizeStr) > 1 && sizeStr[0] == '0' {
		return ErrInvalidDecimalSize
	}
	return nil
}

// CanonicalManifestItem represents an item within a canonical RFC 8785 transfer manifest.
type CanonicalManifestItem struct {
	DisplayName  string `json:"displayName"`
	RelativePath string `json:"relativePath"`
	Sequence     int    `json:"sequence"`
	SHA256       string `json:"sha256"`
	Size         string `json:"size"` // Decimal integer string
}

// CanonicalManifest represents the complete RFC 8785 transfer manifest structure.
type CanonicalManifest struct {
	Items    []CanonicalManifestItem `json:"items"`
	Kind     string                  `json:"kind"`
	RootName string                  `json:"rootName"`
}

// UTF16Less compares two strings by UTF-16 code units as required by RFC 8785 Section 3.2.3.
func UTF16Less(a, b string) bool {
	u16a := utf16.Encode([]rune(a))
	u16b := utf16.Encode([]rune(b))
	minLen := len(u16a)
	if len(u16b) < minLen {
		minLen = len(u16b)
	}
	for i := 0; i < minLen; i++ {
		if u16a[i] != u16b[i] {
			return u16a[i] < u16b[i]
		}
	}
	return len(u16a) < len(u16b)
}

// CanonicalizeString escapes a string according to RFC 8785 Section 3.2.2.2:
// - Quotation mark (") -> \"
// - Reverse solidus (\) -> \\
// - Control characters (U+0000..U+001F) -> \u00XX (or \b, \f, \n, \r, \t)
// - Forward slash (/) is NOT escaped.
func CanonicalizeString(s string) string {
	var buf bytes.Buffer
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				buf.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				buf.WriteString(s[i : i+size])
			}
		}
		i += size
	}
	buf.WriteByte('"')
	return buf.String()
}

// CanonicalizeJSON serializes arbitrary JSON data into RFC 8785 Canonical JSON bytes.
func CanonicalizeJSON(data []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical: json decode error: %w", err)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		buf.WriteString(CanonicalizeString(val))
	case json.Number:
		buf.WriteString(val.String())
	case map[string]any:
		buf.WriteByte('{')
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return UTF16Less(keys[i], keys[j])
		})
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(CanonicalizeString(k))
			buf.WriteByte(':')
			if err := writeCanonical(buf, val[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, elem := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, elem); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		return fmt.Errorf("canonical: unsupported JSON type: %T", v)
	}
	return nil
}

// CanonicalizeManifest builds deterministic, RFC 8785 JCS bytes from a CanonicalManifest.
// Items are ordered by Sequence ascending. Size strings are validated.
func CanonicalizeManifest(m *CanonicalManifest) ([]byte, error) {
	if m == nil {
		return nil, errors.New("canonical: nil manifest")
	}

	// Validate items and sort by sequence ascending
	items := make([]CanonicalManifestItem, len(m.Items))
	copy(items, m.Items)
	for _, item := range items {
		if err := ValidateDecimalSize(item.Size); err != nil {
			return nil, fmt.Errorf("canonical: invalid size for %q: %w", item.DisplayName, err)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Sequence < items[j].Sequence
	})

	// Build raw JSON
	data, err := json.Marshal(map[string]any{
		"items":    items,
		"kind":     m.Kind,
		"rootName": m.RootName,
	})
	if err != nil {
		return nil, err
	}

	return CanonicalizeJSON(data)
}

// CanonicalManifestFromBatch converts a transfer.BatchManifest and per-item SHA-256 hashes
// into a CanonicalManifest ready for RFC 8785 canonicalization.
func CanonicalManifestFromBatch(manifest BatchManifest, itemHashes map[int]string) (*CanonicalManifest, error) {
	cItems := make([]CanonicalManifestItem, 0, len(manifest.Items))
	for i, item := range manifest.Items {
		seq := item.Sequence
		if seq <= 0 {
			seq = i + 1
		}
		hash := item.SHA256
		if hash == "" && itemHashes != nil {
			hash = itemHashes[seq]
		}
		sizeStr := strconv.FormatInt(item.Size, 10)
		if err := ValidateDecimalSize(sizeStr); err != nil {
			return nil, fmt.Errorf("canonical: invalid item size %d: %w", item.Size, err)
		}
		cItems = append(cItems, CanonicalManifestItem{
			DisplayName:  item.DisplayName,
			RelativePath: item.RelativePath,
			Sequence:     seq,
			SHA256:       hash,
			Size:         sizeStr,
		})
	}

	return &CanonicalManifest{
		Items:    cItems,
		Kind:     string(manifest.Kind),
		RootName: manifest.RootName,
	}, nil
}

// ComputeManifestSHA256 returns the canonical RFC 8785 bytes and the 64-character hex SHA-256 digest.
func ComputeManifestSHA256(m *CanonicalManifest) ([]byte, string, error) {
	canonicalBytes, err := CanonicalizeManifest(m)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(canonicalBytes)
	return canonicalBytes, hex.EncodeToString(digest[:]), nil
}
