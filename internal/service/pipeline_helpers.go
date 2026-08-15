package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
	"webtag/internal/service/urlmeta"
	"webtag/internal/textutil"
)

// escalationMinBodyChars mirrors the fetcher Manager's defaultMinBodyChars
// (internal/fetcher/manager.go) — the deep-fetch threshold below which a body
// is considered too thin. The escalation ladder (escalateIfThin) uses it as
// the body-length leg of its thin判定 so a light result that produced a body
// but stayed under the full-mode floor still triggers a deep re-fetch. Kept as
// a service-local const because the fetcher field is unexported; the two are
// expected to move together if the production floor ever changes.
const escalationMinBodyChars = 200

const (
	escalationOutcomeRecovered = "recovered"
	escalationOutcomeStillThin = "still_thin"
)

// isThinContent reports whether a fetched Content is "thin" for the purposes
// of the Phase 10 escalation ladder. It reuses the existing thin signals the
// pipeline already trusts:
//
//   - fetcher_type carries the "+thin" suffix — the canonical marker the
//     Manager / LightFetcher attach when they returned a result but had to fall
//     back to a title-only / truncated body (manager.go appends "+thin"); this
//     is also exactly what evaluateLowConfidence maps to low_confidence_reason
//     = thin_content.
//   - the trimmed body is shorter than escalationMinBodyChars runes (including
//     an empty body) — covers light results that produced *some* body but stayed
//     under the full-mode floor without the fetcher tagging "+thin".
//
// It does NOT treat search_fallback / fetch_quality / title_quality as thin:
// those are separate low-confidence reasons the escalation ladder is not
// responsible for (a deep re-fetch would not help a generic-title page).
func isThinContent(content fetcher.Content) bool {
	if strings.Contains(strings.ToLower(strings.TrimSpace(content.FetcherType)), "thin") {
		return true
	}
	body := strings.TrimSpace(content.Body)
	return textutil.Count(body) < escalationMinBodyChars
}

// isExplicitLight reports whether a link explicitly pinned the light/省 token
// fetch path via SourceMetadata["parse_depth"] == "light" (case-insensitive,
// whitespace-trimmed). The escalation ladder respects this as "user intent":
// a link that asked for light stays light even when the result is thin. Only
// PreferLight-by-default runs (no explicit parse_depth) are eligible for an
// automatic deep re-fetch. Mirrors the parse_depth parsing in shouldUseLight so
// the two read the metadata identically.
func isExplicitLight(link *model.Link) bool {
	if link == nil {
		return false
	}
	return isExplicitLightMetadata(link.SourceMetadata)
}

func isExplicitLightMetadata(metadata map[string]any) bool {
	if len(metadata) == 0 {
		return false
	}
	raw, ok := metadata["parse_depth"]
	if !ok {
		return false
	}
	v, isStr := raw.(string)
	if !isStr {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(v), "light")
}

func evaluateLowConfidence(content fetcher.Content, title string, titleReliable bool) *string {
	fetcherType := strings.ToLower(strings.TrimSpace(content.FetcherType))
	switch {
	case strings.Contains(fetcherType, "search"):
		return stringPtr(lowConfidenceReasonSearchFallback)
	case strings.Contains(fetcherType, "thin"):
		return stringPtr(lowConfidenceReasonThinContent)
	}

	if !titleReliable || textutil.IsGenericTitle(title) {
		return stringPtr(lowConfidenceReasonTitleQuality)
	}

	body := normalizePipelineSpace(content.Body)
	if body == "" {
		return stringPtr(lowConfidenceReasonFetchQuality)
	}
	if qualitySignal, ok := content.Metadata["quality_signal"].(string); ok && strings.EqualFold(strings.TrimSpace(qualitySignal), "weak") {
		return stringPtr(lowConfidenceReasonFetchQuality)
	}

	return nil
}

func normalizeAnalysisTitle(content fetcher.Content) string {
	title := normalizePipelineSpace(content.Title)
	if !textutil.IsGenericTitle(title) {
		return title
	}

	if heading, ok := content.Metadata["best_title"].(string); ok && !textutil.IsGenericTitle(heading) {
		return normalizePipelineSpace(heading)
	}

	if searchTitle, ok := content.Metadata["search_title"].(string); ok && !textutil.IsGenericTitle(searchTitle) {
		return normalizePipelineSpace(searchTitle)
	}

	return title
}

func normalizePipelineSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func normalizeMetricLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

