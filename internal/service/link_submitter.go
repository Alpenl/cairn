package service

import (
	"context"
	"errors"
	"fmt"
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
	reader                  linkSubmissionReader
	jobs                    repository.JobStore
	commands                LinkSubmissionCommands
	locker                  URLLocker
	tagCacheInvalidator     CacheInvalidator
	disableSiteLibraryWrite bool
	inboxWriter             InboxCaptureWriter
	inboxJobScheduler       ReaderInboxJobScheduler
	inboxProposalCommands   InboxProposalCommands
}

type linkSubmissionReader interface {
	repository.LinkSubmitLookupReader
	repository.LinkParseInputReader
}

type submitCandidate struct {
	ID                         uuid.UUID
	URL                        string
	SourceKind                 string
	SourceKey                  string
	InputTitle                 *string
	InputText                  *string
	InputHTML                  *string
	InputImages                []string
	SourceMetadata             map[string]any
	Description                *string
	Status                     model.LinkStatus
	RequestedLibraryKind       model.RequestedLibraryKind
	RequestedLibraryKindSource model.RequestedLibraryKindSource
	LibraryKind                *model.LibraryKind
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
	}
}

func submitCandidateFromLookup(link *repository.LinkSubmitLookup) *submitCandidate {
	if link == nil {
		return nil
	}
	return &submitCandidate{
		ID: link.ID, URL: link.URL, SourceKey: link.SourceKey, Status: link.Status,
		RequestedLibraryKind: link.RequestedLibraryKind, RequestedLibraryKindSource: link.RequestedLibraryKindSource,
		LibraryKind: link.LibraryKind,
	}
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
		RequestedLibraryKind: link.RequestedLibraryKind, RequestedLibraryKindSource: link.RequestedLibraryKindSource,
		LibraryKind: link.LibraryKind,
	}
}

func newLinkSubmitter(
	reader linkSubmissionReader,
	jobs repository.JobStore,
	commands LinkSubmissionCommands,
	locker URLLocker,
	tagCacheInvalidator CacheInvalidator,
	disableSiteLibraryWrite bool,
	inboxWriter InboxCaptureWriter,
	inboxJobScheduler ReaderInboxJobScheduler,
	inboxProposalCommands InboxProposalCommands,
) *linkSubmitter {
	if locker == nil {
		locker = noopURLLocker{}
	}
	return &linkSubmitter{
		reader:                  reader,
		jobs:                    jobs,
		commands:                commands,
		locker:                  locker,
		tagCacheInvalidator:     tagCacheInvalidator,
		disableSiteLibraryWrite: disableSiteLibraryWrite,
		inboxWriter:             inboxWriter,
		inboxJobScheduler:       inboxJobScheduler,
		inboxProposalCommands:   inboxProposalCommands,
	}
}

