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

// LinkQueue is the infrastructure-only parse-work port. It is deliberately
// private to the durable adapter; application services never receive a
// transaction handle or learn River cancellation semantics.
type LinkQueue interface {
	EnqueueTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) error
	CancelActiveTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) error
	CancelAllActiveTx(context.Context, pgx.Tx, uuid.UUID) error
}

type LinkCommandsOptions struct {
	Transactions database.TxBeginner
	Links        *repository.PGXLinkRepository
	Queue        LinkQueue
}

// LinkCommands is the concrete durable command adapter shared by submit,
// conversion, and link deletion services.
type LinkCommands struct {
	transactions database.TxBeginner
	links        *repository.PGXLinkRepository
	queue        LinkQueue
}

func NewLinkCommands(options LinkCommandsOptions) *LinkCommands {
	return &LinkCommands{
		transactions: options.Transactions,
		links:        options.Links,
		queue:        options.Queue,
	}
}

func (c *LinkCommands) SubmitLink(ctx context.Context, command service.SubmitLinkCommand) (service.LinkSubmissionResult, error) {
	results, err := c.submitLinks(ctx, []service.LinkCapture{command.Capture})
	if err != nil {
		return service.LinkSubmissionResult{}, err
	}
	if len(results) != 1 || results[0].Link == nil {
		return service.LinkSubmissionResult{}, fmt.Errorf("submit link: expected one result, got %d", len(results))
	}
	return results[0], nil
}

func (c *LinkCommands) RequeueLink(ctx context.Context, command service.RequeueLinkCommand) (service.LinkSubmissionResult, error) {
	if !c.readyForLinks() {
		return service.LinkSubmissionResult{}, errors.New("requeue link: durable commands are not configured")
	}
	var capture *repository.CreateLinkParams
	if command.Capture != nil {
		converted := repositoryLinkCapture(*command.Capture)
		capture = &converted
	}
	var createdJob *model.ParseJob
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		createdJob, err = c.links.RequeueExistingTx(ctx, tx, command.LinkID, capture)
		if err != nil {
			return err
		}
		if err := c.queue.CancelActiveTx(ctx, tx, command.LinkID, createdJob.ID); err != nil {
			return err
		}
		return c.queue.EnqueueTx(ctx, tx, command.LinkID, createdJob.ID)
	})
	if err != nil {
		return service.LinkSubmissionResult{}, err
	}
	return service.LinkSubmissionResult{Job: createdJob, Inserted: true}, nil
}

// UpdateLinkIntent persists a concrete capture intent while reusing an active
// parse attempt. If terminal completion wins the row lock first, the same
// transaction creates and enqueues a replacement attempt so the later-committed
// explicit selection cannot be stranded on a done row.
func (c *LinkCommands) UpdateLinkIntent(ctx context.Context, command service.UpdateLinkIntentCommand) (service.UpdateLinkIntentResult, error) {
	if !c.readyForLinks() {
		return service.UpdateLinkIntentResult{}, errors.New("update link intent: durable commands are not configured")
	}
	var result service.UpdateLinkIntentResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		updated, err := c.links.UpdateRequestedLibraryIntentTx(ctx, tx, repository.UpdateRequestedLibraryIntentParams{
			ID: command.LinkID, Kind: command.Kind, Source: command.Source,
		})
		if err != nil {
			return err
		}
		result.Status = updated.Status
		if updated.Status == model.LinkStatusPending || updated.Status == model.LinkStatusProcessing {
			return nil
		}

		job, err := c.links.RequeueExistingTx(ctx, tx, command.LinkID, nil)
		if err != nil {
			return err
		}
		if err := c.queue.CancelActiveTx(ctx, tx, command.LinkID, job.ID); err != nil {
			return err
		}
		if err := c.queue.EnqueueTx(ctx, tx, command.LinkID, job.ID); err != nil {
			return err
		}
		result.Status = model.LinkStatusPending
		result.Job = job
		return nil
	})
	if err != nil {
		return service.UpdateLinkIntentResult{}, err
	}
	return result, nil
}

