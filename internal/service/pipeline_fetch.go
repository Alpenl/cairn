package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/fetcher"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

// acquireContent returns the content the analyzer should consume, plus a
// flag reporting whether the content came off the light/省 token path. The
// flag drives the Phase 10 escalation ladder: only runs that actually went
// light are candidates for an automatic deep re-fetch when the light result
// is thin (see escalateIfThin).
//
// Multimodal ingest paths (text / image / browser_capture / multimodal)
// already carry the fetched payload in link.Input* and the URL is a
// synthetic webtag://ingest/* placeholder. Calling the fetcher here would
// either fail with "unsupported scheme" or, for browser_capture links
// that include a real URL, refetch and discard the captured HTML /
// screenshots — which silently drops the multimodal context. So
// isIngestSource short-circuits to ingestContent and only "url"-kind
// links go through the fetcher. Ingest content is never "light" (tookLight
// is false) so the escalation ladder never re-fetches it.
func (p *ParsePipeline) acquireContent(ctx context.Context, link *repository.LinkParseInput) (content fetcher.Content, tookLight bool, err error) {
	if isParseInputIngestSource(link) {
		return parseInputContent(*link), false, nil
	}
	if p.shouldUseLight(link) {
		if lf, ok := p.fetcher.(LightContentFetcher); ok {
			content, err = lf.FetchLight(ctx, link.URL)
			return content, err == nil, err
		}
		// Fetcher doesn't implement the light interface — fall through
		// to the deep path so the run still completes. Logged at debug
		// so operators investigating "why isn't light kicking in?" can
		// see the type assertion failure once at startup-time level.
		if p.logger != nil {
			p.logger.Debug("pipeline.acquireContent: light requested but fetcher does not implement LightContentFetcher, using Fetch", "fetcher_type", fmt.Sprintf("%T", p.fetcher))
		}
	}
	content, err = p.fetcher.Fetch(ctx, link.URL)
	return content, false, err
}

// escalateIfThin is the Phase 10 low-confidence escalation ladder. When a
// run took the light/省 token path AND the light result is thin AND the
// link did not explicitly opt into light, it re-fetches the same URL once
// through the full deep Fetch (three-source fallback) and returns the deep
// result in place of the light one. Each link escalates AT MOST once — the
// caller invokes this exactly once per run, so even a still-thin deep result
// is never re-escalated.
//
// Guards (any false → return the light content untouched):
//   - tookLight: the run must have actually walked the light path. Deep runs
//     and ingest (tookLight always false) are never escalated.
//   - isThinContent(content): the light body must be judged thin (fetcher_type
//     carries the +thin suffix, or the body is below escalationMinBodyChars).
//   - !isExplicitLight(link): a link whose SourceMetadata["parse_depth"] is
//     explicitly "light" stays light — the user deliberately chose the cheap
//     path and we respect that. Only "PreferLight-by-default" runs escalate.
//
// On escalation it counts webtag_fetch_escalation_total (outcome=recovered
// when the deep result is no longer thin, still_thin otherwise) and logs the
// action at INFO. A deep Fetch error leaves the light content in place (the
// light result is still usable for tagging) and is logged at WARN — we never
// fail a parse just because the escalation re-fetch failed.
func (p *ParsePipeline) escalateIfThin(ctx context.Context, link *repository.LinkParseInput, content fetcher.Content, tookLight bool) fetcher.Content {
	if !tookLight || isExplicitLightMetadata(link.SourceMetadata) || !isThinContent(content) {
		return content
	}

	deep, err := p.fetcher.Fetch(ctx, link.URL)
	if err != nil {
		p.logWarn(ctx, "fetch escalation re-fetch failed; keeping light content",
			"link_id", link.ID.String(),
			"url_host", observability.SafeURLHost(link.URL),
			"err", observability.SafeError(err),
		)
		return content
	}

	outcome := escalationOutcomeRecovered
	if isThinContent(deep) {
		outcome = escalationOutcomeStillThin
	}
	p.recordFetchEscalation(outcome)
	p.logInfo(ctx, "fetch escalated from light to deep due to thin content",
		"link_id", link.ID.String(),
		"url_host", observability.SafeURLHost(link.URL),
		"light_fetcher_type", normalizeMetricLabel(content.FetcherType),
		"deep_fetcher_type", normalizeMetricLabel(deep.FetcherType),
		"outcome", outcome,
	)
	return deep
}

// attachConcepts 是 collectionFinalizer.attachConcepts 的薄委托，仅供
// pipeline_concept_test.go 直接驱动概念关联而不必构造整条 finalizer。
// 实现与语义（best-effort、按 kind/reason 记账）都在 pipeline_persist.go，
// 改动请去那里——这里只负责转发。
func (p *ParsePipeline) attachConcepts(ctx context.Context, linkID uuid.UUID, expectedMetadataRevision int64, tags []string) {
	p.collectionFinalizer().attachConcepts(ctx, linkID, expectedMetadataRevision, tags)
}

// shouldUseLight resolves the per-run depth choice. Order of precedence:
//  1. Per-link override via SourceMetadata["parse_depth"] — submit-time
//     callers can pin "deep" (force full body) or "light" (force
//     truncation) regardless of the global default.
//  2. The pipeline-level PreferLight switch (config-driven).
//
// Unknown / missing parse_depth values fall through to the pipeline
// default. Case-insensitive on the metadata value so callers can pass
// either "deep" or "Deep" without worrying about normalization.
func (p *ParsePipeline) shouldUseLight(link *repository.LinkParseInput) bool {
	if link != nil && len(link.SourceMetadata) > 0 {
		if raw, ok := link.SourceMetadata["parse_depth"]; ok {
			// metadata 来自 JSONB，类型只可能是 string 或被静默忽略。
			if v, isStr := raw.(string); isStr {
				switch strings.ToLower(strings.TrimSpace(v)) {
				case "light":
					return true
				case "deep", "full":
					return false
				}
			}
		}
	}
	return p.preferLight
}
