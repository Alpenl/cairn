package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/database"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

const (
	defaultSitePayloadCleanupInterval = 5 * time.Minute
	defaultSitePayloadCleanupBatch    = 100
	defaultSitePayloadCleanupTimeout  = 30 * time.Second
)

const listExpiredSitePayloadsSQL = `
SELECT id
FROM links
WHERE payload_purge_due_at <= NOW()
  AND payload_purged_at IS NULL
ORDER BY payload_purge_due_at ASC, id ASC
LIMIT $1`

// 守卫在这里被刻意重新校验一次——此时 UPDATE 持有行锁，因此一次陈旧的候选查询
// 永远无法清掉一条被并发解析刚刚判定为 reading 的链接。
//
// 重复的是校验动作；条件本身取自 repository.SQLNotReadingPredicate，与解析失败
// 路径共用同一个定义，避免两处措辞漂移。
const purgeExpiredSitePayloadSQL = `
UPDATE links
SET input_text = NULL,
    input_html = NULL,
    input_images = NULL,
    source_metadata = NULL,
    payload_purge_due_at = NULL,
    payload_purged_at = NOW(),
    updated_at = NOW()
WHERE id = $1
  AND payload_purge_due_at <= NOW()
  AND payload_purged_at IS NULL
  AND ` + repository.SQLNotReadingPredicate

type sitePayloadCandidate struct {
	linkID uuid.UUID
}

type sitePayloadCleanupStore interface {
	ListExpired(context.Context, int) ([]sitePayloadCandidate, error)
	PurgeExpired(context.Context, uuid.UUID) (bool, error)
}

type sitePayloadCleanupDB interface {
	database.TxBeginner
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type pgSitePayloadCleanupStore struct {
	db sitePayloadCleanupDB
}

func (s *pgSitePayloadCleanupStore) ListExpired(ctx context.Context, limit int) ([]sitePayloadCandidate, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin site payload discovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, listExpiredSitePayloadsSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired site payloads: %w", err)
	}
	defer rows.Close()

	candidates := make([]sitePayloadCandidate, 0)
	for rows.Next() {
		var candidate sitePayloadCandidate
		if err := rows.Scan(&candidate.linkID); err != nil {
			return nil, fmt.Errorf("scan expired site payload: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired site payloads: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit site payload discovery: %w", err)
	}
	return candidates, nil
}

func (s *pgSitePayloadCleanupStore) PurgeExpired(ctx context.Context, linkID uuid.UUID) (bool, error) {
	var purged bool
	err := database.WithTx(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, purgeExpiredSitePayloadSQL, linkID)
		if err != nil {
			return fmt.Errorf("purge expired site payload: %w", err)
		}
		purged = tag.RowsAffected() == 1
		return nil
	})
	return purged, err
}

// SitePayloadCleaner compensates for process crashes and failed cleanup after
// the 24-hour retention deadline. It handles no page body in Go and only logs
// aggregate counts, never captured material.
type SitePayloadCleaner struct {
	store    sitePayloadCleanupStore
	interval time.Duration
	batch    int
	timeout  time.Duration
	logger   *slog.Logger

	lifecycle backgroundLoop
}

type SitePayloadCleanerOptions struct {
	Pool     *pgxpool.Pool
	Interval time.Duration
	Batch    int
	Logger   *slog.Logger
}

func NewSitePayloadCleaner(opts SitePayloadCleanerOptions) (*SitePayloadCleaner, error) {
	if opts.Pool == nil {
		return nil, errors.New("site payload cleaner: pool is required")
	}
	return newSitePayloadCleaner(&pgSitePayloadCleanupStore{db: opts.Pool}, opts.Interval, opts.Batch, opts.Logger), nil
}

func newSitePayloadCleaner(store sitePayloadCleanupStore, interval time.Duration, batch int, logger *slog.Logger) *SitePayloadCleaner {
	if interval <= 0 {
		interval = defaultSitePayloadCleanupInterval
	}
	if batch <= 0 {
		batch = defaultSitePayloadCleanupBatch
	}
	return &SitePayloadCleaner{
		store: store, interval: interval, batch: batch, timeout: defaultSitePayloadCleanupTimeout,
		logger: logger, lifecycle: newBackgroundLoop("site_payload_cleaner", logger),
	}
}

func (c *SitePayloadCleaner) Start(ctx context.Context) error {
	if c == nil || c.store == nil {
		return errors.New("site payload cleaner is not configured")
	}
	return c.lifecycle.Start(ctx, c.loop)
}

func (c *SitePayloadCleaner) loop(ctx context.Context) {
	c.runIteration(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runIteration(ctx)
		}
	}
}

func (c *SitePayloadCleaner) runIteration(ctx context.Context) {
	iterationCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	purged, err := c.RunOnce(iterationCtx)
	if err != nil && !errors.Is(err, context.Canceled) && c.logger != nil {
		c.logger.Warn("site payload cleanup incomplete", "purged", purged, "error", observability.SafeError(err))
	}
}

func (c *SitePayloadCleaner) RunOnce(ctx context.Context) (int, error) {
	if c == nil || c.store == nil {
		return 0, errors.New("site payload cleaner is not configured")
	}
	candidates, err := c.store.ListExpired(ctx, c.batch)
	if err != nil {
		return 0, err
	}
	purged := 0
	for _, candidate := range candidates {
		didPurge, err := c.store.PurgeExpired(ctx, candidate.linkID)
		if err != nil {
			return purged, err
		}
		if didPurge {
			purged++
		}
	}
	return purged, nil
}

func (c *SitePayloadCleaner) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.lifecycle.Stop(ctx)
}
