package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserCookieLifecycle(t *testing.T) {
	s, _ := fixture(t)
	h := NewHandler(s, "https://books.example", "Test")
	requestCookie := func(method, path, origin, session string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if session != "" {
			r.Header.Set("X-Quire-Session", session)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	body := `{"username":"alice","password":"` + testPassword + `","deviceName":"Browser"}`
	if w := requestCookie("POST", "/v1/browser/session", "https://evil.example", "", nil, body); w.Code != 403 {
		t.Fatal("cross-origin login", w.Code)
	}
	w := requestCookie("POST", "/v1/browser/session", "https://books.example", "", nil, body)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing cookie")
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge <= 0 || cookie.Path != "/" {
		t.Fatal("cookie attributes", cookie)
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatal("exposed token")
	}
	var v struct{ Session Session }
	json.Unmarshal(w.Body.Bytes(), &v)
	if w = requestCookie("GET", "/v1/browser/session", "", "", cookie, ""); w.Code != 200 {
		t.Fatal("reopen", w.Code)
	}
	if w = requestCookie("POST", "/v1/sync", "https://evil.example", v.Session.ID, cookie, `{}`); w.Code != 403 {
		t.Fatal("CSRF", w.Code)
	}
	if w = requestCookie("GET", "/v1/me", "", "wrong-session", cookie, ""); w.Code != 401 {
		t.Fatal("stale tab", w.Code)
	}
	operationBody, _ := json.Marshal(SyncRequest{Operations: []Operation{op("cookie-note", 0, false)}})
	if w = requestCookie("POST", "/v1/sync", "https://books.example", v.Session.ID, cookie, string(operationBody)); w.Code != 200 {
		t.Fatal("cookie sync", w.Code, w.Body.String())
	}
	s.db.Exec("UPDATE sessions SET expires_at=1 WHERE id=?", v.Session.ID)
	if w = requestCookie("GET", "/v1/browser/session", "", "", cookie, ""); w.Code != 401 {
		t.Fatal("expired cookie accepted", w.Code)
	}
	s.db.Exec("UPDATE sessions SET expires_at=? WHERE id=?", cookie.Expires.Unix(), v.Session.ID)
	s.db.Exec("UPDATE users SET disabled=1 WHERE username='alice'")
	if w = requestCookie("GET", "/v1/browser/session", "", "", cookie, ""); w.Code != 401 {
		t.Fatal("disabled user accepted", w.Code)
	}
	s.db.Exec("UPDATE users SET disabled=0 WHERE username='alice'")
	if w = requestCookie("DELETE", "/v1/browser/session", "https://books.example", v.Session.ID, cookie, ""); w.Code != 204 {
		t.Fatal("logout", w.Code)
	}
	if w = requestCookie("GET", "/v1/browser/session", "", "", cookie, ""); w.Code != 401 {
		t.Fatal("revoked cookie accepted", w.Code)
	}
}
