package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/pgvector/pgvector-go"
)

// TestListConceptsOfNearestLinksAppliesExclusion pins the re-parse exclusion
// and the installation-wide pgvector query shape.
func TestListConceptsOfNearestLinksAppliesExclusion(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	exclude := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	conceptID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	vec := []float32{0.1, 0.2}

	mock.ExpectQuery(regexp.QuoteMeta("WITH nearest_links AS (")).
		WithArgs(pgvector.NewVector(vec), "m1", 10, exclude, 15, neighbourMaxCosineDistance).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "freq"}).
			AddRow(conceptID, "科技周刊", int64(4)))

	got, err := repo.ListConceptsOfNearestLinks(context.Background(), vec, "m1", 10, 15, exclude)
	if err != nil {
		t.Fatalf("ListConceptsOfNearestLinks error = %v", err)
	}
	if len(got) != 1 || got[0].ConceptID != conceptID || got[0].Name != "科技周刊" {
		t.Fatalf("candidates = %#v, want the single neighbour concept", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock: %v", err)
	}
}

// TestListConceptsOfNearestLinksSkipsWithoutVector: no embedding or no model
// means there is nothing to search by. Returning (nil, nil) rather than
// erroring keeps this a recall miss, so the pipeline degrades to the other
// leg instead of logging a spurious failure.
func TestListConceptsOfNearestLinksSkipsWithoutVector(t *testing.T) {
	t.Parallel()

	repo := NewPGXConceptRepository(nil)
	for name, run := range map[string]func() error{
		"no embedding": func() error {
			_, err := repo.ListConceptsOfNearestLinks(context.Background(), nil, "m1", 10, 15, uuid.Nil)
			return err
		},
		"no model": func() error {
			_, err := repo.ListConceptsOfNearestLinks(context.Background(), []float32{0.1}, "  ", 10, 15, uuid.Nil)
			return err
		},
	} {
		name, run := name, run
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A nil pool would panic if the query were actually issued, which
			// is the real assertion here.
			if err := run(); err != nil {
				t.Fatalf("%s should no-op without error; got %v", name, err)
			}
		})
	}
}

// TestListConceptsOfNearestLinksClampsNonPositiveLimits stops a zero-value
// caller from turning the recall into LIMIT 0 (a silent empty result that
// would look like "词表 has nothing" instead of a wiring bug).
func TestListConceptsOfNearestLinksClampsNonPositiveLimits(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	vec := []float32{0.1}

	mock.ExpectQuery(regexp.QuoteMeta("WITH nearest_links AS (")).
		WithArgs(pgvector.NewVector(vec), "m1", 1, uuid.Nil, 1, neighbourMaxCosineDistance).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "freq"}))

	if _, err := repo.ListConceptsOfNearestLinks(context.Background(), vec, "m1", 0, -3, uuid.Nil); err != nil {
		t.Fatalf("ListConceptsOfNearestLinks error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock: %v", err)
	}
}
