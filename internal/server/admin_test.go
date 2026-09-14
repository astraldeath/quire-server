package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupAndInvites(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewConfiguredHandler(s, "http://localhost:8080", "Test", "setup-secret")
	request(t, h, "POST", "/v1/setup", "", map[string]string{"username": "owner", "password": testPassword, "code": "wrong"}, 401)
	request(t, h, "POST", "/v1/setup", "", map[string]string{"username": "owner", "password": testPassword, "code": "setup-secret"}, 201)
	request(t, h, "POST", "/v1/setup", "", map[string]string{"username": "second", "password": testPassword, "code": "setup-secret"}, 409)
	token := login(t, h, "owner")
	invite := request(t, h, "POST", "/v1/admin/invites", token, nil, 201)
	var code string
	json.Unmarshal(invite["code"], &code)
	request(t, h, "POST", "/v1/register", "", map[string]string{"username": "guest", "password": testPassword, "code": code}, 201)
	request(t, h, "POST", "/v1/register", "", map[string]string{"username": "other", "password": testPassword, "code": code}, 400)
	guest := login(t, h, "guest")
	request(t, h, "POST", "/v1/admin/invites", guest, nil, 403)
	me := request(t, h, "GET", "/v1/me", token, nil, 200)
	var id string
	json.Unmarshal(me["id"], &id)
	request(t, h, "PUT", "/v1/admin/users/"+id, token, map[string]bool{"admin": false, "disabled": false}, 409)
	if _, err = s.Login("other", testPassword, "test"); err != ErrUnauthorized {
		t.Fatal("failed invitation left user behind")
	}
}
func TestWebDoesNotExposeFiles(t *testing.T) {
	h := WebUI(NewHandler(nil, "", ""), t.TempDir())
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest("GET", "/data/quire.db", nil))
	if r.Code != 404 {
		t.Fatal(r.Code)
	}
}

func TestSharedLibraryRevocation(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	owner, e := s.register("owner", testPassword, "", true)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CreateUser("member", testPassword); e != nil {
		t.Fatal(e)
	}
	var member string
	s.db.QueryRow("SELECT id FROM users WHERE username='member'").Scan(&member)
	s.db.Exec("INSERT INTO libraries VALUES ('shared','Shared',?)", owner)
	s.db.Exec("INSERT INTO library_members VALUES ('shared',?)", member)
	book := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s.db.Exec("INSERT INTO files VALUES (?,?,'upload','',100)", owner, book)
	actual, e := s.accessibleOwner(member, book)
	if e != nil || actual != owner {
		t.Fatal(actual, e)
	}
	files, e := s.fileList(member)
	if e != nil || len(files) != 1 || files[0].Uploaded {
		t.Fatal(files, e)
	}
	s.db.Exec("DELETE FROM library_members WHERE user_id=?", member)
	if _, e = s.accessibleOwner(member, book); e == nil {
		t.Fatal("revoked member can download")
	}
	files, e = s.fileList(member)
	if e != nil || len(files) != 0 {
		t.Fatal(files, e)
	}
}

func TestExpiredInviteAndDisabledSession(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	_, e = s.register("owner", testPassword, "", true)
	if e != nil {
		t.Fatal(e)
	}
	h := NewHandler(s, "http://localhost:8080", "Test")
	owner := login(t, h, "owner")
	v := request(t, h, "POST", "/v1/admin/invites", owner, nil, 201)
	var code, id string
	json.Unmarshal(v["code"], &code)
	json.Unmarshal(v["id"], &id)
	s.db.Exec("UPDATE invites SET expires_at=0 WHERE id=?", id)
	request(t, h, "POST", "/v1/register", "", map[string]string{"username": "expired", "password": testPassword, "code": code}, 400)
	if e = s.CreateUser("member", testPassword); e != nil {
		t.Fatal(e)
	}
	member := login(t, h, "member")
	var uid string
	s.db.QueryRow("SELECT id FROM users WHERE username='member'").Scan(&uid)
	request(t, h, "PUT", "/v1/admin/users/"+uid, owner, map[string]bool{"admin": false, "disabled": true}, 204)
	request(t, h, "GET", "/v1/me", member, nil, 401)
	request(t, h, "POST", "/v1/sessions", "", map[string]string{"username": "member", "password": testPassword, "deviceName": "test"}, 401)
}
func TestPasswordChangeRevokesSessions(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.CreateUser("member", testPassword)
	h := NewHandler(s, "http://localhost:8080", "Test")
	token := login(t, h, "member")
	request(t, h, "PUT", "/v1/me/password", token, map[string]string{"current": "wrong", "password": "replacement password"}, 401)
	request(t, h, "PUT", "/v1/me/password", token, map[string]string{"current": testPassword, "password": "replacement password"}, 204)
	request(t, h, "GET", "/v1/me", token, nil, 401)
	if _, e = s.Login("member", "replacement password", "test"); e != nil {
		t.Fatal(e)
	}
}

func TestAddingWatchScansImmediately(t *testing.T) {
	s, h := fixture(t)
	if err := s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "book.epub"), epubBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	request(t, h, "POST", "/v1/admin/watches", login(t, h, "alice"), map[string]string{"username": "alice", "path": root}, 201)
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM files").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected automatic import, got %d books", count)
	}
}
