package dbintegration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

func TestDurableDeleteLinkReplayPreservesTerminalTranslationHistory(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	linkID := seedDeleteAcceptanceTerminalHistory(t, pool)
	wantHistory := readDeleteAcceptanceHistory(t, pool, linkID)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, repository.NewPGXLinkRepository(pool), queue)

	if err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID}); err != nil {
		t.Fatalf("first DeleteLink() error = %v", err)
	}
	assertDeleteAcceptanceHistory(t, wantHistory, readDeleteAcceptanceHistory(t, pool, linkID))
	firstDeletedAt, firstUpdatedAt := readDeleteAcceptanceLinkTimestamps(t, pool, linkID)

	if err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID}); err != nil {
		t.Fatalf("second DeleteLink() error = %v", err)
	}
	assertDeleteAcceptanceHistory(t, wantHistory, readDeleteAcceptanceHistory(t, pool, linkID))
	secondDeletedAt, secondUpdatedAt := readDeleteAcceptanceLinkTimestamps(t, pool, linkID)
	if !secondDeletedAt.Equal(firstDeletedAt) || !secondUpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf(
			"replayed DeleteLink changed Link timestamps: first deleted/updated=%s/%s second=%s/%s",
			firstDeletedAt, firstUpdatedAt, secondDeletedAt, secondUpdatedAt,
		)
	}
}

func TestDurableDeleteLinkRollsBackLifecycleWhenSoftDeleteFails(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	linkID, attempt := insertPendingLinkAttempt(
		t, pool, "https://durable.example.com/delete-final-update-rollback",
	)
	repo := repository.NewPGXLinkRepository(pool)
	if err := repo.MarkParseProcessing(ctx, attempt); err != nil {
		t.Fatalf("MarkParseProcessing(): %v", err)
	}
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(ctx, attempt); err != nil {
		t.Fatalf("enqueue rollback parse attempt: %v", err)
	}
	translationID := schedulePendingTranslation(
		t, pool, queue, ctx, linkID, "translation must survive final Link delete failure",
	)
	var translationRiverJobID int64
	if err := pool.QueryRow(ctx,
		`SELECT current_river_job_id FROM link_translations WHERE id=$1`,
		translationID,
	).Scan(&translationRiverJobID); err != nil {
		t.Fatalf("read rollback translation River job: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_delete_acceptance_soft_delete() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
				RAISE EXCEPTION 'injected final Link soft-delete failure';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER fail_delete_acceptance_soft_delete
		BEFORE UPDATE OF deleted_at ON links
		FOR EACH ROW EXECUTE FUNCTION fail_delete_acceptance_soft_delete()`); err != nil {
		t.Fatalf("install final soft-delete failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS fail_delete_acceptance_soft_delete ON links;
			DROP FUNCTION IF EXISTS fail_delete_acceptance_soft_delete()`)
	})

	err := dbLinkCommands(pool, repo, queue).DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID})
	if err == nil || !strings.Contains(err.Error(), "injected final Link soft-delete failure") {
		t.Fatalf("DeleteLink(final update failure) error = %v, want injected failure", err)
	}

	var (
		linkStatus model.LinkStatus
		generation int64
		trashed    bool
	)
	if err := pool.QueryRow(ctx,
		`SELECT status,parse_generation,deleted_at IS NOT NULL FROM links WHERE id=$1`,
		linkID,
	).Scan(&linkStatus, &generation, &trashed); err != nil {
		t.Fatalf("read Link after final update rollback: %v", err)
	}
	if linkStatus != model.LinkStatusProcessing || generation != attempt.Generation || trashed {
		t.Fatalf("Link after rollback = %s generation=%d trashed=%v, want processing/%d/false",
			linkStatus, generation, trashed, attempt.Generation)
	}

	var translationStatus model.TranslationStatus
	var translationReason string
	var currentRiverJobID *int64
	if err := pool.QueryRow(ctx, `
		SELECT status,COALESCE(error_msg,''),current_river_job_id
		FROM link_translations
		WHERE id=$1`, translationID).Scan(
		&translationStatus, &translationReason, &currentRiverJobID,
	); err != nil {
		t.Fatalf("read translation after final update rollback: %v", err)
	}
	if translationStatus != model.TranslationStatusPending || translationReason != "" ||
		currentRiverJobID == nil || *currentRiverJobID != translationRiverJobID {
		t.Fatalf("translation after rollback = %s/%q current=%v, want pending/empty/%d",
			translationStatus, translationReason, currentRiverJobID, translationRiverJobID)
	}
	assertActiveRiverAttempt(t, pool, attempt)
	assertDeleteAcceptanceTranslationRiverActive(t, pool, translationID)
}

