package dbintegration

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/app"
	"webtag/internal/repository"
	"webtag/internal/service"
)

type rf3bConditionalReplica struct {
	router      http.Handler
	tagCache    *service.TagCache
	domainCache *service.DomainSummaryCache
	revisions   *service.ReadRevisionService
}

func newRF3BConditionalReplica(pool *pgxpool.Pool) rf3bConditionalReplica {
	return newRF3BConditionalReplicaWithOptions(pool, app.RouterOptions{
		AppEnv:            "prod",
		ExtensionAPIToken: "conditional-get-legacy-fixture",
	})
}

func newRF3BConditionalReplicaWithOptions(pool *pgxpool.Pool, options app.RouterOptions) rf3bConditionalReplica {
	return newRF3BConditionalReplicaConfigured(pool, options, nil, nil)
}

func newRF3BConditionalReplicaConfigured(
	pool *pgxpool.Pool,
	options app.RouterOptions,
	embedder service.RetrievalEmbedder,
	now func() time.Time,
) rf3bConditionalReplica {
	return newRF3BConditionalReplicaWithDisplay(
		pool,
		options,
		repository.NewPGXConceptRepository(pool),
		embedder,
		now,
	)
}

func newRF3BConditionalReplicaWithDisplay(
	pool *pgxpool.Pool,
	options app.RouterOptions,
	conceptDisplay service.ConceptDisplayLookup,
	embedder service.RetrievalEmbedder,
	now func() time.Time,
) rf3bConditionalReplica {
	linkStore := repository.NewPGXLinkRepository(pool)
	tagCache := service.NewTagCache(time.Hour, nil)
	domainCache := service.NewDomainSummaryCache(time.Hour, nil)
	revisions := service.NewReadRevisionService(repository.NewPGXReadRevisionRepository(pool))

	links := service.NewLinkReadService(service.LinkReadServiceOptions{
		Links:          linkStore,
		ConceptDisplay: conceptDisplay,
		ContentReader:  linkStore,
		QueryEmbedder:  embedder,
	})
	tags := service.NewTagReadService(repository.NewPGXTagRepository(pool), tagCache)
	tree := service.NewTreeReadService(repository.NewPGXTreeRepository(pool), nil).WithDomainCache(domainCache)
	sites := service.NewSiteReadService(repository.NewPGXSiteRepository(pool))
	feeds := service.NewFeedService(service.FeedServiceOptions{
		Store: repository.NewPGXFeedRepository(pool, pool),
		Now:   now,
	})
	deps := conditionalGetDependencies(tags)
	deps.LinksRead = links
	deps.Tree = tree
	deps.Sites = sites
	deps.Feeds = feeds

	options.ConditionalGetRevisions = revisions
	return rf3bConditionalReplica{
		router: app.NewRouterWithDependencies(
			deps,
			nil,
			nil,
			nil,
			nil,
			options,
		),
		tagCache:    tagCache,
		domainCache: domainCache,
		revisions:   revisions,
	}
}

type switchingRF3BEmbedder struct {
	fail   bool
	vector []float32
	calls  int
}

func (e *switchingRF3BEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	e.calls++
	if e.fail {
		return nil, errors.New("rf3b embedder unavailable")
	}
	out := make([][]float32, len(inputs))
	for index := range out {
		out[index] = e.vector
	}
	return out, nil
}

func (*switchingRF3BEmbedder) Model() string { return "rf3b-model" }
func (*switchingRF3BEmbedder) Enabled() bool { return true }

type failingRF3BConceptDisplay struct {
	calls int
}

func (d *failingRF3BConceptDisplay) ListDisplayNamesByLinkIDs(
	context.Context,
	[]uuid.UUID,
) (map[uuid.UUID][]string, error) {
	d.calls++
	return nil, errors.New("rf3b concept display unavailable")
}

