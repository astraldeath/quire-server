package server

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestComicArchiveDetectionAndMetadata(t *testing.T) {
	for _, fixture := range []struct{ name, format string }{{"rar4.cbr", "cbr"}, {"rar5.cbr", "cbr"}, {"lzma2.cb7", "cb7"}, {"copy.cb7", "cb7"}, {"lzma.cb7", "cb7"}} {
		t.Run(fixture.name, func(t *testing.T) {
			original, err := os.ReadFile(filepath.Join("testdata", "comics", fixture.name))
			if err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(t.TempDir(), "unknown")
			if err = os.WriteFile(filename, original, 0600); err != nil {
				t.Fatal(err)
			}
			format, err := detectBookFormat(filename)
			if err != nil || format != fixture.format {
				t.Fatalf("detect: %q %v", format, err)
			}
			metadataFile := filename + "." + fixture.format
			os.Rename(filename, metadataFile)
			m := formatMetadata(metadataFile)
			if m.Format != fixture.format || m.Title != "Archive Example" || m.Author != "Ada Example" || m.Series != "Example Series" || m.Cover == "" {
				t.Fatalf("metadata: %+v", m)
			}
		})
	}
}

func TestComicArchivesRejectInvalidContent(t *testing.T) {
	for _, name := range []string{"rar4.cbr", "rar5.cbr", "lzma2.cb7", "copy.cb7"} {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", "comics", name))
			if err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(t.TempDir(), "book")
			for _, data := range [][]byte{b[:8], b[:len(b)/2]} {
				os.WriteFile(filename, data, 0600)
				if _, err := detectBookFormat(filename); err == nil {
					t.Fatal("accepted truncated comic")
				}
			}
		})
	}
	for _, name := range []string{"unsafe.cbr", "encrypted.cb7", "large-dictionary.cb7", "empty.cb7"} {
		if _, err := detectBookFormat(filepath.Join("testdata", "comics", name)); err == nil {
			t.Fatalf("accepted invalid archive %s", name)
		}
	}
}

func TestComicArchiveBudgetsAndPaths(t *testing.T) {
	for _, name := range []string{"../page.png", "/page.png", "a/../page.png", "C:/page.png", "a\\page.png", "a\x00.png"} {
		c := comicInspection{budget: 100, seen: map[string]bool{}}
		if c.member(name, 1, 0, false) == nil {
			t.Fatalf("accepted path %q", name)
		}
	}
	for _, tc := range []struct {
		name      string
		size      uint64
		mode      os.FileMode
		encrypted bool
	}{
		{"page.png", 32<<20 + 1, 0, false}, {"ComicInfo.xml", 1<<20 + 1, 0, false}, {"page.png", 1, os.ModeSymlink, false}, {"page.png", 1, 0, true}, {"other", 513 << 20, 0, false},
	} {
		c := comicInspection{budget: 512 << 20, seen: map[string]bool{}}
		if c.member(tc.name, tc.size, tc.mode, tc.encrypted) == nil {
			t.Fatalf("accepted unsafe member %+v", tc)
		}
	}
	c := comicInspection{budget: 100, seen: map[string]bool{}}
	if c.member("page.png", 60, 0, false) != nil || c.member("page.png", 1, 0, false) == nil || c.member("second.png", 41, 0, false) == nil {
		t.Fatal("duplicate or total budget not enforced")
	}
}

// Fixed layout is represented by package metadata; server storage must retain it.
func TestFixedLayoutEPUBRemainsSupported(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, member := range []struct{ name, content string }{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container" version="1.0"><rootfiles><rootfile full-path="book.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`},
		{"book.opf", `<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:identifier id="id">fixed-test</dc:identifier><dc:title>Fixed Example</dc:title><dc:language>en</dc:language><meta property="rendition:layout">pre-paginated</meta></metadata><manifest><item id="page" href="page.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="page"/></spine></package>`},
		{"page.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>Page</title><meta name="viewport" content="width=600,height=800"/></head><body><p>Fixed page</p></body></html>`},
	} {
		f, err := z.Create(member.name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(member.content))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "fixed.epub")
	os.WriteFile(filename, b.Bytes(), 0600)
	if format, err := detectBookFormat(filename); err != nil || format != "epub" {
		t.Fatalf("fixed layout rejected: %q %v", format, err)
	}
	if m := formatMetadata(filename); m.Title != "Fixed Example" {
		t.Fatalf("fixed metadata: %+v", m)
	}
}
