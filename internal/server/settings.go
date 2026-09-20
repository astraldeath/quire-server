// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type ServerSettings struct {
	Name        string `json:"name"`
	ScanSeconds int    `json:"scanSeconds"`
}

func (s *Store) Settings(fallback ServerSettings) (ServerSettings, error) {
	var v ServerSettings
	err := s.db.QueryRow("SELECT name,scan_seconds FROM server_settings WHERE id=1").Scan(&v.Name, &v.ScanSeconds)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	return v, err
}
func (s *Store) ScanDelay(fallback time.Duration) time.Duration {
	v, err := s.Settings(ServerSettings{ScanSeconds: int(fallback.Seconds())})
	if err != nil {
		return fallback
	}
	return time.Duration(v.ScanSeconds) * time.Second
}
func (a *api) settingsRoutes(mux *http.ServeMux, name string) {
	mux.HandleFunc("GET /v1/admin/settings", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		v, err := a.store.Settings(ServerSettings{name, 300})
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, v)
	})
	mux.HandleFunc("PUT /v1/admin/settings", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		var in ServerSettings
		if !decode(w, r, &in, 1024) {
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || len(in.Name) > 100 || in.ScanSeconds < 0 || in.ScanSeconds > 86400 || (in.ScanSeconds != 0 && in.ScanSeconds < 60) {
			failure(w, ErrInvalid)
			return
		}
		if _, err := a.store.db.Exec("INSERT INTO server_settings VALUES (1,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,scan_seconds=excluded.scan_seconds", in.Name, in.ScanSeconds); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("GET /v1/admin/scans", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		rows, err := a.store.db.Query("SELECT watch_id,last_at,error,imported,existing,skipped,skipped_files,omitted_skipped_files FROM scan_status")
		if err != nil {
			failure(w, err)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, message, details string
			var at int64
			var imported, existing, skipped, omitted int
			if err = rows.Scan(&id, &at, &message, &imported, &existing, &skipped, &details, &omitted); err != nil {
				failure(w, err)
				return
			}
			files := []scanSkippedFile{}
			if err = json.Unmarshal([]byte(details), &files); err != nil {
				failure(w, err)
				return
			}
			out = append(out, map[string]any{"id": id, "lastAt": at, "error": message, "imported": imported, "existing": existing, "skipped": skipped, "skippedFiles": files, "omittedSkippedFiles": omitted})
		}
		if err = rows.Err(); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, out)
	})
}
