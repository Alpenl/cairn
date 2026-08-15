package dbintegration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/app"
	"webtag/internal/dto"
	"webtag/internal/handler"
	"webtag/internal/repository"
	"webtag/internal/representation"
	"webtag/internal/service"
)

type conditionalGetLinkWriteStub struct{}

func (conditionalGetLinkWriteStub) Submit(context.Context, dto.LinkCreateRequest) (dto.SubmitResponse, error) {
	return dto.SubmitResponse{}, nil
}
func (conditionalGetLinkWriteStub) Refresh(context.Context, string) (dto.SubmitResponse, error) {
	return dto.SubmitResponse{}, nil
}
func (conditionalGetLinkWriteStub) Batch(context.Context, dto.BatchCreateRequest) (dto.BatchSubmitResponse, error) {
	return dto.BatchSubmitResponse{}, nil
}

type conditionalGetLinkReadStub struct{}

func (conditionalGetLinkReadStub) List(context.Context, dto.ListLinksRequest) (dto.PaginatedLinksResponse, error) {
	return dto.PaginatedLinksResponse{}, nil
}
func (conditionalGetLinkReadStub) Get(context.Context, string) (dto.LinkResponse, error) {
	return dto.LinkResponse{}, nil
}
func (conditionalGetLinkReadStub) GetWithContent(context.Context, string, bool) (dto.LinkResponse, error) {
	return dto.LinkResponse{}, nil
}
func (conditionalGetLinkReadStub) Delete(context.Context, string) error { return nil }
func (conditionalGetLinkReadStub) Export(context.Context, io.Writer) error {
	return nil
}
func (conditionalGetLinkReadStub) ExportConcepts(context.Context, io.Writer) error {
	return nil
}

type conditionalGetIngestStub struct{}

func (conditionalGetIngestStub) Ingest(context.Context, dto.IngestRequest) (dto.SubmitResponse, error) {
	return dto.SubmitResponse{}, nil
}

type conditionalGetJobStub struct{}

func (conditionalGetJobStub) Get(context.Context, string) (dto.JobResponse, error) {
	return dto.JobResponse{}, nil
}
func (conditionalGetJobStub) List(context.Context, []string) ([]dto.JobResponse, error) {
	return nil, nil
}

type conditionalGetTreeStub struct{}

func (conditionalGetTreeStub) Get(context.Context, string) (dto.TreeResponse, error) {
	return dto.TreeResponse{}, nil
}
func (conditionalGetTreeStub) ListDomains(context.Context) (dto.DomainTreeSummaryEnvelope, error) {
	return dto.DomainTreeSummaryEnvelope{Domains: []dto.DomainTreeSummaryResponse{}}, nil
}
func (conditionalGetTreeStub) ListDomainsScoped(_ context.Context, scope string) (dto.DomainTreeSummaryEnvelope, error) {
	return dto.DomainTreeSummaryEnvelope{
		Domains:     []dto.DomainTreeSummaryResponse{},
		LibraryKind: &scope,
	}, nil
}

func conditionalGetDependencies(tags handler.TagService) handler.Dependencies {
	return handler.Dependencies{
		LinksWrite: conditionalGetLinkWriteStub{},
		LinksRead:  conditionalGetLinkReadStub{},
		Ingest:     conditionalGetIngestStub{},
		Jobs:       conditionalGetJobStub{},
		Tags:       tags,
		Tree:       conditionalGetTreeStub{},
	}
}

func getLibraryTags(t *testing.T, router http.Handler, target, validator string) (*httptest.ResponseRecorder, []dto.TagCountResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer conditional-get-legacy-fixture")
	if validator != "" {
		req.Header.Set("If-None-Match", validator)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var body []dto.TagCountResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode tag response: %v", err)
		}
	}
	return rec, body
}

func getSiteLibraryTags(t *testing.T, router http.Handler, validator string) (*httptest.ResponseRecorder, []dto.TagCountResponse) {
	t.Helper()
	return getLibraryTags(t, router, "/api/tags?library_kind=site", validator)
}

func getReadingLibraryTags(t *testing.T, router http.Handler, validator string) (*httptest.ResponseRecorder, []dto.TagCountResponse) {
	t.Helper()
	return getLibraryTags(t, router, "/api/tags", validator)
}

