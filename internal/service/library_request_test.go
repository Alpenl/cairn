package service

import (
	"errors"
	"testing"

	"webtag/internal/httperr"
	"webtag/internal/model"
)

func TestNormalizeRequestedLibraryKind(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want model.RequestedLibraryKind
	}{
		{"", model.RequestedLibraryKindAuto},
		{" AUTO ", model.RequestedLibraryKindAuto},
		{"reading", model.RequestedLibraryKindReading},
		{"SITE", model.RequestedLibraryKindSite},
	} {
		got, err := normalizeRequestedLibraryKind(tt.raw)
		if err != nil || got != tt.want {
			t.Fatalf("normalizeRequestedLibraryKind(%q) = %q, %v; want %q, nil", tt.raw, got, err, tt.want)
		}
	}

	_, err := normalizeRequestedLibraryKind("archive")
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) || statusErr.HTTPErrorCode() != httperr.CodeInvalidRequestedLibraryKind {
		t.Fatalf("invalid requested kind error = %v, want %s", err, httperr.CodeInvalidRequestedLibraryKind)
	}
}
