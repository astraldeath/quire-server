// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
)

type Operation struct {
	ID           string          `json:"id"`
	BookID       string          `json:"bookId"`
	Kind         string          `json:"kind"`
	RecordID     string          `json:"recordId"`
	BaseRevision int64           `json:"baseRevision"`
	Deleted      bool            `json:"deleted"`
	Value        json.RawMessage `json:"value"`
}
type Candidate struct {
	OperationID string          `json:"operationId"`
	Deleted     bool            `json:"deleted"`
	Value       json.RawMessage `json:"value"`
	CreatedAt   int64           `json:"createdAt"`
}
type Record struct {
	BookID     string      `json:"bookId"`
	Kind       string      `json:"kind"`
	RecordID   string      `json:"recordId"`
	Revision   int64       `json:"revision"`
	Candidates []Candidate `json:"candidates"`
}
type Change struct {
	Cursor int64 `json:"cursor"`
	Record
}
type OperationResult struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Conflict bool   `json:"conflict"`
}
type SyncRequest struct {
	Cursor     int64       `json:"cursor"`
	Operations []Operation `json:"operations"`
}
type SyncResponse struct {
	Results []OperationResult `json:"results"`
	Changes []Change          `json:"changes"`
	Cursor  int64             `json:"cursor"`
	HasMore bool              `json:"hasMore"`
}

var bookPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func strictJSON(data []byte, v any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrInvalid
	}
	return nil
}
func validateOperation(op Operation) error {
	if !idPattern.MatchString(op.ID) || !idPattern.MatchString(op.RecordID) || !bookPattern.MatchString(op.BookID) || op.BaseRevision < 0 || len(op.Value) > 32768 {
		return ErrInvalid
	}
	if op.Kind != "book" && op.Kind != "position" && op.Kind != "annotation" {
		return ErrInvalid
	}
	if op.Kind != "annotation" && op.RecordID != "default" {
		return ErrInvalid
	}
	if op.Deleted {
		if len(op.Value) > 0 && string(op.Value) != "null" {
			return ErrInvalid
		}
		return nil
	}
	if len(op.Value) == 0 || string(op.Value) == "null" {
		return ErrInvalid
	}
	switch op.Kind {
	case "book":
		var v struct {
			Title  string   `json:"title"`
			Author string   `json:"author"`
			Series string   `json:"series"`
			Volume *float64 `json:"volume"`
		}
		if strictJSON(op.Value, &v) != nil || v.Title == "" || len(v.Title) > 2048 || len(v.Author) > 2048 || len(v.Series) > 2048 {
			return ErrInvalid
		}
	case "position":
		var v struct {
			CFI      string   `json:"cfi"`
			Fraction *float64 `json:"fraction"`
			Section  string   `json:"section"`
		}
		if strictJSON(op.Value, &v) != nil || v.CFI == "" || len(v.CFI) > 8192 || v.Fraction == nil || *v.Fraction < 0 || *v.Fraction > 1 || len(v.Section) > 2048 {
			return ErrInvalid
		}
	case "annotation":
		var v struct {
			Kind    string `json:"kind"`
			CFI     string `json:"cfi"`
			Text    string `json:"text"`
			Note    string `json:"note"`
			Section string `json:"section"`
		}
		if strictJSON(op.Value, &v) != nil || (v.Kind != "bookmark" && v.Kind != "highlight") || v.CFI == "" || len(v.CFI) > 8192 || len(v.Section) > 2048 {
			return ErrInvalid
		}
	}
	return nil
}

