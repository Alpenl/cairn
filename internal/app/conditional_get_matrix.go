package app

import (
	"net/url"
	"strings"

	"webtag/internal/middleware"
	"webtag/internal/model"
	"webtag/internal/representation"
	"webtag/internal/service"
)

// conditionalDependency records the database events that can change one
// response dependency. It is audit metadata, not the source of trigger SQL.
type conditionalDependency struct {
	Table   string
	Columns []string
	Events  []string
}

// conditionalRouteEligibility is the executable audit inventory for one
// normalized route variant. Production policy is deliberately assembled by a
// separate function and cross-checked in tests so an omitted matrix row cannot
// make the allowlist prove itself complete.
type conditionalRouteEligibility struct {
	Route                 string
	Variant               string
	Dependencies          []conditionalDependency
	ReadSideEffect        string
	Components            representation.ComponentSet
	CacheStrategy         string
	NondeterministicInput string
	Eligible              bool
	Decision              string
}

// conditionalGetEligibilityMatrix documents every former conditional route at
// route-and-variant granularity, including variants that intentionally remain
// non-cacheable. Returned rows and slices are newly allocated for the caller.
func conditionalGetEligibilityMatrix() []conditionalRouteEligibility {
	library := mustComponentSet(representation.LibraryComponent)
	libraryGlobal := mustComponentSet(representation.LibraryComponent, representation.GlobalComponent)
	libraryFeed := mustComponentSet(representation.LibraryComponent, representation.FeedComponent)
	linkResponseColumns := []string{
		"id", "url", "title", "summary", "tags", "fetcher_type",
		"is_low_confidence", "low_confidence_reason", "status", "error_msg",
		"description", "domain", "content_type", "library_kind",
		"library_kind_source", "library_kind_locked", "predicted_library_kind",
		"classification_confidence", "classification_reason",
		"classification_explanation", "classifier_version", "content_revision",
		"content", "content_cjk_chars", "content_words", "path_depth", "parent_path",
		"parent_id", "created_at", "updated_at",
	}
	linkConcept := dependency("link_concept", "link_id", "concept_id", "surface_tag")
	conceptDisplay := dependency("concept", "id", "primary_name", "display_name")
	linkDependencies := func(columns []string) []conditionalDependency {
		return []conditionalDependency{
			dependency("links", columns...),
			linkConcept,
			conceptDisplay,
		}
	}
	linkListDependencies := linkDependencies(linkResponseColumns)
	linkDetailColumns := append(append([]string(nil), linkResponseColumns...), "content_source")
	linkContentColumns := append(append([]string(nil), linkDetailColumns...), "content_document", "content_format")
	linkContentDependencies := linkDependencies(linkContentColumns)
	linkSearchColumns := append(append([]string(nil), linkResponseColumns...), "embedding", "embedding_model")
	linkSearchDependencies := linkDependencies(linkSearchColumns)
	return []conditionalRouteEligibility{
		{Route: "/api/links", Variant: "list", Dependencies: cloneDependencies(linkListDependencies), ReadSideEffect: "none", Components: libraryGlobal, CacheStrategy: "no body cache; delayed validator commit after concept lookup", NondeterministicInput: "none", Eligible: true, Decision: "database-only list filters are canonicalized before version lookup"},
		{Route: "/api/links", Variant: "url", Dependencies: cloneDependencies(linkListDependencies), ReadSideEffect: "none", Components: libraryGlobal, CacheStrategy: "no body cache; delayed validator commit after concept lookup", NondeterministicInput: "none", Eligible: true, Decision: "normalized exact URL lookup is deterministic and takes precedence over q"},
		{Route: "/api/links", Variant: "search", Dependencies: cloneDependencies(linkSearchDependencies), ReadSideEffect: "none", Components: libraryGlobal, CacheStrategy: "none", NondeterministicInput: "embedder availability and model generation", Eligible: false, Decision: "q additionally depends on external embedder health/model generation"},
		{Route: "/api/links", Variant: "cursor", Dependencies: cloneDependencies(linkListDependencies), ReadSideEffect: "none", Components: libraryGlobal, CacheStrategy: "none", NondeterministicInput: "service-instance cursor signing key", Eligible: false, Decision: "cursor validity depends on the service-instance signing key"},
		{Route: "/api/links/:link_id", Variant: "detail-content", Dependencies: cloneDependencies(linkContentDependencies), ReadSideEffect: "none", Components: libraryGlobal, CacheStrategy: "no body cache; existence, content and fail-soft checks precede validator commit", NondeterministicInput: "none", Eligible: true, Decision: "default detail includes the saved-content snapshot"},
		{Route: "/api/links/:link_id", Variant: "detail-metadata", Dependencies: cloneDependencies(linkDependencies(linkDetailColumns)), ReadSideEffect: "none", Components: libraryGlobal, CacheStrategy: "no body cache; existence and fail-soft checks precede validator commit", NondeterministicInput: "none", Eligible: true, Decision: "include_content=false excludes content_document and content_format but includes content_source"},
		{Route: "/api/tags", Variant: "default", Dependencies: []conditionalDependency{dependency("links", "status", "tags")}, ReadSideEffect: "none", Components: library, CacheStrategy: "version-keyed TagCache", NondeterministicInput: "none", Eligible: true, Decision: "legacy aggregate reads links only"},
		{Route: "/api/tags", Variant: "reading", Dependencies: []conditionalDependency{dependency("links", "status", "library_kind", "tags")}, ReadSideEffect: "none", Components: library, CacheStrategy: "version-keyed TagCache variant", NondeterministicInput: "none", Eligible: true, Decision: "reading aggregate reads links only"},
		{Route: "/api/tags", Variant: "site", Dependencies: []conditionalDependency{dependency("site_tags", "site_id", "normalized_tag")}, ReadSideEffect: "none", Components: library, CacheStrategy: "version-keyed TagCache variant", NondeterministicInput: "none", Eligible: true, Decision: "site aggregate reads site_tags only"},
		{Route: "/api/tags", Variant: "all", Dependencies: []conditionalDependency{dependency("links", "status", "library_kind", "tags"), dependency("site_tags", "site_id", "normalized_tag")}, ReadSideEffect: "none", Components: library, CacheStrategy: "version-keyed TagCache variant", NondeterministicInput: "none", Eligible: true, Decision: "combined aggregate versions both installation-local sources"},
		{Route: "/api/tree", Variant: "tree", Dependencies: []conditionalDependency{dependency("links", "id", "url", "title", "summary", "description", "tags", "content_type", "status", "domain", "path_depth", "parent_id", "fetcher_type", "is_low_confidence", "low_confidence_reason", "created_at", "updated_at")}, ReadSideEffect: "none", Components: library, CacheStrategy: "no body cache", NondeterministicInput: "none", Eligible: true, Decision: "tree is a deterministic installation library projection"},
		{Route: "/api/tree", Variant: "domains", Dependencies: []conditionalDependency{dependency("links", "status", "domain")}, ReadSideEffect: "none", Components: library, CacheStrategy: "version-keyed DomainSummaryCache", NondeterministicInput: "none", Eligible: true, Decision: "domain summary is a deterministic installation library aggregate"},
		{Route: "/api/tree", Variant: "domains-reading", Dependencies: []conditionalDependency{dependency("links", "status", "library_kind", "domain")}, ReadSideEffect: "none", Components: library, CacheStrategy: "version-keyed DomainSummaryCache variant", NondeterministicInput: "none", Eligible: true, Decision: "reading domain summary is library scoped"},
		{Route: "/api/tree", Variant: "domains-site", Dependencies: []conditionalDependency{dependency("links", "status", "library_kind", "domain")}, ReadSideEffect: "none", Components: library, CacheStrategy: "version-keyed DomainSummaryCache variant", NondeterministicInput: "none", Eligible: true, Decision: "site domain summary is library scoped"},
		{Route: "/api/subscriptions", Variant: "overview", Dependencies: []conditionalDependency{dependency("feed_subscriptions", "id", "folder_id", "url", "site_url", "title", "description", "active", "last_fetched_at", "last_success_at", "next_fetch_at", "last_error", "failure_count", "refresh_claim_token", "refresh_claimed_until", "created_at", "updated_at"), dependency("feed_folders", "id", "name", "created_at", "updated_at"), dependency("feed_items", "id", "subscription_id", "read_at", "starred", "read_later")}, ReadSideEffect: "none", Components: mustComponentSet(representation.FeedComponent), CacheStrategy: "none", NondeterministicInput: "refresh lease state and wall clock", Eligible: false, Decision: "Refreshing depends on refresh_claim_token, refresh_claimed_until and wall clock, which deliberately do not bump feed revision"},
		{Route: "/api/feed-items", Variant: "list", Dependencies: []conditionalDependency{dependency("feed_items", "id", "subscription_id", "title", "url", "author", "summary", "content_text", "content_html", "published_at", "created_at", "read_at", "starred", "read_later", "link_id"), dependency("feed_subscriptions", "id", "title", "active", "folder_id"), dependency("links", "id", "status")}, ReadSideEffect: "none", Components: libraryFeed, CacheStrategy: "no body cache", NondeterministicInput: "none", Eligible: true, Decision: "filters and all response dependencies are versioned"},
		{Route: "/api/sites", Variant: "list", Dependencies: []conditionalDependency{dependency("sites", "id", "site_key", "name", "intro", "homepage_url", "icon_url", "pinned", "needs_review", "revision", "first_collected_at", "last_collected_at", "primary_entry_id", "updated_at"), dependency("site_entries", "id", "site_id", "normalized_url"), dependency("site_tags", "site_id", "tag", "normalized_tag")}, ReadSideEffect: "none", Components: library, CacheStrategy: "no body cache", NondeterministicInput: "none", Eligible: true, Decision: "site list excludes identity and related-reading detail projections"},
	}
}

