package server

import (
	"archive/zip"
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestWatchedMetadataAndCoverWithoutDownloadingBook(t *testing.T) {
	s, h := fixture(t)
	var data, cover bytes.Buffer
	png.Encode(&cover, image.NewRGBA(image.Rect(0, 0, 20, 30)))
	z := zip.NewWriter(&data)
	for name, body := range map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": `<container><rootfiles><rootfile full-path="OPS/book.opf"/></rootfiles></container>`,
		"OPS/book.opf":           `<package><metadata><title>A real title</title><creator>Author Name</creator><meta name="cover" content="cover"/></metadata><manifest><item id="cover" href="images/cover.png" media-type="image/png"/></manifest></package>`,
		"OPS/images/cover.png":   cover.String(),
	} {
		f, _ := z.Create(name)
		f.Write([]byte(body))
	}
	z.Close()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "filename.epub"), data.Bytes(), 0600)
	watch, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(watch); err != nil {
		t.Fatal(err)
	}
	alice, bob := login(t, h, "alice"), login(t, h, "bob")
	id := hashBook(data.Bytes())
	result := request(t, h, "GET", "/v1/books/"+id+"/metadata", alice, nil, 200)
	if !bytes.Contains(result["cover"], []byte("data:image/jpeg;base64,")) {
		t.Fatal("missing thumbnail")
	}
	changes := syncRequest(t, h, alice, 0).Changes
	if len(changes) != 1 || !bytes.Contains(changes[0].Candidates[0].Value, []byte("Author Name")) {
		t.Fatal("EPUB metadata not seeded")
	}
	request(t, h, "GET", "/v1/books/"+id+"/metadata", bob, nil, 404)
}
