//go:build unix

package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"syscall"
	"testing"
)

func TestScanDiagnosticsPersistNonregularWhenSupported(t *testing.T) {
	s, h := fixture(t)
	if err := s.Promote("alice"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.epub"), 0600); err != nil {
		t.Skipf("nonregular integration unavailable: %v", err)
	}
	id, err := s.AddWatch("alice", root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ScanWatch(id); err != nil {
		t.Fatal(err)
	}
	raw := rawRequest(t, h, "GET", "/v1/admin/scans", login(t, h, "alice"), http.StatusOK)
	var scans []struct {
		ID                  string            `json:"id"`
		Skipped             int               `json:"skipped"`
		SkippedFiles        []scanSkippedFile `json:"skippedFiles"`
		OmittedSkippedFiles int               `json:"omittedSkippedFiles"`
	}
	if err = json.Unmarshal(raw, &scans); err != nil || len(scans) != 1 {
		t.Fatalf("scan response %s: %v", raw, err)
	}
	if scans[0].ID != id || scans[0].Skipped != 1 || scans[0].OmittedSkippedFiles != 0 || len(scans[0].SkippedFiles) != 1 || scans[0].SkippedFiles[0] != (scanSkippedFile{Path: "pipe.epub", Reason: "not-regular"}) {
		t.Fatalf("nonregular diagnostic was not persisted: %+v", scans[0])
	}
}
