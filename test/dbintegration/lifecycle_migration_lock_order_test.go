package dbintegration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"webtag/internal/migrate"
)

// TestLifecycleRepairMigrationSharesRuntimeRevisionLinkLockOrder guards the
// migration/runtime lock-order boundary. Runtime mutations acquire the
// library/feed revision advisory locks before locking a Link. The lifecycle
// repair must do the same: locking the Link first and then waiting for the
// revisions lets the online transaction form a Link <-> revision deadlock.
func TestLifecycleRepairMigrationSharesRuntimeRevisionLinkLockOrder(t *testing.T) {
	pool := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version=$1`, lifecycleRepairMigrationID); err != nil {
		t.Fatalf("remove lifecycle repair ledger: %v", err)
	}
	orphan := seedReaderVNextSavedLink(t, pool,
		"https://lifecycle-repair.example/lock-order", "Lock order", "body", "summary")
	if _, err := pool.Exec(ctx, `UPDATE links SET source_kind='subscription',feed_managed=true WHERE id=$1`, orphan); err != nil {
		t.Fatalf("seed Feed-managed orphan: %v", err)
	}

	onlineTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin online mutation: %v", err)
	}
	defer func() { _ = onlineTx.Rollback(context.Background()) }()
	if _, err := onlineTx.Exec(ctx, `SELECT lock_library_feed_revisions()`); err != nil {
		t.Fatalf("lock online library/feed revisions: %v", err)
	}

	const migrationApplication = "lifecycle-repair-lock-order-migration"
	migrationPool := openNamedPool(t, migrationApplication)
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrate.Up(ctx, migrationPool)
	}()

	// A correctly ordered migration waits here before it can lock the orphan.
	// In the old Link-first order it instead waits here while already owning the
	// orphan row, so the next online lock completes the deadlock cycle.
	waitForPostgresLock(t, ctx, pool, migrationApplication)
	var lockedID string
	if err := onlineTx.QueryRow(ctx, `SELECT id::text FROM links WHERE id=$1 FOR UPDATE`, orphan).Scan(&lockedID); err != nil {
		t.Fatalf("lock orphan after revision owner: %v", err)
	}
	if err := onlineTx.Commit(ctx); err != nil {
		t.Fatalf("commit online mutation: %v", err)
	}

	select {
	case err := <-migrationDone:
		assertLifecycleMigrationNoConcurrencyAbort(t, err)
		if err != nil {
			t.Fatalf("apply lifecycle repair migration: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("lifecycle repair migration did not finish after online commit: %v", ctx.Err())
	}

	// 本测试守的是锁顺序，末尾只需要一份「修复确实执行到了那一步」的证据。
	// 「feed_managed 且无 save」无法证明是孤儿——保留策略裁掉一条已读的已保存
	// feed item 后，级联删除 save 行会让合法保存的 Link 落到完全相同的状态。
	// 因此修复只记录审计、不销毁，这里相应地断言审计存在且 Link 仍然存活。
	assertLifecycleRepairAudit(t, pool, orphan, "ambiguous_subscription_orphan")
	var deleted bool
	if err := pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM links WHERE id=$1`, orphan).Scan(&deleted); err != nil {
		t.Fatalf("read repaired Feed-managed orphan: %v", err)
	}
	if deleted {
		t.Fatal("lifecycle repair soft-deleted an unproven Feed-managed orphan")
	}
}

func assertLifecycleMigrationNoConcurrencyAbort(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return
	}
	if pgErr.Code == "40P01" || pgErr.Code == "40001" {
		t.Fatalf("lifecycle repair migration hit PostgreSQL concurrency abort %s: %v", pgErr.Code, err)
	}
}
