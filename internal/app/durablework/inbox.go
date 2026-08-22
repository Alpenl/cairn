package durablework

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/service"
)

// InboxQueue is the infrastructure-only River port for proposal work. The
// transaction belongs to InboxCommands so services cannot accidentally split
// product state from queue state.
type InboxQueue interface {
	EnqueueReaderInboxSummaryTx(context.Context, pgx.Tx, service.ReaderInboxSummaryJobArgs) error
}

type inboxRepository interface {
	CreateInboxTx(context.Context, pgx.Tx, model.ReaderInbox) (*model.ReaderInbox, error)
	StartInboxProposalTx(context.Context, pgx.Tx, uuid.UUID, int64) (*model.ReaderInbox, error)
}

// InboxCommands atomically owns an Inbox proposal state transition and its
// River row. River is the only job-state store; the Inbox row contains only
// the product-facing proposal status.
type InboxCommands struct {
	transactions database.TxBeginner
	inbox        inboxRepository
	queue        InboxQueue
}

func NewInboxCommands(transactions database.TxBeginner, inbox inboxRepository, queue InboxQueue) *InboxCommands {
	if transactions == nil || inbox == nil || queue == nil {
		panic("durablework.NewInboxCommands: transactions, inbox, and queue are required")
	}
	return &InboxCommands{
		transactions: transactions,
		inbox:        inbox,
		queue:        queue,
	}
}

func (c *InboxCommands) CreateInboxProposal(ctx context.Context, command service.CreateInboxProposalCommand) (service.InboxProposalResult, error) {
	var result service.InboxProposalResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		item, err := c.inbox.CreateInboxTx(ctx, tx, command.Inbox)
		if err != nil {
			return err
		}
		if item == nil {
			return errors.New("create Inbox proposal: repository returned nil item")
		}
		item, err = c.startTx(ctx, tx, item.ID, item.MetadataRevision)
		if err != nil {
			return err
		}
		result = service.InboxProposalResult{Inbox: item}
		return nil
	})
	if err != nil {
		return service.InboxProposalResult{}, fmt.Errorf("create Inbox proposal: %w", err)
	}
	return result, nil
}

func (c *InboxCommands) EnsureInboxProposal(ctx context.Context, command service.EnsureInboxProposalCommand) (service.InboxProposalResult, error) {
	if command.InboxID == uuid.Nil {
		return service.InboxProposalResult{}, errors.New("ensure Inbox proposal: nil inbox id")
	}
	var result service.InboxProposalResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		item, err := c.startTx(ctx, tx, command.InboxID, command.ExpectedMetadataRevision)
		if err != nil {
			return err
		}
		result.Inbox = item
		return nil
	})
	if err != nil {
		return service.InboxProposalResult{}, fmt.Errorf("ensure Inbox proposal: %w", err)
	}
	return result, nil
}

func (c *InboxCommands) startTx(ctx context.Context, tx pgx.Tx, inboxID uuid.UUID, expectedRevision int64) (*model.ReaderInbox, error) {
	item, err := c.inbox.StartInboxProposalTx(ctx, tx, inboxID, expectedRevision)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, errors.New("start Inbox proposal: repository returned nil item")
	}
	args := service.ReaderInboxSummaryJobArgs{
		InboxID:                  item.ID,
		ExpectedMetadataRevision: item.MetadataRevision,
	}
	if err := c.queue.EnqueueReaderInboxSummaryTx(ctx, tx, args); err != nil {
		return nil, err
	}
	return item, nil
}

var _ service.InboxProposalCommands = (*InboxCommands)(nil)
