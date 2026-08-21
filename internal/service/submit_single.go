package service

import (
	"context"
	"strings"

	"webtag/internal/dto"
	"webtag/internal/model"
)

// Submit 处理单条 URL 提交：校验 URL → URL 锁内查重 → 不存在则建链 + 入队，
// 已存在则交给 submitExisting 复用当前状态；重试只由 Refresh 发起。URL 锁是必要的：
// 两个浏览器标签同时 POST 同一 URL 必须只产生一条记录。
func (s *SubmitService) Submit(ctx context.Context, req dto.LinkCreateRequest) (dto.SubmitResponse, error) {
	identityURL, err := validateURL(req.URL)
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	if err := validateLinkDescription(req.Description); err != nil {
		return dto.SubmitResponse{}, err
	}
	requestedKind, err := normalizeRequestedLibraryKind(req.RequestedLibraryKind)
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	destination, err := normalizeCaptureDestination(req.Destination, captureDestinationInbox)
	if err != nil {
		return dto.SubmitResponse{}, err
	}
	params := LinkCapture{
		URL:                     strings.TrimSpace(req.URL),
		Destination:             destination,
		SourceKey:               identityURL,
		Description:             req.Description,
		Status:                  model.LinkStatusPending,
		SourceMetadata:          withOriginalURL(nil, req.URL, identityURL),
		RequestedLibraryKind:    requestedKind,
		UserSelectedLibraryKind: requestedKind != model.RequestedLibraryKindAuto,
	}
	if destination == captureDestinationInbox {
		return s.submitToInbox(ctx, identityURL, params)
	}

	var resp dto.SubmitResponse
	err = s.core.locker.WithURL(ctx, identityURL, func(lockCtx context.Context) error {
		projection, err := s.core.reader.GetSubmitLookupByURL(lockCtx, identityURL)
		if err != nil {
			return err
		}

		if projection == nil {
			resp, err = s.core.createNewLink(lockCtx, params)
			return err
		}

		resp, err = s.core.submitExisting(lockCtx, submitCandidateFromLookup(projection), &params)
		return err
	})
	return resp, err
}

func (s *SubmitService) submitToInbox(ctx context.Context, rawURL string, capture LinkCapture) (dto.SubmitResponse, error) {
	var response dto.SubmitResponse
	err := s.core.locker.WithURL(ctx, rawURL, func(lockCtx context.Context) error {
		var err error
		response, err = s.core.createInbox(lockCtx, capture)
		return err
	})
	return response, err
}