func TestRF3BSearchRemainsNonCacheableAcrossEmbedderRecovery(t *testing.T) {
	pool := StartPostgres(t)
	query := "rf3b-needle"
	keywordID := mustCreateDoneLink(t, repository.NewPGXLinkRepository(pool), t.Context(),
		"https://rf3b-search-keyword.example.com", "keyword", "rf3b-search.example.com")
	semanticID := mustCreateDoneLink(t, repository.NewPGXLinkRepository(pool), t.Context(),
		"https://rf3b-search-semantic.example.com", "semantic", "rf3b-search.example.com")
	vector := make([]float32, 1536)
	vector[0] = 1
	if _, err := pool.Exec(t.Context(), `UPDATE links SET title=$2 WHERE id=$1`, keywordID, query+" keyword result"); err != nil {
		t.Fatalf("seed keyword title: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET title='semantic only' WHERE id=$1`, semanticID); err != nil {
		t.Fatalf("seed semantic title: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE links SET embedding=$2, embedding_model='rf3b-model' WHERE id=$1`,
		semanticID, pgvector.NewVector(vector)); err != nil {
		t.Fatalf("seed semantic embedding: %v", err)
	}
	embedder := &switchingRF3BEmbedder{fail: true, vector: vector}
	replica := newRF3BConditionalReplicaConfigured(pool, app.RouterOptions{
		AppEnv:            "prod",
		ExtensionAPIToken: "conditional-get-legacy-fixture",
	}, embedder, nil)
	baseline := readRF3BRevisionSnapshot(t, pool)
	path := "/api/links?" + url.Values{"q": {query}}.Encode()

	degraded := rf3bConditionalGET(t, replica.router, path, "*")
	if degraded.Code != http.StatusOK || degraded.Header().Get("ETag") != "" {
		t.Fatalf("degraded search status=%d ETag=%q body=%q", degraded.Code, degraded.Header().Get("ETag"), degraded.Body.String())
	}
	embedder.fail = false
	recovered := rf3bConditionalGET(t, replica.router, path, "*")
	if recovered.Code != http.StatusOK || recovered.Header().Get("ETag") != "" {
		t.Fatalf("recovered search status=%d ETag=%q body=%q", recovered.Code, recovered.Header().Get("ETag"), recovered.Body.String())
	}
	if bytes.Equal(degraded.Body.Bytes(), recovered.Body.Bytes()) {
		t.Fatalf("embedder recovery did not change q= body: %q", recovered.Body.String())
	}
	if embedder.calls != 2 {
		t.Fatalf("embedder calls=%d, want 2 handler executions", embedder.calls)
	}
	if after := readRF3BRevisionSnapshot(t, pool); after != baseline {
		t.Fatalf("embedder-only recovery changed database revisions: before=%+v after=%+v", baseline, after)
	}
}

func TestRF3BSubscriptionsRemainNonCacheableAcrossRefreshDeadline(t *testing.T) {
	pool := StartPostgres(t)
	deadline := time.Date(2040, time.January, 2, 3, 4, 5, 0, time.UTC)
	now := deadline.Add(-time.Minute)
	if _, err := pool.Exec(t.Context(), `INSERT INTO feed_subscriptions
		(url,title,active,next_fetch_at,refresh_claim_token,refresh_claimed_until)
		VALUES ('https://rf3b-clock.example.com/feed','clock',true,NOW(),$1,$2)`,
		uuid.New(), deadline); err != nil {
		t.Fatalf("seed clock subscription: %v", err)
	}
	replica := newRF3BConditionalReplicaConfigured(pool, app.RouterOptions{
		AppEnv:            "prod",
		ExtensionAPIToken: "conditional-get-legacy-fixture",
	}, nil, func() time.Time { return now })
	baseline := readRF3BRevisionSnapshot(t, pool)

	before := rf3bConditionalGET(t, replica.router, "/api/subscriptions", "*")
	if before.Code != http.StatusOK || before.Header().Get("ETag") != "" || !bytes.Contains(before.Body.Bytes(), []byte(`"refreshing":true`)) {
		t.Fatalf("before deadline status=%d ETag=%q body=%q", before.Code, before.Header().Get("ETag"), before.Body.String())
	}
	now = deadline.Add(time.Nanosecond)
	after := rf3bConditionalGET(t, replica.router, "/api/subscriptions", "*")
	if after.Code != http.StatusOK || after.Header().Get("ETag") != "" || !bytes.Contains(after.Body.Bytes(), []byte(`"refreshing":false`)) {
		t.Fatalf("after deadline status=%d ETag=%q body=%q", after.Code, after.Header().Get("ETag"), after.Body.String())
	}
	if bytes.Equal(before.Body.Bytes(), after.Body.Bytes()) {
		t.Fatalf("fake clock crossing did not change subscriptions body: %q", after.Body.String())
	}
	if current := readRF3BRevisionSnapshot(t, pool); current != baseline {
		t.Fatalf("clock-only change moved database revisions: before=%+v after=%+v", baseline, current)
	}
}

func TestRF3BConceptDisplayFailSoftHTTPResponsesPublishNoStrongETag(t *testing.T) {
	pool := StartPostgres(t)
	linkID := createRF3BReadingLink(t, pool,
		"https://rf3b-fail-soft.example.com/article", "raw-fallback", "rf3b-fail-soft.example.com")
	display := &failingRF3BConceptDisplay{}
	replica := newRF3BConditionalReplicaWithDisplay(pool, app.RouterOptions{
		AppEnv:            "prod",
		ExtensionAPIToken: "conditional-get-legacy-fixture",
	}, display, nil, nil)

	paths := []string{
		"/api/links",
		"/api/links/" + linkID.String() + "?include_content=false",
	}
	for _, path := range paths {
		response := rf3bConditionalGET(t, replica.router, path, "*")
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status=%d, want 200; body=%q", path, response.Code, response.Body.String())
			continue
		}
		if tag := response.Header().Get("ETag"); tag != "" {
			t.Errorf("GET %s fail-soft ETag=%q, want empty", path, tag)
		}
		if !bytes.Contains(response.Body.Bytes(), []byte(`"raw-fallback"`)) {
			t.Errorf("GET %s fail-soft body omitted raw fallback: %q", path, response.Body.String())
		}
	}
	if display.calls != len(paths) {
		t.Fatalf("concept display calls=%d, want %d handler executions", display.calls, len(paths))
	}
}

func TestRF3BLinkConceptAttachAndDetachInvalidateHTTPRepresentations(t *testing.T) {
	poolA := StartPostgres(t)
	poolB, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open replica B pool: %v", err)
	}
	t.Cleanup(poolB.Close)
	unique := uuid.NewString()
	linkID := createRF3BReadingLink(t, poolA,
		"https://rf3b-link-concept-"+unique+".example.com/article", "raw-before", "rf3b-link-concept.example.com")
	conceptID := uuid.New()
	if _, err := poolA.Exec(t.Context(), `INSERT INTO concept
		(id,primary_name,display_name)
		VALUES ($1,'attached-primary','attached-display')`, conceptID); err != nil {
		t.Fatalf("insert concept: %v", err)
	}
	replicaA := newRF3BConditionalReplica(poolA)
	replicaB := newRF3BConditionalReplica(poolB)
	path := "/api/links/" + linkID.String() + "?include_content=false"

	prewarm := rf3bConditionalGET(t, replicaA.router, path, "")
	before := rf3bConditionalGET(t, replicaB.router, path, "")
	oldTag := before.Header().Get("ETag")
	if prewarm.Code != http.StatusOK || before.Code != http.StatusOK || oldTag == "" ||
		!bytes.Contains(before.Body.Bytes(), []byte(`"raw-before"`)) {
		t.Fatalf("before attach status A/B=%d/%d ETag=%q body=%q", prewarm.Code, before.Code, oldTag, before.Body.String())
	}

	if _, err := poolA.Exec(t.Context(), `INSERT INTO link_concept
		(link_id,concept_id,surface_tag) VALUES ($1,$2,'attached-surface')`,
		linkID, conceptID); err != nil {
		t.Fatalf("attach concept: %v", err)
	}
	attached := rf3bConditionalGET(t, replicaB.router, path, oldTag)
	attachedTag := attached.Header().Get("ETag")
	if attached.Code != http.StatusOK || attachedTag == "" || attachedTag == oldTag ||
		!bytes.Contains(attached.Body.Bytes(), []byte(`"attached-display"`)) ||
		bytes.Equal(before.Body.Bytes(), attached.Body.Bytes()) {
		t.Fatalf("after attach status=%d ETag=%q body=%q", attached.Code, attachedTag, attached.Body.String())
	}
	if response := rf3bConditionalGET(t, replicaB.router, path, attachedTag); response.Code != http.StatusNotModified {
		t.Fatalf("attached validator status=%d, want 304", response.Code)
	}

	if _, err := poolA.Exec(t.Context(), `DELETE FROM link_concept
		WHERE link_id=$1 AND concept_id=$2`, linkID, conceptID); err != nil {
		t.Fatalf("detach concept: %v", err)
	}
	detached := rf3bConditionalGET(t, replicaB.router, path, attachedTag)
	detachedTag := detached.Header().Get("ETag")
	if detached.Code != http.StatusOK || detachedTag == "" || detachedTag == attachedTag ||
		!bytes.Contains(detached.Body.Bytes(), []byte(`"raw-before"`)) ||
		bytes.Equal(attached.Body.Bytes(), detached.Body.Bytes()) {
		t.Fatalf("after detach status=%d ETag=%q body=%q", detached.Code, detachedTag, detached.Body.String())
	}
	if response := rf3bConditionalGET(t, replicaB.router, path, detachedTag); response.Code != http.StatusNotModified {
		t.Fatalf("detached validator status=%d, want 304", response.Code)
	}
}

