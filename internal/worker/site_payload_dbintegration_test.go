//go:build dbintegration

package worker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestWebsiteCollectionPayloadRetention uses the disposable database supplied
// by the dbintegration target. It deliberately exercises persisted rows, not
// mocks, so the original-content check and the crash compensator are both
// covered by PostgreSQL's actual constraints and predicates.
func TestWebsiteCollectionPayloadRetention(t *testing.T) {
	dsn := os.Getenv("WEBTAG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WEBTAG_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}

	repo := repository.NewPGXLinkRepository(pool)

	successLinkID := seedPayloadIntegrationLink(t, ctx, pool, "success", "pending", nil)
	classification := repository.UpdateLibraryClassificationParams{ID: successLinkID, Kind: model.LibraryKindSite}
	if _, err := repo.CompleteSiteParse(ctx, repository.CompleteSiteParseParams{
		Analysis:       repository.UpdateLinkAnalysisParams{ID: successLinkID, Title: stringRef("Payload Example"), Tags: []string{"tool"}, FetcherType: stringRef("http"), Domain: stringRef("payload.example"), ContentType: stringRef("homepage"), ExpectedParseGeneration: 1, ExpectedMetadataRevision: 1},
		Classification: classification,
		Site:           repository.AggregateSiteParams{LinkID: successLinkID, IdentityKey: "v1:host:payload-success-" + uuid.NewString(), NormalizedURL: "https://payload.example/success", Name: "Payload Example", EntryName: "Payload Example"},
	}); err != nil {
		t.Fatalf("complete site parse: %v", err)
	}
	assertPayloadState(t, ctx, pool, successLinkID, "site", "done", false, true)

	failureLinkID := seedPayloadIntegrationLink(t, ctx, pool, "failure", "pending", stringRef("site"))
	if err := repo.MarkParseFailed(ctx, model.ParseAttempt{LinkID: failureLinkID, Generation: 1, ExpectedMetadataRevision: 1}, "expected parse failure"); err != nil {
		t.Fatalf("mark predicted-site parse failed: %v", err)
	}
	assertPayloadState(t, ctx, pool, failureLinkID, "site", "failed", false, true)

	stuckLinkID := seedPayloadIntegrationLink(t, ctx, pool, "stuck", "processing", stringRef("site"))
	if _, err := pool.Exec(ctx, "UPDATE links SET payload_purge_due_at = NOW() - INTERVAL '1 minute' WHERE id = $1", stuckLinkID); err != nil {
		t.Fatalf("expire stuck site payload: %v", err)
	}
	readingLinkID := seedPayloadIntegrationLink(t, ctx, pool, "reading", "processing", stringRef("reading"))
	if _, err := pool.Exec(ctx, "UPDATE links SET payload_purge_due_at = NOW() - INTERVAL '1 minute' WHERE id = $1", readingLinkID); err != nil {
		t.Fatalf("expire reading payload: %v", err)
	}
	cleaner, err := NewSitePayloadCleaner(SitePayloadCleanerOptions{Pool: pool, Batch: 20})
	if err != nil {
		t.Fatalf("construct payload cleaner: %v", err)
	}
	purged, err := cleaner.RunOnce(ctx)
	if err != nil {
		t.Fatalf("run stuck payload cleaner: %v", err)
	}
	if purged != 1 {
		t.Fatalf("stuck payload cleaner purged %d rows, want 1", purged)
	}
	assertPayloadState(t, ctx, pool, stuckLinkID, "site", "processing", false, true)
	assertPayloadState(t, ctx, pool, readingLinkID, "reading", "processing", true, false)

	// The database constraint remains the last line of defense even if an
	// application path accidentally attempts to persist a site summary.
	if _, err := pool.Exec(ctx, "UPDATE links SET summary = 'must be rejected' WHERE id = $1", successLinkID); err == nil {
		t.Fatal("site original-content constraint accepted a non-null summary")
	}
}

func seedPayloadIntegrationLink(t *testing.T, ctx context.Context, pool *pgxpool.Pool, suffix, status string, kind *string) uuid.UUID {
	t.Helper()
	linkID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO links (id, url, source_key, status, input_text, input_html, input_images, source_metadata, library_kind, library_kind_locked, first_collected_at)
VALUES ($1, $2, $2, $3, 'captured body', '<main>captured body</main>', '["https://payload.example/image.png"]'::jsonb, '{"capture_source":"dbintegration"}'::jsonb, $4, $4::text IS NOT NULL, NOW())`, linkID, "https://payload.example/"+suffix+"/"+uuid.NewString(), status, kind); err != nil {
		t.Fatalf("seed %s link: %v", suffix, err)
	}
	return linkID
}

func assertPayloadState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, linkID uuid.UUID, wantKind, wantStatus string, wantPayload, wantPurged bool) {
	t.Helper()
	var kind *string
	var status string
	var inputText, inputHTML *string
	var purgedAt *time.Time
	if err := pool.QueryRow(ctx, "SELECT library_kind, status, input_text, input_html, payload_purged_at FROM links WHERE id = $1", linkID).Scan(&kind, &status, &inputText, &inputHTML, &purgedAt); err != nil {
		t.Fatalf("read payload state: %v", err)
	}
	gotKind := ""
	if kind != nil {
		gotKind = *kind
	}
	if gotKind != wantKind || status != wantStatus {
		t.Fatalf("payload state kind=%q status=%q, want kind=%q status=%q", gotKind, status, wantKind, wantStatus)
	}
	if (inputText != nil || inputHTML != nil) != wantPayload {
		t.Fatalf("payload presence text=%v html=%v, want payload=%v", inputText != nil, inputHTML != nil, wantPayload)
	}
	if (purgedAt != nil) != wantPurged {
		t.Fatalf("payload purged_at=%v, want present=%v", purgedAt, wantPurged)
	}
}

func stringRef(value string) *string { return &value }
