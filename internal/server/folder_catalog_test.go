package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestFolderCatalogCASAndIsolation(t *testing.T) {
	_, h := fixture(t)
	alice, bob := login(t, h, "alice"), login(t, h, "bob")
	path := "/v1/folders/sync"
	read := map[string]any{"revision": 0, "state": nil}
	request(t, h, "POST", path, "", read, 401)
	state := map[string]any{"library": []string{"Empty/Nested"}, "hidden": []string{"Secret"}}
	write := map[string]any{"revision": 0, "state": state}
	first := request(t, h, "POST", path, alice, write, 200)
	if string(first["revision"]) != "1" {
		t.Fatal(first)
	}
	retry := request(t, h, "POST", path, alice, write, 200)
	if string(retry["revision"]) != "1" || string(retry["accepted"]) != "true" {
		t.Fatal(retry)
	}
	stale := request(t, h, "POST", path, alice, map[string]any{"revision": 0, "state": map[string]any{"library": []string{}, "hidden": []string{}}}, 200)
	if string(stale["accepted"]) != "false" || !strings.Contains(string(stale["state"]), "Empty/Nested") {
		t.Fatal(stale)
	}
	request(t, h, "POST", path, alice, map[string]any{"revision": 99, "state": state}, 409)
	request(t, h, "POST", path, alice, map[string]any{"revision": 99, "state": nil}, 200)
	other := request(t, h, "POST", path, bob, read, 200)
	if strings.Contains(string(other["state"]), "Secret") || string(other["revision"]) != "0" {
		t.Fatal(other)
	}
	for _, value := range []any{map[string]any{"library": nil, "hidden": []string{}}, map[string]any{"library": []string{".."}, "hidden": []string{}}, map[string]any{"library": []string{"a", "a"}, "hidden": []string{}}} {
		request(t, h, "POST", path, alice, map[string]any{"revision": 1, "state": value}, 400)
	}
}
func TestFolderCatalogMigrationBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quire.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DROP TABLE folder_catalog; DROP TABLE catalog_sources; DROP TABLE catalog_operations; DROP TABLE opds_passwords; PRAGMA user_version=12"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 14 {
		t.Fatal(version)
	}
	if err = s.CreateUser("alice", testPassword); err != nil {
		t.Fatal(err)
	}
	var user string
	s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&user)
	state := &FolderCatalog{Library: []string{"Empty/Nested"}, Hidden: []string{"Secret"}}
	if _, err = s.SyncFolderCatalog(context.Background(), user, FolderCatalogRequest{State: state}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "folders.zip")
	if err = s.Backup(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err = RestoreBackup(archive, dest); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(filepath.Join(dest, "quire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	out, err := restored.SyncFolderCatalog(context.Background(), user, FolderCatalogRequest{})
	if err != nil || out.Revision != 1 || len(out.State.Hidden) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestFolderCatalogBoundsCascadeAndFutureBackup(t *testing.T) {
	paths := make([]string, 5001)
	for i := range paths {
		paths[i] = strings.Repeat("a", 250) + string(rune(0x100+i))
	}
	if err := (FolderCatalog{Library: paths, Hidden: []string{}}).validate(); err == nil {
		t.Fatal("accepted too many paths")
	}
	if err := (FolderCatalog{Library: paths[:5000], Hidden: []string{}}).validate(); err == nil {
		t.Fatal("accepted oversized JSON")
	}
	s, _ := fixture(t)
	if _, err := s.db.Exec("INSERT INTO users(id,username,salt,password_hash) VALUES ('catalog-only','catalog-only',X'',X'')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncFolderCatalog(context.Background(), "catalog-only", FolderCatalogRequest{State: &FolderCatalog{Library: []string{"A"}, Hidden: []string{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM users WHERE id='catalog-only'"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM folder_catalog WHERE user_id='catalog-only'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("catalog not removed: %d %v", count, err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=15"); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "future.zip")
	if err := s.Backup(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup(archive, filepath.Join(t.TempDir(), "restore")); err == nil {
		t.Fatal("accepted future schema")
	}
}
