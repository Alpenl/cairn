package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"webtag/internal/repository"
)

// SiteEmbeddingBackfiller repairs missing or stale profile vectors. It is
// intentionally explicit rather than part of an aggregation transaction: an
// embedding outage must never make a user-facing site write fail or hold DB
// locks while making a network request.
type SiteEmbeddingBackfiller struct {
	store         repository.SiteEmbeddingStore
	embedder      RetrievalEmbedder
	batchSize     int
	batchInterval time.Duration
	logger        *slog.Logger
}

type SiteEmbeddingBackfillOptions struct {
	Store         repository.SiteEmbeddingStore
	Embedder      RetrievalEmbedder
	BatchSize     int
	BatchInterval time.Duration
	Logger        *slog.Logger
}

func NewSiteEmbeddingBackfiller(opts SiteEmbeddingBackfillOptions) *SiteEmbeddingBackfiller {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 64
	}
	if opts.BatchInterval <= 0 {
		opts.BatchInterval = 500 * time.Millisecond
	}
	return &SiteEmbeddingBackfiller{store: opts.Store, embedder: opts.Embedder, batchSize: opts.BatchSize, batchInterval: opts.BatchInterval, logger: opts.Logger}
}

func (b *SiteEmbeddingBackfiller) Run(ctx context.Context) (filled, failed int, err error) { //nolint:gocyclo // 批处理主循环：分页、跳过、失败重试、终止条件各占一支
	if b == nil || b.store == nil || b.embedder == nil || !b.embedder.Enabled() {
		return 0, 0, nil
	}
	model, cursor := b.embedder.Model(), uuid.Nil
	for {
		batch, scanErr := b.store.ListSitesNeedingEmbedding(ctx, model, cursor, b.batchSize)
		if scanErr != nil {
			return filled, failed, scanErr
		}
		if len(batch) == 0 {
			return filled, failed, nil
		}
		cursor = batch[len(batch)-1].ID
		inputs, candidates := make([]string, 0, len(batch)), make([]repository.SiteEmbeddingCandidate, 0, len(batch))
		for _, candidate := range batch {
			input := BuildSiteEmbeddingInput(siteEmbeddingDocument(candidate))
			if input == "" {
				failed++
				continue
			}
			inputs, candidates = append(inputs, input), append(candidates, candidate)
		}
		if len(inputs) > 0 {
			vectors, embedErr := b.embedder.Embed(ctx, inputs)
			if embedErr != nil || len(vectors) != len(candidates) {
				failed += len(candidates)
			} else {
				for i, vector := range vectors {
					if len(vector) == 0 {
						failed++
						continue
					}
					updated, updateErr := b.store.UpdateSiteEmbedding(ctx, candidates[i].ID, candidates[i].Revision, vector, model)
					if updateErr != nil {
						failed++
						continue
					}
					if updated {
						filled++
					}
				}
			}
		}
		if len(batch) < b.batchSize {
			return filled, failed, nil
		}
		select {
		case <-ctx.Done():
			return filled, failed, ctx.Err()
		case <-time.After(b.batchInterval):
		}
	}
}

func siteEmbeddingDocument(candidate repository.SiteEmbeddingCandidate) SiteEmbeddingDocument {
	document := SiteEmbeddingDocument{Name: candidate.Name, Intro: candidate.Intro, DisplayHost: candidate.DisplayHost, Tags: candidate.Tags, Entries: make([]SiteEmbeddingEntry, 0, len(candidate.Entries))}
	for _, entry := range candidate.Entries {
		document.Entries = append(document.Entries, SiteEmbeddingEntry{Name: entry.Name, Purpose: entry.Purpose})
	}
	return document
}