func insertSiteTag(t *testing.T, pool *pgxpool.Pool, tag string) {
	t.Helper()
	siteID := uuid.New()
	if _, err := pool.Exec(t.Context(), `INSERT INTO sites
		(id, site_key, name, name_source, intro_source)
		VALUES ($1, $2, $3, 'user', 'user')`,
		siteID, "conditional-get:"+siteID.String(), "Conditional GET "+tag); err != nil {
		t.Fatalf("insert site: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO site_tags
		(site_id, tag, normalized_tag, source)
		VALUES ($1, $2, $2, 'user')`, siteID, tag); err != nil {
		t.Fatalf("insert site tag: %v", err)
	}
}

func hasTag(items []dto.TagCountResponse, tag string) bool {
	for _, item := range items {
		if item.Tag == tag {
			return true
		}
	}
	return false
}

func TestConditionalGetProductionPolicyInvalidatesSiteTagRepresentations(t *testing.T) {
	pool := StartPostgres(t)
	tagService := service.NewTagReadService(repository.NewPGXTagRepository(pool), nil)
	revisionStore := repository.NewPGXReadRevisionRepository(pool)
	revisions := service.NewReadRevisionService(revisionStore)
	deps := conditionalGetDependencies(tagService)
	libraryComponents, err := representation.NewComponentSet(representation.LibraryComponent)
	if err != nil {
		t.Fatalf("NewComponentSet(library): %v", err)
	}
	productionRouter := app.NewRouterWithDependencies(deps, nil, nil, nil, nil, app.RouterOptions{
		AppEnv:                  "dev",
		ExtensionAPIToken:       "conditional-get-legacy-fixture",
		ConditionalGetRevisions: revisions,
	})

	beforeBase, err := revisionStore.Current(t.Context(), libraryComponents)
	if err != nil {
		t.Fatalf("read initial revision: %v", err)
	}
	first, firstBody := getSiteLibraryTags(t, productionRouter, "")
	if first.Code != http.StatusOK {
		t.Fatalf("initial status = %d, want 200", first.Code)
	}
	oldTag := first.Header().Get("ETag")
	if oldTag == "" {
		t.Fatal("production response did not publish an ETag")
	}

	insertSiteTag(t, pool, "conditional-exact")
	afterBase, err := revisionStore.Current(t.Context(), libraryComponents)
	if err != nil {
		t.Fatalf("read revision after site tag write: %v", err)
	}
	beforeRevision := beforeBase.Components[0].Revision
	afterRevision := afterBase.Components[0].Revision
	if afterRevision <= beforeRevision {
		t.Fatalf("site_tags did not advance library_read_revision: %d -> %d", beforeRevision, afterRevision)
	}

	exact, exactBody := getSiteLibraryTags(t, productionRouter, oldTag)
	if exact.Code != http.StatusOK {
		t.Fatalf("old exact validator status = %d, want 200", exact.Code)
	}
	newTag := exact.Header().Get("ETag")
	if newTag == "" || newTag == oldTag {
		t.Fatalf("exact response ETag = %q, want non-empty value distinct from %q", newTag, oldTag)
	}
	if hasTag(firstBody, "conditional-exact") || !hasTag(exactBody, "conditional-exact") {
		t.Fatalf("site tag body did not change across write: before=%#v after=%#v", firstBody, exactBody)
	}

	notModified, _ := getSiteLibraryTags(t, productionRouter, newTag)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("new validator status = %d, want 304", notModified.Code)
	}
}

func TestConditionalGetAcrossIndependentRoutersBindsNewBodyToNewVersion(t *testing.T) {
	poolA := StartPostgres(t)
	poolB, err := pgxpool.New(t.Context(), DSN(t))
	if err != nil {
		t.Fatalf("open second replica pool: %v", err)
	}
	t.Cleanup(poolB.Close)

	type replica struct {
		router    http.Handler
		tagCache  *service.TagCache
		revisions *service.ReadRevisionService
		links     *repository.PGXLinkRepository
	}
	newReplica := func(pool *pgxpool.Pool) replica {
		tagCache := service.NewTagCache(time.Hour, nil)
		revisions := service.NewReadRevisionService(repository.NewPGXReadRevisionRepository(pool))
		tags := service.NewTagReadService(repository.NewPGXTagRepository(pool), tagCache)
		router := app.NewRouterWithDependencies(
			conditionalGetDependencies(tags),
			nil,
			nil,
			nil,
			nil,
			app.RouterOptions{
				AppEnv:                  "prod",
				ExtensionAPIToken:       "conditional-get-legacy-fixture",
				ConditionalGetRevisions: revisions,
			},
		)
		return replica{
			router:    router,
			tagCache:  tagCache,
			revisions: revisions,
			links:     repository.NewPGXLinkRepository(pool),
		}
	}
	replicaA := newReplica(poolA)
	replicaB := newReplica(poolB)
	if replicaA.tagCache == replicaB.tagCache || replicaA.revisions == replicaB.revisions || replicaA.links == replicaB.links {
		t.Fatal("replicas unexpectedly share process-local services or repositories")
	}

	tag := "rf3a-" + uuid.NewString()
	prewarmed, beforeBody := getReadingLibraryTags(t, replicaB.router, "")
	oldETag := prewarmed.Header().Get("ETag")
	if prewarmed.Code != http.StatusOK || oldETag == "" {
		t.Fatalf("prewarm status=%d ETag=%q body=%q", prewarmed.Code, oldETag, prewarmed.Body.String())
	}
	if hasTag(beforeBody, tag) {
		t.Fatalf("prewarm body already contains unique tag %q", tag)
	}

	mustCreateDoneLink(
		t,
		replicaA.links,
		t.Context(),
		"https://rf3a-"+uuid.NewString()+".example.com/article",
		tag,
		"rf3a.example.com",
	)

	refreshed, afterBody := getReadingLibraryTags(t, replicaB.router, oldETag)
	newETag := refreshed.Header().Get("ETag")
	if refreshed.Code != http.StatusOK {
		t.Fatalf("old ETag after write status=%d, want 200; body=%q", refreshed.Code, refreshed.Body.String())
	}
	if newETag == "" || newETag == oldETag {
		t.Fatalf("ETag after write = %q, want non-empty value distinct from %q", newETag, oldETag)
	}
	if !hasTag(afterBody, tag) {
		t.Fatalf("new body does not contain written tag %q: %#v", tag, afterBody)
	}

	notModified, _ := getReadingLibraryTags(t, replicaB.router, newETag)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("new ETag status=%d, want 304; body=%q", notModified.Code, notModified.Body.String())
	}
}
