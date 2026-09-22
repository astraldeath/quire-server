package server

import (
	"crypto/sha256"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type opdsPassword struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"createdAt"`
	Password  string `json:"password,omitempty"`
}

func (a *api) opdsAuthorized(w http.ResponseWriter, r *http.Request) string {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	origin, e := url.Parse(a.publicOrigin)
	if e != nil || (origin.Scheme != "https" && !(origin.Scheme == "http" && (origin.Hostname() == "localhost" || net.ParseIP(origin.Hostname()).IsLoopback()))) {
		respond(w, 503, map[string]string{"error": "OPDS requires an HTTPS public URL"})
		return ""
	}
	username, password, ok := r.BasicAuth()
	var user string
	if ok && len(password) <= 256 && len(username) <= 64 {
		digest := sha256.Sum256([]byte(password))
		e = a.store.db.QueryRow("SELECT u.id FROM opds_passwords p JOIN users u ON u.id=p.user_id WHERE u.username=? AND u.disabled=0 AND p.digest=?", username, digest[:]).Scan(&user)
		if e == nil {
			return user
		}
	}
	if !a.allowLogin(r.RemoteAddr) {
		w.Header().Set("Retry-After", "60")
		respond(w, 429, map[string]string{"error": "too many authentication attempts"})
		return ""
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="Quire OPDS", charset="UTF-8"`)
	failure(w, ErrUnauthorized)
	return ""
}
func (a *api) opdsPasswordRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/opds/passwords", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		rows, e := a.store.db.Query("SELECT id,name,created_at FROM opds_passwords WHERE user_id=? ORDER BY created_at,id", user)
		if e != nil {
			failure(w, e)
			return
		}
		defer rows.Close()
		out := []opdsPassword{}
		for rows.Next() {
			var v opdsPassword
			if e = rows.Scan(&v.ID, &v.Name, &v.CreatedAt); e != nil {
				failure(w, e)
				return
			}
			out = append(out, v)
		}
		if e = rows.Err(); e != nil {
			failure(w, e)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		respond(w, 200, map[string]any{"passwords": out})
	})
	mux.HandleFunc("POST /v1/opds/passwords", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		var in struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &in, 1024) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if len(in.Name) == 0 || len(in.Name) > 100 {
			failure(w, ErrInvalid)
			return
		}
		out := opdsPassword{ID: randomID(), Name: in.Name, CreatedAt: time.Now().UnixMilli(), Password: randomID()}
		digest := sha256.Sum256([]byte(out.Password))
		result, e := a.store.db.Exec("INSERT INTO opds_passwords SELECT ?,?,?,?,? WHERE (SELECT count(*) FROM opds_passwords WHERE user_id=?)<50", out.ID, user, out.Name, digest[:], out.CreatedAt, user)
		if e != nil {
			failure(w, e)
			return
		}
		if n, _ := result.RowsAffected(); n == 0 {
			failure(w, ErrConflict)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		respond(w, 201, out)
	})
	mux.HandleFunc("DELETE /v1/opds/passwords/{id}", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		if _, e := a.store.db.Exec("DELETE FROM opds_passwords WHERE user_id=? AND id=?", user, r.PathValue("id")); e != nil {
			failure(w, e)
			return
		}
		respond(w, 204, nil)
	})
}
