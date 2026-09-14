package server

import (
	"net/http"
	"os"
	"strings"
)

func (a *api) libraryManagementRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PATCH /v1/admin/libraries/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var input struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &input, 1024) {
			return
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Name == "" || len(input.Name) > 100 {
			failure(w, ErrInvalid)
			return
		}
		result, err := a.store.db.Exec("UPDATE libraries SET name=? WHERE id=?", input.Name, r.PathValue("id"))
		if err != nil {
			failure(w, err)
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			respond(w, 404, map[string]string{"error": "Library not found"})
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("DELETE /v1/admin/libraries/{id}", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		if err := a.store.deleteLibrary(r.PathValue("id")); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
}
func (s *Store) deleteLibrary(id string) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner string
	if err = tx.QueryRow("SELECT owner FROM libraries WHERE id=?", id).Scan(&owner); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT DISTINCT book_id FROM files WHERE user_id=?", owner)
	if err != nil {
		return err
	}
	books := []string{}
	for rows.Next() {
		var book string
		if err = rows.Scan(&book); err != nil {
			rows.Close()
			return err
		}
		books = append(books, book)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, query := range []string{"DELETE FROM scan_status WHERE watch_id IN (SELECT id FROM watch_roots WHERE user_id=?)", "DELETE FROM watch_roots WHERE user_id=?", "DELETE FROM files WHERE user_id=?", "DELETE FROM records WHERE user_id=?", "DELETE FROM changes WHERE user_id=?", "DELETE FROM operations WHERE user_id=?", "DELETE FROM sessions WHERE user_id=?", "DELETE FROM cursors WHERE user_id=?"} {
		if _, err = tx.Exec(query, owner); err != nil {
			return err
		}
	}
	for _, query := range []string{"DELETE FROM invite_libraries WHERE library_id=?", "DELETE FROM library_members WHERE library_id=?", "DELETE FROM libraries WHERE id=?"} {
		if _, err = tx.Exec(query, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM users WHERE id=?", owner); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Only remove managed snapshots. Watched source paths are never used here.
	for _, book := range books {
		_ = os.Remove(s.objectPath(owner, book))
	}
	return nil
}
