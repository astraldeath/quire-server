package server

import (
	"archive/zip"
	"context"
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
	"regexp"
	"strings"
	"time"
)

const backupFormat = 1
const maxRestoreBytes int64 = 100 << 30
const browserBackupLimitBytes int64 = 512 << 20

var objectName = regexp.MustCompile(`^objects/[a-f0-9]{64}/[a-f0-9]{64}\.(epub|cbz|fb2|fbz|mobi|azw3|pdf)$`)

type backupEntry struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type backupManifest struct {
	Format    int                    `json:"format"`
	CreatedAt string                 `json:"createdAt"`
	Files     map[string]backupEntry `json:"files"`
}

func archiveName(name string) bool { return name == "quire.db" || objectName.MatchString(name) }

func (s *Store) referencedBackupNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT user_id,book_id FROM files ORDER BY user_id,book_id")
	if err != nil {
		return nil, err
	}
	type reference struct{ owner, book string }
	references := []reference{}
	for rows.Next() {
		var ref reference
		if err = rows.Scan(&ref.owner, &ref.book); err != nil {
			rows.Close()
			return nil, err
		}
		references = append(references, ref)
		if len(references) > 100000 {
			rows.Close()
			return nil, errors.New("backup has too many files")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(references))
	for _, ref := range references {
		name := "objects/" + ref.owner + "/" + filepath.Base(s.objectPath(ref.owner, ref.book))
		if !archiveName(name) {
			return nil, ErrInvalid
		}
		names = append(names, name)
	}
	return names, nil
}

type backupStatus struct {
	InputBytes        int64 `json:"inputBytes"`
	BrowserLimitBytes int64 `json:"browserLimitBytes"`
	FitsBrowser       *bool `json:"fitsBrowser"`
}

func boolPointer(value bool) *bool { return &value }

func (s *Store) backupStatus(ctx context.Context) (backupStatus, error) {
	return s.backupStatusWithOpen(ctx, os.Open)
}

func (s *Store) backupStatusWithOpen(ctx context.Context, open func(string) (*os.File, error)) (backupStatus, error) {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	status := backupStatus{BrowserLimitBytes: browserBackupLimitBytes}
	var pageCount, pageSize int64
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return status, err
	}
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return status, err
	}
	if pageCount < 0 || pageSize < 0 || (pageCount != 0 && pageSize > (1<<63-1)/pageCount) {
		return status, errors.New("database size estimate overflow")
	}
	status.InputBytes = pageCount * pageSize
	names, err := s.referencedBackupNames(ctx, s.db)
	if err != nil {
		return status, err
	}
	var objectBytes int64
	unavailable := false
	for _, name := range names {
		path := filepath.Join(s.data, filepath.FromSlash(name))
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 {
			unavailable = true
			continue
		}
		f, openErr := open(path)
		if openErr != nil {
			unavailable = true
			continue
		}
		if closeErr := f.Close(); closeErr != nil {
			unavailable = true
			continue
		}
		if info.Size() > (1<<63-1)-objectBytes || info.Size() > (1<<63-1)-status.InputBytes {
			return status, errors.New("backup size estimate overflow")
		}
		objectBytes += info.Size()
		status.InputBytes += info.Size()
	}
	if unavailable {
		return status, nil
	}
	if objectBytes > browserBackupLimitBytes {
		status.FitsBrowser = boolPointer(false)
		return status, nil
	}
	// ZIP Store has no compression savings. Leave an explicit margin for the
	// snapshot, manifest and ZIP headers when the estimate is near the limit.
	overhead := int64(1<<20) + int64(len(names))*1024
	if status.InputBytes <= browserBackupLimitBytes-overhead {
		status.FitsBrowser = boolPointer(true)
	}
	return status, nil
}

// Backup snapshots SQLite while file mutations are paused, then packages only
// objects referenced by that snapshot. No logs, local secrets or source folders.
func (s *Store) Backup(ctx context.Context, destination string) (err error) {
	if !s.backupMu.TryLock() {
		return errors.New("another backup is already running")
	}
	defer s.backupMu.Unlock()
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	temp, err := os.MkdirTemp("", "quire-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	snapshot := filepath.Join(temp, "quire.db")
	if _, err = s.db.ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", snapshot)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, "PRAGMA secure_delete=ON; DELETE FROM sessions; DELETE FROM tracking_accounts; UPDATE tracking_links SET auto=0; PRAGMA journal_mode=DELETE;"); err != nil {
		return err
	}
	names := []string{"quire.db"}
	objectNames, err := s.referencedBackupNames(ctx, db)
	if err != nil {
		return err
	}
	names = append(names, objectNames...)
	if err = db.Close(); err != nil {
		return err
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(destination)
		}
	}()
	writer := zip.NewWriter(out)
	defer writer.Close()
	manifest := backupManifest{backupFormat, time.Now().UTC().Format(time.RFC3339), map[string]backupEntry{}}
	var totalBytes int64
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			return err
		}
		source := filepath.Join(s.data, filepath.FromSlash(name))
		if name == "quire.db" {
			source = snapshot
		}
		info, e := os.Lstat(source)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("backup source is not a regular file")
		}
		if info.Size() < 0 || info.Size() > maxRestoreBytes-totalBytes {
			return errors.New("backup exceeds 100 GiB format limit")
		}
		totalBytes += info.Size()
		if name == "quire.db" && info.Size() > 1<<30 {
			return errors.New("database exceeds 1 GiB format limit")
		}
		if name != "quire.db" && info.Size() > MaxStoredBookBytes {
			return ErrInvalid
		}
		f, e := os.Open(source)
		if e != nil {
			return e
		}
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0600)
		entry, e := writer.CreateHeader(header)
		if e != nil {
			f.Close()
			return e
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(entry, hash), f)
		f.Close()
		if e != nil {
			return e
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if name != "quire.db" && strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)) != digest {
			return errors.New("book snapshot checksum mismatch")
		}
		manifest.Files[name] = backupEntry{n, digest}
	}
	entry, err := writer.Create("manifest.json")
	if err != nil {
		return err
	}
	if err = json.NewEncoder(entry).Encode(manifest); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	return out.Close()
}

