package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doRequest(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	s := New(Config{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	err := json.Unmarshal(rec.Body.Bytes(), into)
	if err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %q", err, rec.Body.String())
	}
}

func TestHomePage(t *testing.T) {
	rec := doRequest(t, http.MethodGet, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / returned %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET / returned Content-Type %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Flitnix", "/static/styles.css", "/static/app.js", "later stage"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET / body does not contain %q", want)
		}
	}
}

func TestHomePageHeadHasNoBody(t *testing.T) {
	rec := doRequest(t, http.MethodHead, "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD / returned %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("HEAD / returned Content-Type %q, want text/html", ct)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD / returned a body of %d bytes, want none", rec.Body.Len())
	}
}

func TestStaticAssetsServed(t *testing.T) {
	cases := []struct {
		path       string
		contentTyp string
	}{
		{"/static/styles.css", "text/css"},
		{"/static/app.js", "text/javascript"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := doRequest(t, http.MethodGet, tc.path)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s returned %d, want 200", tc.path, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentTyp) {
				t.Errorf("GET %s returned Content-Type %q, want prefix %q", tc.path, ct, tc.contentTyp)
			}
			if rec.Body.Len() == 0 {
				t.Errorf("GET %s returned an empty body", tc.path)
			}
		})
	}
}

func TestStaticMissingAssetIsNotFound(t *testing.T) {
	rec := doRequest(t, http.MethodGet, "/static/no-such-file.css")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing asset returned %d, want 404", rec.Code)
	}
}

func TestStaticDirectoryListingIsForbidden(t *testing.T) {
	rec := doRequest(t, http.MethodGet, "/static/")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /static/ returned %d, want 404 (no directory listing)", rec.Code)
	}
}

func TestHealthzReturnsJSON(t *testing.T) {
	rec := doRequest(t, http.MethodGet, "/healthz")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz returned %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET /healthz returned Content-Type %q, want application/json", ct)
	}
	var body struct {
		Status string `json:"status"`
	}
	decodeBody(t, rec, &body)
	if body.Status != "ok" {
		t.Errorf("GET /healthz reported status %q, want \"ok\"", body.Status)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	cases := []string{"/definitely-not-here", "/foo/bar", "/healthz/extra"}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			rec := doRequest(t, http.MethodGet, path)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("GET %s returned %d, want 404", path, rec.Code)
			}
			var body struct {
				Error string `json:"error"`
			}
			decodeBody(t, rec, &body)
			if body.Error != "not found" {
				t.Errorf("GET %s error body %q, want \"not found\"", path, body.Error)
			}
		})
	}
}

func TestUnsupportedMethodsRejected(t *testing.T) {
	cases := []struct {
		method, target string
	}{
		{http.MethodPost, "/"},
		{http.MethodPut, "/"},
		{http.MethodPut, "/healthz"},
		{http.MethodDelete, "/healthz"},
		{http.MethodPost, "/static/styles.css"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			rec := doRequest(t, tc.method, tc.target)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s returned %d, want 405", tc.method, tc.target, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Errorf("%s %s returned Allow %q, want \"GET, HEAD\"", tc.method, tc.target, allow)
			}
		})
	}
}

func TestUnknownPathWithNonGetMethodIsStillNotFound(t *testing.T) {
	// The catch-all owns unknown paths regardless of method; a 404 (not 500
	// or a block) is the documented behaviour here.
	rec := doRequest(t, http.MethodPost, "/unknown/path")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /unknown/path returned %d, want 404", rec.Code)
	}
}
