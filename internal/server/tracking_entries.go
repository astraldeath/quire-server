package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"
)

var trackingEntryFields = []string{"state", "progress_chapter", "progress_volume", "rating", "start_date", "finish_date", "is_private"}

func validateTrackingChanges(changes map[string]any) error {
	if len(changes) == 0 {
		return ErrInvalid
	}
	for key, value := range changes {
		switch key {
		case "state":
			switch value {
			case "considering", "completed", "dropped", "paused", "plan_to_read", "reading", "rereading":
			default:
				return ErrInvalid
			}
		case "progress_chapter", "progress_volume", "rating":
			if value == nil {
				continue
			}
			n, ok := value.(float64)
			max := 10000.0
			if key == "rating" {
				max = 100
			}
			if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > max {
				return ErrInvalid
			}
		case "start_date", "finish_date":
			if value == nil {
				continue
			}
			date, ok := value.(string)
			if !ok {
				return ErrInvalid
			}
			parsed, err := time.Parse("2006-01-02", date)
			if err != nil || parsed.Year() < 1679 || parsed.Year() > 2262 {
				return ErrInvalid
			}
		case "is_private":
			if _, ok := value.(bool); !ok {
				return ErrInvalid
			}
		default:
			return ErrInvalid
		}
	}
	return nil
}

// Return only editable provider fields; provider-only metadata never becomes a patch.
func normalizeTrackingEntry(data json.RawMessage) (map[string]any, error) {
	var source map[string]any
	if err := json.Unmarshal(data, &source); err != nil || source == nil {
		return nil, errors.New("Invalid MangaBaka entry response.")
	}
	result := make(map[string]any, len(trackingEntryFields))
	for _, key := range trackingEntryFields {
		value := source[key]
		if key == "start_date" || key == "finish_date" {
			if date, ok := value.(string); ok && len(date) >= 10 {
				value = date[:10]
			}
		}
		result[key] = value
	}
	return result, nil
}

// A link must still belong to a live, settled book in this user's library.
func (s *Store) hasTrackingEntryLink(user string, seriesID int64) (bool, error) {
	rows, err := s.db.Query(`SELECT r.candidates FROM tracking_links l JOIN records r ON r.user_id=l.user_id AND r.book_id=l.book_id AND r.kind='book' AND r.record_id='default' WHERE l.user_id=? AND l.series_id=?`, user, seriesID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return false, err
		}
		var candidates []Candidate
		if json.Unmarshal([]byte(raw), &candidates) == nil && len(candidates) == 1 && !candidates[0].Deleted {
			return true, nil
		}
	}
	return false, rows.Err()
}

type trackingAcknowledgement struct {
	bookID        string
	step, chapter int
}

