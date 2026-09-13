package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func epubBytes() []byte {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("mimetype")
	f.Write([]byte("application/epub+zip"))
	f, _ = z.Create("META-INF/container.xml")
	f.Write([]byte(`<container/>`))
	z.Close()
	return b.Bytes()
}
func hashBook(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func TestUploadedFilesArePrivateAndHashChecked(t *testing.T) {
	_, h := fixture(t)
	alice := login(t, h, "alice")
	bob := login(t, h, "bob")
	b := epubBytes()
	id := hashBook(b)
	put := func(id, token string, data []byte, status int) {
		r := httptest.NewRequest("PUT", "/v1/books/"+id+"/file", bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("upload got %d: %s", w.Code, w.Body.String())
		}
	}
	put(strings.Repeat("a", 64), alice, b, 400)
	put(id, alice, b, 201)
	r := httptest.NewRequest("GET", "/v1/books/"+id+"/file", nil)
	r.Header.Set("Authorization", "Bearer "+alice)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), b) {
		t.Fatal("download mismatch")
	}
	request(t, h, "GET", "/v1/books/"+id+"/file", bob, nil, 404)
	request(t, h, "DELETE", "/v1/books/"+id+"/file", bob, nil, 404)
	request(t, h, "DELETE", "/v1/books/"+id+"/file", alice, nil, 204)
	request(t, h, "GET", "/v1/books/"+id+"/file", alice, nil, 404)
}
func TestWatchedFilesAreReadOnlyAndMissingMountDoesNotErase(t *testing.T) {
	s, h := fixture(t)
	root := t.TempDir()
	b := epubBytes()
	path := filepath.Join(root, "Book.epub")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(id); err != nil {
		t.Fatal(err)
	}
	token := login(t, h, "alice")
	request(t, h, "DELETE", "/v1/books/"+hashBook(b)+"/file", token, nil, 403)
	if _, err = os.Stat(path); err != nil {
		t.Fatal("watch modified source")
	}
	if err = os.Rename(root, root+"-offline"); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root + "-offline")
	if s.ScanWatch(id) == nil {
		t.Fatal("missing mount was accepted")
	}
	out := request(t, h, "GET", "/v1/files", token, nil, 200)
	if !bytes.Contains(out["files"], []byte(hashBook(b))) {
		t.Fatal("failed mount erased availability")
	}
	if err = os.Rename(root+"-offline", root); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(id); err != nil {
		t.Fatal(err)
	}
	out = request(t, h, "GET", "/v1/files", token, nil, 200)
	if bytes.Contains(out["files"], []byte(hashBook(b))) {
		t.Fatal("successful scan failed to reconcile source removal")
	}
	if len(syncRequest(t, h, token, 0).Changes) == 0 {
		t.Fatal("watch did not publish metadata")
	}
}
func TestUploadedCopyDeletionKeepsWatchedCopy(t *testing.T) {
	s, h := fixture(t)
	root := t.TempDir()
	b := epubBytes()
	id := hashBook(b)
	if err := os.WriteFile(filepath.Join(root, "Book.epub"), b, 0600); err != nil {
		t.Fatal(err)
	}
	watch, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(watch); err != nil {
		t.Fatal(err)
	}
	token := login(t, h, "alice")
	r := httptest.NewRequest("PUT", "/v1/books/"+id+"/file", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	request(t, h, "DELETE", "/v1/books/"+id+"/file", token, nil, 204)
	out := request(t, h, "GET", "/v1/files", token, nil, 200)
	if !bytes.Contains(out["files"], []byte(`"watched":true`)) || bytes.Contains(out["files"], []byte(`"uploaded":true`)) {
		t.Fatal("watched copy changed")
	}
}
func TestReplacingWatchRootDoesNotReconcileEmptyReplacement(t *testing.T) {
	s, _ := fixture(t)
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if s.ScanWatch(id) == nil {
		t.Fatal("replaced mount was reconciled")
	}
}
func TestRemoveWatchPreservesSourceAndReadingData(t *testing.T) {
	s, h := fixture(t)
	root := t.TempDir()
	path := filepath.Join(root, "Book.epub")
	if err := os.WriteFile(path, epubBytes(), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(id); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveWatch(id); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("source removed")
	}
	w, err := s.Watches()
	if err != nil || len(w) != 0 {
		t.Fatal("watch not removed")
	}
	token := login(t, h, "alice")
	if len(syncRequest(t, h, token, 0).Changes) != 1 {
		t.Fatal("reading metadata removed")
	}
}
