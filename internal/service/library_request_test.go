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

func TestRequireSiteLibraryWriteRejectsOnlyExplicitSite(t *testing.T) {
	for _, requested := range []model.RequestedLibraryKind{model.RequestedLibraryKindAuto, model.RequestedLibraryKindReading} {
		if err := requireSiteLibraryWrite(requested, true); err != nil {
			t.Fatalf("%q should remain allowed: %v", requested, err)
		}
	}
	err := requireSiteLibraryWrite(model.RequestedLibraryKindSite, true)
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != 503 || statusErr.HTTPErrorCode() != "site_library_write_disabled" {
		t.Fatalf("site gate error = %v", err)
	}
}
