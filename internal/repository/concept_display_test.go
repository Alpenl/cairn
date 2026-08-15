package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestRecalculateDisplayNameRunsSingleUpdate(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	mock.ExpectExec(regexp.QuoteMeta("SET display_name = (")).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := repo.RecalculateDisplayName(context.Background(), id); err != nil {
		t.Fatalf("RecalculateDisplayName error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock: %v", err)
	}
}

func TestRecalculateDisplayNameNoopsOnNilID(t *testing.T) {
	t.Parallel()
	repo := NewPGXConceptRepository(nil)
	if err := repo.RecalculateDisplayName(context.Background(), uuid.Nil); err != nil {
		t.Fatalf("nil id should no-op without error; got %v", err)
	}
}

func TestRecalculateDisplayNameSurfacesDBError(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXConceptRepository(mock)
	id := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta("SET display_name = (")).
		WithArgs(id).
		WillReturnError(errors.New("boom"))
	if err := repo.RecalculateDisplayName(context.Background(), id); err == nil {
		t.Fatal("expected DB error to surface")
	}
}

func TestDetachLinkConceptsExceptDeletesStaleAndReturnsRemoved(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	linkID := uuid.MustParse("dddddddd-0000-0000-0000-000000000001")
	keepA := uuid.MustParse("dddddddd-0000-0000-0000-0000000000a1")
	keepB := uuid.MustParse("dddddddd-0000-0000-0000-0000000000b2")
	removed := uuid.MustParse("dddddddd-0000-0000-0000-0000000000c3")

	// The CTE locks and checks the metadata tuple before it deletes stale edges
	// and returns their concept IDs.
	mock.ExpectQuery(regexp.QuoteMeta("WITH target AS MATERIALIZED (")).
		WithArgs(linkID, int64(1), []string{"current"}, []uuid.UUID{keepA, keepB}).
		WillReturnRows(mock.NewRows([]string{"concept_id"}).AddRow(removed))

	got, err := repo.DetachLinkConceptsExcept(context.Background(), linkID, 1, []string{"current"}, []uuid.UUID{keepA, keepB})
	if err != nil {
		t.Fatalf("DetachLinkConceptsExcept error = %v", err)
	}
	if len(got) != 1 || got[0] != removed {
		t.Fatalf("removed = %v, want [%v]", got, removed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock: %v", err)
	}
}

func TestDetachLinkConceptsExceptNoopOnEmptyKeep(t *testing.T) {
	t.Parallel()
	// Empty keep set must NOT issue a DELETE (which would wipe the link's
	// whole concept set); a nil pool proves no query is attempted.
	repo := NewPGXConceptRepository(nil)
	got, err := repo.DetachLinkConceptsExcept(context.Background(), uuid.New(), 0, nil, nil)
	if err != nil || got != nil {
		t.Fatalf("empty keep should no-op; got (%v, %v)", got, err)
	}
}

func TestDetachLinkConceptsExceptSurfacesDBError(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXConceptRepository(mock)
	linkID := uuid.New()
	keep := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta("WITH target AS MATERIALIZED (")).
		WithArgs(linkID, int64(1), []string{"current"}, []uuid.UUID{keep}).
		WillReturnError(errors.New("boom"))
	if _, err := repo.DetachLinkConceptsExcept(context.Background(), linkID, 1, []string{"current"}, []uuid.UUID{keep}); err == nil {
		t.Fatal("expected DB error to surface")
	}
}

func TestListDisplayNamesByLinkIDsGroupsByLink(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	linkA := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	linkB := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000002")

	mock.ExpectQuery(regexp.QuoteMeta("FROM link_concept lc")).
		WithArgs([]uuid.UUID{linkA, linkB}).
		WillReturnRows(
			mock.NewRows([]string{"link_id", "name"}).
				AddRow(linkA, "RAG").
				AddRow(linkA, "WeKnora").
				AddRow(linkB, "RAG"),
		)

	got, err := repo.ListDisplayNamesByLinkIDs(context.Background(), []uuid.UUID{linkA, linkB})
	if err != nil {
		t.Fatalf("ListDisplayNamesByLinkIDs err = %v", err)
	}
	if len(got[linkA]) != 2 || got[linkA][0] != "RAG" || got[linkA][1] != "WeKnora" {
		t.Fatalf("linkA = %+v", got[linkA])
	}
	if len(got[linkB]) != 1 || got[linkB][0] != "RAG" {
		t.Fatalf("linkB = %+v", got[linkB])
	}
}

func TestListDisplayNamesByLinkIDsEmptyInputReturnsEmptyMap(t *testing.T) {
	t.Parallel()
	repo := NewPGXConceptRepository(nil)
	got, err := repo.ListDisplayNamesByLinkIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map for nil input; got %+v", got)
	}
}

func TestIncrementUseCountsRunsOneInstallationUpdate(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	mock.ExpectExec(regexp.QuoteMeta("UPDATE concept SET use_count = use_count + 1, updated_at = now()")).
		WithArgs([]uuid.UUID{id}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := repo.IncrementUseCounts(context.Background(), []uuid.UUID{id}); err != nil {
		t.Fatalf("IncrementUseCounts error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestIncrementUseCountsSurfacesDBError(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	mock.ExpectExec(regexp.QuoteMeta("UPDATE concept SET use_count = use_count + 1, updated_at = now()")).
		WithArgs([]uuid.UUID{id}).
		WillReturnError(errors.New("boom"))

	if err := repo.IncrementUseCounts(context.Background(), []uuid.UUID{id}); err == nil {
		t.Fatal("expected DB error to surface")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestRecalculateDisplayNamesRunsOneInstallationUpdate(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	mock.ExpectExec(regexp.QuoteMeta("SET display_name = winners.surface_tag")).
		WithArgs([]uuid.UUID{id}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := repo.RecalculateDisplayNames(context.Background(), []uuid.UUID{id}); err != nil {
		t.Fatalf("RecalculateDisplayNames error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}
