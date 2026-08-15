package app

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestConditionalGetEligibilityMatrixRecordsCompleteRouteDependencies(t *testing.T) {
	t.Parallel()

	type expectation struct {
		tables     []string
		components string
		eligible   bool
	}
	want := map[string]expectation{
		"/api/links|list":                     {tables: []string{"concept", "link_concept", "links"}, components: "global,library", eligible: true},
		"/api/links|url":                      {tables: []string{"concept", "link_concept", "links"}, components: "global,library", eligible: true},
		"/api/links|search":                   {tables: []string{"concept", "link_concept", "links"}, components: "global,library", eligible: false},
		"/api/links|cursor":                   {tables: []string{"concept", "link_concept", "links"}, components: "global,library", eligible: false},
		"/api/links/:link_id|detail-content":  {tables: []string{"concept", "link_concept", "links"}, components: "global,library", eligible: true},
		"/api/links/:link_id|detail-metadata": {tables: []string{"concept", "link_concept", "links"}, components: "global,library", eligible: true},
		"/api/tags|default":                   {tables: []string{"links"}, components: "library", eligible: true},
		"/api/tags|reading":                   {tables: []string{"links"}, components: "library", eligible: true},
		"/api/tags|site":                      {tables: []string{"site_tags"}, components: "library", eligible: true},
		"/api/tags|all":                       {tables: []string{"links", "site_tags"}, components: "library", eligible: true},
		"/api/tree|tree":                      {tables: []string{"links"}, components: "library", eligible: true},
		"/api/tree|domains":                   {tables: []string{"links"}, components: "library", eligible: true},
		"/api/tree|domains-reading":           {tables: []string{"links"}, components: "library", eligible: true},
		"/api/tree|domains-site":              {tables: []string{"links"}, components: "library", eligible: true},
		"/api/subscriptions|overview":         {tables: []string{"feed_folders", "feed_items", "feed_subscriptions"}, components: "feed", eligible: false},
		"/api/feed-items|list":                {tables: []string{"feed_items", "feed_subscriptions", "links"}, components: "feed,library", eligible: true},
		"/api/sites|list":                     {tables: []string{"site_entries", "site_tags", "sites"}, components: "library", eligible: true},
	}

	got := make(map[string]conditionalRouteEligibility)
	dependencyTables := make(map[string]struct{})
	eligibleColumns := make(map[string]map[string]struct{})
	for _, row := range conditionalGetEligibilityMatrix() {
		key := row.Route + "|" + row.Variant
		if _, duplicate := got[key]; duplicate {
			t.Fatalf("duplicate eligibility row %q", key)
		}
		got[key] = row
		if row.Decision == "" || row.CacheStrategy == "" || row.NondeterministicInput == "" {
			t.Errorf("%s has incomplete decision/cache/nondeterministic-input documentation", key)
		}
		for _, dependency := range row.Dependencies {
			dependencyTables[dependency.Table] = struct{}{}
			if row.Eligible && eligibleColumns[dependency.Table] == nil {
				eligibleColumns[dependency.Table] = make(map[string]struct{})
			}
			if dependency.Table == "" || len(dependency.Columns) == 0 || len(dependency.Events) == 0 {
				t.Errorf("%s has incomplete dependency %#v", key, dependency)
			}
			if !slices.Equal(dependency.Events, []string{"insert", "update", "delete"}) {
				t.Errorf("%s dependency %s events = %v, want insert/update/delete", key, dependency.Table, dependency.Events)
			}
			for _, column := range dependency.Columns {
				if column == "" || strings.ContainsAny(column, " :/") {
					t.Errorf("%s dependency %s contains non-column descriptor %q", key, dependency.Table, column)
				}
				if row.Eligible {
					eligibleColumns[dependency.Table][column] = struct{}{}
				}
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("matrix rows = %d, want %d: %#v", len(got), len(want), got)
	}
	for key, expected := range want {
		row, ok := got[key]
		if !ok {
			t.Errorf("matrix missing %s", key)
			continue
		}
		tables := make([]string, 0, len(row.Dependencies))
		for _, dependency := range row.Dependencies {
			tables = append(tables, dependency.Table)
		}
		slices.Sort(tables)
		if !slices.Equal(tables, expected.tables) {
			t.Errorf("%s tables = %v, want %v", key, tables, expected.tables)
		}
		if gotKey := row.Components.Key(); gotKey != expected.components {
			t.Errorf("%s components = %q, want %q", key, gotKey, expected.components)
		}
		if row.Eligible != expected.eligible {
			t.Errorf("%s eligible = %v, want %v", key, row.Eligible, expected.eligible)
		}
	}

	wantDependencyTables := []string{
		"concept", "feed_folders", "feed_items", "feed_subscriptions", "link_concept",
		"links", "site_entries", "site_tags", "sites",
	}
	gotDependencyTables := make([]string, 0, len(dependencyTables))
	for table := range dependencyTables {
		gotDependencyTables = append(gotDependencyTables, table)
	}
	slices.Sort(gotDependencyTables)
	if !slices.Equal(gotDependencyTables, wantDependencyTables) {
		t.Errorf("matrix dependency tables = %v, want %v", gotDependencyTables, wantDependencyTables)
	}

	// This contract is deliberately maintained independently from both the
	// production matrix and migration installer tests. It makes the matrix's
	// eligible source-column inventory reviewable without introducing app ->
	// migrate package coupling.
	wantEligibleColumns := map[string][]string{
		"concept": {"display_name", "id", "primary_name"},
		"feed_items": {
			"author", "content_html", "content_text", "created_at", "id", "link_id",
			"published_at", "read_at", "read_later", "starred", "subscription_id",
			"summary", "title", "url",
		},
		"feed_subscriptions": {"active", "folder_id", "id", "title"},
		"link_concept":       {"concept_id", "link_id", "surface_tag"},
		"links": {
			"classification_confidence", "classification_explanation", "classification_reason",
			"classifier_version", "content", "content_cjk_chars", "content_document",
			"content_format", "content_revision", "content_source", "content_type", "content_words", "created_at",
			"description", "domain", "error_msg", "fetcher_type", "id", "is_low_confidence",
			"library_kind", "library_kind_locked", "library_kind_source", "low_confidence_reason",
			"parent_id", "parent_path", "path_depth", "predicted_library_kind", "status",
			"summary", "tags", "title", "updated_at", "url",
		},
		"site_entries": {"id", "normalized_url", "site_id"},
		"site_tags":    {"normalized_tag", "site_id", "tag"},
		"sites": {
			"first_collected_at", "homepage_url", "icon_url", "id", "intro", "last_collected_at",
			"name", "needs_review", "pinned", "primary_entry_id", "revision", "site_key",
			"updated_at",
		},
	}
	if len(eligibleColumns) != len(wantEligibleColumns) {
		t.Fatalf("eligible dependency tables = %v, want %v", eligibleColumns, wantEligibleColumns)
	}
	for table, wantColumns := range wantEligibleColumns {
		gotColumns := make([]string, 0, len(eligibleColumns[table]))
		for column := range eligibleColumns[table] {
			gotColumns = append(gotColumns, column)
		}
		slices.Sort(gotColumns)
		if !slices.Equal(gotColumns, wantColumns) {
			t.Errorf("eligible %s columns = %v, want %v", table, gotColumns, wantColumns)
		}
	}
}

func TestConditionalGetProductionPolicyMatchesEligibleMatrixRoutesIndependently(t *testing.T) {
	t.Parallel()

	wantRoutes := []string{
		"/api/links",
		"/api/links/:link_id",
		"/api/tags",
		"/api/tree",
		"/api/feed-items",
		"/api/sites",
		"/api/v1/links",
		"/api/v1/links/:link_id",
		"/api/v1/tags",
		"/api/v1/tree",
		"/api/v1/feed-items",
		"/api/v1/sites",
	}
	for attempt := 0; attempt < 1000; attempt++ {
		policy := conditionalGetRoutes()
		if len(policy) != len(wantRoutes) {
			keys := make([]string, 0, len(policy))
			for route := range policy {
				keys = append(keys, route)
			}
			slices.Sort(keys)
			t.Fatalf("production policy keys = %v, want exactly %v", keys, wantRoutes)
		}
	}

	policy := conditionalGetRoutes()
	for _, route := range wantRoutes {
		entry, ok := policy[route]
		if !ok {
			t.Errorf("production policy missing eligible route %s", route)
			continue
		}
		if entry.Components.Key() == "" || entry.NormalizeQuery == nil {
			t.Errorf("production policy %s is incomplete: %#v", route, entry)
		}
	}
	for _, route := range []string{"/api/subscriptions", "/api/v1/subscriptions", "/api/sites/:site_id"} {
		if _, ok := policy[route]; ok {
			t.Errorf("production policy unexpectedly enables %s", route)
		}
	}

	// The audit matrix is documentation and must not be the mutable backing
	// store for production policy. A caller-owned matrix mutation must not
	// remove or rename a production entry.
	matrix := conditionalGetEligibilityMatrix()
	matrix[0].Route = "/mutated"
	if _, ok := policy["/api/links"]; !ok {
		t.Fatal("production policy aliases the eligibility matrix")
	}
}

func TestConditionalGetProductionQueryPoliciesMatchHandlerSemantics(t *testing.T) {
	t.Parallel()
	policy := conditionalGetRoutes()

	assertQuery := func(route, raw, wantCanonical string, wantEligible bool) {
		t.Helper()
		values, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", raw, err)
		}
		got, eligible := policy[route].NormalizeQuery(values)
		if got != wantCanonical || eligible != wantEligible {
			t.Errorf("%s query %q = %q/%v, want %q/%v", route, raw, got, eligible, wantCanonical, wantEligible)
		}
	}

	assertQuery("/api/tags", "library_kind=%20SITE%20", "library_kind=site", true)
	assertQuery("/api/tags", "library_kind=invalid", "", false)
	assertQuery("/api/links", "q=semantic", "", false)
	assertQuery("/api/links", "after=opaque", "", false)
	assertQuery("/api/links", "status=not-a-state", "", false)
	assertQuery("/api/links", "page=0&limit=1000&low_confidence=TRUE", "limit=100&low_confidence=true", true)
	createdRange := url.Values{
		"created_from":   {"2026-08-10T16:00:00Z"},
		"created_before": {"2026-08-11T16:00:00Z"},
	}.Encode()
	assertQuery("/api/links", "created_from=2026-08-11T00%3A00%3A00%2B08%3A00&created_before=2026-08-12T00%3A00%3A00%2B08%3A00", createdRange, true)
	assertQuery("/api/links", "created_from=2026-08-10T16%3A00%3A00Z", "", false)
	assertQuery("/api/links", "created_before=2026-08-11T16%3A00%3A00Z", "", false)
	assertQuery("/api/links", "created_from=invalid&created_before=2026-08-11T16%3A00%3A00Z", "", false)
	assertQuery("/api/links", "created_from=2026-08-11T16%3A00%3A00Z&created_before=2026-08-11T16%3A00%3A00Z", "", false)
	assertQuery("/api/links/:link_id", "include_content=false", "include_content=false", true)
	assertQuery("/api/links/:link_id", "include_content=anything", "", true)
	assertQuery("/api/tree", "view=domains&domain=ignored", "view=domains", true)
	assertQuery("/api/tree", "view=domains&library_kind=%20READING%20", "library_kind=reading&view=domains", true)
	assertQuery("/api/tree", "view=domains&library_kind=all", "", false)
	assertQuery("/api/tree", "view=unknown&domain=example.com", "domain=example.com", true)
	assertQuery("/api/sites", "view=%20pinned%20&page=0&limit=200", "limit=100&view=pinned", true)
	assertQuery("/api/sites", "view=unknown", "", false)
	assertQuery("/api/sites", "view=recent", "", false)
	assertQuery("/api/sites", "view=recent&recent_cutoff=2026-07-11T02%3A15%3A30Z", "", false)
	assertQuery("/api/sites", "view=recent&page=2", "", false)
	assertQuery("/api/sites", "view=recent&page=2&recent_cutoff=2026-07-11T10%3A15%3A30%2B08%3A00", "page=2&recent_cutoff=2026-07-11T02%3A15%3A30Z&view=recent", true)
	assertQuery("/api/sites", "view=all&recent_cutoff=2026-07-11T02%3A15%3A30Z", "", false)
	assertQuery("/api/feed-items", "view=%20UNREAD%20&page=0&limit=200", "limit=100&view=unread", true)
	assertQuery("/api/feed-items", "subscription_id=invalid", "", false)
	assertQuery("/api/feed-items", "folder_id=ungrouped&q=%20go%20", "folder_id=ungrouped&q=go", true)
}

func TestConditionalGetLinkCreatedRangeSeparatesRepresentations(t *testing.T) {
	t.Parallel()
	_, router := newConditionalGetProductionRouter(nil)

	first := conditionalRequest(router, "/api/links?created_from=2026-08-10T16%3A00%3A00Z&created_before=2026-08-11T16%3A00%3A00Z", "")
	second := conditionalRequest(router, "/api/links?created_from=2026-08-11T16%3A00%3A00Z&created_before=2026-08-12T16%3A00%3A00Z", "")
	firstTag := first.Header().Get("ETag")
	secondTag := second.Header().Get("ETag")
	if first.Code != http.StatusOK || second.Code != http.StatusOK || firstTag == "" || secondTag == "" {
		t.Fatalf("first=%d/%q second=%d/%q, want 200 with validators", first.Code, firstTag, second.Code, secondTag)
	}
	if firstTag == secondTag {
		t.Fatalf("different created ranges shared ETag %q", firstTag)
	}
	if rec := conditionalRequest(router, "/api/links?created_from=2026-08-11T16%3A00%3A00Z&created_before=2026-08-12T16%3A00%3A00Z", firstTag); rec.Code != http.StatusOK {
		t.Fatalf("first range validator reused by second range: status = %d, want 200", rec.Code)
	}
}

func TestConditionalGetDomainSummarySeparatesLibraryScopes(t *testing.T) {
	t.Parallel()
	_, router := newConditionalGetProductionRouter(nil)

	reading := conditionalRequest(router, "/api/tree?view=domains&library_kind=reading", "")
	site := conditionalRequest(router, "/api/tree?view=domains&library_kind=site", "")
	readingTag := reading.Header().Get("ETag")
	siteTag := site.Header().Get("ETag")
	if reading.Code != http.StatusOK || site.Code != http.StatusOK || readingTag == "" || siteTag == "" {
		t.Fatalf("reading=%d/%q site=%d/%q, want 200 with validators", reading.Code, readingTag, site.Code, siteTag)
	}
	if readingTag == siteTag {
		t.Fatalf("reading and site domain summaries shared ETag %q", readingTag)
	}
	if rec := conditionalRequest(router, "/api/tree?view=domains&library_kind=site", readingTag); rec.Code != http.StatusOK {
		t.Fatalf("reading validator reused by site scope: status = %d, want 200", rec.Code)
	}
}

func TestConditionalGetLinkDomainCanonicalQueryPreservesHandlerInput(t *testing.T) {
	t.Parallel()
	_, router := newConditionalGetProductionRouter(nil)

	withoutDomain := conditionalRequest(router, "/api/links", "")
	oldTag := withoutDomain.Header().Get("ETag")
	if withoutDomain.Code != http.StatusOK || oldTag == "" {
		t.Fatalf("baseline status=%d ETag=%q", withoutDomain.Code, oldTag)
	}

	withWhitespaceDomain := conditionalRequest(router, "/api/links?domain=%20%20", oldTag)
	if withWhitespaceDomain.Code != http.StatusOK {
		t.Fatalf("whitespace domain status=%d body=%q, want 200", withWhitespaceDomain.Code, withWhitespaceDomain.Body.String())
	}
	if newTag := withWhitespaceDomain.Header().Get("ETag"); newTag == "" || newTag == oldTag {
		t.Fatalf("whitespace domain ETag=%q, want value distinct from %q", newTag, oldTag)
	}
	if withWhitespaceDomain.Body.String() == withoutDomain.Body.String() {
		t.Fatalf("handler bodies unexpectedly equal: %q", withWhitespaceDomain.Body.String())
	}
}

func TestConditionalGetTreeDomainCanonicalQueryPreservesHandlerInput(t *testing.T) {
	t.Parallel()
	_, router := newConditionalGetProductionRouter(nil)

	withoutDomain := conditionalRequest(router, "/api/tree", "")
	oldTag := withoutDomain.Header().Get("ETag")
	if withoutDomain.Code != http.StatusOK || oldTag == "" {
		t.Fatalf("baseline status=%d ETag=%q", withoutDomain.Code, oldTag)
	}

	withWhitespaceDomain := conditionalRequest(router, "/api/tree?domain=%20%20", oldTag)
	if withWhitespaceDomain.Code != http.StatusOK {
		t.Fatalf("whitespace domain status=%d body=%q, want 200", withWhitespaceDomain.Code, withWhitespaceDomain.Body.String())
	}
	if newTag := withWhitespaceDomain.Header().Get("ETag"); newTag == "" || newTag == oldTag {
		t.Fatalf("whitespace domain ETag=%q, want value distinct from %q", newTag, oldTag)
	}
	if withWhitespaceDomain.Body.String() == withoutDomain.Body.String() {
		t.Fatalf("handler bodies unexpectedly equal: %q", withWhitespaceDomain.Body.String())
	}
}

func TestConditionalGetProductionEligibleRoutesPublishAndReuseValidators(t *testing.T) {
	probes, router := newConditionalGetProductionRouter(nil)
	paths := []string{
		"/api/links",
		"/api/links/known?include_content=false",
		"/api/tags?library_kind=site",
		"/api/tree?view=domains",
		"/api/feed-items?view=unread",
		"/api/sites?view=pinned",
	}
	for _, path := range paths {
		first := conditionalRequest(router, path, "")
		tag := first.Header().Get("ETag")
		if first.Code != http.StatusOK || tag == "" {
			t.Errorf("%s first response status=%d ETag=%q, want 200/non-empty", path, first.Code, tag)
			continue
		}
		notModified := conditionalRequest(router, path, tag)
		if notModified.Code != http.StatusNotModified {
			t.Errorf("%s exact response status=%d body=%q, want 304", path, notModified.Code, notModified.Body.String())
		}
	}

	wantComponentCalls := map[string]int{
		"global,library": 4,
		"library":        6,
		"feed,library":   2,
	}
	if len(probes.revisions.componentCalls) != len(wantComponentCalls) {
		t.Fatalf("component call sets = %#v, want %#v", probes.revisions.componentCalls, wantComponentCalls)
	}
	for key, want := range wantComponentCalls {
		if got := probes.revisions.componentCalls[key]; got != want {
			t.Errorf("component reads %s = %d, want %d", key, got, want)
		}
	}
}

func TestConditionalGetProductionIneligibleVariantsAlwaysRunHandlers(t *testing.T) {
	probes, router := newConditionalGetProductionRouter(nil)
	paths := []string{
		"/api/links?q=semantic",
		"/api/links?after=opaque",
		"/api/subscriptions",
		"/api/sites/11111111-1111-1111-1111-111111111111",
	}
	for _, path := range paths {
		response := conditionalRequest(router, path, "*")
		if response.Code != http.StatusOK {
			t.Errorf("%s status=%d, want 200", path, response.Code)
		}
		if tag := response.Header().Get("ETag"); tag != "" {
			t.Errorf("%s ETag=%q, want empty", path, tag)
		}
	}
	if probes.links.listCalls != 2 || probes.feeds.subscriptionCalls != 1 {
		t.Fatalf("ineligible handler calls links/subscriptions = %d/%d, want 2/1", probes.links.listCalls, probes.feeds.subscriptionCalls)
	}
	if probes.revisions.routeCalls != 0 {
		t.Fatalf("ineligible route revision reads = %d, want 0", probes.revisions.routeCalls)
	}
}
