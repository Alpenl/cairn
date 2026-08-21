package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"webtag/internal/contentdoc"
	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// linkSubmitter is the shared write-side core used by both SubmitService
// (URL-keyed Submit/Refresh/Batch flow) and IngestService (multimodal
// browser_capture flow). Holding the cross-cutting plumbing — reader
// lookups, write-state transitions, transactional new-link insert, queue
// enqueue, and the existing-link update path — in one struct is the
// reason Wave 12.3 M5 split Ingest out of SubmitService: any future
// change to the new-link / re-queue flow now lands here in one place
// instead of being duplicated across both services. Composition (not
// embedding) is intentional so each top-level service surfaces only
// its public methods through its own type, and so the shared core has
// no exported method set of its own.
type linkSubmitter struct {
	reader                linkSubmissionReader
	commands              LinkSubmissionCommands
	locker                URLLocker
	inboxWriter           InboxCaptureWriter
	inboxProposalCommands InboxProposalCommands
}

type linkSubmissionReader interface {
	GetSubmitLookupByID(context.Context, uuid.UUID) (*repository.LinkSubmitLookup, error)
	GetSubmitLookupByURL(context.Context, string) (*repository.LinkSubmitLookup, error)
	GetParseInputByID(context.Context, uuid.UUID) (*repository.LinkParseInput, error)
	GetParseInputBySourceKeyOrURL(context.Context, string, string) (*repository.LinkParseInput, error)
}

type submitCandidate struct {
	ID                uuid.UUID
	URL               string
	SourceKind        string
	SourceKey         string
	InputTitle        *string
	InputText         *string
	InputHTML         *string
	InputImages       []string
	SourceMetadata    map[string]any
	Description       *string
	Status            model.LinkStatus
	LibraryKind       *model.LibraryKind
	LibraryKindLocked bool
	ParseRequestedAt  time.Time
}

func submitCandidateFromModel(link *model.Link) *submitCandidate {
	if link == nil {
		return nil
	}
	return &submitCandidate{
		ID: link.ID, URL: link.URL, SourceKind: link.SourceKind, SourceKey: link.SourceKey,
		InputTitle: link.InputTitle, InputText: link.InputText, InputHTML: link.InputHTML,
		InputImages: link.InputImages, SourceMetadata: link.SourceMetadata,
		Description: link.Description, Status: link.Status, LibraryKind: link.LibraryKind,
		LibraryKindLocked: link.LibraryKindLocked,
		ParseRequestedAt:  parseRequestedAt(link.FirstCollectedAt, link.LastRecollectedAt),
	}
}

func submitCandidateFromLookup(link *repository.LinkSubmitLookup) *submitCandidate {
	if link == nil {
		return nil
	}
	return &submitCandidate{
		ID: link.ID, URL: link.URL, SourceKey: link.SourceKey, Status: link.Status,
		LibraryKind: link.LibraryKind, LibraryKindLocked: link.LibraryKindLocked,
		ParseRequestedAt: link.ParseRequestedAt,
	}
}

func parseRequestedAt(first time.Time, last *time.Time) time.Time {
	if last != nil {
		return *last
	}
	return first
}

func submitCandidateFromParseInput(link *repository.LinkParseInput) *submitCandidate {
	if link == nil {
		return nil
	}
	return &submitCandidate{
		ID: link.ID, URL: link.URL, SourceKind: link.SourceKind, SourceKey: link.SourceKey,
		InputTitle: link.InputTitle, InputText: link.InputText, InputHTML: link.InputHTML,
		InputImages: link.InputImages, SourceMetadata: link.SourceMetadata,
		Description: link.Description, Status: link.Status,
		LibraryKind: link.LibraryKind, LibraryKindLocked: link.LibraryKindLocked,
	}
}

func newLinkSubmitter(
	reader linkSubmissionReader,
	commands LinkSubmissionCommands,
	locker URLLocker,
	inboxWriter InboxCaptureWriter,
	inboxProposalCommands InboxProposalCommands,
) *linkSubmitter {
	if locker == nil {
		locker = noopURLLocker{}
	}
	return &linkSubmitter{
		reader:                reader,
		commands:              commands,
		locker:                locker,
		inboxWriter:           inboxWriter,
		inboxProposalCommands: inboxProposalCommands,
	}
}

