package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/errsafe"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// parseLinkJobKind 是 River 解析任务的 kind 标识。River 用 kind 字符串把
// 入库的 job args（JSON）路由到对应 worker；改这个值等于换一种 job 类型，
// 历史 job 会变成「未知 kind」被 rescuer 丢弃，因此一旦上线不可轻易更名。
const parseLinkJobKind = "parse_link"

// parseLinkMaxAttempts 是单条解析 job 的最大尝试次数。
//
// WHY 不是 1：River 的 rescuer 只在 Attempt < MaxAttempts 时把卡死（进程
// 崩溃时仍处于 running）的 job 重新排队，否则直接 discard。若设为 1，进程
// 在 Work 执行到一半被 kill，重启后该 job 会被 rescuer 判为「已达上限」直接
// 丢弃——等于任务丢失，违背 Phase 13「崩溃恢复由 River 接管」的目标。设为
// 3 给崩溃恢复留出重试余量。
//
// WHY 不需要担心确定性解析失败被重试：ParsePipeline.Run 失败时会自行把
// links 写成 failed 并返回 *PipelineRunError（errors.Is
// ErrAlreadyPersisted）。Work 把这种「已持久化的业务失败」视为 job 成功
// （返回 nil），River 不再重试——与旧内存队列「一次性、失败即终态」语义
// 一致。只有 Work 真正返回 error（未预期的基础设施抖动 / panic）才进入
// River 的指数退避重试，3 次封顶。
const parseLinkMaxAttempts = 3

// ParseLinkArgs 是 River 解析任务的入参，序列化为 job 行的 args(JSONB)。
// 业务状态只在 links；代次和元数据修订是 River 行携带的不可变写入栅栏。
type ParseLinkArgs struct {
	LinkID                   uuid.UUID `json:"link_id"`
	ParseGeneration          int64     `json:"parse_generation"`
	ExpectedMetadataRevision int64     `json:"expected_metadata_revision"`
}

func (a ParseLinkArgs) Attempt() model.ParseAttempt {
	return model.ParseAttempt{
		LinkID:                   a.LinkID,
		Generation:               a.ParseGeneration,
		ExpectedMetadataRevision: a.ExpectedMetadataRevision,
	}
}

// Kind 返回 River 路由用的 job 类型标识。
func (ParseLinkArgs) Kind() string { return parseLinkJobKind }

// InsertOpts 声明本类 job 的默认入队选项：
//
//   - UniqueOpts.ByArgs + ByState：按 (kind, args) 唯一，但**仅在未完成状态**
//     （available/pending/running/scheduled/retryable）去重，替代旧 worker.Queue
//     的 in-flight map + service/locker 去重。
//
//     ⚠️ 必须显式设 ByState：River v0.39 的 ByState 默认值**包含 completed**
//     （见 vendor insert_opts.go ByState 注释），即一条已完成的 parse_link job
//     会一直挡住同 args 的重新入队，直到 River job cleaner 把它清出 river_job 表
//     （默认保留 24h）。这会让 link 完成后再次提交（refresh）排不进新 job、永久
//     卡 pending。这里只列举 in-flight 状态，从而自然排除 completed/discarded/
//     cancelled——refresh / backfill 总能为已结束的 link 排新 job，而仍在飞的
//     job 不会被重复入队。
//     注：available/pending/running/scheduled 是 River 要求必含的状态；retryable
//     保留以避免对正在重试的 link 重复排队。
//
//   - MaxAttempts：见 parseLinkMaxAttempts 注释。
func (ParseLinkArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		MaxAttempts: parseLinkMaxAttempts,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}

// ParseProcessor 是 River worker 委托的业务处理器。生产实现是
// *ParsePipeline（其 Run 方法签名匹配）。抽成接口让 worker 单测可注入
// 轻量 fake，无需拉起整条解析管线。
type ParseProcessor interface {
	Run(context.Context, model.ParseAttempt) error
	RecordDiscard(context.Context, model.ParseAttempt, error) error
}

// ParseLinkWorker 是 River 解析任务的 worker。它把 River 调度过来的 job
// 转交给 ParsePipeline.Run；Link 保存业务状态，River 保存执行、重试和终态
// 历史，两者通过不可变 ParseAttempt 栅栏避免旧任务覆盖新结果。
//
// 嵌入 river.WorkerDefaults 拿到 Timeout/NextRetry 等默认实现，只覆盖 Work。
type ParseLinkWorker struct {
	river.WorkerDefaults[ParseLinkArgs]
	processor ParseProcessor
	// jobTimeout 覆盖 River 全局 JobTimeout，可选；<=0 时用 River 默认。
	jobTimeout time.Duration
}

// NewParseLinkWorker 构造 worker。processor 必填（生产传 *ParsePipeline）。
func NewParseLinkWorker(processor ParseProcessor, jobTimeout time.Duration) *ParseLinkWorker {
	return &ParseLinkWorker{processor: processor, jobTimeout: jobTimeout}
}

// Timeout 暴露 per-job 超时给 River（0 表示用 Config.JobTimeout 默认）。
func (w *ParseLinkWorker) Timeout(*river.Job[ParseLinkArgs]) time.Duration {
	return w.jobTimeout
}

// Work 执行一次解析。返回值的语义决定 River 的后续动作：
//   - nil → job 标记 completed，不再重试。
//   - 非 nil → job 进入 River 的指数退避重试（直到 MaxAttempts），超出后
//     discard。
//
// 关键映射：ParsePipeline.Run 失败时已经把 Link 写成 failed 并
// 返回 *PipelineRunError（errors.Is ErrAlreadyPersisted）。这类「业务上确定
// 失败、且已持久化终态」的结果对 River 而言是「job 干完了」——返回 nil，避免
// River 反复重跑一条注定失败的解析。只有未
// 预期错误（Run 在写失败状态前自己就崩了，或基础设施抖动）才向 River 冒泡
// 触发重试。
func (w *ParseLinkWorker) Work(ctx context.Context, job *river.Job[ParseLinkArgs]) error {
	attempt := job.Args.Attempt()
	if attempt.LinkID == uuid.Nil || attempt.Generation <= 0 || attempt.ExpectedMetadataRevision <= 0 {
		return errors.New("parse_link job has invalid attempt args")
	}
	err := w.processor.Run(ctx, attempt)
	if err == nil {
		return nil
	}
	// 管线已自行持久化失败终态 → River 视为完成，不重试。
	if errors.Is(err, errsafe.ErrAlreadyPersisted) {
		return nil
	}
	if errors.Is(err, repository.ErrParseAttemptNotRunnable) {
		return nil
	}
	// 未预期错误 → 冒泡给 River，进入重试 / rescue 路径。
	return err
}
