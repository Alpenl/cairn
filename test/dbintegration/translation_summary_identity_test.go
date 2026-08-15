package dbintegration

import (
	"testing"

	"webtag/internal/contentdoc"
	"webtag/internal/model"
)

func TestTranslationSummaryIdentitySeparatesVerifiedBlockFromHistoricalSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract bool
	}{
		{name: "expand", contract: false},
		{name: "contract", contract: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newRF5AScheduleHarness(t, tc.contract)
			const summary = "Alpha beta"
			fixture := harness.createSource(t, "summary-identity-"+tc.name, model.SavedContent{
				Text: "Saved body", Format: model.ContentFormatPlain, Words: 2,
			}, summary)

			request := model.TranslationRequest{
				Scope: model.TranslationScopeSelection, BlockKey: "summary",
				StartOffset: 0, EndOffset: len(summary), SourceText: summary,
			}
			legacy, err := harness.service(false, harness.queue).Create(fixture.ctx, fixture.linkID, request)
			if err != nil || legacy == nil {
				t.Fatalf("compat Create() = %+v, %v", legacy, err)
			}

			rawBlockHash := rf5aRenderedHash(t, summary)
			request.ExpectedSourceHash = &rawBlockHash
			verified, err := harness.service(true, harness.queue).Create(fixture.ctx, fixture.linkID, request)
			if err != nil || verified == nil {
				t.Fatalf("verified Create() = %+v, %v", verified, err)
			}

			verifiedIdentity := contentdoc.RenderedSourceBlockPersistenceHash("summary", summary)
			if legacy.ID == verified.ID || legacy.SourceHash != rawBlockHash ||
				verified.SourceHash != verifiedIdentity || verified.SourceHash == legacy.SourceHash {
				t.Fatalf("summary identities collided: legacy=%+v verified=%+v", legacy, verified)
			}
			var rows, identities int
			if err := harness.pool.QueryRow(t.Context(), `SELECT count(*), count(DISTINCT source_hash)
				FROM link_translations WHERE link_id=$1 AND block_key='summary'`,
				fixture.linkID).Scan(&rows, &identities); err != nil {
				t.Fatalf("count summary products: %v", err)
			}
			if rows != 2 || identities != 2 {
				t.Fatalf("summary products/identities = %d/%d, want 2/2", rows, identities)
			}
		})
	}
}
