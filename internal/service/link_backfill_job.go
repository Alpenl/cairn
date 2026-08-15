package service

import (
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	// LinkEmbeddingBackfillJobKind is the durable maintenance job that repairs
	// missing or stale link content vectors. The job carries no link payload;
	// the worker scans the installation's links table with a bounded cursor.
	LinkEmbeddingBackfillJobKind        = "link_embedding_backfill"
	LinkEmbeddingBackfillJobMaxAttempts = 3
	LinkEmbeddingBackfillJobTimeout     = 15 * time.Minute
	LinkEmbeddingBackfillInterval       = 15 * time.Minute
)

// LinkEmbeddingBackfillJobArgs is intentionally empty: one periodic job owns
// the bounded embedding scan for this installation.
type LinkEmbeddingBackfillJobArgs struct{}

func (LinkEmbeddingBackfillJobArgs) Kind() string { return LinkEmbeddingBackfillJobKind }

func (LinkEmbeddingBackfillJobArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: LinkEmbeddingBackfillJobMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}
