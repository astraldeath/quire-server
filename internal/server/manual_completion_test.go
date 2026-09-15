package server

import (
	"encoding/json"
	"testing"
)

func TestManualCompletionPosition(t *testing.T) {
	for _, tc := range []struct {
		name, kind, value string
		valid             bool
	}{
		{"manual completion", "position", `{"cfi":"","fraction":1,"section":""}`, true},
		{"missing cfi", "position", `{"fraction":1}`, false},
		{"null cfi", "position", `{"cfi":null,"fraction":1}`, false},
		{"partial without location", "position", `{"cfi":"","fraction":0.5}`, false},
		{"unread without location", "position", `{"cfi":"","fraction":0}`, false},
		{"threshold without location", "position", `{"cfi":"","fraction":0.999}`, false},
		{"missing fraction", "position", `{"cfi":""}`, false},
		{"annotation without location", "annotation", `{"kind":"highlight","cfi":"","text":"word"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOperation(Operation{ID: "manual", BookID: testBook, Kind: tc.kind, RecordID: "default", Value: json.RawMessage(tc.value)})
			if (err == nil) != tc.valid {
				t.Fatalf("validation error = %v, want valid %v", err, tc.valid)
			}
		})
	}
}
