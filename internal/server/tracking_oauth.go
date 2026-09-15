package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const mangaIssuer = "https://mangabaka.org/auth"

var errOAuthReconnect = errors.New("Reconnect MangaBaka to resume tracking.")

type oauthConfig struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	tokenURL     string
}

func (s *Store) oauthConfig() oauthConfig {
	if s == nil {
		return oauthConfig{}
	}
	var cfg oauthConfig
	b, err := os.ReadFile(filepath.Join(s.data, "mangabaka-oauth.json"))
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		return oauthConfig{}
	}
	return cfg
}
func (c oauthConfig) enabled() bool { return c.ClientID != "" && c.ClientSecret != "" }

type oauthCredential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	RetryAt      int64  `json:"retry_at,omitempty"`
	Reconnect    bool   `json:"reconnect,omitempty"`
}

func (c oauthConfig) exchange(ctx context.Context, client *http.Client, values url.Values) (oauthCredential, error) {
	endpoint := c.tokenURL
	if endpoint == "" {
		endpoint = mangaIssuer + "/oauth2/token"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return oauthCredential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	res, err := client.Do(req)
	if err != nil {
		return oauthCredential{}, errors.New("Cannot reach MangaBaka. Try connecting again.")
	}
	defer res.Body.Close()
	if res.StatusCode == 400 || res.StatusCode == 401 || res.StatusCode == 403 {
		return oauthCredential{}, errOAuthReconnect
	}
	if res.StatusCode != 200 {
		return oauthCredential{}, errors.New("MangaBaka is temporarily unavailable. Try again shortly.")
	}
	var v struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(b) > 65536 || json.Unmarshal(b, &v) != nil || v.AccessToken == "" || !strings.EqualFold(v.TokenType, "Bearer") || v.ExpiresIn <= 0 || v.ExpiresIn > 365*24*3600 {
		return oauthCredential{}, errors.New("MangaBaka returned an invalid authorization response.")
	}
	if v.Scope != "" && (!hasOAuthScope(v.Scope, "library.read") || !hasOAuthScope(v.Scope, "library.write")) {
		return oauthCredential{}, errors.New("MangaBaka needs library read and write permission.")
	}
	return oauthCredential{AccessToken: v.AccessToken, RefreshToken: v.RefreshToken, ExpiresAt: time.Now().Unix() + v.ExpiresIn}, nil
}
func hasOAuthScope(scopes, want string) bool {
	for _, s := range strings.Fields(scopes) {
		if s == want {
			return true
		}
	}
	return false
}
func oauthRandom() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type oauthPending struct {
	User, Session, Nonce, Verifier, Code, Error string
	Expires                                     time.Time
	Received                                    bool
}
type trackingOAuth struct {
	mu       sync.Mutex
	pending  map[string]oauthPending
	config   oauthConfig
	api      *api
	provider *trackingProvider
}

func (o *trackingOAuth) consume(state, user, session string) (oauthPending, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.pending[state]
	if !ok || time.Now().After(p.Expires) || p.User != user || p.Session != session || !p.Received {
		return oauthPending{}, ErrUnauthorized
	}
	delete(o.pending, state)
	return p, nil
}
func (o *trackingOAuth) cookieName() string {
	if strings.HasPrefix(o.api.publicOrigin, "https://") {
		return "__Host-quire-mangabaka"
	}
	return "quire-mangabaka"
}
func (a *api) trackingOAuthRoutes(mux *http.ServeMux, p *trackingProvider) *trackingOAuth {
	o := &trackingOAuth{pending: map[string]oauthPending{}, config: a.store.oauthConfig(), api: a, provider: p}
	mux.HandleFunc("POST /v1/tracking/oauth/start", o.start)
	mux.HandleFunc("GET /v1/tracking/oauth/callback", o.callback)
	mux.HandleFunc("POST /v1/tracking/oauth/finish", o.finish)
	return o
}
func (o *trackingOAuth) start(w http.ResponseWriter, r *http.Request) {
	u := o.api.authorized(w, r)
	if u == "" {
		return
	}
	cookieUser, session, err := o.api.cookieSession(r)
	if err != nil || cookieUser != u {
		failure(w, ErrUnauthorized)
		return
	}
	if !o.config.enabled() {
		respond(w, 503, map[string]string{"error": "MangaBaka OAuth has not been configured on this server."})
		return
	}
	state, nonce, verifier := oauthRandom(), oauthRandom(), oauthRandom()
	o.mu.Lock()
	for key, p := range o.pending {
		if time.Now().After(p.Expires) || p.Session == session.ID {
			delete(o.pending, key)
		}
	}
	if len(o.pending) >= 1024 {
		o.mu.Unlock()
		respond(w, 429, map[string]string{"error": "Too many connections in progress. Try again later."})
		return
	}
	o.pending[state] = oauthPending{User: u, Session: session.ID, Nonce: nonce, Verifier: verifier, Expires: time.Now().Add(10 * time.Minute)}
	o.mu.Unlock()
	hash := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {o.config.ClientID}, "response_type": {"code"}, "redirect_uri": {o.api.publicOrigin + "/v1/tracking/oauth/callback"}, "scope": {"openid profile library.read library.write offline_access"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}, "prompt": {"consent"}}
	http.SetCookie(w, &http.Cookie{Name: o.cookieName(), Value: nonce, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(o.api.publicOrigin, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	w.Header().Set("Cache-Control", "no-store")
	respond(w, 200, map[string]string{"url": mangaIssuer + "/oauth2/authorize?" + q.Encode()})
}
func (o *trackingOAuth) callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := r.URL.Query()
	state := q.Get("state")
	cookie, e := r.Cookie(o.cookieName())
	o.mu.Lock()
	p, ok := o.pending[state]
	if e != nil || !ok || p.Received || time.Now().After(p.Expires) || cookie.Value != p.Nonce || len(q.Get("code")) > 8192 || (q.Get("iss") != "" && q.Get("iss") != mangaIssuer) {
		o.mu.Unlock()
		http.Error(w, "This connection expired or belongs to another browser. Return to Quire and connect again.", 400)
		return
	}
	p.Received = true
	p.Code = q.Get("code")
	if q.Get("error") != "" || p.Code == "" {
		p.Error = "MangaBaka connection was cancelled. Your existing links are unchanged."
	}
	o.pending[state] = p
	o.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: o.cookieName(), Value: "", Path: "/", HttpOnly: true, Secure: strings.HasPrefix(o.api.publicOrigin, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	// Finish on a same-origin request with the original Strict session and session ID.
	// No tokens are exchanged or accounts changed by this cross-site GET.
	http.Redirect(w, r, "/?tracking_oauth="+url.QueryEscape(state), http.StatusSeeOther)
}
func (o *trackingOAuth) finish(w http.ResponseWriter, r *http.Request) {
	u := o.api.authorized(w, r)
	if u == "" {
		return
	}
	cookieUser, session, err := o.api.cookieSession(r)
	if err != nil || cookieUser != u {
		failure(w, ErrUnauthorized)
		return
	}
	var in struct {
		State string `json:"state"`
	}
	if !decode(w, r, &in, 1024) {
		return
	}
	pending, err := o.consume(in.State, u, session.ID)
	if err != nil {
		respond(w, 400, map[string]string{"error": "Connection expired or the Quire account changed. Connect again."})
		return
	}
	if pending.Error != "" {
		respond(w, 400, map[string]string{"error": pending.Error})
		return
	}
	o.api.store.trackingMu.Lock()
	defer o.api.store.trackingMu.Unlock()
	credential, err := o.config.exchange(r.Context(), o.provider.client, url.Values{"grant_type": {"authorization_code"}, "code": {pending.Code}, "code_verifier": {pending.Verifier}, "redirect_uri": {o.api.publicOrigin + "/v1/tracking/oauth/callback"}})
	if err != nil {
		respond(w, 502, map[string]string{"error": err.Error()})
		return
	}
	if credential.RefreshToken == "" {
		respond(w, 400, map[string]string{"error": "MangaBaka did not grant offline access. Enable refresh_token for the OAuth client and reconnect."})
		return
	}
	data, _, err := o.provider.call(r.Context(), "GET", "/v1/my/profile", credential.AccessToken, nil)
	if err != nil {
		respond(w, 502, map[string]string{"error": err.Error()})
		return
	}
	var profile struct {
		ID                string
		Nickname          string
		PreferredUsername string `json:"preferred_username"`
	}
	if json.Unmarshal(data, &profile) != nil || profile.ID == "" {
		failure(w, ErrInvalid)
		return
	}
	var previous string
	err = o.api.store.db.QueryRow("SELECT provider_id FROM tracking_accounts WHERE user_id=?", u).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		failure(w, err)
		return
	}
	if previous != "" && previous != profile.ID {
		respond(w, 409, map[string]string{"error": "Disconnect your current MangaBaka account before connecting a different one."})
		return
	}
	name := profile.PreferredUsername
	if name == "" {
		name = profile.Nickname
	}
	if name == "" {
		name = "MangaBaka account"
	}
	b, _ := json.Marshal(credential)
	if err = o.api.store.saveTrackingToken(u, profile.ID, name, string(b)); err != nil {
		failure(w, err)
		return
	}
	respond(w, 204, nil)
}

// Called while trackingMu is held, so refresh-token rotation is serialized.
func (s *Store) trackingAccessToken(ctx context.Context, user string, p *trackingProvider) (string, error) {
	raw, err := s.trackingToken(user)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(raw, "{") {
		return raw, nil
	} // Existing PAT links remain usable.
	var credential oauthCredential
	if json.Unmarshal([]byte(raw), &credential) != nil {
		return "", errOAuthReconnect
	}
	if credential.Reconnect {
		return "", errOAuthReconnect
	}
	now := time.Now().Unix()
	if credential.ExpiresAt > now+60 {
		return credential.AccessToken, nil
	}
	if credential.RetryAt > now {
		return "", errors.New("MangaBaka refresh is pending. It will retry shortly.")
	}
	cfg := s.oauthConfig()
	if !cfg.enabled() || credential.RefreshToken == "" {
		return "", errOAuthReconnect
	}
	next, refreshErr := cfg.exchange(ctx, p.client, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {credential.RefreshToken}})
	if refreshErr != nil {
		credential.RetryAt = now + 60
		credential.Reconnect = errors.Is(refreshErr, errOAuthReconnect)
		next = credential
	} else if next.RefreshToken == "" {
		next.RefreshToken = credential.RefreshToken
	}
	var id, name string
	if err = s.db.QueryRow("SELECT provider_id,name FROM tracking_accounts WHERE user_id=?", user).Scan(&id, &name); err != nil {
		return "", err
	}
	b, _ := json.Marshal(next)
	if err = s.saveTrackingToken(user, id, name, string(b)); err != nil {
		return "", err
	}
	if refreshErr != nil {
		return "", refreshErr
	}
	return next.AccessToken, nil
}
