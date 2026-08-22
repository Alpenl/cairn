// Package worker hosts the parse-job execution layer. Phase 13 (v4.0 M2)
// replaced the in-memory channel queue (queue.go, retired) with River — a
// PostgreSQL-native transactional job queue. RiverQueue is the thin façade
// the rest of the app talks to: it owns the *river.Client lifecycle and
// exposes Enqueue / EnqueueTx / Ready / Start / Stop so callers (the link
// submitter, the app runtime) need not import river directly.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/model"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
)

func parseLinkArgs(attempt model.ParseAttempt) service.ParseLinkArgs {
	return service.ParseLinkArgs{
		LinkID:                   attempt.LinkID,
		ParseGeneration:          attempt.Generation,
		ExpectedMetadataRevision: attempt.ExpectedMetadataRevision,
	}
}

const defaultTerminalJobRetention = 7 * 24 * time.Hour

func translationArgsFromSeed(seed model.TranslationAttemptSeed) (linktranslation.JobArgs, error) {
	if seed.SourceHash == "" {
		return linktranslation.JobArgs{}, errors.New("translation attempt seed is missing source_hash")
	}
	if seed.SourceContentRevision != nil && *seed.SourceContentRevision <= 0 {
		return linktranslation.JobArgs{}, errors.New("translation attempt seed has invalid source_content_revision")
	}
	return linktranslation.JobArgs{
		TranslationID:         seed.TranslationID,
		AttemptGeneration:     seed.AttemptGeneration,
		SourceHash:            seed.SourceHash,
		SourceContentRevision: seed.SourceContentRevision,
	}, nil
}

// ErrQueueClosed 表示队列已进入停机流程，新的 Enqueue 将被拒收。沿用旧内存
// 队列的同名 sentinel，让上层调用方（linkSubmitter）的错误处理无需改动。
var ErrQueueClosed = errors.New("queue is closed")

const selectActiveParseRiverJobsForLinksSQL = `SELECT id
	FROM river_job
	WHERE kind = $1
	  AND args->>'link_id' = ANY($2::text[])
	  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
	ORDER BY id
	FOR UPDATE`

const selectActiveTranslationJobsForLinksSQL = `SELECT id
	FROM river_job
	WHERE kind = $1
	  AND args->>'translation_id' IN (
		SELECT id::text
		FROM link_translations
		WHERE link_id::text = ANY($2::text[])
	  )
	  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
	ORDER BY id
	FOR UPDATE`

// RiverQueueOptions 是 NewRiverQueue 的装配参数。
type RiverQueueOptions struct {
	// Pool 是 River 客户端与入队事务共用的连接池。River 用 riverpgxv5 驱动
	// 包装它；EnqueueTx 接收的 pgx.Tx 必须由同一个 Pool 开出，River 才能把
	// job 插进调用方事务、与 link 写同生共死。
	Pool *pgxpool.Pool
	// Processor 是解析任务的业务处理器（生产传 *service.ParsePipeline）。
	Processor service.ParseProcessor
	// TranslationProcessor is optional for compatibility with parse-only test
	// harnesses. Production wiring always supplies it and therefore registers
	// the translation worker on the same durable queue.
	TranslationProcessor linktranslation.JobProcessor
	// TranslationJobTimeout overrides the parse-oriented global timeout for
	// multi-chunk translations. Zero falls back to JobTimeout.
	TranslationJobTimeout time.Duration
	// ReaderInboxProcessor is optional for parse-only test harnesses. Production
	// wiring supplies the Reader Inbox processor so durable summary
	// jobs run on this same River client.
	ReaderInboxProcessor service.ReaderInboxSummaryJobProcessor
	// MaxWorkers 是默认队列的并发 worker 数，映射自 PARSE_CONCURRENCY。<=0
	// 时强制为 1，避免 River 因 MaxWorkers=0 拒绝启动。
	MaxWorkers int
	// JobTimeout 是单条 job 的最长执行时间（0 = River 默认 1 分钟）。一条解析
	// job 要跑完 fetch 和 AI analyze（AI_REQUEST_TIMEOUT_MS 默认 60s），
	// 1 分钟默认不够。
	JobTimeout time.Duration
	// RescueAfter 是 job 进入 running 后多久被 rescuer 判为「卡死」并重排 /
	// discard（River.Config.RescueStuckJobsAfter）。0 = River 默认 1 小时。
	//
	// 约束：River 要求 RescueAfter >= JobTimeout（否则仍在正常执行的 job 会被
	// 误判卡死提前 rescue）。装配层据此设为 JobTimeout + 缓冲，详见 buildQueue。
	RescueAfter time.Duration
	// TerminalJobRetention is applied to cancelled, completed, and discarded
	// rows. Zero selects the safe seven-day default; -1 is an explicit rollback
	// escape hatch that disables cleanup.
	TerminalJobRetention time.Duration
	// Logger 透传给 River 客户端做结构化日志。
	Logger *slog.Logger
}

