package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func TestMergeConceptsByProposalHappyPath(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	proposalID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	winner := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	loser := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	createdAt := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	decidedAt := createdAt.Add(time.Minute)
	mock.ExpectBegin()
	expectRepresentationWriteGateExclusive(mock)
	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal")).
		WithArgs(proposalID).
		WillReturnRows(mock.NewRows(proposalColumnsForTest()).AddRow(proposalID, winner, loser, float32(0.84), "same concept", "pending", nil, createdAt, nil))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM concept WHERE id=$1)")).
		WithArgs(winner, loser).
		WillReturnRows(mock.NewRows([]string{"winner_exists", "loser_exists"}).AddRow(true, true))
	expectLibraryGlobalRevisionPrelock(mock)
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM link_concept")).
		WithArgs(loser, winner).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE link_concept SET concept_id")).
		WithArgs(winner, loser).
		WillReturnResult(pgxmock.NewResult("UPDATE", 4))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM concept_alias")).
		WithArgs(loser, winner).
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE concept_alias SET concept_id")).
		WithArgs(winner, loser).
		WillReturnResult(pgxmock.NewResult("UPDATE", 3))
	mock.ExpectExec(regexp.QuoteMeta("SET use_count = use_count + COALESCE")).
		WithArgs(winner, loser).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("SET display_name = (")).
		WithArgs(winner).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE concept_merge_proposal")).
		WithArgs(proposalID, "ops@acme.io").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM concept WHERE id =")).
		WithArgs(loser).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal WHERE id = $1")).
		WithArgs(proposalID).
		WillReturnRows(mock.NewRows(proposalColumnsForTest()).AddRow(proposalID, winner, loser, float32(0.84), "same concept", "approved", "ops@acme.io", createdAt, decidedAt))
	mock.ExpectCommit()

	if err := repo.MergeConceptsByProposal(context.Background(), proposalID, "ops@acme.io"); err != nil {
		t.Fatalf("MergeConceptsByProposal error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestMergeConceptsByProposalReturnsAlreadyDecidedSentinel(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	proposalID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	mock.ExpectBegin()
	expectRepresentationWriteGateExclusive(mock)
	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal")).
		WithArgs(proposalID).
		WillReturnError(errPgxNoRows())
	mock.ExpectRollback()

	err = repo.MergeConceptsByProposal(context.Background(), proposalID, "admin")
	if !errors.Is(err, ErrConceptMergeAlreadyDecided) {
		t.Fatalf("err = %v, want ErrConceptMergeAlreadyDecided", err)
	}
}

func TestMergeConceptsByProposalRejectsOrphanedPendingProposal(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	proposalID, winner, loser := uuid.New(), uuid.New(), uuid.New()
	mock.ExpectBegin()
	expectRepresentationWriteGateExclusive(mock)
	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal")).
		WithArgs(proposalID).
		WillReturnRows(mock.NewRows(proposalColumnsForTest()).AddRow(
			proposalID, winner, loser, float32(0.77), "orphan", "pending", nil, time.Now(), nil,
		))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM concept WHERE id=$1)")).
		WithArgs(winner, loser).
		WillReturnRows(mock.NewRows([]string{"winner_exists", "loser_exists"}).AddRow(true, false))
	mock.ExpectRollback()

	err = repo.MergeConceptsByProposal(context.Background(), proposalID, "admin")
	if !errors.Is(err, ErrConceptMergeOrphaned) {
		t.Fatalf("error = %v, want ErrConceptMergeOrphaned", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestMergeConceptsByProposalRollsBackWhenAuditDisappears(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	proposalID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	winner := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	loser := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	createdAt := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	expectRepresentationWriteGateExclusive(mock)
	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal")).
		WithArgs(proposalID).
		WillReturnRows(mock.NewRows(proposalColumnsForTest()).AddRow(proposalID, winner, loser, float32(0.8), "audit me", "pending", nil, createdAt, nil))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT EXISTS (SELECT 1 FROM concept WHERE id=$1)")).
		WithArgs(winner, loser).
		WillReturnRows(mock.NewRows([]string{"winner_exists", "loser_exists"}).AddRow(true, true))
	expectLibraryGlobalRevisionPrelock(mock)
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM link_concept")).
		WithArgs(loser, winner).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE link_concept SET concept_id")).
		WithArgs(winner, loser).
		WillReturnResult(pgxmock.NewResult("UPDATE", 4))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM concept_alias")).
		WithArgs(loser, winner).
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE concept_alias SET concept_id")).
		WithArgs(winner, loser).
		WillReturnResult(pgxmock.NewResult("UPDATE", 3))
	mock.ExpectExec(regexp.QuoteMeta("SET use_count = use_count + COALESCE")).
		WithArgs(winner, loser).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("SET display_name = (")).
		WithArgs(winner).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE concept_merge_proposal")).
		WithArgs(proposalID, "admin").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM concept WHERE id =")).
		WithArgs(loser).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal WHERE id = $1")).
		WithArgs(proposalID).
		WillReturnRows(mock.NewRows(proposalColumnsForTest()))
	mock.ExpectRollback()

	if err := repo.MergeConceptsByProposal(context.Background(), proposalID, "admin"); !errors.Is(err, ErrConceptMergeAuditLost) {
		t.Fatalf("error = %v, want ErrConceptMergeAuditLost", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestMergeConceptsByProposalRejectsNilID(t *testing.T) {
	t.Parallel()
	repo := NewPGXConceptProposalRepository(must(pgxmock.NewPool()))
	if err := repo.MergeConceptsByProposal(context.Background(), uuid.Nil, "x"); err == nil {
		t.Fatal("nil proposal id should error")
	}
}

func TestMergeConceptsByProposalRejectsCorruptSelfMerge(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXConceptProposalRepository(mock)

	proposalID := uuid.New()
	same := uuid.New()
	mock.ExpectBegin()
	expectRepresentationWriteGateExclusive(mock)
	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal")).
		WithArgs(proposalID).
		WillReturnRows(mock.NewRows(proposalColumnsForTest()).AddRow(proposalID, same, same, float32(0.8), nil, "pending", nil, time.Now(), nil))
	mock.ExpectRollback()

	err = repo.MergeConceptsByProposal(context.Background(), proposalID, "admin")
	if err == nil {
		t.Fatal("expected self-merge defence error")
	}
}

func errPgxNoRows() error { return pgx.ErrNoRows }

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