// NewLinkServices constructs the shared write core used by Submit and Ingest.
func NewLinkServices(
	reader linkSubmissionReader,
	commands LinkSubmissionCommands,
	locker URLLocker,
	submitOpts SubmitServiceOptions,
) (*SubmitService, *IngestService) {
	core := newLinkSubmitter(reader, commands, locker, submitOpts.InboxWriter, submitOpts.InboxProposalCommands)
	cooldown := submitOpts.RefreshCooldown
	if cooldown <= 0 {
		cooldown = defaultRefreshCooldown
	}
	return &SubmitService{
			core:            core,
			refreshCooldown: cooldown,
			now:             time.Now,
		}, &IngestService{
			core: core,
		}
}

func (s *linkSubmitter) createInbox(ctx context.Context, capture LinkCapture) (dto.SubmitResponse, error) {
	identityKey := capture.SourceKey
	if identityKey == "" {
		identityKey = capture.URL
	}
	existing, err := s.inboxWriter.GetInboxByURL(ctx, identityKey)
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	if existing != nil {
		return s.reuseInbox(ctx, existing)
	}

	created, err := s.createInboxRecord(ctx, newInboxCapture(capture, identityKey))
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	if created == nil {
		return dto.SubmitResponse{}, errors.New("create inbox: durable command returned nil item")
	}
	status := created.Status
	if status == "" {
		status = "pending"
	}
	return inboxSubmitResponse(created.ID, status), nil
}

func (s *linkSubmitter) reuseInbox(ctx context.Context, existing *model.ReaderInbox) (dto.SubmitResponse, error) {
	if existing.Status == "pending" && existing.ProposalStatus != "completed" {
		_, err := s.inboxProposalCommands.EnsureInboxProposal(ctx, EnsureInboxProposalCommand{
			InboxID: existing.ID, ExpectedMetadataRevision: existing.MetadataRevision,
		})
		if err != nil {
			return dto.SubmitResponse{}, err
		}
	}
	return inboxSubmitResponse(existing.ID, existing.Status), nil
}

func newInboxCapture(capture LinkCapture, identityKey string) model.ReaderInbox {
	item := model.ReaderInbox{
		URL:         capture.URL,
		IdentityKey: identityKey,
		SourceKind:  capture.SourceKind,
		Title:       capture.InputTitle,
		Summary:     capture.Description,
		Status:      "pending",
	}
	if item.SourceKind == "" {
		item.SourceKind = "url"
	}
	item.Body, item.BodyDocument, item.BodyFormat = inboxCaptureBody(capture)
	return item
}

// inboxCaptureBody 把一次采集的正文归一化成收件箱要存的三元组
// （纯文本投影, Markdown 文档, 格式）。
//
// 这里必须转换，而不能只留纯文本：收件箱的 body 是确认入库时唯一的正文来源，
// 而确认入库直接写 links 的正文列、不走 ContentService.Save，链接行上也不会有
// input_html。采集时丢掉的结构，之后任何一步都拿不回来——只能重新采集。
//
// 采集到的 HTML 优先于扩展一并送来的纯文本：后者是 innerText 压平的结果，
// 段落、标题、列表、代码块全都塌成同一层文字。contentdoc.FromHTML 会再净化
// 一次（浏览器侧的脱敏不足以信任），并同时给出干净的纯文本投影，因此转换成功
// 时两个字段同源。转换失败或转不出正文时退回纯文本，宁可诚实地存一堆文字，
// 也不要凭空编一个结构出来。
func inboxCaptureBody(capture LinkCapture) (string, *string, model.ContentFormat) {
	if capture.InputHTML != nil && strings.TrimSpace(*capture.InputHTML) != "" {
		document, err := contentdoc.FromHTML(*capture.InputHTML, capture.URL)
		if err == nil && document.Text != "" {
			return document.Text, document.Document, document.Format
		}
	}
	if capture.InputText != nil && strings.TrimSpace(*capture.InputText) != "" {
		plain := contentdoc.Plain(*capture.InputText)
		return plain.Text, nil, plain.Format
	}
	if capture.InputHTML != nil {
		return *capture.InputHTML, nil, model.ContentFormatPlain
	}
	return "", nil, model.ContentFormatPlain
}

func (s *linkSubmitter) createInboxRecord(ctx context.Context, item model.ReaderInbox) (*model.ReaderInbox, error) {
	result, err := s.inboxProposalCommands.CreateInboxProposal(ctx, CreateInboxProposalCommand{Inbox: item})
	if err != nil {
		return nil, err
	}
	return result.Inbox, nil
}

