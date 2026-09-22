package server

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func opdsRequest(h http.Handler, path, user, password string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if user != "" {
		r.SetBasicAuth(user, password)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestOPDSPasswordScopeRevocationAndDisable(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	out := request(t, h, "POST", "/v1/opds/passwords", token, map[string]string{"name": "Reader"}, 201)
	var secret, id string
	json.Unmarshal(out["password"], &secret)
	json.Unmarshal(out["id"], &id)
	for _, path := range []string{"/opds", "/opds/v2"} {
		w := opdsRequest(h, path, "alice", secret)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := opdsRequest(h, "/opds", "", ""); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal(w)
	}
	if w := opdsRequest(h, "/v1/files", "alice", secret); w.Code != 401 {
		t.Fatal("app password authorized normal API")
	}
	if w := opdsRequest(h, "/opds", "bob", secret); w.Code != 401 {
		t.Fatal("cross-user password")
	}
	list := request(t, h, "GET", "/v1/opds/passwords", token, nil, 200)
	if strings.Contains(string(list["passwords"]), secret) {
		t.Fatal("secret disclosed")
	}
	s.db.Exec("UPDATE users SET disabled=1 WHERE username='alice'")
	if w := opdsRequest(h, "/opds", "alice", secret); w.Code != 401 {
		t.Fatal("disabled user")
	}
	s.db.Exec("UPDATE users SET disabled=0 WHERE username='alice'")
	request(t, h, "DELETE", "/v1/opds/passwords/"+id, token, nil, 204)
	if w := opdsRequest(h, "/opds", "alice", secret); w.Code != 401 {
		t.Fatal("revoked password")
	}
}
func TestOPDSPrivacyRecheckedAndMissingFiles(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	data := epubBytes()
	id := hashBook(data)
	r := httptest.NewRequest("PUT", "/v1/books/"+id+"/file", bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	out := request(t, h, "POST", "/v1/opds/passwords", token, map[string]string{"name": "Reader"}, 201)
	var secret string
	json.Unmarshal(out["password"], &secret)
	if w = opdsRequest(h, "/opds/v2?view=all", "alice", secret); w.Code != 200 || !strings.Contains(w.Body.String(), id) {
		t.Fatal(w.Body.String())
	}
	if w = opdsRequest(h, "/opds/books/"+id+"/file", "alice", secret); w.Code != 200 {
		t.Fatal(w.Code)
	}
	state := map[string]any{"credential": map[string]string{"salt": strings.Repeat("a", 32), "hash": strings.Repeat("b", 64)}, "books": map[string]string{id: "hidden"}}
	request(t, h, "POST", "/v1/privacy/sync", token, map[string]any{"revision": 0, "state": state}, 200)
	for _, path := range []string{"/opds?view=all", "/opds/v2?view=all", "/opds/v2?view=folders", "/opds/v2?view=series"} {
		w = opdsRequest(h, path, "alice", secret)
		if w.Code != 200 || strings.Contains(w.Body.String(), id) {
			t.Fatal(path, w.Body.String())
		}
	}
	for _, resource := range []string{"file", "cover"} {
		if w = opdsRequest(h, "/opds/books/"+id+"/"+resource, "alice", secret); w.Code != 404 {
			t.Fatal("privacy leaked", resource, w.Code)
		}
	}
	var user string
	s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&user)
	s.db.Exec("DELETE FROM privacy_settings WHERE user_id=?", user)
	s.db.Exec("DELETE FROM files WHERE user_id=?", user)
	if w = opdsRequest(h, "/opds/v2?view=all", "alice", secret); strings.Contains(w.Body.String(), id) {
		t.Fatal("missing book advertised")
	}
}

func TestOPDSNavigationSearchPaginationAndSharedPrivacy(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	var user, bob string
	s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&user)
	s.db.QueryRow("SELECT id FROM users WHERE username='bob'").Scan(&bob)
	for i := 0; i < 52; i++ {
		id := fmt.Sprintf("%064x", i+1)
		path := s.objectPath(user, id)
		if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, epubBytes(), 0600); e != nil {
			t.Fatal(e)
		}
		s.db.Exec("INSERT INTO files VALUES (?,?,'upload','',1)", user, id)
		tx, _ := s.db.Begin()
		if e := seedBook(tx, user, id, fmt.Sprintf("Book & <%02d>", i), bookMetadata{Author: "A & B", Series: "Series One"}, "Parent/Child"); e != nil {
			t.Fatal(e)
		}
		tx.Commit()
	}
	out := request(t, h, "POST", "/v1/opds/passwords", token, map[string]string{"name": "Reader"}, 201)
	var secret string
	json.Unmarshal(out["password"], &secret)
	read := func(path string) opdsJSONFeed {
		w := opdsRequest(h, path, "alice", secret)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var feed opdsJSONFeed
		if e := json.Unmarshal(w.Body.Bytes(), &feed); e != nil {
			t.Fatal(e)
		}
		return feed
	}
	first := read("/opds/v2?view=all")
	if len(first.Publications) != 50 || first.Metadata.NumberOfItems != 52 {
		t.Fatal(first.Metadata, len(first.Publications))
	}
	second := read("/opds/v2?view=all&page=2")
	if len(second.Publications) != 2 {
		t.Fatal(len(second.Publications))
	}
	// Catalog recency follows stored-file arrival, not later metadata edits.
	newestID := fmt.Sprintf("%064x", 1)
	newestTime := time.Now().Add(time.Hour)
	if e := os.Chtimes(s.objectPath(user, newestID), newestTime, newestTime); e != nil {
		t.Fatal(e)
	}
	if read("/opds/v2?view=recent").Publications[0].Metadata.Identifier != "urn:sha256:"+newestID {
		t.Fatal("recent order ignored file arrival time")
	}
	if len(read("/opds/v2?view=all&q=51").Publications) != 1 {
		t.Fatal("search failed")
	}
	folders := read("/opds/v2?view=folders")
	if len(folders.Navigation) != 1 || folders.Navigation[0].Title != "Parent" {
		t.Fatal(folders)
	}
	folders = read("/opds/v2?view=folders&folder=Parent")
	if len(folders.Navigation) != 1 || folders.Navigation[0].Title != "Child" {
		t.Fatal(folders)
	}
	if read("/opds/v2?view=folders&folder=Parent%2FChild").Metadata.NumberOfItems != 52 {
		t.Fatal("nested folder")
	}
	if len(read("/opds/v2?view=series").Navigation) != 1 {
		t.Fatal("series")
	}
	w := opdsRequest(h, "/opds?view=all", "alice", secret)
	var atom atomFeed
	if e := xml.Unmarshal(w.Body.Bytes(), &atom); e != nil || len(atom.Entries) != 50 {
		t.Fatal(e, w.Body.String())
	}
	// Shared files honor the owner's privacy even when the viewer has no privacy record.
	s.db.Exec("INSERT INTO libraries VALUES ('shared','Shared',?)", user)
	s.db.Exec("INSERT INTO library_members VALUES ('shared',?)", bob)
	bobToken := login(t, h, "bob")
	out = request(t, h, "POST", "/v1/opds/passwords", bobToken, map[string]string{"name": "Reader"}, 201)
	var bobSecret string
	json.Unmarshal(out["password"], &bobSecret)
	id := fmt.Sprintf("%064x", 1)
	state, _ := json.Marshal(PrivacySettings{Books: map[string]string{id: "locked"}})
	s.db.Exec("INSERT INTO privacy_settings VALUES (?,1,?)", user, string(state))
	w = opdsRequest(h, "/opds/v2?view=all", "bob", bobSecret)
	if strings.Contains(w.Body.String(), id) {
		t.Fatal("owner privacy leaked shared book")
	}
	if w = opdsRequest(h, "/opds/books/"+id+"/file", "bob", bobSecret); w.Code != 404 {
		t.Fatal("owner privacy leaked file", w.Code)
	}
}
