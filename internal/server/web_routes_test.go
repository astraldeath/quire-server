package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebRouteDeepLinksAndPrivatePaths(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("quire-app"), 0600)
	h := WebUI(http.NotFoundHandler(), dir)
	for _, path := range []string{"/library", "/reading", "/series/A%2FB/tracking", "/books/" + strings.Repeat("a", 64) + "/read", "/settings/backups", "/settings/server", "/settings/statistics", "/settings/privacy", "/settings/updates", "/admin/accounts", "/account"} {
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

func TestWebPolicySupportsArchiveWorkers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("quire-app"), 0600)
	w := httptest.NewRecorder()
	WebUI(http.NotFoundHandler(), dir).ServeHTTP(w, httptest.NewRequest("GET", "/library", nil))
	policy := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "script-src 'self' 'wasm-unsafe-eval'") || !strings.Contains(policy, "worker-src 'self'") {
		t.Fatalf("archive worker policy unavailable: %s", policy)
	}
}
