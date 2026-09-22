package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogSourceRevisionOwnershipAndSecrets(t *testing.T) {
	s, h := fixture(t)
	alice, bob := login(t, h, "alice"), login(t, h, "bob")
	input := map[string]any{"name": "Books", "url": "https://example.com/opds", "baseRevision": 0, "operationId": "source-write-one", "deleted": false, "credentials": map[string]string{"username": "reader", "password": "source-secret"}}
	first := request(t, h, "PUT", "/v1/catalog-sources/test-source", alice, input, 200)
	if string(first["revision"]) != "1" {
		t.Fatal(first)
	}
	request(t, h, "PUT", "/v1/catalog-sources/test-source", alice, input, 200)
	input["operationId"] = "source-write-two"
	request(t, h, "PUT", "/v1/catalog-sources/test-source", alice, input, 409)
	own := request(t, h, "GET", "/v1/catalog-sources", alice, nil, 200)
	other := request(t, h, "GET", "/v1/catalog-sources", bob, nil, 200)
	if strings.Contains(string(own["sources"]), "source-secret") || string(other["sources"]) != "[]" {
		t.Fatal(own, other)
	}
	var secret []byte
	if err := s.db.QueryRow("SELECT secret FROM catalog_sources WHERE id='test-source'").Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(secret), "source-secret") || len(secret) == 0 {
		t.Fatal("secret not encrypted")
	}
	input["baseRevision"] = 1
	input["deleted"] = true
	delete(input, "credentials")
	request(t, h, "PUT", "/v1/catalog-sources/test-source", alice, input, 200)
	s.db.QueryRow("SELECT secret FROM catalog_sources WHERE id='test-source'").Scan(&secret)
	if len(secret) != 0 {
		t.Fatal("deleted source retained credentials")
	}
	all := request(t, h, "GET", "/v1/catalog-sources", alice, nil, 200)
	var sources []map[string]any
	json.Unmarshal(all["sources"], &sources)
	if len(sources) != 1 || sources[0]["deleted"] != true {
		t.Fatal(sources)
	}
}

func TestCatalogSourceBackupDropsCredentialsAndPasswords(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	request(t, h, "PUT", "/v1/catalog-sources/source", token, map[string]any{"name": "Saved", "url": "https://example.com/opds", "operationId": "save", "credentials": map[string]string{"username": "u", "password": "secret"}}, 200)
	request(t, h, "POST", "/v1/opds/passwords", token, map[string]string{"name": "Reader"}, 201)
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if e := s.Backup(context.Background(), archive); e != nil {
		t.Fatal(e)
	}
	destination := filepath.Join(t.TempDir(), "restore")
	if e := RestoreBackup(archive, destination); e != nil {
		t.Fatal(e)
	}
	restored, e := Open(filepath.Join(destination, "quire.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	var name string
	var secret []byte
	if e = restored.db.QueryRow("SELECT name,secret FROM catalog_sources").Scan(&name, &secret); e != nil || name != "Saved" || len(secret) != 0 {
		t.Fatal(name, len(secret), e)
	}
	for _, table := range []string{"catalog_operations", "opds_passwords"} {
		var count int
		restored.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count)
		if count != 0 {
			t.Fatal(table, count)
		}
	}
}

func TestCatalogSourceEditsRetainCredentialsOnlyWithinOrigin(t *testing.T) {
	s, _ := fixture(t)
	var user string
	if err := s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&user); err != nil {
		t.Fatal(err)
	}
	original := catalogCredentials{Username: "reader", Password: "original"}
	replacement := catalogCredentials{Username: "replacement", Password: "new-password"}
	tests := []struct {
		name, url   string
		credentials *catalogCredentials
		want        *catalogCredentials
	}{
		{"path", "https://example.com/opds/v2", nil, &original},
		{"query", "https://example.com/opds?lang=en", nil, &original},
		{"normalized origin", "https://EXAMPLE.com:443/opds/v2", nil, &original},
		{"different host", "https://other.example/opds", nil, nil},
		{"different port", "https://example.com:8443/opds", nil, nil},
		{"different scheme", "http://example.com/opds", nil, nil},
		{"same origin replacement", "https://example.com/opds/v2", &replacement, &replacement},
		{"cross origin replacement", "https://other.example/opds", &replacement, &replacement},
		{"explicit clear", "https://example.com/opds/v2", &catalogCredentials{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := randomID()
			_, err := s.writeCatalogSource(user, id, catalogSourceWrite{Name: "Books", URL: "https://example.com/opds", OperationID: randomID(), Credentials: &original})
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.writeCatalogSource(user, id, catalogSourceWrite{Name: "Books", URL: tc.url, BaseRevision: 1, OperationID: randomID(), Credentials: tc.credentials})
			if err != nil {
				t.Fatal(err)
			}
			_, got, err := s.catalogSourceSecret(user, id)
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (tc.want == nil) || (got != nil && tc.want != nil && *got != *tc.want) {
				t.Fatalf("credentials presence or value mismatch: got present=%t, want present=%t", got != nil, tc.want != nil)
			}
		})
	}
}
