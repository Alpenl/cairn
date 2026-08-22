package durablework

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type readerLinkQueueStub struct {
	operations []string
	cancelErr  error
	enqueueErr error
	tx         pgx.Tx
}

func (s *readerLinkQueueStub) CancelAllActiveTx(_ context.Context, tx pgx.Tx, id uuid.UUID) error {
	s.tx = tx
	s.operations = append(s.operations, "cancel:"+id.String())
	return s.cancelErr
}

func (s *readerLinkQueueStub) EnqueueTx(_ context.Context, tx pgx.Tx, attempt model.ParseAttempt) error {
	s.tx = tx
	s.operations = append(s.operations, "enqueue:"+attempt.LinkID.String())
	return s.enqueueErr
}

func TestReaderCommandsOwnFeedFeedbackTransaction(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	linkID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM links WHERE id=$1)")).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("(?s)INSERT INTO reader_feed_hides").
		WithArgs("link:" + linkID.String()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	queue := &readerLinkQueueStub{}
	commands := NewReaderCommands(mock, repository.NewPGXReaderVNextRepository(mock), queue)
	result, err := commands.FeedbackFeed(context.Background(), "link:"+linkID.String(), "hide")
	if err != nil {
		t.Fatalf("FeedbackFeed() error = %v", err)
	}
	if result.ItemKey != "link:"+linkID.String() || result.Action != "hide" {
		t.Fatalf("FeedbackFeed() = %+v", result)
	}
	if len(queue.operations) != 0 {
		t.Fatalf("hide queue operations = %v, want none", queue.operations)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestReaderCommandsLifecycleCancelsBeforeEnqueue(t *testing.T) {
	t.Parallel()

	linkID := uuid.New()
	attempt := model.ParseAttempt{LinkID: linkID}
	queue := &readerLinkQueueStub{}
	commands := &ReaderCommands{queue: queue}
	tx := &readerTxIdentity{}

	err := commands.lifecycle(context.Background(), tx, repository.ReaderLinkLifecycleChange{
		LinkID: linkID, ParseAttempt: &attempt,
	})
	if err != nil {
		t.Fatalf("lifecycle() error = %v", err)
	}
	want := []string{"cancel:" + linkID.String(), "enqueue:" + linkID.String()}
	if len(queue.operations) != len(want) || queue.operations[0] != want[0] || queue.operations[1] != want[1] {
		t.Fatalf("lifecycle operations = %v, want %v", queue.operations, want)
	}
	if queue.tx != tx {
		t.Fatal("lifecycle queue calls did not receive the caller-owned transaction")
	}
}

func TestReaderCommandsLifecycleStopsAfterCancelFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("cancel failed")
	linkID := uuid.New()
	attempt := model.ParseAttempt{LinkID: linkID}
	queue := &readerLinkQueueStub{cancelErr: wantErr}
	commands := &ReaderCommands{queue: queue}

	err := commands.lifecycle(context.Background(), &readerTxIdentity{}, repository.ReaderLinkLifecycleChange{
		LinkID: linkID, ParseAttempt: &attempt,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("lifecycle() error = %v, want %v", err, wantErr)
	}
	if len(queue.operations) != 1 {
		t.Fatalf("lifecycle operations after cancel failure = %v", queue.operations)
	}
}

// readerTxIdentity is used only as an opaque pgx.Tx identity. The lifecycle
// callback must forward it but does not call transaction methods itself.
type readerTxIdentity struct{ pgx.Tx }
