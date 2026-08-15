package service

import (
	"context"
	"log/slog"

	"webtag/internal/errsafe"
	"webtag/internal/observability"
)

func (p *ParsePipeline) recordFailure(stage string, err error) {
	if p.metrics == nil {
		return
	}
	p.metrics.ParseFailuresTotal.WithLabelValues(normalizeMetricLabel(stage), errsafe.ClassifyError(err)).Inc()
}

func (p *ParsePipeline) recordFailureRun(stage string, err error, fetcherType, contentType string) {
	p.recordFailure(stage, err)
	p.recordRun("failed", fetcherType, contentType)
}

// 两个 recordRun 都直接委托给 recordParseRun，由后者统一判空——调用侧不再
// 重复判断（此前 ParsePipeline 那个多判了一次，是重构残留）。
func (p *ParsePipeline) recordRun(result, fetcherType, contentType string) {
	recordParseRun(p.metrics, result, fetcherType, contentType)
}

func (f collectionFinalizer) recordRun(result, fetcherType, contentType string) {
	recordParseRun(f.metrics, result, fetcherType, contentType)
}

func recordParseRun(metrics *observability.Metrics, result, fetcherType, contentType string) {
	if metrics == nil {
		return
	}

	normalizedFetcherType := normalizeMetricLabel(fetcherType)
	normalizedContentType := normalizeMetricLabel(contentType)
	metrics.ParseRunsTotal.WithLabelValues(normalizeMetricLabel(result), normalizedFetcherType, normalizedContentType).Inc()
}

// recordFetchEscalation increments webtag_fetch_escalation_total for a single
// light→deep escalation. outcome is "recovered" (deep result no longer thin)
// or "still_thin". nil-metrics-safe so escalation works in fixtures without an
// observability wiring.
func (p *ParsePipeline) recordFetchEscalation(outcome string) {
	if p.metrics == nil {
		return
	}
	p.metrics.FetchEscalationTotal.WithLabelValues(normalizeMetricLabel(outcome)).Inc()
}

func (f collectionFinalizer) recordLowConfidence(fetcherType string, reason string) {
	recordParseLowConfidence(f.metrics, fetcherType, reason)
}

func recordParseLowConfidence(metrics *observability.Metrics, fetcherType string, reason string) {
	if metrics == nil {
		return
	}

	normalizedFetcherType := normalizeMetricLabel(fetcherType)
	metrics.ParseLowConfidenceTotal.WithLabelValues(
		normalizeMetricLabel(reason),
		normalizedFetcherType,
	).Inc()
}

func (p *ParsePipeline) logInfo(ctx context.Context, message string, args ...any) {
	logPipelineInfo(ctx, p.logger, message, args...)
}

func (f collectionFinalizer) logInfo(ctx context.Context, message string, args ...any) {
	logPipelineInfo(ctx, f.logger, message, args...)
}

func (p *ParsePipeline) logWarn(ctx context.Context, message string, args ...any) {
	logPipelineWarn(ctx, p.logger, message, args...)
}

func (f collectionFinalizer) logWarn(ctx context.Context, message string, args ...any) {
	logPipelineWarn(ctx, f.logger, message, args...)
}

func logPipelineInfo(ctx context.Context, logger *slog.Logger, message string, args ...any) {
	if contextual := observability.FromContext(ctx); contextual != nil {
		logger = contextual
	}
	if logger != nil {
		logger.Info(message, args...)
	}
}

func logPipelineWarn(ctx context.Context, logger *slog.Logger, message string, args ...any) {
	if contextual := observability.FromContext(ctx); contextual != nil {
		logger = contextual
	}
	if logger != nil {
		logger.Warn(message, args...)
	}
}

func (p *ParsePipeline) logError(ctx context.Context, message string, args ...any) {
	logger := p.logger
	if contextual := observability.FromContext(ctx); contextual != nil {
		logger = contextual
	}
	if logger != nil {
		logger.Error(message, args...)
	}
}

// 落地后补充步骤（enrichment）的观测标签。这些步骤在链接已写成 done 之后运行，
// 失败不影响解析契约，因此全部 fail-soft——代价是缺口不可见：没有对账 worker，
// 缺失的向量或概念边会一直缺到该链接被重新解析为止。
//
// 每条不产出结果的路径都记一次，让缺口可数。
const (
	enrichmentContentEmbedding = "content_embedding"
	enrichmentConceptAttach    = "concept_attach"

	enrichmentNotWired   = "not_wired"   // 能力未装配（预期内，用于确认灰度状态）
	enrichmentDisabled   = "disabled"    // 已装配但运行时关闭
	enrichmentEmptyInput = "empty_input" // 无可用输入，跳过属正常
	enrichmentFailed     = "failed"      // 调用或写入出错——真正的数据缺口
)

// recordEnrichmentSkipped 记录一次未完成的补充步骤。
// 告警应盯 reason=failed：它等于「有多少链接永久缺了这项数据」。
func (f collectionFinalizer) recordEnrichmentSkipped(kind, reason string) {
	// 同时检查子字段：与 recordLibraryClassificationMetric / recordPurge 的写法
	// 保持一致。observability.Metrics 可被部分构造（如 database/tracer_test.go
	// 的 &observability.Metrics{}），届时子字段为 nil。
	if f.metrics == nil || f.metrics.ParseEnrichmentSkippedTotal == nil {
		return
	}
	f.metrics.ParseEnrichmentSkippedTotal.WithLabelValues(kind, reason).Inc()
}
