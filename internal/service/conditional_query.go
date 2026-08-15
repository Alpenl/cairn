package service

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NormalizeLinkListConditionalQuery returns the canonical query consumed by
// GET /api/links when its body is a deterministic database projection. Search
// and cursor variants remain ineligible because they depend on an external
// embedder or a service-instance cursor signing key, respectively.
//
//nolint:gocyclo // each branch mirrors one public query field or non-deterministic variant; keeping the grammar together prevents handler/policy normalization drift.
func NormalizeLinkListConditionalQuery(values url.Values) (string, bool) {
	createdFrom, createdBefore, err := validateCreatedRange(values.Get("created_from"), values.Get("created_before"))
	if err != nil {
		return "", false
	}
	if rawURL := strings.TrimSpace(values.Get("url")); rawURL != "" {
		normalized, err := validateURL(rawURL)
		if err != nil {
			return "", false
		}
		return url.Values{"url": {normalized}}.Encode(), true
	}
	if strings.TrimSpace(values.Get("q")) != "" {
		return "", false
	}
	if _, cursorMode := values["after"]; cursorMode {
		return "", false
	}

	tags, err := splitAndValidateTags(values.Get("tags"))
	if err != nil {
		return "", false
	}
	if err := validateContentTypeFilter(values.Get("content_type")); err != nil {
		return "", false
	}
	libraryKind, err := validateLibraryKindFilter(values.Get("library_kind"))
	if err != nil {
		return "", false
	}
	if err := validateDomainFilter(values.Get("domain")); err != nil {
		return "", false
	}
	lowConfidence, err := validateLowConfidenceFilter(values.Get("low_confidence"))
	if err != nil {
		return "", false
	}
	statuses, err := splitAndValidateStatuses(values.Get("status"))
	if err != nil {
		return "", false
	}

	canonical := url.Values{}
	if len(tags) > 0 {
		slices.Sort(tags)
		tags = slices.Compact(tags)
		canonical.Set("tags", strings.Join(tags, ","))
	}
	if value := values.Get("content_type"); value != "" {
		canonical.Set("content_type", value)
	}
	if libraryKind != nil {
		canonical.Set("library_kind", string(*libraryKind))
	}
	if value := values.Get("domain"); value != "" {
		canonical.Set("domain", value)
	}
	if lowConfidence != nil {
		canonical.Set("low_confidence", strconv.FormatBool(*lowConfidence))
	}
	if strings.TrimSpace(values.Get("status")) != "" {
		slices.Sort(statuses)
		canonical.Set("status", strings.Join(statuses, ","))
	}
	if createdFrom != nil && createdBefore != nil {
		canonical.Set("created_from", createdFrom.UTC().Format(time.RFC3339Nano))
		canonical.Set("created_before", createdBefore.UTC().Format(time.RFC3339Nano))
	}
	page := normalizedPolicyInt(values.Get("page"), 1, 1, 0)
	limit := normalizedPolicyInt(values.Get("limit"), 20, 1, 100)
	if page != 1 {
		canonical.Set("page", strconv.Itoa(page))
	}
	if limit != 20 {
		canonical.Set("limit", strconv.Itoa(limit))
	}
	return canonical.Encode(), true
}

// NormalizeSiteListConditionalQuery canonicalizes the database-only variants
// of GET /api/sites and rejects the same unsupported views as SiteReadService.
func NormalizeSiteListConditionalQuery(values url.Values) (string, bool) {
	view := strings.TrimSpace(values.Get("view"))
	if view == "" {
		view = "all"
	}
	switch view {
	case "all", "pinned", "recent", "review":
	default:
		return "", false
	}
	tags := splitTags(values.Get("tags"))
	slices.Sort(tags)
	tags = slices.Compact(tags)
	page := normalizedPolicyInt(values.Get("page"), 1, 1, 0)
	limit := normalizedPolicyInt(values.Get("limit"), 30, 1, 100)
	recentCutoff := strings.TrimSpace(values.Get("recent_cutoff"))
	if view == "recent" {
		// Page one receives a request-time cutoff from the handler, so it cannot
		// share a strong validator with another request instant. A supplied page
		// one cutoff is also left to the service so it can return the contract's
		// explicit validation error instead of being short-circuited by a 304.
		if page == 1 {
			return "", false
		}
		if recentCutoff == "" {
			return "", false
		}
		parsed, err := time.Parse(time.RFC3339Nano, recentCutoff)
		if err != nil {
			return "", false
		}
		recentCutoff = parsed.UTC().Format(time.RFC3339Nano)
	} else if recentCutoff != "" {
		return "", false
	}

	canonical := url.Values{}
	if view != "all" {
		canonical.Set("view", view)
	}
	if len(tags) > 0 {
		canonical.Set("tags", strings.Join(tags, ","))
	}
	if recentCutoff != "" {
		canonical.Set("recent_cutoff", recentCutoff)
	}
	if page != 1 {
		canonical.Set("page", strconv.Itoa(page))
	}
	if limit != 30 {
		canonical.Set("limit", strconv.Itoa(limit))
	}
	return canonical.Encode(), true
}

// NormalizeFeedItemsConditionalQuery canonicalizes the filter accepted by
// GET /api/feed-items. UUIDs, view, query length, and pagination are validated
// before the conditional middleware reads any representation revision.
func NormalizeFeedItemsConditionalQuery(values url.Values) (string, bool) {
	filter := FeedItemFilter{
		View:  values.Get("view"),
		Query: values.Get("q"),
		Page:  normalizedPolicyInt(values.Get("page"), 1, 0, 0),
		Limit: normalizedPolicyInt(values.Get("limit"), 20, 0, 0),
	}
	if raw := strings.TrimSpace(values.Get("subscription_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return "", false
		}
		filter.SubscriptionID = &id
	}
	if raw := strings.TrimSpace(values.Get("folder_id")); strings.EqualFold(raw, "ungrouped") {
		filter.Ungrouped = true
	} else if raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return "", false
		}
		filter.FolderID = &id
	}
	normalized, err := normalizeFeedItemFilter(filter)
	if err != nil {
		return "", false
	}

	canonical := url.Values{}
	if normalized.View != "all" {
		canonical.Set("view", normalized.View)
	}
	if normalized.SubscriptionID != nil {
		canonical.Set("subscription_id", normalized.SubscriptionID.String())
	}
	if normalized.Ungrouped {
		canonical.Set("folder_id", "ungrouped")
	} else if normalized.FolderID != nil {
		canonical.Set("folder_id", normalized.FolderID.String())
	}
	if normalized.Query != "" {
		canonical.Set("q", normalized.Query)
	}
	if normalized.Page != 1 {
		canonical.Set("page", strconv.Itoa(normalized.Page))
	}
	if normalized.Limit != defaultFeedPageLimit {
		canonical.Set("limit", strconv.Itoa(normalized.Limit))
	}
	return canonical.Encode(), true
}

func normalizedPolicyInt(raw string, fallback, minimum, maximum int) int {
	value := fallback
	if raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			value = parsed
		}
	}
	if minimum > 0 && value < minimum {
		value = minimum
	}
	if maximum > 0 && value > maximum {
		value = maximum
	}
	return value
}
