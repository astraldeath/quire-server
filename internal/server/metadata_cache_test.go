package server

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func writeTestEPUB(t *testing.T, path, title string) {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, value := range map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": `<container><rootfiles><rootfile full-path="book.opf"/></rootfiles></container>`,
		"book.opf":               `<package><metadata><title>` + title + `</title></metadata></package>`,
	} {
		f, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataCacheCoalescesAndCopiesVolumes(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c := newMetadataCache(2, 1024, func(string) bookMetadata {
		calls.Add(1)
		close(started)
		<-release
		volume := 5.0
		return bookMetadata{Title: "Book", Volume: &volume}
	})
	var workers sync.WaitGroup
	for i := 0; i < 10; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			m := c.get("immutable-object")
			if m.Title != "Book" || *m.Volume != 5 {
				t.Error("incorrect metadata")
			}
			*m.Volume = 999
		}()
	}
	<-started
	close(release)
	workers.Wait()
	if calls.Load() != 1 {
		t.Fatalf("decoded %d times", calls.Load())
	}
	if *c.get("immutable-object").Volume != 5 {
		t.Fatal("caller mutated cached volume")
	}
}

func TestMetadataCacheEvictsByCountAndBytes(t *testing.T) {
	for _, tc := range []struct {
		name           string
		entries, bytes int
	}{
		{"entries", 1, 1024}, {"bytes", 10, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := newMetadataCache(tc.entries, tc.bytes, func(key string) bookMetadata {
				calls++
				return bookMetadata{Title: key, Cover: string(bytes.Repeat([]byte("x"), 150))}
			})
			c.get("a")
			c.get("b")
			c.get("a")
			if calls != 3 {
				t.Fatalf("cache did not evict: %d decodes", calls)
			}
		})
	}
}

func TestMetadataParsingDoesNotHoldDatabaseConnection(t *testing.T) {
	s, _ := fixture(t)
	var alice, bob string
	s.db.QueryRow("SELECT id FROM users WHERE username='alice'").Scan(&alice)
	s.db.QueryRow("SELECT id FROM users WHERE username='bob'").Scan(&bob)
	s.metadata = newMetadataCache(128, 16<<20, func(path string) bookMetadata {
		if s.db.Stats().InUse != 0 {
			t.Error("metadata decoding holds the only database connection")
		}
		return epubMetadata(path)
	})
	b := epubBytes()
	id, size, err := s.stage(alice, bytes.NewReader(b), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO files VALUES (?,?,'upload','',?)", []any{alice, id, size}},
		{"INSERT INTO libraries VALUES ('test','Test',?)", []any{alice}},
		{"INSERT INTO library_members VALUES ('test',?)", []any{bob}},
	} {
		if _, err := s.db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.sharedSeeds(bob); err != nil {
		t.Fatal(err)
	}
	// Give the watch a separate immutable object so the parser runs again.
	root := t.TempDir()
	writeTestEPUB(t, filepath.Join(root, "Book.epub"), "A watched book")
	watch, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ScanWatch(watch); err != nil {
		t.Fatal(err)
	}
}
