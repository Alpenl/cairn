package service

import (
	"testing"

	"webtag/internal/model"
)

func TestDecideFinalLibraryUsesAnalyzerForUnlockedLink(t *testing.T) {
	t.Parallel()

	for _, kind := range []model.LibraryKind{model.LibraryKindReading, model.LibraryKindSite} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			got := decideFinalLibrary(kind, nil, false)
			if got.Kind != kind || got.Locked {
				t.Fatalf("decision = %+v, want %s unlocked", got, kind)
			}
		})
	}
}

func TestDecideFinalLibraryPreservesLockedSelection(t *testing.T) {
	t.Parallel()

	reading := model.LibraryKindReading
	got := decideFinalLibrary(model.LibraryKindSite, &reading, true)
	if got.Kind != model.LibraryKindReading || !got.Locked {
		t.Fatalf("decision = %+v, want locked reading", got)
	}
}

func TestDecideFinalLibraryNormalizesUnknownAnalyzerKind(t *testing.T) {
	t.Parallel()

	got := decideFinalLibrary(model.LibraryKind("archive"), nil, false)
	if got.Kind != model.LibraryKindReading || got.Locked {
		t.Fatalf("decision = %+v, want unlocked reading", got)
	}
}

func TestRequestedKindForLinkUsesOnlyLockedSelection(t *testing.T) {
	t.Parallel()

	reading := model.LibraryKindReading
	if got := requestedKindForLink(parseInputForTest(&model.Link{LibraryKind: &reading})); got != model.RequestedLibraryKindAuto {
		t.Fatalf("unlocked requested kind = %q, want auto", got)
	}
	if got := requestedKindForLink(parseInputForTest(&model.Link{LibraryKind: &reading, LibraryKindLocked: true})); got != model.RequestedLibraryKindReading {
		t.Fatalf("locked requested kind = %q, want reading", got)
	}
}
