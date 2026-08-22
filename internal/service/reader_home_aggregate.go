package service

import (
	"context"
	"fmt"

	"webtag/internal/model"
)

type ReaderHomeResult struct {
	Today           string
	Summary         string
	Counts          map[string]int
	ContinueReading []model.ReaderFeedItem
	RecentThoughts  []model.ReaderThought
	Todos           []model.ReaderTodo
	Freshness       model.ReaderHomeFreshness
	Partial         bool
	Stale           bool
}

// HomeAggregate is the single service call boundary for the Home surface. The
// repository returns one transactionally coherent result; this method only
// derives the stable Home summary and freshness state. It deliberately does
// not reconcile through ListTodos, because that
// would reopen the multi-read and multi-snapshot behavior this seam replaces.
func (s *ReaderLibraryApplication) HomeAggregate(ctx context.Context) (ReaderHomeResult, error) {
	aggregate, err := s.library.LoadHomeAggregate(ctx)
	if err != nil {
		return ReaderHomeResult{}, mapReaderError(err)
	}

	var counts map[string]int
	if aggregate.Counts != nil {
		counts = make(map[string]int, len(aggregate.Counts))
		for key, value := range aggregate.Counts {
			counts[key] = value
		}
	}

	today := s.now().Format("2006-01-02")
	pendingCount, hasPending := homeCount(counts, "pending", "inbox")
	todoCount, hasTodos := homeCount(counts, "todos")
	summary := ""
	if hasPending && hasTodos {
		summary = formatHomeSummary(today, pendingCount, todoCount)
	}
	freshness, partial, stale := homeFreshnessState(aggregate.Freshness)

	return ReaderHomeResult{
		Today:           today,
		Summary:         summary,
		Counts:          counts,
		ContinueReading: aggregate.ContinueReading,
		RecentThoughts:  aggregate.RecentThoughts,
		Todos:           aggregate.Todos,
		Freshness:       freshness,
		Partial:         partial,
		Stale:           stale,
	}, nil
}

func homeCount(counts map[string]int, keys ...string) (int, bool) {
	for _, key := range keys {
		if value, ok := counts[key]; ok {
			return value, true
		}
	}
	return 0, false
}

func homeFreshnessState(freshness model.ReaderHomeFreshness) (model.ReaderHomeFreshness, bool, bool) {
	switch freshness {
	case model.ReaderHomeFreshnessFresh:
		return model.ReaderHomeFreshnessFresh, false, false
	case model.ReaderHomeFreshnessStale:
		return model.ReaderHomeFreshnessStale, false, true
	case model.ReaderHomeFreshnessPartial:
		return model.ReaderHomeFreshnessPartial, true, false
	default:
		// An absent or future repository state is not evidence of freshness.
		// Keep the compatibility stale flag false because an unverified result
		// is not necessarily old; partial is the closed wire state for it.
		return model.ReaderHomeFreshnessPartial, true, false
	}
}

func formatHomeSummary(today string, pendingCount, todoCount int) string {
	return fmt.Sprintf("%s：收件箱 %d 条，待办 %d 项", today, pendingCount, todoCount)
}
