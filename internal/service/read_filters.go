package service

import (
	"strings"
	"time"

	"webtag/internal/model"
	"webtag/internal/problem"
)

func validateCreatedRange(rawFrom, rawBefore string) (*time.Time, *time.Time, error) {
	fromValue := strings.TrimSpace(rawFrom)
	beforeValue := strings.TrimSpace(rawBefore)
	if fromValue == "" && beforeValue == "" {
		return nil, nil, nil
	}
	if fromValue == "" || beforeValue == "" {
		return nil, nil, problem.NewWithCode(problem.Invalid, problem.CodeInvalidCreatedRange, "created_from and created_before must be provided together")
	}

	from, err := time.Parse(time.RFC3339, fromValue)
	if err != nil {
		return nil, nil, problem.NewWithCode(problem.Invalid, problem.CodeInvalidCreatedRange, "created_from and created_before must be RFC3339 timestamps")
	}
	before, err := time.Parse(time.RFC3339, beforeValue)
	if err != nil {
		return nil, nil, problem.NewWithCode(problem.Invalid, problem.CodeInvalidCreatedRange, "created_from and created_before must be RFC3339 timestamps")
	}
	if !from.Before(before) {
		return nil, nil, problem.NewWithCode(problem.Invalid, problem.CodeInvalidCreatedRange, "created_from must be earlier than created_before")
	}
	return &from, &before, nil
}

// splitTags 把逗号分隔的标签串拆成去空白的 []string，全空返回 nil。
func splitTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// splitAndValidateTags is the public-input variant of splitTags; it
// rejects oversized inputs before the slice flows into the SQL ANY().
func splitAndValidateTags(raw string) ([]string, error) {
	tags := splitTags(raw)
	if len(tags) > maxListTagFilters {
		return nil, problem.NewWithCode(problem.Invalid, problem.CodeTagFiltersExceedLimit, "too many tag filters")
	}
	for _, tag := range tags {
		if len(tag) > maxListTagFilterLen {
			return nil, problem.NewWithCode(problem.Invalid, problem.CodeTagFilterTooLong, "tag filter too long")
		}
	}
	return tags, nil
}

// validateContentTypeFilter 拒绝枚举外的 content_type，避免 OpenAPI 与
// 实现脱节后客户端拿到沉默的空结果。
func validateContentTypeFilter(value string) error {
	if value == "" {
		return nil
	}
	if _, ok := allowedContentTypes[value]; !ok {
		return problem.NewWithCode(problem.Invalid, problem.CodeUnsupportedContentTypeFilter, "unsupported content_type filter")
	}
	return nil
}

func validateLibraryKindFilter(value string) (*model.LibraryKind, error) {
	if value == "" {
		return nil, nil
	}
	kind := model.LibraryKind(value)
	if kind != model.LibraryKindReading && kind != model.LibraryKindSite {
		return nil, problem.NewWithCode(problem.Invalid, problem.CodeLibraryKindNotFinal, "unsupported library_kind filter")
	}
	return &kind, nil
}

// validateDomainFilter 拦截超长 domain 过滤值，防止恶意 URL 参数撑爆请求。
func validateDomainFilter(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > maxListDomainLen {
		return problem.NewWithCode(problem.Invalid, problem.CodeDomainFilterTooLong, "domain filter too long")
	}
	return nil
}

// splitAndValidateStatuses parses the comma-separated ?status= filter into a
// validated, de-duplicated status set. The contract:
//   - empty / whitespace-only input → every user-visible enrichment state;
//   - every token must be one of pending/processing/failed/done — any other
//     value yields a 400 with a readable message naming the offender;
//   - duplicate tokens collapse to one (?status=done,done → ["done"]);
//   - more than maxListStatusFilters tokens is rejected as abuse.
//
// Order of the returned slice follows first-seen order in the query string so
// the generated SQL args are deterministic for tests.
func splitAndValidateStatuses(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return append([]string(nil), defaultVisibleStatuses...), nil
	}

	parts := strings.Split(raw, ",")
	if len(parts) > maxListStatusFilters {
		return nil, problem.NewWithCode(problem.Malformed, problem.CodeUnsupportedStatusFilter, "too many status filters")
	}

	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		status := strings.TrimSpace(part)
		if status == "" {
			continue
		}
		if _, ok := allowedListStatuses[status]; !ok {
			return nil, problem.NewWithCode(problem.Malformed, problem.CodeUnsupportedStatusFilter, "unsupported status filter: "+status+" (allowed: pending, processing, failed, done)")
		}
		if _, dup := seen[status]; dup {
			continue
		}
		seen[status] = struct{}{}
		out = append(out, status)
	}
	if len(out) == 0 {
		return append([]string(nil), defaultVisibleStatuses...), nil
	}
	return out, nil
}

// validateLowConfidenceFilter 解析 ?low_confidence=。空 = 不过滤；只认
// "true" / "false"。
//
// 其它值报 422 而不是当作未提供：一个拼错的筛选参数如果被静默忽略，返回的是
// 未过滤的全量列表——而调用方（Reader 的复核视图）会把它当成筛选结果显示，
// 于是「待复核」看起来永远是满的，或者永远是空的，取决于它怎么渲染。
func validateLowConfidenceFilter(value string) (*bool, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "":
		return nil, nil
	case "true":
		v := true
		return &v, nil
	case "false":
		v := false
		return &v, nil
	default:
		return nil, problem.NewWithCode(problem.Invalid, problem.CodeUnsupportedLowConfidenceFilter, "low_confidence must be true or false")
	}
}
