package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/pgvector/pgvector-go"
)

func TestRelatedTagsUsesDoneLinksAndTheSourceModelGeneration(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	mock.ExpectQuery("(?s)SELECT embedding,embedding_model,COALESCE\\(tags,'\\{\\}'\\).*id=\\$1 AND status='done'.*embedding_model IS NOT NULL").
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"embedding", "embedding_model", "tags"}).
			AddRow(pgvector.NewVector([]float32{1, 0, 0}), "embed-v2", []string{"source"}))
	mock.ExpectQuery("(?s)WITH nearest.*l.status='done'.*l.embedding_model=\\$2.*ORDER BY l.embedding <=> \\$1, l.id.*ORDER BY score DESC, uses DESC, candidate.*LIMIT \\$5").
		WithArgs(pgxmock.AnyArg(), "embed-v2", linkID, []string{"source"}, 3).
		WillReturnRows(mock.NewRows([]string{"candidate", "score", "uses"}).
			AddRow("semantic", 1.0, 2).
			AddRow("stable", 0.5, 1))

	items, model, degraded, err := NewPGXReaderVNextRepository(mock).RelatedTags(context.Background(), &linkID, 3)
	if err != nil {
		t.Fatalf("RelatedTags() error = %v", err)
	}
	if degraded || model != "semantic-v1:embed-v2" {
		t.Fatalf("RelatedTags() metadata = model %q degraded %v, want same-generation semantic result", model, degraded)
	}
	if want := []string{"semantic", "stable"}; len(items) != len(want) || items[0] != want[0] || items[1] != want[1] {
		t.Fatalf("items = %#v, want %#v", items, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestRelatedTagsSemanticFailureFallsBackWithinInstallation(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	mock.ExpectQuery("(?s)SELECT embedding,embedding_model,COALESCE\\(tags,'\\{\\}'\\).*id=\\$1 AND status='done'").
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"embedding", "embedding_model", "tags"}).
			AddRow(pgvector.NewVector([]float32{1, 0}), "embed-v1", []string{"source"}))
	mock.ExpectQuery("(?s)WITH nearest.*l.embedding_model=\\$2").
		WithArgs(pgxmock.AnyArg(), "embed-v1", linkID, []string{"source"}, 2).
		WillReturnError(errors.New("vector index unavailable"))
	mock.ExpectQuery("(?s)SELECT candidate FROM.*FROM links l WHERE l.status='done' AND l.deleted_at IS NULL AND l.tags && \\$1::text\\[\\]").
		WithArgs([]string{"source"}, 2).
		WillReturnRows(mock.NewRows([]string{"candidate"}).AddRow("cooccurring"))

	items, model, degraded, err := NewPGXReaderVNextRepository(mock).RelatedTags(context.Background(), &linkID, 2)
	if err != nil {
		t.Fatalf("RelatedTags() fallback error = %v", err)
	}
	if !degraded || model != "cooccurrence-v1" || len(items) != 1 || items[0] != "cooccurring" {
		t.Fatalf("fallback = items %#v model %q degraded %v, want installation-level cooccurrence", items, model, degraded)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}

func TestRelatedTagsReturnsNotFoundForMissingLink(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	linkID := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT embedding,embedding_model,COALESCE(tags,'{}') FROM links WHERE id=$1 AND status='done' AND deleted_at IS NULL AND embedding IS NOT NULL AND embedding_model IS NOT NULL")).
		WithArgs(linkID).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(tags,'{}') FROM links WHERE id=$1 AND status='done' AND deleted_at IS NULL")).
		WithArgs(linkID).
		WillReturnError(pgx.ErrNoRows)

	_, _, _, err = NewPGXReaderVNextRepository(mock).RelatedTags(context.Background(), &linkID, 2)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("RelatedTags() error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pgxmock expectations: %v", err)
	}
}