// NewLinkServices constructs a single shared *linkSubmitter core and wires
// both SubmitService and IngestService against it. Wave 13 Phase B M-1:
// boot wiring uses this factory to avoid building the same core twice.
// The standalone NewSubmitService / NewIngestService constructors remain for
// existing callers (notably the test helpers in submit_helpers_test.go); each
// builds its own private core, while the boot path consolidates both services.
func NewLinkServices(
	reader linkSubmissionReader,
	jobs repository.JobStore,
	commands LinkSubmissionCommands,
	locker URLLocker,
	submitOpts SubmitServiceOptions,
) (*SubmitService, *IngestService) {
	core := newLinkSubmitter(reader, jobs, commands, locker, submitOpts.TagCacheInvalidator, submitOpts.DisableSiteLibraryWrite, submitOpts.InboxWriter, submitOpts.InboxJobScheduler, submitOpts.InboxProposalCommands)
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

// defaultCaptureDestination 返回请求未显式指定 destination 时的采集目的地。
// 收藏默认进收件箱：收藏是「先收着」，抓取结果、标题与归类都还没确认，直接进
// 阅读库会把未经确认的条目混进正式书库。
//
// 但收件箱是可选特性——未启用 Reader Inbox 的部署里 inboxWriter 为 nil。缺省值
// 在那里必须退回阅读库，否则一个「默认行为」会把这类部署的每一次收藏都变成 503。
// 显式请求 inbox 不走这里，仍由 createInbox 明确报 inbox_destination_unavailable，
// 用户点名要收件箱时不该被静默改道。
func (s *linkSubmitter) defaultCaptureDestination() string {
	if s.inboxWriter == nil {
		return captureDestinationLibrary
	}
	return captureDestinationInbox
}

func (s *linkSubmitter) createInbox(ctx context.Context, capture LinkCapture) (dto.SubmitResponse, error) {
	if s.inboxWriter == nil {
		return dto.SubmitResponse{}, httperr.NewWithCode(http.StatusServiceUnavailable, "inbox_destination_unavailable", "inbox destination is not available")
	}
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

	created, jobID, err := s.createInboxRecord(ctx, newInboxCapture(capture, identityKey))
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
	return inboxSubmitResponse(created.ID, status, jobID), nil
}

func (s *linkSubmitter) reuseInbox(ctx context.Context, existing *model.ReaderInbox) (dto.SubmitResponse, error) {
	var jobID *uuid.UUID
	if s.inboxProposalCommands != nil && existing.ProposalStatus != "completed" {
		result, err := s.inboxProposalCommands.EnsureInboxProposal(ctx, EnsureInboxProposalCommand{
			InboxID: existing.ID, ExpectedMetadataRevision: existing.MetadataRevision,
		})
		if err != nil {
			return dto.SubmitResponse{}, err
		}
		if result.Job != nil {
			jobID = &result.Job.ID
		}
	}
	return inboxSubmitResponse(existing.ID, existing.Status, jobID), nil
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

func (s *linkSubmitter) createInboxRecord(ctx context.Context, item model.ReaderInbox) (*model.ReaderInbox, *uuid.UUID, error) {
	if s.inboxProposalCommands != nil {
		result, commandErr := s.inboxProposalCommands.CreateInboxProposal(ctx, CreateInboxProposalCommand{Inbox: item})
		if commandErr != nil {
			return nil, nil, commandErr
		}
		var jobID *uuid.UUID
		if result.Job != nil {
			jobID = &result.Job.ID
		}
		return result.Inbox, jobID, nil
	}

	created, err := s.inboxWriter.CreateInbox(ctx, item)
	if err != nil {
		return nil, nil, err
	}
	if created == nil {
		return nil, nil, errors.New("create inbox: writer returned nil item")
	}
	jobID, err := scheduleInboxCaptureJob(ctx, s.inboxWriter, s.inboxJobScheduler, created)
	if err != nil {
		return nil, nil, err
	}
	return created, jobID, nil
}

func inboxSubmitResponse(inboxID uuid.UUID, status string, jobID *uuid.UUID) dto.SubmitResponse {
	response := dto.SubmitResponse{
		InboxID:     inboxID.String(),
		Destination: captureDestinationInbox,
		Status:      status,
	}
	if jobID != nil {
		id := jobID.String()
		response.JobID = &id
	}
	return response
}

type inboxCaptureJobWriter interface {
	BeginInboxResummarizeJob(context.Context, uuid.UUID, int64) (*model.ReaderInboxJob, bool, error)
	FailInboxJob(context.Context, uuid.UUID, string) error
}

func scheduleInboxCaptureJob(ctx context.Context, writer InboxCaptureWriter, scheduler ReaderInboxJobScheduler, item *model.ReaderInbox) (*uuid.UUID, error) {
	jobs, ok := writer.(inboxCaptureJobWriter)
	if !ok || scheduler == nil {
		return nil, nil
	}
	job, created, err := jobs.BeginInboxResummarizeJob(ctx, item.ID, item.MetadataRevision)
	if err != nil {
		return nil, fmt.Errorf("begin Inbox proposal job: %w", err)
	}
	if created {
		args := ReaderInboxSummaryJobArgs{JobID: job.ID, InboxID: item.ID, ExpectedMetadataRevision: job.ExpectedMetadataRevision}
		if err := scheduler.EnqueueReaderInboxSummary(ctx, args); err != nil {
			_ = jobs.FailInboxJob(ctx, job.ID, "reader_inbox_job_enqueue_failed")
			return nil, fmt.Errorf("enqueue Inbox proposal job: %w", err)
		}
	}
	return &job.ID, nil
}

// createNewLink asks the durable command module to insert the link, initial
// parse_jobs row, and River parse job in one transaction. This closes the
// legacy commit/enqueue gap: previously a
// crash between "link committed" and "in-memory Enqueue" stranded the link in
// pending with no queued work (the old startup ResetProcessingToPending seed
// existed precisely to mop that up; River + same-tx insert makes it moot).
func (s *linkSubmitter) createNewLink(ctx context.Context, capture LinkCapture) (dto.SubmitResponse, error) {
	if s.commands == nil {
		return dto.SubmitResponse{}, errors.New("submit link: durable commands are not configured")
	}
	result, err := s.commands.SubmitLink(ctx, SubmitLinkCommand{Capture: capture})
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	link, job := result.Link, result.Job
	if link == nil {
		return dto.SubmitResponse{}, errors.New("submit link: durable command returned nil link")
	}
	if job == nil {
		// Batch does not take the per-URL lock, so it can commit after the
		// single-submit lookup but before the durable insert transaction. The repository
		// returns that existing link with no new job; reconcile its current
		// attempt instead of treating the idempotent conflict as a failure.
		return s.submitExisting(ctx, submitCandidateFromModel(link), &capture)
	}
	jobIDStr := job.ID.String()
	return dto.SubmitResponse{
		JobID:  &jobIDStr,
		LinkID: link.ID.String(),
		Status: string(model.LinkStatusPending),
	}, nil
}

// requeueExisting atomically resets an existing link, creates the immutable
// parse attempt, and inserts the River job. capture is nil for Refresh; ingest
// re-submits pass their latest normalized source input for replacement.
func (s *linkSubmitter) requeueExisting(ctx context.Context, linkID uuid.UUID, capture *LinkCapture) (dto.SubmitResponse, error) {
	if s.commands == nil {
		return dto.SubmitResponse{}, errors.New("requeue link: durable commands are not configured")
	}
	result, err := s.commands.RequeueLink(ctx, RequeueLinkCommand{LinkID: linkID, Capture: capture})
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	job := result.Job
	if job == nil {
		return dto.SubmitResponse{}, errors.New("requeue link: durable command returned nil parse job")
	}
	if s.tagCacheInvalidator != nil {
		s.tagCacheInvalidator.Invalidate(ctx)
	}

	jobIDStr := job.ID.String()
	return dto.SubmitResponse{
		JobID:  &jobIDStr,
		LinkID: linkID.String(),
		Status: string(model.LinkStatusPending),
	}, nil
}

// submitExisting makes saving the same URL idempotent. Terminal links return
// their persisted attempt without re-parsing; retry is exclusively the
// explicit Refresh operation. In-flight links return their current attempt.
// The sole repair branch is an impossible-under-normal-writes in-flight link
// with no parse job, which is atomically given a new attempt.
func (s *linkSubmitter) submitExisting(ctx context.Context, link *submitCandidate, input *LinkCapture) (dto.SubmitResponse, error) {
	switch link.Status {
	case model.LinkStatusSkeleton:
		// Skeleton rows were created by the retired tree-ancestor flow and have
		// never represented a user-saved bookmark. A real save must promote the
		// row into the normal parse lifecycle instead of exposing the internal
		// legacy state on the wire. Passing the current input preserves notes,
		// parse-depth metadata, and capture payloads during the promotion.
		return s.requeueExisting(ctx, link.ID, input)
	case model.LinkStatusPending, model.LinkStatusProcessing:
		if input != nil {
			kind, source := normalizeCaptureRequestedLibraryIntent(input.RequestedLibraryKind, input.RequestedLibraryKindSource)
			if kind != model.RequestedLibraryKindAuto {
				if s.commands == nil {
					return dto.SubmitResponse{}, errors.New("update link intent: durable commands are not configured")
				}
				updated, err := s.commands.UpdateLinkIntent(ctx, UpdateLinkIntentCommand{LinkID: link.ID, Kind: kind, Source: source})
				if err != nil {
					return dto.SubmitResponse{}, err
				}
				if updated.Job != nil {
					jobID := updated.Job.ID.String()
					return dto.SubmitResponse{JobID: &jobID, LinkID: link.ID.String(), Status: string(updated.Status)}, nil
				}
			}
		}
		job, err := s.jobs.GetLatestByLinkID(ctx, link.ID)
		if err != nil {
			return dto.SubmitResponse{}, err
		}
		if job != nil {
			jobIDStr := job.ID.String()
			return dto.SubmitResponse{
				JobID:  &jobIDStr,
				LinkID: link.ID.String(),
				Status: string(link.Status),
			}, nil
		}
		return s.requeueExisting(ctx, link.ID, nil)
	default:
		return s.reuseExisting(ctx, link)
	}
}

// reuseExisting reports the persisted state without creating work. Ingest uses
// it when a repeated capture has no meaningful changes, including after a
// failed attempt; retry is an explicit Refresh action, not a side effect of
// saving the same snapshot again.
func (s *linkSubmitter) reuseExisting(ctx context.Context, link *submitCandidate) (dto.SubmitResponse, error) {
	job, err := s.jobs.GetLatestByLinkID(ctx, link.ID)
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	response := dto.SubmitResponse{LinkID: link.ID.String(), Status: string(link.Status)}
	if job != nil {
		jobID := job.ID.String()
		response.JobID = &jobID
	}
	return response, nil
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
