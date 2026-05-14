package webmail

import (
	"net/http"
	"os"
	"path/filepath"
)

// spaHandler serves a single-page application from `dir`: a request that
// maps to a real file is served as-is, and any other path falls back to
// index.html so the SPA's client-side router can handle it.
//
// Requests under /api/ never reach this handler — the API routes are
// registered with more specific mux patterns, which take precedence.
func spaHandler(dir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))
	index := filepath.Join(dir, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// filepath.Clean on a rooted path clamps any ".." at the root,
		// so the join cannot escape dir.
		path := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			http.ServeFile(w, r, index)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
