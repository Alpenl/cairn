package service

import (
	"context"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// IngestService owns the multimodal /api/ingest entrypoint. Wave 12.3
// M5 split it out of SubmitService so the URL-keyed Submit/Refresh/
// Batch surface and the source-key-keyed Ingest surface no longer share
// a struct — the only thing they share is the *linkSubmitter core. The
// dto.IngestRequest → normalizedIngest pipeline lives in
// ingest_normalize.go (Wave 12 L-4 file split).
type IngestService struct {
	core *linkSubmitter
}

func captureChanged(link *model.Link, next LinkCapture) bool {
	return submitCaptureChanged(submitCandidateFromModel(link), next)
}

func submitCaptureChanged(link *submitCandidate, next LinkCapture) bool {
	if link == nil {
		return true
	}
	if strings.TrimSpace(link.SourceKind) != strings.TrimSpace(next.SourceKind) ||
		strings.TrimSpace(link.SourceKey) != strings.TrimSpace(next.SourceKey) ||
		captureSourceFingerprint(link.SourceMetadata) != captureSourceFingerprint(next.SourceMetadata) ||
		!optionalTextEqual(link.InputTitle, next.InputTitle) ||
		!optionalTextEqual(link.InputText, next.InputText) ||
		!optionalTextEqual(link.InputHTML, next.InputHTML) ||
		!sameStringSet(link.InputImages, next.InputImages) {
		return true
	}
	// An omitted note means "leave my existing note alone". The extension
	// omits metadata.note for an empty input, so treating nil as a clear would
	// silently erase notes during a context-menu capture.
	return next.Description != nil && !optionalTextEqual(link.Description, next.Description)
}

func sameStringSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	for _, value := range left {
		leftSet[value] = struct{}{}
	}
	rightSet := make(map[string]struct{}, len(right))
	for _, value := range right {
		rightSet[value] = struct{}{}
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for value := range leftSet {
		if _, ok := rightSet[value]; !ok {
			return false
		}
	}
	return true
}

func captureSourceFingerprint(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	fingerprint, _ := metadata[captureSourceFingerprintMetadataKey].(string)
	return strings.TrimSpace(fingerprint)
}

func optionalTextEqual(left, right *string) bool {
	var leftValue, rightValue string
	if left != nil {
		leftValue = strings.TrimSpace(*left)
	}
	if right != nil {
		rightValue = strings.TrimSpace(*right)
	}
	return leftValue == rightValue
}

// NewIngestService takes the same repository / queue dependencies
// SubmitService uses; production wires both services from the same reader,
// durable command adapter, job store, and locker. Tests use an in-memory
// adapter at that same seam.
//
// Boot wiring should call NewLinkServices instead so SubmitService and
// IngestService share a single *linkSubmitter core (Wave 13 M-1). This
// constructor stays as a thin wrapper because submit_helpers_test.go and a
// few other tests build IngestService in isolation.
func NewIngestService(reader linkSubmissionReader, jobs repository.JobStore, commands LinkSubmissionCommands, locker URLLocker) *IngestService {
	_, ingest := NewLinkServices(reader, jobs, commands, locker, SubmitServiceOptions{})
	return ingest
}

// Ingest 处理一次多模态入库请求：先把 IngestRequest 规范化为统一的内部结构，
// 然后按 source_key（必要时再用真实 URL）去重判断是新建还是命中已有链接，
// 整个查询 + 写入过程被 URL/source_key 互斥锁串行化，避免同一来源并发产生两条记录。
func (s *IngestService) Ingest(ctx context.Context, req dto.IngestRequest) (dto.SubmitResponse, error) {
	normalized, err := normalizeIngestRequest(req, s.core.defaultCaptureDestination())
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	params, err := normalized.toLinkCapture()
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	if err := requireSiteLibraryWrite(params.RequestedLibraryKind, s.core.disableSiteLibraryWrite); err != nil {
		return dto.SubmitResponse{}, err
	}
	lockKey := normalized.lockURL
	if lockKey == "" {
		lockKey = normalized.sourceKey
	}
	if params.Destination == captureDestinationInbox {
		return s.submitToInbox(ctx, lockKey, params)
	}

	var resp dto.SubmitResponse
	err = s.core.locker.WithURL(ctx, lockKey, func(lockCtx context.Context) error {
		// Single round trip: source_key UNIQUE matches first, then a
		// secondary URL match catches the rare case where the link was
		// originally submitted via /api/links and is now being touched
		// via /api/ingest. The repository falls back to the
		// source_key-only query when realURL is empty.
		projection, err := s.core.reader.GetParseInputBySourceKeyOrURL(lockCtx, normalized.sourceKey, normalized.realURL)
		if err != nil {
			return err
		}

		if projection == nil {
			resp, err = s.core.createNewLink(lockCtx, params)
			return err
		}
		link := submitCandidateFromParseInput(projection)
		if normalized.sourceKind == "url" && link.Status != model.LinkStatusSkeleton {
			// URL-only ingest carries no capture revision, so it must never replace
			// persisted browser/multimodal input. It still goes through the shared
			// existing-row reconciliation so a concrete partition selection can be
			// committed while the current parse attempt is reused.
			if params.RequestedLibraryKind == model.RequestedLibraryKindAuto {
				resp, err = s.core.reuseExisting(lockCtx, link)
			} else {
				resp, err = s.core.submitExisting(lockCtx, link, &params)
			}
			return err
		}
		if submitCaptureChanged(link, params) {
			resp, err = s.core.requeueExisting(lockCtx, link.ID, &params)
			return err
		}

		resp, err = s.core.submitExisting(lockCtx, link, &params)
		return err
	})
	if err == nil && normalized.destination == captureDestinationSite {
		resp.Destination = captureDestinationSite
	}
	return resp, err
}

func (s *IngestService) submitToInbox(ctx context.Context, lockKey string, capture LinkCapture) (dto.SubmitResponse, error) {
	var response dto.SubmitResponse
	err := s.core.locker.WithURL(ctx, lockKey, func(lockCtx context.Context) error {
		var err error
		response, err = s.core.createInbox(lockCtx, capture)
		return err
	})
	return response, err
}
