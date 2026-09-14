package server

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *api) cookieName() string {
	if strings.HasPrefix(a.publicOrigin, "https://") {
		return "__Host-quire-session"
	}
	return "quire-session"
}
func (a *api) setBrowserCookie(w http.ResponseWriter, token string, expires int64) {
	age := int(expires - time.Now().Unix())
	if token == "" {
		age = -1
	}
	http.SetCookie(w, &http.Cookie{Name: a.cookieName(), Value: token, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.publicOrigin, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: age, Expires: time.Unix(expires, 0)})
}
func (a *api) browserOrigin(w http.ResponseWriter, r *http.Request) bool {
	if a.publicOrigin == "" || r.Header.Get("Origin") != a.publicOrigin {
		respond(w, 403, map[string]string{"error": "same-origin browser request required"})
		return false
	}
	return true
}
func (a *api) cookieSession(r *http.Request) (string, Session, error) {
	c, err := r.Cookie(a.cookieName())
	if err != nil {
		return "", Session{}, ErrUnauthorized
	}
	user, err := a.store.authenticate(c.Value)
	if err != nil {
		return "", Session{}, err
	}
	hash := sha256.Sum256([]byte(c.Value))
	var session Session
	err = a.store.db.QueryRow("SELECT id,device_name,created_at,expires_at FROM sessions WHERE token_hash=? AND user_id=?", hash[:], user).Scan(&session.ID, &session.DeviceName, &session.CreatedAt, &session.ExpiresAt)
	return user, session, err
}
func (a *api) browserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/browser/session", func(w http.ResponseWriter, r *http.Request) {
		if !a.browserOrigin(w, r) {
			return
		}
		if !a.allowLogin(r.RemoteAddr) {
			respond(w, 429, map[string]string{"error": "too many login attempts"})
			return
		}
		select {
		case a.slots <- struct{}{}:
			defer func() { <-a.slots }()
		default:
			respond(w, 429, map[string]string{"error": "login busy"})
			return
		}
		var input struct {
			Username   string `json:"username"`
			Password   string `json:"password"`
			DeviceName string `json:"deviceName"`
		}
		if !decode(w, r, &input, 4096) {
			return
		}
		result, err := a.store.Login(input.Username, input.Password, input.DeviceName)
		if err != nil {
			failure(w, err)
			return
		}
		if user, old, e := a.cookieSession(r); e == nil {
			_ = a.store.revoke(user, old.ID)
		}
		a.setBrowserCookie(w, result.Token, result.Session.ExpiresAt)
		respond(w, 201, map[string]any{"session": result.Session})
	})
	mux.HandleFunc("GET /v1/browser/session", func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && origin != a.publicOrigin {
			respond(w, 403, map[string]string{"error": "same-origin browser request required"})
			return
		}
		user, session, err := a.cookieSession(r)
		if err != nil {
			failure(w, err)
			return
		}
		var username string
		if err = a.store.db.QueryRow("SELECT username FROM users WHERE id=?", user).Scan(&username); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"username": username, "sessionId": session.ID, "expiresAt": session.ExpiresAt})
	})
	mux.HandleFunc("DELETE /v1/browser/session", func(w http.ResponseWriter, r *http.Request) {
		if !a.browserOrigin(w, r) {
			return
		}
		user, session, err := a.cookieSession(r)
		if err != nil && !errors.Is(err, ErrUnauthorized) {
			failure(w, err)
			return
		}
		if err == nil {
			if r.Header.Get("X-Quire-Session") != session.ID {
				failure(w, ErrUnauthorized)
				return
			}
			if err = a.store.revoke(user, session.ID); err != nil {
				failure(w, err)
				return
			}
		}
		a.setBrowserCookie(w, "", 1)
		respond(w, 204, nil)
	})
}
