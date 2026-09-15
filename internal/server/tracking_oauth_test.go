package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOAuthExchangeAndRefresh(t *testing.T) {
	calls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		id, secret, ok := r.BasicAuth()
		if !ok || id != "client" || secret != "secret" {
			t.Error("missing client authentication")
		}
		if calls == 1 && (r.Form.Get("code_verifier") != "verifier" || r.Form.Get("redirect_uri") != "https://books.example/v1/tracking/oauth/callback") {
			t.Error("missing PKCE or redirect binding")
		}
		if calls == 2 && (r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-one") {
			t.Error("incorrect refresh request")
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access-new", "refresh_token": "refresh-one", "token_type": "Bearer", "expires_in": 3600, "scope": "library.read library.write offline_access"})
	}))
	defer provider.Close()
	cfg := oauthConfig{ClientID: "client", ClientSecret: "secret", tokenURL: provider.URL}
	c, err := cfg.exchange(context.Background(), provider.Client(), url.Values{"grant_type": {"authorization_code"}, "code": {"code"}, "code_verifier": {"verifier"}, "redirect_uri": {"https://books.example/v1/tracking/oauth/callback"}})
	if err != nil || c.AccessToken != "access-new" || c.ExpiresAt <= time.Now().Unix() {
		t.Fatal("exchange", err)
	}
	_, err = cfg.exchange(context.Background(), provider.Client(), url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken}})
	if err != nil || calls != 2 {
		t.Fatal("refresh", err)
	}
}

func TestOAuthStateSessionBindingAndReplay(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	o := trackingOAuth{pending: map[string]oauthPending{"state": {User: user, Session: "session", Nonce: "nonce", Expires: time.Now().Add(time.Minute), Code: "code", Received: true}}}
	if _, err := o.consume("state", user, "different-session"); err == nil {
		t.Fatal("accepted another session")
	}
	if _, err := o.consume("state", user, "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.consume("state", user, "session"); err == nil {
		t.Fatal("replayed state")
	}
	o.pending["expired"] = oauthPending{User: user, Session: "session", Received: true, Expires: time.Now().Add(-time.Second)}
	if _, err := o.consume("expired", user, "session"); err == nil {
		t.Fatal("accepted expired state")
	}
}

func TestOAuthUsesBearerInsteadOfPATHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer oauth-access" || r.Header.Get("x-api-key") != "" {
			t.Error("incorrect OAuth authentication")
		}
		w.Write([]byte(`{"data":{}}`))
	}))
	defer server.Close()
	p := newTrackingProvider()
	p.base = server.URL
	p.client = server.Client()
	_, _, err := p.call(context.Background(), "GET", "/profile", "oauth-access", nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestOAuthStartRequiresBrowserSession(t *testing.T) {
	s, _ := fixture(t)
	h := NewHandler(s, "https://books.example", "Test")
	r := httptest.NewRequest("POST", "/v1/tracking/oauth/start", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://books.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestOAuthBrowserRoundTrip(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	var session string
	s.db.QueryRow("SELECT id FROM sessions WHERE user_id=?", user).Scan(&session)
	exchanges := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			exchanges++
			r.ParseForm()
			if r.Form.Get("code_verifier") == "" {
				t.Error("no verifier")
			}
			w.Write([]byte(`{"access_token":"oauth-access","refresh_token":"refresh-secret","expires_in":3600,"token_type":"Bearer","scope":"library.read library.write"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer oauth-access" {
			t.Error("profile missing bearer")
		}
		w.Write([]byte(`{"data":{"id":"remote-user","preferred_username":"Reader"}}`))
	}))
	defer remote.Close()
	a := &api{store: s, publicOrigin: "https://books.example"}
	mux := http.NewServeMux()
	p := newTrackingProvider()
	p.base = remote.URL
	p.client = remote.Client()
	o := a.trackingOAuthRoutes(mux, p)
	o.config = oauthConfig{ClientID: "client", ClientSecret: "secret", tokenURL: remote.URL + "/token"}
	call := func(method, path, body string, cookies []*http.Cookie, sessionID string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", a.publicOrigin)
		r.Header.Set("X-Quire-Session", sessionID)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	sessionCookie := &http.Cookie{Name: a.cookieName(), Value: token}
	begin := call("POST", "/v1/tracking/oauth/start", `{}`, []*http.Cookie{sessionCookie}, session)
	if begin.Code != 200 {
		t.Fatal(begin.Code, begin.Body.String())
	}
	var start struct{ URL string }
	json.Unmarshal(begin.Body.Bytes(), &start)
	destination, _ := url.Parse(start.URL)
	q := destination.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || !strings.Contains(q.Get("scope"), "offline_access") || strings.Contains(start.URL, "secret") {
		t.Fatal("authorization parameters")
	}
	callback := "/v1/tracking/oauth/callback?state=" + q.Get("state") + "&code=code&iss=" + url.QueryEscape(mangaIssuer)
	if w := call("GET", callback, "", nil, ""); w.Code != 400 {
		t.Fatal("accepted missing browser nonce", w.Code)
	}
	finishBody := `{"state":"` + q.Get("state") + `"}`
	if w := call("POST", "/v1/tracking/oauth/finish", finishBody, []*http.Cookie{sessionCookie}, session); w.Code != 400 {
		t.Fatal("completed before callback")
	}
	cb := call("GET", callback, "", begin.Result().Cookies(), "")
	if cb.Code != 303 || exchanges != 0 {
		t.Fatal("callback exchanged tokens without original session", cb.Code)
	}
	if w := call("POST", "/v1/tracking/oauth/finish", finishBody, []*http.Cookie{sessionCookie}, "wrong"); w.Code != 401 {
		t.Fatal("accepted wrong session")
	}
	finish := call("POST", "/v1/tracking/oauth/finish", finishBody, []*http.Cookie{sessionCookie}, session)
	if finish.Code != 204 || exchanges != 1 {
		t.Fatal(finish.Code, finish.Body.String())
	}
	if w := call("POST", "/v1/tracking/oauth/finish", finishBody, []*http.Cookie{sessionCookie}, session); w.Code != 400 || exchanges != 1 {
		t.Fatal("replayed callback")
	}
	raw, err := s.trackingToken(user)
	if err != nil || !strings.Contains(raw, "refresh-secret") {
		t.Fatal("refresh token not retained", err)
	}
	var encrypted []byte
	s.db.QueryRow("SELECT secret FROM tracking_accounts WHERE user_id=?", user).Scan(&encrypted)
	if strings.Contains(string(encrypted), "refresh-secret") {
		t.Fatal("unencrypted refresh token")
	}
}

type oauthTestTransport func(*http.Request) (*http.Response, error)

func (f oauthTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestOAuthRefreshRotatesStoredCredentials(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	if err := os.WriteFile(filepath.Join(s.data, "mangabaka-oauth.json"), []byte(`{"clientId":"client","clientSecret":"secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(oauthCredential{AccessToken: "expired", RefreshToken: "old-refresh", ExpiresAt: 1})
	if err := s.saveTrackingToken(user, "remote", "Reader", string(old)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := newTrackingProvider()
	p.client = &http.Client{Transport: oauthTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		r.ParseForm()
		if r.Form.Get("refresh_token") != "old-refresh" {
			t.Error("wrong refresh token")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600,"token_type":"Bearer"}`)), Header: http.Header{}}, nil
	})}
	for i := 0; i < 2; i++ {
		access, err := s.trackingAccessToken(context.Background(), user, p)
		if err != nil || access != "new-access" {
			t.Fatal(access, err)
		}
	}
	if calls != 1 {
		t.Fatal("refreshed more than once", calls)
	}
	saved, err := s.trackingToken(user)
	if err != nil || !strings.Contains(saved, "rotated-refresh") {
		t.Fatal("rotated token not saved")
	}
}
