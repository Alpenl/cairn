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

	"webtag/internal/errsafe"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/service"
	"webtag/internal/service/linktranslation"
)

func parseLinkArgs(linkID, parseJobID uuid.UUID) service.ParseLinkArgs {
	return service.ParseLinkArgs{LinkID: linkID, ParseJobID: parseJobID}
}

// ErrTranslationSchedulingPaused reports that the rollout deliberately accepts
// no new translation jobs while legacy work drains.
var ErrTranslationSchedulingPaused = errors.New("translation scheduling is paused during job protocol rollout")

const (
	translationTerminalProjectionTimeout = 10 * time.Second
	defaultTerminalJobRetention          = 7 * 24 * time.Hour
)

func translationArgsFromSeed(seed model.TranslationAttemptSeed, protocol model.TranslationJobScheduleProtocol) (river.JobArgs, error) {
	if protocol == model.TranslationJobScheduleLegacy || protocol == model.TranslationJobScheduleV2 {
		if seed.SourceHash == "" {
			return nil, errors.New("translation attempt seed is missing source_hash")
		}
		if seed.SourceContentRevision != nil && *seed.SourceContentRevision <= 0 {
			return nil, errors.New("translation attempt seed has invalid source_content_revision")
		}
	}
	switch protocol {
	case model.TranslationJobScheduleLegacy:
		return linktranslation.LegacyJobArgs{
			TranslationID:         seed.TranslationID,
			AttemptGeneration:     seed.AttemptGeneration,
			SourceHash:            seed.SourceHash,
			SourceContentRevision: seed.SourceContentRevision,
		}, nil
	case model.TranslationJobScheduleV2:
		return linktranslation.JobArgs{
			TranslationID:         seed.TranslationID,
			AttemptGeneration:     seed.AttemptGeneration,
			SourceHash:            seed.SourceHash,
			SourceContentRevision: seed.SourceContentRevision,
		}, nil
	case model.TranslationJobSchedulePaused:
		return nil, ErrTranslationSchedulingPaused
	default:
		return nil, fmt.Errorf("unsupported translation schedule protocol %q", protocol)
	}
}

// ErrQueueClosed 表示队列已进入停机流程，新的 Enqueue 将被拒收。沿用旧内存
// 队列的同名 sentinel，让上层调用方（linkSubmitter）的错误处理无需改动。
var ErrQueueClosed = errors.New("queue is closed")

const selectActiveParseJobsForLinksSQL = `SELECT id
	FROM river_job
	WHERE kind = $1
	  AND args->>'link_id' = ANY($2::text[])
	  AND ($3::text IS NULL OR COALESCE(args->>'parse_job_id', '') <> $3)
	  AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
	ORDER BY id
	FOR UPDATE`