func dependency(table string, columns ...string) conditionalDependency {
	return conditionalDependency{
		Table:   table,
		Columns: append([]string(nil), columns...),
		Events:  []string{"insert", "update", "delete"},
	}
}

func cloneDependencies(source []conditionalDependency) []conditionalDependency {
	out := make([]conditionalDependency, len(source))
	for index, item := range source {
		out[index] = conditionalDependency{
			Table:   item.Table,
			Columns: append([]string(nil), item.Columns...),
			Events:  append([]string(nil), item.Events...),
		}
	}
	return out
}

func mustComponentSet(names ...representation.ComponentName) representation.ComponentSet {
	components, err := representation.NewComponentSet(names...)
	if err != nil {
		panic(err)
	}
	return components
}

// conditionalGetRoutes is the production allowlist. It is intentionally not
// generated from conditionalGetEligibilityMatrix; tests compare the two
// independently maintained representations and exercise every normalizer.
func conditionalGetRoutes() middleware.ConditionalGetPolicy {
	library := mustComponentSet(representation.LibraryComponent)
	libraryGlobal := mustComponentSet(representation.LibraryComponent, representation.GlobalComponent)
	libraryFeed := mustComponentSet(representation.LibraryComponent, representation.FeedComponent)
	canonical := middleware.ConditionalGetPolicy{
		"/api/links": {
			Components: libraryGlobal, NormalizeQuery: service.NormalizeLinkListConditionalQuery,
		},
		"/api/links/:link_id": {
			Components: libraryGlobal, NormalizeQuery: normalizeLinkDetailQuery,
		},
		"/api/tags": {
			Components: library, NormalizeQuery: normalizeTagQuery, MatchBeforeHandler: true,
		},
		"/api/tree": {
			Components: library, NormalizeQuery: normalizeTreeQuery, MatchBeforeHandler: true,
		},
		"/api/feed-items": {
			Components: libraryFeed, NormalizeQuery: service.NormalizeFeedItemsConditionalQuery, MatchBeforeHandler: true,
		},
		"/api/sites": {
			Components: library, NormalizeQuery: service.NormalizeSiteListConditionalQuery, MatchBeforeHandler: true,
		},
	}
	policy := make(middleware.ConditionalGetPolicy, len(canonical)*2)
	for route, entry := range canonical {
		policy[route] = entry
		policy[strings.Replace(route, "/api/", "/api/v1/", 1)] = entry
	}
	return policy
}

func normalizeLinkDetailQuery(values url.Values) (string, bool) {
	if values.Get("include_content") == "false" {
		return "include_content=false", true
	}
	return "", true
}

func normalizeTagQuery(values url.Values) (string, bool) {
	scope, ok := service.NormalizeTagLibraryKind(values.Get("library_kind"))
	if !ok {
		return "", false
	}
	if scope == "" {
		return "", true
	}
	return url.Values{"library_kind": {scope}}.Encode(), true
}

func normalizeTreeQuery(values url.Values) (string, bool) {
	if values.Get("view") == "domains" {
		kind, ok := model.NormalizeOptionalLibraryKind(values.Get("library_kind"))
		if !ok {
			return "", false
		}
		query := url.Values{"view": {"domains"}}
		if kind != "" {
			query.Set("library_kind", string(kind))
		}
		return query.Encode(), true
	}
	domain := values.Get("domain")
	if domain == "" {
		return "", true
	}
	return url.Values{"domain": {domain}}.Encode(), true
}
