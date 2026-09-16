package server

import (
	"path/filepath"
	"testing"
)

func TestExplicitTitleVolumeFallback(t *testing.T) {
	for _, tc := range []struct {
		title, filename, series string
		volume                  float64
	}{
		{"That Time I Got Reincarnated as a Slime, Vol. 5", "", "That Time I Got Reincarnated as a Slime", 5},
		{"Novel Vol5", "", "Novel", 5},
		{"Novel Volume 2.5: Side Story", "", "Novel", 2.5},
		{"Novel", "Novel_Vol5.epub", "Novel", 5},
		{"Novel Vol. 2", "Novel Vol. 9.epub", "Novel", 2},
		{"Novel 2025", "", "", 0},
		{"Novel Chapters 1-100", "", "", 0},
		{"Novel Vol. 1-5", "", "", 0},
		{"Novel Vol. 1 – 5", "", "", 0},
		{"Novel Vol. 1 Vol. 2", "", "", 0},
	} {
		t.Run(tc.title+tc.filename, func(t *testing.T) {
			m := inferMetadataVolume(bookMetadata{Title: tc.title}, tc.filename)
			if m.Series != tc.series || tc.volume == 0 && m.Volume != nil || tc.volume != 0 && (m.Volume == nil || *m.Volume != tc.volume) {
				t.Fatalf("incorrect inference: %+v", m)
			}
		})
	}
	volume := 9.0
	m := inferMetadataVolume(bookMetadata{Title: "Title Vol5", Series: "Publisher series", Volume: &volume}, "")
	if m.Series != "Publisher series" || *m.Volume != 9 {
		t.Fatal("explicit metadata overwritten")
	}
	path := filepath.Join(t.TempDir(), "hash.epub")
	writeTestEPUB(t, path, "Novel Vol5")
	m = epubMetadata(path)
	if m.Series != "Novel" || m.Volume == nil || *m.Volume != 5 {
		t.Fatalf("EPUB title inference missing: %+v", m)
	}
}
