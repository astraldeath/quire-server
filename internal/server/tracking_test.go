package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrackingBackupKeepsLinksButDropsCredentials(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	id := strings.Repeat("d", 64)
	syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: id, Kind: "book", RecordID: "default", Value: json.RawMessage(`{"title":"Book"}`)})
	private := false
	request(t, h, "PUT", "/v1/tracking/books/"+id, token, trackingLink{SeriesID: 42, Title: "Novel", Auto: true, Private: &private}, 204)
	if err := s.saveTrackingToken(user, "provider-user", "Reader", "mb-private"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if err := s.Backup(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restore")
	if err := RestoreBackup(archive, destination); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(filepath.Join(destination, "quire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	links, err := restored.trackingLinks(user)
	if err != nil || len(links) != 1 || links[0].Auto || links[0].Private == nil || *links[0].Private {
		t.Fatal("links missing or tracking enabled", links, err)
	}
	var n int
	restored.db.QueryRow("SELECT count(*) FROM tracking_accounts").Scan(&n)
	if n != 0 {
		t.Fatal("provider credentials restored")
	}
}

func TestTrackingSearchPaths(t *testing.T) {
	for _, q := range []string{"https://mangabaka.org/novel/84039/Title", "84039"} {
		path, err := trackingSearchPath(q)
		if err != nil || path != "/v1/series/84039" {
			t.Fatal(path, err)
		}
	}
	for _, q := range []string{"https://evil.example/84039", "https://mangabaka.org.evil/84039", "https://user:secret@mangabaka.org/84039"} {
		if _, err := trackingSearchPath(q); err == nil {
			t.Fatal("unsafe provider URL accepted")
		}
	}
}

func TestSeriesTrackingNeverReplacesBookOverrides(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	ids := []string{strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)}
	for _, id := range ids {
		syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: id, Kind: "book", RecordID: "default", Value: json.RawMessage(`{"title":"Volume","series":"Novels"}`)})
	}
	request(t, h, "PUT", "/v1/tracking/books/"+ids[0], token, trackingLink{SeriesID: 1, Title: "Separate novel"}, 204)
	request(t, h, "PUT", "/v1/tracking/books/"+ids[1], token, trackingLink{SeriesID: 2, Title: "Whole series", SeriesKey: "Novels", Volume: 2}, 204)
	links, _ := s.trackingLinks(user)
	if len(links) != 2 {
		t.Fatal("third book was automatically linked")
	}
	for _, l := range links {
		if l.BookID == ids[0] && (l.SeriesKey != "" || l.SeriesID != 1) {
			t.Fatal("book override replaced")
		}
	}
	request(t, h, "PUT", "/v1/tracking/books/"+ids[2], token, trackingLink{SeriesID: 3, Title: "Wrong series", SeriesKey: "Novels"}, 409)
	request(t, h, "PUT", "/v1/tracking/books/"+ids[2], token, trackingLink{SeriesID: 3, Title: "Independent entry"}, 204)
}

func TestTrackingWorkerRetriesAndPreservesExternalFields(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	id := strings.Repeat("c", 64)
	syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: id, Kind: "book", RecordID: "default", Value: json.RawMessage(`{"title":"Book"}`)}, Operation{ID: randomID(), BookID: id, Kind: "position", RecordID: "default", Value: json.RawMessage(`{"cfi":"epubcfi(/6/2)","fraction":1}`)})
	request(t, h, "PUT", "/v1/tracking/books/"+id, token, trackingLink{SeriesID: 12, Title: "Novel", Volume: 2, Auto: true}, 204)
	s.trackingMu.Lock()
	err := s.saveTrackingToken(user, "provider-user", "Reader", "mb-test-secret")
	s.trackingMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var encrypted []byte
	s.db.QueryRow("SELECT secret FROM tracking_accounts WHERE user_id=?", user).Scan(&encrypted)
	if strings.Contains(string(encrypted), "mb-test-secret") {
		t.Fatal("plaintext credential stored")
	}
	calls := 0
	fail := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("x-api-key") != "mb-test-secret" {
			t.Error("missing provider credential")
		}
		if fail {
			w.WriteHeader(503)
			return
		}
		if r.Method == "GET" {
			w.Write([]byte(`{"data":{"state":"reading","progress_volume":1,"note":"private note","rating":75}}`))
			return
		}
		var patch map[string]any
		json.NewDecoder(r.Body).Decode(&patch)
		if len(patch) != 1 || patch["progress_volume"] != float64(2) {
			t.Errorf("unexpected update %v", patch)
		}
		w.Write([]byte(`{"data":true}`))
	}))
	defer upstream.Close()
	p := newTrackingProvider()
	p.base = upstream.URL
	if err = s.runTracking(context.Background(), user, p); err != nil {
		t.Fatal(err)
	}
	links, _ := s.trackingLinks(user)
	if links[0].Error == "" || links[0].LastStep != 0 {
		t.Fatal("failed update lost", links)
	}
	fail = false
	s.db.Exec("UPDATE tracking_links SET next_attempt=0 WHERE user_id=?", user)
	if err = s.runTracking(context.Background(), user, p); err != nil {
		t.Fatal(err)
	}
	links, _ = s.trackingLinks(user)
	if links[0].Error != "" || links[0].LastStep != 2 || links[0].LastSync == 0 {
		t.Fatal("update not acknowledged", links)
	}
	before := calls
	s.runTracking(context.Background(), user, p)
	if calls != before {
		t.Fatal("unchanged progress sent again")
	}
	result := request(t, h, "GET", "/v1/tracking", token, nil, 200)
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "mb-test-secret") {
		t.Fatal("credential returned")
	}
	request(t, h, "DELETE", "/v1/tracking/account", token, nil, 204)
	links, _ = s.trackingLinks(user)
	if links[0].Auto {
		t.Fatal("disconnect did not stop tracking")
	}
}

