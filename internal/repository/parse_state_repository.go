package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
)

// 「reading 链接的抓取素材不得被清除」是一条跨包不变量，落在三个位置：
//
//	解析失败路径   updateFailedSiteCandidateSQL  → 引用 SQLSiteCandidatePredicate
//	后台清理 worker site_payload_cleaner.go      → 引用 SQLNotReadingPredicate
//	解析终态写入   clearReadingPayloadPurgeDeadlineSQL（下方）
//
// 前两处是「排除 reading」的否定式判断，共用下面的常量。第三处是正向判定
// （library_kind = 'reading' 时清掉清理截止时间），两个常量都不适用，因此它自带
// 条件——这是刻意的，不是漏改。
//
// worker 那处在持有行锁的 UPDATE 里重新校验一次：陈旧的候选查询否则可能清掉一条
// 并发完成的 reading 链接。需要重复的是**校验动作**，不是**条件定义**——后者只有
// 下面这一个来源，改措辞时不会有第二处跟不上。
const (
	// SQLSiteCandidatePredicate 判定一行是否已固定或完成为 site。
	SQLSiteCandidatePredicate = "library_kind = 'site'"
	// SQLNotReadingPredicate 是清理侧的守卫：已确定为 reading 的行永不清理。
	//
	// COALESCE keeps an unresolved row eligible for a future deadline, but such
	// a row is not selected by the cleaner until payload_purge_due_at is set.
	SQLNotReadingPredicate = "COALESCE(library_kind, '') <> 'reading'"
)

// purgeSitePayloadColumn 按上述不变量生成一个「site 候选才清空，否则原样保留」的
// SET 子句片段。
func purgeSitePayloadColumn(column, purgedValue string) string {
	return column + " = CASE WHEN " + SQLSiteCandidatePredicate + " THEN " + purgedValue + " ELSE " + column + " END"
}

const (
	markParseProcessingSQL = `UPDATE links
		SET status='processing',error_msg=NULL,updated_at=NOW()
		WHERE id=$1 AND parse_generation=$2
		  AND status IN ('pending','processing') AND deleted_at IS NULL`
	clearReadingPayloadPurgeDeadlineSQL = "UPDATE links SET payload_purge_due_at = NULL WHERE id = $1 AND library_kind = 'reading'"
	selectParseAttemptSQL               = "SELECT parse_generation,status FROM links WHERE id=$1 AND deleted_at IS NULL"
)

// updateFailedSiteCandidateSQL：解析失败时，site 候选不得保留浏览器正文素材，
// 而 reading 链接要留着——重试与手动保存正文都依赖它。
//
// 由 purgeSitePayloadColumn 逐列生成，六列共用同一个谓词定义；此前这段 SQL 把
// 同一个 CASE WHEN 条件手写了六遍。
var updateFailedSiteCandidateSQL = "UPDATE links SET status = 'failed', error_msg = $3, " +
	purgeSitePayloadColumn("input_text", "NULL") + ", " +
	purgeSitePayloadColumn("input_html", "NULL") + ", " +
	purgeSitePayloadColumn("input_images", "NULL") + ", " +
	purgeSitePayloadColumn("source_metadata", "NULL") + ", " +
	purgeSitePayloadColumn("payload_purge_due_at", "NULL") + ", " +
	purgeSitePayloadColumn("payload_purged_at", "NOW()") +
	", updated_at = NOW() WHERE id = $1 AND parse_generation = $2 AND status IN ('pending','processing') AND deleted_at IS NULL"

func (r *PGXLinkRepository) MarkParseProcessing(ctx context.Context, attempt model.ParseAttempt) error {
	return r.updateParseState(ctx, attempt, false, "")
}

func (r *PGXLinkRepository) MarkParseFailed(ctx context.Context, attempt model.ParseAttempt, message string) error {
	return r.updateParseState(ctx, attempt, true, message)
}

func (r *PGXLinkRepository) CompleteReadingParse(ctx context.Context, params CompleteReadingParseParams) (CompleteReadingParseResult, error) {
	if params.Classification.Kind != model.LibraryKindReading || params.Analysis.ID != params.Classification.ID {
		return CompleteReadingParseResult{}, fmt.Errorf("complete reading parse: final reading classification and matching link id are required")
	}
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("begin complete reading parse tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := updateLinkAnalysisForParseOn(ctx, tx, params.Analysis)
	if err != nil {
		return CompleteReadingParseResult{}, err
	}
	if err := updateLibraryClassificationForCompletionOn(ctx, tx, params.Classification, params.ExpectedLibraryKind, params.ExpectedLibraryKindLocked); err != nil {
		return CompleteReadingParseResult{}, err
	}
	if err := detachSiteEntryForReadingOn(ctx, tx, params.Analysis.ID, nil, false); err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("detach site entry for reading completion: %w", err)
	}
	if _, err := tx.Exec(ctx, clearReadingPayloadPurgeDeadlineSQL, params.Analysis.ID); err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("clear reading payload purge deadline: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("commit complete reading parse tx: %w", err)
	}
	return result, nil
}

func (r *PGXLinkRepository) updateParseState(
	ctx context.Context,
	attempt model.ParseAttempt,
	failed bool,
	message string,
) error {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin parse state tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateSQL := markParseProcessingSQL
	args := []any{attempt.LinkID, attempt.Generation}
	if failed {
		stateSQL = updateFailedSiteCandidateSQL
		args = append(args, message)
	}
	tag, err := tx.Exec(ctx, stateSQL, args...)
	if err != nil {
		return fmt.Errorf("update link parse state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrParseAttemptNotRunnable
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit parse state tx: %w", err)
	}
	return nil
}

func requireCurrentParseAttempt(ctx context.Context, db database.Querier, attempt model.ParseAttempt) error {
	var generation int64
	var status model.LinkStatus
	if err := db.QueryRow(ctx, selectParseAttemptSQL, attempt.LinkID).Scan(&generation, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrParseAttemptNotRunnable
		}
		return fmt.Errorf("read current parse attempt: %w", err)
	}
	if generation != attempt.Generation || (status != model.LinkStatusPending && status != model.LinkStatusProcessing) {
		return ErrParseAttemptNotRunnable
	}
	return nil
}

var _ ParseStateStore = (*PGXLinkRepository)(nil)
var _ ReadingParseCompleter = (*PGXLinkRepository)(nil)
