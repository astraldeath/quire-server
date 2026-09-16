// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type LibraryInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Owner   string   `json:"-"`
	Members []string `json:"members"`
	Books   int      `json:"books"`
	BookIDs []string `json:"bookIds"`
}

func (s *Store) accessibleOwner(user, book string) (string, error) {
	var owner string
	err := s.db.QueryRow(`SELECT user_id FROM files WHERE book_id=? AND (user_id=? OR user_id IN(SELECT l.owner FROM libraries l JOIN library_members m ON m.library_id=l.id WHERE m.user_id=?)) ORDER BY user_id=? DESC LIMIT 1`, book, user, user, user).Scan(&owner)
	return owner, err
}
func (s *Store) sharedSeeds(user string) error {
	rows, err := s.db.Query(`SELECT DISTINCT f.user_id,f.book_id FROM files f JOIN libraries l ON l.owner=f.user_id JOIN library_members m ON m.library_id=l.id WHERE m.user_id=? AND NOT EXISTS(SELECT 1 FROM records r WHERE r.user_id=m.user_id AND r.book_id=f.book_id AND r.kind='book')`, user)
	if err != nil {
		return err
	}
	type item struct{ owner, id string }
	items := []item{}
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.owner, &i.id); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Archive parsing and thumbnail generation must not hold the sole DB connection.
	metadata := make([]bookMetadata, len(items))
	for n, i := range items {
		metadata[n] = s.objectMetadata(i.owner, i.id)
		metadata[n].Cover = "" // Seed records never contain thumbnails.
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for n, i := range items {
		// Membership or availability may have changed while metadata was decoded.
		var available bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM files f JOIN libraries l ON l.owner=f.user_id JOIN library_members m ON m.library_id=l.id WHERE m.user_id=? AND f.user_id=? AND f.book_id=?)`, user, i.owner, i.id).Scan(&available); err != nil {
			return err
		}
		if !available {
			continue
		}
		meta := metadata[n]
		var raw string
		if e := tx.QueryRow("SELECT candidates FROM records WHERE user_id=? AND book_id=? AND kind='book' AND record_id='default'", i.owner, i.id).Scan(&raw); e == nil {
			var candidates []Candidate
			if json.Unmarshal([]byte(raw), &candidates) == nil && len(candidates) == 1 && !candidates[0].Deleted {
				_ = json.Unmarshal(candidates[0].Value, &meta)
			}
		}
		if err = seedBook(tx, user, i.id, "Untitled", meta); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (a *api) libraryRoutes(mux *http.ServeMux) {
	a.libraryManagementRoutes(mux)
	mux.HandleFunc("GET /v1/admin/libraries/{library}/books", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var owner string
		if err := a.store.db.QueryRow("SELECT owner FROM libraries WHERE id=?", r.PathValue("library")).Scan(&owner); err != nil {
			failure(w, err)
			return
		}
		files, err := a.store.fileList(owner)
		if err != nil {
			failure(w, err)
			return
		}
		out := []map[string]any{}
		for _, f := range files {
			m := a.store.objectMetadata(owner, f.BookID)
			var raw string
			if e := a.store.db.QueryRow("SELECT candidates FROM records WHERE user_id=? AND book_id=? AND kind='book'", owner, f.BookID).Scan(&raw); e == nil {
				var c []Candidate
				if json.Unmarshal([]byte(raw), &c) == nil && len(c) == 1 {
					_ = json.Unmarshal(c[0].Value, &m)
				}
			}
			out = append(out, map[string]any{"id": f.BookID, "title": m.Title, "author": m.Author, "series": m.Series, "volume": m.Volume, "uploaded": f.Uploaded, "watched": f.Watched})
		}
		respond(w, 200, out)
	})
	mux.HandleFunc("PUT /v1/admin/libraries/{library}/books/{id}/metadata", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var value json.RawMessage
		if !decode(w, r, &value, 32768) {
			return
		}
		id := r.PathValue("id")
		op := Operation{ID: randomID(), BookID: id, Kind: "book", RecordID: "default", Value: value}
		if err := validateOperation(op); err != nil {
			failure(w, err)
			return
		}
		var owner string
		if err := a.store.db.QueryRow("SELECT owner FROM libraries WHERE id=?", r.PathValue("library")).Scan(&owner); err != nil {
			failure(w, err)
			return
		}
		if _, err := a.store.accessibleOwner(owner, id); err != nil {
			failure(w, err)
			return
		}
		var rev int64
		_ = a.store.db.QueryRow("SELECT revision FROM records WHERE user_id=? AND book_id=? AND kind='book' AND record_id='default'", owner, id).Scan(&rev)
		op.BaseRevision = rev
		if _, err := a.store.Sync(context.Background(), owner, SyncRequest{Operations: []Operation{op}}); err != nil {
			failure(w, err)
			return
		}
		rows, err := a.store.db.Query("SELECT user_id FROM library_members WHERE library_id=?", r.PathValue("library"))
		if err != nil {
			failure(w, err)
			return
		}
		members := []string{}
		for rows.Next() {
			var member string
			if err = rows.Scan(&member); err != nil {
				rows.Close()
				failure(w, err)
				return
			}
			members = append(members, member)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			failure(w, err)
			return
		}
		for _, member := range members {
			var revision int64
			_ = a.store.db.QueryRow("SELECT revision FROM records WHERE user_id=? AND book_id=? AND kind='book' AND record_id='default'", member, id).Scan(&revision)
			change := op
			change.ID = randomID()
			change.BaseRevision = revision
			if _, err = a.store.Sync(context.Background(), member, SyncRequest{Operations: []Operation{change}}); err != nil {
				failure(w, err)
				return
			}
		}
		respond(w, 204, nil)
	})

	mux.HandleFunc("GET /v1/libraries", func(w http.ResponseWriter, r *http.Request) {
		u := a.authorized(w, r)
		if u == "" {
			return
		}
		a.listLibraries(w, u, false)
	})
	mux.HandleFunc("GET /v1/admin/libraries", func(w http.ResponseWriter, r *http.Request) {
		u := a.admin(w, r)
		if u == "" {
			return
		}
		a.listLibraries(w, u, true)
	})
	mux.HandleFunc("POST /v1/admin/libraries", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var in struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &in, 1024) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 100 {
			failure(w, ErrInvalid)
			return
		}
		id, owner := randomID(), randomID()
		tx, err := a.store.db.Begin()
		if err != nil {
			failure(w, err)
			return
		}
		defer tx.Rollback()
		_, err = tx.Exec("INSERT INTO users(id,username,salt,password_hash,disabled) VALUES (?,?,?, ?,1)", owner, "library."+id, []byte{}, []byte{})
		if err == nil {
			_, err = tx.Exec("INSERT INTO cursors VALUES (?,0)", owner)
		}
		if err == nil {
			_, err = tx.Exec("INSERT INTO libraries VALUES (?,?,?)", id, in.Name, owner)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 201, map[string]string{"id": id})
	})
	mux.HandleFunc("PUT /v1/admin/libraries/{id}/members/{user}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		result, err := a.store.db.Exec("INSERT OR IGNORE INTO library_members SELECT ?,id FROM users WHERE id=? AND id NOT IN (SELECT owner FROM libraries)", r.PathValue("id"), r.PathValue("user"))
		if err != nil {
			failure(w, ErrInvalid)
			return
		}
		n, _ := result.RowsAffected()
		_ = n
		respond(w, 204, nil)
	})
	mux.HandleFunc("DELETE /v1/admin/libraries/{id}/members/{user}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		_, err := a.store.db.Exec("DELETE FROM library_members WHERE library_id=? AND user_id=?", r.PathValue("id"), r.PathValue("user"))
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("PUT /v1/admin/invites/{id}/libraries/{library}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		_, err := a.store.db.Exec("INSERT OR IGNORE INTO invite_libraries SELECT id,? FROM invites WHERE id=? AND redeemed_by IS NULL AND revoked=0 AND expires_at>?", r.PathValue("library"), r.PathValue("id"), time.Now().Unix())
		if err != nil {
			failure(w, ErrInvalid)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("GET /v1/admin/watches", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		v, err := a.store.Watches()
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, v)
	})
	mux.HandleFunc("POST /v1/admin/watches", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var in struct {
			Username string `json:"username"`
			Library  string `json:"library"`
			Path     string `json:"path"`
		}
		if !decode(w, r, &in, 4096) {
			return
		}
		if in.Library != "" {
			if err := a.store.db.QueryRow("SELECT u.username FROM libraries l JOIN users u ON u.id=l.owner WHERE l.id=?", in.Library).Scan(&in.Username); err != nil {
				failure(w, err)
				return
			}
		}
		id, err := a.store.AddWatch(in.Username, in.Path)
		if err != nil {
			failure(w, err)
			return
		}
		scanError := ""
		if err := a.store.ScanWatch(id); err != nil {
			scanError = err.Error()
		}
		respond(w, 201, map[string]string{"id": id, "scanError": scanError})
	})
	mux.HandleFunc("DELETE /v1/admin/watches/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		if err := a.store.RemoveWatch(r.PathValue("id")); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("POST /v1/admin/watches/{id}/scan", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		if err := a.store.ScanWatch(r.PathValue("id")); err != nil {
			respond(w, 422, map[string]string{"error": err.Error()})
			return
		}
		respond(w, 200, map[string]string{"status": "Scan complete"})
	})
	mux.HandleFunc("GET /v1/admin/overview", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var users, books, bytes int64
		err := a.store.db.QueryRow("SELECT count(*) FROM users WHERE id NOT IN (SELECT owner FROM libraries)").Scan(&users)
		if err == nil {
			err = a.store.db.QueryRow("SELECT count(*),coalesce(sum(size),0) FROM (SELECT user_id,book_id,max(size) size FROM files GROUP BY user_id,book_id)").Scan(&books, &bytes)
		}
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"users": users, "books": books, "bytes": bytes, "status": "Healthy"})
	})
	mux.HandleFunc("PUT /v1/admin/libraries/{library}/books/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		id := r.PathValue("id")
		if !bookPattern.MatchString(id) {
			failure(w, ErrInvalid)
			return
		}
		var owner string
		if err := a.store.db.QueryRow("SELECT owner FROM libraries WHERE id=?", r.PathValue("library")).Scan(&owner); err != nil {
			failure(w, err)
			return
		}
		a.store.fileMu.Lock()
		defer a.store.fileMu.Unlock()
		_, size, err := a.store.stage(owner, http.MaxBytesReader(w, r.Body, maxBookBytes+1), id)
		if err == nil {
			_, err = a.store.db.Exec("INSERT INTO files VALUES (?,?,'upload','',?) ON CONFLICT(user_id,book_id,kind,source) DO UPDATE SET size=excluded.size", owner, id, size)
		}
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 201, map[string]string{"bookId": id})
	})
	mux.HandleFunc("DELETE /v1/admin/libraries/{library}/books/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var owner string
		if err := a.store.db.QueryRow("SELECT owner FROM libraries WHERE id=?", r.PathValue("library")).Scan(&owner); err != nil {
			failure(w, err)
			return
		}
		if err := a.store.deleteUpload(owner, r.PathValue("id")); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
}
func (a *api) listLibraries(w http.ResponseWriter, user string, admin bool) {
	query := "SELECT id,name,owner FROM libraries"
	args := []any{}
	if !admin {
		query += " WHERE id IN(SELECT library_id FROM library_members WHERE user_id=?)"
		args = append(args, user)
	}
	rows, err := a.store.db.Query(query+" ORDER BY name", args...)
	if err != nil {
		failure(w, err)
		return
	}
	out := []LibraryInfo{}
	for rows.Next() {
		var l LibraryInfo
		l.Members = []string{}
		if err = rows.Scan(&l.ID, &l.Name, &l.Owner); err != nil {
			rows.Close()
			failure(w, err)
			return
		}
		out = append(out, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		failure(w, err)
		return
	}
	for i := range out {
		out[i].BookIDs = []string{}
		ids, e := a.store.db.Query("SELECT DISTINCT book_id FROM files WHERE user_id=?", out[i].Owner)
		if e != nil {
			failure(w, e)
			return
		}
		for ids.Next() {
			var id string
			if e = ids.Scan(&id); e != nil {
				ids.Close()
				failure(w, e)
				return
			}
			out[i].BookIDs = append(out[i].BookIDs, id)
		}
		e = ids.Err()
		ids.Close()
		if e != nil {
			failure(w, e)
			return
		}

		if err = a.store.db.QueryRow("SELECT count(DISTINCT book_id) FROM files WHERE user_id=?", out[i].Owner).Scan(&out[i].Books); err != nil {
			failure(w, err)
			return
		}
		if admin {
			rs, e := a.store.db.Query("SELECT user_id FROM library_members WHERE library_id=?", out[i].ID)
			if e != nil {
				failure(w, e)
				return
			}
			for rs.Next() {
				var id string
				if e = rs.Scan(&id); e != nil {
					rs.Close()
					failure(w, e)
					return
				}
				out[i].Members = append(out[i].Members, id)
			}
			e = rs.Err()
			rs.Close()
			if e != nil {
				failure(w, e)
				return
			}
		}
	}
	respond(w, 200, out)
}
