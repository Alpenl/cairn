package dbintegration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestConfirmAIProposalsSelectsOnlyCurrentCompletedRowsInRequestedPartition(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXReaderVNextRepository(pool)
	base := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)

	activeFirst := seedConfirmAIProposalCandidate(t, pool, repo, ctx, "active-first", "First active", "completed", 0, false, base)
	activeSecond := seedConfirmAIProposalCandidate(t, pool, repo, ctx, "active-second", "Second active", "completed", 0, false, base.Add(time.Minute))
	stale := seedConfirmAIProposalCandidate(t, pool, repo, ctx, "stale", "Stale completed", "completed", 1, false, base.Add(2*time.Minute))
	running := seedConfirmAIProposalCandidate(t, pool, repo, ctx, "running", "Running", "running", 0, false, base.Add(3*time.Minute))
	failed := seedConfirmAIProposalCandidate(t, pool, repo, ctx, "failed", "Failed", "failed", 0, false, base.Add(4*time.Minute))
	blank := seedConfirmAIProposalCandidate(t, pool, repo, ctx, "blank", "  ", "completed", 0, false, base.Add(5*time.Minute))
	expired := seedConfirmAIProposalCandidate(t, pool, repo, ctx, "expired", "Expired only", "completed", 0, true, base.Add(6*time.Minute))

	activeResult, err := repo.ConfirmAIProposals(ctx, model.ReaderInboxPartitionActive)
	if err != nil {
		t.Fatalf("ConfirmAIProposals(active): %v", err)
	}
	if len(activeResult.Items) != 2 || activeResult.Items[0].ID != activeFirst || activeResult.Items[1].ID != activeSecond || activeResult.Items[0].LinkID == nil || activeResult.Items[1].LinkID == nil || activeResult.RemainingCount != 0 {
		t.Fatalf("active confirmation result = %#v", activeResult)
	}
	for _, id := range []uuid.UUID{activeFirst, activeSecond} {
		if status := confirmAIInboxStatus(t, pool, id); status != "confirmed" {
			t.Fatalf("active eligible Inbox %s status = %q, want confirmed", id, status)
		}
	}
	for _, id := range []uuid.UUID{stale, running, failed, blank, expired} {
		if status := confirmAIInboxStatus(t, pool, id); status != "pending" {
			t.Fatalf("excluded Inbox %s status = %q, want pending", id, status)
		}
	}

	activeRetry, err := repo.ConfirmAIProposals(ctx, model.ReaderInboxPartitionActive)
	if err != nil {
		t.Fatalf("ConfirmAIProposals(active retry): %v", err)
	}
	if len(activeRetry.Items) != 0 || activeRetry.RemainingCount != 0 {
		t.Fatalf("active confirmation retry = %#v, want empty idempotent result", activeRetry)
	}

	expiredResult, err := repo.ConfirmAIProposals(ctx, model.ReaderInboxPartitionExpired)
	if err != nil {
		t.Fatalf("ConfirmAIProposals(expired): %v", err)
	}
	if len(expiredResult.Items) != 1 || expiredResult.Items[0].ID != expired || expiredResult.Items[0].LinkID == nil || expiredResult.RemainingCount != 0 {
		t.Fatalf("expired confirmation result = %#v", expiredResult)
	}
	if status := confirmAIInboxStatus(t, pool, expired); status != "confirmed" {
		t.Fatalf("expired eligible Inbox status = %q, want confirmed", status)
	}
}

