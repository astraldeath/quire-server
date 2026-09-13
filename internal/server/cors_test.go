package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowserOriginsAreExplicit(t *testing.T) {
	h := WithCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }), []string{"http://localhost:1420"})
	for _, origin := range []string{"http://localhost:1420", "https://evil.example"} {
		r := httptest.NewRequest("OPTIONS", "/v1/sync", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if origin == "http://localhost:1420" {
			if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Fatal("allowed origin blocked")
			}
		} else if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("untrusted origin allowed")
		}
	}
}
