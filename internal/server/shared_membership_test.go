package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestSharedMembershipSeedsMigrationAndExplicitAdd(t *testing.T) {
	s, _ := fixture(t)
	var owner, member string
	s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&owner)
	s.db.QueryRow("SELECT id FROM users WHERE username='bob'").Scan(&member)
	s.db.Exec("INSERT INTO libraries VALUES ('membership','Shared',?)", owner)
	s.db.Exec("INSERT INTO library_members VALUES ('membership',?)", member)
	id := strings.Repeat("b", 64)
	s.db.Exec("INSERT INTO files VALUES (?,?,'upload','',100)", owner, id)
	if err := s.sharedSeeds(member); err != nil {
		t.Fatal(err)
	}
	read := func() Record {
		var raw string
		var rec Record
		if err := s.db.QueryRow("SELECT revision,candidates FROM records WHERE user_id=? AND book_id=? AND kind='book'", member, id).Scan(&rec.Revision, &raw); err != nil {
			t.Fatal(err)
		}
		json.Unmarshal([]byte(raw), &rec.Candidates)
		return rec
	}
	membership := func() bool {
		var v map[string]any
		json.Unmarshal(read().Candidates[0].Value, &v)
		return v["inLibrary"] == true
	}
	if membership() {
		t.Fatal("shared seed became personal")
	}
	// Simulate an old automatic seed; migration must create a new sync revision.
	old := read()
	var fields map[string]any
	json.Unmarshal(old.Candidates[0].Value, &fields)
	delete(fields, "inLibrary")
	old.Candidates[0].Value, _ = json.Marshal(fields)
	raw, _ := json.Marshal(old.Candidates)
	s.db.Exec("UPDATE records SET candidates=? WHERE user_id=? AND book_id=? AND kind='book'", string(raw), member, id)
	if err := s.sharedSeeds(member); err != nil {
		t.Fatal(err)
	}
	if read().Revision != old.Revision+1 || membership() {
		t.Fatal("old seed migration failed")
	}
	apply := func(value string) {
		_, err := s.Sync(context.Background(), member, SyncRequest{Operations: []Operation{{ID: randomID(), BookID: id, Kind: "book", RecordID: "default", BaseRevision: read().Revision, Value: json.RawMessage(value)}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	apply(`{"title":"Legacy metadata edit","author":""}`)
	if membership() {
		t.Fatal("legacy edit added shared book")
	}
	apply(`{"title":"Added","author":"","inLibrary":true}`)
	if !membership() {
		t.Fatal("explicit add failed")
	}
	if err := s.sharedSeeds(member); err != nil {
		t.Fatal(err)
	}
	if !membership() {
		t.Fatal("subsequent browsing undid add")
	}
	// Explicit 64-character operation IDs must not be mistaken for automatic seeds.
	old = read()
	json.Unmarshal(old.Candidates[0].Value, &fields)
	delete(fields, "inLibrary")
	old.Candidates[0].Value, _ = json.Marshal(fields)
	raw, _ = json.Marshal(old.Candidates)
	s.db.Exec("UPDATE records SET candidates=? WHERE user_id=? AND book_id=? AND kind='book'", string(raw), member, id)
	revision := read().Revision
	if err := s.sharedSeeds(member); err != nil {
		t.Fatal(err)
	}
	if read().Revision != revision {
		t.Fatal("explicit edit migrated as automatic seed")
	}
}
