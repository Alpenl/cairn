package service

import "webtag/internal/model"

type finalLibraryDecision struct {
	Kind   model.LibraryKind
	Locked bool
}

// decideFinalLibrary preserves a fixed collection choice and otherwise accepts
// the analyzer's result. The repository compares the kind/lock pair again in
// the terminal transaction, so a conversion racing this decision cannot be
// overwritten by stale parse work.
func decideFinalLibrary(analyzed model.LibraryKind, current *model.LibraryKind, locked bool) finalLibraryDecision {
	if locked && current != nil {
		return finalLibraryDecision{Kind: normalizeLibraryKind(*current), Locked: true}
	}
	return finalLibraryDecision{Kind: normalizeLibraryKind(analyzed)}
}

func normalizeLibraryKind(kind model.LibraryKind) model.LibraryKind {
	if kind == model.LibraryKindSite {
		return model.LibraryKindSite
	}
	return model.LibraryKindReading
}