// RiverQueue 封装 *river.Client，对上层提供与旧 worker.Queue 兼容的最小接口
// （Enqueue / Ready / Start / Stop）外加事务性入队 EnqueueTx。
//
// 与旧内存队列的语义差异（均为预期）：
//   - 不再维护 in-flight map / seedDone / canceledRunCtx：去重交给 River 的
//     unique job（见 service.ParseLinkArgs.InsertOpts），崩溃恢复交给 River
//     的 rescuer，启动 seed 不再需要（pending job 已在 river_job 表里，River
//     Start 后自动拉取）。
//   - 失败兜底持久化不再在队列层：ParsePipeline.Run 自行写 failed 终态；
//     River 对返回 error 的 job 走重试 / rescue / discard。
type RiverQueue struct {
	client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewRiverQueue 构造 River 客户端并注册解析 worker。它不启动客户端——调用
// Start 才开始拉取 / 执行 job。Pool 与 Processor 必填。
func NewRiverQueue(opts RiverQueueOptions) (*RiverQueue, error) {
	maxWorkers, err := normalizeRiverQueueOptions(&opts)
	if err != nil {
		return nil, err
	}
	workers := registerRiverWorkers(opts)

	client, err := river.NewClient(
		riverpgxv5.New(opts.Pool),
		newRiverClientConfig(opts, workers, maxWorkers),
	)
	if err != nil {
		return nil, fmt.Errorf("river queue: new client: %w", err)
	}

	return &RiverQueue{
		client: client,
		pool:   opts.Pool,
		logger: opts.Logger,
	}, nil
}

// normalizeRiverQueueOptions validates the required options and fills the
// defaults in place, returning the effective worker count.
// Every default is applied only after the check that guards it, so a caller
// still sees the same first rejection it would have seen with no defaulting at
// all. opts is a pointer so the retention default reaches the river.Config
// built afterwards.
func normalizeRiverQueueOptions(opts *RiverQueueOptions) (int, error) {
	if opts.Pool == nil {
		return 0, fmt.Errorf("river queue: pool is required")
	}
	if opts.Processor == nil {
		return 0, fmt.Errorf("river queue: processor is required")
	}
	maxWorkers := opts.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if opts.TerminalJobRetention < -1 {
		return 0, fmt.Errorf("river queue: terminal job retention cannot be less than -1")
	}
	if opts.TerminalJobRetention == 0 {
		opts.TerminalJobRetention = defaultTerminalJobRetention
	}
	return maxWorkers, nil
}

// registerRiverWorkers keeps translation and Inbox optional for parse-only test
// harnesses. Production wiring supplies all three processors.
func registerRiverWorkers(opts RiverQueueOptions) *river.Workers {
	workers := river.NewWorkers()
	river.AddWorker(workers, service.NewParseLinkWorker(opts.Processor, opts.JobTimeout))
	if opts.TranslationProcessor != nil {
		river.AddWorker(workers, newTranslationWorker(opts))
	}
	if opts.ReaderInboxProcessor != nil {
		river.AddWorker(workers, service.NewReaderInboxSummaryWorker(opts.ReaderInboxProcessor, service.ReaderInboxSummaryJobTimeout))
	}
	return workers
}

func newTranslationWorker(opts RiverQueueOptions) *linktranslation.Worker {
	return linktranslation.NewWorkerWithOptions(
		opts.TranslationProcessor,
		linktranslation.WorkerOptions{
			JobTimeout: opts.TranslationJobTimeout,
			Logger:     opts.Logger,
		},
	)
}

func newRiverClientConfig(
	opts RiverQueueOptions,
	workers *river.Workers,
	maxWorkers int,
) *river.Config {
	return &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: maxWorkers},
		},
		Workers:                     workers,
		JobTimeout:                  opts.JobTimeout,
		CancelledJobRetentionPeriod: opts.TerminalJobRetention,
		CompletedJobRetentionPeriod: opts.TerminalJobRetention,
		DiscardedJobRetentionPeriod: opts.TerminalJobRetention,
		// RescueStuckJobsAfter 把崩溃恢复时延从 River 默认的 1 小时压到分钟级
		// （见 buildQueue 推导）。0 时 River 回退默认 1h。
		RescueStuckJobsAfter: opts.RescueAfter,
		Logger:               opts.Logger,
		ErrorHandler: &riverErrorHandler{
			processor: opts.Processor,
			logger:    opts.Logger,
		},
	}
}

