package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTrackingLinkPrivacyPreference(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: testBook, Kind: "book", RecordID: "default", Value: json.RawMessage(`{"title":"Book","series":"Novels"}`)})
	request(t, h, "PUT", "/v1/tracking/books/"+testBook, token, map[string]any{"seriesId": 42, "title": "Novel", "private": false}, 204)
	request(t, h, "PUT", "/v1/tracking/books/"+testBook, token, map[string]any{"seriesId": 42, "title": "Novel"}, 204)
	var private bool
	if err := s.db.QueryRow("SELECT is_private FROM tracking_links WHERE user_id=?", user).Scan(&private); err != nil || private {
		t.Fatalf("public preference lost: %v %v", private, err)
	}
}

func TestTrackingCompletionPreservesPausedAndDropped(t *testing.T) {
	for _, state := range []string{"paused", "dropped"} {
		if patch := trackingPatch(trackingLink{CompleteEntry: true}, 2, trackingRemote{State: state}); patch["state"] != nil {
			t.Fatalf("overwrote %s: %v", state, patch)
		}
	}
}

type entryProviderCall struct {
	method  string
	changes map[string]any
}

func entryFixture(t *testing.T) (*Store, http.Handler, string, string, *trackingProvider, *map[string]any, *[]entryProviderCall) {
	t.Helper()
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: testBook, Kind: "book", RecordID: "default", Value: json.RawMessage(`{"title":"Book","series":"Novels"}`)})
	request(t, h, "PUT", "/v1/tracking/books/"+testBook, token, map[string]any{"seriesId": 42, "title": "Novel", "auto": true}, 204)
	if err := s.saveTrackingToken(user, "account-one", "Reader", "mb-test"); err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"state": "reading", "progress_chapter": float64(9), "progress_volume": float64(2), "rating": float64(80), "start_date": "2026-09-01T00:00:00.000Z", "finish_date": nil, "is_private": true, "note": "keep me"}
	calls := []entryProviderCall{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/my/library/42" || r.Header.Get("x-api-key") != "mb-test" {
			t.Errorf("unexpected provider request %s", r.URL.Path)
		}
		call := entryProviderCall{method: r.Method}
		if r.Method != "GET" {
			if err := json.NewDecoder(r.Body).Decode(&call.changes); err != nil {
				t.Error(err)
			}
		}
		calls = append(calls, call)
		if r.Method == "GET" {
			if entry == nil {
				w.WriteHeader(404)
			} else {
				json.NewEncoder(w).Encode(map[string]any{"data": entry})
			}
			return
		}
		if entry == nil {
			entry = map[string]any{}
		}
		for key, value := range call.changes {
			entry[key] = value
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(upstream.Close)
	p := newTrackingProvider()
	p.base = upstream.URL
	mux := http.NewServeMux()
	a := &api{store: s}
	a.trackingEntryRoutes(mux, p)
	return s, mux, token, user, p, &entry, &calls
}

