// Package migrate 提供以 schema_migrations 表为状态记录的前向数据库迁移。
// 所有迁移步骤集中维护在 steps 切片中。Up 按顺序幂等应用未执行的自动前缀，
// 在首个 pending manual gate 前停止；UpTo 用于显式执行已批准的目标前缀。
// 不支持反向迁移，恢复策略依赖 PITR + 前向修复，而非在应用内执行任意 down SQL。
package migrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"webtag/internal/database"
)

// Step describes a single forward migration. Each statement in SQL is
// executed in order against the supplied querier; statements are run with
// IF NOT EXISTS / IF EXISTS guards so individual steps remain idempotent.
//
// There is no Down field. Reversible migrations are out of scope for the
// current schema-versioning model — recovery from a bad release is handled
// via point-in-time restore plus a forward fix-up step rather than running
// arbitrary down SQL inside the application.
//
// Manual marks a release-gated step. Up stops successfully before the first
// pending manual step; operators must name that step explicitly via UpTo after
// its rollout preconditions have been satisfied. Once the manual step is
// recorded, later Up calls skip it and may continue with subsequent automatic
// steps.
//
// NonTransactional 标记控制 step 是否在事务里跑（Wave 2 H7）：
//   - false（默认）：Up 用 BeginTx / Commit 把 step 内所有 SQL 包成一个
//     原子事务。中间报错会整体回滚，避免半成功状态。绝大多数 DDL
//     （CREATE TABLE / ALTER / 普通 CREATE INDEX）都应当走这条路。
//   - true：Up 直接用 pool.Exec 跑每条 SQL，不开事务。Postgres 有几类
//     语句明确禁止在事务里执行：
//   - CREATE INDEX CONCURRENTLY / DROP INDEX CONCURRENTLY
//   - ALTER TYPE ... ADD VALUE （部分版本）
//   - VACUUM / REINDEX CONCURRENTLY
//     这类 step 必须显式置为 true，否则 Postgres 会以
//     "CREATE INDEX CONCURRENTLY cannot run inside a transaction block"
//     报错。
//
// 注意：Querier 实现若不支持事务（如单元测试的 fakeQuerier），Up 会
// 自动回退到逐句 Exec 模式，NonTransactional 字段在该路径下被忽略。
type Step struct {
	ID               string
	SQL              []string
	Manual           bool
	NonTransactional bool
	// OnlineUpdate is the reviewed answer to "can the previous release's
	// binary keep serving once this step lands?" — see OnlineUpdateReview.
	// Its zero value refuses the page-triggered update, so a new step gets no
	// online-update permission by omission; the contract test in
	// online_compat_test.go fails until the author reviews it explicitly.
	OnlineUpdate OnlineUpdateReview
	// RecoverInvalidIndexes lists schema-qualified indexes that this
	// non-transactional step creates concurrently. PostgreSQL can leave an
	// index with indisvalid=false when CREATE INDEX CONCURRENTLY is canceled
	// or fails. IF NOT EXISTS alone would accept that unusable relation and
	// incorrectly record the migration as applied, so applyStep drops only
	// those invalid remnants before replaying the step SQL.
	RecoverInvalidIndexes []string
}

// txBeginner 是 database.Querier 之上的可选事务接口。pgxpool.Pool 实现
// 了它（返回 pgx.Tx，pgx.Tx 同时实现 database.Querier）。Up 通过运行时
// 类型断言来检测：实现了的走事务路径，没实现的（如测试 fake）走原 Exec
// 路径。这样 Querier 接口不需要把 BeginTx 抬高为强制方法。
type txBeginner interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
}

// FreshInstallTarget is a compatibility alias for explicitly provisioning a
// known-empty database. The public migration plan is fresh-install first, so
// Up and UpFreshInstall currently apply the same ordered plan.
const FreshInstallTarget = "fresh"

// Steps 返回内部 steps 切片的拷贝，供测试断言迁移定义，调用方对返回值的修改不会影响本包。
func Steps() []Step {
	out := make([]Step, len(steps))
	copy(out, steps)
	return out
}