func TestConfirmAIProposalsRestoresAndAdoptsFeedManagedTrashLink(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXReaderVNextRepository(pool)
	const label = "feed-trash-restore"
	rawURL := "https://confirm-ai.example/" + label
	feedItem := seedReaderFeedSaveItem(t, pool, rawURL, "confirm-ai-feed-trash")
	saved, err := repo.FeedbackFeed(ctx, "subscription:"+feedItem.String(), "save")
	if err != nil || saved.Association == nil {
		t.Fatalf("seed AI Feed save = %#v, %v", saved, err)
	}
	linkID := saved.Association.LinkID
	if _, err := repo.FeedbackFeed(ctx, "subscription:"+feedItem.String(), "unsave"); err != nil {
		t.Fatalf("seed AI Feed trash: %v", err)
	}
	assertReaderFeedLinkLive(t, pool, linkID, false)

	inboxID := seedConfirmAIProposalCandidate(
		t, pool, repo, ctx, label, "Restore AI proposal", "completed", 0, false,
		time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC),
	)
	result, err := repo.ConfirmAIProposals(ctx, model.ReaderInboxPartitionActive)
	if err != nil {
		t.Fatalf("ConfirmAIProposals(): %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != inboxID || result.Items[0].LinkID == nil || *result.Items[0].LinkID != linkID {
		t.Fatalf("AI Trash confirmation = %#v, want Inbox %s and Link %s", result, inboxID, linkID)
	}
	assertReaderFeedLinkLive(t, pool, linkID, true)
	var feedManaged bool
	if err := pool.QueryRow(ctx, `SELECT feed_managed FROM links WHERE id=$1`, linkID).Scan(&feedManaged); err != nil {
		t.Fatalf("read AI-confirmed ownership: %v", err)
	}
	if feedManaged {
		t.Fatal("AI-confirmed Link retained Feed-exclusive ownership")
	}
}

func TestConfirmAIProposalsBatchesBacklogAtomicallyAfterFailedBatch(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	repo := repository.NewPGXReaderVNextRepository(pool)
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	const total = 201
	ids := make([]uuid.UUID, 0, total)
	for index := 0; index < total; index++ {
		label := fmt.Sprintf("backlog-%03d", index)
		ids = append(ids, seedConfirmAIProposalCandidate(
			t,
			pool,
			repo,
			ctx,
			label,
			"Backlog "+label,
			"completed",
			0,
			false,
			base.Add(time.Duration(index)*time.Minute),
		))
	}

	first, err := repo.ConfirmAIProposals(ctx, model.ReaderInboxPartitionActive)
	if err != nil {
		t.Fatalf("ConfirmAIProposals(first batch): %v", err)
	}
	assertConfirmAIProposalBatch(t, first, ids[:100], 101)
	assertConfirmAIProposalDatabaseState(t, pool, 100, 101, 100)

	installConfirmAIProposalFailureTrigger(t, pool, ids[100])
	if _, err := repo.ConfirmAIProposals(ctx, model.ReaderInboxPartitionActive); err == nil {
		t.Fatal("ConfirmAIProposals(failed second batch) error = nil, want injected rollback")
	}
	assertConfirmAIProposalDatabaseState(t, pool, 100, 101, 100)
	dropConfirmAIProposalFailureTrigger(t, pool)

	second, err := repo.ConfirmAIProposals(ctx, model.ReaderInboxPartitionActive)
	if err != nil {
		t.Fatalf("ConfirmAIProposals(retry second batch): %v", err)
	}
	assertConfirmAIProposalBatch(t, second, ids[100:200], 1)
	assertConfirmAIProposalDatabaseState(t, pool, 200, 1, 200)

	third, err := repo.ConfirmAIProposals(ctx, model.ReaderInboxPartitionActive)
	if err != nil {
		t.Fatalf("ConfirmAIProposals(third batch): %v", err)
	}
	assertConfirmAIProposalBatch(t, third, ids[200:], 0)
	assertConfirmAIProposalDatabaseState(t, pool, 201, 0, 201)

	confirmed := append(append(append([]uuid.UUID{}, readerInboxConfirmationIDs(first)...), readerInboxConfirmationIDs(second)...), readerInboxConfirmationIDs(third)...)
	if len(confirmed) != len(ids) {
		t.Fatalf("confirmed IDs = %d, want %d", len(confirmed), len(ids))
	}
	seen := make(map[uuid.UUID]struct{}, len(confirmed))
	for index, id := range confirmed {
		if id != ids[index] {
			t.Fatalf("confirmed ID %d = %s, want stable server order %s", index, id, ids[index])
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("confirmed ID %s was repeated", id)
		}
		seen[id] = struct{}{}
	}
}

