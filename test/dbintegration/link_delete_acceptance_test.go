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

func TestDurableDeleteLinkTerminalizesProcessingAttemptAndCancelsRiver(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	linkID, parseJobID := insertPendingLinkAndJob(
		t,
		pool,
		"https://durable.example.com/delete-processing-acceptance",
	)
	processor := &cancellationAwareProcessor{
		targetJobID: parseJobID,
		started:     make(chan struct{}),
		cancelled:   make(chan struct{}),
	}
	queue := newRiverQueue(t, pool, processor)
	if err := queue.Enqueue(ctx, linkID, parseJobID); err != nil {
		t.Fatalf("enqueue processing attempt: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE parse_jobs SET status='processing',updated_at=NOW() WHERE id=$1`,
		parseJobID,
	); err != nil {
		t.Fatalf("mark parse attempt processing: %v", err)
	}
	if err := queue.Start(ctx); err != nil {
		t.Fatalf("start River queue: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = queue.Stop(stopCtx)
	})
	select {
	case <-processor.started:
	case <-time.After(10 * time.Second):
		t.Fatal("processing River attempt did not start")
	}

	commands := dbLinkCommands(pool, repository.NewPGXLinkRepository(pool), queue)
	if err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID}); err != nil {
		t.Fatalf("DeleteLink(processing) error = %v", err)
	}

	var trashed bool
	if err := pool.QueryRow(ctx,
		`SELECT deleted_at IS NOT NULL FROM links WHERE id=$1`,
		linkID,
	).Scan(&trashed); err != nil {
		t.Fatalf("read deleted Link: %v", err)
	}
	if !trashed {
		t.Fatal("DeleteLink(processing) left Link live")
	}
	var status model.JobStatus
	var reason string
	if err := pool.QueryRow(ctx,
		`SELECT status,COALESCE(error_msg,'') FROM parse_jobs WHERE id=$1`,
		parseJobID,
	).Scan(&status, &reason); err != nil {
		t.Fatalf("read deleted processing attempt: %v", err)
	}
	if status != model.JobStatusFailed || reason != "link_deleted" {
		t.Fatalf("deleted processing attempt = %s/%q, want failed/link_deleted", status, reason)
	}
	assertDeleteAcceptanceRiverCancellationMarked(t, pool, "parse_link", "parse_job_id", parseJobID)

	select {
	case <-processor.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("DeleteLink did not cancel the running River worker context")
	}
	assertDeleteAcceptanceRiverEventuallyCancelled(t, pool, "parse_link", "parse_job_id", parseJobID)
}

func TestDurableDeleteLinkReplayPreservesTerminalAttemptHistory(t *testing.T) {
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
			firstDeletedAt,
			firstUpdatedAt,
			secondDeletedAt,
			secondUpdatedAt,
		)
	}
}

