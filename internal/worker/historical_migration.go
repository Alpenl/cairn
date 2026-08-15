package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"webtag/internal/observability"
	"webtag/internal/service"
)

const (
	defaultHistoricalMigrationInterval = 15 * time.Minute
	defaultHistoricalMigrationTimeout  = 2 * time.Minute
)

// HistoricalMigrationWorker owns scheduling only. The service owns cursor
// traversal and every data mutation, so RunOnce is also suitable for a
// controlled installation-wide admin invocation before automatic scheduling
// is on.
type HistoricalMigrationWorker struct {
	runner   *service.HistoricalMigrationRunner
	options  service.HistoricalMigrationRunOptions
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger

	lifecycle backgroundLoop
}

type HistoricalMigrationWorkerOptions struct {
	Runner    *service.HistoricalMigrationRunner
	BatchSize int
	DryRun    bool
	Interval  time.Duration
	Timeout   time.Duration
	Logger    *slog.Logger
}

func NewHistoricalMigrationWorker(options HistoricalMigrationWorkerOptions) (*HistoricalMigrationWorker, error) {
	if options.Runner == nil {
		return nil, errors.New("historical migration worker: runner is required")
	}
	if options.Interval <= 0 {
		options.Interval = defaultHistoricalMigrationInterval
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultHistoricalMigrationTimeout
	}
	return &HistoricalMigrationWorker{
		runner: options.Runner, options: service.HistoricalMigrationRunOptions{BatchSize: options.BatchSize, DryRun: options.DryRun},
		interval: options.Interval, timeout: options.Timeout, logger: options.Logger,
		lifecycle: newBackgroundLoop("historical_migration", options.Logger),
	}, nil
}

func (w *HistoricalMigrationWorker) Start(ctx context.Context) error {
	if w == nil || w.runner == nil {
		return errors.New("historical migration worker is not configured")
	}
	return w.lifecycle.Start(ctx, w.loop)
}

func (w *HistoricalMigrationWorker) loop(ctx context.Context) {
	w.runIteration(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runIteration(ctx)
		}
	}
}

func (w *HistoricalMigrationWorker) runIteration(ctx context.Context) {
	iterationCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	report, err := w.RunOnce(iterationCtx)
	if w.logger == nil {
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		w.logger.Warn("historical migration incomplete",
			"scanned", report.Scanned,
			"auto_migrated", report.AutoMigrated,
			"suggested", report.Suggested,
			"retained", report.Retained,
			"would_auto_migrate", report.WouldAutoMigrate,
			"would_suggest", report.WouldSuggest,
			"would_retain", report.WouldRetain,
			"skipped", report.Skipped,
			"error", observability.SafeError(err))
		return
	}
	if err != nil {
		return // context.Canceled：正常停机，不值得一条日志
	}
	// 成功路径也必须发声。CAS 冲突走的是「跳过并继续」，整轮因此**成功**返回，
	// 只有这里能报出来——挂在上面那条 err 分支下的话，一轮「候选全被跳过、迁移
	// 一条没推进」的 run 会安静得和什么都没发生一样。
	if report.Skipped > 0 {
		w.logger.Warn("historical migration skipped candidates",
			"scanned", report.Scanned,
			"auto_migrated", report.AutoMigrated,
			"suggested", report.Suggested,
			"retained", report.Retained,
			"would_auto_migrate", report.WouldAutoMigrate,
			"would_suggest", report.WouldSuggest,
			"would_retain", report.WouldRetain,
			"skipped", report.Skipped)
		return
	}
	// 无事可做的那些轮次（绝大多数）保持安静：每 15 分钟一条 scanned=0 的
	// Info 只会淹掉上面两条真正需要被看见的。
	if report.Scanned > 0 {
		w.logger.Info("historical migration completed",
			"scanned", report.Scanned,
			"auto_migrated", report.AutoMigrated,
			"suggested", report.Suggested,
			"retained", report.Retained,
			"would_auto_migrate", report.WouldAutoMigrate,
			"would_suggest", report.WouldSuggest,
			"would_retain", report.WouldRetain)
	}
}

func (w *HistoricalMigrationWorker) RunOnce(ctx context.Context) (service.HistoricalMigrationReport, error) {
	if w == nil || w.runner == nil {
		return service.HistoricalMigrationReport{}, errors.New("historical migration worker is not configured")
	}
	return w.runner.Run(ctx, w.options)
}

func (w *HistoricalMigrationWorker) Stop(ctx context.Context) error {
	if w == nil {
		return nil
	}
	return w.lifecycle.Stop(ctx)
}
