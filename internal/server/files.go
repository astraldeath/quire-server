// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"archive/zip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var errWatched = errors.New("watched files cannot be deleted")

type FileInfo struct {
	BookID   string `json:"bookId"`
	Size     int64  `json:"size"`
	Uploaded bool   `json:"uploaded"`
	Watched  bool   `json:"watched"`
}

func (s *Store) objectPath(user, id string) string {
	for _, format := range bookFormats {
		candidate := filepath.Join(s.data, "objects", user, id+"."+format)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return filepath.Join(s.data, "objects", user, id+".epub")
}
func validateEPUB(path string) error {
	z, err := zip.OpenReader(path)
	if err != nil {
		return ErrInvalid
	}
	defer z.Close()
	var total uint64
	mime, container := false, false
	seen := map[string]bool{}
	for _, f := range z.File {
		if seen[f.Name] || strings.Contains(f.Name, "\\") || strings.HasPrefix(f.Name, "/") || strings.Contains("/"+f.Name, "/../") {
			return ErrInvalid
		}
		seen[f.Name] = true
		if f.UncompressedSize64 > 512<<20-total || len(z.File) > 10000 {
			return ErrInvalid
		}
		total += f.UncompressedSize64
		if f.Name == "META-INF/container.xml" {
			container = true
		}
		if f.Name == "mimetype" {
			r, e := f.Open()
			if e != nil {
				return ErrInvalid
			}
			b, e := io.ReadAll(io.LimitReader(r, 64))
			r.Close()
			if e != nil || string(b) != "application/epub+zip" {
				return ErrInvalid
			}
			mime = true
		}
	}
	if !mime || !container {
		return ErrInvalid
	}
	return nil
}
func (s *Store) stage(user string, r io.Reader, expected string) (string, int64, error) {
	dir := filepath.Join(s.data, "objects", user)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, s.maxUploadBytes+1))
	if n > s.maxUploadBytes {
		err = ErrUploadTooLarge
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", 0, err
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	id := hex.EncodeToString(h.Sum(nil))
	if expected != "" && expected != id {
		return "", 0, ErrInvalid
	}
	format, err := detectBookFormat(f.Name())
	if err != nil {
		return "", 0, err
	}
	target := filepath.Join(dir, id+"."+format)
	if _, err = os.Stat(target); errors.Is(err, os.ErrNotExist) {
		if err = os.Rename(f.Name(), target); err != nil {
			return "", 0, err
		}
	} else if err != nil {
		return "", 0, err
	}
	return id, n, nil
}
func (s *Store) fileList(user string) ([]FileInfo, error) {
	rows, err := s.db.Query("SELECT book_id,max(size),max(kind='upload' AND user_id=?),max(kind='watch' OR user_id<>?) FROM files WHERE user_id=? OR user_id IN (SELECT l.owner FROM libraries l JOIN library_members m ON m.library_id=l.id WHERE m.user_id=?) GROUP BY book_id ORDER BY book_id", user, user, user, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileInfo{}
	for rows.Next() {
		var f FileInfo
		if err = rows.Scan(&f.BookID, &f.Size, &f.Uploaded, &f.Watched); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
func (s *Store) deleteUpload(user, id string) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	var uploads, watches int
	if err := s.db.QueryRow("SELECT count(CASE WHEN kind='upload' THEN 1 END),count(CASE WHEN kind='watch' THEN 1 END) FROM files WHERE user_id=? AND book_id=?", user, id).Scan(&uploads, &watches); err != nil {
		return err
	}
	if uploads == 0 {
		if watches > 0 {
			return errWatched
		}
		return sql.ErrNoRows
	}
	if _, err := s.db.Exec("DELETE FROM files WHERE user_id=? AND book_id=? AND kind='upload'", user, id); err != nil {
		return err
	}
	if watches == 0 {
		_ = os.Remove(s.objectPath(user, id))
	}
	return nil
}

// Both upload APIs use the same streaming, hash, format and persistence checks.
// The owner is resolved from authentication/library state, never the request body.
func (a *api) uploadFile(w http.ResponseWriter, r *http.Request, owner, expected string) {
	if expected == "" && strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/octet-stream" {
		respond(w, http.StatusUnsupportedMediaType, map[string]string{"error": "application/octet-stream required"})
		return
	}
	if r.ContentLength > a.store.maxUploadBytes {
		failure(w, ErrUploadTooLarge)
		return
	}
	a.store.fileMu.Lock()
	defer a.store.fileMu.Unlock()
	// stage bounds unknown-length bodies too. No database transaction is held
	// while receiving or validating the body.
	id, size, err := a.store.stage(owner, r.Body, expected)
	if err == nil {
		_, err = a.store.db.Exec("INSERT INTO files VALUES (?,?,'upload','',?) ON CONFLICT(user_id,book_id,kind,source) DO UPDATE SET size=excluded.size", owner, id, size)
	}
	if err != nil {
		failure(w, err)
		return
	}
	respond(w, http.StatusCreated, map[string]any{"bookId": id, "size": size})
}

func (a *api) fileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/books/files", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user != "" {
			a.uploadFile(w, r, user, "")
		}
	})
	mux.HandleFunc("GET /v1/books/{id}/metadata", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		id := r.PathValue("id")
		if !bookPattern.MatchString(id) {
			failure(w, ErrInvalid)
			return
		}
		owner, err := a.store.accessibleOwner(user, id)
		if err != nil {
			failure(w, err)
			return
		}
		if _, err := os.Stat(a.store.objectPath(owner, id)); err != nil {
			failure(w, sql.ErrNoRows)
			return
		}
		respond(w, 200, a.store.objectMetadata(owner, id))
	})
	mux.HandleFunc("GET /v1/files", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		files, err := a.store.fileList(user)
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 200, map[string]any{"files": files})
	})
	mux.HandleFunc("PUT /v1/books/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		id := r.PathValue("id")
		if !bookPattern.MatchString(id) {
			failure(w, ErrInvalid)
			return
		}
		a.uploadFile(w, r, user, id)
	})
	mux.HandleFunc("GET /v1/books/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		id := r.PathValue("id")
		if !bookPattern.MatchString(id) {
			failure(w, ErrInvalid)
			return
		}
		owner, err := a.store.accessibleOwner(user, id)
		if err != nil {
			failure(w, err)
			return
		}
		f, err := os.Open(a.store.objectPath(owner, id))
		if err != nil {
			failure(w, sql.ErrNoRows)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			failure(w, err)
			return
		}
		format := strings.TrimPrefix(filepath.Ext(f.Name()), ".")
		name := "book." + format
		w.Header().Set("Content-Type", bookMIME(format))
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		http.ServeContent(w, r, name, info.ModTime(), f)
	})
	mux.HandleFunc("DELETE /v1/books/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		user := a.authorized(w, r)
		if user == "" {
			return
		}
		id := r.PathValue("id")
		if !bookPattern.MatchString(id) {
			failure(w, ErrInvalid)
			return
		}
		err := a.store.deleteUpload(user, id)
		if errors.Is(err, errWatched) {
			respond(w, 403, map[string]string{"error": "watched files cannot be deleted"})
			return
		}
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, 204, nil)
	})
}
func (s *Store) AddWatch(username, root string) (string, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", ErrInvalid
	}
	identity, err := rootIdentity(canonical)
	if err != nil {
		return "", err
	}
	var user string
	if err = s.db.QueryRow("SELECT id FROM users WHERE username=?", username).Scan(&user); err != nil {
		return "", err
	}
	id := randomID()
	_, err = s.db.Exec("INSERT INTO watch_roots VALUES (?,?,?,?)", id, user, canonical, identity)
	return id, err
}
func inside(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
func (s *Store) ScanWatch(id string) (scanErr error) {
	imported, existing, skipped := 0, 0, 0
	defer func() {
		message := ""
		if scanErr != nil {
			message = scanErr.Error()
			imported = 0
			existing = 0
		}
		_, _ = s.db.Exec("INSERT INTO scan_status(watch_id,last_at,error,imported,existing,skipped) VALUES (?,?,?,?,?,?) ON CONFLICT(watch_id) DO UPDATE SET last_at=excluded.last_at,error=excluded.error,imported=excluded.imported,existing=excluded.existing,skipped=excluded.skipped", id, time.Now().Unix(), message, imported, existing, skipped)
	}()

	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	var user, root, identity string
	if err := s.db.QueryRow("SELECT user_id,root,identity FROM watch_roots WHERE id=?", id).Scan(&user, &root, &identity); err != nil {
		return err
	}
	current, err := rootIdentity(root)
	if err != nil {
		return err
	}
	if current != identity {
		return errors.New("watched root identity changed; scan cancelled")
	}
	type scanned struct {
		book, path, title string
		size              int64
	}
	items := []scanned{}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			skipped++
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !supportedBookFilename(path) {
			skipped++
			return nil
		}
		if len(items) >= 10000 {
			return errors.New("watched folder exceeds 10000 books")
		}
		resolved, e := filepath.EvalSymlinks(path)
		if e != nil {
			return e
		}
		if !inside(root, resolved) {
			return errors.New("watched file escapes its root")
		}
		before, e := os.Stat(path)
		if e != nil {
			return e
		}
		if !before.Mode().IsRegular() {
			return ErrInvalid
		}
		if before.Size() > s.maxUploadBytes {
			return ErrUploadTooLarge
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		book, size, e := s.stage(user, f, "")
		f.Close()
		if e != nil {
			return fmt.Errorf("cannot snapshot book: %w", e)
		}
		after, e := os.Stat(path)
		if e != nil {
			return e
		}
		resolved, e = filepath.EvalSymlinks(path)
		if e != nil || !inside(root, resolved) || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return errors.New("watched file changed during scan")
		}
		relative, _ := filepath.Rel(root, path)
		items = append(items, scanned{book, relative, strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())), size})
		return nil
	})
	if err != nil {
		return err
	}
	current, err = rootIdentity(root)
	if err != nil || current != identity {
		return errors.New("watched root changed during scan")
	}
	// Snapshot objects are immutable. Parse them before opening the write transaction.
	metadata := make(map[string]bookMetadata, len(items))
	for _, item := range items {
		if _, seen := metadata[item.book]; !seen {
			meta := s.objectMetadata(user, item.book)
			meta.Cover = "" // Do not retain every thumbnail for a large scan.
			metadata[item.book] = meta
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.book] {
			continue
		}
		seen[item.book] = true
		var n int
		if err = tx.QueryRow("SELECT count(*) FROM files WHERE user_id=? AND book_id=?", user, item.book).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			existing++
		} else {
			imported++
		}
	}
	if _, err = tx.Exec("DELETE FROM files WHERE user_id=? AND kind='watch' AND source LIKE ?", user, id+"/%"); err != nil {
		return err
	}
	for _, item := range items {
		if _, err = tx.Exec("INSERT INTO files VALUES (?,?,'watch',?,?)", user, item.book, id+"/"+item.path, item.size); err != nil {
			return err
		}
		folder := filepath.ToSlash(filepath.Dir(item.path))
		if folder == "." {
			folder = ""
		}
		if !validFolder(folder) {
			return ErrInvalid
		}
		if err = seedBook(tx, user, item.book, item.title, metadata[item.book], folder); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func seedBook(tx *sql.Tx, user, book, title string, metadata bookMetadata, folders ...string) error {
	// Watched sources retain their filename; uploaded object names are hashes.
	metadata = inferMetadataVolume(metadata, title)
	revision := int64(1)
	var raw string
	err := tx.QueryRow("SELECT revision,candidates FROM records WHERE user_id=? AND book_id=? AND kind='book' AND record_id='default'", user, book).Scan(&revision, &raw)
	if err == nil {
		var old []Candidate
		if json.Unmarshal([]byte(raw), &old) != nil || len(old) != 1 || old[0].Deleted || len(old[0].OperationID) != 64 {
			return nil
		}
		var previous map[string]any
		if json.Unmarshal(old[0].Value, &previous) != nil || previous["title"] != title || previous["author"] != "" {
			return nil
		}
		if metadata.Title == "" && metadata.Author == "" {
			return nil
		}
		revision++
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if metadata.Title != "" {
		title = metadata.Title
	}
	fields := map[string]any{"title": title, "author": metadata.Author, "series": metadata.Series, "volume": metadata.Volume}
	if metadata.Format != "" {
		fields["format"] = metadata.Format
	}
	if metadata.Folder != "" && validFolder(metadata.Folder) {
		fields["folder"] = metadata.Folder
	}
	if len(folders) > 0 && folders[0] != "" {
		fields["folder"] = folders[0]
	}
	if metadata.Folders != nil && validFolders(metadata.Folders) {
		fields["folders"] = metadata.Folders
	}
	if len(folders) > 0 {
		delete(fields, "folders")
	}
	value, _ := json.Marshal(fields)
	value = canonicalBookFolders(value, nil)
	if raw != "" {
		var old []Candidate
		if json.Unmarshal([]byte(raw), &old) == nil && len(old) == 1 && string(old[0].Value) == string(value) {
			return nil
		}
	}
	record := Record{BookID: book, Kind: "book", RecordID: "default", Revision: revision, Candidates: []Candidate{{OperationID: randomID(), Value: value, CreatedAt: time.Now().UnixMilli()}}}
	candidates, _ := json.Marshal(record.Candidates)
	if _, err = tx.Exec("INSERT INTO records VALUES (?, ?, 'book', 'default', ?, ?) ON CONFLICT(user_id,book_id,kind,record_id) DO UPDATE SET revision=excluded.revision,candidates=excluded.candidates", user, book, revision, string(candidates)); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE cursors SET value=value+1 WHERE user_id=?", user); err != nil {
		return err
	}
	encoded, _ := json.Marshal(record)
	_, err = tx.Exec("INSERT INTO changes SELECT user_id,value,? FROM cursors WHERE user_id=?", string(encoded), user)
	return err
}
func (s *Store) ScanAll() error {
	rows, err := s.db.Query("SELECT id FROM watch_roots ORDER BY id")
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err = s.ScanWatch(id); err != nil {
			failures = append(failures, fmt.Errorf("watch %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}
func (s *Store) RemoveWatch(id string) error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var user string
	if err = tx.QueryRow("SELECT user_id FROM watch_roots WHERE id=?", id).Scan(&user); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM files WHERE user_id=? AND kind='watch' AND source LIKE ?", user, id+"/%"); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM scan_status WHERE watch_id=?", id); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM watch_roots WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

type WatchInfo struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Path     string `json:"path"`
}

func (s *Store) Watches() ([]WatchInfo, error) {
	rows, err := s.db.Query("SELECT w.id,u.username,w.root FROM watch_roots w JOIN users u ON u.id=w.user_id ORDER BY u.username,w.root")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WatchInfo{}
	for rows.Next() {
		var w WatchInfo
		if err = rows.Scan(&w.ID, &w.Username, &w.Path); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
