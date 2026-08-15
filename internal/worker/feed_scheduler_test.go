package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
)

type feedClaimStoreStub struct {
	claimed []model.FeedSubscription
}

func (s *feedClaimStoreStub) ClaimDue(context.Context, int, time.Duration) ([]model.FeedSubscription, error) {
	return s.claimed, nil
}

type feedClaimRefresherStub struct {
	called chan struct{}
}

type blockingFeedClaimStore struct {
	called  chan struct{}
	release chan struct{}
}

func (s *blockingFeedClaimStore) ClaimDue(context.Context, int, time.Duration) ([]model.FeedSubscription, error) {
	select {
	case s.called <- struct{}{}:
	default:
	}
	<-s.release
	return nil, nil
}

type cancelAwareFeedRefresher struct {
	started chan struct{}
}

func (s *cancelAwareFeedRefresher) RefreshClaimed(ctx context.Context, _ model.FeedSubscription) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *feedClaimRefresherStub) RefreshClaimed(ctx context.Context, _ model.FeedSubscription) error {
	select {
	case s.called <- struct{}{}:
	default:
	}
	return nil
}

func TestFeedSchedulerRefreshesClaimedSubscription(t *testing.T) {
	t.Parallel()
	refresher := &feedClaimRefresherStub{called: make(chan struct{}, 1)}
	scheduler := NewFeedScheduler(FeedSchedulerOptions{
		Claims:    &feedClaimStoreStub{claimed: []model.FeedSubscription{{ID: uuid.New()}}},
		Refresher: refresher,
	})
	scheduler.runOnce(context.Background())
	select {
	case <-refresher.called:
	default:
		t.Fatal("refresher was not called")
	}
}

func TestFeedSchedulerStopCancelsLoop(t *testing.T) {
	t.Parallel()
	scheduler := NewFeedScheduler(FeedSchedulerOptions{
		Claims:    &feedClaimStoreStub{},
		Refresher: &feedClaimRefresherStub{called: make(chan struct{}, 1)},
		PollEvery: time.Hour,
	})
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scheduler.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestFeedSchedulerStopCancelsWhileConcurrencyGateIsFull(t *testing.T) {
	t.Parallel()
	claimed := []model.FeedSubscription{
		{ID: uuid.New()},
		{ID: uuid.New()},
	}
	refresher := &cancelAwareFeedRefresher{started: make(chan struct{}, 1)}
	scheduler := NewFeedScheduler(FeedSchedulerOptions{
		Claims: &feedClaimStoreStub{claimed: claimed}, Refresher: refresher,
		Concurrency: 1, PollEvery: time.Hour,
	})
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-refresher.started:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := scheduler.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestFeedSchedulerConstructorAndLifecycleState(t *testing.T) {
	t.Parallel()

	claims := &blockingFeedClaimStore{called: make(chan struct{}, 1), release: make(chan struct{})}
	scheduler := NewFeedScheduler(FeedSchedulerOptions{
		Claims: claims, Refresher: &feedClaimRefresherStub{called: make(chan struct{}, 1)}, PollEvery: time.Hour,
	})
	select {
	case <-claims.called:
		t.Fatal("NewFeedScheduler started polling before Start")
	case <-time.After(30 * time.Millisecond):
	}
	if err := scheduler.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-claims.called:
	case <-time.After(time.Second):
		t.Fatal("Start() did not begin the scheduler loop")
	}
	if err := scheduler.Start(context.Background()); !errors.Is(err, ErrBackgroundAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrBackgroundAlreadyStarted", err)
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelStop()
	if err := scheduler.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want context deadline exceeded", err)
	}
	close(claims.release)
	if err := scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if err := scheduler.Start(context.Background()); !errors.Is(err, ErrBackgroundStopped) {
		t.Fatalf("Start() after Stop error = %v, want ErrBackgroundStopped", err)
	}
}

func TestFeedSchedulerStopBeforeStartIsIdempotent(t *testing.T) {
	t.Parallel()

	scheduler := NewFeedScheduler(FeedSchedulerOptions{
		Claims: &feedClaimStoreStub{}, Refresher: &feedClaimRefresherStub{called: make(chan struct{}, 1)},
	})
	if err := scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() before Start error = %v", err)
	}
	if err := scheduler.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if err := scheduler.Start(context.Background()); !errors.Is(err, ErrBackgroundStopped) {
		t.Fatalf("Start() after Stop-before-Start error = %v, want ErrBackgroundStopped", err)
	}
}