// Up 按顺序应用自动迁移，并在首个尚未应用的 Manual step 前成功停止。
//
// Wave 2 H7：每个 step 默认在事务里跑（BeginTx / Commit），中间报错
// 整体回滚，schema_migrations 也只有在 step 全部成功后才落账。这避免
// "改了一半然后挂"的尴尬状态——下次重启重跑该 step 时第一条 IF NOT
// EXISTS 已经存在、第二条 ALTER 还没跑导致 schema 漂移。
//
// 标记了 NonTransactional=true 的 step（如 CREATE INDEX CONCURRENTLY）
// 仍然走逐句 pool.Exec 不开事务，因为 PG 不允许这些语句在事务块内。
//
// 已经执行过的步骤会被跳过；任意步骤失败立即返回，已成功的步骤保持已提交状态。
//
// 当 db 不实现 txBeginner（典型场景：单测 fakeQuerier）时，事务路径
// 自动退化为逐句 Exec，与 H7 之前的行为完全一致——保证单测无需 mock
// 出 pgx.Tx 也能继续工作。
func Up(ctx context.Context, db database.Querier) error {
	_, err := Run(ctx, db, RunRequest{})
	return err
}

// UpTo applies all ordered migrations through targetID, including manual
// steps. The target is validated against the in-memory migration plan before
// any database call, so a typo cannot partially mutate the target database.
//
// This is the human operator's entry point: AllowManual is on, because naming
// a manual step by hand *is* the approval. The page-triggered helper must use
// Run with AllowManual left false.
func UpTo(ctx context.Context, db database.Querier, targetID string) error {
	_, err := Run(ctx, db, RunRequest{Target: targetID, AllowManual: true})
	return err
}

// RunRequest is one migration run's complete input. It maps one-to-one onto
// cmd/migrate's MIGRATION_TARGET / MIGRATION_ALLOW_MANUAL environment.
type RunRequest struct {
	// Target selects the plan prefix:
	//   ""                  — default release-gated run: apply every automatic
	//                         step, stop successfully before the first pending
	//                         manual step.
	//   FreshInstallTarget  — compatibility alias for provisioning an empty
	//                         database; identical plan to "".
	//   <step ID>           — exact target: apply through that step and not one
	//                         step further.
	Target string
	// AllowManual permits an exact-target run to cross a pending manual step.
	// It is meaningless for the default and fresh plans, which stop at the
	// first manual gate by construction. Leave it false for anything a page
	// can trigger.
	AllowManual bool
}

// RunResult is the evidence a migration run actually did something. The migrate
// command used to be completely silent on success, which once let a deploy
// rebuild containers on top of migrations that had never been applied; every
// caller now gets the start version, the end version and the exact steps.
type RunResult struct {
	// Mode is "default", "fresh" or "target".
	Mode string `json:"mode"`
	// Target echoes the requested exact step ID; empty for default/fresh.
	Target string `json:"target"`
	// StartVersion is the newest plan step already recorded before the run,
	// empty when the ledger held none.
	StartVersion string `json:"start_version"`
	// EndVersion is the newest plan step recorded after the run.
	EndVersion string `json:"end_version"`
	// Applied lists, in plan order, the step IDs this run recorded.
	Applied []string `json:"applied"`
	// AlreadyAtTarget reports an exact-target run that had nothing to do. It
	// is a success: the helper may retry the same job safely.
	AlreadyAtTarget bool `json:"already_at_target"`
	// StoppedAtManual names the pending manual step that ended a default run
	// early, empty when no manual gate was reached.
	StoppedAtManual string `json:"stopped_at_manual"`
}

