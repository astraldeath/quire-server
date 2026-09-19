package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type trackingLink struct {
	BookID        string  `json:"bookId"`
	SeriesKey     string  `json:"seriesKey"`
	SeriesID      int64   `json:"seriesId"`
	Title         string  `json:"title"`
	Volume        float64 `json:"volume"`
	Auto          bool    `json:"auto"`
	CompleteEntry bool    `json:"completeEntry"`
	Private       *bool   `json:"private,omitempty"`
	LastStep      int     `json:"lastStep"`
	LastChapter   int     `json:"lastChapter"`
	LastSync      int64   `json:"lastSync"`
	Error         string  `json:"error"`
	NextAttempt   int64   `json:"-"`
}
type trackingRemote struct {
	State   string  `json:"state"`
	Volume  float64 `json:"progress_volume"`
	Chapter float64 `json:"progress_chapter"`
}

func trackingPatch(l trackingLink, step int, remote trackingRemote) map[string]any {
	patch := map[string]any{}
	if step >= 2 {
		if l.Volume > remote.Volume {
			patch["progress_volume"] = l.Volume
		}
		if l.CompleteEntry && remote.State != "completed" && remote.State != "paused" && remote.State != "dropped" {
			patch["state"] = "completed"
		}
	}
	if step == 1 && (remote.State == "plan_to_read" || remote.State == "considering" || remote.State == "") {
		patch["state"] = "reading"
	}
	return patch
}
func (s *Store) trackingLinks(user string) ([]trackingLink, error) {
	rows, err := s.db.Query("SELECT book_id,series_key,series_id,title,volume,auto,complete_entry,last_step,last_sync,error,next_attempt,last_chapter,is_private FROM tracking_links WHERE user_id=? ORDER BY title", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []trackingLink{}
	for rows.Next() {
		var l trackingLink
		if err = rows.Scan(&l.BookID, &l.SeriesKey, &l.SeriesID, &l.Title, &l.Volume, &l.Auto, &l.CompleteEntry, &l.LastStep, &l.LastSync, &l.Error, &l.NextAttempt, &l.LastChapter, &l.Private); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// The encryption key stays outside backups. Restored libraries require reconnecting.
func (s *Store) trackingCipher() (cipher.AEAD, error) {
	path := filepath.Join(s.data, "tracking.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, err = f.Write(key)
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s *Store) saveTrackingToken(user, id, name, token string) error {
	aead, err := s.trackingCipher()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	secret := aead.Seal(nonce, nonce, []byte(token), []byte(user))
	_, err = s.db.Exec("INSERT INTO tracking_accounts VALUES(?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET provider_id=excluded.provider_id,name=excluded.name,secret=excluded.secret", user, id, name, secret)
	return err
}
func (s *Store) trackingToken(user string) (string, error) {
	var secret []byte
	if err := s.db.QueryRow("SELECT secret FROM tracking_accounts WHERE user_id=?", user).Scan(&secret); err != nil {
		return "", err
	}
	aead, err := s.trackingCipher()
	if err != nil {
		return "", err
	}
	if len(secret) < aead.NonceSize() {
		return "", ErrInvalid
	}
	plain, err := aead.Open(nil, secret[:aead.NonceSize()], secret[aead.NonceSize():], []byte(user))
	return string(plain), err
}

type trackingProvider struct {
	base   string
	client *http.Client
}

func newTrackingProvider() *trackingProvider {
	return &trackingProvider{"https://api.mangabaka.org", &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("provider redirected") }}}
}
func (p *trackingProvider) call(ctx context.Context, method, path, token string, body any) (json.RawMessage, int, error) {
	var payload io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, 0, e
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, payload)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		if strings.HasPrefix(token, "mb-") {
			req.Header.Set("x-api-key", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, 0, errors.New("Cannot reach MangaBaka. Your update remains pending.")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		msg := "MangaBaka could not complete the request."
		if res.StatusCode == 401 || res.StatusCode == 403 {
			msg = "Reconnect MangaBaka with a token that has library.read and library.write access."
		}
		if res.StatusCode == 429 {
			msg = "MangaBaka is busy. The update will retry later."
		}
		return nil, res.StatusCode, errors.New(msg)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20+1))
	if err != nil || len(b) > 4<<20 {
		return nil, res.StatusCode, errors.New("Invalid MangaBaka response.")
	}
	var v struct {
		Data json.RawMessage `json:"data"`
	}
	if len(b) > 0 {
		err = json.Unmarshal(b, &v)
	}
	return v.Data, res.StatusCode, err
}
func (s *Store) runTracking(ctx context.Context, user string, p *trackingProvider) error {
	// Serialize account changes, unlinking and progress writes, including across tabs.
	if !s.trackingMu.TryLock() {
		return nil
	}
	defer s.trackingMu.Unlock()
	token, err := s.trackingAccessToken(ctx, user, p)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		_, _ = s.db.Exec("UPDATE tracking_links SET error=? WHERE user_id=? AND auto=1", "MangaBaka authorization needs attention. Reconnect or try again shortly.", user)
		return err
	}
	links, err := s.trackingLinks(user)
	if err != nil {
		return err
	}
	processed := 0
	for _, l := range links {
		if !l.Auto || l.NextAttempt > time.Now().Unix() {
			continue
		}
		var bookRaw string
		if s.db.QueryRow("SELECT candidates FROM records WHERE user_id=? AND book_id=? AND kind='book' AND record_id='default'", user, l.BookID).Scan(&bookRaw) != nil {
			continue
		}
		var bookCandidates []Candidate
		if json.Unmarshal([]byte(bookRaw), &bookCandidates) != nil || len(bookCandidates) != 1 || bookCandidates[0].Deleted {
			continue
		}
		var raw string
		if s.db.QueryRow("SELECT candidates FROM records WHERE user_id=? AND book_id=? AND kind='position' AND record_id='default'", user, l.BookID).Scan(&raw) != nil {
			continue
		}
		var candidates []Candidate
		if json.Unmarshal([]byte(raw), &candidates) != nil || len(candidates) != 1 || candidates[0].Deleted {
			continue
		}
		var pos struct {
			Fraction         float64 `json:"fraction"`
			CompletedChapter int     `json:"completedChapter"`
			CurrentChapter   *int    `json:"currentChapter"`
		}
		if json.Unmarshal(candidates[0].Value, &pos) != nil {
			continue
		}
		step := 0
		if pos.Fraction > 0 {
			step = 1
		}
		if pos.Fraction >= 0.999 {
			step = 2
		}
		chapter := 0
		detectedChapter := pos.CompletedChapter
		if pos.CurrentChapter != nil {
			detectedChapter = *pos.CurrentChapter
		}
		if l.Volume == 0 && detectedChapter > 0 && detectedChapter <= 100000 {
			chapter = detectedChapter
		}
		if step <= l.LastStep && chapter <= l.LastChapter {
			continue
		}
		// MangaBaka's API bounds progress_chapter at 10000; never clamp and
		// misreport a larger local chapter, or repeatedly send an invalid request.
		if chapter > 10000 {
			if _, err := s.db.Exec("UPDATE tracking_links SET error=?,next_attempt=? WHERE user_id=? AND book_id=?", "MangaBaka supports chapter progress up to 10000. Local progress is saved.", time.Now().Add(time.Minute).Unix(), user, l.BookID); err != nil {
				return err
			}
			continue
		}
		if processed >= 1 {
			break
		}
		processed++
		path := fmt.Sprintf("/v1/my/library/%d", l.SeriesID)
		data, status, e := p.call(ctx, "GET", path, token, nil)
		var remote trackingRemote
		if e == nil {
			e = json.Unmarshal(data, &remote)
		}
		if status == 404 {
			_, _, e = p.call(ctx, "POST", path, token, map[string]any{"state": "reading", "is_private": l.Private == nil || *l.Private})
			remote.State = "reading"
		}
		if e == nil {
			patch := trackingPatch(l, step, remote)
			if float64(chapter) > remote.Chapter {
				patch["progress_chapter"] = chapter
			}
			if len(patch) > 0 {
				_, _, e = p.call(ctx, "PUT", path, token, patch)
			}
		}
		message := ""
		if e != nil {
			message = e.Error()
			_, err = s.db.Exec("UPDATE tracking_links SET error=?,next_attempt=? WHERE user_id=? AND book_id=?", message, time.Now().Add(time.Minute).Unix(), user, l.BookID)
		} else {
			_, err = s.db.Exec("UPDATE tracking_links SET last_step=max(last_step,?),last_chapter=max(last_chapter,?),last_sync=?,error='',next_attempt=0 WHERE user_id=? AND book_id=?", step, chapter, time.Now().Unix(), user, l.BookID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (a *api) trackingRoutes(mux *http.ServeMux) {
	provider := newTrackingProvider()
	a.trackingSearchRoute(mux, provider)
	a.trackingEntryRoutes(mux, provider)
	a.trackingSeriesRoutes(mux)
	oauth := a.trackingOAuthRoutes(mux, provider)
	mux.HandleFunc("GET /v1/tracking", func(w http.ResponseWriter, r *http.Request) {
		u := a.authorized(w, r)
		if u == "" {
			return
		}
		links, err := a.store.trackingLinks(u)
		if err != nil {
			failure(w, err)
			return
		}
		var name, accountID string
		err = a.store.db.QueryRow("SELECT name,provider_id FROM tracking_accounts WHERE user_id=?", u).Scan(&name, &accountID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"connected": err == nil, "name": name, "accountId": accountID, "links": links, "oauthAvailable": oauth.config.enabled()})
	})
	mux.HandleFunc("PUT /v1/tracking/account", func(w http.ResponseWriter, r *http.Request) {
		u := a.authorized(w, r)
		if u == "" {
			return
		}
		var in struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &in, 4096) {
			return
		}
		if !strings.HasPrefix(in.Token, "mb-") || len(in.Token) > 2048 {
			failure(w, ErrInvalid)
			return
		}
		a.store.trackingMu.Lock()
		defer a.store.trackingMu.Unlock()
		data, _, err := provider.call(r.Context(), "GET", "/v1/my/profile", in.Token, nil)
		if err != nil {
			respond(w, 502, map[string]string{"error": err.Error()})
			return
		}
		var profile struct {
			ID                string
			Nickname          string
			PreferredUsername string `json:"preferred_username"`
			Scopes            []string
		}
		if json.Unmarshal(data, &profile) != nil || profile.ID == "" {
			failure(w, ErrInvalid)
			return
		}
		read, write := false, false
		for _, scope := range profile.Scopes {
			read = read || scope == "library.read"
			write = write || scope == "library.write"
		}
		if !read || !write {
			respond(w, 400, map[string]string{"error": "Token needs library.read and library.write access."})
			return
		}
		var previous string
		e := a.store.db.QueryRow("SELECT provider_id FROM tracking_accounts WHERE user_id=?", u).Scan(&previous)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			failure(w, e)
			return
		}
		if previous != "" && previous != profile.ID {
			respond(w, 409, map[string]string{"error": "Disconnect the current MangaBaka account first."})
			return
		}
		name := profile.PreferredUsername
		if name == "" {
			name = profile.Nickname
		}
		if name == "" {
			name = "MangaBaka account"
		}
		if err = a.store.saveTrackingToken(u, profile.ID, name, in.Token); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("DELETE /v1/tracking/account", func(w http.ResponseWriter, r *http.Request) {
		u := a.authorized(w, r)
		if u == "" {
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
		if _, err = tx.Exec("DELETE FROM tracking_accounts WHERE user_id=?", u); err == nil {
			_, err = tx.Exec("UPDATE tracking_links SET auto=0,last_step=0,last_chapter=0,last_sync=0,error='',next_attempt=0 WHERE user_id=?", u)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("PUT /v1/tracking/books/{id}", func(w http.ResponseWriter, r *http.Request) {
		u := a.authorized(w, r)
		if u == "" {
			return
		}
		var l trackingLink
		if !decode(w, r, &l, 8192) {
			return
		}
		id := r.PathValue("id")
		if !bookPattern.MatchString(id) || l.SeriesID <= 0 || l.SeriesID > 1e10 || strings.TrimSpace(l.Title) == "" || len(l.Title) > 1000 || len(l.SeriesKey) > 1000 || math.IsNaN(l.Volume) || math.IsInf(l.Volume, 0) || l.Volume < 0 || l.Volume > 10000 || l.SeriesKey != "" && l.CompleteEntry {
			failure(w, ErrInvalid)
			return
		}
		a.store.trackingMu.Lock()
		defer a.store.trackingMu.Unlock()
		var found int
		err := a.store.db.QueryRow("SELECT 1 FROM records WHERE user_id=? AND book_id=? AND kind='book' AND record_id='default'", u, id).Scan(&found)
		if err != nil {
			failure(w, err)
			return
		}
		if l.SeriesKey != "" {
			var existing int64
			e := a.store.db.QueryRow("SELECT series_id FROM tracking_links WHERE user_id=? AND series_key=? AND book_id!=? LIMIT 1", u, l.SeriesKey, id).Scan(&existing)
			if e == nil && existing != l.SeriesID {
				respond(w, 409, map[string]string{"error": "This series already has a different link. Choose book-only tracking to override it."})
				return
			}
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				failure(w, e)
				return
			}
		}
		_, err = a.store.db.Exec(`INSERT INTO tracking_links(user_id,book_id,series_key,series_id,title,volume,auto,complete_entry,is_private) VALUES(?,?,?,?,?,?,?,?,coalesce(?,1)) ON CONFLICT(user_id,book_id) DO UPDATE SET series_key=excluded.series_key,series_id=excluded.series_id,title=excluded.title,volume=excluded.volume,auto=excluded.auto,complete_entry=excluded.complete_entry,is_private=coalesce(?,tracking_links.is_private),last_step=0,last_chapter=0,last_sync=0,error='',next_attempt=0`, u, id, l.SeriesKey, l.SeriesID, l.Title, l.Volume, l.Auto, l.CompleteEntry, l.Private, l.Private)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("DELETE /v1/tracking/books/{id}", func(w http.ResponseWriter, r *http.Request) {
		u := a.authorized(w, r)
		if u == "" {
			return
		}
		a.store.trackingMu.Lock()
		defer a.store.trackingMu.Unlock()
		_, err := a.store.db.Exec("DELETE FROM tracking_links WHERE user_id=? AND book_id=?", u, r.PathValue("id"))
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
	mux.HandleFunc("POST /v1/tracking/sync", func(w http.ResponseWriter, r *http.Request) {
		u := a.authorized(w, r)
		if u == "" {
			return
		}
		if err := a.store.runTracking(r.Context(), u, provider); err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
}
