package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
	// SQLSiteCandidatePredicate 判定一行是否为 site 候选（已判定为 site，或预测
	// 为 site 但尚未落地）。仅这类行的抓取素材可以被清除。
	SQLSiteCandidatePredicate = "(library_kind = 'site' OR predicted_library_kind = 'site')"
	// SQLNotReadingPredicate 是清理侧的守卫：已确定为 reading 的行永不清理。
	//
	// 注意它与 SQLSiteCandidatePredicate **不是互反**，NULL 的处理尤其不同：
	//   library_kind = NULL 时
	//     SQLSiteCandidatePredicate → NULL='site' OR NULL='site' → NULL → 不命中
	//     SQLNotReadingPredicate    → COALESCE(NULL,'')='' → ''<>'reading' → 命中
	// 即尚未分类的行**会**被清理侧放行。这是正确的：payload_purge_due_at 仅在
	// 链接是 site 候选时才被写入（见 link_repo_submit.go 的 CASE WHEN
	// library_kind='site'），因此能走到清理逻辑的 NULL 行都是预测站点候选，本就
	// 该清。
	//
	// COALESCE 不可省略。若图省事改成 library_kind <> 'reading'，NULL 行会求值为
	// NULL 而不再命中，预测站点的 payload 清理会被静默关掉——不报错、不告警，
	// 只是磁盘慢慢涨。
	SQLNotReadingPredicate = "COALESCE(library_kind, '') <> 'reading'"
)

// purgeSitePayloadColumn 按上述不变量生成一个「site 候选才清空，否则原样保留」的
// SET 子句片段。
func purgeSitePayloadColumn(column, purgedValue string) string {
	return column + " = CASE WHEN " + SQLSiteCandidatePredicate + " THEN " + purgedValue + " ELSE " + column + " END"
}

const (
	updateRunnableParseJobStateSQL = "UPDATE parse_jobs SET status = $2, error_msg = $3, updated_at = NOW() WHERE id = $1 AND link_id = $4 AND status IN ('pending', 'processing')"
	// completeParseJobStateSQL verifies the immutable metadata lease stored on
	// the durable job row as it enters its terminal state. The Link update runs
	// first and holds the Link row lock; a substituted revision therefore turns
	// this into a zero-row update and rolls the whole transaction back instead
	// of letting an internal caller forge ownership of a newer metadata tuple.
	completeParseJobStateSQL            = "UPDATE parse_jobs SET status = 'done', error_msg = NULL, updated_at = NOW() WHERE id = $1 AND link_id = $2 AND status = 'processing' AND expected_metadata_revision = $3"
	clearReadingPayloadPurgeDeadlineSQL = "UPDATE links SET payload_purge_due_at = NULL WHERE id = $1 AND library_kind = 'reading'"
)

// updateFailedSiteCandidateSQL：解析失败时，site 候选不得保留浏览器正文素材，
// 而 reading 链接要留着——重试与手动保存正文都依赖它。
//
// 由 purgeSitePayloadColumn 逐列生成，六列共用同一个谓词定义；此前这段 SQL 把
// 同一个 CASE WHEN 条件手写了六遍。
var updateFailedSiteCandidateSQL = "UPDATE links SET status = $2, error_msg = $3, " +
	purgeSitePayloadColumn("input_text", "NULL") + ", " +
	purgeSitePayloadColumn("input_html", "NULL") + ", " +
	purgeSitePayloadColumn("input_images", "NULL") + ", " +
	purgeSitePayloadColumn("source_metadata", "NULL") + ", " +
	purgeSitePayloadColumn("payload_purge_due_at", "NULL") + ", " +
	purgeSitePayloadColumn("payload_purged_at", "NOW()") +
	", updated_at = NOW() WHERE id = $1"

func (r *PGXLinkRepository) MarkParseProcessing(ctx context.Context, linkID, jobID uuid.UUID) error {
	return r.updateParseState(ctx, linkID, jobID, model.LinkStatusProcessing, model.JobStatusProcessing, nil)
}

