package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivacySurvivesServerBackup(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	user, _ := s.authenticate(token)
	state := &PrivacySettings{Credential: &PrivacyCredential{Salt: strings.Repeat("a", 32), Hash: strings.Repeat("b", 64)}, Books: map[string]string{testBook: "hidden"}}
	if _, err := s.SyncPrivacy(context.Background(), user, PrivacyRequest{State: state}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "privacy.zip")
	if err := s.Backup(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if err := RestoreBackup(archive, dest); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(filepath.Join(dest, "quire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	out, err := restored.SyncPrivacy(context.Background(), user, PrivacyRequest{})
	if err != nil || out.Revision != 1 || out.State.Books[testBook] != "hidden" || out.State.Credential.Hash != state.Credential.Hash {
		t.Fatalf("lost privacy: %+v %v", out, err)
	}
}

func TestPrivacySyncIsolationAndRevision(t *testing.T) {
	_, h := fixture(t)
	alice, bob := login(t, h, "alice"), login(t, h, "bob")
	path := "/v1/privacy/sync"
	read := map[string]any{"revision": 0, "state": nil}
	request(t, h, "POST", path, "", read, 401)
	state := map[string]any{"credential": map[string]string{"salt": strings.Repeat("a", 32), "hash": strings.Repeat("b", 64)}, "books": map[string]string{testBook: "hidden"}}
	first := request(t, h, "POST", path, alice, map[string]any{"revision": 0, "state": state}, 200)
	if string(first["revision"]) != "1" || string(first["accepted"]) != "true" {
		t.Fatalf("first: %s", first)
	}
	stale := request(t, h, "POST", path, alice, map[string]any{"revision": 0, "state": map[string]any{"credential": nil, "books": map[string]string{}}}, 200)
	if string(stale["accepted"]) != "false" || !strings.Contains(string(stale["state"]), "hidden") {
		t.Fatalf("stale overwrote privacy: %s", stale)
	}
	retry := request(t, h, "POST", path, alice, map[string]any{"revision": 1, "state": state}, 200)
	if string(retry["revision"]) != "1" {
		t.Fatal("unchanged state created revision")
	}
	request(t, h, "POST", path, alice, map[string]any{"revision": 99, "state": nil}, 200)
	request(t, h, "POST", path, alice, map[string]any{"revision": 99, "state": state}, 409)
	private := request(t, h, "POST", path, bob, read, 200)
	if string(private["revision"]) != "0" || strings.Contains(string(private["state"]), "hidden") {
		t.Fatal("privacy leaked")
	}
	// Explicit unhide at the current revision propagates; no plaintext PIN field is accepted.
	state["books"] = map[string]string{}
	updated := request(t, h, "POST", path, alice, map[string]any{"revision": 1, "state": state}, 200)
	if string(updated["revision"]) != "2" {
		t.Fatal("unhide not saved")
	}
	state["passcode"] = "123456"
	request(t, h, "POST", path, alice, map[string]any{"revision": 2, "state": state}, 400)
}

func TestPrivacySyncValidation(t *testing.T) {
	_, h := fixture(t)
	token := login(t, h, "alice")
	for _, state := range []any{
		map[string]any{"credential": nil, "books": map[string]string{testBook: "hidden"}},
		map[string]any{"credential": map[string]string{"salt": "x", "hash": "y"}, "books": map[string]string{}},
		map[string]any{"credential": nil, "books": nil},
	} {
		request(t, h, "POST", "/v1/privacy/sync", token, map[string]any{"revision": 0, "state": state}, 400)
	}
}

func TestPrivacyMigrationFromVersionNine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quire.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DROP TABLE folder_catalog; DROP TABLE privacy_settings; ALTER TABLE tracking_links DROP COLUMN is_private; ALTER TABLE scan_status DROP COLUMN skipped_files; ALTER TABLE scan_status DROP COLUMN omitted_skipped_files; PRAGMA user_version=9;"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count, version int
	if err = s.db.QueryRow("SELECT count(*) FROM privacy_settings").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 13 {
		t.Fatalf("migration failed %d %v", version, err)
	}
}
