package app

import (
	"bytes"
	"testing"
)

func TestAdminConceptMergesHTMLLoadsEmbeddedAsset(t *testing.T) {
	data, err := AdminConceptMergesHTML()
	if err != nil {
		t.Fatalf("AdminConceptMergesHTML() error = %v", err)
	}
	if len(data) == 0 {
		t.Fatal("AdminConceptMergesHTML() returned empty content")
	}
	for _, want := range [][]byte{
		[]byte("Concept Merge Review"),
		[]byte("/api/admin/concept-merges"),
		[]byte("data-action=\"approve\""),
		[]byte("data-action=\"reject\""),
	} {
		if !bytes.Contains(data, want) {
			t.Errorf("AdminConceptMergesHTML() missing marker %q", want)
		}
	}
}
