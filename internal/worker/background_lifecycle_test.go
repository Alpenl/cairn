package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/service"
)

type terminalLoggingWorker interface {
	Start(context.Context) error
	Stop(context.Context) error
}

func TestBackgroundWorkersLogComponentAndExitReason(t *testing.T) {
	tests := []struct {
		name      string
		component string
		newWorker func(*slog.Logger) (terminalLoggingWorker, error)
	}{
		{
			name:      "feed scheduler",
			component: "feed_scheduler",
			newWorker: func(logger *slog.Logger) (terminalLoggingWorker, error) {
				return NewFeedScheduler(FeedSchedulerOptions{
					Claims:    &feedClaimStoreStub{},
					Refresher: &feedClaimRefresherStub{called: make(chan struct{}, 1)},
					PollEvery: time.Hour,
					Logger:    logger,
				}), nil
			},
		},
		{
			name:      "site payload cleaner",
			component: "site_payload_cleaner",
			newWorker: func(logger *slog.Logger) (terminalLoggingWorker, error) {
				return newSitePayloadCleaner(
					&sitePayloadCleanupStoreStub{purged: make(map[uuid.UUID]bool)},
					time.Hour,
					1,
					logger,
				), nil
			},
		},
		{
			name:      "site embedding backfill",
			component: "site_embedding_backfill",
			newWorker: func(logger *slog.Logger) (terminalLoggingWorker, error) {
				return NewSiteEmbeddingBackfillWorker(SiteEmbeddingBackfillWorkerOptions{
					Runner: &siteEmbeddingBackfillRunnerStub{}, Interval: time.Hour, Logger: logger,
				})
			},
		},
		{
			name:      "historical migration",
			component: "historical_migration",
			newWorker: func(logger *slog.Logger) (terminalLoggingWorker, error) {
				return NewHistoricalMigrationWorker(HistoricalMigrationWorkerOptions{
					Runner:   service.NewHistoricalMigrationRunner(&migrationWorkerStoreFake{}),
					Interval: time.Hour,
					Logger:   logger,
				})
			},
		},
		{
			name:      "parse terminal reconciler",
			component: "parse_terminal_reconciler",
			newWorker: func(logger *slog.Logger) (terminalLoggingWorker, error) {
				return newParseTerminalReconciler(
					&fakeParseTerminalStore{active: make(map[int64]bool)},
					&fakeParseTerminalProjector{},
					time.Hour,
					10,
					logger,
				), nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			worker, err := tt.newWorker(logger)
			if err != nil {
				t.Fatalf("construct worker: %v", err)
			}

			runCtx, cancelRun := context.WithCancelCause(context.Background())
			if err := worker.Start(runCtx); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			exitCause := errors.New("test-requested worker exit")
			cancelRun(exitCause)
			stopCtx, cancelStop := context.WithTimeout(context.Background(), time.Second)
			defer cancelStop()
			if err := worker.Stop(stopCtx); err != nil {
				t.Fatalf("Stop() error = %v", err)
			}

			var entry struct {
				Message   string `json:"msg"`
				Component string `json:"component"`
				Reason    struct {
					Chain string `json:"chain"`
				} `json:"reason"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
				t.Fatalf("decode terminal log %q: %v", logs.String(), err)
			}
			if entry.Message != "background worker exited" {
				t.Fatalf("terminal log message = %q, want %q", entry.Message, "background worker exited")
			}
			if entry.Component != tt.component {
				t.Fatalf("terminal log component = %q, want %q", entry.Component, tt.component)
			}
			if entry.Reason.Chain != exitCause.Error() {
				t.Fatalf("terminal log reason chain = %q, want %q", entry.Reason.Chain, exitCause)
			}
		})
	}
}
