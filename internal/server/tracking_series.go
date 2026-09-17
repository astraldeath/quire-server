package server

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"strings"
)

func (a *api) trackingSeriesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/tracking/series", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		var in struct {
			SeriesKey string `json:"seriesKey"`
			SeriesID  int64  `json:"seriesId"`
			Title     string `json:"title"`
			Auto      bool   `json:"auto"`
			Private   *bool  `json:"private"`
		}
		if !decode(w, r, &in, 4096) {
			return
		}
		if strings.TrimSpace(in.SeriesKey) == "" || len(in.SeriesKey) > 1000 || in.SeriesID <= 0 || in.SeriesID > 1e10 || strings.TrimSpace(in.Title) == "" || len(in.Title) > 1000 {
			failure(w, ErrInvalid)
			return
		}
		a.store.trackingMu.Lock()
		defer a.store.trackingMu.Unlock()
		tx, err := a.store.db.Begin()
		if err != nil {
			failure(w, err)
			return
		}
		defer tx.Rollback()
		rows, err := tx.Query("SELECT book_id,candidates FROM records WHERE user_id=? AND kind='book' AND record_id='default'", user)
		if err != nil {
			failure(w, err)
			return
		}
		type member struct {
			id     string
			volume float64
		}
		members := []member{}
		for rows.Next() {
			var id, raw string
			if err = rows.Scan(&id, &raw); err != nil {
				break
			}
			var candidates []Candidate
			if json.Unmarshal([]byte(raw), &candidates) != nil || len(candidates) != 1 || candidates[0].Deleted {
				continue
			}
			var book struct {
				Series string
				Volume float64
			}
			if json.Unmarshal(candidates[0].Value, &book) != nil || book.Series != in.SeriesKey {
				continue
			}
			if math.IsNaN(book.Volume) || math.IsInf(book.Volume, 0) || book.Volume < 0 || book.Volume > 10000 {
				book.Volume = 0
			}
			members = append(members, member{id, book.Volume})
		}
		rowErr := rows.Err()
		rows.Close()
		if err != nil {
			failure(w, err)
			return
		}
		if rowErr != nil {
			failure(w, rowErr)
			return
		}
		if len(members) == 0 {
			failure(w, sql.ErrNoRows)
			return
		}
		linked, overrides := 0, 0
		for _, book := range members {
			result, e := tx.Exec(`INSERT INTO tracking_links(user_id,book_id,series_key,series_id,title,volume,auto,complete_entry,is_private) VALUES(?,?,?,?,?,?,?,0,coalesce(?,1)) ON CONFLICT(user_id,book_id) DO UPDATE SET series_id=excluded.series_id,title=excluded.title,volume=excluded.volume,auto=excluded.auto,complete_entry=0,is_private=coalesce(?,tracking_links.is_private),last_step=0,last_chapter=0,last_sync=0,error='',next_attempt=0 WHERE tracking_links.series_key=excluded.series_key`, user, book.id, in.SeriesKey, in.SeriesID, in.Title, book.volume, in.Auto, in.Private, in.Private)
			if e != nil {
				failure(w, e)
				return
			}
			n, _ := result.RowsAffected()
			if n == 0 {
				overrides++
			} else {
				linked++
			}
		}
		if err = tx.Commit(); err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]int{"linked": linked, "overrides": overrides})
	})
	mux.HandleFunc("DELETE /v1/tracking/series", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		var in struct {
			SeriesKey string `json:"seriesKey"`
		}
		if !decode(w, r, &in, 2048) {
			return
		}
		if in.SeriesKey == "" {
			failure(w, ErrInvalid)
			return
		}
		a.store.trackingMu.Lock()
		defer a.store.trackingMu.Unlock()
		_, err := a.store.db.Exec("DELETE FROM tracking_links WHERE user_id=? AND series_key=?", user, in.SeriesKey)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
}
