package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/itsyamin/flitnix/internal/session"
	"github.com/itsyamin/flitnix/internal/transfer"
)

func TestPWAManifestServiceWorkerAndOfflineRoutes(t *testing.T) {
	dir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	svcs := &Services{
		Sessions:    session.NewManager(session.Config{}),
		Transfers:   transfer.NewService(transfer.Config{Store: store}),
		Coordinator: transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
	}
	router := newRouter(svcs, 5*time.Second, 5*time.Second, newGrantStore(svcs.Downloads))

	// 1. Validate /manifest.webmanifest
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43821/manifest.webmanifest", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /manifest.webmanifest = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/manifest+json") {
		t.Fatalf("manifest Content-Type = %q", ct)
	}
	var manifest struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Display   string `json:"display"`
		Icons     []struct {
			Sizes string `json:"sizes"`
		} `json:"icons"`
		ShareTarget struct {
			Action  string `json:"action"`
			Method  string `json:"method"`
			Enctype string `json:"enctype"`
		} `json:"share_target"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest JSON invalid: %v", err)
	}
	if manifest.ShortName != "Flitnix" || manifest.Display != "standalone" || manifest.ShareTarget.Action != "/share-target" || manifest.ShareTarget.Method != "POST" {
		t.Fatalf("unexpected manifest fields: %+v", manifest)
	}
	if len(manifest.Icons) < 2 {
		t.Fatalf("expected at least 192 and 512 icons, got %+v", manifest.Icons)
	}

	// 2. Validate /sw.js headers & cache isolation rules
	swReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43821/sw.js", nil)
	swRec := httptest.NewRecorder()
	router.ServeHTTP(swRec, swReq)
	if swRec.Code != http.StatusOK {
		t.Fatalf("GET /sw.js = %d, want 200", swRec.Code)
	}
	if swRec.Header().Get("Service-Worker-Allowed") != "/" {
		t.Fatalf("missing Service-Worker-Allowed: / header")
	}
	if !strings.Contains(swRec.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("expected no-store Cache-Control on /sw.js, got %q", swRec.Header().Get("Cache-Control"))
	}
	swSrc := swRec.Body.String()
	for _, needle := range []string{"/api/", "/guest-api/", "Authorization", "flitnix-share-target", "/offline.html"} {
		if !strings.Contains(swSrc, needle) {
			t.Fatalf("service worker missing required guard/route %q", needle)
		}
	}

	// 3. Validate /offline.html and CSP manifest-src
	offReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43821/offline.html", nil)
	offRec := httptest.NewRecorder()
	router.ServeHTTP(offRec, offReq)
	if offRec.Code != http.StatusOK || !strings.Contains(offRec.Body.String(), "Retry Connection") {
		t.Fatalf("unexpected /offline.html response: code=%d", offRec.Code)
	}
	if csp := offRec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "manifest-src 'self'") {
		t.Fatalf("expected manifest-src 'self' in CSP, got %q", csp)
	}

	// 4. Validate /static/icon-192.png and /static/icon-512.png
	for _, p := range []string{"/static/icon-192.png", "/static/icon-512.png"} {
		iReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43821"+p, nil)
		iRec := httptest.NewRecorder()
		router.ServeHTTP(iRec, iReq)
		if iRec.Code != http.StatusOK || iRec.Body.Len() == 0 {
			t.Fatalf("GET %s failed: code=%d len=%d", p, iRec.Code, iRec.Body.Len())
		}
	}
}

func TestShareTargetFallbackSecurityAndBounds(t *testing.T) {
	recvDir := t.TempDir()
	store, err := transfer.NewStore(transfer.StoreConfig{Dir: recvDir})
	if err != nil {
		t.Fatal(err)
	}
	svcs := &Services{
		Sessions:    session.NewManager(session.Config{}),
		Transfers:   transfer.NewService(transfer.Config{Store: store}),
		Coordinator: transfer.NewCoordinator(transfer.CoordinatorConfig{}),
		Registry:    transfer.NewRegistry(),
		Downloads:   transfer.NewDownloadRegistry(),
	}
	router := newRouter(svcs, 5*time.Second, 5*time.Second, newGrantStore(svcs.Downloads))

	// 1. Multi-file + text + URL multipart POST fallback returns 200 JSON summary and writes NOTHING to receive dir.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("title", "Holiday Photos")
	_ = mw.WriteField("text", "Shared text note")
	_ = mw.WriteField("url", "https://example.test/album")
	fw1, _ := mw.CreateFormFile("files", "photo1.jpg")
	_, _ = fw1.Write([]byte("fake-jpg-1"))
	fw2, _ := mw.CreateFormFile("files", "photo2.jpg")
	_, _ = fw2.Write([]byte("fake-jpg-2"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43821/share-target", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:43821")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /share-target = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		FileCount int  `json:"fileCount"`
		HasText   bool `json:"hasText"`
		HasURL    bool `json:"hasUrl"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.FileCount != 2 || !out.HasText || !out.HasURL {
		t.Fatalf("unexpected share-target summary: %+v", out)
	}
	entries, _ := os.ReadDir(filepath.Clean(recvDir))
	if len(entries) != 0 {
		t.Fatalf("unauthenticated share-target must never write files to receive dir, found %v", entries)
	}

	// 2. Cross-origin POST /share-target must be rejected by CSRF middleware (403).
	csrfReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43821/share-target", strings.NewReader("text=evil"))
	csrfReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	csrfReq.Header.Set("Origin", "https://evil.example")
	csrfRec := httptest.NewRecorder()
	router.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-origin share-target POST, got %d", csrfRec.Code)
	}

	// 3. Oversized POST (>32 MiB) must be rejected with 413 Payload Too Large.
	bigBody := io.MultiReader(
		strings.NewReader("--abc\r\nContent-Disposition: form-data; name=\"files\"; filename=\"big.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n"),
		io.LimitReader(zeroReader{}, maxShareTargetFallbackBytes+1024),
		strings.NewReader("\r\n--abc--\r\n"),
	)
	bigReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43821/share-target", bigBody)
	bigReq.Header.Set("Content-Type", "multipart/form-data; boundary=abc")
	bigRec := httptest.NewRecorder()
	router.ServeHTTP(bigRec, bigReq)
	if bigRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 on oversized share-target POST, got %d (body=%s)", bigRec.Code, bigRec.Body.String())
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}
