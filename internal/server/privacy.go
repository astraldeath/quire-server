// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
)

type PrivacyCredential struct {
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}
type PrivacySettings struct {
	Credential *PrivacyCredential `json:"credential"`
	Books      map[string]string  `json:"books"`
}
type PrivacyRequest struct {
	Revision int64            `json:"revision"`
	State    *PrivacySettings `json:"state"`
}
type PrivacyResponse struct {
	Revision int64           `json:"revision"`
	State    PrivacySettings `json:"state"`
	Accepted bool            `json:"accepted"`
}

var privacySalt = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (p PrivacySettings) validate() error {
	if p.Books == nil || len(p.Books) > 10000 || (p.Credential == nil && len(p.Books) > 0) {
		return ErrInvalid
	}
	if p.Credential != nil && (!privacySalt.MatchString(p.Credential.Salt) || !bookPattern.MatchString(p.Credential.Hash)) {
		return ErrInvalid
	}
	for id, mode := range p.Books {
		if !bookPattern.MatchString(id) || (mode != "hidden" && mode != "locked") {
			return ErrInvalid
		}
	}
	return nil
}

// Account-scoped compare-and-swap; stale clients receive current state without overwriting it.
func (s *Store) SyncPrivacy(ctx context.Context, user string, req PrivacyRequest) (PrivacyResponse, error) {
	out := PrivacyResponse{State: PrivacySettings{Books: map[string]string{}}, Accepted: true}
	if req.Revision < 0 || req.Revision > 9007199254740991 {
		return out, ErrInvalid
	}
	if req.State != nil {
		if err := req.State.validate(); err != nil {
			return out, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var encoded []byte
	err = tx.QueryRowContext(ctx, "SELECT revision,state FROM privacy_settings WHERE user_id=?", user).Scan(&out.Revision, &encoded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if err == nil {
		if err = json.Unmarshal(encoded, &out.State); err != nil {
			return out, err
		}
	}
	if req.State != nil && req.Revision > out.Revision {
		return out, ErrConflict
	}
	if req.State != nil {
		out.Accepted = req.Revision == out.Revision
		if out.Accepted {
			// No operation currently removes a passcode; an empty client must never reset one.
			if out.State.Credential != nil && req.State.Credential == nil {
				return out, ErrInvalid
			}
			incoming, _ := json.Marshal(req.State)
			existing, _ := json.Marshal(out.State)
			if !bytes.Equal(incoming, existing) {
				out.Revision++
				out.State = *req.State
				if _, err = tx.ExecContext(ctx, `INSERT INTO privacy_settings(user_id,revision,state) VALUES (?,?,?) ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision,state=excluded.state`, user, out.Revision, string(incoming)); err != nil {
					return out, err
				}
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

func (a *api) privacyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/privacy/sync", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		var req PrivacyRequest
		if !decode(w, r, &req, 1<<20) {
			return
		}
		out, err := a.store.SyncPrivacy(r.Context(), user, req)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, out)
	})
}
