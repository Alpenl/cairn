package service

import (
	"context"
	"log/slog"

	"webtag/internal/observability"
)

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
