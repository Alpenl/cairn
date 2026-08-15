package service

import (
	"strings"

	"webtag/internal/errsafe"
	"webtag/internal/model"
)

// deriveLowConfidenceReason 给 LinkResponse 反推「低置信度原因」：优先取
// 显式存储的 LowConfidenceReason，其次根据 ErrorMsg / FetcherType 关键字
// 兜底归类。返回 nil 表示无须暴露此字段。
func deriveLowConfidenceReason(link model.Link) *string {
	return deriveLowConfidenceReasonFields(link.IsLowConfidence, link.LowConfidenceReason, link.ErrorMsg, link.FetcherType)
}

func deriveLowConfidenceReasonFields(isLowConfidence bool, lowConfidenceReason, errorMsg, fetcherTypeValue *string) *string {
	if !isLowConfidence {
		return nil
	}
	if lowConfidenceReason != nil && strings.TrimSpace(*lowConfidenceReason) != "" {
		return stringPtr(strings.TrimSpace(*lowConfidenceReason))
	}
	if errorMsg != nil {
		normalizedError := errsafe.NormalizedPointer(errorMsg)
		switch normalizedError {
		case "fetch_quality", "title_quality", "search_fallback", "thin_content":
			return stringPtr(strings.TrimSpace(*errorMsg))
		}
	}

	fetcherType := errsafe.NormalizedPointer(fetcherTypeValue)
	switch {
	case strings.Contains(fetcherType, "search"):
		return stringPtr("search_fallback")
	case strings.Contains(fetcherType, "thin"):
		return stringPtr("thin_content")
	case fetcherType != "":
		return stringPtr("fetch_quality")
	default:
		return stringPtr("unknown")
	}
}

// deriveLinkErrorCategory returns the public error_category for a link
// response. Done links get "" so the response wrapper (stringPtr →
// omitempty) skips the field entirely; surfacing the literal "none" in
// the JSON was clutter without information. Same treatment for
// pending/processing links with no ErrorMsg — ClassifyPersisted("")
// returns "none", which is equally noisy in those states.
//
// This is the read path: link.ErrorMsg is a persisted DB string with no
// live error chain attached, so ClassifyPersisted is the semantically
// correct entry point (substring matching against the stored message).
func deriveLinkErrorCategory(link model.Link) string {
	return deriveLinkErrorCategoryFields(link.Status, link.ErrorMsg)
}

func deriveLinkErrorCategoryFields(status model.LinkStatus, errorMsg *string) string {
	if status == model.LinkStatusDone {
		return ""
	}
	return omitNoneCategory(errsafe.ClassifyPersisted(derefOrEmpty(errorMsg)))
}

// deriveJobErrorCategory 是 deriveLinkErrorCategory 的任务行版本，逻辑一致。
func deriveJobErrorCategory(job model.ParseJob) string {
	if job.Status == model.JobStatusDone {
		return ""
	}
	return omitNoneCategory(errsafe.ClassifyPersisted(derefOrEmpty(job.ErrorMsg)))
}

// derefOrEmpty returns *s, or "" when s is nil. Local helper so the
// diagnostics call sites can stay one-liners against the new
// ClassifyPersisted entry point without duplicating the nil-check at
// every callsite.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// omitNoneCategory collapses the "none" sentinel into "" so the
// response wrapper skips error_category entirely whenever there is
// no actual error to surface. The errsafe layer keeps "none" as a
// valid category value for internal classification (callers can still
// branch on it); only the JSON output strips it.
func omitNoneCategory(category string) string {
	if category == "none" {
		return ""
	}
	return category
}
