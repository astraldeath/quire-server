// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// These values are embedded by the image build; plain go builds remain explicitly unknown.
var BuildVersion = "dev"
var BuildRevision = "unknown"
var BuildTime = "unknown"
var ReaderRevision = "unknown"

const updateFeedURL = "https://github.com/astraldeath/quire-server/releases/download/server-latest/update.json"
const maxUpdateBytes = 64 << 10

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type buildIdentity struct {
	Version        string `json:"version"`
	Revision       string `json:"revision"`
	ReaderRevision string `json:"readerRevision"`
}
type updateRelease struct {
	Version     string `json:"version"`
	Revision    string `json:"revision"`
	PublishedAt string `json:"publishedAt"`
	Notes       string `json:"notes"`
	NotesURL    string `json:"notesUrl"`
}
type updateStatus struct {
	Current   buildIdentity  `json:"current"`
	Latest    *updateRelease `json:"latest"`
	Available bool           `json:"available"`
	CheckedAt string         `json:"checkedAt"`
	CanManage bool           `json:"canManage"`
	Error     string         `json:"error,omitempty"`
}
type updateChecker struct {
	mu        sync.Mutex
	client    *http.Client
	now       func() time.Time
	current   buildIdentity
	builtAt   string
	nextCheck time.Time
	latest    *updateRelease
	checkedAt string
	lastError string
}

func newUpdateChecker() *updateChecker {
	return &updateChecker{
		client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// GitHub release assets redirect to its public asset CDN. Never follow another host.
			if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.User != nil || (req.URL.Host != "github.com" && req.URL.Host != "release-assets.githubusercontent.com" && req.URL.Host != "objects.githubusercontent.com") {
				return errors.New("untrusted release redirect")
			}
			return nil
		}},
		now: time.Now, current: buildIdentity{BuildVersion, BuildRevision, ReaderRevision}, builtAt: BuildTime,
	}
}
func (c *updateChecker) fetch() (*updateRelease, error) {
	req, err := http.NewRequest(http.MethodGet, updateFeedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Quire-Server-Update-Check")
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("release feed unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxUpdateBytes+1))
	if err != nil || len(body) > maxUpdateBytes {
		return nil, errors.New("invalid release feed size")
	}
	var release updateRelease
	if json.Unmarshal(body, &release) != nil {
		return nil, errors.New("invalid release feed")
	}
	published, err := time.Parse(time.RFC3339, release.PublishedAt)
	notesURL, urlErr := url.Parse(release.NotesURL)
	if err != nil || published.After(c.now().Add(5*time.Minute)) || !revisionPattern.MatchString(release.Revision) || strings.TrimSpace(release.Version) == "" || len(release.Version) > 100 || len(release.Notes) > 32000 || urlErr != nil || notesURL.Scheme != "https" || notesURL.Host != "github.com" || notesURL.User != nil || !strings.HasPrefix(notesURL.Path, "/astraldeath/quire-server/") {
		return nil, errors.New("invalid release metadata")
	}
	return &release, nil
}
func (c *updateChecker) check() updateStatus {
	// Serialize refreshes so concurrent readers cause at most one bounded outbound request.
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if !now.Before(c.nextCheck) {
		c.nextCheck = now.Add(6 * time.Hour)
		release, err := c.fetch()
		if err != nil {
			c.lastError = "Update check unavailable. Last successful information, if any, is shown."
		} else {
			c.latest = release
			c.checkedAt = now.UTC().Format(time.RFC3339)
			c.lastError = ""
		}
	}
	out := updateStatus{Current: c.current, Latest: c.latest, CheckedAt: c.checkedAt, Error: c.lastError}
	built, err := time.Parse(time.RFC3339, c.builtAt)
	if err != nil || !revisionPattern.MatchString(c.current.Revision) {
		if out.Error != "" {
			out.Error += " "
		}
		out.Error += "This build has no published identity; update availability cannot be determined."
		return out
	}
	if c.latest != nil {
		published, _ := time.Parse(time.RFC3339, c.latest.PublishedAt)
		out.Available = c.latest.Revision != c.current.Revision && published.After(built)
	}
	return out
}
func (a *api) updateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/updates", func(w http.ResponseWriter, r *http.Request) {
		id := a.authorized(w, r)
		if id == "" {
			return
		}
		user, err := a.store.userInfo(id)
		if err != nil {
			failure(w, err)
			return
		}
		out := a.updates.check()
		out.CanManage = user.Admin
		respond(w, http.StatusOK, out)
	})
}
