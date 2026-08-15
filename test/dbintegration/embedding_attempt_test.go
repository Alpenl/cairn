package dbintegration

import (
	"testing"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestParseEmbeddingWriteOnlyAcceptsLatestDoneAttempt(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXLinkRepository(pool)
	link, attemptA, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL: "https://example.com/embedding-attempt-cas", Status: model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew attempt A: %v", err)
	}
	titleA := "attempt A"
	completeEmbeddingAttempt(t, repo, link.ID, attemptA.ID, attemptA.ExpectedMetadataRevision, titleA)

	vectorA := make([]float32, 1536)
	vectorA[0] = 1
	if err := repo.UpdateLinkEmbeddingForParse(ctx, link.ID, attemptA.ID, &titleA, nil, vectorA, "model-a"); err != nil {
		t.Fatalf("write attempt A embedding: %v", err)
	}

	attemptB, err := repo.RequeueExisting(ctx, link.ID, nil)
	if err != nil {
		t.Fatalf("RequeueExistingTx attempt B: %v", err)
	}
	// Requeue clears A's vector, and A must not restore it while B is pending.
	if err := repo.UpdateLinkEmbeddingForParse(ctx, link.ID, attemptA.ID, &titleA, nil, vectorA, "model-a-late"); err != nil {
		t.Fatalf("late attempt A embedding while B pending: %v", err)
	}
	var pendingEmbedding *string
	if err := pool.QueryRow(ctx, `SELECT embedding::text FROM links WHERE id=$1`, link.ID).Scan(&pendingEmbedding); err != nil {
		t.Fatalf("read pending embedding: %v", err)
	}
	if pendingEmbedding != nil {
		t.Fatalf("pending link embedding = %q, want NULL", *pendingEmbedding)
	}

	titleB := "attempt B"
	completeEmbeddingAttempt(t, repo, link.ID, attemptB.ID, attemptB.ExpectedMetadataRevision, titleB)
	vectorB := make([]float32, 1536)
	vectorB[0] = 2
	if err := repo.UpdateLinkEmbeddingForParse(ctx, link.ID, attemptB.ID, &titleB, nil, vectorB, "model-b"); err != nil {
		t.Fatalf("write attempt B embedding: %v", err)
	}
	// A finishes last in wall-clock order, but its exact attempt CAS is stale.
	if err := repo.UpdateLinkEmbeddingForParse(ctx, link.ID, attemptA.ID, &titleA, nil, vectorA, "model-a-latest"); err != nil {
		t.Fatalf("late attempt A embedding after B done: %v", err)
	}

	var got pgvector.Vector
	var modelName string
	if err := pool.QueryRow(ctx, `SELECT embedding, embedding_model FROM links WHERE id=$1`, link.ID).Scan(&got, &modelName); err != nil {
		t.Fatalf("read final embedding: %v", err)
	}
	if modelName != "model-b" || len(got.Slice()) != 1536 || got.Slice()[0] != 2 {
		t.Fatalf("final embedding model=%q first=%v, want model-b/2", modelName, got.Slice()[0])
	}
}

func completeEmbeddingAttempt(t *testing.T, repo *repository.PGXLinkRepository, linkID, jobID uuid.UUID, expectedMetadataRevision int64, title string) {
	t.Helper()
	ctx := t.Context()
	if err := repo.MarkParseProcessing(ctx, linkID, jobID); err != nil {
		t.Fatalf("MarkParseProcessing %s: %v", jobID, err)
	}
	if err := repo.CompleteParse(ctx, repository.UpdateLinkAnalysisParams{
		ID: linkID, ExpectedMetadataRevision: expectedMetadataRevision, Title: &title, Tags: []string{}, Status: model.LinkStatusDone,
	}, jobID); err != nil {
		t.Fatalf("CompleteParse %s: %v", jobID, err)
	}
}
