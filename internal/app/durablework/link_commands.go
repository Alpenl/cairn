package durablework

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"webtag/internal/database"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

// LinkQueue is the infrastructure-only parse-work port. It is deliberately
// private to the durable adapter; application services never receive a
// transaction handle or learn River cancellation semantics.
type LinkQueue interface {
	EnqueueTx(context.Context, pgx.Tx, model.ParseAttempt) error
	CancelActiveTx(context.Context, pgx.Tx, uuid.UUID) error
	CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error
}

// LinkCommands is the concrete durable command adapter shared by submit,
// conversion, and link deletion services.
type LinkCommands struct {
	transactions database.TxBeginner
	links        *repository.PGXLinkRepository
	queue        LinkQueue
}

func NewLinkCommands(transactions database.TxBeginner, links *repository.PGXLinkRepository, queue LinkQueue) *LinkCommands {
	if transactions == nil || links == nil || queue == nil {
		panic("durablework.NewLinkCommands: transactions, links, and queue are required")
	}
	return &LinkCommands{
		transactions: transactions,
		links:        links,
		queue:        queue,
	}
}

func (c *LinkCommands) SubmitLink(ctx context.Context, command service.SubmitLinkCommand) (service.LinkSubmissionResult, error) {
	var result repository.LinkSubmitResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		result, err = c.links.SubmitTx(ctx, tx, repositoryLinkCapture(command.Capture))
		if err != nil {
			return err
		}
		if result.Link == nil {
			return errors.New("submit link: repository returned nil link")
		}
		if result.Attempt == nil {
			return nil
		}
		if result.Restored {
			if err := c.queue.CancelActiveTx(ctx, tx, result.Link.ID); err != nil {
				return err
			}
		}
		return c.queue.EnqueueTx(ctx, tx, *result.Attempt)
	})
	if err != nil {
		return service.LinkSubmissionResult{}, err
	}
	return service.LinkSubmissionResult{Link: result.Link, Enqueued: result.Attempt != nil}, nil
}

func (c *LinkCommands) RequeueLink(ctx context.Context, command service.RequeueLinkCommand) (service.LinkSubmissionResult, error) {
	var capture *repository.CreateLinkParams
	if command.Capture != nil {
		converted := repositoryLinkCapture(*command.Capture)
		capture = &converted
	}
	var attempt model.ParseAttempt
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		attempt, err = c.links.RequeueExistingTx(ctx, tx, command.LinkID, capture)
		if err != nil {
			return err
		}
		if err := c.queue.CancelActiveTx(ctx, tx, command.LinkID); err != nil {
			return err
		}
		return c.queue.EnqueueTx(ctx, tx, attempt)
	})
	if err != nil {
		return service.LinkSubmissionResult{}, err
	}
	return service.LinkSubmissionResult{Enqueued: true}, nil
}

// SetLinkLibraryKind persists a concrete capture selection while reusing an active
// parse attempt. If terminal completion wins the row lock first, the same
// transaction creates and enqueues a replacement attempt so the later-committed
// explicit selection cannot be stranded on a done row.
func (c *LinkCommands) SetLinkLibraryKind(ctx context.Context, command service.SetLinkLibraryKindCommand) (service.SetLinkLibraryKindResult, error) {
	var result service.SetLinkLibraryKindResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		updated, err := c.links.SetLibraryKindTx(ctx, tx, repository.SetLibraryKindParams{
			ID: command.LinkID, Kind: command.Kind, Override: command.Override,
		})
		if err != nil {
			return err
		}
		result.Status = updated.Status
		if updated.Status == model.LinkStatusPending || updated.Status == model.LinkStatusProcessing {
			return nil
		}

		attempt, err := c.links.RequeueExistingTx(ctx, tx, command.LinkID, nil)
		if err != nil {
			return err
		}
		if err := c.queue.CancelActiveTx(ctx, tx, command.LinkID); err != nil {
			return err
		}
		if err := c.queue.EnqueueTx(ctx, tx, attempt); err != nil {
			return err
		}
		result.Status = model.LinkStatusPending
		return nil
	})
	if err != nil {
		return service.SetLinkLibraryKindResult{}, err
	}
	return result, nil
}

func (c *LinkCommands) ConvertLink(ctx context.Context, command service.ConvertLinkCommand) (service.ConvertLinkResult, error) {
	var result repository.ConvertLinkResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		result, err = c.links.ConvertLinkTx(ctx, tx, repository.ConvertLinkParams{
			LinkID:                  command.LinkID,
			TargetKind:              command.TargetKind,
			ExpectedContentRevision: command.ExpectedContentRevision,
			TargetSiteID:            command.TargetSiteID,
			ExpectedSiteRevision:    command.ExpectedSiteRevision,
			PreservedUserNote:       command.PreservedUserNote,
		})
		if err != nil || result.ParseAttempt == nil {
			return err
		}
		return c.queue.EnqueueTx(ctx, tx, *result.ParseAttempt)
	})
	if err != nil {
		return service.ConvertLinkResult{}, err
	}
	return service.ConvertLinkResult{
		LinkID: result.LinkID, Kind: result.Kind, ContentRevision: result.ContentRevision,
		Status: result.Status, SiteID: result.SiteID, SiteRevision: result.SiteRevision,
		EntryID: result.EntryID,
	}, nil
}

func (c *LinkCommands) DeleteLink(ctx context.Context, command service.DeleteLinkCommand) error {
	return database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		lockedID, err := c.links.LockLinkForDeleteTx(ctx, tx, command.LinkID)
		if err != nil {
			return err
		}
		if err := c.queue.CancelAllActiveTx(ctx, tx, lockedID); err != nil {
			return err
		}
		return c.links.DeleteLockedLinkTx(ctx, tx, lockedID)
	})
}

func repositoryLinkCapture(capture service.LinkCapture) repository.CreateLinkParams {
	return repository.CreateLinkParams{
		URL: capture.URL, SourceKind: capture.SourceKind, SourceKey: capture.SourceKey,
		InputTitle: capture.InputTitle, InputText: capture.InputText, InputHTML: capture.InputHTML,
		InputImages: capture.InputImages, SourceMetadata: capture.SourceMetadata,
		Description: capture.Description, Status: capture.Status, Domain: capture.Domain,
		ContentType: capture.ContentType, PathDepth: capture.PathDepth, ParentPath: capture.ParentPath,
		ParentID: capture.ParentID, RequestedLibraryKind: capture.RequestedLibraryKind,
		UserSelectedLibraryKind: capture.UserSelectedLibraryKind,
	}
}

var (
	_ service.LinkSubmissionCommands = (*LinkCommands)(nil)
	_ service.LinkConversionCommands = (*LinkCommands)(nil)
	_ service.LinkDeletionCommands   = (*LinkCommands)(nil)
)
