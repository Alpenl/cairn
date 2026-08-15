package dbintegration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/repository"
)

type rf3bRevisionSnapshot struct {
	library int64
	global  int64
	feed    int64
}

func readRF3BRevisionSnapshot(t *testing.T, pool *pgxpool.Pool) rf3bRevisionSnapshot {
	t.Helper()
	var snapshot rf3bRevisionSnapshot
	if err := pool.QueryRow(t.Context(), `SELECT
		(SELECT revision FROM library_read_revision WHERE singleton),
		(SELECT revision FROM global_read_revision WHERE singleton),
		(SELECT revision FROM feed_read_revision WHERE singleton)`).
		Scan(&snapshot.library, &snapshot.global, &snapshot.feed); err != nil {
		t.Fatalf("read representation revision snapshot: %v", err)
	}
	return snapshot
}

func TestRF3BInstalledTriggerContractCoversEveryRepresentationTable(t *testing.T) {
	pool := StartPostgres(t)
	definitions := readRF3BInstalledTriggerDefinitions(t, pool)
	if len(definitions) != 45 {
		t.Fatalf("representation trigger definitions=%d, want 45", len(definitions))
	}
}

func readRF3BInstalledTriggerDefinitions(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	tables := []string{
		"links", "link_concept", "sites", "site_entries", "site_tags",
		"feed_folders", "feed_subscriptions", "feed_items", "concept",
	}
	rows, err := pool.Query(t.Context(), `SELECT t.tgname,
		pg_get_triggerdef(t.oid, true) || E'\n' || pg_get_functiondef(t.tgfoid)
		FROM pg_trigger t
		JOIN pg_class c ON c.oid=t.tgrelid
		WHERE NOT t.tgisinternal
		  AND c.relname = ANY($1::text[])
		  AND (t.tgname LIKE 'trg\_%\_bump\_%revision\_%' ESCAPE '\'
		       OR t.tgname LIKE 'trg\_%\_representation\_write\_gate\_%' ESCAPE '\')
		ORDER BY t.tgname`, tables)
	if err != nil {
		t.Fatalf("query representation trigger definitions: %v", err)
	}
	defer rows.Close()

	definitions := make(map[string]string, 45)
	revisionTriggers := 0
	writeGateTriggers := 0
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatalf("scan representation trigger definition: %v", err)
		}
		if _, duplicate := definitions[name]; duplicate {
			t.Fatalf("duplicate representation trigger %s", name)
		}
		definitions[name] = definition
		if strings.Contains(name, "_representation_write_gate_") {
			writeGateTriggers++
		} else {
			revisionTriggers++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate representation trigger definitions: %v", err)
	}
	if revisionTriggers != 27 || writeGateTriggers != 18 {
		t.Fatalf("revision/write-gate triggers=%d/%d, want 27/18", revisionTriggers, writeGateTriggers)
	}
	return definitions
}