func TestDurableDeleteLinkRollsBackAllLifecycleChangesWhenSoftDeleteFails(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	linkID, parseJobID := insertPendingLinkAndJob(
		t,
		pool,
		"https://durable.example.com/delete-final-update-rollback",
	)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	if err := queue.Enqueue(ctx, linkID, parseJobID); err != nil {
		t.Fatalf("enqueue rollback parse attempt: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE parse_jobs SET status='processing',updated_at=NOW() WHERE id=$1`,
		parseJobID,
	); err != nil {
		t.Fatalf("mark rollback parse attempt processing: %v", err)
	}
	translationID := schedulePendingTranslation(
		t,
		pool,
		queue,
		ctx,
		linkID,
		"translation must survive final Link delete failure",
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

	commands := dbLinkCommands(pool, repository.NewPGXLinkRepository(pool), queue)
	err := commands.DeleteLink(ctx, service.DeleteLinkCommand{LinkID: linkID})
	if err == nil || !strings.Contains(err.Error(), "injected final Link soft-delete failure") {
		t.Fatalf("DeleteLink(final update failure) error = %v, want injected failure", err)
	}

	var linkStatus model.LinkStatus
	var trashed bool
	if err := pool.QueryRow(ctx,
		`SELECT status,deleted_at IS NOT NULL FROM links WHERE id=$1`,
		linkID,
	).Scan(&linkStatus, &trashed); err != nil {
		t.Fatalf("read Link after final update rollback: %v", err)
	}
	if linkStatus != model.LinkStatusPending || trashed {
		t.Fatalf("Link after final update rollback = %s trashed=%v, want pending/false", linkStatus, trashed)
	}

	var parseStatus model.JobStatus
	var parseReason string
	if err := pool.QueryRow(ctx,
		`SELECT status,COALESCE(error_msg,'') FROM parse_jobs WHERE id=$1`,
		parseJobID,
	).Scan(&parseStatus, &parseReason); err != nil {
		t.Fatalf("read parse attempt after final update rollback: %v", err)
	}
	if parseStatus != model.JobStatusProcessing || parseReason != "" {
		t.Fatalf("parse attempt after final update rollback = %s/%q, want processing/empty", parseStatus, parseReason)
	}

	var translationStatus model.TranslationStatus
	var translationReason string
	var currentRiverJobID *int64
	if err := pool.QueryRow(ctx, `
		SELECT status,COALESCE(error_msg,''),current_river_job_id
		FROM link_translations
		WHERE id=$1`, translationID).Scan(
		&translationStatus,
		&translationReason,
		&currentRiverJobID,
	); err != nil {
		t.Fatalf("read translation after final update rollback: %v", err)
	}
	if translationStatus != model.TranslationStatusPending || translationReason != "" ||
		currentRiverJobID == nil || *currentRiverJobID != translationRiverJobID {
		t.Fatalf(
			"translation after final update rollback = %s/%q current=%v, want pending/empty/%d",
			translationStatus,
			translationReason,
			currentRiverJobID,
			translationRiverJobID,
		)
	}
	assertDeleteAcceptanceRiverActive(t, pool, "parse_link", "parse_job_id", parseJobID)
	assertDeleteAcceptanceRiverActive(t, pool, "translate_link_v2", "translation_id", translationID)
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
	doneParseID := uuid.New()
	failedParseID := uuid.New()
	doneTranslationID := uuid.New()
	failedTranslationID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO parse_jobs (id,link_id,status,error_msg,updated_at) VALUES
			($1,$3,'done','historical_parse_done','2025-01-02T03:04:05Z'),
			($2,$3,'failed','historical_parse_failed','2025-02-03T04:05:06Z')`,
		doneParseID,
		failedParseID,
		linkID,
	); err != nil {
		t.Fatalf("seed terminal parse history: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO link_translations (
			id,link_id,scope,block_key,start_offset,end_offset,source_text,
			source_hash,status,error_msg,updated_at
		) VALUES
			($1,$3,'selection','history-done',0,4,'done',
			 repeat('a',64),'done','historical_translation_done','2025-03-04T05:06:07Z'),
			($2,$3,'selection','history-failed',0,4,'fail',
			 repeat('b',64),'failed','historical_translation_failed','2025-04-05T06:07:08Z')`,
		doneTranslationID,
		failedTranslationID,
		linkID,
	); err != nil {
		t.Fatalf("seed terminal translation history: %v", err)
	}
	return linkID
}

func readDeleteAcceptanceHistory(
	t *testing.T,
	pool *pgxpool.Pool,
	linkID uuid.UUID,
) map[string]deleteAcceptanceHistoryRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT 'parse:' || id::text,status,COALESCE(error_msg,''),updated_at
		FROM parse_jobs WHERE link_id=$1
		UNION ALL
		SELECT 'translation:' || id::text,status,COALESCE(error_msg,''),updated_at
		FROM link_translations WHERE link_id=$1`, linkID)
	if err != nil {
		t.Fatalf("read terminal attempt history: %v", err)
	}
	defer rows.Close()
	history := make(map[string]deleteAcceptanceHistoryRow)
	for rows.Next() {
		var key string
		var item deleteAcceptanceHistoryRow
		if err := rows.Scan(&key, &item.status, &item.errorMsg, &item.updatedAt); err != nil {
			t.Fatalf("scan terminal attempt history: %v", err)
		}
		history[key] = item
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate terminal attempt history: %v", err)
	}
	if len(history) != 4 {
		t.Fatalf("terminal attempt history rows = %d, want 4", len(history))
	}
	return history
}

func assertDeleteAcceptanceHistory(
	t *testing.T,
	want map[string]deleteAcceptanceHistoryRow,
	got map[string]deleteAcceptanceHistoryRow,
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
		`SELECT deleted_at,updated_at FROM links WHERE id=$1`,
		linkID,
	).Scan(&deletedAt, &updatedAt); err != nil {
		t.Fatalf("read deleted Link timestamps: %v", err)
	}
	return deletedAt, updatedAt
}

func assertDeleteAcceptanceRiverCancellationMarked(
	t *testing.T,
	pool *pgxpool.Pool,
	kind string,
	argKey string,
	attemptID uuid.UUID,
) {
	t.Helper()
	var marked bool
	if err := pool.QueryRow(t.Context(), `
		SELECT metadata ? 'cancel_attempted_at'
		FROM river_job
		WHERE kind=$1 AND args->>$2=$3`, kind, argKey, attemptID.String()).Scan(&marked); err != nil {
		t.Fatalf("read River cancellation marker for %s/%s: %v", kind, attemptID, err)
	}
	if !marked {
		t.Fatalf("River %s job for %s is missing cancel_attempted_at", kind, attemptID)
	}
}

func assertDeleteAcceptanceRiverEventuallyCancelled(
	t *testing.T,
	pool *pgxpool.Pool,
	kind string,
	argKey string,
	attemptID uuid.UUID,
) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		if err := pool.QueryRow(t.Context(), `
			SELECT state::text FROM river_job
			WHERE kind=$1 AND args->>$2=$3`, kind, argKey, attemptID.String()).Scan(&state); err != nil {
			t.Fatalf("read River state for %s/%s: %v", kind, attemptID, err)
		}
		if state == "cancelled" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("River %s job for %s state = %q, want cancelled", kind, attemptID, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertDeleteAcceptanceRiverActive(
	t *testing.T,
	pool *pgxpool.Pool,
	kind string,
	argKey string,
	attemptID uuid.UUID,
) {
	t.Helper()
	var state string
	var marked bool
	if err := pool.QueryRow(t.Context(), `
		SELECT state::text,metadata ? 'cancel_attempted_at'
		FROM river_job
		WHERE kind=$1 AND args->>$2=$3`, kind, argKey, attemptID.String()).Scan(&state, &marked); err != nil {
		t.Fatalf("read active River job for %s/%s: %v", kind, attemptID, err)
	}
	active := state == "available" || state == "pending" || state == "retryable" ||
		state == "running" || state == "scheduled"
	if !active || marked {
		t.Fatalf("River %s job for %s = state %q marked=%v, want active/unmarked", kind, attemptID, state, marked)
	}
}
