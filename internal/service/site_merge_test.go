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
)

type siteMergeExecuteFake struct {
	siteMergeReaderFake
	params repository.ExecuteSiteMergeParams
	result repository.SiteMergeResult
	err    error
}

func (f *siteMergeExecuteFake) ExecuteSiteMerge(_ context.Context, params repository.ExecuteSiteMergeParams) (repository.SiteMergeResult, error) {
	f.params = params
	return f.result, f.err
}

type siteMergeReaderFake struct {
	details map[uuid.UUID]*repository.SiteDetail
}

func (f siteMergeReaderFake) GetSite(_ context.Context, id uuid.UUID) (*repository.SiteDetail, error) {
	return f.details[id], nil
}
func (f siteMergeReaderFake) ExecuteSiteMerge(context.Context, repository.ExecuteSiteMergeParams) (repository.SiteMergeResult, error) {
	panic("unexpected site merge execution")
}

func TestSiteMergePreviewExposesValueConflictsAndDuplicates(t *testing.T) {
	targetID, sourceID := uuid.New(), uuid.New()
	target := &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: targetID, Name: "Target", Intro: "target intro", UserNote: "target note", Revision: 4}}, Entries: []model.SiteEntry{{ID: uuid.New(), SiteID: targetID, LinkID: uuid.New(), EntryName: "Home", NormalizedURL: "https://example.com/"}}, Tags: []model.SiteTag{{Tag: "Go", NormalizedTag: "go"}}, Identities: []model.SiteIdentity{{IdentityKey: "v1:host:example.com"}}}
	source := &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: sourceID, Name: "Source", Intro: "source intro", UserNote: "source note", Revision: 7}}, Entries: []model.SiteEntry{{ID: uuid.New(), SiteID: sourceID, LinkID: uuid.New(), EntryName: "Duplicate", NormalizedURL: "https://example.com/"}, {ID: uuid.New(), SiteID: sourceID, LinkID: uuid.New(), EntryName: "Docs", NormalizedURL: "https://example.com/docs"}}, Tags: []model.SiteTag{{Tag: "Tools", NormalizedTag: "tools"}}, Identities: []model.SiteIdentity{{IdentityKey: "v1:host:docs.example.com"}}}
	service := NewSiteMergeService(siteMergeReaderFake{details: map[uuid.UUID]*repository.SiteDetail{targetID: target, sourceID: source}})
	got, err := service.Preview(context.Background(), dto.SiteMergePreviewRequest{TargetSiteID: targetID.String(), TargetRevision: 4, Sources: []dto.SiteRevisionRef{{SiteID: sourceID.String(), Revision: 7}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 3 || !got.Entries[1].Duplicate {
		t.Fatalf("entries=%#v", got.Entries)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "Go" || got.Tags[1] != "Tools" {
		t.Fatalf("tags=%v", got.Tags)
	}
	if len(got.IdentityKeys) != 2 || got.IdentityKeys[0] != "v1:host:docs.example.com" {
		t.Fatalf("identities=%v", got.IdentityKeys)
	}
	if !got.RequiresResolution || len(got.FieldConflicts) != 3 || got.FieldConflicts[0].Field != "name" || got.FieldConflicts[1].Field != "intro" || got.FieldConflicts[2].Field != "user_note" {
		t.Fatalf("conflicts=%#v", got.FieldConflicts)
	}
}

func TestSiteMergeExecuteRequiresEveryPreviewConflictToBeResolved(t *testing.T) {
	targetID, sourceID := uuid.New(), uuid.New()
	target := &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: targetID, Name: "Target", Revision: 4}}}
	source := &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: sourceID, Name: "Source", Revision: 7}}}
	fake := &siteMergeExecuteFake{siteMergeReaderFake: siteMergeReaderFake{details: map[uuid.UUID]*repository.SiteDetail{targetID: target, sourceID: source}}}
	service := NewSiteMergeService(fake)
	_, err := service.Execute(context.Background(), dto.SiteMergeExecuteRequest{TargetSiteID: targetID.String(), TargetRevision: 4, Sources: []dto.SiteRevisionRef{{SiteID: sourceID.String(), Revision: 7}}})
	carrier, ok := httperr.As(err)
	if !ok || carrier.HTTPStatus() != http.StatusConflict {
		t.Fatalf("error = %v, want conflict", err)
	}
	if fake.params.TargetID != uuid.Nil {
		t.Fatal("writer called without conflict resolution")
	}
}

func TestSiteMergeExecuteAppliesChosenSourceValueAndPassesCAS(t *testing.T) {
	targetID, sourceID := uuid.New(), uuid.New()
	target := &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: targetID, Name: "Target", Revision: 4}}}
	source := &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: sourceID, Name: "Source", Revision: 7}}}
	fake := &siteMergeExecuteFake{siteMergeReaderFake: siteMergeReaderFake{details: map[uuid.UUID]*repository.SiteDetail{targetID: target, sourceID: source}}, result: repository.SiteMergeResult{SiteID: targetID, Revision: 5, MovedEntries: 2, DeletedLinks: 1}}
	service := NewSiteMergeService(fake)
	got, err := service.Execute(context.Background(), dto.SiteMergeExecuteRequest{TargetSiteID: targetID.String(), TargetRevision: 4, Sources: []dto.SiteRevisionRef{{SiteID: sourceID.String(), Revision: 7}}, Resolutions: []dto.SiteMergeFieldResolutionRequest{{Field: "name", Choice: "source", SourceSiteID: sourceID.String()}}})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if fake.params.TargetID != targetID || fake.params.TargetRevision != 4 || len(fake.params.Sources) != 1 || fake.params.Sources[0].ID != sourceID || fake.params.Name == nil || *fake.params.Name != "Source" {
		t.Fatalf("writer params = %#v", fake.params)
	}
	if got.SiteID != targetID.String() || got.Revision != 5 || got.MovedEntries != 2 || got.DeletedLinks != 1 {
		t.Fatalf("response = %#v", got)
	}
}
