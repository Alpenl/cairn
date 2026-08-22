package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"webtag/internal/errsafe"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

const parseFailurePersistTimeout = 5 * time.Second

// loadParseAttempt resolves the Link and rejects work whose immutable River
// generation no longer owns its product state.
func (p *ParsePipeline) loadParseAttempt(ctx context.Context, attempt model.ParseAttempt) (*repository.LinkParseInput, error) {
	link, err := p.links.GetParseInputByID(ctx, attempt.LinkID)
	if err != nil {
		return nil, err
	}
	if link == nil {
		return nil, fmt.Errorf("link %s not found", attempt.LinkID)
	}
	if link.ParseGeneration != attempt.Generation ||
		(link.Status != model.LinkStatusPending && link.Status != model.LinkStatusProcessing) {
		return nil, repository.ErrParseAttemptNotRunnable
	}
	return link, nil
}

// markProcessing flips both rows to "processing" before the heavy work
// (fetch/analyze) starts so observers (the API GET /api/links/{id} read
// path, queue debug tools) see in-progress state. Either UpdateState
// failure aborts the run by returning an error to the River worker, which
// surfaces it as a non-ErrAlreadyPersisted error → River retries the job
// (Phase 13 / v4.0 M2; the old in-memory queue relied on
// ResetProcessingToPending at startup, now superseded by River retry/rescue).
func (p *ParsePipeline) markProcessing(ctx context.Context, attempt model.ParseAttempt) error {
	return p.links.MarkParseProcessing(ctx, attempt)
}

func (p *ParsePipeline) fail(ctx context.Context, attempt model.ParseAttempt, err error) error {
	// error_msg gets persisted (and surfaced via the API), so strip vendor
	// payloads down to "<category>: <safe summary>" before writing it. The
	// raw error keeps flowing into the structured logger below.
	safe := errsafe.SafeMessage(err)
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parseFailurePersistTimeout)
	defer cancel()
	if updateErr := p.links.MarkParseFailed(cleanupCtx, attempt, safe); updateErr != nil {
		return updateErr
	}
	// Wave 2 H5：用 observability.SafeError 包裹 err，避免 OpenAI key /
	// postgres DSN / Bearer token 等凭据原样进日志。SafeError 内部已经
	// 调用 errsafe.ClassifyError 计算 category，再单独传一遍是冗余，
	// 但保留可以让告警表达式继续基于顶层 error_category 字段，不必跟
	// 着重构。
	p.logError(cleanupCtx, "pipeline marked link and job failed",
		"link_id", attempt.LinkID.String(),
		"parse_generation", attempt.Generation,
		"error", observability.SafeError(err),
		"error_category", errsafe.ClassifyError(err),
	)
	return &PipelineRunError{Cause: err}
}

// RecordDiscard projects River's final infrastructure failure into the
// product-facing state machine. Intermediate retries never call this method;
// worker.parseErrorHandler invokes it only when Attempt >= MaxAttempts.
func (p *ParsePipeline) RecordDiscard(ctx context.Context, attempt model.ParseAttempt, cause error) error {
	err := p.fail(ctx, attempt, fmt.Errorf("parse worker exhausted retries: %w", cause))
	if errors.Is(err, errsafe.ErrAlreadyPersisted) || errors.Is(err, repository.ErrParseAttemptNotRunnable) {
		return nil
	}
	return err
}