// Snapshot before the provider write, so reads completed during the request remain pending.
func (s *Store) trackingEntryAcknowledgements(user string, seriesID int64) ([]trackingAcknowledgement, error) {
	rows, err := s.db.Query(`SELECT l.book_id,l.volume,r.candidates FROM tracking_links l JOIN records r ON r.user_id=l.user_id AND r.book_id=l.book_id AND r.kind='position' AND r.record_id='default' WHERE l.user_id=? AND l.series_id=?`, user, seriesID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []trackingAcknowledgement
	for rows.Next() {
		var id, raw string
		var volume float64
		if err = rows.Scan(&id, &volume, &raw); err != nil {
			return nil, err
		}
		var candidates []Candidate
		if json.Unmarshal([]byte(raw), &candidates) != nil || len(candidates) != 1 || candidates[0].Deleted {
			continue
		}
		var pos struct {
			Fraction         float64
			CompletedChapter int
			CurrentChapter   *int
		}
		if json.Unmarshal(candidates[0].Value, &pos) != nil {
			continue
		}
		ack := trackingAcknowledgement{bookID: id}
		if pos.Fraction > 0 {
			ack.step = 1
		}
		if pos.Fraction >= 0.999 {
			ack.step = 2
		}
		detectedChapter := pos.CompletedChapter
		if pos.CurrentChapter != nil {
			detectedChapter = *pos.CurrentChapter
		}
		if volume == 0 && detectedChapter > 0 && detectedChapter <= 100000 {
			ack.chapter = detectedChapter
		}
		result = append(result, ack)
	}
	return result, rows.Err()
}

func (s *Store) acknowledgeTrackingEntry(user string, seriesID int64, changes map[string]any, acks []trackingAcknowledgement) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if private, ok := changes["is_private"]; ok {
		if _, err = tx.Exec("UPDATE tracking_links SET is_private=? WHERE user_id=? AND series_id=?", private, user, seriesID); err != nil {
			return err
		}
	}
	for _, ack := range acks {
		if _, err = tx.Exec("UPDATE tracking_links SET last_step=max(last_step,?),last_chapter=max(last_chapter,?),last_sync=?,error='',next_attempt=0 WHERE user_id=? AND book_id=? AND series_id=?", ack.step, ack.chapter, time.Now().Unix(), user, ack.bookID, seriesID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *api) trackingEntryRoutes(mux *http.ServeMux, p *trackingProvider) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 || id > 1e10 {
			failure(w, ErrInvalid)
			return
		}
		var expected string
		var changes map[string]any
		if r.Method == "POST" {
			var in struct {
				ExpectedAccountID string `json:"expectedAccountId"`
				Private           *bool  `json:"is_private"`
			}
			if !decode(w, r, &in, 4096) {
				return
			}
			if in.ExpectedAccountID == "" || in.Private == nil {
				failure(w, ErrInvalid)
				return
			}
			expected = in.ExpectedAccountID
			changes = map[string]any{"is_private": *in.Private}
		} else if r.Method == "PUT" {
			var in struct {
				ExpectedAccountID string         `json:"expectedAccountId"`
				Changes           map[string]any `json:"changes"`
			}
			if !decode(w, r, &in, 8192) {
				return
			}
			if in.ExpectedAccountID == "" || validateTrackingChanges(in.Changes) != nil {
				failure(w, ErrInvalid)
				return
			}
			expected = in.ExpectedAccountID
			changes = in.Changes
		}
		a.store.trackingMu.Lock()
		defer a.store.trackingMu.Unlock()
		linked, err := a.store.hasTrackingEntryLink(user, id)
		if err != nil {
			failure(w, err)
			return
		}
		if !linked {
			failure(w, sql.ErrNoRows)
			return
		}
		var accountID string
		err = a.store.db.QueryRow("SELECT provider_id FROM tracking_accounts WHERE user_id=?", user).Scan(&accountID)
		if errors.Is(err, sql.ErrNoRows) {
			respond(w, 409, map[string]string{"error": "Connect MangaBaka before editing this entry."})
			return
		}
		if err != nil {
			failure(w, err)
			return
		}
		if r.Method != "GET" && expected != accountID {
			respond(w, 409, map[string]string{"error": "MangaBaka account changed. Reload this entry before saving."})
			return
		}
		token, err := a.store.trackingAccessToken(r.Context(), user, p)
		if err != nil {
			respond(w, 502, map[string]string{"error": "MangaBaka authorization needs attention. Reconnect or try again shortly."})
			return
		}
		path := fmt.Sprintf("/v1/my/library/%d", id)
		data, status, err := p.call(r.Context(), "GET", path, token, nil)
		missing := status == 404
		if err != nil && !missing {
			respond(w, 502, map[string]string{"error": err.Error()})
			return
		}
		var current map[string]any
		if !missing {
			current, err = normalizeTrackingEntry(data)
			if err != nil {
				respond(w, 502, map[string]string{"error": err.Error()})
				return
			}
		}
		if r.Method == "GET" {
			respond(w, 200, map[string]any{"entry": current, "accountId": accountID})
			return
		}
		if missing && r.Method == "PUT" {
			respond(w, 409, map[string]string{"error": "This MangaBaka entry no longer exists. Reload and add it again before editing."})
			return
		}
		var acks []trackingAcknowledgement
		for _, key := range []string{"state", "progress_chapter", "progress_volume"} {
			if _, ok := changes[key]; ok {
				acks, err = a.store.trackingEntryAcknowledgements(user, id)
				if err != nil {
					failure(w, err)
					return
				}
				break
			}
		}
		method := "PUT"
		if missing {
			method = "POST"
			changes["state"] = "plan_to_read"
			current = map[string]any{}
			for _, key := range trackingEntryFields {
				current[key] = nil
			}
		}
		_, _, err = p.call(r.Context(), method, path, token, changes)
		if err != nil {
			respond(w, 502, map[string]string{"error": err.Error()})
			return
		}
		if err = a.store.acknowledgeTrackingEntry(user, id, changes, acks); err != nil {
			failure(w, err)
			return
		}
		for key, value := range changes {
			current[key] = value
		}
		if latest, _, readErr := p.call(r.Context(), "GET", path, token, nil); readErr == nil {
			if normalized, parseErr := normalizeTrackingEntry(latest); parseErr == nil {
				current = normalized
			}
		}
		respond(w, 200, map[string]any{"entry": current, "accountId": accountID})
	}
	for _, method := range []string{"GET", "POST", "PUT"} {
		mux.HandleFunc(method+" /v1/tracking/entries/{id}", handler)
	}
}
