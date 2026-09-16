// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"sort"
)

// ReadingActivity is immutable, private account history, independent of library files.
type ReadingActivity struct {
	ID             string   `json:"id"`
	BookID         string   `json:"bookId"`
	StartedAt      int64    `json:"startedAt"`
	EndedAt        int64    `json:"endedAt"`
	ActiveMs       int64    `json:"activeMs"`
	Words          int64    `json:"words"`
	SampledMs      int64    `json:"sampledMs"`
	Chapters       []int    `json:"chapters"`
	Volume         *float64 `json:"volume"`
	Finished       bool     `json:"finished"`
	Baseline       bool     `json:"baseline,omitempty"`
	ChapterThrough *int     `json:"chapterThrough,omitempty"`
}

// Reject missing/null required scalars instead of silently accepting Go zero values.
func (a *ReadingActivity) UnmarshalJSON(data []byte) error {
	type plain ReadingActivity
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return ErrInvalid
	}
	for _, key := range []string{"id", "bookId", "startedAt", "endedAt", "activeMs", "words", "sampledMs", "chapters", "volume", "finished"} {
		value, ok := fields[key]
		if !ok || (key != "volume" && string(value) == "null") {
			return ErrInvalid
		}
	}
	for _, key := range []string{"baseline", "chapterThrough"} {
		if value, ok := fields[key]; ok && string(value) == "null" {
			return ErrInvalid
		}
	}
	var decoded plain
	if strictJSON(data, &decoded) != nil {
		return ErrInvalid
	}
	*a = ReadingActivity(decoded)
	return nil
}

type StatisticsSyncRequest struct {
	Cursor     int64             `json:"cursor"`
	Activities []ReadingActivity `json:"activities"`
}
type StatisticsSyncResponse struct {
	Cursor     int64             `json:"cursor"`
	Activities []ReadingActivity `json:"activities"`
	HasMore    bool              `json:"hasMore"`
}

var activityIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func canonicalActivity(a ReadingActivity) (ReadingActivity, error) {
	if !activityIDPattern.MatchString(a.ID) || !bookPattern.MatchString(a.BookID) || a.StartedAt < 0 || a.EndedAt < a.StartedAt || a.EndedAt > 8640000000000000 || a.EndedAt-a.StartedAt > 300000 || a.ActiveMs < 0 || a.ActiveMs > a.EndedAt-a.StartedAt || a.SampledMs < 0 || a.SampledMs > a.ActiveMs || a.Words < 0 || a.Words > 100000 || a.Chapters == nil || len(a.Chapters) > 1000 {
		return a, ErrInvalid
	}
	if a.Volume != nil && (*a.Volume <= 0 || *a.Volume > 100000 || math.IsNaN(*a.Volume) || math.IsInf(*a.Volume, 0)) {
		return a, ErrInvalid
	}
	if a.Words > 0 && a.SampledMs == 0 {
		return a, ErrInvalid
	}
	if a.Baseline && (a.StartedAt != a.EndedAt || a.ActiveMs != 0 || a.Words != 0 || a.SampledMs != 0) {
		return a, ErrInvalid
	}
	if a.ChapterThrough != nil && (!a.Baseline || *a.ChapterThrough < 1 || *a.ChapterThrough > 100000) {
		return a, ErrInvalid
	}
	a.Chapters = append([]int{}, a.Chapters...)
	sort.Ints(a.Chapters)
	for i, n := range a.Chapters {
		if n < 1 || n > 100000 || (i > 0 && n == a.Chapters[i-1]) {
			return a, ErrInvalid
		}
	}
	return a, nil
}

func (s *Store) SyncStatistics(ctx context.Context, user string, req StatisticsSyncRequest) (StatisticsSyncResponse, error) {
	var zero StatisticsSyncResponse
	if req.Cursor < 0 || len(req.Activities) > 100 {
		return zero, ErrInvalid
	}
	canonical := make([]ReadingActivity, len(req.Activities))
	for i, a := range req.Activities {
		var err error
		canonical[i], err = canonicalActivity(a)
		if err != nil {
			return zero, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var cursor int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(cursor),0) FROM reading_activities WHERE user_id=?", user).Scan(&cursor); err != nil {
		return zero, err
	}
	if req.Cursor > cursor {
		return zero, ErrConflict
	}
	for _, a := range canonical {
		encoded, err := json.Marshal(a)
		if err != nil {
			return zero, ErrInvalid
		}
		var previous string
		err = tx.QueryRowContext(ctx, "SELECT activity FROM reading_activities WHERE user_id=? AND id=?", user, a.ID).Scan(&previous)
		if err == nil {
			if previous != string(encoded) {
				return zero, ErrConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return zero, err
		}
		cursor++
		if _, err = tx.ExecContext(ctx, "INSERT INTO reading_activities(user_id,id,cursor,activity) VALUES (?,?,?,?)", user, a.ID, cursor, string(encoded)); err != nil {
			return zero, err
		}
	}
	out := StatisticsSyncResponse{Cursor: req.Cursor, Activities: []ReadingActivity{}}
	rows, err := tx.QueryContext(ctx, "SELECT cursor,activity FROM reading_activities WHERE user_id=? AND cursor>? ORDER BY cursor LIMIT 100", user, req.Cursor)
	if err != nil {
		return zero, err
	}
	for rows.Next() {
		var encoded string
		var a ReadingActivity
		if err = rows.Scan(&out.Cursor, &encoded); err != nil {
			rows.Close()
			return zero, err
		}
		if err = json.Unmarshal([]byte(encoded), &a); err != nil {
			rows.Close()
			return zero, err
		}
		out.Activities = append(out.Activities, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return zero, err
	}
	out.HasMore = out.Cursor < cursor
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	return out, nil
}

func (a *api) statisticsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/statistics/sync", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		var req StatisticsSyncRequest
		if !decode(w, r, &req, 1<<20) {
			return
		}
		out, err := a.store.SyncStatistics(r.Context(), user, req)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, out)
	})
}