func TestRF3BRevisionTriggersCompareOnlyRepresentationColumns(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	unique := uuid.NewString()
	linkID := uuid.New()
	conceptID := uuid.New()
	subscriptionID := uuid.New()

	baseline := readRF3BRevisionSnapshot(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO links
		(id,url,source_key,title,status,first_collected_at)
		VALUES ($1,$2,$3,'before','done',NOW())`,
		linkID, "https://revision-"+unique+".example.com/article", "revision:"+unique); err != nil {
		t.Fatalf("insert link: %v", err)
	}
	afterLinkInsert := readRF3BRevisionSnapshot(t, pool)
	assertRevisionDelta(t, "link insert", baseline, afterLinkInsert, rf3bRevisionSnapshot{library: 1})

	if _, err := pool.Exec(ctx, `UPDATE links SET embedding_model='model-only' WHERE id=$1`, linkID); err != nil {
		t.Fatalf("update non-representation link column: %v", err)
	}
	if after := readRF3BRevisionSnapshot(t, pool); after != afterLinkInsert {
		t.Fatalf("embedding metadata update changed revisions: before=%+v after=%+v", afterLinkInsert, after)
	}
	if _, err := pool.Exec(ctx, `UPDATE links SET title='after' WHERE id=$1`, linkID); err != nil {
		t.Fatalf("update represented link column: %v", err)
	}
	afterLinkUpdate := readRF3BRevisionSnapshot(t, pool)
	assertRevisionDelta(t, "link title update", afterLinkInsert, afterLinkUpdate, rf3bRevisionSnapshot{library: 1})

	if _, err := pool.Exec(ctx, `INSERT INTO concept (id,primary_name,display_name) VALUES ($1,'primary','before')`, conceptID); err != nil {
		t.Fatalf("insert concept: %v", err)
	}
	afterConceptInsert := readRF3BRevisionSnapshot(t, pool)
	assertRevisionDelta(t, "concept insert", afterLinkUpdate, afterConceptInsert, rf3bRevisionSnapshot{global: 1})
	if _, err := pool.Exec(ctx, `UPDATE concept SET embedding_model='model-only' WHERE id=$1`, conceptID); err != nil {
		t.Fatalf("update non-representation concept column: %v", err)
	}
	if after := readRF3BRevisionSnapshot(t, pool); after != afterConceptInsert {
		t.Fatalf("concept embedding metadata update changed revisions: before=%+v after=%+v", afterConceptInsert, after)
	}
	if _, err := pool.Exec(ctx, `UPDATE concept SET display_name='after' WHERE id=$1`, conceptID); err != nil {
		t.Fatalf("update represented concept column: %v", err)
	}
	afterConceptUpdate := readRF3BRevisionSnapshot(t, pool)
	assertRevisionDelta(t, "concept display update", afterConceptInsert, afterConceptUpdate, rf3bRevisionSnapshot{global: 1})

	if _, err := pool.Exec(ctx, `INSERT INTO feed_subscriptions
		(id,url,title,active,next_fetch_at) VALUES ($1,$2,'before',true,NOW()-INTERVAL '1 minute')`,
		subscriptionID, "https://revision-"+unique+".example.com/feed"); err != nil {
		t.Fatalf("insert feed subscription: %v", err)
	}
	afterFeedInsert := readRF3BRevisionSnapshot(t, pool)
	assertRevisionDelta(t, "feed insert", afterConceptUpdate, afterFeedInsert, rf3bRevisionSnapshot{feed: 1})
	if _, err := pool.Exec(ctx, `UPDATE feed_subscriptions SET
		refresh_claim_token=gen_random_uuid(), refresh_claimed_until=NOW()+INTERVAL '1 minute',
		next_fetch_at=NOW()+INTERVAL '1 hour'
		WHERE id=$1`, subscriptionID); err != nil {
		t.Fatalf("update feed transport columns: %v", err)
	}
	if after := readRF3BRevisionSnapshot(t, pool); after != afterFeedInsert {
		t.Fatalf("feed lease/transport update changed revisions: before=%+v after=%+v", afterFeedInsert, after)
	}
	if _, err := pool.Exec(ctx, `UPDATE feed_subscriptions SET title='after' WHERE id=$1`, subscriptionID); err != nil {
		t.Fatalf("update represented feed column: %v", err)
	}
	afterFeedUpdate := readRF3BRevisionSnapshot(t, pool)
	assertRevisionDelta(t, "feed title update", afterFeedInsert, afterFeedUpdate, rf3bRevisionSnapshot{feed: 1})

	if _, err := pool.Exec(ctx, `DELETE FROM feed_subscriptions WHERE id=$1`, subscriptionID); err != nil {
		t.Fatalf("delete feed subscription: %v", err)
	}
	afterFeedDelete := readRF3BRevisionSnapshot(t, pool)
	assertRevisionDelta(t, "feed delete", afterFeedUpdate, afterFeedDelete, rf3bRevisionSnapshot{feed: 1})
	if _, err := pool.Exec(ctx, `DELETE FROM links WHERE id=$1`, linkID); err != nil {
		t.Fatalf("delete link: %v", err)
	}
	afterLinkDelete := readRF3BRevisionSnapshot(t, pool)
	assertRevisionDelta(t, "link delete", afterFeedDelete, afterLinkDelete, rf3bRevisionSnapshot{library: 1})
}

func assertRevisionDelta(t *testing.T, stage string, before, after, want rf3bRevisionSnapshot) {
	t.Helper()
	got := rf3bRevisionSnapshot{
		library: after.library - before.library,
		global:  after.global - before.global,
		feed:    after.feed - before.feed,
	}
	if got != want {
		t.Fatalf("%s revision delta=%+v, want %+v (before=%+v after=%+v)", stage, got, want, before, after)
	}
}

func TestRF3BClaimDueLeaseChurnDoesNotInvalidateRepresentations(t *testing.T) {
	pool := StartPostgres(t)
	subscriptionID := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO feed_subscriptions
		(id,url,title,active,next_fetch_at) VALUES ($1,$2,'lease',true,NOW()-INTERVAL '1 minute')`,
		subscriptionID, "https://lease-"+uuid.NewString()+".example.com/feed"); err != nil {
		t.Fatalf("insert due subscription: %v", err)
	}
	baseline := readRF3BRevisionSnapshot(t, pool)
	feeds := repository.NewPGXFeedRepository(pool, pool)
	for iteration := 0; iteration < 25; iteration++ {
		claimed, err := feeds.ClaimDue(t.Context(), 1, time.Minute)
		if err != nil {
			t.Fatalf("ClaimDue iteration %d: %v", iteration, err)
		}
		if len(claimed) != 1 || claimed[0].ID != subscriptionID {
			t.Fatalf("ClaimDue iteration %d=%#v, want subscription %s", iteration, claimed, subscriptionID)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE feed_subscriptions SET
			refresh_claim_token=NULL, refresh_claimed_until=NULL, next_fetch_at=NOW()-INTERVAL '1 minute'
			WHERE id=$1`, subscriptionID); err != nil {
			t.Fatalf("release lease iteration %d: %v", iteration, err)
		}
	}
	if after := readRF3BRevisionSnapshot(t, pool); after != baseline {
		t.Fatalf("lease churn changed revisions: before=%+v after=%+v", baseline, after)
	}
}

func TestRF3BExclusiveWriteGateRejectsOtherWritersButAllowsOwner(t *testing.T) {
	pool := StartPostgres(t)
	owner, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin exclusive owner: %v", err)
	}
	defer rollbackRF3BTestTx(owner)
	if _, err := owner.Exec(t.Context(), `SELECT lock_representation_write_gate_exclusive()`); err != nil {
		t.Fatalf("acquire exclusive write gate: %v", err)
	}

	ownerID := uuid.New()
	if _, err := owner.Exec(t.Context(), `INSERT INTO links
		(id,url,source_key,status,first_collected_at) VALUES ($1,$2,$3,'done',NOW())`,
		ownerID, "https://gate-owner.example.com/"+ownerID.String(), "gate-owner:"+ownerID.String()); err != nil {
		t.Fatalf("exclusive gate owner write: %v", err)
	}

	otherID := uuid.New()
	_, err = pool.Exec(t.Context(), `INSERT INTO links
		(id,url,source_key,status,first_collected_at) VALUES ($1,$2,$3,'done',NOW())`,
		otherID, "https://gate-other.example.com/"+otherID.String(), "gate-other:"+otherID.String())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("competing write error=%v, want PostgreSQL 40001 retry signal", err)
	}
	if err := owner.Commit(t.Context()); err != nil {
		t.Fatalf("commit exclusive gate owner: %v", err)
	}
}

func TestRF3BRevisionLockHelpersUseOneDeterministicGate(t *testing.T) {
	pool := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	owner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin library/feed lock owner: %v", err)
	}
	defer rollbackRF3BTestTx(owner)
	if _, err := owner.Exec(ctx, `SELECT lock_library_feed_revisions()`); err != nil {
		t.Fatalf("lock library/feed revisions: %v", err)
	}

	const waiterApplication = "rf3b-library-global-waiter"
	waiterPool := openRF3BNamedPool(t, waiterApplication)
	waiterResult := make(chan error, 1)
	go func() {
		tx, beginErr := waiterPool.Begin(ctx)
		if beginErr != nil {
			waiterResult <- beginErr
			return
		}
		defer rollbackRF3BTestTx(tx)
		if _, lockErr := tx.Exec(ctx, `SELECT lock_library_global_revisions()`); lockErr != nil {
			waiterResult <- lockErr
			return
		}
		waiterResult <- tx.Commit(ctx)
	}()

	waitForRF3BLockWait(t, ctx, pool, waiterApplication, "lock_library_global_revisions")
	if err := owner.Commit(ctx); err != nil {
		t.Fatalf("release library/feed lock owner: %v", err)
	}
	select {
	case err := <-waiterResult:
		if err != nil {
			t.Fatalf("library/global waiter after owner release: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("library/global waiter did not finish: %v", ctx.Err())
	}
}

// rollbackRF3BTestTx outlives t.Context because testing cancels that context
// before cleanup callbacks run.
func rollbackRF3BTestTx(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

func openRF3BNamedPool(t *testing.T, applicationName string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(DSN(t))
	if err != nil {
		t.Fatalf("parse named pool config: %v", err)
	}
	config.ConnConfig.RuntimeParams["application_name"] = applicationName
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("open named pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func waitForRF3BLockWait(t *testing.T, ctx context.Context, inspector *pgxpool.Pool, applicationName string, queryFragments ...string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	lastQuery := "<no active session>"
	lastWait := "<none>"
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("%s did not reach expected lock wait %v: %v; last query=%q wait=%s",
				applicationName, queryFragments, ctx.Err(), lastQuery, lastWait)
		case <-ticker.C:
			var query string
			var waitEventType *string
			err := inspector.QueryRow(ctx, `SELECT query, wait_event_type
				FROM pg_stat_activity
				WHERE application_name=$1 AND state='active'`, applicationName).Scan(&query, &waitEventType)
			if errors.Is(err, pgx.ErrNoRows) {
				lastQuery = "<no active session>"
				lastWait = "<none>"
				continue
			}
			if err != nil {
				t.Fatalf("inspect %s lock wait: %v", applicationName, err)
			}
			lastQuery = query
			if waitEventType == nil {
				lastWait = "<none>"
			} else {
				lastWait = *waitEventType
			}
			if waitEventType == nil || *waitEventType != "Lock" {
				continue
			}
			for _, fragment := range queryFragments {
				if strings.Contains(query, fragment) {
					return
				}
			}
		}
	}
}

func formatRF3BRevisionSnapshot(snapshot rf3bRevisionSnapshot) string {
	return fmt.Sprintf("library=%d global=%d feed=%d", snapshot.library, snapshot.global, snapshot.feed)
}
