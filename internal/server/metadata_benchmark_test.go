package server

import (
	"os"
	"path/filepath"
	"testing"
)

// Opt-in, read-only benchmark against a local EPUB corpus; no account or server required.
func BenchmarkEPUBMetadataCorpus(b *testing.B) {
	root := os.Getenv("QUIRE_BENCH_EPUB_DIR")
	if root == "" {
		b.Skip("set QUIRE_BENCH_EPUB_DIR to a local EPUB directory")
	}
	paths, err := filepath.Glob(filepath.Join(root, "*.epub"))
	if err != nil || len(paths) == 0 {
		b.Fatal("no EPUB corpus", err)
	}
	b.Run("uncached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, path := range paths {
				epubMetadata(path)
			}
		}
	})
	c := newMetadataCache(128, 16<<20, epubMetadata)
	for _, path := range paths {
		c.get(path)
	}
	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, path := range paths {
				if _, err := os.Stat(path); err != nil {
					b.Fatal(err)
				}
				c.get(path)
			}
		}
	})
}