type riverErrorHandler struct {
	processor service.ParseProcessor
	logger    *slog.Logger
}

func (h *riverErrorHandler) HandleError(ctx context.Context, job *rivertype.JobRow, workErr error) *river.ErrorHandlerResult {
	h.projectFinalFailure(ctx, job, workErr)
	return nil
}

func (h *riverErrorHandler) HandlePanic(ctx context.Context, job *rivertype.JobRow, _ any, _ string) *river.ErrorHandlerResult {
	h.projectFinalFailure(ctx, job, errors.New("river worker panicked"))
	return nil
}

func (h *riverErrorHandler) projectFinalFailure(ctx context.Context, job *rivertype.JobRow, cause error) {
	if job == nil {
		return
	}
	if job.Attempt < job.MaxAttempts {
		return
	}
	if job.Kind != (service.ParseLinkArgs{}).Kind() {
		return
	}
	var args service.ParseLinkArgs
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil || args.LinkID == uuid.Nil || args.ParseGeneration <= 0 || args.ExpectedMetadataRevision <= 0 {
		if h.logger != nil {
			h.logger.Error("river discarded parse job with invalid args", "river_job_id", job.ID)
		}
		return
	}
	if err := h.processor.RecordDiscard(ctx, args.Attempt(), cause); err != nil && h.logger != nil {
		h.logger.Error("failed to project discarded parse job", "river_job_id", job.ID, "link_id", args.LinkID.String(), "parse_generation", args.ParseGeneration)
	}
}

// Enqueue 在自有事务里插入一条解析 job（非事务调用方用，如 Refresh）。
// args 的 UniqueOpts/MaxAttempts 来自 ParseLinkArgs.InsertOpts（opts 传 nil）。
//
// 返回 ErrQueueClosed 的时机：River 客户端已停机时 Insert 会报错——这里不
// 强行区分，统一冒泡原始错误即可，上层只关心「入队是否成功」。
func (q *RiverQueue) Enqueue(ctx context.Context, attempt model.ParseAttempt) error {
	_, err := q.client.Insert(ctx, parseLinkArgs(attempt), nil)
	if err != nil {
		return fmt.Errorf("river enqueue link %s generation %d: %w", attempt.LinkID, attempt.Generation, err)
	}
	return nil
}

// EnqueueReaderInboxSummaryTx inserts proposal work in the caller's product
// transaction. A rollback removes both the Inbox proposal transition and the
// River row; a commit makes both visible together.
func (q *RiverQueue) EnqueueReaderInboxSummaryTx(ctx context.Context, tx pgx.Tx, args service.ReaderInboxSummaryJobArgs) error {
	if _, err := q.client.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("river enqueue Reader inbox %s revision %d (tx): %w", args.InboxID, args.ExpectedMetadataRevision, err)
	}
	return nil
}

