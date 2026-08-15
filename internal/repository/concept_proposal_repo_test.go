package repository

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/concept"
)

func proposalColumnsForTest() []string {
	return []string{
		"id", "winner_id", "loser_id", "score", "llm_reason",
		"status", "decided_by", "created_at", "decided_at",
	}
}

func TestCreateProposalReturnsIDOnInsert(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	winner := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	loser := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	newID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	mock.ExpectQuery(`INSERT INTO concept_merge_proposal`).
		WithArgs(winner, loser, float32(0.82), "same concept").
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(newID))

	got, err := repo.CreateProposal(context.Background(), concept.CreateMergeProposalParams{
		WinnerID:  winner,
		LoserID:   loser,
		Score:     0.82,
		LLMReason: "same concept",
	})
	if err != nil {
		t.Fatalf("CreateProposal() error = %v", err)
	}
	if got != newID {
		t.Fatalf("CreateProposal() = %s, want %s", got, newID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestCreateProposalRejectsMissingLiveConcept(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	winner, missingLoser := uuid.New(), uuid.New()
	mock.ExpectQuery(`INSERT INTO concept_merge_proposal`).
		WithArgs(winner, missingLoser, float32(0.82), nil).
		WillReturnRows(mock.NewRows([]string{"id"}))

	_, err = repo.CreateProposal(context.Background(), concept.CreateMergeProposalParams{
		WinnerID: winner,
		LoserID:  missingLoser,
		Score:    0.82,
	})
	if !errors.Is(err, ErrConceptMergeOrphaned) {
		t.Fatalf("CreateProposal() error = %v, want ErrConceptMergeOrphaned", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestCreateProposalRejectsSelfMerge(t *testing.T) {
	t.Parallel()

	repo := NewPGXConceptProposalRepository(nil) // no DB call expected
	id := uuid.New()

	if _, err := repo.CreateProposal(context.Background(), concept.CreateMergeProposalParams{
		WinnerID: id, LoserID: id,
	}); err == nil || !strings.Contains(err.Error(), "winner == loser") {
		t.Fatalf("CreateProposal(self) error = %v, want winner==loser refusal", err)
	}
}

func TestCreateProposalRejectsNilIDs(t *testing.T) {
	t.Parallel()

	repo := NewPGXConceptProposalRepository(nil)
	if _, err := repo.CreateProposal(context.Background(), concept.CreateMergeProposalParams{
		WinnerID: uuid.Nil, LoserID: uuid.New(),
	}); err == nil {
		t.Fatal("CreateProposal(nil winner) should error")
	}
}

func TestListPendingProposalsReturnsRowsAndAppliesDefaults(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	createdAt := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	// limit=0 → repo defaults to 50, offset=-1 → 0. The pending row
	// has llm_reason/decided_by/decided_at as nil to exercise the
	// nullable scan path that replaced the COALESCE-sentinel hack.
	mock.ExpectQuery(regexp.QuoteMeta("WHERE status = 'pending'")).
		WithArgs(50, 0).
		WillReturnRows(
			mock.NewRows(proposalColumnsForTest()).
				AddRow(
					uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001"),
					uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002"),
					uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000003"),
					float32(0.91), nil,
					"pending", nil, createdAt, nil,
				),
		)

	got, err := repo.ListPendingProposals(context.Background(), 0, -1)
	if err != nil {
		t.Fatalf("ListPendingProposals() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Status != "pending" {
		t.Fatalf("got[0].Status = %q, want pending", got[0].Status)
	}
	if !got[0].DecidedAt.IsZero() {
		t.Fatalf("got[0].DecidedAt = %v, want zero (pending)", got[0].DecidedAt)
	}
	if got[0].LLMReason != "" || got[0].DecidedBy != "" {
		t.Fatalf("nullable strings should flatten to empty; got reason=%q decidedBy=%q",
			got[0].LLMReason, got[0].DecidedBy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestListPendingProposalsScansDecidedRowVerbatim(t *testing.T) {
	// Regression test: after the nullable-scan refactor a row that
	// actually has decided_at / decided_by populated must still be
	// surfaced — this used to be papered over by the COALESCE
	// sentinel and Year()>1 check.
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	createdAt := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	decidedAt := time.Date(2026, 5, 11, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("WHERE status = 'pending'")).
		WithArgs(50, 0).
		WillReturnRows(
			mock.NewRows(proposalColumnsForTest()).
				AddRow(
					uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001"),
					uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002"),
					uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000003"),
					float32(0.91), "same concept",
					"pending", "ops@acme.io", createdAt, decidedAt,
				),
		)

	got, err := repo.ListPendingProposals(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ListPendingProposals() error = %v", err)
	}
	if got[0].DecidedAt.IsZero() || !got[0].DecidedAt.Equal(decidedAt) {
		t.Fatalf("DecidedAt = %v, want %v", got[0].DecidedAt, decidedAt)
	}
	if got[0].DecidedBy != "ops@acme.io" || got[0].LLMReason != "same concept" {
		t.Fatalf("got[0] = %+v", got[0])
	}
}

func TestCreateProposalRejectsScoreOutOfRange(t *testing.T) {
	t.Parallel()

	repo := NewPGXConceptProposalRepository(nil)
	winner := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	loser := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	bads := []float32{-0.01, 1.5, float32(math.NaN())}
	for _, s := range bads {
		_, err := repo.CreateProposal(context.Background(), concept.CreateMergeProposalParams{
			WinnerID: winner, LoserID: loser, Score: s,
		})
		if err == nil {
			t.Fatalf("CreateProposal(score=%f) should error", s)
		}
		if !strings.Contains(err.Error(), "out of [0,1]") {
			t.Fatalf("CreateProposal(score=%f) error = %v, want out-of-range message", s, err)
		}
	}
}

func TestCreateProposalTruncatesLongLLMReason(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	winner := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	loser := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	long := strings.Repeat("a", 500)
	expected := long[:maxLLMReasonChars]

	mock.ExpectQuery(`INSERT INTO concept_merge_proposal`).
		WithArgs(winner, loser, float32(0.5), expected).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(id))

	if _, err := repo.CreateProposal(context.Background(), concept.CreateMergeProposalParams{
		WinnerID: winner, LoserID: loser, Score: 0.5, LLMReason: long,
	}); err != nil {
		t.Fatalf("CreateProposal() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestGetProposalRejectsNilID(t *testing.T) {
	t.Parallel()

	repo := NewPGXConceptProposalRepository(nil)
	_, err := repo.GetProposal(context.Background(), uuid.Nil)
	if err == nil || !strings.Contains(err.Error(), "nil id") {
		t.Fatalf("GetProposal(nil) error = %v, want nil-id refusal", err)
	}
}

func TestGetProposalReturnsNilOnNotFound(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	id := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal WHERE id = $1")).
		WithArgs(id).
		WillReturnRows(mock.NewRows(proposalColumnsForTest()))

	got, err := repo.GetProposal(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProposal() error = %v", err)
	}
	if got != nil {
		t.Fatalf("GetProposal() = %+v, want nil", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestGetProposalReturnsDurableTerminalAuditByStableID(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	id, winner, deletedLoser := uuid.New(), uuid.New(), uuid.New()
	createdAt := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	decidedAt := createdAt.Add(time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta("FROM concept_merge_proposal WHERE id = $1")).
		WithArgs(id).
		WillReturnRows(mock.NewRows(proposalColumnsForTest()).AddRow(
			id, winner, deletedLoser, float32(0.93), "duplicate", "approved", "ops", createdAt, decidedAt,
		))

	got, err := repo.GetProposal(context.Background(), id)
	if err != nil {
		t.Fatalf("GetProposal() error = %v", err)
	}
	if got == nil || got.ID != id || got.WinnerID != winner || got.LoserID != deletedLoser ||
		got.Status != concept.MergeProposalApproved || got.DecidedBy != "ops" || !got.DecidedAt.Equal(decidedAt) ||
		got.Score != float32(0.93) || got.LLMReason != "duplicate" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("GetProposal() = %+v, want complete terminal audit", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestMarkProposalDecidedAcceptsApprovedOnly(t *testing.T) {
	t.Parallel()

	repo := NewPGXConceptProposalRepository(nil)
	id := uuid.New()

	for _, bad := range []string{"pending", "PENDING", "", "merged"} {
		if err := repo.MarkProposalDecided(context.Background(), id, bad, "admin"); err == nil {
			t.Fatalf("MarkProposalDecided(%q) should error", bad)
		}
	}
}

func TestMarkProposalDecidedRequiresActor(t *testing.T) {
	t.Parallel()
	repo := NewPGXConceptProposalRepository(nil)
	if err := repo.MarkProposalDecided(context.Background(), uuid.New(), concept.MergeProposalRejected, "  "); err == nil {
		t.Fatal("MarkProposalDecided() accepted an empty audit actor")
	}
}

func TestMarkProposalDecidedErrorsOnAlreadyTerminalRow(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)
	id := uuid.MustParse("66666666-6666-6666-6666-666666666666")

	mock.ExpectExec(regexp.QuoteMeta("UPDATE concept_merge_proposal")).
		WithArgs(id, "approved", "admin").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	err = repo.MarkProposalDecided(context.Background(), id, concept.MergeProposalApproved, "admin")
	if !errors.Is(err, ErrConceptMergeAlreadyDecided) {
		t.Fatalf("MarkProposalDecided() error = %v, want ErrConceptMergeAlreadyDecided", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestCountPendingProposalsReturnsCount(t *testing.T) {
	t.Parallel()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptProposalRepository(mock)

	mock.ExpectQuery(`count\(\*\) FROM concept_merge_proposal WHERE status = 'pending'`).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(7))

	got, err := repo.CountPendingProposals(context.Background())
	if err != nil {
		t.Fatalf("CountPendingProposals() error = %v", err)
	}
	if got != 7 {
		t.Fatalf("CountPendingProposals() = %d, want 7", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}