// Run applies the requested plan and reports what it did.
//
// An exact-target request is fail-closed. It refuses, without touching the
// database in the target-validation case:
//
//   - ErrUnknownTarget — the target is not a step this binary defines;
//   - ErrLedgerAhead — schema_migrations holds versions this binary does not
//     know, so a newer migrate already ran here;
//   - ErrTargetBehindLedger — the target is older than the applied ledger;
//     there is no down direction, so this needs a restore, not a migration;
//   - ErrManualStepInRange — the span contains a release-gated manual step and
//     AllowManual is false.
func Run(ctx context.Context, db database.Querier, request RunRequest) (RunResult, error) {
	target := strings.TrimSpace(request.Target)
	result := RunResult{Mode: "default"}
	targetIndex := -1
	switch target {
	case "":
	case FreshInstallTarget:
		result.Mode = "fresh"
	default:
		index, err := stepIndex(target)
		if err != nil {
			return RunResult{}, err
		}
		result.Mode = "target"
		result.Target = target
		targetIndex = index
	}

	if db == nil {
		return RunResult{}, fmt.Errorf("nil querier")
	}
	// The partial result travels with the error on purpose. Non-transactional
	// steps commit individually, so "which steps did land before this failed"
	// is exactly what the operator needs at a HOLD point.
	err := withMigrationRunnerSession(ctx, db, func(sessionDB database.Querier) error {
		return upLocked(ctx, sessionDB, targetIndex, request.AllowManual, &result)
	})
	return result, err
}

func stepIndex(targetID string) (int, error) {
	if index, ok := stepIndexOf(targetID); ok {
		return index, nil
	}
	return -1, fmt.Errorf("%w %q; known targets: %s", ErrUnknownTarget, targetID, strings.Join(stepIDList(), ", "))
}

// stepIndexOf is the boolean form, for callers that treat an unknown target as
// a reportable state rather than an error.
func stepIndexOf(targetID string) (int, bool) {
	for index, step := range steps {
		if step.ID == targetID {
			return index, true
		}
	}
	return -1, false
}

// UpFreshInstall provisions a known-empty database. It remains a separate API
// so existing automation using MIGRATION_TARGET=fresh keeps working, while the
// actual runner path stays identical to ordinary Up.
func UpFreshInstall(ctx context.Context, db database.Querier) error {
	return Up(ctx, db)
}

// upLocked is Run's body without lock acquisition. It fills result in place so
// a failure mid-run still leaves the caller's own error path in charge of what
// gets reported.
func upLocked(ctx context.Context, db database.Querier, targetIndex int, allowManual bool, result *RunResult) error {
	if _, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadAppliedMigrations(ctx, db)
	if err != nil {
		return err
	}

	pending, err := planRun(applied, targetIndex, allowManual, result)
	if err != nil {
		return err
	}

	// River owns river_job's schema, while later WebTag steps may add indexes
	// for WebTag's access patterns on that table. Apply River first so a fresh
	// install and an upgrade use the same dependency order; otherwise a guarded
	// WebTag step could be recorded while river_job is absent and never run
	// again after River creates the table. The schemas keep independent version
	// ledgers, so replay remains idempotent. A fake Querier still skips River.
	if err := maybeRunRiverMigrations(ctx, db); err != nil {
		return err
	}

	// 提前做一次类型断言，避免对每个 step 都重复尝试。txDB == nil 表示
	// 当前 Querier 不支持事务（fakeQuerier 等），所有 step 都退回 Exec 模式。
	txDB, _ := db.(txBeginner)

	for _, step := range pending {
		if err := applyStep(ctx, db, txDB, step); err != nil {
			return err
		}
		result.Applied = append(result.Applied, step.ID)
		applied[step.ID] = struct{}{}
		// Recompute rather than assume the last applied step is the newest:
		// a ledger with a gap replays an older step after a newer one is
		// already recorded, and reporting that older ID as the end version
		// would understate where the database actually is.
		result.EndVersion = headVersion(applied)
	}
	return nil
}

// planRun turns the applied ledger into the ordered steps this run will apply
// and records the run's start/end reporting fields. Every exact-target
// fail-closed condition is decided here, before a single step executes.
func planRun(applied map[string]struct{}, targetIndex int, allowManual bool, result *RunResult) ([]Step, error) {
	if targetIndex >= 0 {
		return planExactTargetRun(applied, targetIndex, allowManual, result)
	}

	result.StartVersion = headVersion(applied)
	result.EndVersion = result.StartVersion
	pending := make([]Step, 0, len(steps))
	for _, step := range steps {
		if _, ok := applied[step.ID]; ok {
			continue
		}
		if step.Manual {
			result.StoppedAtManual = step.ID
			break
		}
		pending = append(pending, step)
	}
	return pending, nil
}

