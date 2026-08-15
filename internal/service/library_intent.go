package service

import "webtag/internal/model"

type requestedLibraryIntent struct {
	Kind   model.RequestedLibraryKind
	Source model.RequestedLibraryKindSource
}

func userRequestedLibraryIntent(kind model.RequestedLibraryKind) requestedLibraryIntent {
	return normalizeRequestedLibraryIntent(kind, model.RequestedLibraryKindSourceUser)
}

func automaticRequestedLibraryIntent(kind model.RequestedLibraryKind) requestedLibraryIntent {
	return normalizeRequestedLibraryIntent(kind, model.RequestedLibraryKindSourceAuto)
}

func normalizeRequestedLibraryIntent(kind model.RequestedLibraryKind, source model.RequestedLibraryKindSource) requestedLibraryIntent {
	switch kind {
	case model.RequestedLibraryKindReading, model.RequestedLibraryKindSite:
		if source == model.RequestedLibraryKindSourceAuto {
			return requestedLibraryIntent{Kind: kind, Source: source}
		}
		return requestedLibraryIntent{Kind: kind, Source: model.RequestedLibraryKindSourceUser}
	default:
		return requestedLibraryIntent{Kind: model.RequestedLibraryKindAuto, Source: model.RequestedLibraryKindSourceAuto}
	}
}

// resolveRequestedLibraryIntent is the single precedence rule used by every
// capture and requeue entry point. Automatic traffic cannot erase a committed
// user choice; a later explicit user choice always replaces the previous one.
func resolveRequestedLibraryIntent(current, incoming requestedLibraryIntent) requestedLibraryIntent {
	current = normalizeRequestedLibraryIntent(current.Kind, current.Source)
	incoming = normalizeRequestedLibraryIntent(incoming.Kind, incoming.Source)
	if incoming.Source == model.RequestedLibraryKindSourceUser {
		return incoming
	}
	if current.Source == model.RequestedLibraryKindSourceUser {
		return current
	}
	if incoming.Kind != model.RequestedLibraryKindAuto {
		return incoming
	}
	return current
}
