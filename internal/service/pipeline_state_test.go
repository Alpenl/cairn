package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/errsafe"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestPipelineFailurePersistenceSurvivesCancelledWorkContext(t *testing.T) {
	linkID, jobID := uuid.New(), uuid.New()
	called := false
	links := &repotest.ObservableLinkStore{
		MarkParseFailedFunc: func(ctx context.Context, gotLinkID, gotJobID uuid.UUID, message string) error {
			called = true
			if ctx.Err() != nil {
				t.Fatalf("cleanup context is already cancelled: %v", ctx.Err())
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > parseFailurePersistTimeout {
				t.Fatalf("cleanup deadline = %v, want within %s", deadline, parseFailurePersistTimeout)
			}
			if gotLinkID != linkID || gotJobID != jobID || message == "" {
				t.Fatalf("failure payload = %s/%s %q", gotLinkID, gotJobID, message)
			}
			return nil
		},
	}
	pipeline := &ParsePipeline{links: links}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pipeline.fail(ctx, linkID, jobID, errors.New("fetch timed out"))
	if !called {
		t.Fatal("MarkParseFailed was not called")
	}
	if !errors.Is(err, errsafe.ErrAlreadyPersisted) {
		t.Fatalf("fail() error = %v, want ErrAlreadyPersisted", err)
	}
}

func TestRecordDiscardTreatsAlreadyTerminalAttemptAsReconciled(t *testing.T) {
	t.Parallel()
	linkID, jobID := uuid.New(), uuid.New()
	links := &repotest.ObservableLinkStore{
		MarkParseFailedFunc: func(context.Context, uuid.UUID, uuid.UUID, string) error {
			return repository.ErrParseJobNotRunnable
		},
	}
	pipeline := &ParsePipeline{links: links}

	if err := pipeline.RecordDiscard(context.Background(), linkID, jobID, errors.New("river job completed")); err != nil {
		t.Fatalf("RecordDiscard() error = %v, want nil for terminal attempt", err)
	}
}

func TestRecordDiscardReturnsProjectionFailure(t *testing.T) {
	t.Parallel()
	projectionErr := errors.New("database unavailable")
	links := &repotest.ObservableLinkStore{
		MarkParseFailedFunc: func(context.Context, uuid.UUID, uuid.UUID, string) error {
			return projectionErr
		},
	}
	pipeline := &ParsePipeline{links: links}

	err := pipeline.RecordDiscard(context.Background(), uuid.New(), uuid.New(), errors.New("river job discarded"))
	if !errors.Is(err, projectionErr) {
		t.Fatalf("RecordDiscard() error = %v, want %v", err, projectionErr)
	}
}

var _ repository.ParseStateStore = (*repotest.ObservableLinkStore)(nil)
