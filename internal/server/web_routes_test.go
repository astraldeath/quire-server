package server

import (
	"bytes"
	"encoding/json"
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
	for _, path := range []string{"/library", "/reading", "/catalogs", "/catalogs?source=saved-source", "/series/A%2FB/tracking", "/books/" + strings.Repeat("a", 64) + "/read", "/settings/backups", "/settings/server", "/settings/statistics", "/settings/privacy", "/settings/updates", "/admin/accounts", "/account"} {
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

func TestWebUIForwardsOPDSCatalogsAndResources(t *testing.T) {
	_, api := fixture(t)
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("quire-app"), 0600); err != nil {
		t.Fatal(err)
	}
	h := WebUI(api, directory)
	token := login(t, h, "alice")
	created := request(t, h, "POST", "/v1/opds/passwords", token, map[string]string{"name": "Reader"}, 201)
	var password string
	if err := json.Unmarshal(created["password"], &password); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/opds", "/opds/", "/opds/v2", "/opds/search.xml"} {
		w := opdsRequest(h, path, "alice", password)
		if w.Code != 200 || strings.Contains(w.Body.String(), "quire-app") {
			t.Errorf("catalog %s: %d %s", path, w.Code, w.Body.String())
		}
		w = opdsRequest(h, path, "", "")
		if w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("catalog challenge %s: %d", path, w.Code)
		}
	}
	data := epubBytes()
	id := hashBook(data)
	upload := httptest.NewRequest("PUT", "/v1/books/"+id+"/file", bytes.NewReader(data))
	upload.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, upload)
	if w.Code != 201 {
		t.Fatal("upload", w.Code, w.Body.String())
	}
	w = opdsRequest(h, "/opds/books/"+id+"/file", "alice", password)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("OPDS file", w.Code)
	}
	for _, resource := range []string{"file", "cover"} {
		w = opdsRequest(h, "/opds/books/"+id+"/"+resource, "", "")
		if w.Code != 401 {
			t.Fatal("OPDS resource not routed", resource, w.Code)
		}
	}
}
