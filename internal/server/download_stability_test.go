package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/transfer"
)

// ---- helpers ----

func (e *testEnv) grantFor(tok string, ids ...string) (createGrantResponse, *http.Cookie) {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"sourceIds": ids})
	rec := e.do(http.MethodPost, "/api/download-grants", tok, body)
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("create grant = %d %s", rec.Code, rec.Body.String())
	}
	var g createGrantResponse
	decodeBody(e.t, rec, &g)
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		e.t.Fatalf("grant response set %d cookies, want 1", len(cookies))
	}
	return g, cookies[0]
}

func (e *testEnv) getDownload(g createGrantResponse, c *http.Cookie, mutate func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, g.DownloadURL, nil)
	if c != nil {
		req.AddCookie(c)
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *testEnv) progress(tok, id string) (int, map[string]any) {
	e.t.Helper()
	rec := e.do(http.MethodGet, "/api/download-progress/"+id, tok, nil)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// dispositionFilename parses a Content-Disposition header the way a browser
// does and returns the effective filename.
func dispositionFilename(t *testing.T, header string) string {
	t.Helper()
	disp, params, err := mime.ParseMediaType(header)
	if err != nil || disp != "attachment" {
		t.Fatalf("Content-Disposition %q is not a valid attachment header: disp=%q err=%v", header, disp, err)
	}
	return params["filename"]
}

// readZip opens the bytes as a ZIP, reads every entry completely (which
// verifies each CRC) and returns name -> content ("" for directories).
func readZip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("response is not a valid ZIP (central directory unreadable): %v", err)
	}
	out := make(map[string]string, len(zr.File))
	for _, f := range zr.File {
		if f.Method != zip.Store {
			t.Errorf("entry %q uses method %d, want Store", f.Name, f.Method)
		}
		if strings.HasSuffix(f.Name, "/") {
			out[f.Name] = ""
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %q: %v", f.Name, err)
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read %q (CRC/size check): %v", f.Name, err)
		}
		out[f.Name] = string(content)
	}
	return out
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// ---- filenames ----

func TestContentDispositionIsSafeAndRoundTrips(t *testing.T) {
	cases := []struct{ name, want string }{
		{"report.pdf", "report.pdf"},
		{"Animals season 1 part 1.zip", "Animals season 1 part 1.zip"},
		{"résumé 日本 ☃.zip", "résumé 日本 ☃.zip"},
		{`quo"te; semi.txt`, `quo"te; semi.txt`},
		{"100% real.txt", "100% real.txt"},
	}
	for _, tc := range cases {
		header := contentDisposition(tc.name)
		if strings.ContainsAny(header, "\r\n") {
			t.Fatalf("%q produced a header with a line break: %q", tc.name, header)
		}
		if got := dispositionFilename(t, header); got != tc.want {
			t.Errorf("%q: browser sees %q via %q, want %q", tc.name, got, header, tc.want)
		}
	}
	if h := contentDisposition("report.pdf"); h != "attachment; filename=report.pdf" {
		t.Errorf("plain ASCII header = %q", h)
	}
	if h := contentDisposition("Animals season 1 part 1.zip"); h != `attachment; filename="Animals season 1 part 1.zip"` {
		t.Errorf("spaced header = %q", h)
	}
	uni := contentDisposition("été.zip")
	if !strings.Contains(uni, "filename*=UTF-8''%C3%A9t%C3%A9.zip") || !strings.Contains(uni, "filename=_t_.zip;") {
		t.Errorf("unicode header lacks the fallback + filename* pair: %q", uni)
	}
}

func TestContentDispositionNeutralizesHostileNames(t *testing.T) {
	hostile := []string{
		"evil\r\nSet-Cookie: pwn=1",
		"a/../../etc/passwd",
		`..\..\windows\system.ini`,
		"nul\x00byte.txt",
		"   ",
		"..",
		"",
		"bad\xffutf8.bin",
	}
	for _, name := range hostile {
		header := contentDisposition(name)
		if strings.ContainsAny(header, "\r\n\x00") {
			t.Fatalf("%q leaked control characters into %q", name, header)
		}
		got := dispositionFilename(t, header)
		if got == "" || strings.ContainsAny(got, "/\\\r\n\x00") || got == ".." || got == "." {
			t.Errorf("%q yielded unsafe save name %q (header %q)", name, got, header)
		}
	}
}

