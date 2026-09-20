package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfiguredUploadBoundary(t *testing.T) {
	b := epubBytes()
	s, err := OpenWithOptions(filepath.Join(t.TempDir(), "quire.db"), StoreOptions{MaxUploadBytes: int64(len(b))})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if id, n, err := s.stage(strings.Repeat("a", 64), bytes.NewReader(b), ""); err != nil || n != int64(len(b)) || id != hashBook(b) {
		t.Fatalf("exact-bound upload: id=%s bytes=%d error=%v", id, n, err)
	}
	if _, _, err := s.stage(strings.Repeat("b", 64), bytes.NewReader(append(b, 0)), ""); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("expected size error, got %v", err)
	}
}

func TestUploadOptionsValidation(t *testing.T) {
	for _, limit := range []int64{0, -1, MaxStoredBookBytes + 1, math.MaxInt64} {
		if s, err := OpenWithOptions(filepath.Join(t.TempDir(), "quire.db"), StoreOptions{MaxUploadBytes: limit}); err == nil {
			s.Close()
			t.Fatalf("accepted %d", limit)
		}
	}
	s, _ := fixture(t)
	if s.maxUploadBytes != DefaultMaxUploadBytes {
		t.Fatal("wrong default")
	}
}

func uploadRequest(t *testing.T, h http.Handler, method, path, token string, body io.Reader, unknown bool, status int) map[string]json.RawMessage {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	if unknown {
		r.ContentLength = -1
	}
	r.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServerAssignedUploadAndDiscoveryLimits(t *testing.T) {
	s, h := fixture(t)
	b := epubBytes()
	s.maxUploadBytes = int64(len(b))
	alice, bob := login(t, h, "alice"), login(t, h, "bob")
	uploadRequest(t, h, "POST", "/v1/books/files", "", bytes.NewReader(b), false, 401)
	request(t, h, "POST", "/v1/books/files", alice, nil, 415)
	for _, unknown := range []bool{false, true} {
		out := uploadRequest(t, h, "POST", "/v1/books/files", alice, bytes.NewReader(b), unknown, 201)
		var id string
		var size int64
		json.Unmarshal(out["bookId"], &id)
		json.Unmarshal(out["size"], &size)
		if id != hashBook(b) || size != int64(len(b)) {
			t.Fatal("incorrect upload result", out)
		}
		uploadRequest(t, h, "POST", "/v1/books/files", alice, bytes.NewReader(append(b, 0)), unknown, 413)
	}
	uploadRequest(t, h, "PUT", "/v1/books/"+strings.Repeat("b", 64)+"/file", alice, bytes.NewReader(b), false, 400)
	uploadRequest(t, h, "PUT", "/v1/books/"+hashBook(b)+"/file", alice, bytes.NewReader(append(b, 0)), true, 413)
	request(t, h, "GET", "/v1/books/"+hashBook(b)+"/file", bob, nil, 404)
	request(t, h, "GET", "/v1/books/"+hashBook(b)+"/metadata", bob, nil, 404)
	request(t, h, "DELETE", "/v1/books/"+hashBook(b)+"/file", bob, nil, 404)
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM files").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	d := request(t, h, "GET", "/.well-known/quire", "", nil, 200)
	var limits struct{ MaxUploadBytes, MaxDownloadBytes int64 }
	if err := json.Unmarshal(d["limits"], &limits); err != nil || limits.MaxUploadBytes != int64(len(b)) || limits.MaxDownloadBytes != MaxStoredBookBytes {
		t.Fatal("wrong limits", d, err)
	}
	if !bytes.Contains(d["capabilities"], []byte(`"server-assigned-upload"`)) {
		t.Fatal("missing capability")
	}
}

func TestSharedServerAssignedUpload(t *testing.T) {
	s, h := fixture(t)
	if err := s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	alice, bob := login(t, h, "alice"), login(t, h, "bob")
	l := request(t, h, "POST", "/v1/admin/libraries", alice, map[string]string{"name": "Shared"}, 201)
	var library string
	json.Unmarshal(l["id"], &library)
	path := "/v1/admin/libraries/" + library + "/books"
	b := epubBytes()
	s.maxUploadBytes = int64(len(b))
	uploadRequest(t, h, "POST", path, bob, bytes.NewReader(b), false, 403)
	uploadRequest(t, h, "POST", "/v1/admin/libraries/missing/books", alice, bytes.NewReader(b), false, 404)
	uploadRequest(t, h, "POST", path, alice, bytes.NewReader(append(b, 0)), true, 413)
	out := uploadRequest(t, h, "POST", path, alice, bytes.NewReader(b), true, 201)
	if string(out["bookId"]) != `"`+hashBook(b)+`"` || string(out["size"]) != fmt.Sprint(len(b)) {
		t.Fatal(out)
	}
	request(t, h, "GET", "/v1/books/"+hashBook(b)+"/file", bob, nil, 404)
	var owner string
	if err := s.db.QueryRow("SELECT owner FROM libraries WHERE id=?", library).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM files WHERE user_id=? AND book_id=?", owner, hashBook(b)).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

type interruptedUpload struct{}

func (interruptedUpload) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

type databaseProbeUpload struct {
	store *Store
	body  io.Reader
}

func (r databaseProbeUpload) Read(p []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// The store has one SQLite connection. A transaction held across a body
	// read would make this independent query time out.
	var count int
	if err := r.store.db.QueryRowContext(ctx, "SELECT count(*) FROM files").Scan(&count); err != nil {
		return 0, err
	}
	return r.body.Read(p)
}

func TestUploadBodyDoesNotHoldDatabaseTransaction(t *testing.T) {
	s, h := fixture(t)
	reader := databaseProbeUpload{s, bytes.NewReader(epubBytes())}
	uploadRequest(t, h, "POST", "/v1/books/files", login(t, h, "alice"), reader, true, 201)
}

func TestInterruptedUploadCleansStaging(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	uploadRequest(t, h, "POST", "/v1/books/files", token, io.MultiReader(bytes.NewReader(epubBytes()[:20]), interruptedUpload{}), true, 500)
	files, err := filepath.Glob(filepath.Join(s.data, "objects", "*", "*"))
	if err != nil || len(files) != 0 {
		t.Fatal(files, err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM files").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestWatchedFileUploadLimit(t *testing.T) {
	s, _ := fixture(t)
	root := t.TempDir()
	b := epubBytes()
	if err := os.WriteFile(filepath.Join(root, "book.epub"), b, 0600); err != nil {
		t.Fatal(err)
	}
	id, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	s.maxUploadBytes = int64(len(b) - 1)
	if err = s.ScanWatch(id); err != nil {
		t.Fatalf("watch limit scan: %v", err)
	}
	var imported, existing, skipped, omitted int
	var message, details string
	if err = s.db.QueryRow("SELECT error,imported,existing,skipped,skipped_files,omitted_skipped_files FROM scan_status WHERE watch_id=?", id).Scan(&message, &imported, &existing, &skipped, &details, &omitted); err != nil {
		t.Fatal(err)
	}
	if message != "" || imported != 0 || existing != 0 || skipped != 1 || omitted != 0 || details != `[{"path":"book.epub","reason":"too-large"}]` {
		t.Fatalf("watch limit diagnostics: error=%q imported=%d existing=%d skipped=%d details=%s omitted=%d", message, imported, existing, skipped, details, omitted)
	}
	files, _ := filepath.Glob(filepath.Join(s.data, "objects", "*", "*"))
	if len(files) != 0 {
		t.Fatal(files)
	}
	var stored int
	if err = s.db.QueryRow("SELECT count(*) FROM files").Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("oversized watch import persisted: count=%d error=%v", stored, err)
	}
}

// A stored archive has no decompression amplification. Construct it by streaming
// valid BMP entries with deterministic noisy pixels, retaining only a copy buffer.
func writeLargeStoredCBZ(t *testing.T, filename string) int64 {
	t.Helper()
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	const pixels = 2048 * 2048 * 3
	header := make([]byte, 54)
	copy(header, "BM")
	binary.LittleEndian.PutUint32(header[2:], 54+pixels)
	binary.LittleEndian.PutUint32(header[10:], 54)
	binary.LittleEndian.PutUint32(header[14:], 40)
	binary.LittleEndian.PutUint32(header[18:], 2048)
	binary.LittleEndian.PutUint32(header[22:], 2048)
	binary.LittleEndian.PutUint16(header[26:], 1)
	binary.LittleEndian.PutUint16(header[28:], 24)
	binary.LittleEndian.PutUint32(header[34:], pixels)
	noise := rand.New(rand.NewSource(42))
	for i := 0; i < 12; i++ {
		w, e := z.CreateHeader(&zip.FileHeader{Name: fmt.Sprintf("%03d.bmp", i), Method: zip.Store})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write(header); e != nil {
			t.Fatal(e)
		}
		if _, e = io.CopyN(w, noise, pixels); e != nil {
			t.Fatal(e)
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

type zeroUploadReader struct{}

func (zeroUploadReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestLargeStoredCBZUploadAndRestoreBelowUploadLimit(t *testing.T) {
	s, h := fixture(t)
	token := login(t, h, "alice")
	filename := filepath.Join(t.TempDir(), "large.cbz")
	size := writeLargeStoredCBZ(t, filename)
	if size <= 128<<20 {
		t.Fatal(size)
	}
	f, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	out := uploadRequest(t, h, "POST", "/v1/books/files", token, f, true, 201)
	f.Close()
	var id string
	json.Unmarshal(out["bookId"], &id)
	if string(out["size"]) != fmt.Sprint(size) {
		t.Fatal(out)
	}
	s.maxUploadBytes = 1
	archive := filepath.Join(t.TempDir(), "backup.zip")
	if err = s.Backup(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restore")
	if err = RestoreBackup(archive, destination); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenWithOptions(filepath.Join(destination, "quire.db"), StoreOptions{MaxUploadBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var owner string
	var stored int64
	if err = restored.db.QueryRow("SELECT user_id,size FROM files WHERE book_id=?", id).Scan(&owner, &stored); err != nil || stored != size {
		t.Fatal(stored, err)
	}
	if format, err := detectBookFormat(restored.objectPath(owner, id)); err != nil || format != "cbz" {
		t.Fatal(format, err)
	}
}

func TestUploadArchiveExpansionAndImageBounds(t *testing.T) {
	for _, test := range []struct {
		name   string
		method uint16
		size   int64
	}{{"compressed-bomb.bin", zip.Deflate, 513 << 20}, {"oversized.png", zip.Store, (32 << 20) + 1}} {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "invalid.zip")
			f, err := os.Create(filename)
			if err != nil {
				t.Fatal(err)
			}
			z := zip.NewWriter(f)
			imageEntry, err := z.Create("page.png")
			if err != nil {
				t.Fatal(err)
			}
			if err = png.Encode(imageEntry, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
				t.Fatal(err)
			}
			w, err := z.CreateHeader(&zip.FileHeader{Name: test.name, Method: test.method})
			if err != nil {
				t.Fatal(err)
			}
			var prefix bytes.Buffer
			if strings.HasSuffix(test.name, ".png") {
				if err = png.Encode(&prefix, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
					t.Fatal(err)
				}
				if _, err = w.Write(prefix.Bytes()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = io.CopyN(w, zeroUploadReader{}, test.size-int64(prefix.Len())); err != nil {
				t.Fatal(err)
			}
			if err = z.Close(); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if _, err = detectBookFormat(filename); !errors.Is(err, ErrInvalid) {
				t.Fatal("accepted unsafe archive", err)
			}
		})
	}
}

func TestUploadArchiveAggregateExpansionBounds(t *testing.T) {
	for _, test := range []struct {
		stored  int64
		entries []uint64
		valid   bool
	}{
		{600 << 20, []uint64{300 << 20, 300 << 20}, true},
		{1, []uint64{512 << 20, 1}, true},
		{1, []uint64{512 << 20, 2}, false},
		{1, []uint64{math.MaxUint64}, false},
		{MaxStoredBookBytes, []uint64{uint64(MaxStoredBookBytes) + (512 << 20)}, true},
		{MaxStoredBookBytes, []uint64{uint64(MaxStoredBookBytes) + (512 << 20) + 1}, false},
		{-1, nil, false},
		{MaxStoredBookBytes + 1, nil, false},
	} {
		z := &zip.ReadCloser{}
		for i, size := range test.entries {
			z.File = append(z.File, &zip.File{FileHeader: zip.FileHeader{Name: fmt.Sprintf("entry-%d", i), UncompressedSize64: size}})
		}
		if err := validateArchive(z, test.stored); (err == nil) != test.valid {
			t.Fatalf("stored %d entries %v valid %v: %v", test.stored, test.entries, test.valid, err)
		}
	}
}
