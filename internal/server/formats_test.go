package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func formatFixtures() map[string][]byte {
	var img bytes.Buffer
	png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	archive := func(name string, data []byte) []byte {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		f, _ := z.Create(name)
		f.Write(data)
		z.Close()
		return b.Bytes()
	}
	fb2 := []byte(`<?xml version="1.0"?><FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0"><description><title-info><book-title>Example</book-title><author><first-name>Ada</first-name><last-name>Lovelace</last-name></author></title-info></description><body><section><p>Hello</p></section></body></FictionBook>`)
	return map[string][]byte{"epub": epubBytes(), "cbz": archive("001.png", img.Bytes()), "fb2": fb2, "fbz": archive("book.fb2", fb2), "mobi": mobiFixture(6), "azw3": mobiFixture(8)}
}

func mobiFixture(version uint32) []byte {
	b := make([]byte, 400)
	copy(b[60:68], "BOOKMOBI")
	binary.BigEndian.PutUint16(b[76:78], 2)
	binary.BigEndian.PutUint32(b[78:82], 96)
	binary.BigEndian.PutUint32(b[86:90], 360)
	r := b[96:360]
	binary.BigEndian.PutUint16(r[:2], 1)
	binary.BigEndian.PutUint16(r[8:10], 1)
	copy(r[16:20], "MOBI")
	binary.BigEndian.PutUint32(r[20:24], 232)
	binary.BigEndian.PutUint32(r[36:40], version)
	copy(b[360:], "<html><body>Example</body></html>")
	return b
}
func TestFormatDetectionRejectsUnsafeContent(t *testing.T) {
	for _, version := range []uint32{6, 8} {
		b := mobiFixture(version)
		name := filepath.Join(t.TempDir(), "book")
		os.WriteFile(name, b, 0600)
		format, err := detectBookFormat(name)
		if err != nil || (version == 6 && format != "mobi") || (version == 8 && format != "azw3") {
			t.Fatal(format, err)
		}
		b[96+13] = 1
		os.WriteFile(name, b, 0600)
		if _, err = detectBookFormat(name); err == nil {
			t.Fatal("accepted encrypted MOBI")
		}
	}
	for _, data := range [][]byte{[]byte("not a book"), []byte(`<FictionBook><body/></FictionBook><extra/>`), []byte(`<!DOCTYPE FictionBook><FictionBook><body/></FictionBook>`), []byte(`<FictionBook/>`)} {
		name := filepath.Join(t.TempDir(), "book.cbz")
		os.WriteFile(name, data, 0600)
		if _, err := detectBookFormat(name); err == nil {
			t.Fatal("accepted invalid content")
		}
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	f, _ := z.Create("../book.fb2")
	f.Write(formatFixtures()["fb2"])
	z.Close()
	name := filepath.Join(t.TempDir(), "unsafe")
	os.WriteFile(name, b.Bytes(), 0600)
	if _, err := detectBookFormat(name); err == nil {
		t.Fatal("accepted traversal")
	}
}
func TestWatchedFormatsSeedFolders(t *testing.T) {
	s, h := fixture(t)
	root := t.TempDir()
	folder := filepath.Join(root, "Fiction", "Classics")
	os.MkdirAll(folder, 0700)
	for format, data := range formatFixtures() {
		os.WriteFile(filepath.Join(folder, "Example."+format), data, 0600)
	}
	watch, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(watch); err != nil {
		t.Fatal(err)
	}
	changes := syncRequest(t, h, login(t, h, "alice"), 0).Changes
	if len(changes) != len(formatFixtures()) {
		t.Fatal(len(changes))
	}
	for _, c := range changes {
		var v map[string]any
		json.Unmarshal(c.Candidates[0].Value, &v)
		if v["folder"] != "Fiction/Classics" || !validBookFormat(v["format"].(string)) {
			t.Fatal(v)
		}
	}
}
func TestLegacyMetadataEditPreservesFolder(t *testing.T) {
	s, h := fixture(t)
	user, _ := s.authenticate(login(t, h, "alice"))
	id := hashBook(nil)
	send := func(opid string, rev int64, value string) {
		t.Helper()
		_, err := s.Sync(context.Background(), user, SyncRequest{Operations: []Operation{{ID: opid, BookID: id, Kind: "book", RecordID: "default", BaseRevision: rev, Value: json.RawMessage(value)}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	send("first", 0, `{"title":"Book","folder":"Fiction/Classics","format":"cbz"}`)
	send("legacy", 1, `{"title":"Renamed"}`)
	var raw string
	s.db.QueryRow("SELECT candidates FROM records WHERE user_id=? AND book_id=?", user, id).Scan(&raw)
	var candidates []Candidate
	json.Unmarshal([]byte(raw), &candidates)
	var value map[string]any
	json.Unmarshal(candidates[0].Value, &value)
	if value["folder"] != "Fiction/Classics" || value["format"] != "cbz" {
		t.Fatal(value)
	}
	send("clear", 2, `{"title":"Renamed","folder":""}`)
	s.db.QueryRow("SELECT candidates FROM records WHERE user_id=? AND book_id=?", user, id).Scan(&raw)
	json.Unmarshal([]byte(raw), &candidates)
	json.Unmarshal(candidates[0].Value, &value)
	if value["folder"] != "" {
		t.Fatal(value)
	}
}

func TestFormatsUploadDownloadBackup(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	for format, data := range formatFixtures() {
		t.Run(format, func(t *testing.T) {
			id := hashBook(data)
			r := httptest.NewRequest("PUT", "/v1/books/"+id+"/file", bytes.NewReader(data))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 201 {
				t.Fatalf("upload: %d %s", w.Code, w.Body.String())
			}
			r = httptest.NewRequest("GET", "/v1/books/"+id+"/file", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w = httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
				t.Fatal("original file changed")
			}
			if w.Header().Get("Content-Disposition") != `attachment; filename="book.`+format+`"` {
				t.Fatal(w.Header())
			}
		})
	}
	archive := filepath.Join(t.TempDir(), "books.zip")
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
	var user string
	restored.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&user)
	for format, original := range formatFixtures() {
		data, err := os.ReadFile(restored.objectPath(user, hashBook(original)))
		if err != nil || !bytes.Equal(data, original) {
			t.Fatalf("restore %s: %v", format, err)
		}
	}
}

func TestFormatMetadata(t *testing.T) {
	for format, data := range formatFixtures() {
		filename := filepath.Join(t.TempDir(), "book."+format)
		os.WriteFile(filename, data, 0600)
		meta := formatMetadata(filename)
		if meta.Format != format {
			t.Fatal(meta)
		}
		if (format == "fb2" || format == "fbz") && (meta.Title != "Example" || meta.Author != "Ada Lovelace") {
			t.Fatal(meta)
		}
		if format == "cbz" && meta.Cover == "" {
			t.Fatal("missing comic thumbnail")
		}
	}
}

func TestBookFolderAndFormatValidation(t *testing.T) {
	for _, value := range []string{`{"title":"Book","folder":null}`, `{"title":"Book","format":null}`, `{"title":"Book","format":""}`} {
		if validateOperation(Operation{ID: "test", BookID: hashBook(nil), Kind: "book", RecordID: "default", Value: json.RawMessage(value)}) == nil {
			t.Fatalf("accepted null optional metadata: %s", value)
		}
	}
	for _, folder := range []string{"", "Fiction", "Fiction/Science Fiction"} {
		value, _ := json.Marshal(map[string]string{"title": "Book", "folder": folder, "format": "cbz"})
		if err := validateOperation(Operation{ID: "test", BookID: hashBook(nil), Kind: "book", RecordID: "default", Value: value}); err != nil {
			t.Fatalf("%q: %v", folder, err)
		}
	}
	for _, folder := range []string{"/absolute", "../escape", "a/../b", "a\\b", "a//b", "a/", "a\x00b"} {
		value, _ := json.Marshal(map[string]string{"title": "Book", "folder": folder})
		if validateOperation(Operation{ID: "test", BookID: hashBook(nil), Kind: "book", RecordID: "default", Value: value}) == nil {
			t.Fatalf("accepted %q", folder)
		}
	}
}
