package service

import (
	"context"
	"errors"
	"fmt"

	"webtag/internal/dto"
	"webtag/internal/repository"
)

// ErrReaderHomeAggregateUnavailable means the service was constructed with a
// Reader store that has not installed the authoritative Home query seam.
// Keeping this explicit prevents silently falling back to the old collection
// of independent reads and presenting an unverified partial snapshot.
var ErrReaderHomeAggregateUnavailable = errors.New("reader home aggregate unavailable")

type readerHomeAggregateReader interface {
	LoadHomeAggregate(context.Context) (repository.ReaderHomeAggregate, error)
}

// HomeAggregate is the single service call boundary for the Home surface. The
// repository returns one transactionally coherent result; this method only
// maps domain projections to the existing Home DTO and derives the display
// summary. It deliberately does not reconcile through ListTodos, because that
// would reopen the multi-read and multi-snapshot behavior this seam replaces.
func (s *ReaderVNextService) HomeAggregate(ctx context.Context) (dto.ReaderHomeResponse, error) {
	reader, ok := s.store.(readerHomeAggregateReader)
	if !ok {
		return dto.ReaderHomeResponse{}, ErrReaderHomeAggregateUnavailable
	}
	aggregate, err := reader.LoadHomeAggregate(ctx)
	if err != nil {
		return dto.ReaderHomeResponse{}, mapReaderError(err)
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
	freshness, partial, stale := homeFreshnessWireState(aggregate.Freshness)

	var reading []dto.ReaderFeedItemResponse
	if aggregate.ContinueReading != nil {
		reading = make([]dto.ReaderFeedItemResponse, 0, len(aggregate.ContinueReading))
		for _, item := range aggregate.ContinueReading {
			reading = append(reading, feedItemResponse(item))
		}
	}
	var recent []dto.ReaderThoughtResponse
	if aggregate.RecentThoughts != nil {
		recent = make([]dto.ReaderThoughtResponse, 0, len(aggregate.RecentThoughts))
		for _, thought := range aggregate.RecentThoughts {
			recent = append(recent, thoughtResponse(thought))
		}
	}
	var todos []dto.ReaderTodoResponse
	if aggregate.Todos != nil {
		todos = make([]dto.ReaderTodoResponse, 0, len(aggregate.Todos))
		for _, todo := range aggregate.Todos {
			todos = append(todos, todoResponse(todo))
		}
	}

	return dto.ReaderHomeResponse{
		Today:           today,
		Summary:         summary,
		Counts:          counts,
		ContinueReading: reading,
		RecentThoughts:  recent,
		Todos:           todos,
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

func homeFreshnessWireState(freshness repository.ReaderHomeFreshness) (string, bool, bool) {
	switch freshness {
	case repository.ReaderHomeFreshnessFresh:
		return dto.ReaderHomeFreshnessFresh, false, false
	case repository.ReaderHomeFreshnessStale:
		return dto.ReaderHomeFreshnessStale, false, true
	case repository.ReaderHomeFreshness(dto.ReaderHomeFreshnessPartial):
		return dto.ReaderHomeFreshnessPartial, true, false
	default:
		// An absent or future repository state is not evidence of freshness.
		// Keep the compatibility stale flag false because an unverified result
		// is not necessarily old; partial is the closed wire state for it.
		return dto.ReaderHomeFreshnessPartial, true, false
	}
}

func formatHomeSummary(today string, pendingCount, todoCount int) string {
	return fmt.Sprintf("%s：收件箱 %d 条，待办 %d 项", today, pendingCount, todoCount)
}