func inboxSubmitResponse(inboxID uuid.UUID, status string) dto.SubmitResponse {
	return dto.SubmitResponse{
		InboxID:     inboxID.String(),
		Destination: captureDestinationInbox,
		Status:      status,
	}
}

// createNewLink asks the durable command module to insert the link and River
// parse job in one transaction. This closes the
// legacy commit/enqueue gap: previously a
// crash between "link committed" and "in-memory Enqueue" stranded the link in
// pending with no queued work (the old startup ResetProcessingToPending seed
// existed precisely to mop that up; River + same-tx insert makes it moot).
func (s *linkSubmitter) createNewLink(ctx context.Context, capture LinkCapture) (dto.SubmitResponse, error) {
	result, err := s.commands.SubmitLink(ctx, SubmitLinkCommand{Capture: capture})
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	link := result.Link
	if link == nil {
		return dto.SubmitResponse{}, errors.New("submit link: durable command returned nil link")
	}
	if !result.Enqueued {
		// A soft-deleted identity may be restored without a parse, and a writer
		// outside the URL-lock boundary may win after the service lookup. Reuse
		// that persisted state instead of treating either outcome as a failure.
		return s.submitExisting(ctx, submitCandidateFromModel(link), &capture)
	}
	return dto.SubmitResponse{
		LinkID: link.ID.String(),
		Status: string(model.LinkStatusPending),
	}, nil
}

// requeueExisting atomically resets an existing link, creates the immutable
// parse attempt, and inserts the River job. capture is nil for Refresh; ingest
// re-submits pass their latest normalized source input for replacement.
func (s *linkSubmitter) requeueExisting(ctx context.Context, linkID uuid.UUID, capture *LinkCapture) (dto.SubmitResponse, error) {
	result, err := s.commands.RequeueLink(ctx, RequeueLinkCommand{LinkID: linkID, Capture: capture})
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	if !result.Enqueued {
		return dto.SubmitResponse{}, errors.New("requeue link: durable command did not enqueue parse work")
	}
	return dto.SubmitResponse{
		LinkID: linkID.String(),
		Status: string(model.LinkStatusPending),
	}, nil
}

// submitExisting makes saving the same URL idempotent. Terminal links return
// their persisted state without re-parsing; retry is exclusively the explicit
// Refresh operation. In-flight links return their current Link status while
// River remains the execution source of truth.
func (s *linkSubmitter) submitExisting(ctx context.Context, link *submitCandidate, input *LinkCapture) (dto.SubmitResponse, error) {
	switch link.Status {
	case model.LinkStatusPending, model.LinkStatusProcessing:
		if input != nil {
			if input.RequestedLibraryKind != model.RequestedLibraryKindAuto {
				updated, err := s.commands.SetLinkLibraryKind(ctx, SetLinkLibraryKindCommand{
					LinkID:   link.ID,
					Kind:     model.LibraryKind(input.RequestedLibraryKind),
					Override: input.UserSelectedLibraryKind,
				})
				if err != nil {
					return dto.SubmitResponse{}, err
				}
				return dto.SubmitResponse{LinkID: link.ID.String(), Status: string(updated.Status)}, nil
			}
		}
		return dto.SubmitResponse{LinkID: link.ID.String(), Status: string(link.Status)}, nil
	default:
		return s.reuseExisting(ctx, link)
	}
}

// reuseExisting reports the persisted state without creating work. Ingest uses
// it when a repeated capture has no meaningful changes, including after a
// failed attempt; retry is an explicit Refresh action, not a side effect of
// saving the same snapshot again.
func (s *linkSubmitter) reuseExisting(ctx context.Context, link *submitCandidate) (dto.SubmitResponse, error) {
	return dto.SubmitResponse{LinkID: link.ID.String(), Status: string(link.Status)}, nil
}

// requireExisting wraps the "look up by id, 404 on miss" pattern used
// by Refresh and any future re-submit-by-id paths. Centralised so the
// 400/404 status codes stay consistent.
func (s *linkSubmitter) requireExisting(ctx context.Context, linkID string) (*submitCandidate, error) {
	id, err := uuid.Parse(linkID)
	if err != nil {
		return nil, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidLinkID, "invalid link id")
	}
	link, err := s.reader.GetSubmitLookupByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if link == nil {
		return nil, httperr.NewWithCode(http.StatusNotFound, httperr.CodeLinkNotFound, "link not found")
	}
	return submitCandidateFromLookup(link), nil
}
