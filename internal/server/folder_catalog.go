// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
)

type FolderCatalog struct {
	Library []string `json:"library"`
	Hidden  []string `json:"hidden"`
}
type FolderCatalogRequest struct {
	Revision int64          `json:"revision"`
	State    *FolderCatalog `json:"state"`
}
type FolderCatalogResponse struct {
	Revision int64         `json:"revision"`
	State    FolderCatalog `json:"state"`
	Accepted bool          `json:"accepted"`
}

func (p FolderCatalog) validate() error {
	for _, paths := range [][]string{p.Library, p.Hidden} {
		if paths == nil || len(paths) > 5000 {
			return ErrInvalid
		}
		seen := map[string]bool{}
		for _, path := range paths {
			if path == "" || !validFolder(path) || seen[path] {
				return ErrInvalid
			}
			seen[path] = true
		}
		sort.Strings(paths)
	}
	encoded, _ := json.Marshal(p)
	if len(encoded) > 1<<20 {
		return ErrInvalid
	}
	return nil
}

// Account-scoped compare-and-swap; stale clients receive current state without overwriting it.
func (s *Store) SyncFolderCatalog(ctx context.Context, user string, req FolderCatalogRequest) (FolderCatalogResponse, error) {
	out := FolderCatalogResponse{State: FolderCatalog{Library: []string{}, Hidden: []string{}}, Accepted: true}
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
	err = tx.QueryRowContext(ctx, "SELECT revision,state FROM folder_catalog WHERE user_id=?", user).Scan(&out.Revision, &encoded)
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
		incoming, _ := json.Marshal(req.State)
		existing, _ := json.Marshal(out.State)
		// An accepted write whose response was lost can be retried without a new revision.
		out.Accepted = req.Revision == out.Revision || bytes.Equal(incoming, existing)
		if out.Accepted {
			if !bytes.Equal(incoming, existing) {
				out.Revision++
				out.State = *req.State
				if _, err = tx.ExecContext(ctx, `INSERT INTO folder_catalog(user_id,revision,state) VALUES (?,?,?) ON CONFLICT(user_id) DO UPDATE SET revision=excluded.revision,state=excluded.state`, user, out.Revision, string(incoming)); err != nil {
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

func (a *api) folderCatalogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/folders/sync", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		var req FolderCatalogRequest
		if !decode(w, r, &req, (1<<20)+128) {
			return
		}
		out, err := a.store.SyncFolderCatalog(r.Context(), user, req)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, out)
	})
}
