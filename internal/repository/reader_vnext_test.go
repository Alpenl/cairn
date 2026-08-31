package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

var readerThoughtOperationCreatedAt = time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)

func TestNewPGXReaderVNextRepositoryRequiresTransactionBeginner(t *testing.T) {
	t.Parallel()

	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("NewPGXReaderVNextRepository() did not panic for a nil transaction beginner")
		}
	}()
	_ = NewPGXReaderVNextRepository(nil)
}

func TestReaderRepositoryWithTxCommitsAndRollsBackSurfaceMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		surface  string
		success  func(context.Context, *PGXReaderVNextRepository) error
		commit   func(pgxmock.PgxPoolIface)
		failure  func(context.Context, *PGXReaderVNextRepository) error
		rollback func(pgxmock.PgxPoolIface, error)
	}{
		{
			surface: "thought",
			success: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				op := derivedThoughtOpForTest("tx-thought-duplicate")
				_, err := repo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{op})
				return err
			},
			commit: func(mock pgxmock.PgxPoolIface) {
				op := derivedThoughtOpForTest("tx-thought-duplicate")
				mock.ExpectQuery("(?s)SELECT sequence,created_at,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload,recovery_of,expected_winner_key.*FROM reader_thought_ops").
					WithArgs(op.OpID).
					WillReturnRows(mock.NewRows([]string{
						"sequence", "created_at", "device_id", "logical_clock", "operation_kind", "annotation_id",
						"host_kind", "host_id", "target", "payload", "recovery_of", "expected_winner_key",
					}).AddRow(
						int64(11), readerThoughtOperationCreatedAt, op.DeviceID, op.LogicalClock, op.OperationKind, op.AnnotationID,
						op.HostKind, op.HostID, []byte(op.Target), []byte(op.Payload), nil, nil,
					))
				mock.ExpectQuery("(?s)SELECT id\\s+FROM reader_thoughts\\s+WHERE id=\\$1\\s+FOR UPDATE").
					WithArgs(op.AnnotationID).
					WillReturnRows(mock.NewRows([]string{"id"}).AddRow(op.AnnotationID))
				mock.ExpectExec("(?s)SELECT pg_advisory_xact_lock\\(hashtextextended").
					WithArgs(op.AnnotationID).
					WillReturnResult(pgxmock.NewResult("SELECT", 1))
				mock.ExpectQuery("(?s)SELECT winner_logical_clock,winner_device_id,winner_op_id\\s+FROM reader_thoughts\\s+WHERE id=\\$1").
					WithArgs(op.AnnotationID).
					WillReturnRows(mock.NewRows([]string{"winner_logical_clock", "winner_device_id", "winner_op_id"}).
						AddRow(op.LogicalClock, op.DeviceID, op.OpID))
			},
			failure: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				op := derivedThoughtOpForTest("tx-thought-failure")
				_, err := repo.AppendThoughtOps(ctx, []model.ReaderThoughtOp{op})
				return err
			},
			rollback: func(mock pgxmock.PgxPoolIface, sentinel error) {
				op := derivedThoughtOpForTest("tx-thought-failure")
				mock.ExpectQuery("(?s)SELECT sequence,created_at,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload,recovery_of,expected_winner_key.*FROM reader_thought_ops").
					WithArgs(op.OpID).
					WillReturnError(sentinel)
			},
		},
		{
			surface: "note",
			success: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				noteID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
				_, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
					NoteID: noteID, ExpectedDraftRevision: 2, ExpectedPublishedRevision: 1,
				})
				return err
			},
			commit: func(mock pgxmock.PgxPoolIface) {
				noteID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
				expectPublishedNoteLock(
					mock,
					noteID,
					readerNoteRowForTest(noteID, "Reader note", "published", 1, nil, 2, readerThoughtOperationCreatedAt),
				)
			},
			failure: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				noteID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
				_, err := repo.PublishNote(ctx, model.ReaderNotePublishCommand{
					NoteID: noteID, ExpectedDraftRevision: 2, ExpectedPublishedRevision: 1,
				})
				return err
			},
			rollback: func(mock pgxmock.PgxPoolIface, sentinel error) {
				noteID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
				mock.ExpectQuery("(?s)SELECT " + regexp.QuoteMeta(readerNoteColumns) + " FROM reader_notes.*FOR UPDATE").
					WithArgs(noteID).
					WillReturnError(sentinel)
			},
		},
		{
			surface: "inbox",
			success: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				return repo.RestoreInbox(ctx, uuid.MustParse("33333333-3333-3333-3333-333333333333"))
			},
			commit: func(mock pgxmock.PgxPoolIface) {
				inboxID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
				mock.ExpectQuery("(?s)SELECT " + regexp.QuoteMeta(readerInboxColumns) + ".*FROM reader_inbox.*WHERE id=\\$1.*FOR UPDATE").
					WithArgs(inboxID).
					WillReturnRows(mock.NewRows(readerInboxColumnsForTest()).
						AddRow(readerInboxRowForTest(inboxID, nil, false, nil, readerThoughtOperationCreatedAt)...))
			},
			failure: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				return repo.RestoreInbox(ctx, uuid.MustParse("44444444-4444-4444-4444-444444444444"))
			},
			rollback: func(mock pgxmock.PgxPoolIface, sentinel error) {
				inboxID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
				mock.ExpectQuery("(?s)SELECT " + regexp.QuoteMeta(readerInboxColumns) + ".*FROM reader_inbox.*WHERE id=\\$1.*FOR UPDATE").
					WithArgs(inboxID).
					WillReturnError(sentinel)
			},
		},
		{
			surface: "todo",
			success: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				todoID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
				text := "changed"
				_, err := repo.PatchTodo(ctx, model.ReaderTodoPatch{ID: todoID, Text: &text})
				return err
			},
			commit: func(mock pgxmock.PgxPoolIface) {
				todoID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
				mock.ExpectQuery("SELECT origin_kind,host_revision FROM reader_todos WHERE id=\\$1 AND deleted_at IS NULL FOR UPDATE").
					WithArgs(todoID).
					WillReturnRows(mock.NewRows([]string{"origin_kind", "host_revision"}).AddRow("standalone", int64(0)))
				mock.ExpectQuery("(?s)UPDATE reader_todos SET.*RETURNING "+regexp.QuoteMeta(readerTodoColumns)).
					WithArgs(pgxmock.AnyArg(), false, (*time.Time)(nil), (*bool)(nil), todoID).
					WillReturnRows(mock.NewRows(readerTodoColumnsForTest()).
						AddRow(readerTodoRowForTest(todoID, "standalone", false, 0)...))
			},
			failure: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				todoID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
				text := "changed"
				_, err := repo.PatchTodo(ctx, model.ReaderTodoPatch{ID: todoID, Text: &text})
				return err
			},
			rollback: func(mock pgxmock.PgxPoolIface, sentinel error) {
				todoID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
				mock.ExpectQuery("SELECT origin_kind,host_revision FROM reader_todos WHERE id=\\$1 AND deleted_at IS NULL FOR UPDATE").
					WithArgs(todoID).
					WillReturnError(sentinel)
			},
		},
		{
			surface: "library",
			success: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				title := "title"
				summary := "summary"
				_, err := repo.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
					LinkID: uuid.MustParse("77777777-7777-7777-7777-777777777777"), ExpectedRevision: 3,
					Title: &title, Summary: &summary, Tags: []string{"tag"},
				})
				return err
			},
			commit: func(mock pgxmock.PgxPoolIface) {
				linkID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
				mock.ExpectQuery(readerMetadataUpdateQueryPattern).
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), []string{"tag"}, linkID, int64(3), model.LinkMetadataMaxRevision).
					WillReturnRows(mock.NewRows([]string{"found", "metadata_revision", "tags_changed", "changed", "tuple_changed"}).
						AddRow(true, int64(3), false, false, false))
			},
			failure: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				title := "title"
				summary := "summary"
				_, err := repo.UpdateLinkMetadata(ctx, model.ReaderLinkMetadataPatch{
					LinkID: uuid.MustParse("88888888-8888-8888-8888-888888888888"), ExpectedRevision: 3,
					Title: &title, Summary: &summary, Tags: []string{"tag"},
				})
				return err
			},
			rollback: func(mock pgxmock.PgxPoolIface, sentinel error) {
				linkID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
				mock.ExpectQuery(readerMetadataUpdateQueryPattern).
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), []string{"tag"}, linkID, int64(3), model.LinkMetadataMaxRevision).
					WillReturnError(sentinel)
			},
		},
		{
			surface: "host",
			success: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				_, err := repo.RestoreHost(ctx, model.ReaderHostNote, uuid.MustParse("99999999-9999-9999-9999-999999999999"))
				return err
			},
			commit: func(mock pgxmock.PgxPoolIface) {
				noteID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
				mock.ExpectQuery("SELECT deleted_at,published_content,published_revision FROM reader_notes WHERE id=\\$1 FOR UPDATE").
					WithArgs(noteID).
					WillReturnRows(mock.NewRows([]string{"deleted_at", "published_content", "published_revision"}).
						AddRow(nil, "body", int64(1)))
			},
			failure: func(ctx context.Context, repo *PGXReaderVNextRepository) error {
				_, err := repo.RestoreHost(ctx, model.ReaderHostNote, uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
				return err
			},
			rollback: func(mock pgxmock.PgxPoolIface, sentinel error) {
				noteID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
				mock.ExpectQuery("SELECT deleted_at,published_content,published_revision FROM reader_notes WHERE id=\\$1 FOR UPDATE").
					WithArgs(noteID).
					WillReturnError(sentinel)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.surface+"/commit", func(t *testing.T) {
			t.Parallel()
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()
			mock.ExpectBegin()
			test.commit(mock)
			mock.ExpectCommit()

			err = test.success(context.Background(), NewPGXReaderVNextRepository(mock))
			if err != nil {
				t.Fatalf("%s commit path error = %v", test.surface, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})

		t.Run(test.surface+"/rollback", func(t *testing.T) {
			t.Parallel()
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()
			mock.ExpectBegin()
			sentinel := errors.New(test.surface + " surface failed")
			test.rollback(mock, sentinel)
			mock.ExpectRollback()

			err = test.failure(context.Background(), NewPGXReaderVNextRepository(mock))
			if !errors.Is(err, sentinel) {
				t.Fatalf("%s rollback path error = %v, want %v", test.surface, err, sentinel)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func derivedThoughtOpForTest(opID string) model.ReaderThoughtOp {
	return model.ReaderThoughtOp{
		OpID:          opID,
		DeviceID:      "reader-test",
		OperationKind: "update",
		AnnotationID:  "thought-derived",
		HostKind:      "note",
		HostID:        "note-derived",
		Target:        json.RawMessage(`{"kind":"note","host_id":"note-derived","version":{"note_revision":1}}`),
		Payload:       json.RawMessage(`{"body":"derived","quote":{"exact":"quote"},"source":"user"}`),
	}
}

func expectDerivedThoughtClock(mock pgxmock.PgxPoolIface, annotationID, opID string, winnerClock int64) {
	mock.ExpectQuery("(?s)SELECT logical_clock.*FROM reader_thought_ops.*op_id=\\$1").
		WithArgs(opID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)SELECT id.*FROM reader_thoughts.*id=\\$1.*FOR UPDATE").
		WithArgs(annotationID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(annotationID))
	mock.ExpectExec("(?s)SELECT pg_advisory_xact_lock\\(hashtextextended").
		WithArgs(annotationID).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery("(?s)SELECT winner_logical_clock.*FROM reader_thoughts.*FOR UPDATE").
		WithArgs(annotationID).
		WillReturnRows(mock.NewRows([]string{"winner_logical_clock"}).AddRow(winnerClock))
}

func TestLockReaderThoughtOpsUsesCanonicalRowThenAdvisoryOrder(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	for _, annotationID := range []string{"thought-a", "thought-z"} {
		mock.ExpectQuery("(?s)SELECT id.*FROM reader_thoughts.*id=\\$1.*FOR UPDATE").
			WithArgs(annotationID).
			WillReturnRows(mock.NewRows([]string{"id"}).AddRow(annotationID))
		mock.ExpectExec("(?s)SELECT pg_advisory_xact_lock\\(hashtextextended").
			WithArgs(annotationID).
			WillReturnResult(pgxmock.NewResult("SELECT", 1))
	}

	err = lockReaderThoughtOps(context.Background(), mock, []model.ReaderThoughtOp{
		{AnnotationID: "thought-z"},
		{AnnotationID: "thought-a"},
	})
	if err != nil {
		t.Fatalf("lockReaderThoughtOps() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectThoughtEventPreviousWinner(
	mock pgxmock.PgxPoolIface,
	annotationID, hostKind, hostID string,
	sequence, logicalClock int64,
) {
	mock.ExpectQuery("(?s)SELECT operation.sequence,operation.op_id,operation.device_id,operation.logical_clock,operation.operation_kind,operation.annotation_id,operation.host_kind,operation.host_id,operation.target,operation.payload,operation.recovery_of,operation.expected_winner_key,operation.created_at FROM reader_thoughts").
		WithArgs(annotationID).
		WillReturnRows(mock.NewRows([]string{
			"sequence", "op_id", "device_id", "logical_clock", "operation_kind",
			"annotation_id", "host_kind", "host_id", "target", "payload",
			"recovery_of", "expected_winner_key", "created_at",
		}).AddRow(
			sequence, "previous-"+annotationID, "reader-previous", logicalClock, "update",
			annotationID, hostKind, hostID, []byte(`{"kind":"saved-content"}`),
			[]byte(`{"body":"previous","quote":{"exact":"previous"}}`), nil, nil,
			time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		))
}

func expectThoughtSupersessionEvent(
	mock pgxmock.PgxPoolIface,
	annotationID string,
	loserSequence, winnerSequence int64,
) {
	mock.ExpectExec("(?s)INSERT INTO reader_thought_supersession_events").
		WithArgs(
			annotationID,
			loserSequence,
			winnerSequence,
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func expectThoughtLifecycleAppend(
	mock pgxmock.PgxPoolIface,
	thoughtID, hostKind, hostID, action, reason string,
	winnerClock, sequence int64,
) string {
	opID := readerThoughtLifecycleOpID(action, reason, &model.ReaderThought{
		ID:           thoughtID,
		LastSequence: winnerClock,
	})
	expectDerivedThoughtClock(mock, thoughtID, opID, winnerClock)
	mock.ExpectQuery("(?s)INSERT INTO reader_thought_ops.*RETURNING sequence").
		WithArgs(
			opID,
			"reader-lifecycle",
			"update",
			thoughtID,
			hostKind,
			hostID,
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			winnerClock+1,
		).
		WillReturnRows(mock.NewRows([]string{"sequence", "created_at"}).AddRow(sequence, readerThoughtOperationCreatedAt))
	expectThoughtEventPreviousWinner(mock, thoughtID, hostKind, hostID, winnerClock, winnerClock)
	if hostKind == "link" {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS(SELECT 1 FROM links WHERE id=$1 AND deleted_at IS NULL)")).
			WithArgs(uuid.MustParse(hostID)).
			WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	}
	mock.ExpectExec("(?s)INSERT INTO reader_thoughts.*winner_logical_clock.*ON CONFLICT").
		WithArgs(
			thoughtID,
			hostKind,
			hostID,
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			pgxmock.AnyArg(),
			false,
			sequence,
			winnerClock+1,
			"reader-lifecycle",
			opID,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	expectThoughtTodoProjectionRefresh(mock, thoughtID)
	expectThoughtSupersessionEvent(mock, thoughtID, winnerClock, sequence)
	return opID
}

func expectMarkThoughtLifecycle(
	mock pgxmock.PgxPoolIface,
	thoughtID string,
	row []any,
	hostKind, hostID, reason string,
	winnerClock, sequence int64,
) string {
	mock.ExpectQuery("(?s)SELECT " + regexp.QuoteMeta(readerThoughtColumns) + ".*FROM reader_thoughts.*id=\\$1.*FOR UPDATE").
		WithArgs(thoughtID).
		WillReturnRows(mock.NewRows(readerThoughtSyncColumnsForTest()).AddRow(row...))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT reason FROM reader_thought_tombstones WHERE thought_id=$1")).
		WithArgs(thoughtID).
		WillReturnError(pgx.ErrNoRows)
	return expectThoughtLifecycleAppend(
		mock,
		thoughtID,
		hostKind,
		hostID,
		"tombstone",
		reason,
		winnerClock,
		sequence,
	)
}

func TestAppendDerivedThoughtOpAllocatesFirstClock(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	op := derivedThoughtOpForTest("derived-first")
	mock.ExpectQuery("(?s)SELECT logical_clock.*FROM reader_thought_ops.*op_id=\\$1").
		WithArgs(op.OpID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)SELECT id.*FROM reader_thoughts.*id=\\$1.*FOR UPDATE").
		WithArgs(op.AnnotationID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec("(?s)SELECT pg_advisory_xact_lock\\(hashtextextended").
		WithArgs(op.AnnotationID).
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectQuery("(?s)SELECT winner_logical_clock.*FROM reader_thoughts.*FOR UPDATE").
		WithArgs(op.AnnotationID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)INSERT INTO reader_thought_ops.*RETURNING sequence").
		WithArgs(
			op.OpID,
			op.DeviceID,
			op.OperationKind,
			op.AnnotationID,
			op.HostKind,
			op.HostID,
			[]byte(op.Target),
			[]byte(op.Payload),
			int64(1),
		).
		WillReturnRows(mock.NewRows([]string{"sequence", "created_at"}).AddRow(int64(11), readerThoughtOperationCreatedAt))

	repo := NewPGXReaderVNextRepository(mock)
	allocated, sequence, duplicate, err := repo.appendDerivedThoughtOp(context.Background(), mock, op)
	if err != nil {
		t.Fatalf("appendDerivedThoughtOp() error = %v", err)
	}
	if allocated.LogicalClock != 1 || sequence != 11 || duplicate {
		t.Fatalf("appendDerivedThoughtOp() = clock %d sequence %d duplicate %v", allocated.LogicalClock, sequence, duplicate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendDerivedThoughtOpAdvancesWinnerAndReusesDuplicateClock(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	op := derivedThoughtOpForTest("derived-existing")
	expectDerivedThoughtClock(mock, op.AnnotationID, op.OpID, 37)
	mock.ExpectQuery("(?s)INSERT INTO reader_thought_ops.*RETURNING sequence").
		WithArgs(
			op.OpID,
			op.DeviceID,
			op.OperationKind,
			op.AnnotationID,
			op.HostKind,
			op.HostID,
			[]byte(op.Target),
			[]byte(op.Payload),
			int64(38),
		).
		WillReturnRows(mock.NewRows([]string{"sequence", "created_at"}).AddRow(int64(12), readerThoughtOperationCreatedAt))

	repo := NewPGXReaderVNextRepository(mock)
	allocated, sequence, duplicate, err := repo.appendDerivedThoughtOp(context.Background(), mock, op)
	if err != nil || allocated.LogicalClock != 38 || sequence != 12 || duplicate {
		t.Fatalf("first append = op %+v sequence %d duplicate %v error %v", allocated, sequence, duplicate, err)
	}

	mock.ExpectQuery("(?s)SELECT logical_clock.*FROM reader_thought_ops.*op_id=\\$1").
		WithArgs(op.OpID).
		WillReturnRows(mock.NewRows([]string{"logical_clock"}).AddRow(int64(38)))
	mock.ExpectQuery("(?s)INSERT INTO reader_thought_ops.*RETURNING sequence").
		WithArgs(
			op.OpID,
			op.DeviceID,
			op.OperationKind,
			op.AnnotationID,
			op.HostKind,
			op.HostID,
			[]byte(op.Target),
			[]byte(op.Payload),
			int64(38),
		).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)SELECT sequence,created_at,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload.*FROM reader_thought_ops").
		WithArgs(op.OpID).
		WillReturnRows(mock.NewRows([]string{
			"sequence", "created_at", "device_id", "logical_clock", "operation_kind", "annotation_id",
			"host_kind", "host_id", "target", "payload", "recovery_of", "expected_winner_key",
		}).AddRow(
			int64(12), readerThoughtOperationCreatedAt, op.DeviceID, int64(38), op.OperationKind, op.AnnotationID,
			op.HostKind, op.HostID, []byte(op.Target), []byte(op.Payload), nil, nil,
		))

	replayed, replaySequence, replayDuplicate, err := repo.appendDerivedThoughtOp(context.Background(), mock, op)
	if err != nil || replayed.LogicalClock != 38 || replaySequence != 12 || !replayDuplicate {
		t.Fatalf("duplicate append = op %+v sequence %d duplicate %v error %v", replayed, replaySequence, replayDuplicate, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendDerivedThoughtOpFailsWhenClockIsExhausted(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	op := derivedThoughtOpForTest("derived-exhausted")
	expectDerivedThoughtClock(mock, op.AnnotationID, op.OpID, model.ReaderThoughtMaxLogicalClock)
	repo := NewPGXReaderVNextRepository(mock)
	_, _, _, err = repo.appendDerivedThoughtOp(context.Background(), mock, op)
	if !errors.Is(err, ErrReaderThoughtClockExhausted) {
		t.Fatalf("appendDerivedThoughtOp() error = %v, want ErrReaderThoughtClockExhausted", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateThoughtRecoveryUsesCurrentWinnerCAS(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	op := derivedThoughtOpForTest("recovery-cas")
	op.RecoveryOf = &model.ReaderThoughtVersionKey{LogicalClock: 4, DeviceID: "loser-device", OpID: "loser-op"}
	op.ExpectedWinnerKey = &model.ReaderThoughtVersionKey{LogicalClock: 8, DeviceID: "winner-device", OpID: "winner-op"}
	mock.ExpectQuery(`(?s)SELECT EXISTS\(.*reader_thought_supersession_events.*loser\.logical_clock=\$2`).
		WithArgs(op.AnnotationID, int64(4), "loser-device", "loser-op").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("(?s)SELECT winner_logical_clock,winner_device_id,winner_op_id.*FOR UPDATE").
		WithArgs(op.AnnotationID).
		WillReturnRows(mock.NewRows([]string{"winner_logical_clock", "winner_device_id", "winner_op_id"}).
			AddRow(int64(9), "third-device", "third-op"))

	repo := NewPGXReaderVNextRepository(mock)
	err = repo.validateThoughtRecovery(context.Background(), mock, op)
	if !errors.Is(err, ErrReaderThoughtRecoveryConflict) {
		t.Fatalf("validateThoughtRecovery() error = %v, want ErrReaderThoughtRecoveryConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateThoughtRecoveryRejectsUnknownLoserBeforeWinnerCAS(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	op := derivedThoughtOpForTest("unknown-recovery")
	op.RecoveryOf = &model.ReaderThoughtVersionKey{LogicalClock: 4, DeviceID: "loser-device", OpID: "loser-op"}
	op.ExpectedWinnerKey = &model.ReaderThoughtVersionKey{LogicalClock: 8, DeviceID: "winner-device", OpID: "winner-op"}
	mock.ExpectQuery(`(?s)SELECT EXISTS\(.*reader_thought_supersession_events.*loser\.logical_clock=\$2`).
		WithArgs(op.AnnotationID, int64(4), "loser-device", "loser-op").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))

	repo := NewPGXReaderVNextRepository(mock)
	err = repo.validateThoughtRecovery(context.Background(), mock, op)
	if !errors.Is(err, ErrInvalidReaderThought) {
		t.Fatalf("validateThoughtRecovery() error = %v, want ErrInvalidReaderThought", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendThoughtOpRejectsCrossKindDuplicateRecoveryProvenance(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	op := derivedThoughtOpForTest("recovery-provenance-collision")
	op.LogicalClock = 8
	mock.ExpectQuery("(?s)INSERT INTO reader_thought_ops.*RETURNING sequence").
		WithArgs(
			op.OpID,
			op.DeviceID,
			op.OperationKind,
			op.AnnotationID,
			op.HostKind,
			op.HostID,
			[]byte(op.Target),
			[]byte(op.Payload),
			op.LogicalClock,
		).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("(?s)SELECT sequence,created_at,device_id,logical_clock,operation_kind,annotation_id,host_kind,host_id,target,payload,recovery_of,expected_winner_key.*FROM reader_thought_ops").
		WithArgs(op.OpID).
		WillReturnRows(mock.NewRows([]string{
			"sequence", "created_at", "device_id", "logical_clock", "operation_kind", "annotation_id",
			"host_kind", "host_id", "target", "payload", "recovery_of", "expected_winner_key",
		}).AddRow(
			int64(12), readerThoughtOperationCreatedAt, op.DeviceID, op.LogicalClock, op.OperationKind, op.AnnotationID,
			op.HostKind, op.HostID, []byte(op.Target), []byte(op.Payload),
			[]byte(`{"logical_clock":4,"device_id":"loser-device","op_id":"loser-op"}`),
			[]byte(`{"logical_clock":7,"device_id":"winner-device","op_id":"winner-op"}`),
		))

	repo := NewPGXReaderVNextRepository(mock)
	_, _, duplicate, err := repo.appendThoughtOp(context.Background(), mock, op)
	if !errors.Is(err, ErrReaderThoughtOpConflict) || duplicate {
		t.Fatalf("appendThoughtOp() = duplicate %v error %v, want provenance conflict", duplicate, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestThoughtVersionKeyFromJSONFailsClosedForNonCanonicalProvenance(t *testing.T) {
	t.Parallel()
	for _, raw := range [][]byte{
		[]byte(`{"logical_clock":4,"device_id":"device","op_id":"op","extra":true}`),
		[]byte(`{"logical_clock":4,"device_id":"device"}`),
		[]byte(`[]`),
	} {
		if _, ok := thoughtVersionKeyFromJSON(raw); ok {
			t.Fatalf("thoughtVersionKeyFromJSON(%s) accepted noncanonical provenance", raw)
		}
	}
}

func readerThoughtSyncColumnsForTest() []string {
	return []string{
		"id", "host_kind", "host_id", "link_id", "target", "quote", "body",
		"source", "deleted", "last_sequence", "winner_logical_clock",
		"winner_device_id", "winner_op_id", "created_at", "updated_at",
	}
}

func readerThoughtSyncLifecycleColumnsForTest() []string {
	return append(readerThoughtSyncColumnsForTest(), "snapshot", "reason", "tombstoned_at")
}

func readerThoughtSyncRow(
	id string,
	sequence int64,
	deleted bool,
	updatedAt time.Time,
) []any {
	return []any{
		id,
		"link",
		"link-1",
		nil,
		[]byte(`{"kind":"summary","source_hash":"hash"}`),
		[]byte(`{"exact":"quote","start":0,"end":5}`),
		"thought",
		"user",
		deleted,
		sequence,
		sequence,
		"device-test",
		"op-" + id,
		updatedAt,
		updatedAt,
	}
}

func readerThoughtReplaySnapshotForTest(id, body string, at time.Time) []byte {
	snapshot, err := json.Marshal(map[string]any{
		"snapshot_version":       1,
		"id":                     id,
		"host_kind":              "link",
		"host_id":                "link-1",
		"link_id":                nil,
		"type":                   "thought",
		"body":                   body,
		"target":                 map[string]any{"kind": "summary", "source_hash": "frozen-hash"},
		"quote":                  map[string]any{"exact": "frozen quote", "prefix": "before ", "suffix": " after", "start": 0, "end": 12},
		"source":                 "frozen-source",
		"created_at":             at,
		"updated_at":             at,
		"original_host_snapshot": map[string]any{"content": "frozen host"},
		"original_host_identity": map[string]any{"kind": "link", "id": "link-1"},
		"frozen_at":              at,
	})
	if err != nil {
		panic(err)
	}
	return snapshot
}

func readerThoughtSyncLifecycleRow(id string, sequence int64, deleted bool, updatedAt time.Time, snapshot, reason, tombstonedAt any) []any {
	row := readerThoughtSyncRow(id, sequence, deleted, updatedAt)
	return append(row, snapshot, reason, tombstonedAt)
}

func TestListThoughtsSinceReturnsTombstonesAndServerCursor(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	updatedAt := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	thoughtID := "annotation-1"
	tombstoneReason := "link_deleted"
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT " + readerThoughtColumns + ",tt.snapshot,tt.reason,tt.created_at FROM reader_thoughts LEFT JOIN reader_thought_tombstones tt ON tt.thought_id=reader_thoughts.id WHERE true ORDER BY last_sequence ASC, id ASC LIMIT $1",
	)).
		WithArgs(1).
		WillReturnRows(mock.NewRows(readerThoughtSyncLifecycleColumnsForTest()).
			AddRow(readerThoughtSyncLifecycleRow(thoughtID, 7, true, updatedAt, readerThoughtReplaySnapshotForTest(thoughtID, "frozen tombstone", updatedAt), tombstoneReason, updatedAt)...))

	repo := NewPGXReaderVNextRepository(mock)
	items, next, err := repo.ListThoughtsSince(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("ListThoughtsSince() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != thoughtID || !items[0].Deleted {
		t.Fatalf("items = %+v, want one deleted thought", items)
	}
	if items[0].LifecycleStatus != "tombstone" || items[0].LifecycleReason == nil || *items[0].LifecycleReason != tombstoneReason {
		t.Fatalf("lifecycle = status=%q reason=%v, want host tombstone", items[0].LifecycleStatus, items[0].LifecycleReason)
	}
	if items[0].HostKind != "link" || items[0].HostID != "link-1" || items[0].LinkID != nil ||
		items[0].Body != "frozen tombstone" || items[0].Source != "frozen-source" ||
		!readerJSONEqual(items[0].Target, []byte(`{"kind":"summary","source_hash":"frozen-hash"}`)) ||
		!readerJSONEqual(items[0].Quote, []byte(`{"exact":"frozen quote","prefix":"before ","suffix":" after","start":0,"end":12}`)) ||
		!readerJSONEqual(items[0].OriginalHostSnapshot, []byte(`{"content":"frozen host"}`)) {
		t.Fatalf("sync replay did not reconstruct immutable snapshot authority: %+v", items[0])
	}
	if next != thoughtSyncCursor(7, thoughtID) {
		t.Fatalf("next cursor = %q, want %q", next, thoughtSyncCursor(7, thoughtID))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListThoughtsSinceUsesStableInstallationCursor(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	cursor := thoughtSyncCursor(7, "annotation-1")
	updatedAt := time.Date(2026, 8, 9, 12, 1, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT "+readerThoughtColumns+",tt.snapshot,tt.reason,tt.created_at FROM reader_thoughts LEFT JOIN reader_thought_tombstones tt ON tt.thought_id=reader_thoughts.id WHERE true AND (last_sequence > $1 OR (last_sequence = $1 AND id > $2)) ORDER BY last_sequence ASC, id ASC LIMIT $3",
	)).
		WithArgs(int64(7), "annotation-1", 2).
		WillReturnRows(mock.NewRows(readerThoughtSyncLifecycleColumnsForTest()).
			AddRow(readerThoughtSyncLifecycleRow("annotation-2", 8, false, updatedAt, nil, nil, nil)...))

	repo := NewPGXReaderVNextRepository(mock)
	items, next, err := repo.ListThoughtsSince(context.Background(), cursor, 2)
	if err != nil {
		t.Fatalf("ListThoughtsSince() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != "annotation-2" || items[0].Deleted {
		t.Fatalf("items = %+v, want one live thought", items)
	}
	if next != thoughtSyncCursor(8, "annotation-2") {
		t.Fatalf("next cursor = %q, want %q", next, thoughtSyncCursor(8, "annotation-2"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListThoughtsSinceCursorAdvancesThroughTombstoneLifecycleRows(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	firstAt := time.Date(2026, 8, 9, 12, 2, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Minute)
	reason := "note_deleted"
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT " + readerThoughtColumns + ",tt.snapshot,tt.reason,tt.created_at FROM reader_thoughts LEFT JOIN reader_thought_tombstones tt ON tt.thought_id=reader_thoughts.id WHERE true ORDER BY last_sequence ASC, id ASC LIMIT $1",
	)).
		WithArgs(2).
		WillReturnRows(mock.NewRows(readerThoughtSyncLifecycleColumnsForTest()).
			AddRow(readerThoughtSyncLifecycleRow("annotation-tombstone", 7, false, firstAt, readerThoughtReplaySnapshotForTest("annotation-tombstone", "frozen lifecycle", firstAt), reason, firstAt)...).
			AddRow(readerThoughtSyncLifecycleRow("annotation-live", 8, false, secondAt, nil, nil, nil)...))

	repo := NewPGXReaderVNextRepository(mock)
	items, next, err := repo.ListThoughtsSince(context.Background(), "", 2)
	if err != nil {
		t.Fatalf("ListThoughtsSince() error = %v", err)
	}
	if len(items) != 2 || items[0].LifecycleStatus != "tombstone" || items[1].LifecycleStatus != "active" {
		t.Fatalf("items = %+v, want tombstone followed by active lifecycle rows", items)
	}
	if next != thoughtSyncCursor(8, "annotation-live") {
		t.Fatalf("next cursor = %q, want %q", next, thoughtSyncCursor(8, "annotation-live"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListThoughtsSinceRejectsMalformedTombstoneSnapshotWithoutMutableFallback(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	updatedAt := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	thoughtID := "annotation-malformed-snapshot"
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT " + readerThoughtColumns + ",tt.snapshot,tt.reason,tt.created_at FROM reader_thoughts LEFT JOIN reader_thought_tombstones tt ON tt.thought_id=reader_thoughts.id WHERE true ORDER BY last_sequence ASC, id ASC LIMIT $1",
	)).
		WithArgs(1).
		WillReturnRows(mock.NewRows(readerThoughtSyncLifecycleColumnsForTest()).
			AddRow(readerThoughtSyncLifecycleRow(thoughtID, 9, false, updatedAt, []byte(`{"id":"annotation-malformed-snapshot","body":"must never fall back"}`), "link_deleted", updatedAt)...))

	items, next, err := NewPGXReaderVNextRepository(mock).ListThoughtsSince(context.Background(), "", 1)
	if !errors.Is(err, ErrInvalidReaderThought) {
		t.Fatalf("ListThoughtsSince() error = %v, want ErrInvalidReaderThought", err)
	}
	if items != nil || next != "" {
		t.Fatalf("ListThoughtsSince() = items=%#v next=%q, want no mutable fallback", items, next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListThoughtsSinceScrubsUserDeletedTombstone(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	updatedAt := time.Date(2026, 8, 11, 10, 1, 0, 0, time.UTC)
	thoughtID := "annotation-user-deleted"
	row := readerThoughtSyncRow(thoughtID, 10, true, updatedAt)
	row[6] = "mutable secret body"
	row[7] = "mutable secret source"
	row[4] = []byte(`{"kind":"summary","source_hash":"mutable-secret"}`)
	row[5] = []byte(`{"exact":"mutable secret quote"}`)
	mock.ExpectQuery(regexp.QuoteMeta(
		"SELECT " + readerThoughtColumns + ",tt.snapshot,tt.reason,tt.created_at FROM reader_thoughts LEFT JOIN reader_thought_tombstones tt ON tt.thought_id=reader_thoughts.id WHERE true ORDER BY last_sequence ASC, id ASC LIMIT $1",
	)).
		WithArgs(1).
		WillReturnRows(mock.NewRows(readerThoughtSyncLifecycleColumnsForTest()).
			AddRow(readerThoughtSyncLifecycleRow(thoughtID, 10, true, updatedAt, []byte(`{"id":"annotation-user-deleted","host_kind":"link","host_id":"link-1"}`), "user_deleted", updatedAt)...))

	items, next, err := NewPGXReaderVNextRepository(mock).ListThoughtsSince(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("ListThoughtsSince() error = %v", err)
	}
	if len(items) != 1 || !items[0].Deleted || items[0].LifecycleStatus != "tombstone" ||
		items[0].LifecycleReason == nil || *items[0].LifecycleReason != "user_deleted" ||
		items[0].Body != "" || items[0].Source != "" || items[0].LinkID != nil ||
		!readerJSONEqual(items[0].Target, []byte(`{}`)) || len(items[0].Quote) != 0 || len(items[0].OriginalHostSnapshot) != 0 {
		t.Fatalf("user deletion replay leaked mutable content: %+v", items[0])
	}
	if next != thoughtSyncCursor(10, thoughtID) {
		t.Fatalf("next cursor = %q, want %q", next, thoughtSyncCursor(10, thoughtID))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListThoughtsSinceRejectsInvalidCursorBeforeQuery(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	_, _, err = repo.ListThoughtsSince(context.Background(), "not-base64", 10)
	if !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("ListThoughtsSince() error = %v, want ErrInvalidReaderCursor", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database query: %v", err)
	}
}

func TestListThoughtConflictsReturnsDurableLoserAndWinner(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	columns := []string{"sequence", "annotation_id", "loser", "winner_at_detection"}
	mock.ExpectQuery("(?s)SELECT sequence,annotation_id,loser,winner_at_detection FROM reader_thought_supersession_events.*ORDER BY sequence ASC").
		WithArgs(10).
		WillReturnRows(mock.NewRows(columns).AddRow(
			int64(5), "thought-1",
			[]byte(`{"Sequence":8,"OpID":"op-loser","DeviceID":"device-b","LogicalClock":3,"OperationKind":"update","AnnotationID":"thought-1","HostKind":"link","HostID":"link-1","Target":{"kind":"summary"},"Payload":{"body":"old"},"CreatedAt":"2026-08-10T09:00:00Z"}`),
			[]byte(`{"Sequence":9,"OpID":"op-winner","DeviceID":"device-a","LogicalClock":3,"OperationKind":"delete","AnnotationID":"thought-1","HostKind":"link","HostID":"link-1","Target":{"kind":"summary"},"Payload":{"body":"winner"},"CreatedAt":"2026-08-10T09:00:01Z"}`),
		))

	repo := NewPGXReaderVNextRepository(mock)
	items, next, err := repo.ListThoughtConflicts(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("ListThoughtConflicts() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	item := items[0]
	if item.Sequence != 5 || item.AnnotationID != "thought-1" {
		t.Fatalf("conflict identity = %+v", item)
	}
	if item.Loser.OpID != "op-loser" || item.Loser.LogicalClock != 3 || item.Loser.OperationKind != "update" {
		t.Fatalf("loser = %+v", item.Loser)
	}
	if string(item.Loser.Payload) != `{"body":"old"}` {
		t.Fatalf("loser payload = %s, want original payload", item.Loser.Payload)
	}
	if item.Winner.OpID != "op-winner" || item.Winner.OperationKind != "delete" {
		t.Fatalf("winner = %+v", item.Winner)
	}
	if next != thoughtSyncCursor(5, "event") {
		t.Fatalf("next cursor = %q, want event cursor", next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestListThoughtConflictsRejectsOrdinaryThoughtCursorBeforeQuery(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	_, _, err = repo.ListThoughtConflicts(
		context.Background(),
		thoughtSyncCursor(7, "annotation-1"),
		10,
	)
	if !errors.Is(err, ErrInvalidReaderCursor) {
		t.Fatalf("ListThoughtConflicts() error = %v, want ErrInvalidReaderCursor", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database query: %v", err)
	}
}

func TestSearchThoughtsIncludesInstallationHistoricalProjection(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	updatedAt := time.Date(2026, 8, 9, 13, 0, 0, 0, time.UTC)
	snapshotAt := updatedAt.Add(time.Hour)
	linkID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	mock.ExpectQuery("(?s)WITH search_authority.*matching_thoughts.*LEFT JOIN reader_thought_tombstones.*thought.deleted=false.*snapshot.*quote.*ORDER BY updated_at DESC, thought_id DESC LIMIT \\$4").
		WithArgs("%anchor%", nil, nil, 6).
		WillReturnRows(mock.NewRows([]string{"id", "host_kind", "host_id", "link_id", "snippet", "count", "updated_at", "lifecycle_status", "lifecycle_reason", "history_deep_link", "snapshot_sequence", "snapshot_at"}).
			AddRow("thought-1", "link", "link-1", linkID.String(), "matching anchor", int64(2), updatedAt, "tombstone", "link_converted_to_site", "?tool=history&thought_view=history&thought_id=thought-1", int64(9), snapshotAt))

	repo := NewPGXReaderVNextRepository(mock)
	items, total, next, err := repo.SearchThoughts(context.Background(), " anchor ", "", 5)
	if err != nil {
		t.Fatalf("SearchThoughts() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != "thought-1" || items[0].LinkID == nil || *items[0].LinkID != linkID || total != 2 {
		t.Fatalf("SearchThoughts() = items=%+v total=%d", items, total)
	}
	if items[0].LifecycleStatus != "tombstone" || items[0].HistoryDeepLink == "" || items[0].LifecycleReason == nil || *items[0].LifecycleReason != "link_converted_to_site" {
		t.Fatalf("historical projection omitted lifecycle route: %+v", items[0])
	}
	if next != "" {
		t.Fatalf("SearchThoughts() next cursor = %q, want empty", next)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestSearchThoughtsPaginatesFullSortTupleWithoutDuplicates(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	updatedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	snapshotAt := updatedAt.Add(time.Hour)
	const snapshotSequence = int64(44)
	columns := []string{"id", "host_kind", "host_id", "link_id", "snippet", "count", "updated_at", "lifecycle_status", "lifecycle_reason", "history_deep_link", "snapshot_sequence", "snapshot_at"}
	firstRows := mock.NewRows(columns)
	for number := 21; number >= 1; number-- {
		id := fmt.Sprintf("thought-%02d", number)
		firstRows.AddRow(id, "link", "host-"+id, "", "matching "+id, int64(21), updatedAt, "active", nil, "", snapshotSequence, snapshotAt)
	}
	mock.ExpectQuery("(?s)WITH search_authority.*matching_thoughts.*ORDER BY updated_at DESC, thought_id DESC LIMIT \\$4").
		WithArgs("%anchor%", nil, nil, 21).
		WillReturnRows(firstRows)
	mock.ExpectQuery("(?s)WITH search_authority.*matching_thoughts.*WHERE \\(updated_at < \\$4 OR \\(updated_at = \\$4 AND thought_id < \\$5\\)\\).*ORDER BY updated_at DESC, thought_id DESC LIMIT \\$6").
		WithArgs("%anchor%", snapshotSequence, snapshotAt, updatedAt, "thought-02", 21).
		WillReturnRows(mock.NewRows(columns).
			AddRow("thought-01", "link", "host-thought-01", "", "matching thought-01", int64(99), updatedAt, "active", nil, "", snapshotSequence, snapshotAt))

	repo := NewPGXReaderVNextRepository(mock)
	first, firstTotal, next, err := repo.SearchThoughts(context.Background(), "anchor", "", 20)
	if err != nil {
		t.Fatalf("first SearchThoughts() error = %v", err)
	}
	if firstTotal != 21 || len(first) != 20 {
		t.Fatalf("first SearchThoughts() = %d items, total=%d; want 20 / 21", len(first), firstTotal)
	}
	want, err := thoughtSearchCursor(updatedAt, "thought-02", "anchor", snapshotSequence, snapshotAt, 21)
	if err != nil {
		t.Fatalf("thoughtSearchCursor() error = %v", err)
	}
	if next != want {
		t.Fatalf("first next cursor = %q, want %q", next, want)
	}

	second, secondTotal, secondNext, err := repo.SearchThoughts(context.Background(), "anchor", next, 20)
	if err != nil {
		t.Fatalf("second SearchThoughts() error = %v", err)
	}
	if secondTotal != 21 || len(second) != 1 || second[0].ID != "thought-01" || secondNext != "" {
		t.Fatalf("second SearchThoughts() = items=%+v total=%d next=%q", second, secondTotal, secondNext)
	}

	seen := make(map[string]struct{}, 21)
	for _, page := range [][]model.ReaderThoughtSearch{first, second} {
		for _, item := range page {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("duplicate thought %q across cursor pages", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
	}
	if len(seen) != 21 {
		t.Fatalf("unique paged thought IDs = %d, want 21", len(seen))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestThoughtSearchCursorRejectsMalformedAndWrongScopes(t *testing.T) {
	t.Parallel()

	updatedAt := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	snapshotAt := updatedAt.Add(time.Minute)
	cursor, err := thoughtSearchCursor(updatedAt, "thought-1", "  Query With Space  ", 17, snapshotAt, 23)
	if err != nil {
		t.Fatalf("thoughtSearchCursor() error = %v", err)
	}

	at, id, sequence, parsedSnapshotAt, total, err := parseThoughtSearchCursor(cursor, "query with space")
	if err != nil {
		t.Fatalf("parseThoughtSearchCursor() matching scope error = %v", err)
	}
	if !at.Equal(updatedAt) || id != "thought-1" || sequence != 17 || !parsedSnapshotAt.Equal(snapshotAt) || total != 23 {
		t.Fatalf("parseThoughtSearchCursor() = (%s, %q, %d, %s, %d)", at, id, sequence, parsedSnapshotAt, total)
	}

	for _, test := range []struct {
		name   string
		cursor string
		query  string
	}{
		{name: "malformed", cursor: "not-a-cursor", query: "query with space"},
		{name: "cross query", cursor: cursor, query: "different query"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, _, _, err := parseThoughtSearchCursor(test.cursor, test.query)
			if !errors.Is(err, ErrInvalidReaderCursor) {
				t.Fatalf("parseThoughtSearchCursor() error = %v, want ErrInvalidReaderCursor", err)
			}
		})
	}
}

func TestSearchPublishedNotesExcludesDraftProjection(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	noteID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	updatedAt := time.Date(2026, 8, 9, 14, 0, 0, 0, time.UTC)
	mock.ExpectQuery("(?s)SELECT id, title.*FROM reader_notes.*deleted_at IS NULL AND published_revision > 0.*title ILIKE \\$1 OR published_content ILIKE \\$1.*LIMIT \\$2").
		WithArgs("%published%", 4).
		WillReturnRows(mock.NewRows([]string{"id", "title", "snippet", "published_revision", "count", "updated_at"}).
			AddRow(noteID, "Published note", "published content", int64(3), int64(1), updatedAt))

	repo := NewPGXReaderVNextRepository(mock)
	items, total, err := repo.SearchPublishedNotes(context.Background(), "published", 4)
	if err != nil {
		t.Fatalf("SearchPublishedNotes() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != noteID || items[0].PublishedRevision != 3 || items[0].Snippet != "published content" || total != 1 {
		t.Fatalf("SearchPublishedNotes() = items=%+v total=%d", items, total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func readerInboxColumnsForTest() []string {
	return []string{
		"id", "url", "identity_key", "source_kind", "title", "body", "body_document", "body_format", "note", "summary", "suggested_tags", "proposal_status", "tags",
		"status", "metadata_revision", "expires_at", "expired", "deleted_at", "created_at", "updated_at",
	}
}

func readerInboxRowForTest(id uuid.UUID, expiresAt any, expired bool, deletedAt any, now time.Time) []any {
	return []any{
		id, "https://example.com/inbox", "https://example.com/inbox", "url", nil, "body", nil, "plain", "", nil,
		[]string{"suggested"}, "completed", []string{"tag"}, "pending", int64(1),
		expiresAt, expired, deletedAt, now.Add(-time.Hour), now,
	}
}

func readerInboxListColumnsForTest() []string {
	return []string{"id", "url", "source_kind", "title", "preview", "tags", "status", "metadata_revision", "expired", "updated_at"}
}

func readerInboxListRowForTest(id uuid.UUID, preview string, expired bool, now time.Time) []any {
	return []any{id, "https://example.com/inbox", "url", nil, preview, []string{"tag"}, "pending", int64(1), expired, now}
}

func TestListInboxDerivesExpiryPartitionsFromServerTimeAndReturnsBothCounts(t *testing.T) {
	tests := []struct {
		name         string
		partition    model.ReaderInboxPartition
		wantExpired  bool
		partitionSQL string
		activeCount  int
		expiredCount int
	}{
		{name: "active", partition: model.ReaderInboxPartitionActive, partitionSQL: "(expires_at IS NULL OR expires_at > NOW())", activeCount: 1, expiredCount: 2},
		{name: "expired", partition: model.ReaderInboxPartitionExpired, wantExpired: true, partitionSQL: "expires_at IS NOT NULL AND expires_at <= NOW()", activeCount: 1, expiredCount: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool() error = %v", err)
			}
			defer mock.Close()

			now := time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC)
			inboxID := uuid.New()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT " + readerInboxListColumns + " FROM reader_inbox WHERE status='pending' AND deleted_at IS NULL AND " + test.partitionSQL + " ORDER BY updated_at DESC,id DESC LIMIT $1")).
				WithArgs(2).
				WillReturnRows(mock.NewRows(readerInboxListColumnsForTest()).
					AddRow(readerInboxListRowForTest(inboxID, "card preview", test.wantExpired, now)...))
			mock.ExpectQuery("(?s)SELECT\\s+count\\(\\*\\) FILTER \\(WHERE expires_at IS NULL OR expires_at > NOW\\(\\)\\)::int,\\s+count\\(\\*\\) FILTER \\(WHERE expires_at IS NOT NULL AND expires_at <= NOW\\(\\)\\)::int\\s+FROM reader_inbox\\s+WHERE status='pending' AND deleted_at IS NULL").
				WillReturnRows(mock.NewRows([]string{"active_count", "expired_count"}).AddRow(test.activeCount, test.expiredCount))

			repo := NewPGXReaderVNextRepository(mock)
			items, activeCount, expiredCount, next, err := repo.ListInbox(context.Background(), test.partition, "", 2)
			if err != nil {
				t.Fatalf("ListInbox() error = %v", err)
			}
			if len(items) != 1 || items[0].Expired != test.wantExpired || activeCount != test.activeCount || expiredCount != test.expiredCount || next != "" {
				t.Fatalf("ListInbox() = items=%+v active=%d expired=%d next=%q", items, activeCount, expiredCount, next)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("pgxmock expectations: %v", err)
			}
		})
	}
}

func TestListInboxRejectsInvalidPartition(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXReaderVNextRepository(mock)
	_, _, _, _, err = repo.ListInbox(context.Background(), model.ReaderInboxPartition("other"), "", 30)
	if !errors.Is(err, ErrReaderInboxStateConflict) {
		t.Fatalf("ListInbox() error = %v, want ErrReaderInboxStateConflict", err)
	}
}

func TestRestoreInboxRenewsExpiredLiveRowOnlyOnce(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	inboxID := uuid.New()
	past := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	renewed := past.Add(30 * 24 * time.Hour)
	lockQuery := "(?s)SELECT " + regexp.QuoteMeta(readerInboxColumns) + ".*FROM reader_inbox.*WHERE id=\\$1.*FOR UPDATE"

	mock.ExpectBegin()
	mock.ExpectQuery(lockQuery).
		WithArgs(inboxID).
		WillReturnRows(mock.NewRows(readerInboxColumnsForTest()).
			AddRow(readerInboxRowForTest(inboxID, past, true, nil, past)...))
	mock.ExpectQuery("(?s)UPDATE reader_inbox.*SET deleted_at=NULL,.*expires_at=CASE WHEN \\$2 THEN NOW\\(\\) \\+ INTERVAL '30 days' ELSE expires_at END,.*updated_at=NOW\\(\\).*WHERE id=\\$1.*RETURNING "+regexp.QuoteMeta(readerInboxColumns)).
		WithArgs(inboxID, true).
		WillReturnRows(mock.NewRows(readerInboxColumnsForTest()).
			AddRow(readerInboxRowForTest(inboxID, renewed, false, nil, renewed)...))
	mock.ExpectCommit()

	// Once the deadline is renewed, a retry must not move it again or
	// invoke the thought lifecycle machinery for an already-live item.
	mock.ExpectBegin()
	mock.ExpectQuery(lockQuery).
		WithArgs(inboxID).
		WillReturnRows(mock.NewRows(readerInboxColumnsForTest()).
			AddRow(readerInboxRowForTest(inboxID, renewed, false, nil, renewed)...))
	mock.ExpectCommit()

	repo := NewPGXReaderVNextRepository(mock)
	if err := repo.RestoreInbox(context.Background(), inboxID); err != nil {
		t.Fatalf("first RestoreInbox() error = %v", err)
	}
	if err := repo.RestoreInbox(context.Background(), inboxID); err != nil {
		t.Fatalf("retry RestoreInbox() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestConfirmAIProposalsUsesBoundedCurrentProposalSelectionForRequestedPartition(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("(?s)SELECT " + regexp.QuoteMeta(readerInboxColumnsQualified) + ".*FROM reader_inbox inbox.*inbox.status='pending'.*inbox.deleted_at IS NULL.*inbox.expires_at IS NOT NULL AND inbox.expires_at <= NOW\\(\\).*btrim\\(COALESCE\\(inbox.title,''\\)\\) <> ''.*inbox.proposal_status='completed'.*ORDER BY inbox.created_at ASC,inbox.id ASC.*LIMIT \\$1.*FOR UPDATE OF inbox").
		WithArgs(readerInboxAIProposalBatchSize).
		WillReturnRows(mock.NewRows(readerInboxColumnsForTest()))
	mock.ExpectQuery("(?s)SELECT count\\(\\*\\)::int.*FROM reader_inbox inbox.*inbox.expires_at IS NOT NULL AND inbox.expires_at <= NOW\\(\\).*inbox.proposal_status='completed'").
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectCommit()

	result, err := NewPGXReaderVNextRepository(mock).ConfirmAIProposals(context.Background(), model.ReaderInboxPartitionExpired)
	if err != nil {
		t.Fatalf("ConfirmAIProposals() error = %v", err)
	}
	if result.Items == nil || len(result.Items) != 0 || result.RemainingCount != 0 {
		t.Fatalf("ConfirmAIProposals() = %#v, want an empty atomic batch", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}
