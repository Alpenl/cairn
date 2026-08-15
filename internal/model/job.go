package model

import (
	"time"

	"github.com/google/uuid"
)

// JobStatus 表示一次解析任务（parse_jobs 表）的生命周期状态。
type JobStatus string

// 解析任务的四种状态。
const (
	JobStatusPending    JobStatus = "pending"    // 已入队，等待 worker 拉起
	JobStatusProcessing JobStatus = "processing" // worker 正在执行解析流水线
	JobStatusDone       JobStatus = "done"       // 解析完成
	JobStatusFailed     JobStatus = "failed"     // 解析失败，详见 ErrorMsg
)

// ParseJob 对应 parse_jobs 表的一行，记录一次链接解析任务的运行状态。
type ParseJob struct {
	ID     uuid.UUID
	LinkID uuid.UUID
	// ExpectedMetadataRevision is the immutable Link metadata revision observed
	// when this parse attempt was enqueued. Zero identifies a pre-fence legacy
	// row and deliberately grants no authority to replace title, summary, or
	// tags when it later reaches a terminal state.
	ExpectedMetadataRevision int64
	Status                   JobStatus
	ErrorMsg                 *string // 失败时的简短错误描述
	CreatedAt                time.Time
	UpdatedAt                time.Time
}