const selectActiveTranslationJobsForLinksSQL = `SELECT id
	FROM river_job
	WHERE kind = ANY($1::text[])
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
	// the rollout-selected translation workers on the same durable queue.
	TranslationProcessor linktranslation.JobProcessor
	// TranslationAttempts resolves legacy jobs to the exact persisted current
	// attempt. It is required only when the rollout registers a legacy worker.
	TranslationAttempts linktranslation.LegacyAttemptResolver
	// TranslationJobsRollout selects the worker compatibility set and the
	// durable job protocol used by translation scheduling.
	TranslationJobsRollout model.TranslationJobsRolloutStage
	// TranslationJobTimeout overrides the parse-oriented global timeout for
	// multi-chunk translations. Zero falls back to JobTimeout.
	TranslationJobTimeout time.Duration
	// ReaderInboxProcessor is optional for parse-only test harnesses. Production
	// wiring supplies the shared ReaderVNextService so durable Inbox summary
	// jobs run on this same River client.
	ReaderInboxProcessor service.ReaderInboxSummaryJobProcessor
	// LinkEmbeddingBackfill is optional because the embedding feature is
	// capability-gated. When present, River registers an installation-level
	// periodic worker for the idempotent link vector repair scan.
	LinkEmbeddingBackfill interface {
		Run(context.Context) (filled int, failed int, skipped int, err error)
	}
	// ContentHistoryRetention is always supplied by production wiring. It is a
	// direct maintenance repository rather than a service use case: the worker
	// only invokes bounded transactions and never reads snapshot text.
	ContentHistoryRetention contentHistoryCleanupStore
	// Metrics receives aggregate retention outcomes. Labels are intentionally
	// limited to backlog and bounded failure category.
	Metrics *observability.Metrics
	// MaxWorkers 是默认队列的并发 worker 数，映射自 PARSE_CONCURRENCY。<=0
	// 时强制为 1，避免 River 因 MaxWorkers=0 拒绝启动。
	MaxWorkers int
	// JobTimeout 是单条 job 的最长执行时间（0 = River 默认 1 分钟）。一条解析
	// job 要跑完 fetch（含 light→deep 升级、ytdlp 30s 等）+ AI analyze
	// （AI_REQUEST_TIMEOUT_MS 默认 60s）+ embedding 写入，1 分钟默认远不够。
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
	client                 *river.Client[pgx.Tx]
	pool                   *pgxpool.Pool
	logger                 *slog.Logger
	translationJobsRollout model.TranslationJobsRolloutStage
}

// NewRiverQueue 构造 River 客户端并注册解析 worker。它不启动客户端——调用
// Start 才开始拉取 / 执行 job。Pool 与 Processor 必填。
func NewRiverQueue(opts RiverQueueOptions) (*RiverQueue, error) {
	rollout, maxWorkers, err := normalizeRiverQueueOptions(&opts)
	if err != nil {
		return nil, err
	}
	translationPolicy := rollout.JobPolicy()

	workers, err := registerRiverWorkers(opts, rollout, translationPolicy)
	if err != nil {
		return nil, err
	}

	client, err := river.NewClient(
		riverpgxv5.New(opts.Pool),
		newRiverClientConfig(opts, workers, maxWorkers, translationPolicy),
	)
	if err != nil {
		return nil, fmt.Errorf("river queue: new client: %w", err)
	}

	return &RiverQueue{
		client:                 client,
		pool:                   opts.Pool,
		logger:                 opts.Logger,
		translationJobsRollout: rollout,
	}, nil
}

// normalizeRiverQueueOptions validates the required options and fills the
// defaults in place, returning the effective rollout stage and worker count.
// Every default is applied only after the check that guards it, so a caller
// still sees the same first rejection it would have seen with no defaulting at
// all. opts is a pointer so the retention default reaches the river.Config
// built afterwards.
func normalizeRiverQueueOptions(opts *RiverQueueOptions) (model.TranslationJobsRolloutStage, int, error) {
	if opts.Pool == nil {
		return "", 0, fmt.Errorf("river queue: pool is required")
	}
	if opts.Processor == nil {
		return "", 0, fmt.Errorf("river queue: processor is required")
	}
	rollout := opts.TranslationJobsRollout
	if rollout == "" {
		rollout = model.TranslationJobsRolloutStrictV2
	} else if !rollout.Valid() {
		return "", 0, fmt.Errorf("river queue: invalid translation jobs rollout stage %q", rollout)
	}
	maxWorkers := opts.MaxWorkers
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	if opts.TerminalJobRetention < -1 {
		return "", 0, fmt.Errorf("river queue: terminal job retention cannot be less than -1")
	}
	if opts.TerminalJobRetention == 0 {
		opts.TerminalJobRetention = defaultTerminalJobRetention
	}
	return rollout, maxWorkers, nil
}

// registerRiverWorkers builds the worker registry for the compatibility set the
// rollout selected. Translation workers appear only when a processor is wired,
// which keeps parse-only test harnesses free of translation dependencies; a
// legacy worker without its attempt resolver must fail here rather than at
// client construction, since such a worker would pick up legacy jobs it cannot
// resolve to a persisted attempt.
func registerRiverWorkers(
	opts RiverQueueOptions,
	rollout model.TranslationJobsRolloutStage,
	translationPolicy model.TranslationJobsPolicy,
) (*river.Workers, error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, service.NewParseLinkWorker(opts.Processor, opts.JobTimeout))
	if opts.TranslationProcessor != nil {
		if translationPolicy.RegisterLegacyWorker {
			if opts.TranslationAttempts == nil {
				return nil, fmt.Errorf("river queue: legacy translation attempt resolver is required during %s rollout", rollout)
			}
			river.AddWorker(workers, linktranslation.NewLegacyWorker(
				opts.TranslationAttempts,
				opts.TranslationProcessor,
				opts.TranslationJobTimeout,
				opts.Logger,
			))
		}
		if translationPolicy.RegisterV2Worker {
			river.AddWorker(workers, newTranslationV2Worker(opts))
		}
	}
	if opts.ReaderInboxProcessor != nil {
		river.AddWorker(workers, service.NewReaderInboxSummaryWorker(opts.ReaderInboxProcessor, service.ReaderInboxSummaryJobTimeout))
		if expiryProcessor, ok := opts.ReaderInboxProcessor.(service.ReaderInboxExpiryJobProcessor); ok {
			river.AddWorker(workers, NewReaderInboxExpiryWorker(expiryProcessor, service.ReaderInboxExpiryJobTimeout))
		}
	}
	if opts.LinkEmbeddingBackfill != nil {
		river.AddWorker(workers, NewLinkEmbeddingBackfillWorker(opts.LinkEmbeddingBackfill, service.LinkEmbeddingBackfillJobTimeout))
	}
	if opts.ContentHistoryRetention != nil {
		river.AddWorker(workers, NewContentHistoryRetentionWorker(
			opts.ContentHistoryRetention,
			ContentHistoryRetentionJobTimeout,
			opts.Logger,
			opts.Metrics,
		))
	}
	return workers, nil
}

func newTranslationV2Worker(opts RiverQueueOptions) *linktranslation.Worker {
	return linktranslation.NewWorkerWithOptions(
		opts.TranslationProcessor,
		linktranslation.WorkerOptions{
			JobTimeout: opts.TranslationJobTimeout,
			Logger:     opts.Logger,
		},
	)
}

// TranslationJobsRollout returns the immutable stage that selects both this
// queue's worker set/job wire format and the translation service's scheduling
// gate. The service reads it from the queue so the two decisions cannot be
// configured independently at the composition root.
func (q *RiverQueue) TranslationJobsRollout() model.TranslationJobsRolloutStage {
	return q.translationJobsRollout
}

func newRiverClientConfig(
	opts RiverQueueOptions,
	workers *river.Workers,
	maxWorkers int,
	translationPolicy model.TranslationJobsPolicy,
) *river.Config {
	registrations := riverPeriodicJobRegistrations(opts)
	periodicJobs := make([]*river.PeriodicJob, 0, len(registrations))
	for _, registration := range registrations {
		periodicJobs = append(periodicJobs, river.NewPeriodicJob(
			river.PeriodicInterval(registration.interval),
			registration.constructor,
			&river.PeriodicJobOpts{ID: registration.id, RunOnStart: registration.runOnStart},
		))
	}
	return &river.Config{
		PeriodicJobs: periodicJobs,
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
			processor:                 opts.Processor,
			translationProcessor:      opts.TranslationProcessor,
			legacyTranslationAttempts: opts.TranslationAttempts,
			translationPolicy:         translationPolicy,
			logger:                    opts.Logger,
		},
	}
}

type riverPeriodicJobRegistration struct {
	id          string
	interval    time.Duration
	runOnStart  bool
	constructor river.PeriodicJobConstructor
}

func riverPeriodicJobRegistrations(opts RiverQueueOptions) []riverPeriodicJobRegistration {
	registrations := make([]riverPeriodicJobRegistration, 0, 3)
	if _, ok := opts.ReaderInboxProcessor.(service.ReaderInboxExpiryJobProcessor); ok {
		registrations = append(registrations, riverPeriodicJobRegistration{
			id: service.ReaderInboxExpiryJobKind, interval: service.ReaderInboxExpiryInterval, runOnStart: true,
			constructor: func() (river.JobArgs, *river.InsertOpts) {
				return service.ReaderInboxExpiryJobArgs{BatchSize: service.ReaderInboxExpiryBatchSize}, nil
			},
		})
	}
	if opts.LinkEmbeddingBackfill != nil {
		registrations = append(registrations, riverPeriodicJobRegistration{
			id: service.LinkEmbeddingBackfillJobKind, interval: service.LinkEmbeddingBackfillInterval, runOnStart: true,
			constructor: func() (river.JobArgs, *river.InsertOpts) {
				return service.LinkEmbeddingBackfillJobArgs{}, nil
			},
		})
	}
	if opts.ContentHistoryRetention != nil {
		registrations = append(registrations, riverPeriodicJobRegistration{
			id: ContentHistoryRetentionJobKind, interval: ContentHistoryRetentionInterval, runOnStart: true,
			constructor: func() (river.JobArgs, *river.InsertOpts) {
				return ContentHistoryRetentionJobArgs{}, nil
			},
		})
	}
	return registrations
}

type riverErrorHandler struct {
	processor                 service.ParseProcessor
	translationProcessor      linktranslation.JobProcessor
	legacyTranslationAttempts linktranslation.LegacyAttemptResolver
	translationPolicy         model.TranslationJobsPolicy
	logger                    *slog.Logger
}

func (h *riverErrorHandler) HandleError(ctx context.Context, job *rivertype.JobRow, workErr error) *river.ErrorHandlerResult {
	if h.projectFinalFailure(ctx, job, workErr) {
		return &river.ErrorHandlerResult{SetCancelled: true}
	}
	return nil
}

func (h *riverErrorHandler) HandlePanic(ctx context.Context, job *rivertype.JobRow, _ any, _ string) *river.ErrorHandlerResult {
	h.projectFinalFailure(ctx, job, errors.New("river worker panicked"))
	return nil
}

func (h *riverErrorHandler) projectFinalFailure(ctx context.Context, job *rivertype.JobRow, cause error) bool {
	if job == nil {
		return false
	}
	if handled, cancelJob := h.handleTranslationTerminal(ctx, job, cause); handled {
		return cancelJob
	}
	if job.Attempt < job.MaxAttempts {
		return false
	}
	if job.Kind != (service.ParseLinkArgs{}).Kind() {
		return false
	}
	var args service.ParseLinkArgs
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil || args.LinkID == uuid.Nil || args.ParseJobID == uuid.Nil {
		if h.logger != nil {
			h.logger.Error("river discarded parse job with invalid args", "river_job_id", job.ID)
		}
		return false
	}
	if err := h.processor.RecordDiscard(ctx, args.LinkID, args.ParseJobID, cause); err != nil && h.logger != nil {
		h.logger.Error("failed to project discarded parse job", "river_job_id", job.ID, "link_id", args.LinkID.String(), "parse_job_id", args.ParseJobID.String())
	}
	return false
}

func (h *riverErrorHandler) handleTranslationTerminal(ctx context.Context, job *rivertype.JobRow, cause error) (bool, bool) {
	var project func(context.Context, *rivertype.JobRow, error, bool)
	switch job.Kind {
	case (linktranslation.JobArgs{}).Kind():
		project = h.projectV2TranslationTerminal
	case (linktranslation.LegacyJobArgs{}).Kind():
		if !h.translationPolicy.RegisterLegacyWorker {
			return true, false
		}
		project = h.projectLegacyTranslationTerminal
	default:
		return false, false
	}

	if errors.Is(cause, rivertype.ErrJobCancelledRemotely) {
		return true, false
	}
	if errors.Is(cause, errsafe.ErrUnsafeTarget) {
		project(ctx, job, cause, false)
		return true, true
	}
	if errors.Is(cause, context.Canceled) {
		// River cancels the error context before terminal projection. Let the product
		// state update outlive that cancellation, while bounding it with a timeout.
		projectionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), translationTerminalProjectionTimeout)
		defer cancel()
		project(projectionCtx, job, cause, true)
		return true, true
	} else if job.Attempt >= job.MaxAttempts {
		project(ctx, job, cause, false)
	}
	return true, false
}

func (h *riverErrorHandler) projectV2TranslationTerminal(ctx context.Context, job *rivertype.JobRow, cause error, cancelled bool) {
	if h.translationProcessor == nil {
		return
	}
	resolution := translationAttemptFromV2Job(job)
	if resolution.Rejected() {
		h.logTranslationTerminalRejection(ctx, job, resolution.Rejection)
		return
	}
	h.projectResolvedTranslationTerminal(ctx, resolution.Attempt, cause, cancelled, "v2")
}

func (h *riverErrorHandler) projectLegacyTranslationTerminal(ctx context.Context, job *rivertype.JobRow, cause error, cancelled bool) {
	if h.translationProcessor == nil || h.legacyTranslationAttempts == nil {
		return
	}
	if job == nil || job.ID <= 0 {
		h.logTranslationTerminalRejection(ctx, job, model.TranslationAttemptRejectionInvalidRiverJobID)
		return
	}
	if job.Kind != (linktranslation.LegacyJobArgs{}).Kind() {
		h.logTranslationTerminalRejection(ctx, job, model.TranslationAttemptRejectionKindMismatch)
		return
	}
	var args linktranslation.LegacyJobArgs
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		h.logTranslationTerminalRejection(ctx, job, model.TranslationAttemptRejectionMalformedArgs)
		return
	}
	if args.TranslationID == uuid.Nil {
		h.logTranslationTerminalRejection(ctx, job, model.TranslationAttemptRejectionMissingTranslationID)
		return
	}
	resolution, err := linktranslation.ResolveLegacyAttempt(
		ctx, h.legacyTranslationAttempts, args, job.ID,
	)
	if err != nil {
		if h.logger != nil {
			h.logger.ErrorContext(ctx, "failed to resolve legacy translation attempt",
				"river_job_id", job.ID,
				"translation_id", args.TranslationID.String(),
			)
		}
		return
	}
	if resolution.Rejected() {
		h.logTranslationTerminalRejection(ctx, job, resolution.Rejection)
		return
	}
	h.projectResolvedTranslationTerminal(ctx, resolution.Attempt, cause, cancelled, "legacy")
}

func (h *riverErrorHandler) projectResolvedTranslationTerminal(
	ctx context.Context,
	attempt model.TranslationAttempt,
	cause error,
	cancelled bool,
	protocol string,
) {
	projection := "failure"
	if cancelled {
		projection = "cancellation"
	}
	var err error
	if cancelled {
		err = h.translationProcessor.RecordCancellation(ctx, attempt, cause)
	} else {
		err = h.translationProcessor.RecordDiscard(ctx, attempt, cause)
	}
	if err != nil && h.logger != nil {
		h.logger.ErrorContext(ctx, "failed to project terminal translation job",
			"protocol", protocol,
			"projection", projection,
			"river_job_id", attempt.RiverJobID,
			"translation_id", attempt.TranslationID.String(),
		)
	}
}

func translationAttemptFromV2Job(job *rivertype.JobRow) model.TranslationAttemptResolution {
	return linktranslation.ResolveV2Attempt(job)
}

func rejectedTranslationAttemptResolution(reason model.TranslationAttemptRejectionReason) model.TranslationAttemptResolution {
	return model.TranslationAttemptResolution{Rejection: reason}
}

func (h *riverErrorHandler) logTranslationTerminalRejection(
	ctx context.Context,
	job *rivertype.JobRow,
	reason model.TranslationAttemptRejectionReason,
) {
	if h.logger == nil {
		return
	}
	var riverJobID int64
	var kind string
	if job != nil {
		riverJobID = job.ID
		kind = job.Kind
	}
	h.logger.WarnContext(ctx, "translation terminal projection rejected",
		"reason", reason.String(),
		"river_job_id", riverJobID,
		"kind", kind,
	)
}

// Enqueue 在自有事务里插入一条解析 job（非事务调用方用，如 Refresh）。
// args 的 UniqueOpts/MaxAttempts 来自 ParseLinkArgs.InsertOpts（opts 传 nil）。
//
// 返回 ErrQueueClosed 的时机：River 客户端已停机时 Insert 会报错——这里不
// 强行区分，统一冒泡原始错误即可，上层只关心「入队是否成功」。
func (q *RiverQueue) Enqueue(ctx context.Context, linkID, parseJobID uuid.UUID) error {
	_, err := q.client.Insert(ctx, parseLinkArgs(linkID, parseJobID), nil)
	if err != nil {
		return fmt.Errorf("river enqueue link %s: %w", linkID, err)
	}
	return nil
}

// EnqueueReaderInboxSummary inserts a durable Inbox summary job. The job
// identity and captured metadata revision are already persisted by the Reader
// repository; River's unique active-state policy makes enqueue retries safe.
func (q *RiverQueue) EnqueueReaderInboxSummary(ctx context.Context, args service.ReaderInboxSummaryJobArgs) error {
	if _, err := q.client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("river enqueue Reader inbox job %s: %w", args.JobID, err)
	}
	return nil
}

// EnqueueReaderInboxSummaryTx inserts proposal work in the caller's product
// transaction. A rollback removes both the reader_inbox_jobs attempt and the
// River row; a commit makes both visible together.
func (q *RiverQueue) EnqueueReaderInboxSummaryTx(ctx context.Context, tx pgx.Tx, args service.ReaderInboxSummaryJobArgs) error {
	if _, err := q.client.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("river enqueue Reader inbox job %s (tx): %w", args.JobID, err)
	}
	return nil
}

// EnqueueTranslation inserts a durable rollout-selected translation job in its
// own transaction. Production request handling uses EnqueueTranslationTx so
// the pending state transition and River insert commit atomically; this adapter
// is retained for explicit maintenance and integration-test scheduling.
func (q *RiverQueue) EnqueueTranslation(ctx context.Context, seed model.TranslationAttemptSeed) (int64, error) {
	args, err := translationArgsFromSeed(seed, q.translationJobsRollout.JobPolicy().ScheduleProtocol)
	if err != nil {
		return 0, err
	}
	result, err := q.client.Insert(ctx, args, nil)
	if err != nil {
		return 0, fmt.Errorf("river enqueue translation %s: %w", seed.TranslationID, err)
	}
	return translationInsertResultID(result, seed.TranslationID)
}

// EnqueueTranslationTx applies a rollout-selected scheduling command in the
// caller's transaction. When Previous is present it is cancelled before Seed
// is inserted, so rollback restores the old job together with product state.
func (q *RiverQueue) EnqueueTranslationTx(ctx context.Context, tx pgx.Tx, command model.TranslationScheduleCommand) (int64, error) {
	seed := command.Seed
	if command.Previous != nil {
		previous := command.Previous
		if previous.TranslationID != seed.TranslationID ||
			previous.AttemptGeneration >= seed.AttemptGeneration || previous.RiverJobID <= 0 {
			return 0, fmt.Errorf("river enqueue translation %s (tx): invalid superseded attempt", seed.TranslationID)
		}
		if _, err := q.client.JobCancelTx(ctx, tx, previous.RiverJobID); err != nil {
			return 0, fmt.Errorf("river cancel superseded translation %s job %d (tx): %w",
				seed.TranslationID, previous.RiverJobID, err)
		}
	}

	args, err := translationArgsFromSeed(seed, q.translationJobsRollout.JobPolicy().ScheduleProtocol)
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
func (q *RiverQueue) EnqueueTx(ctx context.Context, tx pgx.Tx, linkID, parseJobID uuid.UUID) error {
	_, err := q.client.InsertTx(ctx, tx, parseLinkArgs(linkID, parseJobID), nil)
	if err != nil {
		return fmt.Errorf("river enqueue link %s (tx): %w", linkID, err)
	}
	return nil
}

// CancelActiveTx cancels every older active River attempt for linkID inside
// the caller's requeue transaction. River immediately finalizes queued jobs;
// running jobs receive cancel_attempted_at plus a transactional NOTIFY so the
// worker that owns them cancels its context after commit.
func (q *RiverQueue) CancelActiveTx(ctx context.Context, tx pgx.Tx, linkID, keepParseJobID uuid.UUID) error {
	keep := keepParseJobID.String()
	return q.cancelActiveForLinksTx(ctx, tx, []uuid.UUID{linkID}, &keep)
}

// CancelAllActiveTx cancels every active River parse and translation row for
// linkID. It is the delete counterpart of CancelActiveTx: no attempt is
// retained, including legacy parse rows whose args omit parse_job_id.
func (q *RiverQueue) CancelAllActiveTx(ctx context.Context, tx pgx.Tx, linkID uuid.UUID) error {
	return q.cancelAllActiveForLinksTx(ctx, tx, []uuid.UUID{linkID})
}

func (q *RiverQueue) cancelAllActiveForLinksTx(ctx context.Context, tx pgx.Tx, linkIDs []uuid.UUID) error {
	if len(linkIDs) == 0 {
		return nil
	}
	jobIDs, err := q.activeParseJobIDsForLinks(ctx, tx, linkIDs, nil)
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

func (q *RiverQueue) cancelActiveForLinksTx(ctx context.Context, tx pgx.Tx, linkIDs []uuid.UUID, keepParseJobID *string) error {
	if len(linkIDs) == 0 {
		return nil
	}
	jobIDs, err := q.activeParseJobIDsForLinks(ctx, tx, linkIDs, keepParseJobID)
	if err != nil {
		return err
	}
	return q.cancelRiverJobsTx(ctx, tx, jobIDs)
}

func (q *RiverQueue) activeParseJobIDsForLinks(ctx context.Context, tx pgx.Tx, linkIDs []uuid.UUID, keepParseJobID *string) ([]int64, error) {
	encodedLinkIDs := make([]string, 0, len(linkIDs))
	for _, linkID := range linkIDs {
		encodedLinkIDs = append(encodedLinkIDs, linkID.String())
	}
	rows, err := tx.Query(
		ctx,
		selectActiveParseJobsForLinksSQL,
		(service.ParseLinkArgs{}).Kind(),
		encodedLinkIDs,
		keepParseJobID,
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
		[]string{
			(linktranslation.LegacyJobArgs{}).Kind(),
			(linktranslation.JobArgs{}).Kind(),
		},
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
