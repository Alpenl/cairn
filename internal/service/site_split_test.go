package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type siteSplitFake struct {
	siteMergeReaderFake
	params repository.ExecuteSiteSplitParams
	result repository.SiteSplitResult
	err    error
}

func (f *siteSplitFake) ExecuteSiteSplit(_ context.Context, params repository.ExecuteSiteSplitParams) (repository.SiteSplitResult, error) {
	f.params = params
	return f.result, f.err
}

func TestSiteSplitPreviewReturnsNormalizedFinalPayloadAndIdentityOwnership(t *testing.T) {
	siteID, remainingID, movedID := uuid.New(), uuid.New(), uuid.New()
	githubIdentity := "v1:github:acme/docs"
	site := &repository.SiteDetail{
		SiteListItem: repository.SiteListItem{Site: model.Site{ID: siteID, Revision: 4}},
		Entries: []model.SiteEntry{
			{ID: remainingID, SiteID: siteID, LinkID: uuid.New(), EntryName: "Home", NormalizedURL: "https://acme.test/"},
			{ID: movedID, SiteID: siteID, LinkID: uuid.New(), EntryName: "Docs", NormalizedURL: "https://github.com/Acme/Docs/tree/main"},
		},
		Identities: []model.SiteIdentity{{IdentityKey: "v1:host:acme.test"}, {IdentityKey: githubIdentity}},
		Tags:       []model.SiteTag{{Tag: "Tools", Source: model.FieldSourceUser}},
	}
	svc := NewSiteSplitService(&siteSplitFake{siteMergeReaderFake: siteMergeReaderFake{details: map[uuid.UUID]*repository.SiteDetail{siteID: site}}})
	intro, homepage, icon, note := "  API docs  ", " https://docs.acme.test/ ", " https://docs.acme.test/icon.png ", "  team note  "
	request := dto.SiteSplitRequest{ExpectedRevision: 4, EntryIDs: []string{movedID.String()}, Name: "  Acme Docs  ", Intro: &intro, HomepageURL: &homepage, IconURL: &icon, UserNote: &note, PrimaryEntryID: movedID.String(), IdentityKeysForNewSite: []string{githubIdentity}}

	got, err := svc.Preview(context.Background(), siteID.String(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Payload.Name != "Acme Docs" || got.Payload.Intro == nil || *got.Payload.Intro != "API docs" || got.Payload.HomepageURL == nil || *got.Payload.HomepageURL != "https://docs.acme.test/" || got.Payload.IconURL == nil || *got.Payload.IconURL != "https://docs.acme.test/icon.png" || got.Payload.UserNote == nil || *got.Payload.UserNote != "team note" {
		t.Fatalf("normalized payload=%#v", got.Payload)
	}
	if got.Payload.ExpectedRevision != got.SourceRevision || len(got.Entries) != 1 || got.Entries[0].ID != movedID.String() || len(got.UserTags) != 1 {
		t.Fatalf("preview=%#v", got)
	}
	if len(got.Identities) != 2 || got.Identities[0].IdentityKey != githubIdentity || !got.Identities[0].EligibleForNewSite || got.Identities[0].Owner != "new_site" || got.Identities[1].EligibleForNewSite || got.Identities[1].Owner != "source" {
		t.Fatalf("identity ownership=%#v", got.Identities)
	}
}

func TestSiteSplitExecuteUsesThePreviewPayloadAndDefaultsIdentityToSource(t *testing.T) {
	siteID, remainingID, movedID := uuid.New(), uuid.New(), uuid.New()
	site := &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: siteID, Revision: 4}}, Entries: []model.SiteEntry{{ID: remainingID, SiteID: siteID, NormalizedURL: "https://x.test/"}, {ID: movedID, SiteID: siteID, NormalizedURL: "https://docs.x.test/"}}, Identities: []model.SiteIdentity{{IdentityKey: "v1:host:docs.x.test"}}}
	newID := uuid.New()
	fake := &siteSplitFake{siteMergeReaderFake: siteMergeReaderFake{details: map[uuid.UUID]*repository.SiteDetail{siteID: site}}, result: repository.SiteSplitResult{SourceID: siteID, SourceRevision: 5, NewSiteID: newID, NewRevision: 1, MovedEntries: 1}}
	svc := NewSiteSplitService(fake)
	intro := "Separate profile"
	request := dto.SiteSplitRequest{ExpectedRevision: 4, EntryIDs: []string{movedID.String()}, Name: "Separated", Intro: &intro, PrimaryEntryID: movedID.String()}
	preview, err := svc.Preview(context.Background(), siteID.String(), request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Execute(context.Background(), siteID.String(), preview.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if fake.params.IdentityKeyForNewSite != nil || fake.params.Name != "Separated" || fake.params.Intro == nil || *fake.params.Intro != intro || len(fake.params.EntryIDs) != 1 || got.NewSiteID != newID.String() {
		t.Fatalf("params=%#v response=%#v", fake.params, got)
	}
}

func TestSiteSplitExecuteTransfersEligibleSelectedIdentity(t *testing.T) {
	siteID, remainingID, movedID := uuid.New(), uuid.New(), uuid.New()
	identity := "v1:github:acme/docs"
	site := &repository.SiteDetail{
		SiteListItem: repository.SiteListItem{Site: model.Site{ID: siteID, Revision: 4}},
		Entries: []model.SiteEntry{
			{ID: remainingID, SiteID: siteID, NormalizedURL: "https://source.test/"},
			{ID: movedID, SiteID: siteID, NormalizedURL: "https://github.com/acme/docs/tree/main"},
		},
		Identities: []model.SiteIdentity{{IdentityKey: identity}},
	}
	fake := &siteSplitFake{
		siteMergeReaderFake: siteMergeReaderFake{details: map[uuid.UUID]*repository.SiteDetail{siteID: site}},
		result:              repository.SiteSplitResult{SourceID: siteID, SourceRevision: 5, NewSiteID: uuid.New(), NewRevision: 1, MovedEntries: 1},
	}
	svc := NewSiteSplitService(fake)
	preview, err := svc.Preview(context.Background(), siteID.String(), dto.SiteSplitRequest{
		ExpectedRevision:       4,
		EntryIDs:               []string{movedID.String()},
		Name:                   "Acme Docs",
		PrimaryEntryID:         movedID.String(),
		IdentityKeysForNewSite: []string{identity},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(context.Background(), siteID.String(), preview.Payload); err != nil {
		t.Fatal(err)
	}
	if fake.params.IdentityKeyForNewSite == nil || *fake.params.IdentityKeyForNewSite != identity {
		t.Fatalf("identity transfer params=%#v", fake.params)
	}
}

func TestSiteSplitRejectsMultipleOrIneligibleIdentitySelections(t *testing.T) {
	siteID, remainingID, movedID := uuid.New(), uuid.New(), uuid.New()
	site := &repository.SiteDetail{
		SiteListItem: repository.SiteListItem{Site: model.Site{ID: siteID, Revision: 4}},
		Entries:      []model.SiteEntry{{ID: remainingID, SiteID: siteID, NormalizedURL: "https://source.test/"}, {ID: movedID, SiteID: siteID, NormalizedURL: "https://moved.test/docs"}},
		Identities:   []model.SiteIdentity{{IdentityKey: "v1:host:source.test"}, {IdentityKey: "v1:host:moved.test"}},
	}
	svc := NewSiteSplitService(&siteSplitFake{siteMergeReaderFake: siteMergeReaderFake{details: map[uuid.UUID]*repository.SiteDetail{siteID: site}}})
	base := dto.SiteSplitRequest{ExpectedRevision: 4, EntryIDs: []string{movedID.String()}, Name: "Moved", PrimaryEntryID: movedID.String()}

	multiple := base
	multiple.IdentityKeysForNewSite = []string{"v1:host:moved.test", "v1:host:source.test"}
	if _, err := svc.Preview(context.Background(), siteID.String(), multiple); err == nil {
		t.Fatal("multiple identities were accepted")
	}
	ineligible := base
	ineligible.IdentityKeysForNewSite = []string{"v1:host:source.test"}
	if _, err := svc.Preview(context.Background(), siteID.String(), ineligible); err == nil {
		t.Fatal("identity unrelated to moved entries was accepted")
	}
}

func TestSiteSplitStaleRevisionReturnsConflict(t *testing.T) {
	siteID, remainingID, movedID := uuid.New(), uuid.New(), uuid.New()
	site := &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: siteID, Revision: 5}}, Entries: []model.SiteEntry{{ID: remainingID}, {ID: movedID}}}
	svc := NewSiteSplitService(&siteSplitFake{siteMergeReaderFake: siteMergeReaderFake{details: map[uuid.UUID]*repository.SiteDetail{siteID: site}}})
	_, err := svc.Preview(context.Background(), siteID.String(), dto.SiteSplitRequest{ExpectedRevision: 4, EntryIDs: []string{movedID.String()}, Name: "Moved", PrimaryEntryID: movedID.String()})
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusConflict {
		t.Fatalf("stale preview error=%v", err)
	}
}

var _ repository.SiteSplitWriter = (*siteSplitFake)(nil)
