package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestUpdateSiteEmbeddingIsInstallScopedAndDoesNotTouchProfileTimestamp(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	id := uuid.New()
	const sql = `UPDATE sites SET embedding=$3, embedding_model=$4
WHERE id=$1 AND revision=$2 AND (embedding IS NULL OR embedding_model IS DISTINCT FROM $4)`
	mock.ExpectExec(regexp.QuoteMeta(sql)).WithArgs(id, int64(7), pgxmock.AnyArg(), "site-model").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	updated, err := NewPGXSiteRepository(mock).UpdateSiteEmbedding(context.Background(), id, 7, []float32{1, 2}, " site-model ")
	if err != nil || !updated {
		t.Fatalf("UpdateSiteEmbedding() = %v, %v", updated, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSiteEmbeddingCandidateQueryNeverReadsLinksPayload(t *testing.T) {
	// This repository query is the persistence boundary for backfill input.
	// Keep the assertion structural so future changes cannot quietly join links.
	for _, forbidden := range []string{" links", "content", "summary", "input_text", "input_html", "user_note"} {
		if regexp.MustCompile(`(?i)` + regexp.QuoteMeta(forbidden)).MatchString(listSitesNeedingEmbeddingSQL) {
			t.Fatalf("candidate query must not contain %q", forbidden)
		}
	}
}

func TestSiteEmbeddingQueriesDoNotDependOnTenantIdentity(t *testing.T) {
	for _, query := range []string{listSitesNeedingEmbeddingSQL, `UPDATE sites SET embedding=$3, embedding_model=$4 WHERE id=$1 AND revision=$2`} {
		if regexp.MustCompile(`(?i)tenant`).MatchString(query) {
			t.Fatalf("site embedding query retains tenant identity: %s", query)
		}
	}
}

func TestSiteEmbeddingInputMutationsInvalidateBeforeAdvancingRevision(t *testing.T) {
	queries := map[string]string{
		"profile":          updateSiteProfileSQL,
		"entry":            updateSiteManagementRevisionSQL,
		"entry deletion":   clearManagedPrimaryEntrySQL,
		"entry attachment": invalidateSiteEmbeddingSQL,
		"merge":            mergeTargetSQL,
		"split source":     splitUpdateSourceSQL,
		"conversion":       advanceSiteForConversionSQL,
	}
	for name, query := range queries {
		if !regexp.MustCompile(`(?i)embedding\s*=\s*null`).MatchString(query) ||
			!regexp.MustCompile(`(?i)embedding_model\s*=\s*null`).MatchString(query) {
			t.Errorf("%s mutation does not invalidate site embedding: %s", name, query)
		}
	}
}
