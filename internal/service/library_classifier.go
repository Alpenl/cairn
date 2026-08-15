package service

import (
	"strings"

	"webtag/internal/model"
	"webtag/internal/service/urlmeta"
)

const libraryClassifierVersion = "library-v1"

// CaptureSignals is the bounded, body-free subset of capture metadata used by
// the deterministic classification pass. It intentionally excludes page text,
// HTML, form values, and arbitrary JSON-LD payloads.
type CaptureSignals struct {
	OGType                string
	JSONLDTypes           []string
	HasApplicationName    bool
	HasWebAppManifest     bool
	HasAuthorAndPublished bool
	ProseDominant         bool
	InteractiveDominant   bool
	NavigationDominant    bool
	PlatformIdentity      bool
	SiteDescription       bool
	DocumentSource        bool
}

// ClassificationInput contains only the information needed before an optional
// analyzer resolution. ExistingDecision represents a previously finalized
// link and prevents a duplicate capture from changing its partition.
type ClassificationInput struct {
	RequestedKind    model.RequestedLibraryKind
	SourceKind       string
	URL              string
	URLMetadata      urlmeta.URLMetadata
	CaptureSignals   CaptureSignals
	ExistingDecision *ClassificationDecision
}

type ClassificationDecision struct {
	Kind              model.LibraryKind
	Source            model.LibraryKindSource
	Locked            bool
	Confidence        float32
	Reason            string
	Explanation       string
	Evidence          []string
	ClassifierVersion string
	NeedsReview       bool
	Ambiguous         bool
}

// ClassifyLibrary applies the non-LLM portion of the confirmed priority
// order. Ambiguous decisions deliberately have no final Kind: callers use the
// existing analyzer call to resolve them, and a failed/invalid resolution then
// defaults to site with a review item.
func ClassifyLibrary(in ClassificationInput) ClassificationDecision {
	if in.ExistingDecision != nil && in.ExistingDecision.Kind != "" {
		decision := *in.ExistingDecision
		decision.Reason = "existing_final_decision"
		decision.ClassifierVersion = libraryClassifierVersion
		return decision
	}

	switch in.RequestedKind {
	case model.RequestedLibraryKindReading:
		return fixedLibraryDecision(model.LibraryKindReading, "explicit_reading", model.LibraryKindSourceUser, true)
	case model.RequestedLibraryKindSite:
		return fixedLibraryDecision(model.LibraryKindSite, "explicit_site", model.LibraryKindSourceUser, true)
	}

	source := strings.ToLower(strings.TrimSpace(in.SourceKind))
	if in.URL == "" && (source == "text" || source == "image") {
		return fixedLibraryDecision(model.LibraryKindReading, "unstable_ingest_source", model.LibraryKindSourceAuto, false)
	}
	if source == "feed_item" || source == "rss" {
		return fixedLibraryDecision(model.LibraryKindReading, "feed_reading_source", model.LibraryKindSourceAuto, false)
	}

	reading, site, evidence := scoreLibraryClassification(in)
	if reading >= site+4 && reading >= 5 {
		return scoredLibraryDecision(model.LibraryKindReading, reading, site, evidence)
	}
	if site >= reading+4 && site >= 5 {
		return scoredLibraryDecision(model.LibraryKindSite, site, reading, evidence)
	}
	return ClassificationDecision{
		Reason:            "ambiguous_features",
		Evidence:          evidence,
		ClassifierVersion: libraryClassifierVersion,
		NeedsReview:       true,
		Ambiguous:         true,
	}
}

func fixedLibraryDecision(kind model.LibraryKind, reason string, source model.LibraryKindSource, locked bool) ClassificationDecision {
	return ClassificationDecision{
		Kind:              kind,
		Source:            source,
		Locked:            locked,
		Confidence:        1,
		Reason:            reason,
		ClassifierVersion: libraryClassifierVersion,
	}
}

func scoredLibraryDecision(kind model.LibraryKind, winner, loser int, evidence []string) ClassificationDecision {
	confidence := float32(0.72) + float32(min(winner-loser, 20))*0.014
	if confidence > 0.95 {
		confidence = 0.95
	}
	return ClassificationDecision{
		Kind:              kind,
		Source:            model.LibraryKindSourceAuto,
		Confidence:        confidence,
		Reason:            evidence[0],
		Evidence:          evidence,
		ClassifierVersion: libraryClassifierVersion,
	}
}

func scoreLibraryClassification(in ClassificationInput) (reading, site int, evidence []string) { //nolint:gocyclo // 打分表：每个信号一个分支，这就是规则本身
	addReading := func(score int, reason string) { reading += score; evidence = append(evidence, reason) }
	addSite := func(score int, reason string) { site += score; evidence = append(evidence, reason) }
	for _, raw := range in.CaptureSignals.JSONLDTypes {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "article", "blogposting", "newsarticle", "techarticle":
			addReading(6, "jsonld_article")
		case "website", "softwareapplication", "webapplication", "organization", "product":
			addSite(6, "jsonld_site_app")
		}
	}
	if strings.EqualFold(strings.TrimSpace(in.CaptureSignals.OGType), "article") {
		addReading(4, "og_article")
	}
	if in.CaptureSignals.HasAuthorAndPublished {
		addReading(2, "authored_dated_content")
	}
	if in.CaptureSignals.ProseDominant {
		addReading(3, "prose_dominant")
	}
	if in.CaptureSignals.DocumentSource {
		addReading(5, "document_source")
	}
	if in.URLMetadata.ContentType == model.ContentTypeArticle && urlmeta.HasArticleParentSegment(in.URLMetadata.ParentPath) {
		addReading(3, "article_path")
	}
	if in.URLMetadata.PathDepth == 0 {
		addSite(3, "root_or_locale_path")
	}
	if in.CaptureSignals.HasApplicationName || in.CaptureSignals.HasWebAppManifest {
		addSite(3, "application_metadata")
	}
	if in.CaptureSignals.InteractiveDominant {
		addSite(3, "interactive_dominant")
	}
	if in.CaptureSignals.NavigationDominant {
		addSite(2, "navigation_dominant")
	}
	if in.CaptureSignals.PlatformIdentity {
		addSite(4, "platform_identity")
	}
	if in.CaptureSignals.SiteDescription {
		addSite(2, "site_description")
	}
	return reading, site, evidence
}