func TestGrantFilenameComesFromTrustedSourceMetadata(t *testing.T) {
	file := func(n string) archiveSource {
		return archiveSource{Kind: transfer.SourceKindFile, Name: n, File: transfer.SharedFile{Name: n}}
	}
	folder := func(n string) archiveSource { return archiveSource{Kind: transfer.SourceKindFolder, Name: n} }
	long := strings.Repeat("é", 200) // 400 bytes
	cases := []struct {
		name    string
		sources []archiveSource
		want    string
	}{
		{"one file", []archiveSource{file("report.pdf")}, "report.pdf"},
		{"one folder", []archiveSource{folder("Photos")}, "Photos.zip"},
		{"folder with spaces", []archiveSource{folder("Animals season 1 part 1")}, "Animals season 1 part 1.zip"},
		{"two files", []archiveSource{file("a.txt"), file("b.txt")}, defaultArchiveName},
		{"file and folder", []archiveSource{file("a.txt"), folder("Photos")}, defaultArchiveName},
		{"two folders", []archiveSource{folder("A"), folder("B")}, defaultArchiveName},
		{"unicode folder", []archiveSource{folder("日本の旅")}, "日本の旅.zip"},
		{"unusable folder name", []archiveSource{folder("...")}, defaultArchiveName},
	}
	for _, tc := range cases {
		if got := grantFilename(tc.sources); got != tc.want {
			t.Errorf("%s: filename = %q, want %q", tc.name, got, tc.want)
		}
	}
	got := grantFilename([]archiveSource{folder(long)})
	if len(got) > transfer.MaxFilenameBytes || !strings.HasSuffix(got, ".zip") || !strings.HasPrefix(got, "é") {
		t.Errorf("long folder name produced %d-byte filename %q", len(got), got)
	}
	if !strings.HasPrefix(got, strings.Repeat("é", 10)) {
		t.Errorf("truncation split a rune or lost the name: %q", got)
	}
}

// ---- archive contents, filenames and headers over HTTP ----

