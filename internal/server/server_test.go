package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentRetriesCommitExactlyOnce(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, err := s.authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := s.Sync(context.Background(), user, SyncRequest{Operations: []Operation{op("retry-same", 0, false)}})
			errs <- err
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	result := syncRequest(t, h, token, 0)
	if result.Cursor != 1 || len(result.Changes) != 1 {
		t.Fatal("concurrent retry duplicated record")
	}
}

func TestSameBookAndOperationIDsRemainPrivate(t *testing.T) {
	_, h := fixture(t)
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")
	syncRequest(t, h, alice, 0, op("same-op", 0, false))
	other := op("same-op", 0, false)
	other.Value = json.RawMessage(`{"kind":"highlight","cfi":"epubcfi(/6/2)","note":"Bob private note"}`)
	b := syncRequest(t, h, bob, 0, other)
	a := syncRequest(t, h, alice, 0)
	if b.Results[0].Conflict || b.Cursor != 1 || strings.Contains(string(a.Changes[0].Candidates[0].Value), "Bob") {
		t.Fatal("shared book identity leaked state")
	}
}

func TestExpiredAndRevokedSessions(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	out := request(t, h, "GET", "/v1/sessions", token, nil, 200)
	var sessions []Session
	json.Unmarshal(out["sessions"], &sessions)
	request(t, h, "DELETE", "/v1/sessions/"+sessions[0].ID, token, nil, 204)
	request(t, h, "GET", "/v1/sessions", token, nil, 401)
	token = login(t, h, "alice")
	if _, err := s.db.Exec("UPDATE sessions SET expires_at=?", time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	request(t, h, "GET", "/v1/sessions", token, nil, 401)
}

func TestCursorPagingDoesNotSkipChangesAndConflictResolution(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, err := s.authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ops := []Operation{}
		for j := 0; j < 50; j++ {
			v := op(fmt.Sprintf("op-%d-%d", i, j), 0, false)
			v.RecordID = v.ID
			ops = append(ops, v)
		}
		if _, err = s.Sync(context.Background(), user, SyncRequest{Operations: ops}); err != nil {
			t.Fatal(err)
		}
	}
	first := syncRequest(t, h, token, 0)
	if len(first.Changes) != 100 || !first.HasMore || first.Cursor != 100 {
		t.Fatal("first page incorrect")
	}
	second := syncRequest(t, h, token, first.Cursor)
	if len(second.Changes) != 50 || second.HasMore || second.Cursor != 150 {
		t.Fatal("second page incorrect")
	}
	syncRequest(t, h, token, 150, op("edit-1", 0, false), op("edit-2", 0, false))
	resolved := syncRequest(t, h, token, 152, op("resolve", 2, false))
	if len(resolved.Changes[0].Candidates) != 1 || resolved.Results[0].Conflict {
		t.Fatal("explicit resolution failed")
	}
}

func TestPositionConflictIgnoresDeviceProgressOrder(t *testing.T) {
	_, h := fixture(t)
	token := login(t, h, "alice")
	a := Operation{ID: "position-a", BookID: testBook, Kind: "position", RecordID: "default", Value: json.RawMessage(`{"cfi":"epubcfi(/6/4)","fraction":0.9,"section":"Later"}`)}
	b := a
	b.ID = "position-b"
	b.Value = json.RawMessage(`{"cfi":"epubcfi(/6/2)","fraction":0.1,"section":"Earlier"}`)
	result := syncRequest(t, h, token, 0, a, b)
	if len(result.Changes[1].Candidates) != 2 || !result.Results[1].Conflict {
		t.Fatal("lost resume candidate")
	}
}

func TestHTTPRejectsOversizedAndMalformedRequests(t *testing.T) {
	_, h := fixture(t)
	token := login(t, h, "alice")
	for _, body := range []string{`{"cursor":0} {}`, `null`, `{"operations":[null]}`} {
		r := httptest.NewRequest("POST", "/v1/sync", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("body %q got %d", body, w.Code)
		}
	}
	request(t, h, "POST", "/v1/sync", token, map[string]string{"padding": strings.Repeat("x", 2<<20)}, 413)
}

func TestLoginThrottling(t *testing.T) {
	_, h := fixture(t)
	// Invalid shapes also consume the limit, without performing expensive hashing.
	for i := 0; i < 10; i++ {
		request(t, h, "POST", "/v1/sessions", "", map[string]string{}, 400)
	}
	request(t, h, "POST", "/v1/sessions", "", map[string]string{}, 429)
}

const testPassword = "a sufficiently long test password"

var testBook = strings.Repeat("a", 64)

