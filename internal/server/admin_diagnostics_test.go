package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type diagnosticsFileInfo struct {
	name string
	mode os.FileMode
	size int64
}

func (i diagnosticsFileInfo) Name() string       { return i.name }
func (i diagnosticsFileInfo) Size() int64        { return i.size }
func (i diagnosticsFileInfo) Mode() os.FileMode  { return i.mode }
func (i diagnosticsFileInfo) ModTime() time.Time { return time.Time{} }
func (i diagnosticsFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i diagnosticsFileInfo) Sys() any           { return nil }

type diagnosticsDirEntry struct{ diagnosticsFileInfo }

func (e diagnosticsDirEntry) Type() os.FileMode          { return e.mode.Type() }
func (e diagnosticsDirEntry) Info() (os.FileInfo, error) { return e.diagnosticsFileInfo, nil }

func rawRequest(t *testing.T, h http.Handler, method, path, token string, status int) []byte {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w.Body.Bytes()
}

func TestScanDiagnosticsMigrationUpgradesVersionElevenAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quire.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`ALTER TABLE scan_status DROP COLUMN skipped_files;
		ALTER TABLE scan_status DROP COLUMN omitted_skipped_files;
		DROP TABLE folder_catalog; PRAGMA user_version=11;`); err != nil {
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
	var version int
	var details string
	var omitted int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 13 {
		t.Fatalf("migration version=%d error=%v", version, err)
	}
	if _, err = s.db.Exec("INSERT INTO scan_status(watch_id,last_at,error,imported,existing,skipped) VALUES ('watch',1,'',0,0,0)"); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT skipped_files,omitted_skipped_files FROM scan_status WHERE watch_id='watch'").Scan(&details, &omitted); err != nil || details != "[]" || omitted != 0 {
		t.Fatalf("migration defaults details=%q omitted=%d error=%v", details, omitted, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 13 {
		t.Fatalf("reopen version=%d error=%v", version, err)
	}
}

func TestScanDiagnosticsSurviveBackupRestore(t *testing.T) {
	s, _ := fixture(t)
	details := `[{"path":"notes.txt","reason":"unsupported-format"}]`
	if _, err := s.db.Exec("INSERT INTO scan_status(watch_id,last_at,error,imported,existing,skipped,skipped_files,omitted_skipped_files) VALUES ('watch',1,'failed',0,0,3,?,2)", details); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "server.zip")
	if err := s.Backup(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := RestoreBackup(archive, destination); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(filepath.Join(destination, "quire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var gotDetails string
	var gotOmitted, version int
	if err = restored.db.QueryRow("SELECT skipped_files,omitted_skipped_files FROM scan_status WHERE watch_id='watch'").Scan(&gotDetails, &gotOmitted); err != nil || gotDetails != details || gotOmitted != 2 {
		t.Fatalf("restored details=%q omitted=%d error=%v", gotDetails, gotOmitted, err)
	}
	if err = restored.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 13 {
		t.Fatalf("restored version=%d error=%v", version, err)
	}
}

func TestScanWatchReturnsDiagnosticPersistenceErrors(t *testing.T) {
	for _, test := range []struct {
		name        string
		invalidBook bool
	}{
		{name: "after successful scan"},
		{name: "joined with scan failure", invalidBook: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _ := fixture(t)
			root := t.TempDir()
			if test.invalidBook {
				if err := os.WriteFile(filepath.Join(root, "invalid.epub"), []byte("not an epub"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			id, err := s.AddWatch("alice", root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.Exec("DROP TABLE scan_status"); err != nil {
				t.Fatal(err)
			}
			err = s.ScanWatch(id)
			if err == nil || !strings.Contains(err.Error(), "scan_status") {
				t.Fatalf("missing diagnostics persistence error: %v", err)
			}
			if test.invalidBook && !errors.Is(err, ErrInvalid) {
				t.Fatalf("original scan failure was lost: %v", err)
			}
		})
	}
}

func TestScanDiagnosticsCaptureFailedAttemptWithinBounds(t *testing.T) {
	data := t.TempDir()
	limit := int64(len(epubBytes()))
	s, err := OpenWithOptions(filepath.Join(data, "quire.db"), StoreOptions{MaxUploadBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.CreateUser("alice", testPassword); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	tooLarge := filepath.Join(root, "000-too-large.epub")
	if err = os.WriteFile(tooLarge, make([]byte, limit+1), 0600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "003-good.epub")
	if err = os.WriteFile(good, epubBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	longName := "002-" + strings.Repeat("x", 180) + ".txt"
	if err = os.WriteFile(filepath.Join(root, longName), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 105; i++ {
		name := filepath.Join(root, "100-skip-"+string(rune('a'+i/26))+string(rune('a'+i%26))+".txt")
		if err = os.WriteFile(name, []byte("ignored"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "zzz-corrupt.epub"), []byte("not an epub"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(id); err == nil {
		t.Fatal("invalid final book should fail the scan")
	}

	h := NewHandler(s, "https://books.example", "Test Quire")
	rawRequest(t, h, "GET", "/v1/admin/scans", "", http.StatusUnauthorized)
	rawRequest(t, h, "GET", "/v1/admin/scans", login(t, h, "alice"), http.StatusForbidden)
	if err = s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	raw := rawRequest(t, h, "GET", "/v1/admin/scans", login(t, h, "alice"), http.StatusOK)
	var scans []struct {
		Imported            int               `json:"imported"`
		Existing            int               `json:"existing"`
		Skipped             int               `json:"skipped"`
		SkippedFiles        []scanSkippedFile `json:"skippedFiles"`
		OmittedSkippedFiles int               `json:"omittedSkippedFiles"`
		Error               string            `json:"error"`
	}
	if err = json.Unmarshal(raw, &scans); err != nil || len(scans) != 1 {
		t.Fatalf("scan response %s: %v", raw, err)
	}
	scan := scans[0]
	wantSkipped := 107
	if scan.Imported != 0 || scan.Existing != 0 || scan.Error == "" || scan.Skipped != wantSkipped {
		t.Fatalf("failed attempt counters: %+v", scan)
	}
	if len(scan.SkippedFiles) > 100 || scan.OmittedSkippedFiles != scan.Skipped-len(scan.SkippedFiles) {
		t.Fatalf("detail bound=%d omitted=%d skipped=%d", len(scan.SkippedFiles), scan.OmittedSkippedFiles, scan.Skipped)
	}
	encoded, err := json.Marshal(scan.SkippedFiles)
	if err != nil || len(encoded) > 64<<10 {
		t.Fatalf("serialized details=%d error=%v", len(encoded), err)
	}
	reasons := map[string]string{}
	for _, detail := range scan.SkippedFiles {
		reasons[detail.Path] = detail.Reason
	}
	if reasons["000-too-large.epub"] != "too-large" || reasons[longName] != "unsupported-format" {
		t.Fatalf("missing classified details: %#v", reasons)
	}
	var files int
	if err = s.db.QueryRow("SELECT count(*) FROM files").Scan(&files); err != nil || files != 0 {
		t.Fatalf("failed scan reported committed files=%d error=%v", files, err)
	}

	diagnostics := scanDiagnostics{}
	diagnostics.add(strings.Repeat("x", 64<<10), "unsupported-format")
	if len(diagnostics.files) != 0 || diagnostics.omitted != 1 || diagnostics.skipped != 1 {
		t.Fatalf("oversized detail was retained: %+v", diagnostics)
	}
}

func TestScanSkipReasonClassifiesSymlinkAndNonregular(t *testing.T) {
	for _, test := range []struct {
		name  string
		entry diagnosticsDirEntry
		want  string
	}{
		{name: "symlink", entry: diagnosticsDirEntry{diagnosticsFileInfo{name: "link.epub", mode: os.ModeSymlink}}, want: "symlink"},
		{name: "nonregular", entry: diagnosticsDirEntry{diagnosticsFileInfo{name: "pipe.epub", mode: os.ModeNamedPipe}}, want: "not-regular"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := scanSkipReason(test.entry.Name(), test.entry, DefaultMaxUploadBytes)
			if err != nil || got != test.want {
				t.Fatalf("reason=%q want=%q error=%v", got, test.want, err)
			}
		})
	}
}

func TestScanDiagnosticsPersistSymlinkWhenSupported(t *testing.T) {
	s, h := fixture(t)
	if err := s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target.epub")
	if err := os.WriteFile(target, epubBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "link.epub")); err != nil {
		t.Skipf("symlink integration unavailable: %v", err)
	}
	id, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(id); err != nil {
		t.Fatal(err)
	}
	raw := rawRequest(t, h, "GET", "/v1/admin/scans", login(t, h, "alice"), http.StatusOK)
	var scans []struct {
		ID                  string            `json:"id"`
		Skipped             int               `json:"skipped"`
		SkippedFiles        []scanSkippedFile `json:"skippedFiles"`
		OmittedSkippedFiles int               `json:"omittedSkippedFiles"`
	}
	if err = json.Unmarshal(raw, &scans); err != nil || len(scans) != 1 {
		t.Fatalf("scan response %s: %v", raw, err)
	}
	if scans[0].ID != id || scans[0].Skipped != 1 || scans[0].OmittedSkippedFiles != 0 || len(scans[0].SkippedFiles) != 1 || scans[0].SkippedFiles[0] != (scanSkippedFile{Path: "link.epub", Reason: "symlink"}) {
		t.Fatalf("symlink diagnostic was not persisted: %+v", scans[0])
	}
}

type backupStatusResponse struct {
	InputBytes        int64 `json:"inputBytes"`
	BrowserLimitBytes int64 `json:"browserLimitBytes"`
	FitsBrowser       *bool `json:"fitsBrowser"`
}

func databaseSizeEstimate(t *testing.T, s *Store) int64 {
	t.Helper()
	var pages, pageSize int64
	if err := s.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	return pages * pageSize
}

func backupStatusRequest(t *testing.T, h http.Handler, token string, status int) backupStatusResponse {
	t.Helper()
	raw := rawRequest(t, h, "GET", "/v1/admin/backup/status", token, status)
	var got backupStatusResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
	}
	return got
}

func TestBackupStatusUsesOnlyReferencedObjects(t *testing.T) {
	s, h := fixture(t)
	if err := s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	admin := login(t, h, "alice")
	backupStatusRequest(t, h, "", http.StatusUnauthorized)
	backupStatusRequest(t, h, login(t, h, "bob"), http.StatusForbidden)
	baseline := backupStatusRequest(t, h, admin, http.StatusOK)
	if baseline.InputBytes <= 0 || baseline.BrowserLimitBytes != 512<<20 || baseline.FitsBrowser == nil || !*baseline.FitsBrowser {
		t.Fatalf("unexpected empty status: %+v", baseline)
	}
	var alice string
	if err := s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&alice); err != nil {
		t.Fatal(err)
	}
	orphanDir := filepath.Join(s.data, "objects", alice)
	if err := os.MkdirAll(orphanDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, ".upload-orphan"), []byte(strings.Repeat("x", 4096)), 0600); err != nil {
		t.Fatal(err)
	}
	withOrphan := backupStatusRequest(t, h, admin, http.StatusOK)
	if withOrphan.InputBytes != databaseSizeEstimate(t, s) {
		t.Fatalf("orphan staging file counted: before=%d after=%d", baseline.InputBytes, withOrphan.InputBytes)
	}
	book, size, err := s.stage(alice, strings.NewReader(string(epubBytes())), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO files VALUES (?,?,'upload','',?)", alice, book, size); err != nil {
		t.Fatal(err)
	}
	withBook := backupStatusRequest(t, h, admin, http.StatusOK)
	if withBook.InputBytes != databaseSizeEstimate(t, s)+size || withBook.FitsBrowser == nil || !*withBook.FitsBrowser {
		t.Fatalf("referenced object not counted once: baseline=%+v withBook=%+v size=%d", baseline, withBook, size)
	}
}

func TestBackupStatusIsUnknownForUnavailableObject(t *testing.T) {
	s, h := fixture(t)
	if err := s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	var alice string
	if err := s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&alice); err != nil {
		t.Fatal(err)
	}
	book := strings.Repeat("a", 64)
	if _, err := s.db.Exec("INSERT INTO files VALUES (?,?,'upload','',1)", alice, book); err != nil {
		t.Fatal(err)
	}
	status := backupStatusRequest(t, h, login(t, h, "alice"), http.StatusOK)
	if status.FitsBrowser != nil {
		t.Fatalf("missing object should be unknown: %+v", status)
	}
	path := s.objectPath(alice, book)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	status = backupStatusRequest(t, h, login(t, h, "alice"), http.StatusOK)
	if status.FitsBrowser != nil {
		t.Fatalf("nonregular object should be unknown: %+v", status)
	}
}

func TestBackupStatusIsUnknownWhenReferencedObjectCannotBeOpened(t *testing.T) {
	s, _ := fixture(t)
	var alice string
	if err := s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&alice); err != nil {
		t.Fatal(err)
	}
	book, size, err := s.stage(alice, strings.NewReader(string(epubBytes())), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO files VALUES (?,?,'upload','',?)", alice, book, size); err != nil {
		t.Fatal(err)
	}
	path := s.objectPath(alice, book)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("test object must pass regular-file stat: mode=%v", info.Mode())
	}
	opened := false
	status, err := s.backupStatusWithOpen(context.Background(), func(name string) (*os.File, error) {
		opened = true
		if name != path {
			t.Fatalf("opened unexpected path %q", name)
		}
		return nil, os.ErrPermission
	})
	if err != nil {
		t.Fatal(err)
	}
	if !opened || status.FitsBrowser != nil {
		t.Fatalf("unreadable regular object should be unknown: opened=%v status=%+v", opened, status)
	}
}

func TestBackupStatusIsUnknownNearLimitAndRejectsProvablyOversizedStoredPayload(t *testing.T) {
	s, h := fixture(t)
	if err := s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	var alice string
	if err := s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&alice); err != nil {
		t.Fatal(err)
	}
	book := strings.Repeat("b", 64)
	path := s.objectPath(alice, book)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate((512 << 20) - 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO files VALUES (?,?,'upload','',?)", alice, book, (512<<20)-1); err != nil {
		t.Fatal(err)
	}
	status := backupStatusRequest(t, h, login(t, h, "alice"), http.StatusOK)
	if status.FitsBrowser != nil {
		t.Fatalf("near-boundary estimate should be unknown: %+v", status)
	}
	if err = os.Truncate(path, (512<<20)+1); err != nil {
		t.Fatal(err)
	}
	status = backupStatusRequest(t, h, login(t, h, "alice"), http.StatusOK)
	if status.FitsBrowser == nil || *status.FitsBrowser || status.InputBytes <= status.BrowserLimitBytes {
		t.Fatalf("oversized stored payload was not rejected: %+v", status)
	}
}

func TestInviteListIncludesCurrentGrantsWithoutSecrets(t *testing.T) {
	s, h := fixture(t)
	if err := s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	admin := login(t, h, "alice")
	rawRequest(t, h, "GET", "/v1/admin/invites", login(t, h, "bob"), http.StatusForbidden)
	created := request(t, h, "POST", "/v1/admin/invites", admin, nil, http.StatusCreated)
	var invite string
	if err := json.Unmarshal(created["id"], &invite); err != nil {
		t.Fatal(err)
	}
	library := request(t, h, "POST", "/v1/admin/libraries", admin, map[string]string{"name": "Shared"}, http.StatusCreated)
	var libraryID string
	if err := json.Unmarshal(library["id"], &libraryID); err != nil {
		t.Fatal(err)
	}
	request(t, h, "PUT", "/v1/admin/invites/"+invite+"/libraries/"+libraryID, admin, nil, http.StatusNoContent)
	request(t, h, "DELETE", "/v1/admin/invites/"+invite, admin, nil, http.StatusNoContent)

	raw := rawRequest(t, h, "GET", "/v1/admin/invites", admin, http.StatusOK)
	var invites []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &invites); err != nil || len(invites) != 1 {
		t.Fatalf("invite list %s: %v", raw, err)
	}
	for _, secret := range []string{"code", "code_hash", "token"} {
		if _, ok := invites[0][secret]; ok {
			t.Fatalf("invite response exposed %s: %s", secret, raw)
		}
	}
	var status string
	var grants []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(invites[0]["status"], &status); err != nil || status != "revoked" {
		t.Fatalf("revoked invite status=%q error=%v", status, err)
	}
	if err := json.Unmarshal(invites[0]["libraries"], &grants); err != nil || len(grants) != 1 || grants[0].ID != libraryID || grants[0].Name != "Shared" {
		t.Fatalf("invite grants=%+v error=%v body=%s", grants, err, raw)
	}

	request(t, h, "DELETE", "/v1/admin/libraries/"+libraryID, admin, nil, http.StatusNoContent)
	raw = rawRequest(t, h, "GET", "/v1/admin/invites", admin, http.StatusOK)
	if err := json.Unmarshal(raw, &invites); err != nil || len(invites) != 1 {
		t.Fatalf("invite list after delete %s: %v", raw, err)
	}
	if err := json.Unmarshal(invites[0]["libraries"], &grants); err != nil || len(grants) != 0 {
		t.Fatalf("deleted library retained in grants=%+v error=%v", grants, err)
	}
}