func (r *PGXLinkRepository) MarkParseFailed(ctx context.Context, linkID, jobID uuid.UUID, message string) error {
	return r.updateParseState(ctx, linkID, jobID, model.LinkStatusFailed, model.JobStatusFailed, &message)
}

func (r *PGXLinkRepository) CompleteParse(ctx context.Context, params UpdateLinkAnalysisParams, jobID uuid.UUID) error {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete parse tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return err
	}

	if _, err := updateLinkAnalysisForParseOn(ctx, tx, params); err != nil {
		return err
	}
	if err := completeExactParseJob(ctx, tx, params.ID, jobID, params.ExpectedMetadataRevision); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit complete parse tx: %w", err)
	}
	return nil
}

func (r *PGXLinkRepository) CompleteReadingParse(ctx context.Context, params CompleteReadingParseParams, jobID uuid.UUID) (CompleteReadingParseResult, error) {
	if params.Classification.Kind != model.LibraryKindReading || params.Analysis.ID != params.Classification.ID {
		return CompleteReadingParseResult{}, fmt.Errorf("complete reading parse: final reading classification and matching link id are required")
	}
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("begin complete reading parse tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return CompleteReadingParseResult{}, err
	}
	result, err := updateLinkAnalysisForParseOn(ctx, tx, params.Analysis)
	if err != nil {
		return CompleteReadingParseResult{}, err
	}
	if err := updateLibraryClassificationForCompletionOn(ctx, tx, params.Classification, params.ExpectedRequestedLibraryKind, params.ExpectedRequestedLibraryKindSource); err != nil {
		return CompleteReadingParseResult{}, err
	}
	if params.DetachSiteEntry {
		if err := detachSiteEntryForReadingOn(ctx, tx, params.Analysis.ID, nil, false); err != nil {
			return CompleteReadingParseResult{}, fmt.Errorf("detach site entry for reading completion: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, clearReadingPayloadPurgeDeadlineSQL, params.Analysis.ID); err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("clear reading payload purge deadline: %w", err)
	}
	if err := completeExactParseJob(ctx, tx, params.Analysis.ID, jobID, params.Analysis.ExpectedMetadataRevision); err != nil {
		return CompleteReadingParseResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CompleteReadingParseResult{}, fmt.Errorf("commit complete reading parse tx: %w", err)
	}
	return result, nil
}

func (r *PGXLinkRepository) updateParseState(
	ctx context.Context,
	linkID, jobID uuid.UUID,
	linkStatus model.LinkStatus,
	jobStatus model.JobStatus,
	errorMsg *string,
) error {
	tx, err := r.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin parse state tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := prelockRepresentationWriteGateShared(ctx, tx); err != nil {
		return err
	}

	stateSQL := updateLinkStateSQL
	if linkStatus == model.LinkStatusFailed {
		stateSQL = updateFailedSiteCandidateSQL
	}
	tag, err := tx.Exec(ctx, stateSQL, linkID, linkStatus, errorMsg)
	if err != nil {
		return fmt.Errorf("update link parse state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := updateRunnableParseJobState(ctx, tx, linkID, jobID, jobStatus, errorMsg); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit parse state tx: %w", err)
	}
	return nil
}

func updateRunnableParseJobState(
	ctx context.Context,
	tx pgx.Tx,
	linkID, jobID uuid.UUID,
	status model.JobStatus,
	errorMsg *string,
) error {
	tag, err := tx.Exec(ctx, updateRunnableParseJobStateSQL, jobID, status, errorMsg, linkID)
	if err != nil {
		return fmt.Errorf("update exact parse job state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrParseJobNotRunnable
	}
	return nil
}

func completeExactParseJob(ctx context.Context, tx pgx.Tx, linkID, jobID uuid.UUID, expectedMetadataRevision int64) error {
	tag, err := tx.Exec(ctx, completeParseJobStateSQL, jobID, linkID, expectedMetadataRevision)
	if err != nil {
		return fmt.Errorf("complete exact parse job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrParseJobNotRunnable
	}
	return nil
}

var _ ParseStateStore = (*PGXLinkRepository)(nil)
var _ ReadingParseCompleter = (*PGXLinkRepository)(nil)
