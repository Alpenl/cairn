package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/errsafe"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestPipelineFailurePersistenceSurvivesCancelledWorkContext(t *testing.T) {
	attempt := model.ParseAttempt{LinkID: uuid.New(), Generation: 4, ExpectedMetadataRevision: 2}
	called := false
	links := &repotest.ObservableLinkStore{
		MarkParseFailedFunc: func(ctx context.Context, got model.ParseAttempt, message string) error {
			called = true
			if ctx.Err() != nil {
				t.Fatalf("cleanup context is already cancelled: %v", ctx.Err())
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > parseFailurePersistTimeout {
				t.Fatalf("cleanup deadline = %v, want within %s", deadline, parseFailurePersistTimeout)
			}
			if got != attempt || message == "" {
				t.Fatalf("failure payload = %#v %q", got, message)
			}
			return nil
		},
	}
	pipeline := &ParsePipeline{links: links}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pipeline.fail(ctx, attempt, errors.New("fetch timed out"))
	if !called {
		t.Fatal("MarkParseFailed was not called")
	}
	if !errors.Is(err, errsafe.ErrAlreadyPersisted) {
		t.Fatalf("fail() error = %v, want ErrAlreadyPersisted", err)
	}
}

func TestRecordDiscardTreatsAlreadyTerminalAttemptAsReconciled(t *testing.T) {
	t.Parallel()
	attempt := model.ParseAttempt{LinkID: uuid.New(), Generation: 1, ExpectedMetadataRevision: 1}
	links := &repotest.ObservableLinkStore{
		MarkParseFailedFunc: func(context.Context, model.ParseAttempt, string) error {
			return repository.ErrParseAttemptNotRunnable
		},
	}
	pipeline := &ParsePipeline{links: links}

	if err := pipeline.RecordDiscard(context.Background(), attempt, errors.New("river job completed")); err != nil {
		t.Fatalf("RecordDiscard() error = %v, want nil for terminal attempt", err)
	}
}

func TestRecordDiscardReturnsProjectionFailure(t *testing.T) {
	t.Parallel()
	projectionErr := errors.New("database unavailable")
	links := &repotest.ObservableLinkStore{
		MarkParseFailedFunc: func(context.Context, model.ParseAttempt, string) error {
			return projectionErr
		},
	}
	pipeline := &ParsePipeline{links: links}

	err := pipeline.RecordDiscard(context.Background(), model.ParseAttempt{LinkID: uuid.New(), Generation: 1, ExpectedMetadataRevision: 1}, errors.New("river job discarded"))
	if !errors.Is(err, projectionErr) {
		t.Fatalf("RecordDiscard() error = %v, want %v", err, projectionErr)
	}
}

var _ repository.ParseStateStore = (*repotest.ObservableLinkStore)(nil)
