package service

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service/urlmeta"
	"webtag/internal/textutil"
)

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
// simply stay as roots. Missing ancestors are never materialized as rows, so
// the links table stays limited to user-visible records.
func ensureParent(ctx context.Context, tree AncestorLinkLookup, rawURL string) (*uuid.UUID, error) {
	ancestors := urlmeta.AncestorURLs(rawURL, 32)
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