// EnqueueTranslation inserts a durable translation job in its own transaction.
// Production request handling uses EnqueueTranslationTx so
// the pending state transition and River insert commit atomically; this adapter
// is retained for explicit maintenance and integration-test scheduling.
func (q *RiverQueue) EnqueueTranslation(ctx context.Context, seed model.TranslationAttemptSeed) (int64, error) {
	args, err := translationArgsFromSeed(seed)
	if err != nil {
		return 0, err
	}
	result, err := q.client.Insert(ctx, args, nil)
	if err != nil {
		return 0, fmt.Errorf("river enqueue translation %s: %w", seed.TranslationID, err)
	}
	return translationInsertResultID(result, seed.TranslationID)
}

// EnqueueTranslationTx inserts a generation-fenced translation job in the
// caller's product transaction. Older jobs may finish in River, but cannot
// project after the product generation advances.
func (q *RiverQueue) EnqueueTranslationTx(ctx context.Context, tx pgx.Tx, seed model.TranslationAttemptSeed) (int64, error) {
	args, err := translationArgsFromSeed(seed)
	if err != nil {
		return 0, err
	}
	result, err := q.client.InsertTx(ctx, tx, args, nil)
	if err != nil {
		return 0, fmt.Errorf("river enqueue translation %s (tx): %w", seed.TranslationID, err)
	}
	return translationInsertResultID(result, seed.TranslationID)
}

func translationInsertResultID(result *rivertype.JobInsertResult, translationID uuid.UUID) (int64, error) {
	if result == nil || result.Job == nil || result.Job.ID <= 0 {
		return 0, fmt.Errorf("river enqueue translation %s: insert returned no job", translationID)
	}
	if result.UniqueSkippedAsDuplicate {
		return 0, fmt.Errorf("river enqueue translation %s: attempt unexpectedly matched an active duplicate", translationID)
	}
	return result.Job.ID, nil
}

// EnqueueTx 在调用方提供的事务里插入解析 job，实现「入队与 link 写同事务」。
// tx 必须由同一个 Pool 开出。事务回滚则 job 不入库（River 的快照可见性保证
// 跨事务，未提交的 job 不会被 worker 拉取）。
func (q *RiverQueue) EnqueueTx(ctx context.Context, tx pgx.Tx, attempt model.ParseAttempt) error {
	_, err := q.client.InsertTx(ctx, tx, parseLinkArgs(attempt), nil)
	if err != nil {
		return fmt.Errorf("river enqueue link %s generation %d (tx): %w", attempt.LinkID, attempt.Generation, err)
	}
	return nil
}

// CancelActiveTx cancels every older active River attempt for linkID inside
// the caller's requeue transaction. River immediately finalizes queued jobs;
// running jobs receive cancel_attempted_at plus a transactional NOTIFY so the
// worker that owns them cancels its context after commit.
func (q *RiverQueue) CancelActiveTx(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	return q.cancelActiveForLinksTx(ctx, tx, []uuid.UUID{linkID})
}

// CancelAllActiveTx cancels every active River parse and translation row for
// linkID. It is the delete counterpart of CancelActiveTx: no attempt is
// retained, including rows created by older argument protocols.
func (q *RiverQueue) CancelAllActiveTx(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	return q.cancelAllActiveForLinksTx(ctx, tx, []uuid.UUID{linkID})
}

func (q *RiverQueue) cancelAllActiveForLinksTx(ctx context.Context, tx pgx.Tx, linkIDs []uuid.UUID) error {
	if len(linkIDs) == 0 {
		return nil
	}
	jobIDs, err := q.activeParseRiverJobIDsForLinks(ctx, tx, linkIDs)
	if err != nil {
		return err
	}
	translationJobIDs, err := activeTranslationJobIDsForLinks(ctx, tx, linkIDs)
	if err != nil {
		return err
	}
	jobIDs = append(jobIDs, translationJobIDs...)
	return q.cancelRiverJobsTx(ctx, tx, jobIDs)
}

func (q *RiverQueue) cancelActiveForLinksTx(ctx context.Context, tx pgx.Tx, linkIDs []uuid.UUID) error {
	if len(linkIDs) == 0 {
		return nil
	}
	jobIDs, err := q.activeParseRiverJobIDsForLinks(ctx, tx, linkIDs)
	if err != nil {
		return err
	}
	return q.cancelRiverJobsTx(ctx, tx, jobIDs)
}

