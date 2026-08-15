package app

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/config"
	"webtag/internal/observability"
)

type contentHistoryRetentionWiringProcessor struct{}

func (contentHistoryRetentionWiringProcessor) Run(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func (contentHistoryRetentionWiringProcessor) RecordDiscard(context.Context, uuid.UUID, uuid.UUID, error) error {
	return nil
}

func TestBuildRiverQueueOptionsAlwaysWiresContentHistoryRetention(t *testing.T) {
	t.Parallel()

	metrics := observability.NewMetrics()
	layer := &persistenceLayer{pool: &pgxpool.Pool{}, metrics: metrics}
	options := buildRiverQueueOptions(
		config.Config{DB: config.DBConfig{ParseConcurrency: 1}},
		layer,
		contentHistoryRetentionWiringProcessor{},
		nil,
	)
	if options.ContentHistoryRetention == nil {
		t.Fatal("production River options omitted content history retention repository")
	}
	if options.Metrics != metrics {
		t.Fatal("production River options did not share runtime metrics with retention worker")
	}
}
