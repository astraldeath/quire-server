// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"net/http"
	"time"
)

type UserInfo struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Admin    bool   `json:"admin"`
	Disabled bool   `json:"disabled"`
}

func (s *Store) userInfo(id string) (UserInfo, error) {
	var u UserInfo
	err := s.db.QueryRow("SELECT id,username,admin,disabled FROM users WHERE id=?", id).Scan(&u.ID, &u.Username, &u.Admin, &u.Disabled)
	return u, err
}
func (s *Store) NeedsSetup() (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT count(*) FROM users WHERE admin=1").Scan(&n)
	return n == 0, err
}

// Registration and privilege assignment commit together; an invite is never consumed on failure.
func (s *Store) register(username, password, code string, setup bool) (string, error) {
	if !usernamePattern.MatchString(username) || !passwordValid(password) {
		return "", ErrInvalid
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := passwordKey(password, salt)
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if setup {
		var n int
		if err = tx.QueryRow("SELECT count(*) FROM users WHERE admin=1").Scan(&n); err != nil {
			return "", err
		}
		if n != 0 {
			return "", ErrConflict
		}
	}
	id := randomID()
	if _, err = tx.Exec("INSERT INTO users (id,username,salt,password_hash,admin) VALUES (?,?,?,?,?)", id, username, salt, key, setup); err != nil {
		return "", ErrConflict
	}
	if !setup {
		hash := sha256.Sum256([]byte(code))
		result, e := tx.Exec("UPDATE invites SET redeemed_by=? WHERE code_hash=? AND expires_at>? AND redeemed_by IS NULL AND revoked=0", id, hash[:], time.Now().Unix())
		if e != nil {
			return "", e
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return "", ErrInvalid
		}
		if _, err = tx.Exec("INSERT INTO library_members SELECT library_id,? FROM invite_libraries WHERE invite_id=(SELECT id FROM invites WHERE code_hash=?)", id, hash[:]); err != nil {
			return "", err
		}

	}
	if _, err = tx.Exec("INSERT INTO cursors VALUES (?,0)", id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}
func (a *api) admin(w http.ResponseWriter, r *http.Request) string {
	id := a.authorized(w, r)
	if id == "" {
		return ""
	}
	u, err := a.store.userInfo(id)
	if err != nil {
		failure(w, err)
		return ""
	}
	if !u.Admin {
		respond(w, 403, map[string]string{"error": "administrator access required"})
		return ""
	}
	return id
}
func (a *api) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/me/password", func(w http.ResponseWriter, r *http.Request) {
		id := a.authorized(w, r)
		if id == "" {
			return
		}
		if !a.allowLogin(r.RemoteAddr) {
			respond(w, 429, nil)
			return
		}
		select {
		case a.slots <- struct{}{}:
			defer func() { <-a.slots }()
		default:
			respond(w, 429, nil)
			return
		}
		var in struct {
			Current  string `json:"current"`
			Password string `json:"password"`
		}
		if !decode(w, r, &in, 4096) {
			return
		}
		if !passwordValid(in.Password) || len(in.Current) > 1024 {
			failure(w, ErrInvalid)
			return
		}
		tx, err := a.store.db.Begin()
		if err != nil {
			failure(w, err)
			return
		}
		defer tx.Rollback()
		var salt, key []byte
		if err = tx.QueryRow("SELECT salt,password_hash FROM users WHERE id=? AND disabled=0", id).Scan(&salt, &key); err != nil {
			failure(w, err)
			return
		}
		if subtle.ConstantTimeCompare(passwordKey(in.Current, salt), key) != 1 {
			failure(w, ErrUnauthorized)
			return
		}
		next := make([]byte, 16)
		if _, err = rand.Read(next); err != nil {
			failure(w, err)
			return
		}
		hash := passwordKey(in.Password, next)
		_, err = tx.Exec("UPDATE users SET salt=?,password_hash=? WHERE id=?", next, hash, id)
		if err == nil {
			_, err = tx.Exec("DELETE FROM sessions WHERE user_id=?", id)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})

	mux.HandleFunc("GET /v1/setup", func(w http.ResponseWriter, r *http.Request) {
		needed, err := a.store.NeedsSetup()
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]bool{"required": needed})
	})
	for _, route := range []string{"setup", "register"} {
		mux.HandleFunc("POST /v1/"+route, func(w http.ResponseWriter, r *http.Request) {
			if !a.allowLogin(r.RemoteAddr) {
				respond(w, 429, map[string]string{"error": "try again later"})
				return
			}
			select {
			case a.slots <- struct{}{}:
				defer func() { <-a.slots }()
			default:
				respond(w, 429, nil)
				return
			}
			var input struct {
				Username string `json:"username"`
				Password string `json:"password"`
				Code     string `json:"code"`
			}
			if !decode(w, r, &input, 4096) {
				return
			}
			setup := route == "setup"
			if setup && (a.setupCode == "" || subtle.ConstantTimeCompare([]byte(input.Code), []byte(a.setupCode)) != 1) {
				failure(w, ErrUnauthorized)
				return
			}
			_, err := a.store.register(input.Username, input.Password, input.Code, setup)
			if err != nil {
				failure(w, err)
				return
			}
			respond(w, 201, map[string]bool{"created": true})
		})
	}
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		id := a.authorized(w, r)
		if id == "" {
			return
		}
		u, err := a.store.userInfo(id)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, u)
	})
	mux.HandleFunc("GET /v1/admin/users", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		rows, err := a.store.db.Query("SELECT id,username,admin,disabled FROM users WHERE id NOT IN (SELECT owner FROM libraries) ORDER BY username")
		if err != nil {
			failure(w, err)
			return
		}
		defer rows.Close()
		out := []UserInfo{}
		for rows.Next() {
			var u UserInfo
			if err = rows.Scan(&u.ID, &u.Username, &u.Admin, &u.Disabled); err != nil {
				failure(w, err)
				return
			}
			out = append(out, u)
		}
		if err = rows.Err(); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, out)
	})
	mux.HandleFunc("PUT /v1/admin/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var in struct {
			Admin    bool `json:"admin"`
			Disabled bool `json:"disabled"`
		}
		if !decode(w, r, &in, 1024) {
			return
		}
		tx, err := a.store.db.Begin()
		if err != nil {
			failure(w, err)
			return
		}
		defer tx.Rollback()
		id := r.PathValue("id")
		var current bool
		if err = tx.QueryRow("SELECT admin FROM users WHERE id=? AND id NOT IN (SELECT owner FROM libraries)", id).Scan(&current); err != nil {
			failure(w, err)
			return
		}
		if current && (!in.Admin || in.Disabled) {
			var n int
			err = tx.QueryRow("SELECT count(*) FROM users WHERE admin=1 AND disabled=0 AND id<>?", id).Scan(&n)
			if err != nil {
				failure(w, err)
				return
			}
			if n == 0 {
				respond(w, 409, map[string]string{"error": "keep at least one active admin"})
				return
			}
		}
		if _, err = tx.Exec("UPDATE users SET admin=?,disabled=? WHERE id=?", in.Admin, in.Disabled, id); err == nil {
			_, err = tx.Exec("DELETE FROM sessions WHERE user_id=?", id)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("DELETE /v1/admin/users/{id}/sessions", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		if _, err := a.store.db.Exec("DELETE FROM sessions WHERE user_id=?", r.PathValue("id")); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("POST /v1/admin/invites", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		code, id := randomID(), randomID()
		hash := sha256.Sum256([]byte(code))
		expires := time.Now().Add(7 * 24 * time.Hour).Unix()
		if _, err := a.store.db.Exec("INSERT INTO invites(id,code_hash,expires_at) VALUES (?,?,?)", id, hash[:], expires); err != nil {
			failure(w, err)
			return
		}
		respond(w, 201, map[string]any{"id": id, "code": code, "expiresAt": expires})
	})
	mux.HandleFunc("GET /v1/admin/invites", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		rows, err := a.store.db.Query("SELECT id,expires_at,redeemed_by,revoked FROM invites ORDER BY expires_at DESC LIMIT 200")
		if err != nil {
			failure(w, err)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id string
			var exp int64
			var used sql.NullString
			var revoked bool
			if err = rows.Scan(&id, &exp, &used, &revoked); err != nil {
				failure(w, err)
				return
			}
			status := "pending"
			if revoked {
				status = "revoked"
			} else if used.Valid {
				status = "redeemed"
			} else if exp <= time.Now().Unix() {
				status = "expired"
			}
			out = append(out, map[string]any{"id": id, "expiresAt": exp, "status": status})
		}
		if err = rows.Err(); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, out)
	})
	mux.HandleFunc("DELETE /v1/admin/invites/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		if _, err := a.store.db.Exec("UPDATE invites SET revoked=1 WHERE id=? AND redeemed_by IS NULL", r.PathValue("id")); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
}

// Promote is a local owner command for upgrading an existing installation.
func (s *Store) Promote(username string) error {
	result, err := s.db.Exec("UPDATE users SET admin=1,disabled=0 WHERE username=? AND id NOT IN (SELECT owner FROM libraries)", username)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}
