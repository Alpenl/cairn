package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"webtag/internal/database"
)

// runRiverMigrations 用 rivermigrate 把 River 的内置 schema 迁移应用到目标库。
//
// 设计抉择（DEV-PLAN Phase 13「River 迁移接入现有 migrate 流程」）：把 River
// 迁移并入 internal/migrate.Up，保持单一迁移入口（走 Up 的调用方自动带上 River
// 表），而不是在 cmd/migrate 里另起一段。生产里唯一的调用方是 cmd/migrate 这个
// 一次性容器——应用启动期不跑迁移，所以 Up 的 runner advisory lock 不会阻塞任何
// 副本的启动。River 在独立的
// river_migration 表里记录自己的版本，与本包的 schema_migrations 互不干扰。
// Up 先运行 River，再运行可能依赖 river_job 的 WebTag steps；重复 Up 时 River
// 按 river_migration 跳过已应用版本，自研 step 则按 schema_migrations 跳过。
//
// rivermigrate.Migrate(DirectionUp, nil) 会把 MaxSteps 当 -1（应用全部未应用
// 的 up 迁移）。River v0.39 的 migrator 默认每个迁移各自一个事务（MigrateTx
// 已废弃，因为部分 schema 变更无法塞进单一事务），所以这里不需要也不应该把
// 它包进外层事务。
func runRiverMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("construct river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("apply river migrations: %w", err)
	}
	return nil
}

// RiverBundleVersions returns the River schema versions this binary can apply,
// ascending. It is what a release manifest's river_ledger_target must be drawn
// from: declaring a version outside this list means the manifest was produced
// by a different River dependency than the binary being deployed.
//
// The migrator is constructed against a nil pool on purpose — the bundle is
// read from River's embedded migration FS and needs no database at all, which
// lets the release pipeline compute the target offline.
func RiverBundleVersions() []int {
	migrator, err := rivermigrate.New(riverpgxv5.New(nil), nil)
	if err != nil {
		// River's bundle is embedded in the binary; a failure here means the
		// dependency itself is broken, not that a caller passed bad input.
		return nil
	}
	all := migrator.AllVersions()
	versions := make([]int, 0, len(all))
	for _, migration := range all {
		versions = append(versions, migration.Version)
	}
	return versions
}

// RiverBundleTarget returns the newest River schema version this binary
// applies, i.e. where river_migration lands after a successful run.
func RiverBundleTarget() int {
	versions := RiverBundleVersions()
	if len(versions) == 0 {
		return 0
	}
	return versions[len(versions)-1]
}

// maybeRunRiverMigrations 仅在 db 是真 *pgxpool.Pool 时运行 River 迁移；
// 单测的 fakeQuerier 不满足断言，直接 no-op 返回。
func maybeRunRiverMigrations(ctx context.Context, db database.Querier) error {
	pool, ok := db.(*pgxpool.Pool)
	if !ok {
		return nil
	}
	return runRiverMigrations(ctx, pool)
}
