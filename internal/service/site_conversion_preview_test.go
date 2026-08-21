package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

type conversionLinksFake struct {
	repotest.BaseLinkStore
	link *model.Link
	err  error
}

func (f conversionLinksFake) GetLifecycleByID(context.Context, uuid.UUID) (*repository.LinkLifecycleProjection, error) {
	if f.link == nil {
		return nil, f.err
	}
	projection := repository.LinkLifecycleProjection{
		ID: f.link.ID, URL: f.link.URL, Status: f.link.Status,
		LibraryKind: f.link.LibraryKind, LibraryKindLocked: f.link.LibraryKindLocked,
		ContentRevision: f.link.ContentRevision, HasContent: f.link.HasContent,
	}
	return &projection, f.err
}

type conversionContentFake struct {
	content *model.SavedContent
	err     error
}

func (f conversionContentFake) GetContent(context.Context, uuid.UUID) (*model.SavedContent, error) {
	return f.content, f.err
}

type conversionTranslationsFake struct {
	rows []model.LinkTranslation
	err  error
}

func (conversionTranslationsFake) FindByIdentity(context.Context, repository.UpsertTranslationParams) (*model.LinkTranslation, error) {
	return nil, nil
}
func (f conversionTranslationsFake) ListByLink(context.Context, uuid.UUID) ([]model.LinkTranslation, error) {
	return f.rows, f.err
}
func (conversionTranslationsFake) GetByID(context.Context, uuid.UUID) (*model.LinkTranslation, error) {
	return nil, nil
}
func (conversionTranslationsFake) MarkProcessing(context.Context, model.TranslationAttempt) (*model.LinkTranslation, error) {
	return nil, nil
}
func (conversionTranslationsFake) Complete(context.Context, model.TranslationAttempt, string, string) (bool, error) {
	return false, nil
}
func (conversionTranslationsFake) Fail(context.Context, model.TranslationAttempt, string) (bool, error) {
	return false, nil
}

type conversionSiteLookupFake struct {
	candidate *repository.SiteConversionCandidate
}

func (f conversionSiteLookupFake) FindByIdentityKey(context.Context, string) (*repository.SiteConversionCandidate, error) {
	return f.candidate, nil
}

func TestConversionPreviewReadingToSiteReportsAssets(t *testing.T) {
	id := uuid.New()
	kind := model.LibraryKindReading
	link := &model.Link{ID: id, URL: "https://example.com/article", Status: model.LinkStatusDone, LibraryKind: &kind, ContentRevision: 7}
	svc := NewConversionPreviewService(conversionLinksFake{link: link}, conversionContentFake{content: &model.SavedContent{Text: "saved"}}, conversionTranslationsFake{rows: []model.LinkTranslation{{}, {}, {}}}, conversionSiteLookupFake{})

	got, err := svc.Preview(context.Background(), id.String(), dto.ConversionPreviewRequest{TargetKind: "site"})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if !got.Destructive || !got.SavedOriginal || got.TranslationCount != 3 {
		t.Fatalf("asset preview = %#v", got)
	}
	if got.AnnotationPolicy != "extract_local_note_then_hide_stale" || got.ExpectedContentRevision != 7 {
		t.Fatalf("preview = %#v", got)
	}
}

func TestConversionPreviewSiteToReadingRequiresReparse(t *testing.T) {
	id := uuid.New()
	kind := model.LibraryKindSite
	svc := NewConversionPreviewService(conversionLinksFake{link: &model.Link{ID: id, Status: model.LinkStatusDone, LibraryKind: &kind}}, conversionContentFake{}, conversionTranslationsFake{}, conversionSiteLookupFake{})
	got, err := svc.Preview(context.Background(), id.String(), dto.ConversionPreviewRequest{TargetKind: "reading"})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if !got.ReparseRequired || got.Destructive || got.AnnotationPolicy != "" {
		t.Fatalf("preview = %#v", got)
	}
}

func TestConversionPreviewReadingToSiteIncludesCASReadyMatchingSite(t *testing.T) {
	id, siteID := uuid.New(), uuid.New()
	kind := model.LibraryKindReading
	svc := NewConversionPreviewService(conversionLinksFake{link: &model.Link{ID: id, URL: "https://example.com/tool", Status: model.LinkStatusDone, LibraryKind: &kind}}, conversionContentFake{}, conversionTranslationsFake{}, conversionSiteLookupFake{candidate: &repository.SiteConversionCandidate{ID: siteID, Name: "Example", Revision: 9}})
	got, err := svc.Preview(context.Background(), id.String(), dto.ConversionPreviewRequest{TargetKind: "site"})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if got.MatchingSite == nil || got.MatchingSite.ID != siteID.String() || got.MatchingSite.Revision != 9 {
		t.Fatalf("matching site = %#v", got.MatchingSite)
	}
}

func TestConversionPreviewRejectsUnavailableOrUnchangedLink(t *testing.T) {
	id := uuid.New()
	reading := model.LibraryKindReading
	tests := []struct {
		name, raw, target string
		link              *model.Link
		wantStatus        int
		wantCode          string
	}{
		{"invalid id", "bad", "site", nil, http.StatusBadRequest, httperr.CodeInvalidLinkID},
		{"missing", id.String(), "site", nil, http.StatusNotFound, httperr.CodeLinkNotFound},
		{"not final", id.String(), "site", &model.Link{ID: id, Status: model.LinkStatusPending}, http.StatusConflict, httperr.CodeLibraryKindNotFinal},
		{"unchanged", id.String(), "reading", &model.Link{ID: id, Status: model.LinkStatusDone, LibraryKind: &reading}, http.StatusConflict, httperr.CodeConversionTargetUnchanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewConversionPreviewService(conversionLinksFake{link: tt.link}, conversionContentFake{}, conversionTranslationsFake{}, conversionSiteLookupFake{}).Preview(context.Background(), tt.raw, dto.ConversionPreviewRequest{TargetKind: tt.target})
			status, ok := httperr.As(err)
			code := ""
			if coded, yes := status.(httperr.ErrorCoder); yes {
				code = coded.HTTPErrorCode()
			}
			if !ok || status.HTTPStatus() != tt.wantStatus || code != tt.wantCode {
				t.Fatalf("Preview() error = %v, want %d %s", err, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

var _ repository.LinkLifecycleReader = conversionLinksFake{}
