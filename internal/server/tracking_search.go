package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type trackingMatch struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Author      string   `json:"author"`
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	Description string   `json:"description"`
	Cover       string   `json:"cover"`
	Sources     []string `json:"sources"`
}

var trackingID = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

func trackingSearchPath(q string) (string, error) {
	q = strings.TrimSpace(q)
	if len(q) == 0 || len(q) > 300 {
		return "", ErrInvalid
	}
	if strings.Contains(q, "://") {
		u, e := url.Parse(q)
		if e != nil || u.Scheme != "https" || u.Host != "mangabaka.org" || u.User != nil || u.RawQuery != "" {
			return "", ErrInvalid
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) > 1 && (parts[0] == "manga" || parts[0] == "novel" || parts[0] == "manhwa" || parts[0] == "manhua" || parts[0] == "series") {
			parts = parts[1:]
		}
		if !trackingID.MatchString(parts[0]) {
			return "", ErrInvalid
		}
		q = parts[0]
	}
	if trackingID.MatchString(q) {
		return "/v1/series/" + q, nil
	}
	return "/v1/series/search?q=" + url.QueryEscape(q), nil
}
func (a *api) trackingSearchRoute(mux *http.ServeMux, p *trackingProvider) {
	var mu sync.Mutex
	next := time.Time{}
	type cached struct {
		values []trackingMatch
		until  time.Time
	}
	cache := map[string]cached{}
	mux.HandleFunc("GET /v1/tracking/search", func(w http.ResponseWriter, r *http.Request) {
		if a.authorized(w, r) == "" {
			return
		}
		path, err := trackingSearchPath(r.URL.Query().Get("q"))
		if err != nil {
			failure(w, err)
			return
		}
		mu.Lock()
		if item, ok := cache[path]; ok && time.Now().Before(item.until) {
			mu.Unlock()
			respond(w, 200, map[string]any{"matches": item.values})
			return
		}
		if time.Now().Before(next) {
			mu.Unlock()
			respond(w, 429, map[string]string{"error": "Wait a moment before searching again."})
			return
		}
		next = time.Now().Add(2100 * time.Millisecond)
		mu.Unlock()
		data, _, err := p.call(r.Context(), "GET", path, "", nil)
		if err != nil {
			respond(w, 502, map[string]string{"error": err.Error()})
			return
		}
		type entry struct {
			ID                               int64
			Title, Type, Status, Description string
			Authors                          []string
			Cover                            struct{ X150 struct{ X1 string } }
			Source                           map[string]json.RawMessage
		}
		var entries []entry
		if strings.Contains(path, "/search?") {
			err = json.Unmarshal(data, &entries)
		} else {
			var e entry
			err = json.Unmarshal(data, &e)
			entries = []entry{e}
		}
		if err != nil {
			respond(w, 502, map[string]string{"error": "Invalid MangaBaka response."})
			return
		}
		matches := []trackingMatch{}
		for _, e := range entries {
			if e.ID <= 0 || e.Title == "" {
				continue
			}
			cover := ""
			if u, err := url.Parse(e.Cover.X150.X1); err == nil && u.Scheme == "https" && u.Host == "cdn.mangabaka.dev" && u.User == nil {
				cover = u.String()
			}
			sources := []string{}
			for name, v := range e.Source {
				if string(v) != "null" {
					sources = append(sources, strings.ReplaceAll(name, "_", " "))
				}
			}
			sort.Strings(sources)
			matches = append(matches, trackingMatch{e.ID, e.Title, strings.Join(e.Authors, ", "), e.Type, e.Status, e.Description, cover, sources})
			if len(matches) == 10 {
				break
			}
		}
		mu.Lock()
		if len(cache) >= 128 {
			cache = map[string]cached{}
		}
		cache[path] = cached{matches, time.Now().Add(time.Hour)}
		mu.Unlock()
		respond(w, 200, map[string]any{"matches": matches})
	})
}