func TestFolderArchiveHasFolderNameAndExactContents(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	parent := t.TempDir()
	root := filepath.Join(parent, "Animals season 1 part 1")
	writeTree(t, root, map[string]string{
		"episode 01.txt":        "first",
		"extras/été notes.txt":  "unicode name",
		"extras/deep/日本.txt":    "nested",
		"extras/deep/empty.txt": "",
	})
	if err := os.MkdirAll(filepath.Join(root, "recorded empty dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "link-to-outside")); err != nil {
		t.Logf("symlinks unavailable, skipping symlink policy check: %v", err)
	}
	source, err := env.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	g, cookie := env.grantFor(tok, source.ID)
	if g.Filename != "Animals season 1 part 1.zip" || !g.Archive {
		t.Fatalf("grant = %+v, want the folder's own name as a zip", g)
	}
	rec := env.getDownload(g, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d", rec.Code)
	}
	if got := dispositionFilename(t, rec.Header().Get("Content-Disposition")); got != "Animals season 1 part 1.zip" {
		t.Fatalf("response filename = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing hardening headers: %v", rec.Header())
	}
	prefix := "Animals season 1 part 1/"
	want := map[string]string{
		prefix + "episode 01.txt":        "first",
		prefix + "extras/été notes.txt":  "unicode name",
		prefix + "extras/deep/日本.txt":    "nested",
		prefix + "extras/deep/empty.txt": "",
		prefix + "recorded empty dir/":   "",
	}
	got := readZip(t, rec.Body.Bytes())
	if !sameStringMap(got, want) {
		t.Fatalf("archive entries\n got  %v\n want %v", got, want)
	}
	for name := range got {
		if strings.Contains(name, "link-to-outside") {
			t.Fatalf("symlink was followed into the archive: %q", name)
		}
	}
	// Progress ends completed only after the archive is whole.
	code, p := env.progress(tok, g.ProgressID)
	if code != http.StatusOK || p["state"] != "completed" || p["error"] != "" {
		t.Fatalf("progress = %d %v", code, p)
	}
}

func TestMixedSelectionArchiveHandlesNameCollisions(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	dir := t.TempDir()
	one := filepath.Join(dir, "one")
	two := filepath.Join(dir, "two")
	writeTree(t, one, map[string]string{"notes.txt": "from one"})
	writeTree(t, two, map[string]string{"notes.txt": "from two"})
	a, err := env.reg.AddFileSource(filepath.Join(one, "notes.txt"), "notes.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := env.reg.AddFileSource(filepath.Join(two, "notes.txt"), "notes.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	folderRoot := filepath.Join(dir, "Same Name")
	writeTree(t, folderRoot, map[string]string{"x.txt": "in folder"})
	f1, err := env.reg.AddFolder(folderRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other", "Same Name")
	writeTree(t, other, map[string]string{"y.txt": "in other folder"})
	f2, err := env.reg.AddFolder(other, "")
	if err != nil {
		t.Fatal(err)
	}
	g, cookie := env.grantFor(tok, a.ID, b.ID, f1.ID, f2.ID)
	if g.Filename != defaultArchiveName {
		t.Fatalf("multi-source filename = %q, want %q", g.Filename, defaultArchiveName)
	}
	rec := env.getDownload(g, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d", rec.Code)
	}
	if got := dispositionFilename(t, rec.Header().Get("Content-Disposition")); got != defaultArchiveName {
		t.Fatalf("response filename = %q", got)
	}
	want := map[string]string{
		"notes.txt":           "from one",
		"notes (1).txt":       "from two",
		"Same Name/x.txt":     "in folder",
		"Same Name (1)/y.txt": "in other folder",
	}
	if got := readZip(t, rec.Body.Bytes()); !sameStringMap(got, want) {
		t.Fatalf("archive entries\n got  %v\n want %v", got, want)
	}
}

func TestSingleFileDirectDownloadKeepsLengthAndName(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	content := strings.Repeat("payload-", 1000)
	for _, name := range []string{"report.pdf", "Quarterly report final.pdf", "naïve résumé 日本.txt"} {
		sf := env.shareFile(t.TempDir(), name, []byte(content))
		g, cookie := env.grantFor(tok, sf.ID)
		if g.Archive || g.Filename != name {
			t.Fatalf("%q: grant = %+v", name, g)
		}
		rec := env.getDownload(g, cookie, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != content {
			t.Fatalf("%q: download = %d, %d bytes", name, rec.Code, rec.Body.Len())
		}
		if got := dispositionFilename(t, rec.Header().Get("Content-Disposition")); got != name {
			t.Errorf("%q: response filename = %q", name, got)
		}
		if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(len(content)) {
			t.Errorf("%q: Content-Length = %q, want %d (a short body must be detectable)", name, cl, len(content))
		}
		if rec.Header().Get("Content-Type") != "application/octet-stream" {
			t.Errorf("%q: Content-Type = %q", name, rec.Header().Get("Content-Type"))
		}
	}
}

func TestLegacyDirectDownloadUsesSafeDisposition(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	sf := env.shareFile(t.TempDir(), "naïve résumé.txt", []byte("legacy"))
	rec := env.do(http.MethodGet, "/api/files/"+sf.ID+"/download", tok, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "legacy" {
		t.Fatalf("legacy download = %d %q", rec.Code, rec.Body.String())
	}
	if got := dispositionFilename(t, rec.Header().Get("Content-Disposition")); got != "naïve résumé.txt" {
		t.Fatalf("legacy filename = %q", got)
	}
}

func TestBrowserSideNameEditsCannotChangeTheDownload(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	root := filepath.Join(t.TempDir(), "Photos")
	writeTree(t, root, map[string]string{"a.jpg": "AAA", "sub/b.jpg": "BBB"})
	source, err := env.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	other := env.shareFile(t.TempDir(), "secret.txt", []byte("not selected"))
	g, cookie := env.grantFor(tok, source.ID)

	// A browser retry/rename cannot steer the plan with a query string, an
	// echoed Content-Disposition, or a Range header.
	rec := env.getDownload(g, cookie, func(r *http.Request) {
		r.URL.RawQuery = "filename=hacked.zip&source=" + other.ID + "&path=/etc/passwd"
		r.Header.Set("Content-Disposition", `attachment; filename="renamed-by-user.zip"`)
		r.Header.Set("Range", "bytes=10-")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d", rec.Code)
	}
	if got := dispositionFilename(t, rec.Header().Get("Content-Disposition")); got != "Photos.zip" {
		t.Fatalf("filename followed the client: %q", got)
	}
	want := map[string]string{"Photos/a.jpg": "AAA", "Photos/sub/b.jpg": "BBB"}
	if got := readZip(t, rec.Body.Bytes()); !sameStringMap(got, want) {
		t.Fatalf("contents changed: %v", got)
	}
}

// ---- grant cookie, replay, expiry, isolation ----

func TestGrantCookieAttributesAndAuthorization(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	sf := env.shareFile(t.TempDir(), "a.txt", []byte("A"))
	sf2 := env.shareFile(t.TempDir(), "b.txt", []byte("B"))
	g1, c1 := env.grantFor(tok, sf.ID)
	g2, c2 := env.grantFor(tok, sf2.ID)

	if c1.Path != g1.DownloadURL || !c1.HttpOnly || c1.SameSite != http.SameSiteStrictMode || c1.MaxAge != 60 {
		t.Fatalf("cookie attributes = %+v for %s", c1, g1.DownloadURL)
	}
	if strings.Contains(g1.DownloadURL, c1.Value) || strings.Contains(g1.DownloadURL, tok) {
		t.Fatal("a secret leaked into the download URL")
	}
	if rec := env.getDownload(g1, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie = %d, want 401", rec.Code)
	}
	if rec := env.getDownload(g1, &http.Cookie{Name: grantCookieName, Value: "wrong"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong secret = %d, want 403", rec.Code)
	}
	if rec := env.getDownload(g1, c2, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("another grant's cookie = %d, want 403", rec.Code)
	}
	// A refused attempt must not burn the real grant.
	if rec := env.getDownload(g1, c1, nil); rec.Code != http.StatusOK || rec.Body.String() != "A" {
		t.Fatalf("legitimate download after failed attempts = %d %q", rec.Code, rec.Body.String())
	}
	if rec := env.getDownload(g1, c1, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("replay = %d, want 403", rec.Code)
	}
	if rec := env.getDownload(g2, c2, nil); rec.Code != http.StatusOK || rec.Body.String() != "B" {
		t.Fatalf("second grant = %d", rec.Code)
	}
}

func TestOnlyGETRedeemsAGrantAndPollingNeverConsumesIt(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	sf := env.shareFile(t.TempDir(), "a.txt", []byte("A"))
	g, cookie := env.grantFor(tok, sf.ID)

	for i := 0; i < 20; i++ {
		code, p := env.progress(tok, g.ProgressID)
		if code != http.StatusOK || p["state"] != "pending" {
			t.Fatalf("poll %d = %d %v", i, code, p)
		}
	}
	req := httptest.NewRequest(http.MethodHead, g.DownloadURL, nil)
	req.AddCookie(cookie)
	head := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(head, req)
	if head.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD = %d, want 405 without redeeming", head.Code)
	}
	req = httptest.NewRequest(http.MethodPost, g.DownloadURL, nil)
	req.AddCookie(cookie)
	post := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(post, req)
	if post.Code == http.StatusOK {
		t.Fatalf("POST redeemed a download grant")
	}
	rec := env.getDownload(g, cookie, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "A" {
		t.Fatalf("grant was consumed by polling or a probe: %d %q", rec.Code, rec.Body.String())
	}
}

func TestExpiredGrantAnswersGoneAndIsCleanedUp(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	clock := newFakeClock()
	env.srv.grants.now = clock.Now
	sf := env.shareFile(t.TempDir(), "a.txt", []byte("A"))
	g, cookie := env.grantFor(tok, sf.ID)

	clock.Advance(grantLifetime + time.Second)
	if rec := env.getDownload(g, cookie, nil); rec.Code != http.StatusGone {
		t.Fatalf("expired grant = %d, want 410", rec.Code)
	}
	code, p := env.progress(tok, g.ProgressID)
	if code != http.StatusOK || p["state"] != "expired" {
		t.Fatalf("progress after expiry = %d %v, want expired (the phone must stop waiting)", code, p)
	}
	if rec := env.getDownload(g, cookie, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("expired grant stays redeemable: %d", rec.Code)
	}
	clock.Advance(progressRetention + time.Second)
	if code, _ := env.progress(tok, g.ProgressID); code != http.StatusNotFound {
		t.Fatalf("finished progress record outlived its retention: %d", code)
	}
	if grants, progress, active := env.srv.grants.size(); grants+progress+active != 0 {
		t.Fatalf("store still holds grants=%d progress=%d active=%d", grants, progress, active)
	}
}

func TestCompletedProgressIsRemovedAfterRetention(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	clock := newFakeClock()
	env.srv.grants.now = clock.Now
	sf := env.shareFile(t.TempDir(), "a.txt", []byte("A"))
	g, cookie := env.grantFor(tok, sf.ID)
	if rec := env.getDownload(g, cookie, nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if _, p := env.progress(tok, g.ProgressID); p["state"] != "completed" {
		t.Fatalf("state = %v", p["state"])
	}
	clock.Advance(progressRetention + time.Second)
	if code, _ := env.progress(tok, g.ProgressID); code != http.StatusNotFound {
		t.Fatalf("completed record kept past retention: %d", code)
	}
}

func TestDownloadSessionIsolationAndRevocation(t *testing.T) {
	env := newTestEnv(t)
	a, b := env.token(), env.token()
	sf := env.shareFile(t.TempDir(), "a.txt", []byte("A"))
	g, cookie := env.grantFor(a, sf.ID)

	if code, _ := env.progress(b, g.ProgressID); code != http.StatusNotFound {
		t.Fatalf("session B read session A's progress: %d", code)
	}
	if code, _ := env.progress("", g.ProgressID); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated progress = %d", code)
	}

	sessA, _ := env.sess.Validate(a)
	env.sess.RevokeSession(sessA)
	if rec := env.getDownload(g, cookie, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("grant of a revoked session = %d, want 401", rec.Code)
	}
	// B's world is untouched.
	g2, c2 := env.grantFor(b, sf.ID)
	if rec := env.getDownload(g2, c2, nil); rec.Code != http.StatusOK {
		t.Fatalf("session B download = %d", rec.Code)
	}
}

func TestConcurrentRedemptionHasExactlyOneWinner(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	sf := env.shareFile(t.TempDir(), "a.txt", []byte("A"))
	g, cookie := env.grantFor(tok, sf.ID)
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := env.getDownload(g, cookie, nil)
			mu.Lock()
			codes[rec.Code]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	// Busy retries (409) leave the grant intact; replays after the winner
	// finishes are 403. Either way exactly one request got the bytes.
	if codes[http.StatusOK] != 1 {
		t.Fatalf("status counts = %v, want exactly one 200", codes)
	}
	for code := range codes {
		if code != http.StatusOK && code != http.StatusForbidden && code != http.StatusConflict {
			t.Fatalf("unexpected status %d in %v", code, codes)
		}
	}
}

func TestGrantStoreBoundsGrantsAndProgressPerSession(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	sf := env.shareFile(t.TempDir(), "a.txt", []byte("A"))
	body, _ := json.Marshal(map[string]any{"sourceIds": []string{sf.ID}})
	for i := 0; i < maxGrantsPerSession; i++ {
		if rec := env.do(http.MethodPost, "/api/download-grants", tok, body); rec.Code != http.StatusCreated {
			t.Fatalf("grant %d = %d", i, rec.Code)
		}
	}
	if rec := env.do(http.MethodPost, "/api/download-grants", tok, body); rec.Code != http.StatusConflict {
		t.Fatalf("grant beyond the cap = %d, want 409", rec.Code)
	}
	// Another session is unaffected by this one's cap.
	other := env.token()
	if rec := env.do(http.MethodPost, "/api/download-grants", other, body); rec.Code != http.StatusCreated {
		t.Fatalf("independent session blocked: %d", rec.Code)
	}
}

func TestOneActiveArchiveStreamPerSession(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()
	sess, _ := env.sess.Validate(tok)
	other, _ := env.sess.Validate(env.token())
	sf := env.shareFile(t.TempDir(), "a.txt", []byte("A"))
	src := []archiveSource{{Kind: transfer.SourceKindFile, Name: "a.txt", File: sf}}
	store := env.srv.grants

	g1, _, err := store.create(sess, src, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	g2, _, err := store.create(sess, src, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	won, _, err := store.redeem(g1.ID, g1.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.redeem(g2.ID, g2.Secret); !errors.Is(err, errGrantBusy) {
		t.Fatalf("second concurrent redemption = %v, want busy", err)
	}
	if _, _, err := store.create(sess, src, "a.txt"); err == nil {
		t.Fatal("new grant allowed while a stream is active for the session")
	}
	// Another session streams in parallel.
	go1, _, err := store.create(other, src, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.redeem(go1.ID, go1.Secret); err != nil {
		t.Fatalf("independent session blocked by this session's stream: %v", err)
	}
	store.finish(won)
	if _, _, err := store.redeem(g2.ID, g2.Secret); err != nil {
		t.Fatalf("grant stayed unusable after the stream finished: %v", err)
	}
}

// ---- streaming failures, disconnects, large synthetic archives (real HTTP) ----

type liveEnv struct {
	*testEnv
	ts *httptest.Server
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()
	env := newTestEnv(t)
	ts := httptest.NewServer(env.srv.Handler())
	t.Cleanup(ts.Close)
	return &liveEnv{testEnv: env, ts: ts}
}

// liveGrant creates a grant through a specific running server (grants live in
// the server that issued them).
func liveGrant(t *testing.T, baseURL, tok string, ids ...string) (createGrantResponse, *http.Cookie) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"sourceIds": ids})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/download-grants", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create grant = %d", resp.StatusCode)
	}
	var g createGrantResponse
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatal(err)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	return g, cookies[0]
}

func liveProgress(t *testing.T, baseURL, tok, id string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/download-progress/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (l *liveEnv) get(g createGrantResponse, c *http.Cookie) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, l.ts.URL+g.DownloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.AddCookie(c)
	return (&http.Client{Timeout: 90 * time.Second}).Do(req)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (l *liveEnv) waitIdle(tok string) {
	l.t.Helper()
	sess, err := l.sess.Validate(tok)
	if err != nil {
		l.t.Fatal(err)
	}
	waitFor(l.t, "stream to release its grant slot and session pin", func() bool {
		_, _, active := l.srv.grants.size()
		return active == 0 && sess.PinnedTransfers() == 0
	})
}

func TestMidStreamSourceFailureAbortsInsteadOfSealingATruncatedZip(t *testing.T) {
	for _, mutation := range []string{"shrinks", "vanishes"} {
		t.Run(mutation, func(t *testing.T) {
			live := newLiveEnv(t)
			tok := live.token()
			root := filepath.Join(t.TempDir(), "Season")
			writeTree(t, root, map[string]string{
				"a-first.txt":  strings.Repeat("A", 4096),
				"b-second.txt": strings.Repeat("B", 4096),
				"c-third.txt":  strings.Repeat("C", 4096),
			})
			source, err := live.reg.AddFolder(root, "")
			if err != nil {
				t.Fatal(err)
			}
			g, cookie := live.grantFor(tok, source.ID)
			victim := filepath.Join(root, "b-second.txt")
			if mutation == "shrinks" {
				if err := os.Truncate(victim, 10); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(victim); err != nil {
				t.Fatal(err)
			}

			resp, err := live.get(g, cookie)
			if err != nil {
				t.Fatalf("request failed outright: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			body, readErr := io.ReadAll(resp.Body)
			if readErr == nil {
				t.Fatalf("body ended cleanly after a source failure (%d bytes): the browser would save a truncated ZIP as complete", len(body))
			}
			if bytes.Contains(body, []byte(`"error"`)) {
				t.Fatalf("error JSON was appended to a partially written ZIP: %q", body)
			}
			if _, zerr := zip.NewReader(bytes.NewReader(body), int64(len(body))); zerr == nil {
				t.Fatal("the truncated stream parses as a valid ZIP: a central directory was written after the failure")
			}
			live.waitIdle(tok)
			code, p := live.progress(tok, g.ProgressID)
			if code != http.StatusOK || p["state"] != "failed" {
				t.Fatalf("progress = %d %v, want failed", code, p)
			}
			msg, _ := p["error"].(string)
			if msg == "" || strings.Contains(msg, root) || strings.Contains(msg, os.TempDir()) {
				t.Fatalf("progress error %q is empty or leaks a filesystem path", msg)
			}
		})
	}
}

func TestSingleFileSourceVanishedBeforeStreamAnswersGone(t *testing.T) {
	live := newLiveEnv(t)
	tok := live.token()
	sf := live.shareFile(t.TempDir(), "gone.txt", []byte("here"))
	g, cookie := live.grantFor(tok, sf.ID)
	if err := os.Remove(sf.Path); err != nil {
		t.Fatal(err)
	}
	resp, err := live.get(g, cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want 410", resp.StatusCode)
	}
	live.waitIdle(tok)
	if _, p := live.progress(tok, g.ProgressID); p["state"] != "failed" {
		t.Fatalf("progress = %v, want failed", p)
	}
}

func TestClientDisconnectReleasesStreamSessionPinAndGrantSlot(t *testing.T) {
	live := newLiveEnv(t)
	tok := live.token()
	root := filepath.Join(t.TempDir(), "Big")
	sparseFile(t, filepath.Join(root, "huge.bin"), 512<<20)
	source, err := live.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	g, cookie := live.grantFor(tok, source.ID)
	resp, err := live.get(g, cookie)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	sess, _ := live.sess.Validate(tok)
	if sess.PinnedTransfers() != 1 {
		t.Fatalf("a live archive stream must pin its session, pins = %d", sess.PinnedTransfers())
	}
	_ = resp.Body.Close() // the phone browser goes away

	live.waitIdle(tok)
	_, p := live.progress(tok, g.ProgressID)
	if p["state"] != "failed" {
		t.Fatalf("a disconnected download must not be reported complete: %v", p)
	}
	// The slot is free again: a new grant can be issued and streamed.
	sf := live.shareFile(t.TempDir(), "next.txt", []byte("next"))
	g2, c2 := live.grantFor(tok, sf.ID)
	resp2, err := live.get(g2, c2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if body, _ := io.ReadAll(resp2.Body); string(body) != "next" {
		t.Fatalf("follow-up download = %q", body)
	}
}

func TestLargeSyntheticArchiveStreamsWithBoundedMemory(t *testing.T) {
	live := newLiveEnv(t)
	tok := live.token()
	root := filepath.Join(t.TempDir(), "Large Set")
	const big = 96 << 20
	sparseFile(t, filepath.Join(root, "video", "movie part 1.bin"), big)
	sparseFile(t, filepath.Join(root, "video", "movie part 2.bin"), big)
	writeTree(t, root, map[string]string{"readme.txt": "small", "empty/": ""})
	_ = os.Remove(filepath.Join(root, "empty"))
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := live.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	g, cookie := live.grantFor(tok, source.ID)

	out := filepath.Join(t.TempDir(), "download.zip")
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	resp, err := live.get(g, cookie)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	n, copyErr := io.Copy(f, resp.Body)
	_ = resp.Body.Close()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if copyErr != nil {
		t.Fatalf("streaming failed after %d bytes: %v", n, copyErr)
	}
	if n < 2*big {
		t.Fatalf("archive is only %d bytes", n)
	}
	// ~192 MiB went through; if the archive (or a file) were buffered, the
	// process would have allocated at least that much. A streaming copy
	// allocates only fixed buffers.
	if delta := after.TotalAlloc - before.TotalAlloc; delta > 48<<20 {
		t.Fatalf("allocated %d MiB while streaming a %d MiB archive: payload is being buffered", delta>>20, n>>20)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("central directory unreadable: %v", err)
	}
	defer zr.Close()
	sizes := map[string]uint64{}
	for _, zf := range zr.File {
		if strings.HasSuffix(zf.Name, "/") {
			sizes[zf.Name] = 0
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		copied, err := io.Copy(io.Discard, rc) // verifies the CRC at EOF
		_ = rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", zf.Name, err)
		}
		if uint64(copied) != zf.UncompressedSize64 {
			t.Fatalf("%s: read %d, header says %d", zf.Name, copied, zf.UncompressedSize64)
		}
		sizes[zf.Name] = uint64(copied)
	}
	want := map[string]uint64{
		"Large Set/video/movie part 1.bin": big,
		"Large Set/video/movie part 2.bin": big,
		"Large Set/readme.txt":             5,
		"Large Set/empty/":                 0,
	}
	if len(sizes) != len(want) {
		t.Fatalf("entries = %v", sizes)
	}
	for name, size := range want {
		if got, ok := sizes[name]; !ok || got != size {
			t.Fatalf("entry %q = %d (present=%v), want %d", name, got, ok, size)
		}
	}
	live.waitIdle(tok)
	if _, p := live.progress(tok, g.ProgressID); p["state"] != "completed" || p["processedBytes"] != float64(2*big+5) {
		t.Fatalf("progress = %v", p)
	}
}

func TestBrowserPauseLongerThanUploadStallDoesNotFailADownload(t *testing.T) {
	// A phone browser stops reading while it shows a save dialog. The upload
	// stall window (here 100 ms) must not apply to downloads.
	env := newTestEnv(t)
	srv, errc, stop := startRealServer(t, Config{
		TransferStall: 100 * time.Millisecond,
		Services: &Services{
			Sessions: env.sess, Transfers: env.trans, Coordinator: env.coord, Registry: env.reg,
		},
	})
	tok := env.token()
	root := filepath.Join(t.TempDir(), "Dialog")
	sparseFile(t, filepath.Join(root, "data.bin"), 64<<20)
	source, err := env.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	g, cookie := liveGrant(t, srv.URL(), tok, source.ID)
	req, _ := http.NewRequest(http.MethodGet, srv.URL()+g.DownloadURL, nil)
	req.AddCookie(cookie)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 32<<10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond) // the "save dialog": 7x the upload stall
	n, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		t.Fatalf("download was cut during a browser pause after %d bytes: %v", n, err)
	}
	if n+int64(len(buf)) < 64<<20 {
		t.Fatalf("only %d bytes arrived", n+int64(len(buf)))
	}
	stop()
	awaitServeReturn(t, errc)
}

func TestDownloadStallStillCutsAReceiverThatWentSilent(t *testing.T) {
	env := newTestEnv(t)
	srv, errc, stop := startRealServer(t, Config{
		DownloadStall: 300 * time.Millisecond,
		Services: &Services{
			Sessions: env.sess, Transfers: env.trans, Coordinator: env.coord, Registry: env.reg,
		},
	})
	tok := env.token()
	root := filepath.Join(t.TempDir(), "Silent")
	sparseFile(t, filepath.Join(root, "data.bin"), 256<<20)
	source, err := env.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	g, cookie := liveGrant(t, srv.URL(), tok, source.ID)
	req, _ := http.NewRequest(http.MethodGet, srv.URL()+g.DownloadURL, nil)
	req.AddCookie(cookie)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 32<<10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	_, err = io.Copy(io.Discard, resp.Body)
	if err == nil {
		t.Fatal("a receiver that stopped reading was never cut and the body ended cleanly")
	}
	sess, _ := env.sess.Validate(tok)
	waitFor(t, "stalled stream to release its resources", func() bool {
		_, _, active := srv.grants.size()
		return active == 0 && sess.PinnedTransfers() == 0
	})
	if p := liveProgress(t, srv.URL(), tok, g.ProgressID); p["state"] != "failed" {
		t.Fatalf("progress = %v, want failed", p)
	}
	stop()
	awaitServeReturn(t, errc)
}

func TestArchiveDownloadKeepsSessionAliveAndRestartsIdleWindow(t *testing.T) {
	live := newLiveEnv(t)
	tok := live.token()
	root := filepath.Join(t.TempDir(), "Long")
	sparseFile(t, filepath.Join(root, "data.bin"), 192<<20)
	source, err := live.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	g, cookie := live.grantFor(tok, source.ID)
	resp, err := live.get(g, cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 256<<10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	// The stream is far from over (the client is not draining); the session
	// sees no other traffic for three idle windows.
	live.clock.Advance(30 * time.Minute)
	if _, err := live.sess.Validate(tok); err != nil {
		t.Fatalf("session expired under a live, authenticated download: %v", err)
	}
	if n, err := io.Copy(io.Discard, resp.Body); err != nil || n+int64(len(buf)) < 192<<20 {
		t.Fatalf("download ended early: %d bytes, %v", n+int64(len(buf)), err)
	}
	live.waitIdle(tok)
	// The long download just ended; the very next request must still work.
	if _, err := live.sess.Validate(tok); err != nil {
		t.Fatalf("session was dead the instant the download finished: %v", err)
	}
	if code, p := live.progress(tok, g.ProgressID); code != http.StatusOK || p["state"] != "completed" {
		t.Fatalf("progress after the download = %d %v", code, p)
	}
}

// TestZip64ArchiveBeyondFourGiB verifies archives whose entry (and offsets)
// exceed 4 GiB stay valid ZIP64. It moves ~9 GiB through the stack and a
// temporary file, so it only runs when FLITNIX_LARGE_TESTS=1.
func TestZip64ArchiveBeyondFourGiB(t *testing.T) {
	if os.Getenv("FLITNIX_LARGE_TESTS") != "1" {
		t.Skip("set FLITNIX_LARGE_TESTS=1 to run the multi-gigabyte ZIP64 check")
	}
	live := newLiveEnv(t)
	tok := live.token()
	root := filepath.Join(t.TempDir(), "Huge")
	const big = int64(4<<30) + 12345
	sparseFile(t, filepath.Join(root, "a-first.bin"), big)
	sparseFile(t, filepath.Join(root, "b-second.bin"), big) // starts beyond a 4 GiB offset
	writeTree(t, root, map[string]string{"c-last.txt": "tail"})
	source, err := live.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	g, cookie := live.grantFor(tok, source.ID)
	resp, err := live.get(g, cookie)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "huge.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = f.Close()
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("ZIP64 central directory unreadable: %v", err)
	}
	defer zr.Close()
	want := map[string]uint64{"Huge/a-first.bin": uint64(big), "Huge/b-second.bin": uint64(big), "Huge/c-last.txt": 4}
	if len(zr.File) != len(want) {
		t.Fatalf("%d entries", len(zr.File))
	}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, rc)
		_ = rc.Close()
		if err != nil || uint64(n) != want[zf.Name] || zf.UncompressedSize64 != want[zf.Name] {
			t.Fatalf("%s: read %d err=%v header=%d want=%d", zf.Name, n, err, zf.UncompressedSize64, want[zf.Name])
		}
	}
}
