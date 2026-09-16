package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func activity(n int) ReadingActivity {
	return ReadingActivity{ID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), BookID: testBook, StartedAt: 1000, EndedAt: 2000, ActiveMs: 1000, Words: 5, SampledMs: 1000, Chapters: []int{2, 1}}
}

func TestStatisticsSyncContract(t *testing.T) {
	s, h := fixture(t)
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")
	user, _ := s.authenticate(alice)
	request(t, h, "POST", "/v1/statistics/sync", "", StatisticsSyncRequest{}, 401)
	a := activity(1)
	request(t, h, "POST", "/v1/statistics/sync", alice, StatisticsSyncRequest{Activities: []ReadingActivity{a}}, 200)
	a.Chapters = []int{1, 2}
	out, err := s.SyncStatistics(context.Background(), user, StatisticsSyncRequest{Activities: []ReadingActivity{a}})
	if err != nil || out.Cursor != 1 || len(out.Activities) != 1 {
		t.Fatalf("retry: %+v %v", out, err)
	}
	private := request(t, h, "POST", "/v1/statistics/sync", bob, StatisticsSyncRequest{}, 200)
	if string(private["activities"]) != "[]" {
		t.Fatal("history leaked")
	}
	a.Words++
	request(t, h, "POST", "/v1/statistics/sync", alice, StatisticsSyncRequest{Activities: []ReadingActivity{activity(2), a}}, 409)
	out, err = s.SyncStatistics(context.Background(), user, StatisticsSyncRequest{})
	if err != nil || out.Cursor != 1 {
		t.Fatal("conflict partially committed")
	}
	for i := 2; i <= 105; i++ {
		_, err = s.SyncStatistics(context.Background(), user, StatisticsSyncRequest{Activities: []ReadingActivity{activity(i)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	out, _ = s.SyncStatistics(context.Background(), user, StatisticsSyncRequest{})
	if out.Cursor != 100 || !out.HasMore || len(out.Activities) != 100 {
		t.Fatalf("first page %+v", out)
	}
	out, _ = s.SyncStatistics(context.Background(), user, StatisticsSyncRequest{Cursor: 100})
	if out.Cursor != 105 || out.HasMore || len(out.Activities) != 5 {
		t.Fatalf("last page %+v", out)
	}
	request(t, h, "POST", "/v1/statistics/sync", alice, StatisticsSyncRequest{Cursor: 106}, 409)
	// A book tombstone must not erase its immutable history.
	_, err = s.Sync(context.Background(), user, SyncRequest{Operations: []Operation{{ID: "delete-book", BookID: testBook, Kind: "book", RecordID: "default", Deleted: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO files(user_id,book_id,kind,source,size) VALUES (?,?,'upload','',0)", user, testBook); err != nil {
		t.Fatal(err)
	}
	if err = s.deleteUpload(user, testBook); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "history.zip")
	if err = s.Backup(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restore")
	if err = RestoreBackup(archive, restored); err != nil {
		t.Fatal(err)
	}
	other, err := Open(filepath.Join(restored, "quire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	out, err = other.SyncStatistics(context.Background(), user, StatisticsSyncRequest{Cursor: 100})
	if err != nil || out.Cursor != 105 || len(out.Activities) != 5 {
		t.Fatalf("history lost: %+v %v", out, err)
	}
}

func TestStatisticsBoundaryValidation(t *testing.T) {
	tests := map[string]func(*ReadingActivity){
		"duration":            func(a *ReadingActivity) { a.EndedAt = a.StartedAt + 300001 },
		"chapter zero":        func(a *ReadingActivity) { a.Chapters = []int{0} },
		"chapter huge":        func(a *ReadingActivity) { a.Chapters = []int{100001} },
		"chapter count":       func(a *ReadingActivity) { a.Chapters = make([]int, 1001) },
		"negative words":      func(a *ReadingActivity) { a.Words = -1 },
		"sample without time": func(a *ReadingActivity) { a.SampledMs = 0 },
		"volume huge":         func(a *ReadingActivity) { v := 100001.0; a.Volume = &v },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			a := activity(1)
			mutate(&a)
			if _, err := canonicalActivity(a); err == nil {
				t.Fatal("accepted invalid activity")
			}
		})
	}
	a := activity(1)
	a.StartedAt = 8640000000000000
	a.EndedAt = a.StartedAt
	a.ActiveMs = 0
	a.Words = 0
	a.SampledMs = 0
	a.Baseline = true
	n := 100000
	a.ChapterThrough = &n
	if _, err := canonicalActivity(a); err != nil {
		t.Fatal("valid baseline rejected", err)
	}
}

func TestStatisticsRejectMalformed(t *testing.T) {
	_, h := fixture(t)
	token := login(t, h, "alice")
	b, _ := json.Marshal(activity(1))
	var base map[string]any
	json.Unmarshal(b, &base)
	bad := map[string]any{"id": "not-uuid", "bookId": "x", "startedAt": -1, "endedAt": 8640000000000001, "activeMs": 1001, "words": 100001, "sampledMs": 1001, "chapters": []int{1, 1}, "volume": 0, "baseline": true, "chapterThrough": 1, "finished": nil}
	for field, value := range bad {
		t.Run(field, func(t *testing.T) {
			candidate := map[string]any{}
			for k, v := range base {
				candidate[k] = v
			}
			candidate[field] = value
			request(t, h, "POST", "/v1/statistics/sync", token, map[string]any{"cursor": 0, "activities": []any{candidate}}, 400)
		})
	}
	delete(base, "words")
	request(t, h, "POST", "/v1/statistics/sync", token, map[string]any{"cursor": 0, "activities": []any{base}}, 400)
	request(t, h, "POST", "/v1/statistics/sync", token, StatisticsSyncRequest{Activities: make([]ReadingActivity, 101)}, 400)
}

func TestStatisticsConcurrentRetriesAndRevocation(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := s.SyncStatistics(context.Background(), user, StatisticsSyncRequest{Activities: []ReadingActivity{activity(1)}})
			failures <- err
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.SyncStatistics(context.Background(), user, StatisticsSyncRequest{})
	if err != nil || out.Cursor != 1 || len(out.Activities) != 1 {
		t.Fatalf("retry duplicated history: %+v %v", out, err)
	}
	if _, err = s.db.Exec("DELETE FROM sessions WHERE user_id=?", user); err != nil {
		t.Fatal(err)
	}
	request(t, h, "POST", "/v1/statistics/sync", token, StatisticsSyncRequest{}, 401)
}
