package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

type FeedClaimStore interface {
	ClaimDue(context.Context, int, time.Duration) ([]model.FeedSubscription, error)
}

type FeedClaimRefresher interface {
	RefreshClaimed(context.Context, model.FeedSubscription) error
}

type FeedSchedulerOptions struct {
	Claims      FeedClaimStore
	Refresher   FeedClaimRefresher
	PollEvery   time.Duration
	Lease       time.Duration
	BatchSize   int
	Concurrency int
	Logger      *slog.Logger
}

// FeedScheduler polls a durable due-time index. Claiming happens before any
// network access, with a lease that another process can recover after expiry.
type FeedScheduler struct {
	claims      FeedClaimStore
	refresher   FeedClaimRefresher
	pollEvery   time.Duration
	lease       time.Duration
	batchSize   int
	concurrency int
	logger      *slog.Logger

	lifecycle backgroundLoop
}

func NewFeedScheduler(options FeedSchedulerOptions) *FeedScheduler {
	if options.PollEvery <= 0 {
		options.PollEvery = 5 * time.Second
	}
	if options.Lease <= 0 {
		options.Lease = 2 * time.Minute
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 16
	}
	if options.Concurrency <= 0 {
		options.Concurrency = 4
	}
	return &FeedScheduler{
		claims: options.Claims, refresher: options.Refresher,
		pollEvery: options.PollEvery, lease: options.Lease,
		batchSize: options.BatchSize, concurrency: options.Concurrency,
		logger:    options.Logger,
		lifecycle: newBackgroundLoop("feed_scheduler", options.Logger),
	}
}

func (s *FeedScheduler) Start(parent context.Context) error {
	if s == nil || s.claims == nil || s.refresher == nil {
		return errors.New("feed scheduler is not configured")
	}
	return s.lifecycle.Start(parent, s.loop)
}

func (s *FeedScheduler) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.lifecycle.Stop(ctx)
}

func (s *FeedScheduler) loop(ctx context.Context) {
	ticker := time.NewTicker(s.pollEvery)
	defer ticker.Stop()
	for {
		s.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *FeedScheduler) runOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	claimed, err := s.claims.ClaimDue(ctx, s.batchSize, s.lease)
	if err != nil {
		if ctx.Err() == nil && s.logger != nil {
			s.logger.Warn("claim due feeds failed", "error", observability.SafeError(err))
		}
		return
	}
	if len(claimed) == 0 {
		return
	}
	gate := make(chan struct{}, s.concurrency)
	var refreshes sync.WaitGroup
claimLoop:
	for _, subscription := range claimed {
		if ctx.Err() != nil {
			break
		}
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			break claimLoop
		}
		refreshes.Add(1)
		go func(subscription model.FeedSubscription) {
			defer refreshes.Done()
			defer func() { <-gate }()
			if err := s.refresher.RefreshClaimed(ctx, subscription); err != nil &&
				!errors.Is(err, context.Canceled) &&
				!errors.Is(err, repository.ErrFeedRefreshClaimLost) && s.logger != nil {
				s.logger.Warn("complete claimed feed refresh failed",
					"subscription_id", subscription.ID,
					"error", observability.SafeError(err))
			}
		}(subscription)
	}
	refreshes.Wait()
}
