package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestSourceExplorerBrowseAndSelectiveDownloads(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token()

	root := filepath.Join(t.TempDir(), "Vacation")
	writeTree(t, root, map[string]string{
		"readme.md":         "trip summary",
		"day1/beach.jpg":    strings.Repeat("B", 8192),
		"day1/sunset.jpg":   "SUNSET",
		"day2/mountain.jpg": "PEAK",
		"day2/unwanted.bin": "SKIP-ME",
	})
	src, err := env.reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}

	otherDir := filepath.Join(t.TempDir(), "PrivateOther")
	writeTree(t, otherDir, map[string]string{"secret.txt": "SECRET"})
	otherSrc, err := env.reg.AddFolder(otherDir, "")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Unauthorized browse rejected
	unauthRec := env.do(http.MethodGet, "/api/sources/"+src.ID+"/items", "", nil)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth browse = %d, want 401", unauthRec.Code)
	}

	// 2. Traversal & bad pagination rejected cleanly
	for _, bad := range []string{
		"?prefix=../etc",
		"?prefix=day1/../day2",
		"?prefix=day1\\beach.jpg",
		"?cursor=-5",
		"?limit=0",
	} {
		rec := env.do(http.MethodGet, "/api/sources/"+src.ID+"/items"+bad, tok, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET items%s = %d, want 400", bad, rec.Code)
		}
	}

	// 3. Browse root -> find day1/ and day2/
	rootRec := env.do(http.MethodGet, "/api/sources/"+src.ID+"/items", tok, nil)
	if rootRec.Code != http.StatusOK {
		t.Fatalf("browse root = %d: %s", rootRec.Code, rootRec.Body.String())
	}
	if strings.Contains(rootRec.Body.String(), root) {
		t.Fatal("browse response leaked absolute folder path")
	}
	var rootPage transfer.SourceBrowsePage
	if err := json.Unmarshal(rootRec.Body.Bytes(), &rootPage); err != nil {
		t.Fatal(err)
	}
	if len(rootPage.Items) != 3 { // day1, day2, readme.md
		t.Fatalf("root items = %d, want 3", len(rootPage.Items))
	}

	// 4. Browse day1 & day2 to capture opaque item IDs
	day1Rec := env.do(http.MethodGet, "/api/sources/"+src.ID+"/items?prefix=day1", tok, nil)
	if day1Rec.Code != http.StatusOK {
		t.Fatalf("browse day1 = %d", day1Rec.Code)
	}
	var day1Page transfer.SourceBrowsePage
	_ = json.Unmarshal(day1Rec.Body.Bytes(), &day1Page)
	if len(day1Page.Items) != 2 {
		t.Fatalf("day1 items = %d, want 2", len(day1Page.Items))
	}
	beachItem := day1Page.Items[0] // sorted: beach.jpg, sunset.jpg
	if beachItem.Name != "beach.jpg" || beachItem.ID == "" {
		t.Fatalf("unexpected beach item: %+v", beachItem)
	}

	day2Rec := env.do(http.MethodGet, "/api/sources/"+src.ID+"/items?prefix=day2", tok, nil)
	var day2Page transfer.SourceBrowsePage
	_ = json.Unmarshal(day2Rec.Body.Bytes(), &day2Page)
	mountainItem := day2Page.Items[0] // mountain.jpg before unwanted.bin

	// 5. Cross-folder itemId injection rejected
	otherPageRec := env.do(http.MethodGet, "/api/sources/"+otherSrc.ID+"/items", tok, nil)
	var otherPage transfer.SourceBrowsePage
	_ = json.Unmarshal(otherPageRec.Body.Bytes(), &otherPage)
	crossBody, _ := json.Marshal(map[string]any{
		"sourceId": src.ID,
		"itemIds":  []string{otherPage.Items[0].ID},
	})
	if crossRec := env.do(http.MethodPost, "/api/download-grants", tok, crossBody); crossRec.Code != http.StatusNotFound {
		t.Fatalf("cross-source itemId grant = %d, want 404", crossRec.Code)
	}

	// 6. Single selected file inside shared folder downloads directly (no ZIP) with G2 HTTP Range resumption
	singleReqBody, _ := json.Marshal(map[string]any{
		"sourceId": src.ID,
		"itemIds":  []string{beachItem.ID},
	})
	singleGrantRec := env.do(http.MethodPost, "/api/download-grants", tok, singleReqBody)
	if singleGrantRec.Code != http.StatusCreated {
		t.Fatalf("single item grant = %d: %s", singleGrantRec.Code, singleGrantRec.Body.String())
	}
	var singleGrant createGrantResponse
	_ = json.Unmarshal(singleGrantRec.Body.Bytes(), &singleGrant)
	if singleGrant.Archive || singleGrant.Filename != "beach.jpg" {
		t.Fatalf("singleGrant = %+v, want Archive=false Filename=beach.jpg", singleGrant)
	}
	var initCookie *http.Cookie
	for _, c := range singleGrantRec.Result().Cookies() {
		if c.Name == grantCookieName {
			initCookie = c
		}
	}
	initRec := env.getDownload(singleGrant, initCookie, nil)
	if initRec.Code != http.StatusOK || !bytes.Equal(initRec.Body.Bytes(), []byte(strings.Repeat("B", 8192))) {
		t.Fatalf("single folder item download = %d len=%d", initRec.Code, initRec.Body.Len())
	}
	if initRec.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("single folder item missing Accept-Ranges: bytes (%q)", initRec.Header().Get("Accept-Ranges"))
	}
	etag := initRec.Header().Get("ETag")
	contCookie := extractContinuationCookie(t, initRec, extractGrantID(singleGrant.DownloadURL))

	rangeReq := httptest.NewRequest(http.MethodGet, singleGrant.DownloadURL, nil)
	rangeReq.AddCookie(contCookie)
	rangeReq.Header.Set("Range", "bytes=100-199")
	rangeReq.Header.Set("If-Range", etag)
	rangeRec := httptest.NewRecorder()
	env.srv.Handler().ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent || rangeRec.Body.Len() != 100 {
		t.Fatalf("single folder item Range = %d len=%d", rangeRec.Code, rangeRec.Body.Len())
	}

	// 7. Multi-file selection across subdirectories streams ZIP with ONLY selected files
	multiBody, _ := json.Marshal(map[string]any{
		"sourceId": src.ID,
		"itemIds":  []string{beachItem.ID, mountainItem.ID},
	})
	multiGrantRec := env.do(http.MethodPost, "/api/download-grants", tok, multiBody)
	if multiGrantRec.Code != http.StatusCreated {
		t.Fatalf("multi item grant = %d: %s", multiGrantRec.Code, multiGrantRec.Body.String())
	}
	var multiGrant createGrantResponse
	_ = json.Unmarshal(multiGrantRec.Body.Bytes(), &multiGrant)
	if !multiGrant.Archive || multiGrant.Filename != "Vacation.zip" {
		t.Fatalf("multiGrant = %+v", multiGrant)
	}
	var multiCookie *http.Cookie
	for _, c := range multiGrantRec.Result().Cookies() {
		if c.Name == grantCookieName {
			multiCookie = c
		}
	}
	zipRec := env.getDownload(multiGrant, multiCookie, nil)
	if zipRec.Code != http.StatusOK {
		t.Fatalf("multi selective zip status = %d", zipRec.Code)
	}
	gotEntries := readZip(t, zipRec.Body.Bytes())
	wantEntries := map[string]string{
		"Vacation/day1/beach.jpg":    strings.Repeat("B", 8192),
		"Vacation/day2/mountain.jpg": "PEAK",
	}
	if !sameStringMap(gotEntries, wantEntries) {
		t.Fatalf("selective ZIP entries mismatch:\ngot=%v\nwant=%v", len(gotEntries), len(wantEntries))
	}
	if _, leaked := gotEntries["Vacation/day2/unwanted.bin"]; leaked {
		t.Fatal("unselected file leaked into selective ZIP")
	}

	// Verify pagination over HTTP too
	page1Rec := env.do(http.MethodGet, fmt.Sprintf("/api/sources/%s/items?limit=2&cursor=0", src.ID), tok, nil)
	var page1 transfer.SourceBrowsePage
	_ = json.Unmarshal(page1Rec.Body.Bytes(), &page1)
	if len(page1.Items) != 2 || !page1.HasMore || page1.NextCursor == nil {
		t.Fatalf("HTTP paginated page 1 = %+v", page1)
	}
}

func TestSourceExplorerConfinementRejectsSymlinkSubdir(t *testing.T) {
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "leak.txt"), []byte("secret"), 0o600)

	root := filepath.Join(t.TempDir(), "SharedRoot")
	_ = os.MkdirAll(root, 0o700)
	_ = os.WriteFile(filepath.Join(root, "ok.txt"), []byte("ok"), 0o600)
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unsupported on platform: %v", err)
	}

	reg := transfer.NewRegistry()
	src, err := reg.AddFolder(root, "")
	if err != nil {
		t.Fatal(err)
	}
	page, err := reg.BrowseSource(src.ID, "", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		if it.Name == "escape" || strings.Contains(it.Name, "leak") {
			t.Fatalf("symlink escaped dir surfaced in BrowseSource: %+v", it)
		}
	}
	if _, err := reg.BrowseSource(src.ID, "escape", 0, 50); err == nil {
		t.Fatal("expected browsing symlink escape prefix to fail")
	}
}
