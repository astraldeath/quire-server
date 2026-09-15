// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var webRoute = regexp.MustCompile(`^/(library|reading|account|settings(/(appearance|library|backups))?|admin(/(overview|libraries|folders|settings|accounts|invites|backups))?|books/[a-f0-9]{64}(/(read|tracking))?|series/[^/]+(/tracking)?)$`)

// WebUI serves only a dedicated reader build directory, never the data directory.
func WebUI(api http.Handler, directory string) http.Handler {
	if directory == "" {
		return api
	}
	files := http.FileServer(http.Dir(directory))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/.well-known/") || r.URL.Path == "/healthz" {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' blob:; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https://cdn.mangabaka.dev; font-src 'self' data: blob:; frame-src 'self' blob:; connect-src 'self' blob: https://en.wiktionary.org; worker-src 'self' blob:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-cache")
		if r.URL.Path == "/" || r.URL.Path == "/index.html" || webRoute.MatchString(r.URL.EscapedPath()) {
			http.ServeFile(w, r, filepath.Join(directory, "index.html"))
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			http.NotFound(w, r)
			return
		}
		path := filepath.Join(directory, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/")))
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
