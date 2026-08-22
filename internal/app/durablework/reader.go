package durablework

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

// ReaderLinkQueue is the River surface needed by Reader commands that restore
// or trash Links. Keeping it here prevents persistence adapters from retaining
// infrastructure dependencies.
type ReaderLinkQueue interface {
	EnqueueTx(context.Context, pgx.Tx, model.ParseAttempt) error
	CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error
}

// ReaderCommands owns every Reader transaction that can change durable Link
// work. Feature applications see five narrow command interfaces; this
// concrete adapter is shared only at the composition root.
type ReaderCommands struct {
	transactions database.TxBeginner
	reader       *repository.PGXReaderVNextRepository
	queue        ReaderLinkQueue
}

func NewReaderCommands(
	transactions database.TxBeginner,
	reader *repository.PGXReaderVNextRepository,
	queue ReaderLinkQueue,
) *ReaderCommands {
	if transactions == nil || reader == nil || queue == nil {
		panic("durablework.NewReaderCommands: transactions, reader, and queue are required")
	}
	return &ReaderCommands{transactions: transactions, reader: reader, queue: queue}
}

func (c *ReaderCommands) lifecycle(ctx context.Context, tx pgx.Tx, change repository.ReaderLinkLifecycleChange) error {
	if err := c.queue.CancelAllActiveTx(ctx, tx, change.LinkID); err != nil {
		return fmt.Errorf("cancel Reader Link work: %w", err)
	}
	if change.ParseAttempt == nil {
		return nil
	}
	if err := c.queue.EnqueueTx(ctx, tx, *change.ParseAttempt); err != nil {
		return fmt.Errorf("enqueue Reader Link parse attempt: %w", err)
	}
	return nil
}

func (c *ReaderCommands) RestoreHost(
	ctx context.Context,
	kind model.ReaderHostKind,
	id uuid.UUID,
) (model.ReaderHostLifecycleResult, error) {
	var result model.ReaderHostLifecycleResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		result, err = c.reader.RestoreHostTx(ctx, tx, kind, id, c.lifecycle)
		return err
	})
	return result, err
}

func (c *ReaderCommands) FeedbackFeed(
	ctx context.Context,
	itemKey string,
	action string,
) (model.ReaderFeedFeedback, error) {
	var result model.ReaderFeedFeedback
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		result, err = c.reader.FeedbackFeedTx(ctx, tx, itemKey, action, c.lifecycle)
		return err
	})
	return result, err
}

func (c *ReaderCommands) ConfirmInbox(
	ctx context.Context,
	id uuid.UUID,
	expectedRevision *int64,
) (uuid.UUID, error) {
	var linkID uuid.UUID
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		linkID, err = c.reader.ConfirmInboxTx(ctx, tx, id, expectedRevision, c.lifecycle)
		return err
	})
	return linkID, err
}

func (c *ReaderCommands) BulkConfirmInbox(
	ctx context.Context,
	confirmations []model.ReaderInboxBulkConfirmation,
) ([]model.ReaderInboxBulkResult, error) {
	var results []model.ReaderInboxBulkResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		results, err = c.reader.BulkConfirmInboxTx(ctx, tx, confirmations, c.lifecycle)
		return err
	})
	return results, err
}

func (c *ReaderCommands) ConfirmAIProposals(
	ctx context.Context,
	partition model.ReaderInboxPartition,
) (model.ReaderInboxAIProposalConfirmation, error) {
	var result model.ReaderInboxAIProposalConfirmation
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		result, err = c.reader.ConfirmAIProposalsTx(ctx, tx, partition, c.lifecycle)
		return err
	})
	return result, err
}

var (
	_ service.ReaderInboxConfirmCommands     = (*ReaderCommands)(nil)
	_ service.ReaderInboxBulkConfirmCommands = (*ReaderCommands)(nil)
	_ service.ReaderInboxAIConfirmCommands   = (*ReaderCommands)(nil)
	_ service.ReaderFeedFeedbackCommands     = (*ReaderCommands)(nil)
	_ service.ReaderHostRestoreCommands      = (*ReaderCommands)(nil)
)
