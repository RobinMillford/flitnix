package transfer

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeNameAcceptsOrdinaryNames(t *testing.T) {
	cases := []string{
		"photo.jpg",
		"spaces in name.txt",
		"dot.separated.name.md",
		"ключ-безопасности.pdf",
		"日本語のファイル名.png",
		"emoji-file-🙂.webm",
		".hidden",
		"UPPER_and_lower.TXT",
		strings.Repeat("a", MaxFilenameBytes), // exactly at the byte bound
	}
	for _, name := range cases {
		got, err := SanitizeName(name)
		if err != nil {
			t.Errorf("SanitizeName(%q) failed: %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("SanitizeName(%q) = %q, want unchanged", name, got)
		}
	}
}

func TestSanitizeNameTrimsSurroundingWhitespace(t *testing.T) {
	got, err := SanitizeName("  photo.jpg  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "photo.jpg" {
		t.Errorf("got %q, want \"photo.jpg\"", got)
	}
}

func TestSanitizeNameRejects(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"only whitespace":  "   \t   ",
		"dot":              ".",
		"dotdot":           "..",
		"dot slash dotdot": "photos/../../../etc/passwd",
		"absolute":         "/etc/passwd",
		"homedir":          "~/notes.txt",
		"windows sep":      `defense\in\depth.txt`,
		"trailing sep":     "photo.jpg/",
		"nul":              "photo\x00.jpg",
		"newline":          "photo\n.jpg",
		"carriage return":  "photo\r.jpg",
		"tab":              "pho\tbo.jpg",
		"control byte":     "ph\x01oto.jpg",
		"invalid utf8":     "ph\xff\xfeoto.jpg",
		"too long":         strings.Repeat("a", MaxFilenameBytes+1),
	}
	for label, name := range cases {
		if _, err := SanitizeName(name); !errors.Is(err, ErrInvalidFilename) {
			t.Errorf("%s: SanitizeName(%q) err = %v, want ErrInvalidFilename", label, name, err)
		}
	}
}

func TestSanitizeNameErrorIsPrecise(t *testing.T) {
	if _, err := SanitizeName(""); !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty-name error text %q does not mention emptiness", err)
	}
	if _, err := SanitizeName("/x"); !strings.Contains(err.Error(), "separator") {
		t.Errorf("separator error text %q does not mention separators", err)
	}
}

func TestSplitExt(t *testing.T) {
	cases := []struct{ in, stem, ext string }{
		{"photo.jpg", "photo", ".jpg"},
		{"photo", "photo", ""},
		{".hidden", ".hidden", ""},
		{"a.b.c", "a.b", ".c"},
		{"archive.tar.gz", "archive.tar", ".gz"},
		{"日本語.png", "日本語", ".png"},
	}
	for _, c := range cases {
		stem, ext := splitExt(c.in)
		if stem != c.stem || ext != c.ext {
			t.Errorf("splitExt(%q) = (%q, %q), want (%q, %q)", c.in, stem, ext, c.stem, c.ext)
		}
	}
}

func TestVariantName(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want string
	}{
		{"photo.jpg", 0, "photo.jpg"},
		{"photo.jpg", 1, "photo (1).jpg"},
		{"photo.jpg", 42, "photo (42).jpg"},
		{"photo", 1, "photo (1)"},
		{".hidden", 1, ".hidden (1)"},
		{"a.b.c", 2, "a.b (2).c"},
	}
	for _, c := range cases {
		if got := variantName(c.name, c.n); got != c.want {
			t.Errorf("variantName(%q, %d) = %q, want %q", c.name, c.n, got, c.want)
		}
	}
}