func TestRF3BConceptMergeRecalculatesDisplayNameAndInvalidatesHTTP(t *testing.T) {
	poolA := StartPostgres(t)
	poolB, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open replica B pool: %v", err)
	}
	t.Cleanup(poolB.Close)
	unique := uuid.NewString()
	linkID := createRF3BReadingLink(t, poolA,
		"https://rf3b-merge-"+unique+".example.com/article", "raw-before-merge", "rf3b-merge.example.com")
	winnerID, loserID, proposalID := uuid.New(), uuid.New(), uuid.New()
	if _, err := poolA.Exec(t.Context(), `INSERT INTO concept
		(id,primary_name,display_name) VALUES
		($1,'winner-primary','winner-before'),
		($2,'loser-primary','loser-before')`, winnerID, loserID); err != nil {
		t.Fatalf("insert merge concepts: %v", err)
	}
	if _, err := poolA.Exec(t.Context(), `INSERT INTO link_concept
		(link_id,concept_id,surface_tag) VALUES ($1,$2,'surface-after-merge')`,
		linkID, loserID); err != nil {
		t.Fatalf("attach loser concept: %v", err)
	}
	if _, err := poolA.Exec(t.Context(), `INSERT INTO concept_merge_proposal
		(id,winner_id,loser_id,score,llm_reason)
		VALUES ($1,$2,$3,0.9,'rf3b HTTP acceptance')`,
		proposalID, winnerID, loserID); err != nil {
		t.Fatalf("insert merge proposal: %v", err)
	}
	replicaA := newRF3BConditionalReplica(poolA)
	replicaB := newRF3BConditionalReplica(poolB)
	path := "/api/links/" + linkID.String() + "?include_content=false"
	prewarm := rf3bConditionalGET(t, replicaA.router, path, "")
	before := rf3bConditionalGET(t, replicaB.router, path, "")
	oldTag := before.Header().Get("ETag")
	if prewarm.Code != http.StatusOK || before.Code != http.StatusOK || oldTag == "" ||
		!bytes.Contains(before.Body.Bytes(), []byte(`"loser-before"`)) {
		t.Fatalf("before merge status A/B=%d/%d ETag=%q body=%q", prewarm.Code, before.Code, oldTag, before.Body.String())
	}

	if err := repository.NewPGXConceptProposalRepository(poolA).
		MergeConceptsByProposal(t.Context(), proposalID, "rf3b-test"); err != nil {
		t.Fatalf("MergeConceptsByProposal: %v", err)
	}
	merged := rf3bConditionalGET(t, replicaB.router, path, oldTag)
	mergedTag := merged.Header().Get("ETag")
	if merged.Code != http.StatusOK || mergedTag == "" || mergedTag == oldTag ||
		!bytes.Contains(merged.Body.Bytes(), []byte(`"surface-after-merge"`)) ||
		bytes.Equal(before.Body.Bytes(), merged.Body.Bytes()) {
		t.Fatalf("after merge status=%d ETag=%q body=%q", merged.Code, mergedTag, merged.Body.String())
	}
	var displayName string
	var loserCount, winnerEdges, pendingProposals int
	if err := poolA.QueryRow(t.Context(), `SELECT
		(SELECT display_name FROM concept WHERE id=$1),
		(SELECT count(*) FROM concept WHERE id=$3),
		(SELECT count(*) FROM link_concept WHERE link_id=$4 AND concept_id=$1),
		(SELECT count(*) FROM concept_merge_proposal WHERE id=$2 AND status='pending')`,
		winnerID, proposalID, loserID, linkID).Scan(&displayName, &loserCount, &winnerEdges, &pendingProposals); err != nil {
		t.Fatalf("read merged state: %v", err)
	}
	if displayName != "surface-after-merge" || loserCount != 0 || winnerEdges != 1 || pendingProposals != 0 {
		t.Fatalf("merged state display/loser/edges/pending=%q/%d/%d/%d", displayName, loserCount, winnerEdges, pendingProposals)
	}
	if response := rf3bConditionalGET(t, replicaB.router, path, mergedTag); response.Code != http.StatusNotModified {
		t.Fatalf("merged validator status=%d, want 304", response.Code)
	}
}

