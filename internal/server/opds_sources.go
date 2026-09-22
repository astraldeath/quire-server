package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type catalogSource struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Revision int64  `json:"revision"`
	Deleted  bool   `json:"deleted"`
}
type catalogCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type catalogSourceWrite struct {
	Name         string              `json:"name"`
	URL          string              `json:"url"`
	BaseRevision int64               `json:"baseRevision"`
	OperationID  string              `json:"operationId"`
	Deleted      bool                `json:"deleted"`
	Credentials  *catalogCredentials `json:"credentials,omitempty"`
}

func catalogURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 4096 || u == nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Fragment != "" {
		return nil, ErrInvalid
	}
	return u, nil
}
func (s *Store) writeCatalogSource(user, id string, in catalogSourceWrite) (catalogSource, error) {
	out := catalogSource{ID: id, Name: strings.TrimSpace(in.Name), URL: in.URL, Deleted: in.Deleted}
	if len(id) == 0 || len(id) > 128 || strings.ContainsAny(id, "/\\\x00") || out.Name == "" || len(out.Name) > 200 || len(in.OperationID) == 0 || len(in.OperationID) > 128 || in.BaseRevision < 0 || in.BaseRevision >= 9007199254740991 {
		return out, ErrInvalid
	}
	if _, e := catalogURL(in.URL); e != nil {
		return out, e
	}
	if in.Credentials != nil && (len(in.Credentials.Username) > 512 || len(in.Credentials.Password) > 4096 || strings.ContainsAny(in.Credentials.Username, ":\r\n")) {
		return out, ErrInvalid
	}
	raw, _ := json.Marshal(struct {
		ID    string
		Input catalogSourceWrite
	}{id, in})
	digest := sha256.Sum256(raw)
	s.trackingMu.Lock()
	defer s.trackingMu.Unlock()
	var encrypted []byte
	if in.Credentials != nil && !in.Deleted && (in.Credentials.Username != "" || in.Credentials.Password != "") {
		aead, e := s.trackingCipher()
		if e != nil {
			return out, e
		}
		nonce := make([]byte, aead.NonceSize())
		if _, e = rand.Read(nonce); e != nil {
			return out, e
		}
		plain, _ := json.Marshal(in.Credentials)
		encrypted = aead.Seal(nonce, nonce, plain, []byte("opds:"+user+":"+id))
	}
	tx, e := s.db.Begin()
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	var oldDigest []byte
	var result string
	e = tx.QueryRow("SELECT digest,result FROM catalog_operations WHERE user_id=? AND id=?", user, in.OperationID).Scan(&oldDigest, &result)
	if e == nil {
		if !bytes.Equal(oldDigest, digest[:]) {
			return out, ErrConflict
		}
		e = json.Unmarshal([]byte(result), &out)
		return out, e
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	var rev int64
	var oldURL string
	var oldSecret []byte
	e = tx.QueryRow("SELECT revision,url,secret FROM catalog_sources WHERE user_id=? AND id=?", user, id).Scan(&rev, &oldURL, &oldSecret)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if rev != in.BaseRevision {
		return out, ErrConflict
	}
	if rev == 0 {
		var count int
		if e = tx.QueryRow("SELECT count(*) FROM catalog_sources WHERE user_id=?", user).Scan(&count); e != nil {
			return out, e
		}
		if count >= 500 {
			return out, ErrConflict
		}
	}
	// Changing a destination never silently sends credentials to its new origin.
	if in.Credentials == nil && !in.Deleted && oldURL == in.URL {
		encrypted = oldSecret
	}
	out.Revision = rev + 1
	_, e = tx.Exec("INSERT INTO catalog_sources VALUES(?,?,?,?,?,?,?) ON CONFLICT(user_id,id) DO UPDATE SET name=excluded.name,url=excluded.url,revision=excluded.revision,deleted=excluded.deleted,secret=excluded.secret", user, id, out.Name, out.URL, out.Revision, out.Deleted, encrypted)
	if e != nil {
		return out, e
	}
	encoded, _ := json.Marshal(out)
	if _, e = tx.Exec("INSERT INTO catalog_operations VALUES(?,?,?,?)", user, in.OperationID, digest[:], string(encoded)); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (s *Store) catalogSources(user string) ([]catalogSource, error) {
	rows, e := s.db.Query("SELECT id,name,url,revision,deleted FROM catalog_sources WHERE user_id=? ORDER BY name,id", user)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []catalogSource{}
	for rows.Next() {
		var v catalogSource
		if e = rows.Scan(&v.ID, &v.Name, &v.URL, &v.Revision, &v.Deleted); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (a *api) catalogSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/catalog-sources", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		out, e := a.store.catalogSources(user)
		if e != nil {
			failure(w, e)
			return
		}
		respond(w, 200, map[string]any{"sources": out})
	})
	mux.HandleFunc("PUT /v1/catalog-sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		var in catalogSourceWrite
		if !decode(w, r, &in, 16384) {
			return
		}
		out, e := a.store.writeCatalogSource(user, r.PathValue("id"), in)
		if e != nil {
			failure(w, e)
			return
		}
		respond(w, 200, out)
	})
}
