package model

type ReaderHomeFreshness string

const (
	ReaderHomeFreshnessFresh   ReaderHomeFreshness = "fresh"
	ReaderHomeFreshnessPartial ReaderHomeFreshness = "partial"
	ReaderHomeFreshnessStale   ReaderHomeFreshness = "stale"
)

// ReaderHomeAggregate is one transactionally coherent Home projection.
type ReaderHomeAggregate struct {
	Freshness       ReaderHomeFreshness
	Counts          map[string]int
	ContinueReading []ReaderFeedItem
	RecentThoughts  []ReaderThought
	Todos           []ReaderTodo
}
