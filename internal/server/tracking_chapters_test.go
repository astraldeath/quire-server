package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrackingChapterMigrationKeepsExistingLinks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quire.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Model an existing v7 database, including an acknowledged reading link.
	if _, err = s.db.Exec(`INSERT INTO users(id,username,salt,password_hash) VALUES ('user','reader',X'',X'');
		INSERT INTO tracking_links(user_id,book_id,series_key,series_id,title,volume,auto,complete_entry,last_step) VALUES ('user','book','',1,'Novel',0,1,0,1);
		ALTER TABLE tracking_links DROP COLUMN last_chapter; ALTER TABLE tracking_links DROP COLUMN is_private; DROP TABLE reading_activities; DROP TABLE folder_catalog; DROP TABLE privacy_settings;
		ALTER TABLE scan_status DROP COLUMN skipped_files; ALTER TABLE scan_status DROP COLUMN omitted_skipped_files; DROP TABLE catalog_sources; DROP TABLE catalog_operations; DROP TABLE opds_passwords; PRAGMA user_version=7;`); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	links, err := s.trackingLinks("user")
	if err != nil || len(links) != 1 || links[0].LastStep != 1 || links[0].LastChapter != 0 || !links[0].Auto {
		t.Fatalf("migration lost existing tracking state: %v %v", links, err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 14 {
		t.Fatalf("migration version %d: %v", version, err)
	}
}

func TestPositionCompletedChapterValidation(t *testing.T) {
	for _, tc := range []struct {
		chapter string
		valid   bool
	}{
		{"0", true}, {"7", true}, {"100000", true}, {"-1", false}, {"1.5", false}, {"100001", false}, {`"2"`, false},
	} {
		t.Run(tc.chapter, func(t *testing.T) {
			err := validateOperation(Operation{ID: "chapter", BookID: testBook, Kind: "position", RecordID: "default", Value: json.RawMessage(`{"cfi":"chapter","fraction":0.5,"completedChapter":` + tc.chapter + `}`)})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestPositionCurrentChapterValidation(t *testing.T) {
	for _, tc := range []struct {
		chapter string
		valid   bool
	}{
		{"1", true}, {"9", true}, {"100000", true}, {"0", false}, {"-1", false}, {"1.5", false}, {"100001", false}, {`"9"`, false},
	} {
		err := validateOperation(Operation{ID: "chapter", BookID: testBook, Kind: "position", RecordID: "default", Value: json.RawMessage(`{"cfi":"chapter","fraction":0.5,"section":"009—My First Monster","currentChapter":` + tc.chapter + `,"completedChapter":8}`)})
		if (err == nil) != tc.valid {
			t.Fatalf("chapter=%s valid=%v error=%v", tc.chapter, tc.valid, err)
		}
	}
}

func TestTrackingChaptersAdvanceAfterReadingAcknowledgement(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, err := s.authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 64)
	syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: id, Kind: "book", RecordID: "default", Value: json.RawMessage(`{"title":"Web novel"}`)})
	request(t, h, "PUT", "/v1/tracking/books/"+id, token, trackingLink{SeriesID: 42, Title: "Web novel", Auto: true}, 204)
	if err := s.saveTrackingToken(user, "test-user", "Reader", "mb-test"); err != nil {
		t.Fatal(err)
	}
	remoteChapter, calls := 2.0, 0
	patches := []map[string]any{}
	fail := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == "GET" {
			fmt.Fprintf(w, `{"data":{"state":"reading","progress_chapter":%g}}`, remoteChapter)
			return
		}
		if fail {
			w.WriteHeader(503)
			return
		}
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Error(err)
		}
		patches = append(patches, patch)
		if chapter, ok := patch["progress_chapter"].(float64); ok {
			remoteChapter = chapter
		}
		w.Write([]byte(`{"data":true}`))
	}))
	defer upstream.Close()
	p := newTrackingProvider()
	p.base = upstream.URL
	var revision int64
	position := func(chapter int) {
		t.Helper()
		out := syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: id, Kind: "position", RecordID: "default", BaseRevision: revision, Value: json.RawMessage(fmt.Sprintf(`{"cfi":"chapter","fraction":0.5,"currentChapter":%d,"completedChapter":%d}`, chapter, chapter-1))})
		for _, r := range out.Changes {
			if r.Kind == "position" && r.Revision > revision {
				revision = r.Revision
			}
		}
	}
	run := func() {
		t.Helper()
		if err := s.runTracking(context.Background(), user, p); err != nil {
			t.Fatal(err)
		}
	}
	position(3)
	run()
	position(4)
	run()
	if len(patches) != 2 || patches[0]["progress_chapter"] != float64(3) || patches[1]["progress_chapter"] != float64(4) {
		t.Fatalf("missing chapter advances: %v", patches)
	}
	before := calls
	run()
	if calls != before {
		t.Fatal("unchanged chapter retried")
	}
	remoteChapter = 10
	position(5)
	run()
	if len(patches) != 2 {
		t.Fatal("remote chapter regressed")
	}
	fail = true
	position(11)
	run()
	links, _ := s.trackingLinks(user)
	if links[0].Error == "" {
		t.Fatal("failure not retained")
	}
	fail = false
	if _, err := s.db.Exec("UPDATE tracking_links SET next_attempt=0 WHERE user_id=?", user); err != nil {
		t.Fatal(err)
	}
	run()
	if remoteChapter != 11 {
		t.Fatal("failed chapter not retried")
	}
	links, err = s.trackingLinks(user)
	if err != nil || links[0].LastChapter != 11 || links[0].LastStep != 1 {
		t.Fatalf("chapter acknowledgement missing: %v %v", links, err)
	}
	state := request(t, h, "GET", "/v1/tracking", token, nil, 200)
	var returned []trackingLink
	if err := json.Unmarshal(state["links"], &returned); err != nil || len(returned) != 1 || returned[0].LastChapter != 11 {
		t.Fatalf("chapter acknowledgement not returned: %s", state["links"])
	}
	position(10001)
	before = calls
	run()
	if calls != before {
		t.Fatal("out-of-range chapter sent to provider")
	}
	links, _ = s.trackingLinks(user)
	if links[0].LastChapter != 11 || links[0].Error == "" {
		t.Fatal("unsupported chapter was acknowledged or failure hidden")
	}
	// A volume-specific link must never publish TOC-local chapter numbers.
	request(t, h, "PUT", "/v1/tracking/books/"+id, token, trackingLink{SeriesID: 42, Title: "Volume", Volume: 2, Auto: true}, 204)
	position(12)
	run()
	if remoteChapter != 11 {
		t.Fatal("volume-specific link published chapter count")
	}
	for _, patch := range patches {
		if len(patch) != 1 {
			t.Fatalf("unrelated provider fields overwritten: %v", patch)
		}
	}
}
