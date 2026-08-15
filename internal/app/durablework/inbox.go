package durablework

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

const MaxInboxProposalOrphanRepairBatchSize = 100

// InboxQueue is the infrastructure-only River port for proposal work. The
// transaction belongs to InboxCommands so services cannot accidentally split
// product state from queue state again.
type InboxQueue interface {
	EnqueueReaderInboxSummaryTx(context.Context, pgx.Tx, service.ReaderInboxSummaryJobArgs) error
}

type inboxRepository interface {
	CreateInboxTx(context.Context, pgx.Tx, model.ReaderInbox) (*model.ReaderInbox, error)
	BeginInboxResummarizeJobTx(context.Context, pgx.Tx, uuid.UUID, int64) (*model.ReaderInboxJob, bool, error)
	ClaimInboxDispatchOrphansTx(context.Context, pgx.Tx, string, int) ([]repository.ReaderInboxDispatchOrphan, error)
	ResetInboxDispatchOrphanTx(context.Context, pgx.Tx, uuid.UUID) error
}

type InboxCommandsOptions struct {
	Transactions database.TxBeginner
	Inbox        inboxRepository
	Queue        InboxQueue
}

// InboxCommands atomically owns an Inbox proposal attempt and its River row.
// EnsureInboxProposal deliberately calls InsertTx for both new and reused
// attempts: a legacy queued orphan therefore converges on replay, while
// River's active-state uniqueness prevents duplicate analysis.
type InboxCommands struct {
	transactions database.TxBeginner
	inbox        inboxRepository
	queue        InboxQueue
}

func NewInboxCommands(options InboxCommandsOptions) *InboxCommands {
	return &InboxCommands{
		transactions: options.Transactions,
		inbox:        options.Inbox,
		queue:        options.Queue,
	}
}

func (c *InboxCommands) CreateInboxProposal(ctx context.Context, command service.CreateInboxProposalCommand) (service.InboxProposalResult, error) {
	if !c.ready() {
		return service.InboxProposalResult{}, errors.New("create Inbox proposal: durable commands are not configured")
	}
	var result service.InboxProposalResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		item, err := c.inbox.CreateInboxTx(ctx, tx, command.Inbox)
		if err != nil {
			return err
		}
		if item == nil {
			return errors.New("create Inbox proposal: repository returned nil item")
		}
		job, err := c.ensureTx(ctx, tx, item.ID, item.MetadataRevision)
		if err != nil {
			return err
		}
		jobID := job.ID
		item.JobID = &jobID
		result = service.InboxProposalResult{Inbox: item, Job: job}
		return nil
	})
	if err != nil {
		return service.InboxProposalResult{}, fmt.Errorf("create Inbox proposal: %w", err)
	}
	return result, nil
}

func (c *InboxCommands) EnsureInboxProposal(ctx context.Context, command service.EnsureInboxProposalCommand) (service.InboxProposalResult, error) {
	if !c.ready() {
		return service.InboxProposalResult{}, errors.New("ensure Inbox proposal: durable commands are not configured")
	}
	if command.InboxID == uuid.Nil {
		return service.InboxProposalResult{}, errors.New("ensure Inbox proposal: nil inbox id")
	}
	var result service.InboxProposalResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		job, err := c.ensureTx(ctx, tx, command.InboxID, command.ExpectedMetadataRevision)
		if err != nil {
			return err
		}
		result.Job = job
		return nil
	})
	if err != nil {
		return service.InboxProposalResult{}, fmt.Errorf("ensure Inbox proposal: %w", err)
	}
	return result, nil
}

// RepairInboxProposalOrphans atomically requeues one claimed batch. Any
// insertion failure rolls the complete batch back so every candidate remains
// discoverable on the next pass.
func (c *InboxCommands) RepairInboxProposalOrphans(ctx context.Context, limit int) (service.InboxProposalOrphanRepairResult, error) {
	var result service.InboxProposalOrphanRepairResult
	if !c.ready() {
		return result, errors.New("repair Inbox proposal orphans: durable commands are not configured")
	}
	if limit <= 0 {
		return result, errors.New("repair Inbox proposal orphans: batch size must be positive")
	}
	if limit > MaxInboxProposalOrphanRepairBatchSize {
		limit = MaxInboxProposalOrphanRepairBatchSize
	}

	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		orphans, err := c.inbox.ClaimInboxDispatchOrphansTx(ctx, tx, service.ReaderInboxSummaryJobKind, limit)
		if err != nil {
			return err
		}
		result.Claimed = len(orphans)
		for _, orphan := range orphans {
			switch orphan.Status {
			case "queued":
			case "running":
				if err := c.inbox.ResetInboxDispatchOrphanTx(ctx, tx, orphan.JobID); err != nil {
					return err
				}
			default:
				return fmt.Errorf("repair Inbox proposal orphans: job %s has unexpected status %q", orphan.JobID, orphan.Status)
			}
			if err := c.queue.EnqueueReaderInboxSummaryTx(ctx, tx, service.ReaderInboxSummaryJobArgs{
				JobID:                    orphan.JobID,
				InboxID:                  orphan.InboxID,
				ExpectedMetadataRevision: orphan.ExpectedMetadataRevision,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("repair Inbox proposal orphans: %w", err)
	}
	result.Repaired = result.Claimed
	return result, nil
}

func (c *InboxCommands) ensureTx(ctx context.Context, tx pgx.Tx, inboxID uuid.UUID, expectedRevision int64) (*model.ReaderInboxJob, error) {
	job, _, err := c.inbox.BeginInboxResummarizeJobTx(ctx, tx, inboxID, expectedRevision)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, errors.New("ensure Inbox proposal: repository returned nil job")
	}
	args := service.ReaderInboxSummaryJobArgs{
		JobID:                    job.ID,
		InboxID:                  job.InboxID,
		ExpectedMetadataRevision: job.ExpectedMetadataRevision,
	}
	if err := c.queue.EnqueueReaderInboxSummaryTx(ctx, tx, args); err != nil {
		return nil, err
	}
	return job, nil
}

func (c *InboxCommands) ready() bool {
	return c != nil && c.transactions != nil && c.inbox != nil && c.queue != nil
}

var _ service.InboxProposalCommands = (*InboxCommands)(nil)
var _ service.InboxProposalOrphanRepairer = (*InboxCommands)(nil)
