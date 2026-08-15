package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/concept"
)

func TestCreateConceptAtomicallyCreatesIdentityAlias(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO concept (primary_name, wikidata_qid, embedding, embedding_model)")).
		WithArgs("RAG", nil, nil, nil).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(id))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO concept_alias (alias, concept_id, source, confidence)")).
		WithArgs("rag", id, concept.SourceIdentity).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	got, err := repo.CreateConcept(context.Background(), concept.CreateConceptParams{PrimaryName: " RAG "})
	if err != nil {
		t.Fatalf("CreateConcept() error = %v", err)
	}
	if got != id {
		t.Fatalf("CreateConcept() = %v, want %v", got, id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestCreateConceptAliasRaceRollsBackAndReturnsCanonicalOwner(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	loser := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	winner := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO concept (primary_name, wikidata_qid, embedding, embedding_model)")).
		WithArgs("RAG", nil, nil, nil).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(loser))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO concept_alias (alias, concept_id, source, confidence)")).
		WithArgs("rag", loser, concept.SourceIdentity).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectRollback()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT concept_id FROM concept_alias WHERE alias = $1")).
		WithArgs("rag").
		WillReturnRows(mock.NewRows([]string{"concept_id"}).AddRow(winner))

	got, err := repo.CreateConcept(context.Background(), concept.CreateConceptParams{PrimaryName: "RAG"})
	if err != nil {
		t.Fatalf("CreateConcept() error = %v", err)
	}
	if got != winner {
		t.Fatalf("CreateConcept() = %v, want canonical owner %v", got, winner)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestUpsertAliasConflictDoesNotOverwriteCanonicalOwner(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()

	repo := NewPGXConceptRepository(mock)
	requested := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	existing := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO concept_alias (alias, concept_id, lang, source, confidence)")).
		WithArgs("rag", requested, nil, concept.SourceEmbeddingMerge, float32(0.95)).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT concept_id FROM concept_alias WHERE alias = $1")).
		WithArgs("rag").
		WillReturnRows(mock.NewRows([]string{"concept_id"}).AddRow(existing))

	err = repo.UpsertAlias(context.Background(), concept.UpsertAliasParams{
		Alias: "RAG", ConceptID: requested, Source: concept.SourceEmbeddingMerge, Confidence: 0.95,
	})
	if err == nil {
		t.Fatal("UpsertAlias() error = nil, want canonical-owner conflict")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}