func assertConfirmAIProposalBatch(t *testing.T, got model.ReaderInboxAIProposalConfirmation, want []uuid.UUID, remaining int) {
	t.Helper()
	if len(got.Items) != len(want) || got.RemainingCount != remaining {
		t.Fatalf("AI proposal batch = items:%d remaining:%d, want items:%d remaining:%d", len(got.Items), got.RemainingCount, len(want), remaining)
	}
	for index, result := range got.Items {
		if result.ID != want[index] || result.Status != "confirmed" || result.LinkID == nil {
			t.Fatalf("AI proposal batch item %d = %#v, want confirmed %s with a link", index, result, want[index])
		}
	}
}

func readerInboxConfirmationIDs(result model.ReaderInboxAIProposalConfirmation) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(result.Items))
	for _, item := range result.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

func assertConfirmAIProposalDatabaseState(t *testing.T, pool *pgxpool.Pool, wantConfirmed, wantPending, wantLinks int) {
	t.Helper()
	var confirmed, pending, links int
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE status='confirmed')::int,
			count(*) FILTER (WHERE status='pending')::int
		FROM reader_inbox`).Scan(&confirmed, &pending); err != nil {
		t.Fatalf("count Inbox batch states: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*)::int FROM links`).Scan(&links); err != nil {
		t.Fatalf("count confirmed links: %v", err)
	}
	if confirmed != wantConfirmed || pending != wantPending || links != wantLinks {
		t.Fatalf("AI batch database state = confirmed:%d pending:%d links:%d, want %d/%d/%d", confirmed, pending, links, wantConfirmed, wantPending, wantLinks)
	}
}

func installConfirmAIProposalFailureTrigger(t *testing.T, pool *pgxpool.Pool, inboxID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION fail_confirm_ai_proposal_batch() RETURNS trigger AS $$
		BEGIN
			IF NEW.id = '%s'::uuid AND OLD.status='pending' AND NEW.status='confirmed' THEN
				RAISE EXCEPTION 'forced AI proposal batch failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER fail_confirm_ai_proposal_batch
			BEFORE UPDATE OF status ON reader_inbox
			FOR EACH ROW EXECUTE FUNCTION fail_confirm_ai_proposal_batch();
	`, inboxID)); err != nil {
		t.Fatalf("install AI proposal failure trigger: %v", err)
	}
}

func dropConfirmAIProposalFailureTrigger(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		DROP TRIGGER fail_confirm_ai_proposal_batch ON reader_inbox;
		DROP FUNCTION fail_confirm_ai_proposal_batch();
	`); err != nil {
		t.Fatalf("drop AI proposal failure trigger: %v", err)
	}
}

func seedConfirmAIProposalCandidate(t *testing.T, pool *pgxpool.Pool, repo *repository.PGXReaderVNextRepository, ctx context.Context, label, title, jobStatus string, expectedRevisionOffset int64, expired bool, createdAt time.Time) uuid.UUID {
	t.Helper()
	url := "https://confirm-ai.example/" + label
	created, err := repo.CreateInbox(ctx, model.ReaderInbox{
		URL:             url,
		IdentityKey:     url,
		SourceKind:      "url",
		Title:           &title,
		Body:            "body " + label,
		ProposalSignals: json.RawMessage(`{}`),
		ProposalStatus:  jobStatus,
	})
	if err != nil {
		t.Fatalf("CreateInbox(%s): %v", label, err)
	}
	jobID := uuid.New()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO reader_inbox_jobs (id,inbox_id,expected_metadata_revision,status)
		VALUES ($1,$2,$3,$4)`, jobID, created.ID, created.MetadataRevision+expectedRevisionOffset, jobStatus); err != nil {
		t.Fatalf("insert %s Inbox job: %v", label, err)
	}
	if _, err := pool.Exec(t.Context(), `
		UPDATE reader_inbox
		SET job_id=$2,proposal_status=$3,created_at=$4,expired_at=CASE WHEN $5 THEN NOW() ELSE NULL END
		WHERE id=$1`, created.ID, jobID, jobStatus, createdAt, expired); err != nil {
		t.Fatalf("configure %s Inbox candidate: %v", label, err)
	}
	return created.ID
}

func confirmAIInboxStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(t.Context(), `SELECT status FROM reader_inbox WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatalf("read Inbox %s status: %v", id, err)
	}
	return status
}
