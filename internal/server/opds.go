package server

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type opdsBook struct {
	ID, Owner string
	Metadata  bookMetadata
	Added     int64
}

func privacyExcludes(raw, id string) bool {
	if raw == "" {
		return false
	}
	var state PrivacySettings
	if json.Unmarshal([]byte(raw), &state) != nil {
		return true
	}
	return state.Books[id] != ""
}

// Recompute visibility for every feed and direct resource request. Both the
// library owner and reader's privacy state constrain shared-library exposure.
func (s *Store) opdsBooks(user string) ([]opdsBook, error) {
	rows, e := s.db.Query(`SELECT DISTINCT f.book_id,f.user_id,coalesce(r.candidates,''),coalesce(own.candidates,''),coalesce(p.state,''),coalesce(op.state,'') FROM files f
 LEFT JOIN records r ON r.user_id=? AND r.book_id=f.book_id AND r.kind='book' AND r.record_id='default'
 LEFT JOIN records own ON own.user_id=f.user_id AND own.book_id=f.book_id AND own.kind='book' AND own.record_id='default'
 LEFT JOIN privacy_settings p ON p.user_id=? LEFT JOIN privacy_settings op ON op.user_id=f.user_id
 WHERE f.user_id=? OR f.user_id IN(SELECT l.owner FROM libraries l JOIN library_members m ON m.library_id=l.id WHERE m.user_id=?) ORDER BY f.user_id=? DESC,f.book_id`, user, user, user, user, user)
	if e != nil {
		return nil, e
	}
	out := []opdsBook{}
	seen := map[string]bool{}
	for rows.Next() {
		var b opdsBook
		var raw, ownerRaw, privacy, ownerPrivacy string
		if e = rows.Scan(&b.ID, &b.Owner, &raw, &ownerRaw, &privacy, &ownerPrivacy); e != nil {
			rows.Close()
			return nil, e
		}
		if seen[b.ID] || privacyExcludes(privacy, b.ID) || privacyExcludes(ownerPrivacy, b.ID) {
			continue
		}
		if raw == "" {
			raw = ownerRaw
		}
		var candidates []Candidate
		if raw != "" && (json.Unmarshal([]byte(raw), &candidates) != nil || len(candidates) != 1 || candidates[0].Deleted) {
			continue
		}
		if raw != "" && json.Unmarshal(candidates[0].Value, &b.Metadata) != nil {
			continue
		}
		if raw != "" {
			b.Added = candidates[0].CreatedAt
		}
		seen[b.ID] = true
		out = append(out, b)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	available := out[:0]
	for _, b := range out {
		info, e := os.Stat(s.objectPath(b.Owner, b.ID))
		if e == nil && info.Mode().IsRegular() {
			if b.Added == 0 {
				b.Metadata = s.objectMetadata(b.Owner, b.ID)
			}
			// Metadata edits replace sync candidates; they must not make an old
			// book appear newly added. Stored objects retain their arrival time.
			b.Added = info.ModTime().UnixMilli()
			if b.Metadata.Title == "" {
				b.Metadata.Title = "Untitled"
			}
			b.Metadata.Format = strings.TrimPrefix(filepath.Ext(s.objectPath(b.Owner, b.ID)), ".")
			available = append(available, b)
		}
	}
	return available, nil
}

type opdsLink struct {
	Href  string `json:"href" xml:"href,attr"`
	Type  string `json:"type,omitempty" xml:"type,attr,omitempty"`
	Rel   string `json:"rel,omitempty" xml:"rel,attr,omitempty"`
	Title string `json:"title,omitempty" xml:"title,attr,omitempty"`
}
type opdsAuthor struct {
	Name string `json:"name" xml:"name"`
}
type opdsMetadata struct {
	Identifier    string       `json:"identifier,omitempty"`
	Title         string       `json:"title"`
	Author        []opdsAuthor `json:"author,omitempty"`
	NumberOfItems int          `json:"numberOfItems"`
	ItemsPerPage  int          `json:"itemsPerPage,omitempty"`
	CurrentPage   int          `json:"currentPage,omitempty"`
}
type opdsPublication struct {
	Metadata opdsMetadata `json:"metadata"`
	Links    []opdsLink   `json:"links"`
	Images   []opdsLink   `json:"images,omitempty"`
}
type opdsJSONFeed struct {
	Metadata     opdsMetadata      `json:"metadata"`
	Links        []opdsLink        `json:"links"`
	Navigation   []opdsLink        `json:"navigation,omitempty"`
	Publications []opdsPublication `json:"publications,omitempty"`
}
type atomEntry struct {
	ID      string       `xml:"id"`
	Title   string       `xml:"title"`
	Updated string       `xml:"updated"`
	Authors []opdsAuthor `xml:"author,omitempty"`
	Links   []opdsLink   `xml:"link"`
}
type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Links   []opdsLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

func (a *api) opdsRoutes(mux *http.ServeMux) {
	a.opdsPasswordRoutes(mux)
	mux.HandleFunc("GET /opds", a.serveOPDS)
	mux.HandleFunc("GET /opds/{$}", a.serveOPDS)
	mux.HandleFunc("GET /opds/v2", a.serveOPDS)
	mux.HandleFunc("GET /opds/search.xml", func(w http.ResponseWriter, r *http.Request) {
		if a.opdsAuthorized(w, r) == "" {
			return
		}
		var out struct {
			XMLName     xml.Name `xml:"http://a9.com/-/spec/opensearch/1.1/ OpenSearchDescription"`
			ShortName   string   `xml:"ShortName"`
			Description string   `xml:"Description"`
			URL         struct {
				Type     string `xml:"type,attr"`
				Template string `xml:"template,attr"`
			} `xml:"Url"`
		}
		out.ShortName = "Quire"
		out.Description = "Search your Quire library"
		out.URL.Type = "application/atom+xml;profile=opds-catalog;kind=acquisition"
		out.URL.Template = a.publicOrigin + "/opds?view=all&q={searchTerms}"
		w.Header().Set("Content-Type", "application/opensearchdescription+xml")
		w.Write([]byte(xml.Header))
		xml.NewEncoder(w).Encode(out)
	})
	for _, resource := range []string{"file", "cover"} {
		resource := resource
		mux.HandleFunc("GET /opds/books/{id}/"+resource, func(w http.ResponseWriter, r *http.Request) {
			user := a.opdsAuthorized(w, r)
			if user == "" {
				return
			}
			books, e := a.store.opdsBooks(user)
			if e != nil {
				failure(w, e)
				return
			}
			var book *opdsBook
			for _, b := range books {
				if b.ID == r.PathValue("id") {
					book = &b
					break
				}
			}
			if book == nil {
				failure(w, sql.ErrNoRows)
				return
			}
			if resource == "cover" {
				cover := a.store.objectMetadata(book.Owner, book.ID).Cover
				encoded, ok := strings.CutPrefix(cover, "data:image/jpeg;base64,")
				if !ok {
					failure(w, sql.ErrNoRows)
					return
				}
				data, e := base64.StdEncoding.DecodeString(encoded)
				if e != nil {
					failure(w, sql.ErrNoRows)
					return
				}
				w.Header().Set("Content-Type", "image/jpeg")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.Write(data)
				return
			}
			f, e := os.Open(a.store.objectPath(book.Owner, book.ID))
			if e != nil {
				failure(w, sql.ErrNoRows)
				return
			}
			defer f.Close()
			info, e := f.Stat()
			if e != nil {
				failure(w, e)
				return
			}
			format := book.Metadata.Format
			name := "book." + format
			w.Header().Set("Content-Type", bookMIME(format))
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
			http.ServeContent(w, r, name, info.ModTime(), f)
		})
	}
}
func (a *api) serveOPDS(w http.ResponseWriter, r *http.Request) {
	user := a.opdsAuthorized(w, r)
	if user == "" {
		return
	}
	books, e := a.store.opdsBooks(user)
	if e != nil {
		failure(w, e)
		return
	}
	v2 := r.URL.Path == "/opds/v2"
	path := "/opds"
	media := "application/atom+xml;profile=opds-catalog;kind=acquisition"
	if v2 {
		path = "/opds/v2"
		media = "application/opds+json"
	}
	link := func(title, view, key, value string) opdsLink {
		q := url.Values{"view": {view}}
		if key != "" {
			q.Set(key, value)
		}
		return opdsLink{Href: a.publicOrigin + path + "?" + q.Encode(), Title: title, Type: media}
	}
	query := r.URL.Query()
	view := query.Get("view")
	if view == "" && query.Get("q") != "" {
		view = "all"
	}
	out := opdsJSONFeed{Metadata: opdsMetadata{Title: "Quire library"}, Links: []opdsLink{{Href: a.publicOrigin + path + "?" + query.Encode(), Type: media, Rel: "self"}, {Href: a.publicOrigin + path, Type: media, Rel: "start"}}}
	if v2 {
		out.Links = append(out.Links, opdsLink{Href: a.publicOrigin + path + "?view=all&q={searchTerms}", Type: media, Rel: "search"})
	} else {
		out.Links = append(out.Links, opdsLink{Href: a.publicOrigin + "/opds/search.xml", Type: "application/opensearchdescription+xml", Rel: "search"})
	}
	switch view {
	case "":
		out.Navigation = []opdsLink{link("All books", "all", "", ""), link("Recently added", "recent", "", ""), link("Folders", "folders", "", ""), link("Series", "series", "", "")}
		books = nil
	case "all", "recent":
	case "folders":
		prefix := query.Get("folder")
		children := map[string]bool{}
		filtered := []opdsBook{}
		for _, b := range books {
			matches := false
			for _, folder := range metadataFolders(b.Metadata.Folder, b.Metadata.Folders) {
				if folder == prefix {
					matches = true
				}
				rest := folder
				if prefix != "" {
					if !strings.HasPrefix(folder, prefix+"/") {
						continue
					}
					rest = strings.TrimPrefix(folder, prefix+"/")
				}
				child := strings.SplitN(rest, "/", 2)[0]
				if child != "" {
					children[child] = true
				}
			}
			if matches {
				filtered = append(filtered, b)
			}
		}
		names := []string{}
		for child := range children {
			names = append(names, child)
		}
		sort.Strings(names)
		for _, name := range names {
			folder := name
			if prefix != "" {
				folder = prefix + "/" + name
			}
			out.Navigation = append(out.Navigation, link(name, "folders", "folder", folder))
		}
		books = filtered
		if prefix != "" {
			out.Metadata.Title = prefix
		}
	case "series":
		series := query.Get("series")
		filtered := []opdsBook{}
		names := map[string]bool{}
		for _, b := range books {
			if series != "" && b.Metadata.Series == series {
				filtered = append(filtered, b)
			}
			if b.Metadata.Series != "" {
				names[b.Metadata.Series] = true
			}
		}
		if series == "" {
			sorted := []string{}
			for name := range names {
				sorted = append(sorted, name)
			}
			sort.Strings(sorted)
			for _, name := range sorted {
				out.Navigation = append(out.Navigation, link(name, "series", "series", name))
			}
		} else {
			out.Metadata.Title = series
		}
		books = filtered
	default:
		failure(w, ErrInvalid)
		return
	}
	search := strings.ToLower(strings.TrimSpace(query.Get("q")))
	if len(search) > 512 {
		failure(w, ErrInvalid)
		return
	}
	if search != "" {
		filtered := books[:0]
		for _, b := range books {
			if strings.Contains(strings.ToLower(b.Metadata.Title+" "+b.Metadata.Author+" "+b.Metadata.Series), search) {
				filtered = append(filtered, b)
			}
		}
		books = filtered
	}
	sort.Slice(books, func(i, j int) bool {
		if view == "recent" && books[i].Added != books[j].Added {
			return books[i].Added > books[j].Added
		}
		if books[i].Metadata.Title == books[j].Metadata.Title {
			return books[i].ID < books[j].ID
		}
		return books[i].Metadata.Title < books[j].Metadata.Title
	})
	page := 1
	if raw := query.Get("page"); raw != "" {
		page, e = strconv.Atoi(raw)
		if e != nil || page < 1 || page > 1000000 {
			failure(w, ErrInvalid)
			return
		}
	}
	const size = 50
	out.Metadata.NumberOfItems = len(books)
	out.Metadata.ItemsPerPage = size
	out.Metadata.CurrentPage = page
	start := min((page-1)*size, len(books))
	end := min(start+size, len(books))
	for _, b := range books[start:end] {
		publication := opdsPublication{Metadata: opdsMetadata{Identifier: "urn:sha256:" + b.ID, Title: b.Metadata.Title, Author: []opdsAuthor{{Name: b.Metadata.Author}}}, Links: []opdsLink{{Href: a.publicOrigin + "/opds/books/" + b.ID + "/file", Type: bookMIME(b.Metadata.Format), Rel: "http://opds-spec.org/acquisition"}}}
		if a.store.objectMetadata(b.Owner, b.ID).Cover != "" {
			publication.Images = []opdsLink{{Href: a.publicOrigin + "/opds/books/" + b.ID + "/cover", Type: "image/jpeg", Rel: "http://opds-spec.org/image"}}
		}
		out.Publications = append(out.Publications, publication)
	}
	for _, p := range []struct {
		number int
		rel    string
	}{{page - 1, "previous"}, {page + 1, "next"}} {
		if (p.rel == "previous" && page > 1) || (p.rel == "next" && end < len(books)) {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(p.number))
			out.Links = append(out.Links, opdsLink{Href: a.publicOrigin + path + "?" + q.Encode(), Type: media, Rel: p.rel})
		}
	}
	if v2 {
		w.Header().Set("Content-Type", media)
		json.NewEncoder(w).Encode(out)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	atom := atomFeed{ID: out.Links[0].Href, Title: out.Metadata.Title, Updated: now, Links: out.Links}
	for _, nav := range out.Navigation {
		atom.Entries = append(atom.Entries, atomEntry{ID: nav.Href, Title: nav.Title, Updated: now, Links: []opdsLink{nav}})
	}
	for _, p := range out.Publications {
		atom.Entries = append(atom.Entries, atomEntry{ID: p.Metadata.Identifier, Title: p.Metadata.Title, Updated: now, Authors: p.Metadata.Author, Links: append(p.Links, p.Images...)})
	}
	if len(out.Navigation) > 0 {
		media = "application/atom+xml;profile=opds-catalog;kind=navigation"
	}
	w.Header().Set("Content-Type", media)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(atom)
}