func TestRF3BV1AliasConditionalRoundTripAcrossIndependentRouters(t *testing.T) {
	poolA := StartPostgres(t)
	poolB, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open replica B pool: %v", err)
	}
	t.Cleanup(poolB.Close)
	prepared := prepareRF3BTagCase("/api/v1/tags?library_kind=reading", "links")(t, poolA)
	replicaA := newRF3BConditionalReplica(poolA)
	replicaB := newRF3BConditionalReplica(poolB)

	prewarm := rf3bConditionalGET(t, replicaA.router, prepared.path, "")
	before := rf3bConditionalGET(t, replicaB.router, prepared.path, "")
	oldTag := before.Header().Get("ETag")
	if prewarm.Code != http.StatusOK || before.Code != http.StatusOK || oldTag == "" {
		t.Fatalf("v1 before status A/B=%d/%d ETag=%q body=%q", prewarm.Code, before.Code, oldTag, before.Body.String())
	}
	prepared.mutate()
	refreshed := rf3bConditionalGET(t, replicaB.router, prepared.path, oldTag)
	newTag := refreshed.Header().Get("ETag")
	if refreshed.Code != http.StatusOK || newTag == "" || newTag == oldTag ||
		bytes.Equal(before.Body.Bytes(), refreshed.Body.Bytes()) {
		t.Fatalf("v1 refresh status=%d ETag=%q body=%q", refreshed.Code, newTag, refreshed.Body.String())
	}
	if response := rf3bConditionalGET(t, replicaB.router, prepared.path, newTag); response.Code != http.StatusNotModified {
		t.Fatalf("v1 new validator status=%d, want 304", response.Code)
	}
}

