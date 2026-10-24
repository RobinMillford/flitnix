package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
)

// newRouter builds the HTTP router for the phone page.
//
// Routes (stage 2):
//
//	/             GET/HEAD  the embedded phone webpage
//	/healthz      GET/HEAD  JSON health response
//	/static/<f>   GET/HEAD  embedded stylesheet / script / other assets
//	anything else           404 JSON error
//
// Method handling is explicit inside each handler so behaviour is identical
// across Go versions: anything but GET/HEAD yields 405 with an Allow header.
// No upload/download endpoints exist yet; file transfers are later stages.
func newRouter() http.Handler {
	index, err := pageAssets.ReadFile("assets/index.html")
	if err != nil {
		panic("server: embedded assets missing: " + err.Error())
	}
	assetsRoot, err := fs.Sub(pageAssets, "assets")
	if err != nil {
		panic("server: embedded assets unusable: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", serveHome(index))
	mux.HandleFunc("/healthz", serveHealth)
	mux.HandleFunc("/static/", serveStatic(assetsRoot))
	mux.HandleFunc("/", serveNotFound)
	return mux
}

// serveHome serves the embedded index page.
func serveHome(index []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireGetHead(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(index)
	}
}

// serveHealth reports server liveness as a small JSON document. It carries
// no request-derived data, so there is nothing to leak into logs or output.
func serveHealth(w http.ResponseWriter, r *http.Request) {
	if !requireGetHead(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// serveStatic serves files embedded under /static/<name>. The directory
// listing of /static/ itself is deliberately 404 so the asset tree is not
// exposed.
func serveStatic(assetsRoot fs.FS) http.HandlerFunc {
	files := http.StripPrefix("/static", http.FileServer(http.FS(assetsRoot)))
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireGetHead(w, r) {
			return
		}
		if r.URL.Path == "/static/" || r.URL.Path == "/static" {
			serveNotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	}
}

// serveNotFound answers unknown paths with a JSON 404.
func serveNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

// requireGetHead allows GET and HEAD; anything else gets a 405 response
// with an Allow header, and the caller must stop handling the request.
func requireGetHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeError(w, http.StatusMethodNotAllowed, "method not allowed; use GET")
	return false
}

// writeJSON writes a JSON response body.
func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError writes a JSON error response akin to http.Error, with nosniff
// protection included.
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