func TestTrackingLinksArePersonalAndSeriesOptIn(t *testing.T) {
	s, h := fixture(t)
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")
	id := strings.Repeat("a", 64)
	for _, token := range []string{alice, bob} {
		syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: id, Kind: "book", RecordID: "default", Value: json.RawMessage(`{"title":"Book","series":"Collection"}`)})
	}
	input := trackingLink{BookID: id, SeriesID: 42, Title: "Individual novel", Volume: 1, CompleteEntry: true}
	request(t, h, "PUT", "/v1/tracking/books/"+id, alice, input, 204)
	a, _ := s.authenticate(alice)
	b, _ := s.authenticate(bob)
	links, err := s.trackingLinks(a)
	if err != nil || len(links) != 1 || links[0].SeriesKey != "" {
		t.Fatal(links, err)
	}
	other, err := s.trackingLinks(b)
	if err != nil || len(other) != 0 {
		t.Fatal("link leaked", other, err)
	}
	input.SeriesKey = "Collection"
	input.CompleteEntry = false
	request(t, h, "PUT", "/v1/tracking/books/"+id, bob, input, 204)
	links, _ = s.trackingLinks(a)
	if links[0].SeriesKey != "" {
		t.Fatal("other account altered book scope")
	}
	request(t, h, "DELETE", "/v1/tracking/books/"+id, bob, nil, 204)
	links, _ = s.trackingLinks(a)
	if len(links) != 1 {
		t.Fatal("unlink crossed accounts")
	}
	request(t, h, "PUT", "/v1/tracking/books/"+strings.Repeat("b", 64), alice, input, 404)
	request(t, h, "GET", "/v1/tracking", "", nil, 401)
}

func TestTrackingProgressDoesNotGuessChaptersOrLowerRemoteProgress(t *testing.T) {
	link := trackingLink{Volume: 3, CompleteEntry: false}
	state := trackingRemote{State: "reading", Volume: 5}
	patch := trackingPatch(link, 2, state)
	if len(patch) != 0 {
		t.Fatal("must not lower external progress", patch)
	}
	state.Volume = 1
	patch = trackingPatch(link, 2, state)
	if patch["progress_volume"] != float64(3) || patch["state"] != nil || patch["progress_chapter"] != nil {
		t.Fatal(patch)
	}
	link.CompleteEntry = true
	patch = trackingPatch(link, 2, state)
	if patch["state"] != "completed" {
		t.Fatal(patch)
	}
	state.State = "paused"
	patch = trackingPatch(link, 1, state)
	if len(patch) != 0 {
		t.Fatal("auto tracking resumed paused entry")
	}
}

func TestTrackWholeSeriesPreservesBookOverrides(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	for i, id := range ids {
		value, _ := json.Marshal(map[string]any{"title": "Volume", "series": "Novels", "volume": i + 1})
		syncRequest(t, h, token, 0, Operation{ID: randomID(), BookID: id, Kind: "book", RecordID: "default", Value: value})
	}
	request(t, h, "PUT", "/v1/tracking/books/"+ids[0], token, trackingLink{SeriesID: 10, Title: "Separate novel"}, 204)
	request(t, h, "PUT", "/v1/tracking/series", token, map[string]any{"seriesKey": "Novels", "seriesId": 20, "title": "Whole series", "auto": false}, 200)
	links, _ := s.trackingLinks(user)
	if len(links) != 3 {
		t.Fatal("missing volumes", links)
	}
	for _, l := range links {
		if l.BookID == ids[0] {
			if l.SeriesID != 10 || l.SeriesKey != "" {
				t.Fatal("override replaced")
			}
		} else if l.SeriesID != 20 || l.Volume < 2 || l.CompleteEntry {
			t.Fatal("incorrect series mapping", l)
		}
	}
	request(t, h, "PUT", "/v1/tracking/series", token, map[string]any{"seriesKey": "Novels", "seriesId": 30, "title": "Changed match", "auto": false}, 200)
	links, _ = s.trackingLinks(user)
	for _, l := range links {
		if l.BookID != ids[0] && l.SeriesID != 30 {
			t.Fatal("series change incomplete")
		}
	}
	request(t, h, "DELETE", "/v1/tracking/series", token, map[string]any{"seriesKey": "Novels"}, 204)
	links, _ = s.trackingLinks(user)
	if len(links) != 1 || links[0].SeriesID != 10 {
		t.Fatal("unlink removed individual override")
	}
	request(t, h, "PUT", "/v1/tracking/series", token, map[string]any{"seriesKey": "Missing", "seriesId": 20, "title": "Match"}, 404)
}