type rf3bPreparedConditionalCase struct {
	path   string
	mutate func()
}

type rf3bConditionalCase struct {
	name    string
	prepare func(*testing.T, *pgxpool.Pool) rf3bPreparedConditionalCase
}

func TestRF3BEligibleRouteDependencyMatrixAcrossIndependentRouters(t *testing.T) {
	cases := []rf3bConditionalCase{
		{name: "links/list/links", prepare: prepareRF3BLinkCase("list", "links")},
		{name: "links/list/link_concept", prepare: prepareRF3BLinkCase("list", "link_concept")},
		{name: "links/list/concept", prepare: prepareRF3BLinkCase("list", "concept")},
		{name: "links/url/links", prepare: prepareRF3BLinkCase("url", "links")},
		{name: "links/url/link_concept", prepare: prepareRF3BLinkCase("url", "link_concept")},
		{name: "links/url/concept", prepare: prepareRF3BLinkCase("url", "concept")},
		{name: "links/detail-content/links", prepare: prepareRF3BLinkCase("detail-content", "links")},
		{name: "links/detail-content/link_concept", prepare: prepareRF3BLinkCase("detail-content", "link_concept")},
		{name: "links/detail-content/concept", prepare: prepareRF3BLinkCase("detail-content", "concept")},
		{name: "links/detail-metadata/links", prepare: prepareRF3BLinkCase("detail-metadata", "links")},
		{name: "links/detail-metadata/link_concept", prepare: prepareRF3BLinkCase("detail-metadata", "link_concept")},
		{name: "links/detail-metadata/concept", prepare: prepareRF3BLinkCase("detail-metadata", "concept")},
		{name: "tags/default/links", prepare: prepareRF3BTagCase("/api/tags", "links")},
		{name: "tags/reading/links", prepare: prepareRF3BTagCase("/api/tags?library_kind=reading", "links")},
		{name: "tags/site/site_tags", prepare: prepareRF3BTagCase("/api/tags?library_kind=site", "site_tags")},
		{name: "tags/all/links", prepare: prepareRF3BTagCase("/api/tags?library_kind=all", "links")},
		{name: "tags/all/site_tags", prepare: prepareRF3BTagCase("/api/tags?library_kind=all", "site_tags")},
		{name: "tree/tree/links", prepare: prepareRF3BTreeCase("/api/tree")},
		{name: "tree/domains/links", prepare: prepareRF3BTreeCase("/api/tree?view=domains")},
		{name: "tree/domains-reading/links", prepare: prepareRF3BScopedTreeCase("/api/tree?view=domains&library_kind=reading", "reading")},
		{name: "tree/domains-site/links", prepare: prepareRF3BScopedTreeCase("/api/tree?view=domains&library_kind=site", "site")},
		{name: "feed-items/list/feed_items", prepare: prepareRF3BFeedCase("feed_items")},
		{name: "feed-items/list/feed_subscriptions", prepare: prepareRF3BFeedCase("feed_subscriptions")},
		{name: "feed-items/list/links", prepare: prepareRF3BFeedCase("links")},
		{name: "sites/list/sites", prepare: prepareRF3BSiteCase("sites")},
		{name: "sites/list/site_entries", prepare: prepareRF3BSiteCase("site_entries")},
		{name: "sites/list/site_tags", prepare: prepareRF3BSiteCase("site_tags")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poolA := StartPostgres(t)
			poolB, err := pgxpool.New(t.Context(), DSN(t))
			if err != nil {
				t.Fatalf("open replica B pool: %v", err)
			}
			t.Cleanup(poolB.Close)
			prepared := tc.prepare(t, poolA)
			replicaA := newRF3BConditionalReplica(poolA)
			replicaB := newRF3BConditionalReplica(poolB)
			if replicaA.tagCache == replicaB.tagCache ||
				replicaA.domainCache == replicaB.domainCache ||
				replicaA.revisions == replicaB.revisions {
				t.Fatal("replicas unexpectedly share process-local cache or revision services")
			}

			prewarm := rf3bConditionalGET(t, replicaA.router, prepared.path, "")
			before := rf3bConditionalGET(t, replicaB.router, prepared.path, "")
			oldTag := before.Header().Get("ETag")
			if prewarm.Code != http.StatusOK || before.Code != http.StatusOK || oldTag == "" {
				t.Fatalf("prewarm status A/B=%d/%d ETag=%q body=%q", prewarm.Code, before.Code, oldTag, before.Body.String())
			}
			if prewarm.Header().Get("ETag") != oldTag || !bytes.Equal(prewarm.Body.Bytes(), before.Body.Bytes()) {
				t.Fatalf("replicas disagree before mutation: A=%q B=%q", prewarm.Body.String(), before.Body.String())
			}

			prepared.mutate()

			refreshed := rf3bConditionalGET(t, replicaB.router, prepared.path, oldTag)
			newTag := refreshed.Header().Get("ETag")
			if refreshed.Code != http.StatusOK {
				t.Fatalf("old validator status=%d, want 200; body=%q", refreshed.Code, refreshed.Body.String())
			}
			if newTag == "" || newTag == oldTag {
				t.Fatalf("new ETag=%q, want non-empty value distinct from %q", newTag, oldTag)
			}
			if bytes.Equal(before.Body.Bytes(), refreshed.Body.Bytes()) {
				t.Fatalf("dependency write did not change body: %q", refreshed.Body.String())
			}

			notModified := rf3bConditionalGET(t, replicaB.router, prepared.path, newTag)
			if notModified.Code != http.StatusNotModified {
				t.Fatalf("new validator status=%d, want 304; body=%q", notModified.Code, notModified.Body.String())
			}
		})
	}
}

