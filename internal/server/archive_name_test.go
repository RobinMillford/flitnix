package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestEveryListedSourceCanBeDownloaded pins the availability contract between
// the phone's source list and the grant endpoint: a name the share path
// accepts must still produce a downloadable plan. SanitizeName (the registry's
// rule, and a legal Linux/macOS filename) admits ':' while the archive name
// validator used to reject it, so such a source appeared in /api/sources and
// then failed every download with an internal "invalid archive entry name"
// error.
func TestEveryListedSourceCanBeDownloaded(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	base := t.TempDir()

	names := []string{
		"plain.txt",
		"with space.txt",
		"notes:2024.txt",
		"12:30 sync.txt",
		`quote".txt`,
		"hash#tag.txt",
		"q?mark.txt",
		"semi;colon.txt",
		"100%.txt",
		"résumé 日本 ☃.txt",
	}
	for _, name := range names {
		path := filepath.Join(base, name)
		body := "body of " + name
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sf, err := env.reg.Add(path, name, "")
		if err != nil {
			t.Fatalf("%s: the share path refused a legal filename: %v", name, err)
		}

		g, cookie := env.grantFor(tok, sf.ID)
		if g.Archive {
			t.Fatalf("%s: a single file was planned as an archive", name)
		}
		rec := env.getDownload(g, cookie, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: listed source is not downloadable: %d %s", name, rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != body {
			t.Fatalf("%s: downloaded %q, want %q", name, got, body)
		}
		if got := dispositionFilename(t, rec.Header().Get("Content-Disposition")); got != name {
			t.Errorf("%s: browser save name = %q, want the source name unchanged", name, got)
		}
	}
}

// TestFolderRootWithAColonIsArchivable covers the folder half of the same
// contract: the root name comes from the selected directory's basename, which
// SanitizeName accepts with a colon.
func TestFolderRootWithAColonIsArchivable(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	root := filepath.Join(t.TempDir(), "Season:1")
	writeTree(t, root, map[string]string{"readme.txt": "hello", "nested/a.txt": "A"})
	source, err := env.reg.AddFolder(root, "")
	if err != nil {
		t.Fatalf("AddFolder refused a legal folder name: %v", err)
	}

	g, cookie := env.grantFor(tok, source.ID)
	if !g.Archive || g.Filename != "Season:1.zip" {
		t.Fatalf("grant = %+v, want an archive named Season:1.zip", g)
	}
	rec := env.getDownload(g, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive download = %d %s", rec.Code, rec.Body.String())
	}
	entries := readZip(t, rec.Body.Bytes())
	want := map[string]string{
		"Season:1/readme.txt":   "hello",
		"Season:1/nested/a.txt": "A",
	}
	if !sameStringMap(entries, want) {
		t.Fatalf("archive entries = %v, want %v", entries, want)
	}
}
