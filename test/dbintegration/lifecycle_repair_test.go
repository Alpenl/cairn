package dbintegration

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/migrate"
)

const lifecycleRepairMigrationID = "lifecycle2026081401"

func TestLifecycleRepairMigrationConvergesHistoricalOrphans(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()

	// Simulate a rolling installation that already recorded every earlier
	// repair before these lifecycle defects were discovered.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, lifecycleRepairMigrationID); err != nil {
		t.Fatalf("remove lifecycle repair ledger: %v", err)
	}

	deletedLink := seedReaderVNextSavedLink(t, pool,
		"https://lifecycle-repair.example/deleted", "Deleted", "body", "summary")
	if _, err := pool.Exec(ctx, `UPDATE links SET deleted_at=NOW() WHERE id=$1`, deletedLink); err != nil {
		t.Fatalf("trash parse repair link: %v", err)
	}
	const activeAttempts = 205
	var parseRiverAttempt uuid.UUID
	for index := range activeAttempts {
		status := "pending"
		if index%2 == 1 {
			status = "processing"
		}
		attemptID := uuid.New()
		if index == 0 {
			parseRiverAttempt = attemptID
		}
		if _, err := pool.Exec(ctx, `INSERT INTO parse_jobs (id,link_id,status,error_msg) VALUES ($1,$2,$3,$4)`,
			attemptID, deletedLink, status, "legacy-active"); err != nil {
			t.Fatalf("seed active parse attempt %d: %v", index, err)
		}
	}
	parseRiverJob := insertActiveRiverJob(t, pool, map[string]any{
		"link_id":      deletedLink,
		"parse_job_id": parseRiverAttempt,
	}, "parse_link")
	legacyTranslation := uuid.New()
	legacyRiverJob := insertActiveRiverJob(t, pool, map[string]any{
		"translation_id": legacyTranslation,
	}, "translate_link_content")
	seedLifecycleRepairTranslation(t, pool, deletedLink, legacyTranslation, "pending", 0, legacyRiverJob, 0)
	v2Translation := uuid.New()
	v2RiverJob := insertActiveRiverJob(t, pool, map[string]any{
		"translation_id":     v2Translation,
		"attempt_generation": 3,
	}, "translate_link_v2")
	seedLifecycleRepairTranslation(t, pool, deletedLink, v2Translation, "processing", 3, v2RiverJob, 8)
	doneAttempt, failedAttempt := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO parse_jobs (id,link_id,status,error_msg) VALUES
		($1,$3,'done','keep-done'),($2,$3,'failed','keep-failed')`, doneAttempt, failedAttempt, deletedLink); err != nil {
		t.Fatalf("seed terminal parse attempts: %v", err)
	}

	managedOrphan := seedReaderVNextSavedLink(t, pool,
		"https://lifecycle-repair.example/managed-orphan", "Managed orphan", "body", "summary")
	if _, err := pool.Exec(ctx, `UPDATE links SET source_kind='subscription',feed_managed=true WHERE id=$1`, managedOrphan); err != nil {
		t.Fatalf("seed proven Feed orphan: %v", err)
	}

	ambiguousOrphan := seedReaderVNextSavedLink(t, pool,
		"https://lifecycle-repair.example/ambiguous-orphan", "Ambiguous orphan", "body", "summary")
	if _, err := pool.Exec(ctx, `UPDATE links SET source_kind='subscription',feed_managed=false WHERE id=$1`, ambiguousOrphan); err != nil {
		t.Fatalf("seed ambiguous Feed orphan: %v", err)
	}

	legacyClaimedLink := seedReaderVNextSavedLink(t, pool,
		"https://lifecycle-repair.example/claimed", "Claimed", "body", "summary")
	claimedItem := seedReaderFeedSaveItem(t, pool,
		"https://lifecycle-repair.example/claimed", "lifecycle-repair-claimed")
	if _, err := pool.Exec(ctx, `UPDATE links SET source_kind='subscription',feed_managed=false WHERE id=$1`, legacyClaimedLink); err != nil {
		t.Fatalf("seed legacy claimed Link ownership: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reader_feed_saves (feed_item_id,link_id,created_link) VALUES ($1,$2,true)`, claimedItem, legacyClaimedLink); err != nil {
		t.Fatalf("seed creator Feed association: %v", err)
	}
	adoptedLink := seedReaderVNextSavedLink(t, pool,
		"https://lifecycle-repair.example/adopted", "Adopted", "body", "summary")
	adoptedItem := seedReaderFeedSaveItem(t, pool,
		"https://lifecycle-repair.example/adopted", "lifecycle-repair-adopted")
	if _, err := pool.Exec(ctx, `UPDATE links SET source_kind='subscription',feed_managed=false WHERE id=$1`, adoptedLink); err != nil {
		t.Fatalf("seed adopted Link ownership: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO reader_feed_saves (feed_item_id,link_id,created_link) VALUES ($1,$2,true)`, adoptedItem, adoptedLink); err != nil {
		t.Fatalf("seed adopted creator association history: %v", err)
	}
	// NULL is the only trustworthy marker that this row predates lifecycle
	// provenance. A stored false is a current independent Library claim and
	// must never be overwritten merely because created_link remains true.
	if _, err := pool.Exec(ctx, `ALTER TABLE links ALTER COLUMN feed_managed DROP NOT NULL`); err != nil {
		t.Fatalf("open legacy Feed ownership state: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE links SET feed_managed=NULL WHERE id=$1`, legacyClaimedLink); err != nil {
		t.Fatalf("seed NULL legacy Feed ownership: %v", err)
	}

	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatalf("apply lifecycle repair migration: %v", err)
	}

	var active, repaired int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status IN ('pending','processing')),
		count(*) FILTER (WHERE status='failed' AND error_msg='link_deleted')
		FROM parse_jobs WHERE link_id=$1`, deletedLink).Scan(&active, &repaired); err != nil {
		t.Fatalf("read repaired parse attempts: %v", err)
	}
	if active != 0 || repaired != activeAttempts {
		t.Fatalf("parse repair = active:%d repaired:%d, want 0/%d", active, repaired, activeAttempts)
	}
	for id, want := range map[uuid.UUID]string{doneAttempt: "done/keep-done", failedAttempt: "failed/keep-failed"} {
		var status, reason string
		if err := pool.QueryRow(ctx, `SELECT status,COALESCE(error_msg,'') FROM parse_jobs WHERE id=$1`, id).Scan(&status, &reason); err != nil {
			t.Fatalf("read terminal parse attempt %s: %v", id, err)
		}
		if got := status + "/" + reason; got != want {
			t.Fatalf("terminal parse attempt %s = %q, want %q", id, got, want)
		}
	}
	for _, fixture := range []struct {
		name  string
		jobID int64
	}{
		{name: "parse", jobID: parseRiverJob},
		{name: "legacy translation", jobID: legacyRiverJob},
		{name: "v2 translation", jobID: v2RiverJob},
	} {
		var state string
		var cancelAttempted, finalized bool
		if err := pool.QueryRow(ctx, `SELECT state::text,
			metadata ? 'cancel_attempted_at', finalized_at IS NOT NULL
			FROM river_job WHERE id=$1`, fixture.jobID).Scan(&state, &cancelAttempted, &finalized); err != nil {
			t.Fatalf("read repaired %s River job: %v", fixture.name, err)
		}
		if state != "cancelled" || !cancelAttempted || !finalized {
			t.Fatalf("repaired %s River job = state:%q cancel_attempted:%v finalized:%v, want cancelled/true/true",
				fixture.name, state, cancelAttempted, finalized)
		}
	}
	for _, translationID := range []uuid.UUID{legacyTranslation, v2Translation} {
		var status, reason string
		var currentRiverJobID *int64
		if err := pool.QueryRow(ctx, `SELECT status,COALESCE(error_msg,''),current_river_job_id
			FROM link_translations WHERE id=$1`, translationID).Scan(&status, &reason, &currentRiverJobID); err != nil {
			t.Fatalf("read repaired translation %s: %v", translationID, err)
		}
		if status != "failed" || reason != "link_deleted" || currentRiverJobID != nil {
			t.Fatalf("repaired translation %s = status:%q reason:%q current_job:%v, want failed/link_deleted/nil",
				translationID, status, reason, currentRiverJobID)
		}
	}
	var activeRiverJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*)
		FROM river_job
		WHERE state IN ('available','pending','retryable','running','scheduled')
		  AND ((kind='parse_link' AND args->>'link_id'=$1)
		    OR (kind IN ('translate_link_content','translate_link_v2')
		      AND args->>'translation_id' IN ($2,$3)))`,
		deletedLink.String(), legacyTranslation.String(), v2Translation.String()).Scan(&activeRiverJobs); err != nil {
		t.Fatalf("count active lifecycle River jobs: %v", err)
	}
	if activeRiverJobs != 0 {
		t.Fatalf("active lifecycle River jobs after repair = %d, want 0", activeRiverJobs)
	}

	// 「feed_managed 且无 save」不再被软删：保留策略裁掉一条已读的已保存 feed
	// item 后，级联删除 save 行会让合法保存的 Link 落到完全相同的状态，二者在库里
	// 无法区分。修复只记录审计证据，Link 必须存活。
	assertReaderFeedLinkLive(t, pool, managedOrphan, true)
	assertReaderFeedLinkLive(t, pool, ambiguousOrphan, true)
	assertReaderFeedLinkLive(t, pool, legacyClaimedLink, true)
	assertReaderFeedLinkLive(t, pool, adoptedLink, true)
	var claimedManaged, adoptedManaged bool
	if err := pool.QueryRow(ctx, `SELECT feed_managed FROM links WHERE id=$1`, legacyClaimedLink).Scan(&claimedManaged); err != nil {
		t.Fatalf("read claimed Feed ownership: %v", err)
	}
	if !claimedManaged {
		t.Fatal("creator association did not restore Feed lifecycle ownership")
	}
	if err := pool.QueryRow(ctx, `SELECT feed_managed FROM links WHERE id=$1`, adoptedLink).Scan(&adoptedManaged); err != nil {
		t.Fatalf("read adopted Feed ownership: %v", err)
	}
	if adoptedManaged {
		t.Fatal("historical created_link overwrote an independent Library claim")
	}

	assertLifecycleRepairAudit(t, pool, managedOrphan, "ambiguous_subscription_orphan")
	assertLifecycleRepairAudit(t, pool, ambiguousOrphan, "ambiguous_subscription_orphan")

	// The repair is forward-only but must remain replay-safe if its ledger row
	// is restored from an older database snapshot or removed after a failed
	// deployment. Replaying cannot duplicate audit evidence or change terminals.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, lifecycleRepairMigrationID); err != nil {
		t.Fatalf("remove lifecycle repair ledger for replay: %v", err)
	}
	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatalf("replay lifecycle repair migration: %v", err)
	}
	var auditRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM feed_lifecycle_repair_audit WHERE link_id=ANY($1::uuid[])`,
		[]uuid.UUID{managedOrphan, ambiguousOrphan}).Scan(&auditRows); err != nil {
		t.Fatalf("count replayed lifecycle audit: %v", err)
	}
	if auditRows != 2 {
		t.Fatalf("lifecycle audit rows after replay = %d, want 2", auditRows)
	}
}

func seedLifecycleRepairTranslation(
	t *testing.T,
	db interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	linkID uuid.UUID,
	translationID uuid.UUID,
	status string,
	generation int64,
	riverJobID int64,
	startOffset int,
) {
	t.Helper()
	sourceText := "migration translation"
	if err := db.QueryRow(t.Context(), `INSERT INTO link_translations (
		id,link_id,scope,block_key,start_offset,end_offset,
		source_text,source_format,target_language,source_hash,status,
		attempt_generation,current_river_job_id
	) VALUES ($1,$2,'selection','summary',$3,$4,
		$5,'plain','zh-CN',$6,$7,$8,$9) RETURNING id`,
		translationID, linkID, startOffset, startOffset+len(sourceText), sourceText,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		status, generation, riverJobID).Scan(&translationID); err != nil {
		t.Fatalf("seed lifecycle translation %s: %v", status, err)
	}
}

func assertLifecycleRepairAudit(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, linkID uuid.UUID, classification string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM feed_lifecycle_repair_audit WHERE link_id=$1 AND classification=$2`,
		linkID, classification).Scan(&count); err != nil {
		t.Fatalf("read lifecycle repair audit %s/%s: %v", linkID, classification, err)
	}
	if count != 1 {
		t.Fatalf("lifecycle repair audit %s/%s count = %d, want 1", linkID, classification, count)
	}
}