func rf3bConditionalGET(t *testing.T, router http.Handler, path, validator string) *httptest.ResponseRecorder {
	t.Helper()
	return rf3bCredentialGET(t, router, path, "conditional-get-legacy-fixture", validator)
}

func rf3bCredentialGET(t *testing.T, router http.Handler, path, credential, validator string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+credential)
	if validator != "" {
		req.Header.Set("If-None-Match", validator)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func prepareRF3BLinkCase(variant, dependency string) func(*testing.T, *pgxpool.Pool) rf3bPreparedConditionalCase {
	return func(t *testing.T, pool *pgxpool.Pool) rf3bPreparedConditionalCase {
		t.Helper()
		unique := uuid.NewString()
		rawURL := "https://rf3b-links-" + unique + ".example.com/article"
		linkID := createRF3BReadingLink(t, pool, rawURL, "raw-"+unique, "rf3b.example.com")
		conceptID := uuid.New()
		if dependency == "link_concept" || dependency == "concept" {
			if _, err := pool.Exec(t.Context(), `INSERT INTO concept
				(id, primary_name, display_name)
				VALUES ($1,$2,$2)`, conceptID, "concept-before-"+unique); err != nil {
				t.Fatalf("seed concept: %v", err)
			}
		}
		if dependency == "concept" {
			if _, err := pool.Exec(t.Context(), `INSERT INTO link_concept
				(link_id,concept_id,surface_tag) VALUES ($1,$2,$3)`,
				linkID, conceptID, "surface-"+unique); err != nil {
				t.Fatalf("seed link_concept: %v", err)
			}
		}
		path := "/api/links"
		switch variant {
		case "url":
			path += "?" + url.Values{"url": {rawURL}}.Encode()
		case "detail-content":
			path += "/" + linkID.String()
		case "detail-metadata":
			path += "/" + linkID.String() + "?include_content=false"
		}
		return rf3bPreparedConditionalCase{
			path: path,
			mutate: func() {
				t.Helper()
				var err error
				switch dependency {
				case "links":
					if variant == "detail-content" {
						_, err = pool.Exec(t.Context(), `UPDATE links SET
							content=$2, content_document=$3, content_format='plain',
							content_revision=content_revision+1, content_cjk_chars=3,
							content_words=2, updated_at=NOW()
							WHERE id=$1`, linkID, "body-after-"+unique, "document-after-"+unique)
					} else {
						_, err = pool.Exec(t.Context(), `UPDATE links SET title=$2, updated_at=NOW() WHERE id=$1`, linkID, "title-after-"+unique)
					}
				case "link_concept":
					_, err = pool.Exec(t.Context(), `INSERT INTO link_concept
						(link_id,concept_id,surface_tag) VALUES ($1,$2,$3)`,
						linkID, conceptID, "surface-"+unique)
				case "concept":
					_, err = pool.Exec(t.Context(), `UPDATE concept SET display_name=$2 WHERE id=$1`, conceptID, "concept-after-"+unique)
				default:
					t.Fatalf("unknown link dependency %q", dependency)
				}
				if err != nil {
					t.Fatalf("mutate %s: %v", dependency, err)
				}
			},
		}
	}
}

func prepareRF3BTagCase(path, dependency string) func(*testing.T, *pgxpool.Pool) rf3bPreparedConditionalCase {
	return func(t *testing.T, pool *pgxpool.Pool) rf3bPreparedConditionalCase {
		t.Helper()
		unique := uuid.NewString()
		var siteID uuid.UUID
		if dependency == "site_tags" {
			siteID = seedRF3BMatrixSite(t, pool, unique)
		}
		return rf3bPreparedConditionalCase{
			path: path,
			mutate: func() {
				t.Helper()
				switch dependency {
				case "links":
					createRF3BReadingLink(t, pool,
						"https://rf3b-tag-"+unique+".example.com", "tag-after-"+unique, "rf3b-tag.example.com")
				case "site_tags":
					if _, err := pool.Exec(t.Context(), `INSERT INTO site_tags
						(site_id,tag,normalized_tag,source) VALUES ($1,$2,$2,'user')`,
						siteID, "site-tag-after-"+unique); err != nil {
						t.Fatalf("insert site tag: %v", err)
					}
				default:
					t.Fatalf("unknown tag dependency %q", dependency)
				}
			},
		}
	}
}

func prepareRF3BTreeCase(path string) func(*testing.T, *pgxpool.Pool) rf3bPreparedConditionalCase {
	return prepareRF3BScopedTreeCase(path, "")
}

func prepareRF3BScopedTreeCase(path, libraryKind string) func(*testing.T, *pgxpool.Pool) rf3bPreparedConditionalCase {
	return func(t *testing.T, pool *pgxpool.Pool) rf3bPreparedConditionalCase {
		t.Helper()
		unique := uuid.NewString()
		return rf3bPreparedConditionalCase{
			path: path,
			mutate: func() {
				t.Helper()
				linkID := mustCreateDoneLink(t, repository.NewPGXLinkRepository(pool), t.Context(),
					"https://rf3b-tree-"+unique+".example.com/article", "tree-"+unique, "rf3b-tree-"+unique+".example.com")
				if libraryKind != "" {
					if _, err := pool.Exec(t.Context(), `UPDATE links
						SET library_kind=$2, library_kind_source='user'
						WHERE id=$1`, linkID, libraryKind); err != nil {
						t.Fatalf("scope RF3B tree link as %s: %v", libraryKind, err)
					}
				}
			},
		}
	}
}

func prepareRF3BFeedCase(dependency string) func(*testing.T, *pgxpool.Pool) rf3bPreparedConditionalCase {
	return func(t *testing.T, pool *pgxpool.Pool) rf3bPreparedConditionalCase {
		t.Helper()
		unique := uuid.NewString()
		subscriptionID := uuid.New()
		if _, err := pool.Exec(t.Context(), `INSERT INTO feed_subscriptions
			(id,url,title,active,next_fetch_at) VALUES ($1,$2,$3,true,NOW())`,
			subscriptionID, "https://rf3b-feed-"+unique+".example.com/rss", "subscription-before-"+unique); err != nil {
			t.Fatalf("seed feed subscription: %v", err)
		}
		var linkID uuid.UUID
		if dependency == "links" {
			linkID = mustCreateDoneLink(t, repository.NewPGXLinkRepository(pool), t.Context(),
				"https://rf3b-feed-link-"+unique+".example.com", "feed-link-"+unique, "rf3b-feed-link.example.com")
		}
		itemID := uuid.New()
		if _, err := pool.Exec(t.Context(), `INSERT INTO feed_items
			(id,subscription_id,external_id,url,title,link_id)
			VALUES ($1,$2,$3,$4,$5,$6)`, itemID, subscriptionID,
			"external-"+unique, "https://rf3b-item-"+unique+".example.com", "item-before-"+unique, nullableUUID(linkID)); err != nil {
			t.Fatalf("seed feed item: %v", err)
		}
		return rf3bPreparedConditionalCase{
			path: "/api/feed-items",
			mutate: func() {
				t.Helper()
				var err error
				switch dependency {
				case "feed_items":
					_, err = pool.Exec(t.Context(), `UPDATE feed_items SET title=$2, updated_at=NOW() WHERE id=$1`, itemID, "item-after-"+unique)
				case "feed_subscriptions":
					_, err = pool.Exec(t.Context(), `UPDATE feed_subscriptions SET title=$2, updated_at=NOW() WHERE id=$1`, subscriptionID, "subscription-after-"+unique)
				case "links":
					_, err = pool.Exec(t.Context(), `UPDATE links SET status='failed', error_msg='rf3b', updated_at=NOW() WHERE id=$1`, linkID)
				default:
					t.Fatalf("unknown feed dependency %q", dependency)
				}
				if err != nil {
					t.Fatalf("mutate %s: %v", dependency, err)
				}
			},
		}
	}
}

func prepareRF3BSiteCase(dependency string) func(*testing.T, *pgxpool.Pool) rf3bPreparedConditionalCase {
	return func(t *testing.T, pool *pgxpool.Pool) rf3bPreparedConditionalCase {
		t.Helper()
		unique := uuid.NewString()
		siteID := seedRF3BMatrixSite(t, pool, unique)
		var siteEntryLinkID uuid.UUID
		if dependency == "site_entries" {
			siteEntryLinkID = mustCreateDoneLink(t, repository.NewPGXLinkRepository(pool), t.Context(),
				"https://rf3b-site-entry-"+unique+".example.com", "entry-"+unique, "rf3b-site-entry.example.com")
		}
		return rf3bPreparedConditionalCase{
			path: "/api/sites",
			mutate: func() {
				t.Helper()
				var err error
				switch dependency {
				case "sites":
					_, err = pool.Exec(t.Context(), `UPDATE sites SET name=$2, revision=revision+1, updated_at=NOW() WHERE id=$1`, siteID, "site-after-"+unique)
				case "site_entries":
					_, err = pool.Exec(t.Context(), `INSERT INTO site_entries
						(site_id,link_id,entry_name,entry_name_source,purpose,purpose_source,normalized_url)
						VALUES ($1,$2,$3,'user','','user',$4)`, siteID, siteEntryLinkID,
						"entry-after-"+unique, "https://rf3b-site-entry-"+unique+".example.com")
				case "site_tags":
					_, err = pool.Exec(t.Context(), `INSERT INTO site_tags
						(site_id,tag,normalized_tag,source) VALUES ($1,$2,$2,'user')`,
						siteID, "site-tag-after-"+unique)
				default:
					t.Fatalf("unknown site dependency %q", dependency)
				}
				if err != nil {
					t.Fatalf("mutate %s: %v", dependency, err)
				}
			},
		}
	}
}

func seedRF3BMatrixSite(t *testing.T, pool *pgxpool.Pool, unique string) uuid.UUID {
	t.Helper()
	siteID := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sites
		(id,site_key,name,name_source,intro_source)
		VALUES ($1,$2,$3,'user','user')`, siteID,
		"rf3b-site:"+unique, "site-before-"+unique); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	return siteID
}

func createRF3BReadingLink(t *testing.T, pool *pgxpool.Pool, rawURL, tag, domain string) uuid.UUID {
	t.Helper()
	linkID := mustCreateDoneLink(t, repository.NewPGXLinkRepository(pool), t.Context(), rawURL, tag, domain)
	if _, err := pool.Exec(t.Context(), `UPDATE links
		SET library_kind='reading', library_kind_source='user', updated_at=NOW()
		WHERE id=$1`, linkID); err != nil {
		t.Fatalf("mark matrix link reading: %v", err)
	}
	return linkID
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
