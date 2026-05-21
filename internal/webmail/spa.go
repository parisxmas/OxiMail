package webmail

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// spaHandler serves a single-page application from `dir`: a request that
// maps to a real file is served as-is, and any other path falls back to
// index.html so the SPA's client-side router can handle it.
//
// Requests under /api/ never reach this handler — the API routes are
// registered with more specific mux patterns, which take precedence.
//
// One subtlety: the index.html fallback is for SPA routes
// (/mail, /settings, /login, …), NOT for missing static assets. A
// browser that has an old index.html cached can ask for a stale
// hashed chunk filename like /chunk-XYZ.js that no longer exists in
// the new build; falling back to index.html there would respond with
// `text/html` to a script request, and the browser refuses to
// execute it ("MIME type 'text/html'" error). For requests that look
// like a static asset (any path with a file extension), return a real
// 404 — the browser will retry, see the new index.html with the new
// hash, and recover cleanly on the next reload.
func spaHandler(dir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// filepath.Clean on a rooted path clamps any ".." at the root,
		// so the join cannot escape dir.
		path := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			if looksLikeAsset(r.URL.Path) {
				http.NotFound(w, r)
				return
			}
			http.ServeFile(w, r, index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// looksLikeAsset reports whether the path looks like a static asset
// (i.e. has a dot in its final segment) rather than an SPA route.
// SPA routes are word-y paths like /mail or /settings; static assets
// are things like /chunk-NLSFM2YN.js, /styles.css, /favicon.ico,
// /assets/logo.png. The heuristic is "last path segment contains a
// dot" — the same rule nginx's try_files-with-fallback uses.
func looksLikeAsset(urlPath string) bool {
	slash := strings.LastIndexByte(urlPath, '/')
	tail := urlPath
	if slash >= 0 {
		tail = urlPath[slash+1:]
	}
	return strings.Contains(tail, ".")
}