func TestTrackingEntryReadAndMinimalEdit(t *testing.T) {
	_, h, token, _, _, entry, calls := entryFixture(t)
	out := request(t, h, "GET", "/v1/tracking/entries/42", token, nil, 200)
	var initial map[string]any
	json.Unmarshal(out["entry"], &initial)
	if initial["start_date"] != "2026-09-01" || initial["note"] != nil || string(out["accountId"]) != `"account-one"` {
		t.Fatalf("wrong read response: %v", out)
	}
	changes := map[string]any{"state": "paused", "progress_chapter": float64(4.5), "progress_volume": nil, "rating": nil, "start_date": nil, "finish_date": "2026-09-15", "is_private": false}
	request(t, h, "PUT", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one", "changes": changes}, 200)
	if len(*calls) != 4 || (*calls)[2].method != "PUT" || !reflect.DeepEqual((*calls)[2].changes, changes) {
		t.Fatalf("wrong patch requests: %v", *calls)
	}
	if (*entry)["note"] != "keep me" {
		t.Fatal("provider note changed")
	}
}

func TestTrackingEntryEnsureAndMissingEdit(t *testing.T) {
	s, h, token, user, _, entry, calls := entryFixture(t)
	request(t, h, "POST", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one", "is_private": false}, 200)
	if (*calls)[1].method != "PUT" || !reflect.DeepEqual((*calls)[1].changes, map[string]any{"is_private": false}) || (*entry)["rating"] != float64(80) || (*entry)["progress_chapter"] != float64(9) {
		t.Fatalf("ensure overwrote entry: %v %v", *calls, *entry)
	}
	links, _ := s.trackingLinks(user)
	if links[0].Private == nil || *links[0].Private {
		t.Fatal("privacy preference not persisted")
	}
	*entry = nil
	*calls = nil
	out := request(t, h, "GET", "/v1/tracking/entries/42", token, nil, 200)
	if string(out["entry"]) != "null" {
		t.Fatal("missing entry must be null")
	}
	request(t, h, "PUT", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one", "changes": map[string]any{"rating": 20}}, 409)
	if len(*calls) != 2 {
		t.Fatal("missing manual edit created an entry")
	}
	*calls = nil
	request(t, h, "POST", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one", "is_private": false}, 200)
	if (*calls)[1].method != "POST" || !reflect.DeepEqual((*calls)[1].changes, map[string]any{"state": "plan_to_read", "is_private": false}) {
		t.Fatalf("unexpected creation: %v", *calls)
	}
}

func TestTrackingEntryGuards(t *testing.T) {
	s, h, token, user, _, _, calls := entryFixture(t)
	request(t, h, "GET", "/v1/tracking/entries/42", "", nil, 401)
	request(t, h, "GET", "/v1/tracking/entries/43", token, nil, 404)
	request(t, h, "PUT", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "different", "changes": map[string]any{"rating": 10}}, 409)
	request(t, h, "POST", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "different", "is_private": false}, 409)
	request(t, h, "POST", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one"}, 400)
	for _, changes := range []map[string]any{
		{}, {"note": "no"}, {"state": "unknown"}, {"state": nil}, {"rating": 101}, {"rating": "80"}, {"progress_volume": -1}, {"progress_chapter": 10001}, {"is_private": nil}, {"is_private": "true"}, {"start_date": "2026-02-30"}, {"start_date": "1678-12-31"}, {"finish_date": "2263-01-01"}, {"start_date": "2026-09-01T00:00:00Z"},
	} {
		request(t, h, "PUT", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one", "changes": changes}, 400)
	}
	if len(*calls) != 0 {
		t.Fatalf("invalid request reached provider: %v", *calls)
	}
	if _, err := s.db.Exec("DELETE FROM tracking_accounts WHERE user_id=?", user); err != nil {
		t.Fatal(err)
	}
	request(t, h, "GET", "/v1/tracking/entries/42", token, nil, 409)
}

func TestTrackingManualProgressAcknowledgesAllLinkedBooks(t *testing.T) {
	s, h, token, user, p, _, calls := entryFixture(t)
	// Two books linked to one provider entry both have unsent local progress.
	for _, id := range []string{testBook, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		if id != testBook {
			if _, err := s.db.Exec("INSERT INTO records(user_id,book_id,kind,record_id,revision,candidates) SELECT user_id,?,kind,record_id,revision,candidates FROM records WHERE user_id=? AND book_id=? AND kind='book'", id, user, testBook); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("INSERT INTO tracking_links(user_id,book_id,series_key,series_id,title,volume,auto,complete_entry) VALUES(?,?,'',42,'Novel',0,1,0)", user, id); err != nil {
				t.Fatal(err)
			}
		}
		raw := `[{"value":{"fraction":1,"currentChapter":9,"completedChapter":8}}]`
		if _, err := s.db.Exec("INSERT INTO records(user_id,book_id,kind,record_id,revision,candidates) VALUES(?,?,'position','default',1,?)", user, id, raw); err != nil {
			t.Fatal(err)
		}
	}
	request(t, h, "PUT", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one", "changes": map[string]any{"state": "paused", "progress_chapter": 3, "is_private": false}}, 200)
	links, err := s.trackingLinks(user)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if l.LastStep != 2 || l.LastChapter != 9 || l.Private == nil || *l.Private {
			t.Fatalf("link not acknowledged: %+v", l)
		}
	}
	before := len(*calls)
	if err := s.runTracking(context.Background(), user, p); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != before {
		t.Fatal("autosync immediately undid manual edit")
	}
	if _, err := s.db.Exec(`UPDATE records SET candidates='[{"value":{"fraction":1,"completedChapter":10}}]' WHERE user_id=? AND book_id=? AND kind='position'`, user, testBook); err != nil {
		t.Fatal(err)
	}
	if err := s.runTracking(context.Background(), user, p); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != before+2 || (*calls)[before+1].changes["progress_chapter"] != float64(10) || (*calls)[before+1].changes["state"] != nil {
		t.Fatalf("future reads did not sync correctly: %v", *calls)
	}
}

func TestTrackingPrivacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quire.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO users(id,username,salt,password_hash) VALUES ('user','reader',X'',X''); INSERT INTO tracking_links(user_id,book_id,series_key,series_id,title,volume,auto,complete_entry) VALUES ('user','book','',42,'Novel',0,0,0); ALTER TABLE tracking_links DROP COLUMN is_private; ALTER TABLE scan_status DROP COLUMN skipped_files; ALTER TABLE scan_status DROP COLUMN omitted_skipped_files; DROP TABLE folder_catalog; DROP TABLE catalog_sources; DROP TABLE catalog_operations; DROP TABLE opds_passwords; PRAGMA user_version=10;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	links, err := s.trackingLinks("user")
	if err != nil || len(links) != 1 || links[0].Private == nil || !*links[0].Private {
		t.Fatalf("migration failed: %v %v", links, err)
	}
}

func TestTrackingSeriesPrivacyPreference(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: testBook, Kind: "book", RecordID: "default", Value: json.RawMessage(`{"title":"Book","series":"Novels"}`)})
	in := map[string]any{"seriesKey": "Novels", "seriesId": 42, "title": "Novel", "private": false}
	request(t, h, "PUT", "/v1/tracking/series", token, in, 200)
	delete(in, "private")
	request(t, h, "PUT", "/v1/tracking/series", token, in, 200)
	links, _ := s.trackingLinks(user)
	if len(links) != 1 || links[0].Private == nil || *links[0].Private {
		t.Fatal("omitted series privacy reset public choice")
	}
	in["private"] = true
	request(t, h, "PUT", "/v1/tracking/series", token, in, 200)
	links, _ = s.trackingLinks(user)
	if links[0].Private == nil || !*links[0].Private {
		t.Fatal("explicit series privacy ignored")
	}
}

func TestTrackingWorkerCreatesWithSavedPrivacy(t *testing.T) {
	for _, private := range []bool{false, true} {
		s, h, token, user, p, entry, calls := entryFixture(t)
		request(t, h, "PUT", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one", "changes": map[string]any{"is_private": private}}, 200)
		*entry = nil
		*calls = nil
		if _, err := s.db.Exec(`INSERT INTO records(user_id,book_id,kind,record_id,revision,candidates) VALUES(?,?,'position','default',1,'[{"value":{"fraction":0.5}}]')`, user, testBook); err != nil {
			t.Fatal(err)
		}
		if err := s.runTracking(context.Background(), user, p); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 2 || (*calls)[1].method != "POST" || !reflect.DeepEqual((*calls)[1].changes, map[string]any{"state": "reading", "is_private": private}) {
			t.Fatalf("wrong worker creation privacy: %v", *calls)
		}
	}
}

func TestTrackingEntryFallsBackAfterSuccessfulWrite(t *testing.T) {
	s, h, token, user, _, _, _ := entryFixture(t)
	puts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			puts++
			w.WriteHeader(204)
			return
		}
		if puts > 0 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"data":{"state":"reading","rating":80,"start_date":"2026-09-01T00:00:00Z","is_private":true}}`))
	}))
	defer upstream.Close()
	p := newTrackingProvider()
	p.base = upstream.URL
	mux := http.NewServeMux()
	(&api{store: s}).trackingEntryRoutes(mux, p)
	out := request(t, mux, "PUT", "/v1/tracking/entries/42", token, map[string]any{"expectedAccountId": "account-one", "changes": map[string]any{"rating": 10}}, 200)
	var entry map[string]any
	json.Unmarshal(out["entry"], &entry)
	if puts != 1 || entry["rating"] != float64(10) || entry["start_date"] != "2026-09-01" {
		t.Fatalf("fallback lost entry: %v", entry)
	}
	if _, err := s.db.Exec(`UPDATE records SET candidates='[{"deleted":true}]' WHERE user_id=? AND kind='book'`, user); err != nil {
		t.Fatal(err)
	}
	request(t, h, "GET", "/v1/tracking/entries/42", token, nil, 404)
}
