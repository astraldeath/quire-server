package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type updateTransport func(*http.Request) (*http.Response, error)

func (f updateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdatesCacheFailureAndOrdering(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	calls, status := 0, 200
	body := `{"version":"main.new","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","publishedAt":"2026-09-17T11:00:00Z","notes":"Update awareness.","notesUrl":"https://github.com/astraldeath/quire-server/blob/main/CHANGELOG.md"}`
	c := newUpdateChecker()
	c.now = func() time.Time { return now }
	c.current = buildIdentity{Version: "main.old", Revision: strings.Repeat("a", 40), ReaderRevision: "reader"}
	c.builtAt = "2026-09-16T11:00:00Z"
	c.client = &http.Client{Transport: updateTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != updateFeedURL {
			t.Fatalf("unexpected URL %s", r.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	first := c.check()
	if !first.Available || first.Error != "" || first.Latest == nil {
		t.Fatalf("first: %+v", first)
	}
	c.check()
	if calls != 1 {
		t.Fatal("cache missed")
	}
	now = now.Add(7 * time.Hour)
	status = 503
	stale := c.check()
	if stale.Error == "" || stale.Latest == nil || stale.CheckedAt != first.CheckedAt || !stale.Available {
		t.Fatalf("lost last success: %+v", stale)
	}
	c.check()
	if calls != 2 {
		t.Fatal("failed checks not cached")
	}
	now = now.Add(7 * time.Hour)
	status = 200
	body = strings.ReplaceAll(body, "2026-09-17T11:00:00Z", "2026-09-15T11:00:00Z")
	if c.check().Available {
		t.Fatal("offered older update")
	}
	c.current.Revision = "unknown"
	now = now.Add(7 * time.Hour)
	if got := c.check(); got.Available || got.Error == "" {
		t.Fatalf("unknown identity reported current: %+v", got)
	}
}

func TestUpdatesRejectInvalidFeed(t *testing.T) {
	for _, body := range []string{`{}`, strings.Repeat("x", maxUpdateBytes+1), `{"version":"x","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","publishedAt":"2026-09-17T11:00:00Z","notesUrl":"https://evil.example/"}`} {
		c := newUpdateChecker()
		c.client = &http.Client{Transport: updateTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if got := c.check(); got.Error == "" || got.Latest != nil || got.Available {
			t.Fatalf("accepted invalid feed: %+v", got)
		}
	}
}

func TestUpdatesOnlyOfferStrictlyNewerDifferentRevision(t *testing.T) {
	for _, tc := range []struct {
		name, revision, published string
		want                      bool
	}{
		{"newer", strings.Repeat("b", 40), "2026-09-17T11:00:00Z", true},
		{"same revision", strings.Repeat("a", 40), "2026-09-17T11:00:00Z", false},
		{"equal time", strings.Repeat("b", 40), "2026-09-16T11:00:00Z", false},
		{"older", strings.Repeat("b", 40), "2026-09-15T11:00:00Z", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newUpdateChecker()
			c.current.Revision = strings.Repeat("a", 40)
			c.builtAt = "2026-09-16T11:00:00Z"
			c.nextCheck = time.Now().Add(time.Hour)
			c.latest = &updateRelease{Revision: tc.revision, PublishedAt: tc.published}
			if got := c.check(); got.Available != tc.want || got.Error != "" {
				t.Fatalf("unexpected result: %+v", got)
			}
		})
	}
}

func TestUpdatesRedirectAndTimeoutPolicy(t *testing.T) {
	c := newUpdateChecker()
	if c.client.Timeout <= 0 || c.client.Timeout > 10*time.Second {
		t.Fatal("missing bounded timeout")
	}
	for _, target := range []string{"http://github.com/file", "https://example.org/file", "https://github.com:444/file", "https://user@github.com/file"} {
		req, _ := http.NewRequest("GET", target, nil)
		if c.client.CheckRedirect(req, nil) == nil {
			t.Fatalf("accepted redirect to %s", target)
		}
	}
	req, _ := http.NewRequest("GET", "https://release-assets.githubusercontent.com/file", nil)
	if c.client.CheckRedirect(req, nil) != nil {
		t.Fatal("blocked GitHub asset CDN")
	}
}

func TestUpdatesAuthorizationAndAdminVisibility(t *testing.T) {
	s, h := fixture(t)
	request(t, h, "GET", "/v1/updates", "", nil, 401)
	c := newUpdateChecker()
	c.nextCheck = time.Now().Add(time.Hour)
	a := &api{store: s, updates: c}
	mux := http.NewServeMux()
	a.updateRoutes(mux)
	token := login(t, h, "alice")
	for _, admin := range []bool{false, true} {
		if _, err := s.db.Exec("UPDATE users SET admin=? WHERE username='alice'", admin); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/v1/updates", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var got updateStatus
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.CanManage != admin {
			t.Fatalf("admin=%v: %s", admin, w.Body)
		}
	}
}