// RestoreBackup publishes an entirely validated staging directory. Existing
// destinations are never overwritten, including a running server's directory.
func RestoreBackup(archive, destination string) error {
	target, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return errors.New("restore destination must be a new directory")
	}
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	if len(reader.File) > 100002 {
		return errors.New("backup has too many entries")
	}
	entries := map[string]*zip.File{}
	var total uint64
	for _, f := range reader.File {
		if entries[f.Name] != nil || (!archiveName(f.Name) && f.Name != "manifest.json") || !f.Mode().IsRegular() {
			return errors.New("invalid archive entry")
		}
		if f.UncompressedSize64 > uint64(maxRestoreBytes)-total {
			return errors.New("backup exceeds 100 GiB restore limit")
		}
		total += f.UncompressedSize64
		entries[f.Name] = f
	}
	meta := entries["manifest.json"]
	if meta == nil || meta.UncompressedSize64 > 32<<20 {
		return errors.New("missing or oversized backup manifest")
	}
	r, err := meta.Open()
	if err != nil {
		return err
	}
	var manifest backupManifest
	err = json.NewDecoder(io.LimitReader(r, 32<<20)).Decode(&manifest)
	r.Close()
	if err != nil {
		return err
	}
	if manifest.Format != backupFormat || len(manifest.Files)+1 != len(entries) || entries["quire.db"] == nil {
		return errors.New("unsupported or incomplete backup")
	}
	for name, expected := range manifest.Files {
		f := entries[name]
		if !archiveName(name) || f == nil || expected.Size < 0 || uint64(expected.Size) != f.UncompressedSize64 || len(expected.SHA256) != 64 {
			return errors.New("backup inventory mismatch")
		}
		if name != "quire.db" && expected.Size > MaxStoredBookBytes {
			return ErrInvalid
		}
		if name == "quire.db" && expected.Size > 1<<30 {
			return errors.New("database exceeds 1 GiB restore limit")
		}
	}
	parent := filepath.Dir(target)
	if err = os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".quire-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for name, expected := range manifest.Files {
		dest := filepath.Join(stage, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		input, e := entries[name].Open()
		if e != nil {
			return e
		}
		out, e := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			input.Close()
			return e
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(out, hash), io.LimitReader(input, expected.Size+1))
		input.Close()
		closeErr := out.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if n != expected.Size || digest != expected.SHA256 {
			return errors.New("backup checksum mismatch")
		}
		if name != "quire.db" && digest != strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)) {
			return errors.New("book checksum mismatch")
		}
		if name != "quire.db" {
			format, e := detectBookFormat(dest)
			if e != nil || "."+format != filepath.Ext(name) {
				return errors.New("backup book format mismatch")
			}
		}
	}
	if err = prepareRestoredDatabase(filepath.Join(stage, "quire.db"), manifest.Files); err != nil {
		return err
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return errors.New("restore destination already exists")
	}
	return os.Rename(stage, target)
}
func prepareRestoredDatabase(path string, files map[string]backupEntry) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 6 || version > 12 {
		return errors.New("backup database version is not supported")
	}
	var check string
	if err = db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return errors.New("backup database is damaged")
	}
	rows, err := db.Query("SELECT DISTINCT user_id,book_id FROM files")
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var owner, book string
		if err = rows.Scan(&owner, &book); err != nil {
			rows.Close()
			return err
		}
		found := 0
		for _, format := range bookFormats {
			if _, ok := files["objects/"+owner+"/"+book+"."+format]; ok {
				found++
			}
		}
		if found != 1 {
			rows.Close()
			return errors.New("backup is missing a referenced book")
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if count+1 != len(files) {
		return errors.New("unreferenced files in backup")
	}
	if version >= 7 {
		if _, err = db.Exec("DELETE FROM tracking_accounts; UPDATE tracking_links SET auto=0"); err != nil {
			return err
		}
	}
	_, err = db.Exec("DELETE FROM sessions; INSERT INTO server_settings(id,name,scan_seconds) VALUES (1,'Quire',0) ON CONFLICT(id) DO UPDATE SET scan_seconds=0; PRAGMA journal_mode=DELETE;")
	return err
}
func (a *api) backupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/backup/status", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		status, err := a.store.backupStatus(r.Context())
		if err != nil {
			failure(w, err)
			return
		}
		respond(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /v1/admin/backup", func(w http.ResponseWriter, r *http.Request) {
		if a.admin(w, r) == "" {
			return
		}
		dir, err := os.MkdirTemp("", "quire-download-")
		if err != nil {
			failure(w, err)
			return
		}
		defer os.RemoveAll(dir)
		archive := filepath.Join(dir, "server.quire-server-backup")
		if err = a.store.Backup(r.Context(), archive); err != nil {
			respond(w, 500, map[string]string{"error": "Backup could not be created. Check available disk space and server book files."})
			return
		}
		f, err := os.Open(archive)
		if err != nil {
			failure(w, err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			failure(w, err)
			return
		}
		if info.Size() > browserBackupLimitBytes {
			respond(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Backup exceeds the browser download limit."})
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="quire-server-%s.quire-server-backup"`, time.Now().UTC().Format("2006-01-02")))
		w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
		io.Copy(w, f)
	})
}