func planExactTargetRun(applied map[string]struct{}, targetIndex int, allowManual bool, result *RunResult) ([]Step, error) {
	appliedList := make([]string, 0, len(applied))
	for version := range applied {
		appliedList = append(appliedList, version)
	}
	resolved, err := resolveTargetRange(appliedList, steps[targetIndex].ID)
	if err != nil {
		return nil, err
	}
	if !allowManual {
		for _, step := range resolved.pending {
			if step.Manual {
				return nil, fmt.Errorf("%w: %q is release-gated and stands between %s and target %s; "+
					"satisfy its rollout preconditions and re-run with MIGRATION_ALLOW_MANUAL=true from the operator "+
					"runbook — a page-triggered update must never cross it",
					ErrManualStepInRange, step.ID, startVersionLabel(resolved.startID), steps[targetIndex].ID)
			}
		}
	}
	result.StartVersion = resolved.startID
	result.EndVersion = resolved.startID
	result.AlreadyAtTarget = len(resolved.pending) == 0
	return resolved.pending, nil
}

// tolerateMissingLedger reports whether schema_migrations exists yet. A helper
// pre-check runs before any migration, so "table absent" is an empty ledger,
// not a failure.
func tolerateMissingLedger(ctx context.Context, db database.Querier) (bool, error) {
	var present bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&present); err != nil {
		return false, fmt.Errorf("probe schema_migrations: %w", err)
	}
	return present, nil
}

// headVersion returns the newest plan step present in the ledger. Versions the
// plan does not define are ignored here; the exact-target path rejects them
// outright via ErrLedgerAhead.
func headVersion(applied map[string]struct{}) string {
	head := ""
	for index, step := range steps {
		if _, ok := applied[step.ID]; ok {
			head = steps[index].ID
		}
	}
	return head
}

// stepIDList names every target this binary accepts, so a typo in
// MIGRATION_TARGET produces a message the operator can act on rather than just
// "unknown".
func stepIDList() []string {
	ids := make([]string, 0, len(steps))
	for _, step := range steps {
		ids = append(ids, step.ID)
	}
	return ids
}

func startVersionLabel(startID string) string {
	if startID == "" {
		return "an empty ledger"
	}
	return startID
}

// AppliedVersions returns the raw contents of schema_migrations in plan order,
// with any version this binary does not define appended afterwards. The deploy
// helper reads it to feed PlanOnlineUpdate before it decides whether a
// page-triggered update may proceed.
func AppliedVersions(ctx context.Context, db database.Querier) ([]string, error) {
	if db == nil {
		return nil, fmt.Errorf("nil querier")
	}
	present, err := tolerateMissingLedger(ctx, db)
	if err != nil {
		return nil, err
	}
	if !present {
		return []string{}, nil
	}
	applied, err := loadAppliedMigrations(ctx, db)
	if err != nil {
		return nil, err
	}
	ordered := make([]string, 0, len(applied))
	for _, step := range steps {
		if _, ok := applied[step.ID]; ok {
			ordered = append(ordered, step.ID)
			delete(applied, step.ID)
		}
	}
	unknown := make([]string, 0, len(applied))
	for version := range applied {
		unknown = append(unknown, version)
	}
	slices.Sort(unknown)
	return append(ordered, unknown...), nil
}

// loadAppliedMigrations closes its rows before River starts. This is required
// by the session-bound MaxConns=1 executor; deferring Close from upLocked would
// reserve the only connection and make River wait on the runner itself.
func loadAppliedMigrations(ctx context.Context, db database.Querier) (map[string]struct{}, error) {
	applied := make(map[string]struct{}, len(steps))
	rows, err := db.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return applied, nil
}

