package app

import (
	"context"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/handler"
)

// NewRouter returns a test-only router with zero-behavior handler services.
func NewRouter() *gin.Engine {
	return NewRouterWithDependencies(handler.Dependencies{
		LinksWrite: smokeLinkWriteService{},
		LinksRead:  smokeLinkReadService{},
		Ingest:     smokeIngestService{},
		Tags:       smokeTagService{},
		Tree:       smokeTreeService{},
		Feeds:      smokeFeedService{},
	}, nil, nil, RouterOptions{})
}

// Including the narrow handler interface supplies a zero-behavior method set;
// smoke tests only enumerate routes and never invoke these methods.
type smokeFeedService struct{ handler.FeedService }

type smokeLinkWriteService struct{}

func (smokeLinkWriteService) Submit(context.Context, dto.LinkCreateRequest) (dto.SubmitResponse, error) {
	return dto.SubmitResponse{}, nil
}
func (smokeLinkWriteService) Refresh(context.Context, string) (dto.SubmitResponse, error) {
	return dto.SubmitResponse{}, nil
}

type smokeLinkContentService struct{}

func (smokeLinkContentService) Save(context.Context, string) (dto.LinkContentResponse, error) {
	return dto.LinkContentResponse{}, nil
}
func (smokeLinkContentService) Replace(context.Context, string) (dto.LinkContentResponse, error) {
	return dto.LinkContentResponse{}, nil
}
func (smokeLinkContentService) Edit(context.Context, string, dto.ContentEditRequest) (dto.LinkContentResponse, error) {
	return dto.LinkContentResponse{}, nil
}
func (smokeLinkContentService) Get(context.Context, string) (dto.LinkContentResponse, error) {
	return dto.LinkContentResponse{}, nil
}

type smokeLinkReadService struct{}

func (smokeLinkReadService) List(context.Context, dto.ListLinksRequest) (dto.PaginatedLinksResponse, error) {
	return dto.PaginatedLinksResponse{}, nil
}
func (smokeLinkReadService) Get(context.Context, string) (dto.LinkResponse, error) {
	return dto.LinkResponse{}, nil
}
func (smokeLinkReadService) GetWithContent(context.Context, string, bool) (dto.LinkResponse, error) {
	return dto.LinkResponse{}, nil
}
func (smokeLinkReadService) Delete(context.Context, string) error { return nil }

type smokeIngestService struct{}

func (smokeIngestService) Ingest(context.Context, dto.IngestRequest) (dto.SubmitResponse, error) {
	return dto.SubmitResponse{}, nil
}

type smokeTagService struct{}

func (smokeTagService) List(context.Context) ([]dto.TagCountResponse, error) {
	return nil, nil
}

type smokeTreeService struct{}

func (smokeTreeService) Get(context.Context, string) (dto.TreeResponse, error) {
	return dto.TreeResponse{}, nil
}
func (smokeTreeService) ListDomains(context.Context) (dto.DomainTreeSummaryEnvelope, error) {
	return dto.DomainTreeSummaryEnvelope{Domains: []dto.DomainTreeSummaryResponse{}}, nil
}
func (smokeTreeService) ListDomainsScoped(context.Context, string) (dto.DomainTreeSummaryEnvelope, error) {
	return dto.DomainTreeSummaryEnvelope{Domains: []dto.DomainTreeSummaryResponse{}}, nil
}
