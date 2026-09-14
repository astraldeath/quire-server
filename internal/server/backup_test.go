package server

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupRestoresLibraryAndRevokesSessions(t *testing.T) {
	s, h := fixture(t)
	s.Promote("alice")
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "book.epub"), epubBytes(), 0600)
	watch, e := s.AddWatch("alice", root)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ScanWatch(watch); e != nil {
		t.Fatal(e)
	}
	session, e := s.Login("alice", testPassword, "test")
	if e != nil {
		t.Fatal(e)
	}
	_ = session
	syncRequest(t, h, login(t, h, "alice"), 0, op("backup-note", 0, false))
	var originalRecords int
	s.db.QueryRow("SELECT count(*) FROM records").Scan(&originalRecords)
	archive := filepath.Join(t.TempDir(), "server.zip")
	if e = s.Backup(context.Background(), archive); e != nil {
		t.Fatal(e)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if e = RestoreBackup(archive, destination); e != nil {
		t.Fatal(e)
	}
	restored, e := Open(filepath.Join(destination, "quire.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	if _, e = restored.Login("alice", testPassword, "restored"); e != nil {
		t.Fatal(e)
	}
	var restoredRecords int
	restored.db.QueryRow("SELECT count(*) FROM records").Scan(&restoredRecords)
	if restoredRecords != originalRecords {
		t.Fatal("reading records changed")
	}
	var count int
	restored.db.QueryRow("SELECT count(*) FROM sessions WHERE device_name='test'").Scan(&count)
	if count != 0 {
		t.Fatal("old sessions restored")
	}
	settings, e := restored.Settings(ServerSettings{"Quire", 300})
	if e != nil || settings.ScanSeconds != 0 {
		t.Fatal("scans not paused", e)
	}
	var owner, book string
	if e = restored.db.QueryRow("SELECT user_id,book_id FROM files").Scan(&owner, &book); e != nil {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(restored.objectPath(owner, book)); e != nil || !bytes.Equal(b, epubBytes()) {
		t.Fatal("book snapshot lost", e)
	}
	corrupt := filepath.Join(t.TempDir(), "corrupt.zip")
	source, _ := zip.OpenReader(archive)
	output, _ := os.Create(corrupt)
	writer := zip.NewWriter(output)
	for _, entry := range source.File {
		input, _ := entry.Open()
		body, _ := io.ReadAll(input)
		input.Close()
		if entry.Name == "quire.db" {
			body[0] ^= 1
		}
		out, _ := writer.Create(entry.Name)
		out.Write(body)
	}
	writer.Close()
	output.Close()
	source.Close()
	badDestination := filepath.Join(t.TempDir(), "bad-restored")
	if e = RestoreBackup(corrupt, badDestination); e == nil {
		t.Fatal("accepted corrupt database")
	}
	if _, e = os.Stat(badDestination); !os.IsNotExist(e) {
		t.Fatal("partial corrupt restore")
	}
	if e = RestoreBackup(archive, destination); e == nil {
		t.Fatal("overwrote existing directory")
	}
}
func TestRestoreRejectsUnsafeOrIncompleteArchive(t *testing.T) {
	for _, name := range []string{"../escape", "objects/link", "quire.db"} {
		t.Run(name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "bad.zip")
			f, _ := os.Create(archive)
			z := zip.NewWriter(f)
			entry, _ := z.Create(name)
			entry.Write([]byte("bad"))
			z.Close()
			f.Close()
			destination := filepath.Join(t.TempDir(), "restore")
			if e := RestoreBackup(archive, destination); e == nil {
				t.Fatal("accepted malformed archive")
			}
			if _, e := os.Stat(destination); !os.IsNotExist(e) {
				t.Fatal("left partial destination")
			}
		})
	}
}
func TestBackupRequiresAdmin(t *testing.T) {
	_, h := fixture(t)
	r := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/backup", strings.NewReader("{}"))
	h.ServeHTTP(r, req)
	if r.Code != 401 {
		t.Fatal(r.Code)
	}
}

func TestBackupHTTPAndNoOverwrite(t *testing.T) {
	s, h := fixture(t)
	request(t, h, "POST", "/v1/admin/backup", login(t, h, "bob"), nil, 403)
	s.Promote("alice")
	r := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/backup", nil)
	req.Header.Set("Authorization", "Bearer "+login(t, h, "alice"))
	h.ServeHTTP(r, req)
	if r.Code != 200 || r.Header().Get("Content-Type") != "application/zip" {
		t.Fatal(r.Code, r.Body.String())
	}
	if _, e := zip.NewReader(bytes.NewReader(r.Body.Bytes()), int64(r.Body.Len())); e != nil {
		t.Fatal(e)
	}
	archive := filepath.Join(t.TempDir(), "existing")
	os.WriteFile(archive, []byte("keep"), 0600)
	if e := s.Backup(context.Background(), archive); e == nil {
		t.Fatal("overwrote backup")
	}
	b, _ := os.ReadFile(archive)
	if string(b) != "keep" {
		t.Fatal("changed existing backup")
	}
}