// applyStep 按 step 的事务标记选择执行路径：
//   - NonTransactional == true 或 txDB == nil → 逐句 Exec
//   - 否则 → BeginTx → 逐句 Exec → 写 schema_migrations → Commit
//
// schema_migrations 的插入也在同一事务里，保证 "step 跑成功" 与 "状态
// 落账" 是原子的；任何环节失败都 Rollback。
func applyStep(ctx context.Context, db database.Querier, txDB txBeginner, step Step) error {
	if len(step.RecoverInvalidIndexes) > 0 && !step.NonTransactional {
		return fmt.Errorf("migration %s configures invalid-index recovery but is transactional", step.ID)
	}
	if step.NonTransactional || txDB == nil {
		if err := recoverInvalidIndexes(ctx, db, step.RecoverInvalidIndexes); err != nil {
			return fmt.Errorf("prepare migration %s: %w", step.ID, err)
		}
		return execStepStatements(ctx, db, step)
	}
	return applyStepInTx(ctx, txDB, step)
}

// stepExecutor 是 database.Querier 与 pgx.Tx 共有的语句执行面，让两条
// 应用路径共用同一段「逐句执行 + 落账」逻辑。
type stepExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// execStepStatements 逐句执行 step.SQL 并写入 schema_migrations。事务路径把
// 二者放在同一个 tx 里，因此「step 跑成功」与「状态落账」保持原子。
func execStepStatements(ctx context.Context, exec stepExecutor, step Step) error {
	for idx, stmt := range step.SQL {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := exec.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply migration %s statement %d: %w", step.ID, idx+1, decorateStatementError(stmt, err))
		}
	}
	if _, err := exec.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, step.ID); err != nil {
		return fmt.Errorf("record migration %s: %w", step.ID, err)
	}
	return nil
}

func applyStepInTx(ctx context.Context, txDB txBeginner, step Step) error {
	tx, err := txDB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx for migration %s: %w", step.ID, err)
	}
	// 显式 Rollback 兜底：成功路径会调 Commit 后再调 Rollback，pgx
	// 的契约是 "Rollback after Commit" 返回 ErrTxClosed 且无副作用，
	// 因此这条 defer 既能在中途 return 时回滚，也不会影响成功路径。
	defer func() { _ = tx.Rollback(ctx) }()

	if err := execStepStatements(ctx, tx, step); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migration %s: %w", step.ID, err)
	}
	return nil
}

func recoverInvalidIndexes(ctx context.Context, db database.Querier, qualifiedNames []string) error {
	for _, qualifiedName := range qualifiedNames {
		parts := strings.Split(qualifiedName, ".")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return fmt.Errorf("invalid schema-qualified index name %q", qualifiedName)
		}
		// Probe and drop must name the index the same way. to_regclass folds an
		// unquoted identifier to lower case while DROP matches the quoted name
		// verbatim, so feeding the raw string to one and the sanitized form to
		// the other would silently disable recovery for any mixed-case name.
		quotedName := pgx.Identifier(parts).Sanitize()

		var invalid bool
		if err := db.QueryRow(ctx, `SELECT COALESCE((
			SELECT NOT indexes.indisvalid
			FROM pg_catalog.pg_index AS indexes
			WHERE indexes.indexrelid = pg_catalog.to_regclass($1)
		), FALSE)`, quotedName).Scan(&invalid); err != nil {
			return fmt.Errorf("inspect concurrent index %q: %w", qualifiedName, err)
		}
		if !invalid {
			continue
		}

		if _, err := db.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+quotedName); err != nil {
			return fmt.Errorf("drop invalid concurrent index %q: %w", qualifiedName, err)
		}
	}
	return nil
}

// decorateStatementError appends an operator-actionable hint to the raw
// Postgres error for failure modes whose default message does not point at
// the fix. Currently it covers `CREATE EXTENSION ... vector`: when the
// target database cannot load pgvector the server returns a terse
// `extension "vector" is not available`, which doesn't tell the operator
// what to install. The hint stays out of the SQL itself (which has to be
// valid Postgres) and is wrapped here so it surfaces in migrate logs.
//
// The original error is preserved via %w so callers can still errors.Is /
// errors.As against the underlying pgconn.PgError.
func decorateStatementError(stmt string, err error) error {
	lowered := strings.ToLower(stmt)
	if strings.Contains(lowered, "create extension") && strings.Contains(lowered, "vector") {
		return fmt.Errorf("%w (hint: PostgreSQL 需安装 pgvector 扩展（推荐 pgvector/pgvector:pg16 镜像）)", err)
	}
	return err
}
