package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"webtag/internal/errsafe"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

const parseFailurePersistTimeout = 5 * time.Second

// loadLinkAndJob resolves the link + the exact parse_jobs row carried by the
// River job. Both must
// exist; a missing link or job is a programming error (the queue worker
// only schedules ids that round-trip through the link store) so the
// function returns a wrapped error without recording metrics — those would
// pollute the failure dashboard with bookkeeping bugs.
func (p *ParsePipeline) loadLinkAndJob(ctx context.Context, linkID, jobID uuid.UUID) (*repository.LinkParseInput, *model.ParseJob, error) {
	link, err := p.links.GetParseInputByID(ctx, linkID)
	if err != nil {
		return nil, nil, err
	}
	if link == nil {
		return nil, nil, fmt.Errorf("link %s not found", linkID)
	}

	job, err := p.jobs.GetByID(ctx, jobID)
	if err != nil {
		return nil, nil, err
	}
	if job == nil {
		return nil, nil, fmt.Errorf("job %s for link %s not found", jobID, linkID)
	}
	if job.LinkID != linkID {
		return nil, nil, fmt.Errorf("job %s belongs to link %s, not %s", jobID, job.LinkID, linkID)
	}
	if job.Status != model.JobStatusPending && job.Status != model.JobStatusProcessing {
		return nil, nil, repository.ErrParseJobNotRunnable
	}
	return link, job, nil
}

// markProcessing flips both rows to "processing" before the heavy work
// (fetch/analyze) starts so observers (the API GET /api/links/{id} read
// path, queue debug tools) see in-progress state. Either UpdateState
// failure aborts the run by returning an error to the River worker, which
// surfaces it as a non-ErrAlreadyPersisted error → River retries the job
// (Phase 13 / v4.0 M2; the old in-memory queue relied on
// ResetProcessingToPending at startup, now superseded by River retry/rescue).
func (p *ParsePipeline) markProcessing(ctx context.Context, linkID, jobID uuid.UUID) error {
	return p.links.MarkParseProcessing(ctx, linkID, jobID)
}

func (p *ParsePipeline) fail(ctx context.Context, linkID, jobID uuid.UUID, err error) error {
	// error_msg gets persisted (and surfaced via the API), so strip vendor
	// payloads down to "<category>: <safe summary>" before writing it. The
	// raw error keeps flowing into the structured logger below.
	safe := errsafe.SafeMessage(err)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parseFailurePersistTimeout)
	defer cancel()
	if updateErr := p.links.MarkParseFailed(cleanupCtx, linkID, jobID, safe); updateErr != nil {
		return updateErr
	}
	// Wave 2 H5：用 observability.SafeError 包裹 err，避免 OpenAI key /
	// postgres DSN / Bearer token 等凭据原样进日志。SafeError 内部已经
	// 调用 errsafe.ClassifyError 计算 category，再单独传一遍是冗余，
	// 但保留可以让告警表达式继续基于顶层 error_category 字段，不必跟
	// 着重构。
	p.logError(cleanupCtx, "pipeline marked link and job failed",
		"link_id", linkID.String(),
		"job_id", jobID.String(),
		"error", observability.SafeError(err),
		"error_category", errsafe.ClassifyError(err),
	)
	return &PipelineRunError{Cause: err}
}

// RecordDiscard projects River's final infrastructure failure into the
// product-facing state machine. Intermediate retries never call this method;
// worker.parseErrorHandler invokes it only when Attempt >= MaxAttempts.
func (p *ParsePipeline) RecordDiscard(ctx context.Context, linkID, jobID uuid.UUID, cause error) error {
	err := p.fail(ctx, linkID, jobID, fmt.Errorf("parse worker exhausted retries: %w", cause))
	if errors.Is(err, errsafe.ErrAlreadyPersisted) || errors.Is(err, repository.ErrParseJobNotRunnable) {
		return nil
	}
	return err
}
