package server

import (
	"encoding/json"
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