type deleteAcceptanceHistoryRow struct {
	status    string
	errorMsg  string
	updatedAt time.Time
}

func seedDeleteAcceptanceTerminalHistory(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	var linkID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO links (url,source_key,status,first_collected_at)
		VALUES ('https://durable.example.com/delete-history-acceptance',
		        'https://durable.example.com/delete-history-acceptance','done',NOW())
		RETURNING id`).Scan(&linkID); err != nil {
		t.Fatalf("seed terminal-history Link: %v", err)
	}
	doneTranslationID := uuid.New()
	failedTranslationID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO link_translations (
			id,link_id,scope,block_key,start_offset,end_offset,source_text,
			source_hash,status,error_msg,updated_at
		) VALUES
			($1,$3,'selection','summary',0,4,'done',
			 repeat('a',64),'done','historical_translation_done','2025-03-04T05:06:07Z'),
			($2,$3,'selection','summary',0,4,'fail',
			 repeat('b',64),'failed','historical_translation_failed','2025-04-05T06:07:08Z')`,
		doneTranslationID, failedTranslationID, linkID,
	); err != nil {
		t.Fatalf("seed terminal translation history: %v", err)
	}
	return linkID
}

func readDeleteAcceptanceHistory(
	t *testing.T,
	pool *pgxpool.Pool,
	linkID uuid.UUID,
) map[uuid.UUID]deleteAcceptanceHistoryRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT id,status,COALESCE(error_msg,''),updated_at
		FROM link_translations WHERE link_id=$1`, linkID)
	if err != nil {
		t.Fatalf("read terminal translation history: %v", err)
	}
	defer rows.Close()
	history := make(map[uuid.UUID]deleteAcceptanceHistoryRow)
	for rows.Next() {
		var key uuid.UUID
		var item deleteAcceptanceHistoryRow
		if err := rows.Scan(&key, &item.status, &item.errorMsg, &item.updatedAt); err != nil {
			t.Fatalf("scan terminal translation history: %v", err)
		}
		history[key] = item
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate terminal translation history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("terminal translation history rows = %d, want 2", len(history))
	}
	return history
}

func assertDeleteAcceptanceHistory(
	t *testing.T,
	want map[uuid.UUID]deleteAcceptanceHistoryRow,
	got map[uuid.UUID]deleteAcceptanceHistoryRow,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("terminal history rows = %d, want %d", len(got), len(want))
	}
	for key, wantRow := range want {
		gotRow, ok := got[key]
		if !ok {
			t.Fatalf("terminal history row %s disappeared", key)
		}
		if gotRow.status != wantRow.status || gotRow.errorMsg != wantRow.errorMsg ||
			!gotRow.updatedAt.Equal(wantRow.updatedAt) {
			t.Fatalf("terminal history row %s = %#v, want %#v", key, gotRow, wantRow)
		}
	}
}

func readDeleteAcceptanceLinkTimestamps(
	t *testing.T,
	pool *pgxpool.Pool,
	linkID uuid.UUID,
) (time.Time, time.Time) {
	t.Helper()
	var deletedAt time.Time
	var updatedAt time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT deleted_at,updated_at FROM links WHERE id=$1`, linkID,
	).Scan(&deletedAt, &updatedAt); err != nil {
		t.Fatalf("read deleted Link timestamps: %v", err)
	}
	return deletedAt, updatedAt
}

func assertDeleteAcceptanceTranslationRiverActive(
	t *testing.T,
	pool *pgxpool.Pool,
	translationID uuid.UUID,
) {
	t.Helper()
	var state string
	var marked bool
	if err := pool.QueryRow(t.Context(), `
		SELECT state::text,metadata ? 'cancel_attempted_at'
		FROM river_job
		WHERE kind='translate_link_v2' AND args->>'translation_id'=$1`,
		translationID.String(),
	).Scan(&state, &marked); err != nil {
		t.Fatalf("read active translation River job for %s: %v", translationID, err)
	}
	active := state == "available" || state == "pending" || state == "retryable" ||
		state == "running" || state == "scheduled"
	if !active || marked {
		t.Fatalf("translation River job for %s = state %q marked=%v, want active/unmarked",
			translationID, state, marked)
	}
}