// ensureParent walks the URL's ancestor chain (root -> leaf), but no longer
// auto-creates missing placeholder rows. Instead it returns the nearest
// already-existing ancestor link so parsed links keep a real parent/child
// relation when the user explicitly saved both URLs, while standalone links
// simply stay as roots. This removes the previous skeleton-row fan-out from
// the ingest path and keeps the links table limited to user-visible records.
func ensureParent(ctx context.Context, tree AncestorLinkLookup, metrics *observability.Metrics, rawURL string) (*uuid.UUID, error) {
	ancestors := urlmeta.AncestorURLs(rawURL, 32)
	// Record the depth before any DB work so the histogram captures
	// every ingest, including those that fail later in the function.
	// The histogram still tracks how deep real-world URLs are even
	// though the pipeline no longer auto-materializes every level.
	if metrics != nil {
		metrics.EnsureParentPathDepth.Observe(float64(len(ancestors)))
	}
	if len(ancestors) == 0 || tree == nil {
		return nil, nil
	}

	existing, err := tree.LookupByURLs(ctx, ancestors)
	if err != nil {
		return nil, err
	}

	for i := len(ancestors) - 1; i >= 0; i-- {
		current, ok := existing[ancestors[i]]
		if !ok || current == nil {
			continue
		}
		parentCopy := current.ID
		return &parentCopy, nil
	}

	return nil, nil
}

func isParseInputIngestSource(link *repository.LinkParseInput) bool {
	if link == nil {
		return false
	}
	return isIngestSourceFields(link.SourceKind, link.InputText, link.InputHTML)
}

func isIngestSourceFields(sourceKind string, inputText, inputHTML *string) bool {
	switch strings.TrimSpace(strings.ToLower(sourceKind)) {
	case "", "url":
		return false
	case "rss":
		// Thin feeds often carry only a title or teaser. RSS links retain their
		// provenance in source_kind, but without a stored body they must fall back
		// to the ordinary SSRF-hardened page fetch before AI analysis.
		return inputText != nil && strings.TrimSpace(*inputText) != "" ||
			inputHTML != nil && strings.TrimSpace(*inputHTML) != ""
	}
	return true
}

// ingestContent maps a stored ingest link onto a fetcher.Content the analyzer
// can consume. Body concatenates input_text and input_html so the analyzer's
// existing rune-budget truncation still applies; ImageURLs is forwarded
// verbatim so vision-capable models can attach the screenshots / image set.
// FetcherType="ingest" is intentionally distinct from "basic" / "search" so
// metrics and low-confidence heuristics can recognize the path.
func ingestContent(link model.Link) fetcher.Content {
	return buildIngestContent(
		link.URL, link.SourceKind, link.InputTitle, link.InputText, link.InputHTML,
		link.InputImages, link.SourceMetadata,
	)
}

func parseInputContent(link repository.LinkParseInput) fetcher.Content {
	return buildIngestContent(
		link.URL, link.SourceKind, link.InputTitle, link.InputText, link.InputHTML,
		link.InputImages, link.SourceMetadata,
	)
}

func buildIngestContent(url, sourceKind string, inputTitle, inputText, inputHTML *string, inputImages []string, sourceMetadata map[string]any) fetcher.Content {
	title := ""
	if inputTitle != nil {
		title = strings.TrimSpace(*inputTitle)
	}

	body := ""
	if inputText != nil {
		if text := strings.TrimSpace(*inputText); text != "" {
			body = text
		}
	}
	htmlBody := ""
	if inputHTML != nil {
		htmlBody = strings.TrimSpace(*inputHTML)
	}
	if body == "" {
		body = htmlBody
	}

	images := append([]string(nil), inputImages...)

	// Pre-size the metadata map for len(SourceMetadata) + 1 (the
	// source_kind insertion below). Skips the implicit rehash that
	// would otherwise fire on first insert. We still clone rather than
	// mutate link.SourceMetadata in place: the caller owns that map and
	// reuses it across pipeline stages, so a downstream side-effect
	// would surface as flaky tag/metadata fields on the persisted link.
	metadata := make(map[string]any, len(sourceMetadata)+1)
	for k, v := range sourceMetadata {
		metadata[k] = v
	}
	metadata["source_kind"] = sourceKind

	return fetcher.Content{
		URL:         url,
		Title:       title,
		Body:        body,
		SourceKind:  sourceKind,
		HTML:        htmlBody,
		ImageURLs:   images,
		Metadata:    metadata,
		FetcherType: "ingest",
	}
}

func stringPtr(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	copied := value
	return &copied
}

func intPtr(value int) *int {
	copied := value
	return &copied
}

// firstNonBlank 返回第一个去空白后非空的候选，全空时返回空串。
// 用于把多级兜底写成一行，避免层层嵌套的 if。
func firstNonBlank(candidates ...string) string {
	for _, c := range candidates {
		if strings.TrimSpace(c) != "" {
			return c
		}
	}
	return ""
}
