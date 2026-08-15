package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// Batch issues one repository.SubmitBatch (multi-row ON CONFLICT INSERT)
// per request. This replaced the previous fan-out across N goroutines /
// per-URL advisory locks because the lock contention dominated wall-clock
// time for hot URLs and the goroutine overhead added a fixed ~5ms per
// item. The new path completes a 100-item batch in roughly one round-
// trip to the DB plus one INSERT for the freshly inserted subset's
// parse_jobs rows.
//
// Race-safety: single and batch writes acquire the same URL lock namespace
// before opening a repository transaction. Batch takes the deduplicated set on
// one lock connection in stable order, preserving one repository round-trip
// without introducing the transaction -> advisory-lock inversion that can
// deadlock a small connection pool.
//
// Pre-batch dedupe: SubmitBatch requires the caller to deduplicate by
// source_key (PostgreSQL rejects a single ON CONFLICT command targeting
// the same conflict row twice). Each URL capture carries its canonical URL
// identity as SourceKey, so request-level dedupe uses that same value.
//
//nolint:gocyclo // reason: batch ingest 的编排：per-item 校验 + URL 规范化 + dedupe + 调 repo SubmitBatch + per-item 结果回填，链路上每一步都依赖前一步的中间状态，拆 helper 会反复传递 items/dedupeMap/results 三个切片。
func (s *SubmitService) Batch(ctx context.Context, req dto.BatchCreateRequest) (dto.BatchSubmitResponse, error) {
	// Wave 9 MED 迁移：给 batch 入口的两个 422 加 slug，前端能在
	// "调用方该重发空载请求 vs. 必须拆分为多个 batch" 这两种语义之间分支。
	if len(req.Items) == 0 {
		return dto.BatchSubmitResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeBatchItemsRequired, "batch items are required")
	}
	if len(req.Items) > defaultBatchSubmitLimit {
		return dto.BatchSubmitResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeBatchItemsExceedLimit, "batch items exceed limit")
	}

	// Step 1: validate each input URL. Items that fail validation are
	// recorded as errored slots in the output and excluded from the
	// SubmitBatch call. The downstream dedupe operates on canonical
	// URLs, so this loop must run before the dedupe.
	type itemContext struct {
		index         int    // position in the original req.Items slice
		submittedURL  string // exactly what the client sent; trimmed for display and kept verbatim as evidence
		rawURL        string // canonical URL identity after validateURL
		descPtr       *string
		parseDepth    string // normalized; "" means "use deployment default"
		requestedKind model.RequestedLibraryKind
		destination   string
	}
	entries := make([]dto.BatchItemResponse, len(req.Items))
	defaultDestination := s.core.defaultCaptureDestination()
	contexts := make([]itemContext, 0, len(req.Items))
	for i, item := range req.Items {
		rawURL, err := validateURL(item.URL)
		if err != nil {
			entries[i] = dto.BatchItemResponse{Error: batchItemErrorMessage(err)}
			continue
		}
		parseDepth, err := dto.NormalizeParseDepth(item.ParseDepth)
		if err != nil {
			entries[i] = dto.BatchItemResponse{Error: batchItemErrorMessage(err)}
			continue
		}
		if err := validateLinkDescription(item.Description); err != nil {
			entries[i] = dto.BatchItemResponse{Error: batchItemErrorMessage(err)}
			continue
		}
		requestedKind, err := normalizeRequestedLibraryKind(item.RequestedLibraryKind)
		if err != nil {
			entries[i] = dto.BatchItemResponse{Error: batchItemErrorMessage(err)}
			continue
		}
		if err := requireSiteLibraryWrite(requestedKind, s.core.disableSiteLibraryWrite); err != nil {
			entries[i] = dto.BatchItemResponse{Error: batchItemErrorMessage(err)}
			continue
		}
		destination, err := normalizeCaptureDestination(item.Destination, defaultDestination)
		if err != nil {
			entries[i] = dto.BatchItemResponse{Error: batchItemErrorMessage(err)}
			continue
		}
		contexts = append(contexts, itemContext{
			index:         i,
			submittedURL:  item.URL,
			rawURL:        rawURL,
			descPtr:       item.Description,
			parseDepth:    parseDepth,
			requestedKind: requestedKind,
			destination:   destination,
		})
	}

	// Step 2: dedupe by canonical URL while preserving the first-seen
	// index. A duplicate inside the same /api/links/batch request gets
	// silently collapsed onto its first occurrence — the response slot
	// for the duplicate receives the same SubmitResponse as the leader.
	// This matches the legacy fan-out behaviour where two concurrent
	// SubmitNews on the same URL would observe the second one as an
	// existing-link (via the URL lock + GetByURL).
	firstByURL := make(map[string]int, len(contexts))
	dedupedParams := make([]LinkCapture, 0, len(contexts))
	dedupedCtxIdx := make([]int, 0, len(contexts)) // ctx index in `contexts` for each deduped row
	inboxLeaders := make([]itemContext, 0, len(contexts))
	for ci, ictx := range contexts {
		identity := ictx.destination + "\x00" + ictx.rawURL
		if _, seen := firstByURL[identity]; seen {
			continue
		}
		firstByURL[identity] = ci
		if ictx.destination == captureDestinationInbox {
			inboxLeaders = append(inboxLeaders, ictx)
			continue
		}
		intent := resolveRequestedLibraryIntent(requestedLibraryIntent{}, userRequestedLibraryIntent(ictx.requestedKind))
		dedupedParams = append(dedupedParams, LinkCapture{
			URL:                        strings.TrimSpace(ictx.submittedURL),
			Destination:                ictx.destination,
			SourceKey:                  ictx.rawURL,
			Description:                ictx.descPtr,
			Status:                     model.LinkStatusPending,
			SourceMetadata:             withOriginalURL(sourceMetadataForParseDepth(ictx.parseDepth), ictx.submittedURL, ictx.rawURL),
			RequestedLibraryKind:       intent.Kind,
			RequestedLibraryKindSource: intent.Source,
		})
		dedupedCtxIdx = append(dedupedCtxIdx, ci)
	}

	// Inbox captures have no parse job and therefore do not belong in the
	// durable links batch transaction. They still use the same URL lock and
	// idempotent URL lookup as the single-item path.
	inboxResponses := make(map[string]dto.SubmitResponse, len(inboxLeaders))
	inboxErrors := make(map[string]error, len(inboxLeaders))
	for _, ictx := range inboxLeaders {
		capture := LinkCapture{
			URL:         strings.TrimSpace(ictx.submittedURL),
			Destination: captureDestinationInbox,
			SourceKind:  "url",
			SourceKey:   ictx.rawURL,
			Description: ictx.descPtr,
			Status:      model.LinkStatusPending,
		}
		var response dto.SubmitResponse
		err := s.core.locker.WithURL(ctx, ictx.rawURL, func(lockCtx context.Context) error {
			var err error
			response, err = s.core.createInbox(lockCtx, capture)
			return err
		})
		if err != nil {
			inboxErrors[ictx.rawURL] = err
			continue
		}
		inboxResponses[ictx.rawURL] = response
	}

	// Step 3: one repo round-trip for the whole batch.
	var results []LinkSubmissionResult
	if len(dedupedParams) > 0 {
		lockURLs := make([]string, len(dedupedParams))
		for i := range dedupedParams {
			lockURLs[i] = dedupedParams[i].SourceKey
		}
		err := s.core.locker.WithURLs(ctx, lockURLs, func(lockCtx context.Context) error {
			if s.core.commands == nil {
				return errors.New("submit links batch: durable commands are not configured")
			}
			out, submitErr := s.core.commands.SubmitLinksBatch(lockCtx, SubmitLinksBatchCommand{Captures: dedupedParams})
			results = out
			return submitErr
		})
		if err != nil {
			// Whole-batch failure: every item slot that has not already been
			// filled by validation errors gets the same error. The durable
			// transaction either commits all new rows and jobs or rolls back.
			batchErr := batchItemErrorMessage(err)
			for _, ictx := range contexts {
				if entries[ictx.index].Result == nil && entries[ictx.index].Error == "" {
					entries[ictx.index] = dto.BatchItemResponse{Error: batchErr}
				}
			}
			return dto.BatchSubmitResponse{Results: entries}, nil
		}
	}

	// Step 4: turn each repo result into a SubmitResponse. Inserted ==
	// true rows need an Enqueue + the brand-new job_id; Inserted ==
	// false rows go through submitExisting (which idempotently reports done /
	// pending / processing / failed the same way the single-URL path does).
	// Duplicates collapsed in step 2 read the
	// same response back from the leader slot in step 5.
	leaderResponses := make(map[string]dto.SubmitResponse, len(results))
	leaderErrors := make(map[string]error, len(results))
	for ri, result := range results {
		ictx := contexts[dedupedCtxIdx[ri]]
		var (
			resp dto.SubmitResponse
			err  error
		)
		switch {
		case result.Error != nil:
			err = result.Error
		case result.Inserted:
			jobIDStr := result.Job.ID.String()
			resp = dto.SubmitResponse{
				JobID:  &jobIDStr,
				LinkID: result.Link.ID.String(),
				Status: string(model.LinkStatusPending),
			}
		default:
			// Existing row: route through the same reconciliation
			// helper the URL-keyed Submit path uses so the
			// response shape and no-implicit-retry behavior stay identical
			// across the two surfaces.
			resp, err = s.submitBatchExisting(ctx, ictx.rawURL, submitCandidateFromModel(result.Link), &dedupedParams[ri])
		}
		if err != nil {
			leaderErrors[ictx.rawURL] = err
		} else {
			leaderResponses[ictx.rawURL] = resp
		}
	}

	// Step 5: write final entries (leader + duplicates collapsed in
	// step 2). Duplicates inherit the leader's response or error —
	// they cannot have independent outcomes by construction.
	for _, ictx := range contexts {
		if entries[ictx.index].Error != "" {
			// Validation error in step 1 already filled this slot.
			continue
		}
		if ictx.destination == captureDestinationInbox {
			if inboxErr, hasErr := inboxErrors[ictx.rawURL]; hasErr {
				entries[ictx.index] = dto.BatchItemResponse{Error: batchItemErrorMessage(inboxErr)}
				continue
			}
			if response, ok := inboxResponses[ictx.rawURL]; ok {
				entry := response
				entries[ictx.index].Result = &entry
			}
			continue
		}
		if leaderErr, hasErr := leaderErrors[ictx.rawURL]; hasErr {
			entries[ictx.index] = dto.BatchItemResponse{Error: batchItemErrorMessage(leaderErr)}
			continue
		}
		if resp, ok := leaderResponses[ictx.rawURL]; ok {
			entry := resp
			entries[ictx.index].Result = &entry
		}
	}

	return dto.BatchSubmitResponse{Results: entries}, nil
}

// submitBatchExisting keeps terminal reuse lock-free. States that can turn an
// existing snapshot into a write are serialized and re-read: skeleton promotes
// into the real lifecycle, while pending/processing with no attempt repairs an
// orphan. Without the locked re-read, concurrent batches could each create an
// attempt and return a job id that the other request immediately supersedes.
func (s *SubmitService) submitBatchExisting(
	ctx context.Context,
	rawURL string,
	link *submitCandidate,
	input *LinkCapture,
) (dto.SubmitResponse, error) {
	switch link.Status {
	case model.LinkStatusSkeleton, model.LinkStatusPending, model.LinkStatusProcessing:
		// Continue below through the locked re-read.
	default:
		return s.core.submitExisting(ctx, link, input)
	}

	var response dto.SubmitResponse
	err := s.core.locker.WithURL(ctx, rawURL, func(lockCtx context.Context) error {
		projection, err := s.core.reader.GetSubmitLookupByURL(lockCtx, rawURL)
		if err != nil {
			return err
		}
		if projection == nil {
			return repository.ErrNotFound
		}
		response, err = s.core.submitExisting(lockCtx, submitCandidateFromLookup(projection), input)
		return err
	})
	return response, err
}
