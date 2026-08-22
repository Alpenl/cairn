package dbintegration

import (
	"testing"

	"webtag/internal/contentdoc"
	"webtag/internal/model"
)

func TestTranslationSummaryIdentityUsesRenderedBlockPersistenceHash(t *testing.T) {
	harness := newRF5AScheduleHarness(t)
	const summary = "Alpha beta"
	fixture := harness.createSource(t, "summary-identity", model.SavedContent{
		Text: "Saved body", Format: model.ContentFormatPlain, Words: 2,
	}, summary)

	rawBlockHash := rf5aRenderedHash(t, summary)
	verified, err := harness.service(harness.queue).Create(fixture.ctx, fixture.linkID, model.TranslationRequest{
		Scope: model.TranslationScopeSelection, BlockKey: "summary",
		StartOffset: 0, EndOffset: len(summary), SourceText: summary,
		ExpectedSourceHash: &rawBlockHash,
	})
	if err != nil || verified == nil {
		t.Fatalf("Create() = %+v, %v", verified, err)
	}

	verifiedIdentity := contentdoc.RenderedSourceBlockPersistenceHash("summary", summary)
	if verified.SourceHash != verifiedIdentity {
		t.Fatalf("summary source hash = %q, want domain-separated identity %q", verified.SourceHash, verifiedIdentity)
	}
	var rows, identities int
	if err := harness.pool.QueryRow(t.Context(), `SELECT count(*), count(DISTINCT source_hash)
		FROM link_translations WHERE link_id=$1 AND block_key='summary'`,
		fixture.linkID).Scan(&rows, &identities); err != nil {
		t.Fatalf("count summary products: %v", err)
	}
	if rows != 1 || identities != 1 {
		t.Fatalf("summary products/identities = %d/%d, want 1/1", rows, identities)
	}
}