func (q *RiverQueue) activeParseRiverJobIDsForLinks(ctx context.Context, tx pgx.Tx, linkIDs []uuid.UUID) ([]int64, error) {
	encodedLinkIDs := make([]string, 0, len(linkIDs))
	for _, linkID := range linkIDs {
		encodedLinkIDs = append(encodedLinkIDs, linkID.String())
	}
	rows, err := tx.Query(
		ctx,
		selectActiveParseRiverJobsForLinksSQL,
		(service.ParseLinkArgs{}).Kind(),
		encodedLinkIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("list active River parse attempts for %d links: %w", len(linkIDs), err)
	}
	jobIDs := make([]int64, 0)
	for rows.Next() {
		var jobID int64
		if err := rows.Scan(&jobID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan active River parse attempt: %w", err)
		}
		jobIDs = append(jobIDs, jobID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate active River parse attempts: %w", err)
	}
	rows.Close()
	return jobIDs, nil
}

func activeTranslationJobIDsForLinks(ctx context.Context, tx pgx.Tx, linkIDs []uuid.UUID) ([]int64, error) {
	encodedLinkIDs := make([]string, 0, len(linkIDs))
	for _, linkID := range linkIDs {
		encodedLinkIDs = append(encodedLinkIDs, linkID.String())
	}
	rows, err := tx.Query(
		ctx,
		selectActiveTranslationJobsForLinksSQL,
		(linktranslation.JobArgs{}).Kind(),
		encodedLinkIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("list active River translation attempts for %d links: %w", len(linkIDs), err)
	}
	defer rows.Close()

	jobIDs := make([]int64, 0)
	for rows.Next() {
		var jobID int64
		if err := rows.Scan(&jobID); err != nil {
			return nil, fmt.Errorf("scan active River translation attempt: %w", err)
		}
		jobIDs = append(jobIDs, jobID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active River translation attempts: %w", err)
	}
	return jobIDs, nil
}

func (q *RiverQueue) cancelRiverJobsTx(ctx context.Context, tx pgx.Tx, jobIDs []int64) error {
	for _, riverJobID := range jobIDs {
		if _, err := q.client.JobCancelTx(ctx, tx, riverJobID); err != nil {
			return fmt.Errorf("cancel River attempt %d: %w", riverJobID, err)
		}
	}
	return nil
}

// Start 启动 River 客户端开始拉取并执行 job。幂等性由 River 客户端自身保证。
func (q *RiverQueue) Start(ctx context.Context) error {
	if err := q.client.Start(ctx); err != nil {
		return fmt.Errorf("river queue start: %w", err)
	}
	return nil
}

// Ready 报告队列是否就绪。River 客户端 Start 成功后即可接收并执行 job——
// 没有旧队列那种「seed backlog 未排完」的中间态（pending job 由 River 自己
// 从 river_job 表拉，不需要应用层 seed）。实现用 Stopped() 通道是否已关闭
// 反推运行状态：通道未关 = 运行中 = ready，已关 = 已停机 = not ready。
//
// 不引入额外的 started 标志：Ready 只负责「停机后翻成 false」，把「Start 是否
// 已成功」交给 app 装配层——readiness 聚合（见 deps_router 的 readiness 闭包）
// 只在 queue.Start 返回 nil 后才把本方法纳入，因此 Start 之前 Ready 不会被
// 查询，无需在此区分「尚未 Start」与「运行中」。Stopped() 在 Stop 调用后才
// close，未停机时 select-default 命中「未关闭」分支返回 true。
func (q *RiverQueue) Ready() bool {
	select {
	case <-q.client.Stopped():
		return false
	default:
		return true
	}
}

// Stop 优雅停机：停止拉取新 job 并等待在飞 job 跑完，受 ctx 超时约束。
func (q *RiverQueue) Stop(ctx context.Context) error {
	if err := q.client.Stop(ctx); err != nil {
		return fmt.Errorf("river queue stop: %w", err)
	}
	return nil
}