func fixture(t *testing.T) (*Store, http.Handler) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "quire.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, name := range []string{"alice", "bob"} {
		if err = s.CreateUser(name, testPassword); err != nil {
			t.Fatal(err)
		}
	}
	return s, NewHandler(s, "https://books.example", "Test Quire")
}
func request(t *testing.T, h http.Handler, method, path, token string, body any, status int) map[string]json.RawMessage {
	t.Helper()
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	out := map[string]json.RawMessage{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}
func login(t *testing.T, h http.Handler, name string) string {
	out := request(t, h, "POST", "/v1/sessions", "", map[string]string{"username": name, "password": testPassword, "deviceName": "Test device"}, 201)
	var token string
	json.Unmarshal(out["token"], &token)
	if token == "" {
		t.Fatal("missing token")
	}
	return token
}
func op(id string, base int64, deleted bool) Operation {
	return Operation{ID: id, BookID: testBook, Kind: "annotation", RecordID: "note-1", BaseRevision: base, Deleted: deleted, Value: json.RawMessage(`{"kind":"highlight","cfi":"epubcfi(/6/2)","text":"Alice","note":"first note","section":"Chapter 1"}`)}
}
func syncRequest(t *testing.T, h http.Handler, token string, cursor int64, ops ...Operation) SyncResponse {
	out := request(t, h, "POST", "/v1/sync", token, SyncRequest{Cursor: cursor, Operations: ops}, 200)
	b, _ := json.Marshal(out)
	var result SyncResponse
	json.Unmarshal(b, &result)
	return result
}
func TestPrivateSessionsAndOwnerReset(t *testing.T) {
	s, h := fixture(t)
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")
	request(t, h, "GET", "/v1/sessions", "", nil, 401)
	request(t, h, "POST", "/v1/users", alice, map[string]string{}, 404)
	out := request(t, h, "GET", "/v1/sessions", alice, nil, 200)
	var sessions []Session
	json.Unmarshal(out["sessions"], &sessions)
	if len(sessions) != 1 {
		t.Fatal("session missing")
	}
	request(t, h, "DELETE", "/v1/sessions/"+sessions[0].ID, bob, nil, 404)
	if err := s.ResetPassword("alice", "replacement password for test"); err != nil {
		t.Fatal(err)
	}
	request(t, h, "GET", "/v1/sessions", alice, nil, 401)
	request(t, h, "POST", "/v1/sessions", "", map[string]string{"username": "alice", "password": testPassword, "deviceName": "test"}, 401)
	request(t, h, "GET", "/v1/sessions", bob, nil, 200)
}
func TestSyncRetryConflictsTombstonesAndIsolation(t *testing.T) {
	_, h := fixture(t)
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")
	original := op("operation-1", 0, false)
	first := syncRequest(t, h, alice, 0, original)
	if first.Cursor != 1 || len(first.Changes) != 1 || first.Changes[0].Revision != 1 {
		t.Fatalf("first: %+v", first)
	}
	retry := syncRequest(t, h, alice, 0, original)
	if retry.Cursor != 1 || len(retry.Changes) != 1 {
		t.Fatal("retry duplicated change")
	}
	other := syncRequest(t, h, bob, 0)
	if len(other.Changes) != 0 || other.Cursor != 0 {
		t.Fatal("cross-user data leak")
	}
	stale := op("operation-2", 0, false)
	stale.Value = json.RawMessage(`{"kind":"highlight","cfi":"epubcfi(/6/2)","text":"Alice","note":"offline edit","section":"Chapter 1"}`)
	conflict := syncRequest(t, h, alice, 1, stale)
	if len(conflict.Changes) != 1 || len(conflict.Changes[0].Candidates) != 2 {
		t.Fatal("conflict lost an edit")
	}
	del := op("operation-3", 2, true)
	del.Value = nil
	deleted := syncRequest(t, h, alice, 2, del)
	if !deleted.Changes[0].Candidates[0].Deleted {
		t.Fatal("missing tombstone")
	}
	late := op("operation-4", 1, false)
	after := syncRequest(t, h, alice, 3, late)
	if len(after.Changes[0].Candidates) != 2 || !after.Changes[0].Candidates[0].Deleted {
		t.Fatal("offline edit erased tombstone")
	}
	reused := original
	reused.Deleted = true
	reused.Value = nil
	request(t, h, "POST", "/v1/sync", alice, SyncRequest{Operations: []Operation{reused}}, 409)
}
func TestSyncAtomicValidationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CreateUser("alice", testPassword); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, "https://books.example", "Test")
	token := login(t, h, "alice")
	bad := op("bad", 50, false)
	request(t, h, "POST", "/v1/sync", token, SyncRequest{Operations: []Operation{op("good", 0, false), bad}}, 409)
	if got := syncRequest(t, h, token, 0); len(got.Changes) != 0 {
		t.Fatal("partial batch committed")
	}
	syncRequest(t, h, token, 0, op("good", 0, false))
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h = NewHandler(s, "https://books.example", "Test")
	got := syncRequest(t, h, token, 0)
	if len(got.Changes) != 1 || got.Changes[0].Candidates[0].Value == nil {
		t.Fatal("restart lost data/session")
	}
}
func TestDiscoveryAndInputLimits(t *testing.T) {
	_, h := fixture(t)
	out := request(t, h, "GET", "/.well-known/quire", "", nil, 200)
	if string(out["apiVersion"]) != `"1"` {
		t.Fatal("missing version")
	}
	token := login(t, h, "alice")
	bad := op("bad", 0, false)
	bad.BookID = "../someone"
	request(t, h, "POST", "/v1/sync", token, SyncRequest{Operations: []Operation{bad}}, 400)
	request(t, h, "POST", "/v1/sync", token, map[string]any{"cursor": -1}, 400)
	request(t, h, "POST", "/v1/sync", token, map[string]any{"unknown": true}, 400)
	request(t, h, "POST", "/v1/sessions", "", map[string]string{"username": "missing", "password": testPassword, "deviceName": "test"}, 401)
}