func (c *LinkCommands) SubmitLinksBatch(ctx context.Context, command service.SubmitLinksBatchCommand) ([]service.LinkSubmissionResult, error) {
	return c.submitLinks(ctx, command.Captures)
}

func (c *LinkCommands) ConvertLink(ctx context.Context, command service.ConvertLinkCommand) (service.ConvertLinkResult, error) {
	if !c.readyForLinks() {
		return service.ConvertLinkResult{}, errors.New("convert link: durable commands are not configured")
	}
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
		if err != nil || result.ParseJobID == nil {
			return err
		}
		return c.queue.EnqueueTx(ctx, tx, result.LinkID, *result.ParseJobID)
	})
	if err != nil {
		return service.ConvertLinkResult{}, err
	}
	return service.ConvertLinkResult{
		LinkID: result.LinkID, Kind: result.Kind, ContentRevision: result.ContentRevision,
		Status: result.Status, SiteID: result.SiteID, SiteRevision: result.SiteRevision,
		EntryID: result.EntryID, ParseJobID: result.ParseJobID,
	}, nil
}

func (c *LinkCommands) DeleteLink(ctx context.Context, command service.DeleteLinkCommand) error {
	if !c.readyForLinks() {
		return errors.New("delete link: durable commands are not configured")
	}
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

func (c *LinkCommands) readyForLinks() bool {
	return c != nil && c.transactions != nil && c.links != nil && c.queue != nil
}

func (c *LinkCommands) submitLinks(ctx context.Context, captures []service.LinkCapture) ([]service.LinkSubmissionResult, error) {
	if len(captures) == 0 {
		return nil, nil
	}
	if !c.readyForLinks() {
		return nil, errors.New("submit links: durable commands are not configured")
	}
	items := make([]repository.CreateLinkParams, len(captures))
	for i := range captures {
		items[i] = repositoryLinkCapture(captures[i])
	}

	var results []repository.LinkSubmitResult
	err := database.WithTx(ctx, c.transactions, func(tx pgx.Tx) error {
		var err error
		results, err = c.links.SubmitBatchTx(ctx, tx, items)
		if err != nil {
			return err
		}
		for _, result := range results {
			if result.Job == nil {
				continue
			}
			if result.Link == nil {
				return errors.New("submit links: result with parse job is missing link")
			}
			if result.Restored {
				if err := c.queue.CancelActiveTx(ctx, tx, result.Link.ID, result.Job.ID); err != nil {
					return err
				}
			}
			if err := c.queue.EnqueueTx(ctx, tx, result.Link.ID, result.Job.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return serviceLinkSubmissionResults(results), err
	}
	return serviceLinkSubmissionResults(results), nil
}

func serviceLinkSubmissionResults(results []repository.LinkSubmitResult) []service.LinkSubmissionResult {
	out := make([]service.LinkSubmissionResult, len(results))
	for i := range results {
		out[i] = service.LinkSubmissionResult{
			Link: results[i].Link, Job: results[i].Job,
			Inserted: results[i].Inserted, Restored: results[i].Restored, Error: results[i].Error,
		}
	}
	return out
}

func repositoryLinkCapture(capture service.LinkCapture) repository.CreateLinkParams {
	return repository.CreateLinkParams{
		URL: capture.URL, SourceKind: capture.SourceKind, SourceKey: capture.SourceKey,
		InputTitle: capture.InputTitle, InputText: capture.InputText, InputHTML: capture.InputHTML,
		InputImages: capture.InputImages, SourceMetadata: capture.SourceMetadata,
		Description: capture.Description, Status: capture.Status, Domain: capture.Domain,
		ContentType: capture.ContentType, PathDepth: capture.PathDepth, ParentPath: capture.ParentPath,
		ParentID: capture.ParentID, RequestedLibraryKind: capture.RequestedLibraryKind,
		RequestedLibraryKindSource: capture.RequestedLibraryKindSource,
		PredictedLibraryKind:       capture.PredictedLibraryKind,
	}
}

var (
	_ service.LinkSubmissionCommands = (*LinkCommands)(nil)
	_ service.LinkConversionCommands = (*LinkCommands)(nil)
	_ service.LinkDeletionCommands   = (*LinkCommands)(nil)
)