// Sync atomically accepts an outbox batch and returns a bounded page of immutable
// changes. Revisions are server assigned; device timestamps never pick a winner.
func (s *Store) Sync(ctx context.Context, user string, req SyncRequest) (SyncResponse, error) {
	if err := s.sharedSeeds(user); err != nil {
		return SyncResponse{}, err
	}
	if req.Cursor < 0 || len(req.Operations) > 50 {
		return SyncResponse{}, ErrInvalid
	}
	for _, op := range req.Operations {
		if err := validateOperation(op); err != nil {
			return SyncResponse{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SyncResponse{}, err
	}
	defer tx.Rollback()
	var cursor int64
	if err = tx.QueryRowContext(ctx, "SELECT value FROM cursors WHERE user_id=?", user).Scan(&cursor); err != nil {
		return SyncResponse{}, err
	}
	if req.Cursor > cursor {
		return SyncResponse{}, ErrConflict
	}
	out := SyncResponse{Results: []OperationResult{}, Changes: []Change{}, Cursor: req.Cursor}
	for _, op := range req.Operations {
		b, _ := json.Marshal(op)
		digest := sha256.Sum256(b)
		var previousDigest []byte
		var previous string
		err = tx.QueryRowContext(ctx, "SELECT digest,result FROM operations WHERE user_id=? AND id=?", user, op.ID).Scan(&previousDigest, &previous)
		if err == nil {
			if !bytes.Equal(digest[:], previousDigest) {
				return SyncResponse{}, ErrConflict
			}
			var result OperationResult
			if err = json.Unmarshal([]byte(previous), &result); err != nil {
				return SyncResponse{}, err
			}
			out.Results = append(out.Results, result)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return SyncResponse{}, err
		}
		record := Record{BookID: op.BookID, Kind: op.Kind, RecordID: op.RecordID}
		var candidates string
		err = tx.QueryRowContext(ctx, "SELECT revision,candidates FROM records WHERE user_id=? AND book_id=? AND kind=? AND record_id=?", user, op.BookID, op.Kind, op.RecordID).Scan(&record.Revision, &candidates)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return SyncResponse{}, err
		}
		if err == nil {
			if err = json.Unmarshal([]byte(candidates), &record.Candidates); err != nil {
				return SyncResponse{}, err
			}
		}
		if op.BaseRevision > record.Revision {
			return SyncResponse{}, ErrConflict
		}
		conflict := op.BaseRevision != record.Revision
		if !conflict {
			record.Candidates = nil
		}
		if len(record.Candidates) >= 16 {
			return SyncResponse{}, ErrConflict
		}
		record.Candidates = append(record.Candidates, Candidate{op.ID, op.Deleted, op.Value, time.Now().UnixMilli()})
		record.Revision++
		encoded, _ := json.Marshal(record.Candidates)
		if _, err = tx.ExecContext(ctx, `INSERT INTO records VALUES (?,?,?,?,?,?) ON CONFLICT(user_id,book_id,kind,record_id) DO UPDATE SET revision=excluded.revision,candidates=excluded.candidates`, user, op.BookID, op.Kind, op.RecordID, record.Revision, string(encoded)); err != nil {
			return SyncResponse{}, err
		}
		cursor++
		encoded, _ = json.Marshal(record)
		if _, err = tx.ExecContext(ctx, "INSERT INTO changes VALUES (?,?,?)", user, cursor, string(encoded)); err != nil {
			return SyncResponse{}, err
		}
		result := OperationResult{op.ID, record.Revision, conflict}
		encoded, _ = json.Marshal(result)
		if _, err = tx.ExecContext(ctx, "INSERT INTO operations VALUES (?,?,?,?)", user, op.ID, digest[:], string(encoded)); err != nil {
			return SyncResponse{}, err
		}
		out.Results = append(out.Results, result)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE cursors SET value=? WHERE user_id=?", cursor, user); err != nil {
		return SyncResponse{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT cursor,record FROM changes WHERE user_id=? AND cursor>? ORDER BY cursor LIMIT 100", user, req.Cursor)
	if err != nil {
		return SyncResponse{}, err
	}
	for rows.Next() {
		var change Change
		var encoded string
		if err = rows.Scan(&change.Cursor, &encoded); err != nil {
			rows.Close()
			return SyncResponse{}, err
		}
		if err = json.Unmarshal([]byte(encoded), &change.Record); err != nil {
			rows.Close()
			return SyncResponse{}, err
		}
		out.Changes = append(out.Changes, change)
		out.Cursor = change.Cursor
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return SyncResponse{}, err
	}
	out.HasMore = out.Cursor < cursor
	if err = tx.Commit(); err != nil {
		return SyncResponse{}, err
	}
	return out, nil
}
