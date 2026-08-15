package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository/repotest"
)

type conversionWriterFake struct {
	params ConvertLinkCommand
	result ConvertLinkResult
	err    error
}

func (f *conversionWriterFake) ConvertLink(_ context.Context, params ConvertLinkCommand) (ConvertLinkResult, error) {
	f.params = params
	return f.result, f.err
}

func conversionExecuteErrorCode(t *testing.T, err error, wantStatus int, wantCode string) {
	t.Helper()
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != wantStatus {
		t.Fatalf("error = %v, want HTTP %d", err, wantStatus)
	}
	coded, ok := carrier.(httperr.ErrorCoder)
	if !ok || coded.HTTPErrorCode() != wantCode {
		t.Fatalf("error = %v, want code %s", err, wantCode)
	}
}

func TestConversionExecuteRequiresConfirmationAndCurrentRevision(t *testing.T) {
	id := uuid.New()
	kind := model.LibraryKindReading
	links := conversionLinksFake{link: &model.Link{ID: id, Status: model.LinkStatusDone, LibraryKind: &kind, ContentRevision: 4}}
	writer := &conversionWriterFake{}
	svc := NewConversionExecuteService(links, writer)
	_, err := svc.Execute(context.Background(), id.String(), dto.ConversionExecuteRequest{TargetKind: "site", ExpectedContentRevision: 4})
	conversionExecuteErrorCode(t, err, http.StatusConflict, httperr.CodeDestructiveConfirmationRequired)
	if writer.params.LinkID != uuid.Nil {
		t.Fatal("writer called before destructive confirmation")
	}
	_, err = svc.Execute(context.Background(), id.String(), dto.ConversionExecuteRequest{TargetKind: "site", ExpectedContentRevision: 3, ConfirmDestructive: true})
	conversionExecuteErrorCode(t, err, http.StatusConflict, httperr.CodeRevisionConflict)
}

func TestConversionExecuteRejectsReadingToSiteWhenSiteWritesDisabled(t *testing.T) {
	id := uuid.New()
	kind := model.LibraryKindReading
	writer := &conversionWriterFake{}
	svc := NewConversionExecuteServiceWithOptions(ConversionExecuteServiceOptions{
		Links: conversionLinksFake{link: &model.Link{ID: id, Status: model.LinkStatusDone, LibraryKind: &kind, ContentRevision: 4}}, Commands: writer,
		DisableSiteLibraryWrite: true,
	})
	_, err := svc.Execute(context.Background(), id.String(), dto.ConversionExecuteRequest{TargetKind: "site", ExpectedContentRevision: 4, ConfirmDestructive: true})
	conversionExecuteErrorCode(t, err, http.StatusServiceUnavailable, "site_library_write_disabled")
	if writer.params.LinkID != uuid.Nil {
		t.Fatal("writer called while site writes were disabled")
	}
}

func TestConversionExecutePassesCASAndReturnsStructuredTarget(t *testing.T) {
	id, siteID, entryID := uuid.New(), uuid.New(), uuid.New()
	kind := model.LibraryKindReading
	note := "  preserved note  "
	target := siteID.String()
	revision := int64(8)
	writer := &conversionWriterFake{result: ConvertLinkResult{LinkID: id, Kind: model.LibraryKindSite, ContentRevision: 5, Status: model.LinkStatusDone, SiteID: &siteID, SiteRevision: &revision, EntryID: &entryID}}
	svc := NewConversionExecuteService(conversionLinksFake{link: &model.Link{ID: id, Status: model.LinkStatusDone, LibraryKind: &kind, ContentRevision: 4}}, writer)
	got, err := svc.Execute(context.Background(), id.String(), dto.ConversionExecuteRequest{TargetKind: "site", ExpectedContentRevision: 4, TargetSiteID: &target, ExpectedSiteRevision: &revision, ConfirmDestructive: true, PreservedUserNote: &note})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if writer.params.TargetSiteID == nil || *writer.params.TargetSiteID != siteID || writer.params.PreservedUserNote == nil || *writer.params.PreservedUserNote != "preserved note" {
		t.Fatalf("params = %#v", writer.params)
	}
	if got.ReaderTarget.View != "sites" || got.ReaderTarget.SiteID != siteID.String() || got.ReaderTarget.EntryID != entryID.String() || got.Status != "done" {
		t.Fatalf("response = %#v", got)
	}
}

func TestConversionExecuteSiteToReadingRequiresSourceSiteRevision(t *testing.T) {
	id := uuid.New()
	kind := model.LibraryKindSite
	svc := NewConversionExecuteService(conversionLinksFake{link: &model.Link{ID: id, Status: model.LinkStatusDone, LibraryKind: &kind, ContentRevision: 4}}, &conversionWriterFake{})
	_, err := svc.Execute(context.Background(), id.String(), dto.ConversionExecuteRequest{TargetKind: "reading", ExpectedContentRevision: 4})
	// The repository must know which site aggregate is being modified. Its
	// revision is required before executing, even though no target_site_id is.
	conversionExecuteErrorCode(t, err, http.StatusConflict, httperr.CodeRevisionConflict)
}

var _ LinkConversionCommands = (*conversionWriterFake)(nil)
var _ = repotest.BaseLinkStore{}
