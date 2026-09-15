package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebDeepLinksAndPrivatePaths(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("quire-app"), 0600)
	h := WebUI(http.NotFoundHandler(), dir)
	for _, path := range []string{"/library", "/reading", "/series/A%2FB/tracking", "/books/" + strings.Repeat("a", 64) + "/read", "/settings/backups", "/admin/accounts", "/account"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "quire-app") {
			t.Errorf("deep link %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/data/quire.db", "/mangabaka-oauth.json", "/assets/missing.js", "/admin/unknown", "/v1/missing"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Errorf("unexpected fallback %s: %d", path, w.Code)
		}
	}
}
