package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMultipleFoldersSyncCompatibility(t *testing.T) {
	s, h := fixture(t)
	user, _ := s.authenticate(login(t, h, "alice"))
	cases := []struct {
		value   string
		folders []string
	}{
		{`{"title":"Book","folder":"A"}`, []string{"A"}},
		{`{"title":"Book","folders":["A","B"],"folder":"A"}`, []string{"A", "B"}},
		{`{"title":"Edited"}`, []string{"A", "B"}},
		{`{"title":"Moved","folder":"C"}`, []string{"C", "B"}},
		{`{"title":"Removed primary","folder":""}`, []string{"B"}},
		{`{"title":"Root","folders":[],"folder":""}`, []string{}},
	}
	for i, c := range cases {
		op := Operation{ID: fmt.Sprintf("folders-%d", i), BookID: hashBook(nil), Kind: "book", RecordID: "default", BaseRevision: int64(i), Value: json.RawMessage(c.value)}
		out, err := s.Sync(context.Background(), user, SyncRequest{Cursor: int64(i), Operations: []Operation{op}})
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Folder  string
			Folders []string
		}
		json.Unmarshal(out.Changes[0].Candidates[0].Value, &got)
		if !reflect.DeepEqual(got.Folders, c.folders) {
			t.Fatalf("step %d: %s", i, out.Changes[0].Candidates[0].Value)
		}
		primary := ""
		if len(c.folders) > 0 {
			primary = c.folders[0]
		}
		if got.Folder != primary {
			t.Fatal(got)
		}
		retry, err := s.Sync(context.Background(), user, SyncRequest{Operations: []Operation{op}})
		if err != nil || retry.Results[0].Revision != int64(i+1) {
			t.Fatal("replay", err)
		}
	}
}
func TestMultipleFoldersValidation(t *testing.T) {
	for _, value := range []string{`{"title":"Book","folders":[]}`, `{"title":"Book","folders":["A","B"]}`} {
		if err := validateOperation(Operation{ID: "valid", BookID: hashBook(nil), Kind: "book", RecordID: "default", Value: json.RawMessage(value)}); err != nil {
			t.Fatal(value, err)
		}
	}
	for _, value := range []string{`null`, `[""]`, `["A","A"]`, `["../A"]`, `[" A"]`, `[null]`, `"A"`, `["A"],"folder":"B"`} {
		raw := `{"title":"Book","folders":` + value + `}`
		if err := validateOperation(Operation{ID: "invalid", BookID: hashBook(nil), Kind: "book", RecordID: "default", Value: json.RawMessage(raw)}); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}

func TestMultipleFoldersWatchShareAndBackup(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Watched"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Watched", "book.epub"), epubBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	watch, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(watch); err != nil {
		t.Fatal(err)
	}
	out := syncRequest(t, h, token, 0)
	rec := out.Changes[len(out.Changes)-1]
	var value struct {
		Folder  string
		Folders []string
	}
	json.Unmarshal(rec.Candidates[0].Value, &value)
	if !reflect.DeepEqual(value.Folders, []string{"Watched"}) {
		t.Fatal(string(rec.Candidates[0].Value))
	}
	syncRequest(t, h, token, out.Cursor, Operation{ID: "curated", BookID: rec.BookID, Kind: "book", RecordID: "default", BaseRevision: rec.Revision, Value: json.RawMessage(`{"title":"Curated","folders":["Favorites","To read"]}`)})
	if err = s.ScanWatch(watch); err != nil {
		t.Fatal(err)
	}
	member, _ := s.authenticate(login(t, h, "bob"))
	if _, err = s.db.Exec("INSERT INTO libraries VALUES ('folder-shared','Shared',?)", user); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO library_members VALUES ('folder-shared',?)", member); err != nil {
		t.Fatal(err)
	}
	if err = s.sharedSeeds(member); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if err = s.Backup(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err = RestoreBackup(archive, destination); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(filepath.Join(destination, "quire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	for _, u := range []string{user, member} {
		var raw string
		if err = restored.db.QueryRow("SELECT candidates FROM records WHERE user_id=? AND book_id=? AND kind='book'", u, rec.BookID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var candidates []Candidate
		json.Unmarshal([]byte(raw), &candidates)
		json.Unmarshal(candidates[0].Value, &value)
		if !reflect.DeepEqual(value.Folders, []string{"Favorites", "To read"}) || value.Folder != "Favorites" {
			t.Fatal(raw)
		}
	}
}

func TestMultipleFoldersLegacyRecordMigrationAndConflict(t *testing.T) {
	s, h := fixture(t)
	user, _ := s.authenticate(login(t, h, "alice"))
	id := hashBook(nil)
	candidates, _ := json.Marshal([]Candidate{{OperationID: "old", Value: json.RawMessage(`{"title":"Legacy","folder":"Legacy"}`)}})
	if _, err := s.db.Exec("INSERT INTO records VALUES (?,?,'book','default',1,?)", user, id, string(candidates)); err != nil {
		t.Fatal(err)
	}
	out, err := s.Sync(context.Background(), user, SyncRequest{Operations: []Operation{{ID: "migration", BookID: id, Kind: "book", RecordID: "default", BaseRevision: 1, Value: json.RawMessage(`{"title":"Edited"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	var value struct{ Folders []string }
	json.Unmarshal(out.Changes[0].Candidates[0].Value, &value)
	if !reflect.DeepEqual(value.Folders, []string{"Legacy"}) {
		t.Fatal(string(out.Changes[0].Candidates[0].Value))
	}
	conflict, err := s.Sync(context.Background(), user, SyncRequest{Cursor: out.Cursor, Operations: []Operation{{ID: "conflicting", BookID: id, Kind: "book", RecordID: "default", BaseRevision: 1, Value: json.RawMessage(`{"title":"Other","folders":["Other"]}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if !conflict.Results[0].Conflict || len(conflict.Changes[0].Candidates) != 2 {
		t.Fatal(conflict)
	}
	json.Unmarshal(conflict.Changes[0].Candidates[0].Value, &value)
	if !reflect.DeepEqual(value.Folders, []string{"Legacy"}) {
		t.Fatal(value)
	}
	json.Unmarshal(conflict.Changes[0].Candidates[1].Value, &value)
	if !reflect.DeepEqual(value.Folders, []string{"Other"}) {
		t.Fatal(value)
	}
}

func TestMultipleFoldersLimit(t *testing.T) {
	folders := make([]string, 33)
	for i := range folders {
		folders[i] = fmt.Sprintf("Folder %d", i)
	}
	for _, count := range []int{32, 33} {
		value, _ := json.Marshal(map[string]any{"title": "Book", "folders": folders[:count]})
		err := validateOperation(Operation{ID: "limit", BookID: hashBook(nil), Kind: "book", RecordID: "default", Value: value})
		if (err == nil) != (count == 32) {
			t.Fatalf("count %d: %v", count, err)
		}
	}
}

func TestMultipleFoldersLegacyConflictPreservesFolderIntent(t *testing.T) {
	for _, legacy := range []string{`{"title":"Offline title"}`, `{"title":"Offline title","folder":"Moved"}`} {
		t.Run(legacy, func(t *testing.T) {
			s, h := fixture(t)
			user, _ := s.authenticate(login(t, h, "alice"))
			id := hashBook(nil)
			send := func(opid string, base int64, value string) SyncResponse {
				t.Helper()
				out, err := s.Sync(context.Background(), user, SyncRequest{Operations: []Operation{{ID: opid, BookID: id, Kind: "book", RecordID: "default", BaseRevision: base, Value: json.RawMessage(value)}}})
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			send("modern", 0, `{"title":"Book","folder":"A","folders":["A","B"]}`)
			out := send("legacy-offline", 0, legacy)
			rec := out.Changes[len(out.Changes)-1]
			if !out.Results[0].Conflict {
				t.Fatal("expected conflict")
			}
			candidate := rec.Candidates[len(rec.Candidates)-1]
			var fields map[string]json.RawMessage
			json.Unmarshal(candidate.Value, &fields)
			if _, present := fields["folders"]; present {
				t.Fatalf("legacy conflict synthesized authoritative folders: %s", candidate.Value)
			}
			var original map[string]json.RawMessage
			json.Unmarshal([]byte(legacy), &original)
			if string(fields["folder"]) != string(original["folder"]) {
				t.Fatalf("folder intent changed: %s", candidate.Value)
			}
			// A modern resolver applies the legacy candidate to existing memberships before
			// submitting its authoritative resolution, retaining secondary memberships.
			resolved := canonicalBookFolders(candidate.Value, rec.Candidates[0].Value)
			result := send("resolved", rec.Revision, string(resolved))
			final := result.Changes[len(result.Changes)-1]
			if len(final.Candidates) != 1 {
				t.Fatal(final)
			}
			var got struct{ Folders []string }
			json.Unmarshal(final.Candidates[0].Value, &got)
			primary := "A"
			if original["folder"] != nil {
				primary = "Moved"
			}
			if !reflect.DeepEqual(got.Folders, []string{primary, "B"}) {
				t.Fatal(string(final.Candidates[0].Value))
			}
		})
	}
}
