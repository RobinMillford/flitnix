package server

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// portableSourceNames are accepted by every supported host filesystem.
var portableSourceNames = []string{
	"plain.txt",
	"with space.txt",
	"hash#tag.txt",
	"semi;colon.txt",
	"100%.txt",
	"résumé 日本 ☃.txt",
}

// hostInvalidSourceNames are legal Linux/macOS filenames that a Windows
// filesystem cannot store (':' '?' and '"' are reserved there). They are still
// part of the availability contract, so the rule they exercise is tested
// directly — without a filesystem fixture — in
// TestShareableNamesStayArchivable.
var hostInvalidSourceNames = []string{
	"notes:2024.txt",
	"12:30 sync.txt",
	`quote".txt`,
	"q?mark.txt",
}

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

	names := append([]string(nil), portableSourceNames...)
	if runtime.GOOS != "windows" {
		// Windows cannot create these entries at all; the name rule itself is
		// covered by TestShareableNamesStayArchivable.
		names = append(names, hostInvalidSourceNames...)
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

// TestShareableNamesStayArchivable isolates the archive-name rule from host
// filesystem restrictions: every name the share path accepts must also be a
// valid archive entry name, including characters Windows cannot store. The
// archive writer only reads such a file on a platform that can hold it, but
// the rule itself must never reject a name the share path admitted.
func TestShareableNamesStayArchivable(t *testing.T) {
	names := append(append([]string(nil), portableSourceNames...), hostInvalidSourceNames...)
	for _, name := range names {
		if _, err := transfer.SanitizeName(name); err != nil {
			t.Fatalf("%s: the share path refused a legal filename: %v", name, err)
		}
		if err := validateArchiveName(name); err != nil {
			t.Errorf("%s: archive entry name rejected: %v", name, err)
		}
		if _, err := safeArchivePath("root/" + name); err != nil {
			t.Errorf("%s: archive entry path rejected: %v", name, err)
		}
	}
}

// TestFolderRootWithAColonIsArchivable covers the folder half of the same
// contract: the root name comes from the selected directory's basename, which
// SanitizeName accepts with a colon. A Windows filesystem cannot hold such a
// directory, so the fixture uses a portable root there and the colon rule is
// pinned by TestFolderRootNameWithAColonIsArchivable.
func TestFolderRootWithAColonIsArchivable(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	rootName := "Season:1"
	if runtime.GOOS == "windows" {
		rootName = "Season 1"
	}
	root := filepath.Join(t.TempDir(), rootName)
	writeTree(t, root, map[string]string{"readme.txt": "hello", "nested/a.txt": "A"})
	source, err := env.reg.AddFolder(root, "")
	if err != nil {
		t.Fatalf("AddFolder refused a legal folder name: %v", err)
	}

	g, cookie := env.grantFor(tok, source.ID)
	if !g.Archive || g.Filename != rootName+".zip" {
		t.Fatalf("grant = %+v, want an archive named %s.zip", g, rootName)
	}
	rec := env.getDownload(g, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive download = %d %s", rec.Code, rec.Body.String())
	}
	entries := readZip(t, rec.Body.Bytes())
	want := map[string]string{
		rootName + "/readme.txt":   "hello",
		rootName + "/nested/a.txt": "A",
	}
	if !sameStringMap(entries, want) {
		t.Fatalf("archive entries = %v, want %v", entries, want)
	}
}

// TestFolderRootNameWithAColonIsArchivable pins the folder-root name rule
// without a filesystem fixture, so it holds on every host: a root the share
// path accepts keeps its exact name in the archive and in the .zip save name.
func TestFolderRootNameWithAColonIsArchivable(t *testing.T) {
	const rootName = "Season:1"
	if _, err := transfer.SanitizeName(rootName); err != nil {
		t.Fatalf("the share path refused the folder name: %v", err)
	}
	if err := validateArchiveName(rootName + "/readme.txt"); err != nil {
		t.Fatalf("archive entry rejected a legal folder root: %v", err)
	}
	if got, want := zipFilename(rootName), "Season:1.zip"; got != want {
		t.Fatalf("zipFilename(%q) = %q, want %q", rootName, got, want)
	}
}
