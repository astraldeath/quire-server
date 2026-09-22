// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type loginWindow struct {
	count int
	until time.Time
}
type api struct {
	updates      *updateChecker
	publicOrigin string
	setupCode    string
	store        *Store
	loginMu      sync.Mutex
	attempts     map[string]loginWindow
	slots        chan struct{}
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if value != nil {
		json.NewEncoder(w).Encode(value)
	}
}
func failure(w http.ResponseWriter, err error) {
	status := 500
	message := "internal server error"
	switch {
	case errors.Is(err, ErrUploadTooLarge):
		status = http.StatusRequestEntityTooLarge
		message = "book exceeds configured upload limit"
	case errors.Is(err, ErrInvalid):
		status = 400
		message = "invalid request"
	case errors.Is(err, ErrUnauthorized):
		status = 401
		message = "invalid credentials or expired session"
	case errors.Is(err, ErrConflict):
		status = 409
		message = "revision, operation ID, or capacity conflict"
	case errors.Is(err, sql.ErrNoRows):
		status = 404
		message = "not found"
	}
	respond(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		respond(w, 415, map[string]string{"error": "application/json required"})
		return false
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		respond(w, 413, map[string]string{"error": "request too large"})
		return false
	}
	if strictJSON(b, v) != nil {
		failure(w, ErrInvalid)
		return false
	}
	return true
}
func (a *api) authorized(w http.ResponseWriter, r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		if _, err := r.Cookie(a.cookieName()); err != nil {
			failure(w, ErrUnauthorized)
			return ""
		}
		if r.Method != "GET" && r.Method != "HEAD" && !a.browserOrigin(w, r) {
			return ""
		}
		user, session, err := a.cookieSession(r)
		if err != nil {
			failure(w, err)
			return ""
		}
		if r.Header.Get("X-Quire-Session") != session.ID {
			failure(w, ErrUnauthorized)
			return ""
		}
		return user
	}
	if !strings.HasPrefix(auth, "Bearer ") {
		failure(w, ErrUnauthorized)
		return ""
	}
	user, err := a.store.authenticate(strings.TrimPrefix(auth, "Bearer "))
	if err != nil {
		failure(w, err)
		return ""
	}
	return user
}

// Forwarded IP headers are deliberately ignored. A reverse proxy must enforce
// its own per-client limit; the service also limits its immediate peer.
func (a *api) allowLogin(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	now := time.Now()
	for k, v := range a.attempts {
		if now.After(v.until) {
			delete(a.attempts, k)
		}
	}
	v, exists := a.attempts[host]
	if !exists {
		if len(a.attempts) >= 4096 {
			return false
		}
		v.until = now.Add(time.Minute)
	}
	if v.count >= 10 {
		return false
	}
	v.count++
	a.attempts[host] = v
	return true
}
func NewHandler(store *Store, publicURL, name string) http.Handler {
	return NewConfiguredHandler(store, publicURL, name, "")
}
func NewConfiguredHandler(store *Store, publicURL, name, setupCode string) http.Handler {
	a := &api{publicOrigin: strings.TrimRight(publicURL, "/"), setupCode: setupCode, store: store, attempts: map[string]loginWindow{}, slots: make(chan struct{}, 2)}
	a.updates = newUpdateChecker()
	mux := http.NewServeMux()
	a.updateRoutes(mux)
	a.browserRoutes(mux)
	a.fileRoutes(mux)
	a.backupRoutes(mux)
	a.adminRoutes(mux)
	a.libraryRoutes(mux)
	a.trackingRoutes(mux)
	a.statisticsRoutes(mux)
	a.privacyRoutes(mux)
	a.folderCatalogRoutes(mux)
	a.catalogSourceRoutes(mux)
	a.catalogProxyRoutes(mux)
	a.opdsRoutes(mux)
	a.settingsRoutes(mux, name)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.db.PingContext(r.Context()); err != nil {
			respond(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		respond(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /.well-known/quire", func(w http.ResponseWriter, r *http.Request) {
		settings, err := store.Settings(ServerSettings{name, 300})
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"name": settings.Name, "apiVersion": "1", "apiUrl": strings.TrimRight(publicURL, "/") + "/v1", "registration": "owner-only", "limits": map[string]int64{"maxUploadBytes": store.maxUploadBytes, "maxDownloadBytes": MaxStoredBookBytes}, "capabilities": []string{"server-assigned-upload", "reading-data-sync", "device-sessions", "epub-files", "watched-folders", "multiple-folders", "current-chapter", "folder-catalog", "opds"}})
	})
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !a.allowLogin(r.RemoteAddr) {
			w.Header().Set("Retry-After", "60")
			respond(w, 429, map[string]string{"error": "too many login attempts"})
			return
		}
		select {
		case a.slots <- struct{}{}:
			defer func() { <-a.slots }()
		default:
			w.Header().Set("Retry-After", "2")
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
		out, err := store.Login(input.Username, input.Password, input.DeviceName)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 201, out)
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		out, err := store.sessions(user)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"sessions": out})
	})
	mux.HandleFunc("DELETE /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		if err := store.revoke(user, r.PathValue("id")); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("POST /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		var req SyncRequest
		if !decode(w, r, &req, 2<<20) {
			return
		}
		out, err := store.Sync(r.Context(), user, req)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, out)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { failure(w, sql.ErrNoRows) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}
