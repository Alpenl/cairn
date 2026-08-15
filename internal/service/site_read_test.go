package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type siteReadFake struct {
	items        []repository.SiteListItem
	total        int
	detail       *repository.SiteDetail
	err          error
	listFilter   repository.SiteListFilter
	listFilters  []repository.SiteListFilter
	gotID        uuid.UUID
	relatedHosts []string
	relatedTags  []string
	relatedLimit int
	related      []repository.RelatedReading
	relatedErr   error
}

type siteProfileWriterFake struct {
	updated           bool
	err               error
	params            repository.UpdateSiteProfileParams
	entryParams       repository.UpdateSiteEntryParams
	primaryParams     repository.SetSitePrimaryEntryParams
	deleteEntryParams repository.DeleteSiteEntryParams
	deleteParams      repository.DeleteSiteParams
	deletedEntry      repository.SiteEntryDeleteResult
	tagParams         repository.UpdateSiteProfileParams
}

func (f *siteProfileWriterFake) UpdateSiteProfile(_ context.Context, params repository.UpdateSiteProfileParams) (bool, error) {
	f.params = params
	return f.updated, f.err
}
func (f *siteProfileWriterFake) UpdateSiteProfileAndTags(_ context.Context, params repository.UpdateSiteProfileParams) (bool, error) {
	f.tagParams = params
	return f.updated, f.err
}
func (f *siteProfileWriterFake) UpdateSiteEntry(_ context.Context, params repository.UpdateSiteEntryParams) (bool, error) {
	f.entryParams = params
	return f.updated, f.err
}
func (f *siteProfileWriterFake) SetSitePrimaryEntry(_ context.Context, params repository.SetSitePrimaryEntryParams) (bool, error) {
	f.primaryParams = params
	return f.updated, f.err
}
func (f *siteProfileWriterFake) DeleteSiteEntry(_ context.Context, params repository.DeleteSiteEntryParams) (repository.SiteEntryDeleteResult, error) {
	f.deleteEntryParams = params
	return f.deletedEntry, f.err
}
func (f *siteProfileWriterFake) DeleteSite(_ context.Context, params repository.DeleteSiteParams) (bool, error) {
	f.deleteParams = params
	return f.updated, f.err
}

func (f *siteReadFake) ListSites(_ context.Context, filter repository.SiteListFilter) ([]repository.SiteListItem, int, error) {
	f.listFilter = filter
	f.listFilters = append(f.listFilters, filter)
	return f.items, f.total, f.err
}

func (f *siteReadFake) GetSite(_ context.Context, id uuid.UUID) (*repository.SiteDetail, error) {
	f.gotID = id
	return f.detail, f.err
}

func (f *siteReadFake) ListRelatedReadings(_ context.Context, hosts, tags []string, limit int) ([]repository.RelatedReading, error) {
	f.relatedHosts = append([]string(nil), hosts...)
	f.relatedTags = append([]string(nil), tags...)
	f.relatedLimit = limit
	return f.related, f.relatedErr
}

func TestSiteReadServiceGetPreservesAggregateFields(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0).UTC()
	siteID, entryID, linkID := uuid.New(), uuid.New(), uuid.New()
	primaryURL := "https://example.com/tools"
	homepageURL := "https://example.com/"
	store := &siteReadFake{detail: &repository.SiteDetail{
		SiteListItem: repository.SiteListItem{
			Site:        model.Site{ID: siteID, Name: "Example", Intro: "Useful tools", HomepageURL: &homepageURL, UserNote: "keep", GroupingLocked: true, Revision: 4, FirstCollectedAt: now, LastCollectedAt: now, PrimaryEntryID: &entryID},
			DisplayHost: "example.com", Tags: []string{"go", "tools"}, EntryCount: 3, PrimaryEntryURL: &primaryURL,
		},
		Tags:    []model.SiteTag{{Tag: "go", Source: model.FieldSourceAuto}},
		Entries: []model.SiteEntry{{ID: entryID, LinkID: linkID, EntryName: "Tools", EntryNameSource: model.FieldSourceUser, Purpose: "Work", PurposeSource: model.FieldSourceAuto, NormalizedURL: primaryURL, FirstCollectedAt: now}},
	}}

	got, err := NewSiteReadService(store).Get(context.Background(), siteID.String())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.DisplayHost != "example.com" || got.EntryCount != 3 || len(got.Tags) != 2 {
		t.Fatalf("aggregate detail fields lost: %#v", got.SiteListItemResponse)
	}
	if got.PrimaryEntry == nil || got.PrimaryEntry.ID != entryID.String() || got.PrimaryEntry.URL != primaryURL {
		t.Fatalf("primary entry = %#v", got.PrimaryEntry)
	}
	if got.HomepageURL == nil || *got.HomepageURL != homepageURL {
		t.Fatalf("HomepageURL = %#v", got.HomepageURL)
	}
	if got.UserNote != "keep" || !got.GroupingLocked || len(got.TagsWithSource) != 1 || len(got.Entries) != 1 {
		t.Fatalf("detail metadata = %#v", got)
	}
}

func TestSiteReadServiceGetSerializesEmptyDetailCollectionsAsArrays(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0).UTC()
	siteID := uuid.New()
	store := &siteReadFake{detail: &repository.SiteDetail{
		SiteListItem: repository.SiteListItem{Site: model.Site{ID: siteID, FirstCollectedAt: now, LastCollectedAt: now}},
	}}

	got, err := NewSiteReadService(store).Get(context.Background(), siteID.String())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tags", "tags_with_source", "entries", "related_readings"} {
		if _, ok := decoded[key].([]any); !ok {
			t.Fatalf("%s must serialize as an array, got %s", key, body)
		}
	}
}

func TestSiteReadServiceListValidatesViewAndNormalizesPaging(t *testing.T) {
	t.Parallel()
	store := &siteReadFake{total: 2}
	service := NewSiteReadService(store)
	_, err := service.List(context.Background(), "review", " go, tools ,,", "", 0, 200)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if store.listFilter.View != "review" || store.listFilter.Limit != 100 || store.listFilter.Offset != 0 || len(store.listFilter.Tags) != 2 {
		t.Fatalf("filter = %#v", store.listFilter)
	}
	_, err = service.List(context.Background(), "unknown", "", "", 1, 10)
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusUnprocessableEntity || statusErr.HTTPErrorCode() != httperr.CodeInvalidSiteView {
		t.Fatalf("invalid view error = %v", err)
	}
}

func TestSiteReadServiceRecentUsesExactRollingWindowAndFrozenCutoff(t *testing.T) {
	t.Parallel()
	requestNow := time.Date(2026, 8, 10, 18, 15, 30, 123456789, time.FixedZone("CST", 8*60*60))
	store := &siteReadFake{}
	service := NewSiteReadService(store)
	service.now = func() time.Time { return requestNow }

	first, err := service.List(context.Background(), "recent", "", "", 1, 2)
	if err != nil {
		t.Fatalf("first recent List() error = %v", err)
	}
	wantCutoff := requestNow.UTC().Add(-720 * time.Hour)
	if first.RecentCutoff == nil || !first.RecentCutoff.Equal(wantCutoff) || store.listFilter.RecentCutoff == nil || !store.listFilter.RecentCutoff.Equal(wantCutoff) {
		t.Fatalf("first cutoff response=%v filter=%#v want=%s", first.RecentCutoff, store.listFilter, wantCutoff)
	}

	service.now = func() time.Time { return requestNow.Add(48 * time.Hour) }
	second, err := service.List(context.Background(), "recent", "", first.RecentCutoff.Format(time.RFC3339Nano), 2, 2)
	if err != nil {
		t.Fatalf("second recent List() error = %v", err)
	}
	if second.RecentCutoff == nil || !second.RecentCutoff.Equal(wantCutoff) || len(store.listFilters) != 2 || store.listFilters[1].Offset != 2 || !store.listFilters[1].RecentCutoff.Equal(wantCutoff) {
		t.Fatalf("frozen page cutoff response=%v filters=%#v", second.RecentCutoff, store.listFilters)
	}
}

func TestSiteReadServiceRecentCutoffIsTimezoneInvariantAndAllStaysUnfiltered(t *testing.T) {
	t.Parallel()
	store := &siteReadFake{}
	service := NewSiteReadService(store)

	for _, cutoff := range []string{"2026-07-11T10:15:30+08:00", "2026-07-11T02:15:30Z"} {
		if _, err := service.List(context.Background(), "recent", "", cutoff, 2, 30); err != nil {
			t.Fatalf("List(%q) error = %v", cutoff, err)
		}
	}
	if len(store.listFilters) != 2 || !store.listFilters[0].RecentCutoff.Equal(*store.listFilters[1].RecentCutoff) {
		t.Fatalf("timezone cutoffs differ: %#v", store.listFilters)
	}
	if _, err := service.List(context.Background(), "all", "", "", 1, 30); err != nil {
		t.Fatalf("all List() error = %v", err)
	}
	if store.listFilter.RecentCutoff != nil {
		t.Fatalf("all view unexpectedly filtered by cutoff: %#v", store.listFilter)
	}
	if _, err := service.List(context.Background(), "all", "", "2026-07-11T02:15:30Z", 1, 30); err == nil {
		t.Fatal("all view accepted a recent cutoff")
	}
	if _, err := service.List(context.Background(), "recent", "", "2026-07-11T02:15:30Z", 1, 30); err == nil {
		t.Fatal("first recent page accepted a client cutoff")
	}
	if _, err := service.List(context.Background(), "recent", "", "", 2, 30); err == nil {
		t.Fatal("later recent page accepted a missing cutoff")
	}
	if _, err := service.List(context.Background(), "recent", "", "not-an-instant", 2, 30); err == nil {
		t.Fatal("recent view accepted an invalid cutoff")
	}
}

func TestSiteReadServiceGetMapsInvalidAndMissingIDs(t *testing.T) {
	t.Parallel()
	service := NewSiteReadService(&siteReadFake{})
	_, err := service.Get(context.Background(), "not-a-uuid")
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusBadRequest || statusErr.HTTPErrorCode() != httperr.CodeInvalidSiteID {
		t.Fatalf("invalid id error = %v", err)
	}
	_, err = service.Get(context.Background(), uuid.New().String())
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusNotFound || statusErr.HTTPErrorCode() != httperr.CodeSiteNotFound {
		t.Fatalf("missing error = %v", err)
	}
}

func TestSiteReadServiceGetAddsIndependentRelatedReadings(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0).UTC()
	siteID := uuid.New()
	homepage := "https://Example.com/"
	store := &siteReadFake{
		detail: &repository.SiteDetail{
			SiteListItem: repository.SiteListItem{Site: model.Site{ID: siteID, HomepageURL: &homepage, FirstCollectedAt: now, LastCollectedAt: now}},
			Tags:         []model.SiteTag{{Tag: "Go", NormalizedTag: "go"}, {Tag: "Tools", NormalizedTag: "tools"}},
			Entries:      []model.SiteEntry{{NormalizedURL: "https://docs.example.com/guide"}},
		},
		related: []repository.RelatedReading{{ID: uuid.New(), Title: "Guide", URL: "https://example.com/guide", CreatedAt: now}},
	}
	got, err := NewSiteReadService(store).Get(context.Background(), siteID.String())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if len(got.RelatedReadings) != 1 || got.RelatedReadings[0].Title != "Guide" {
		t.Fatalf("related_readings = %#v", got.RelatedReadings)
	}
	if store.relatedLimit != 6 || len(store.relatedHosts) != 2 || store.relatedHosts[0] != "docs.example.com" || store.relatedHosts[1] != "example.com" {
		t.Fatalf("related hosts = %#v, limit=%d", store.relatedHosts, store.relatedLimit)
	}
	if len(store.relatedTags) != 2 || store.relatedTags[0] != "go" || store.relatedTags[1] != "tools" {
		t.Fatalf("related tags = %#v", store.relatedTags)
	}
}

func TestSiteManagementServiceUpdateUsesRevisionAndUserFields(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	now := time.Unix(100, 0).UTC()
	reader := &siteReadFake{detail: &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: id, Revision: 8, FirstCollectedAt: now, LastCollectedAt: now}}}}
	writer := &siteProfileWriterFake{updated: true}
	name, note, homepage := "  Example  ", "  useful  ", " https://example.com/ "
	got, err := NewSiteManagementService(reader, writer).Update(context.Background(), id.String(), `"7"`, dto.SiteUpdateRequest{Name: &name, UserNote: &note, HomepageURL: &homepage})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if writer.params.ID != id || writer.params.Revision != 7 || writer.params.Name == nil || *writer.params.Name != "Example" || writer.params.UserNote == nil || *writer.params.UserNote != "useful" {
		t.Fatalf("writer params = %#v", writer.params)
	}
	if writer.params.HomepageURL == nil || *writer.params.HomepageURL != "https://example.com/" {
		t.Fatalf("HomepageURL = %#v", writer.params.HomepageURL)
	}
	if got.ID != id.String() {
		t.Fatalf("response id = %q", got.ID)
	}
}

func TestSiteManagementServiceUpdateAppliesUserTagDeltaAtomically(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	now := time.Unix(100, 0).UTC()
	reader := &siteReadFake{detail: &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: id, Revision: 2, FirstCollectedAt: now, LastCollectedAt: now}}}}
	writer := &siteProfileWriterFake{updated: true}
	service := NewSiteManagementService(reader, writer)
	_, err := service.Update(context.Background(), id.String(), "1", dto.SiteUpdateRequest{Tags: &dto.SiteTagPatchRequest{Add: []string{" Go ", "go", "Tools"}, Remove: []string{"Legacy"}}})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if writer.tagParams.ID != id || writer.tagParams.Revision != 1 || len(writer.tagParams.TagAdds) != 2 || writer.tagParams.TagAdds[0].Tag != "Go" || writer.tagParams.TagAdds[0].NormalizedTag != "go" || len(writer.tagParams.TagRemovals) != 1 || writer.tagParams.TagRemovals[0] != "legacy" {
		t.Fatalf("tag params = %#v", writer.tagParams)
	}
	_, err = service.Update(context.Background(), id.String(), "1", dto.SiteUpdateRequest{Tags: &dto.SiteTagPatchRequest{Add: []string{"go"}, Remove: []string{"GO"}}})
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusUnprocessableEntity || statusErr.HTTPErrorCode() != httperr.CodeInvalidSiteUpdate {
		t.Fatalf("overlapping tag patch error = %v", err)
	}
}

func TestSiteManagementUnicodeLengthMatchesAPIContract(t *testing.T) {
	t.Parallel()
	for _, maximum := range []int{128, 256, 1000, 2048, 10000} {
		for _, unit := range []string{"a", "中", "😀"} {
			if value := strings.Repeat(unit, maximum); !validUnicodeLength(value, maximum) {
				t.Fatalf("validUnicodeLength(%d x %q, %d) = false", maximum, unit, maximum)
			}
			if value := strings.Repeat(unit, maximum+1); validUnicodeLength(value, maximum) {
				t.Fatalf("validUnicodeLength(%d x %q, %d) = true", maximum+1, unit, maximum)
			}
		}
	}

	for _, tc := range []struct {
		name    string
		value   string
		request func(*string) dto.SiteUpdateRequest
		wantErr bool
	}{
		{name: "name BMP at limit", value: strings.Repeat("中", 256), request: func(value *string) dto.SiteUpdateRequest { return dto.SiteUpdateRequest{Name: value} }},
		{name: "name emoji at limit", value: strings.Repeat("😀", 256), request: func(value *string) dto.SiteUpdateRequest { return dto.SiteUpdateRequest{Name: value} }},
		{name: "name over limit", value: strings.Repeat("中", 257), request: func(value *string) dto.SiteUpdateRequest { return dto.SiteUpdateRequest{Name: value} }, wantErr: true},
		{name: "intro at limit", value: strings.Repeat("界", 1000), request: func(value *string) dto.SiteUpdateRequest { return dto.SiteUpdateRequest{Intro: value} }},
		{name: "user note at limit", value: strings.Repeat("🙂", 10000), request: func(value *string) dto.SiteUpdateRequest { return dto.SiteUpdateRequest{UserNote: value} }},
		{name: "invalid UTF-8", value: string([]byte{0xff}), request: func(value *string) dto.SiteUpdateRequest { return dto.SiteUpdateRequest{Name: value} }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value
			_, err := normalizeSiteUpdate(tc.request(&value))
			if (err != nil) != tc.wantErr {
				t.Fatalf("normalizeSiteUpdate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}

	entryName := strings.Repeat("文", 256)
	if _, err := normalizeSiteEntryUpdate(dto.SiteEntryUpdateRequest{Name: &entryName}); err != nil {
		t.Fatalf("256-code-point entry name rejected: %v", err)
	}
	entryName += "文"
	if _, err := normalizeSiteEntryUpdate(dto.SiteEntryUpdateRequest{Name: &entryName}); err == nil {
		t.Fatal("257-code-point entry name accepted")
	}

	tag := strings.Repeat("标", 128)
	if _, _, err := normalizeSiteTagPatch(&dto.SiteTagPatchRequest{Add: []string{tag}}); err != nil {
		t.Fatalf("128-code-point tag rejected: %v", err)
	}
	tag += "签"
	if _, _, err := normalizeSiteTagPatch(&dto.SiteTagPatchRequest{Add: []string{tag}}); err == nil {
		t.Fatal("129-code-point tag accepted")
	}
}

func TestSiteManagementServiceUpdateRejectsBadRevisionAndConflict(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	pinned := true
	service := NewSiteManagementService(&siteReadFake{}, &siteProfileWriterFake{})
	_, err := service.Update(context.Background(), id.String(), "", dto.SiteUpdateRequest{Pinned: &pinned})
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusPreconditionRequired || statusErr.HTTPErrorCode() != httperr.CodeSiteRevisionRequired {
		t.Fatalf("revision error = %v", err)
	}
	service = NewSiteManagementService(&siteReadFake{}, &siteProfileWriterFake{updated: false})
	_, err = service.Update(context.Background(), id.String(), "3", dto.SiteUpdateRequest{Pinned: &pinned})
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusConflict || statusErr.HTTPErrorCode() != httperr.CodeSiteRevisionConflict {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestSiteManagementServiceEntryMutationsUseRootRevision(t *testing.T) {
	t.Parallel()
	siteID, entryID := uuid.New(), uuid.New()
	now := time.Unix(100, 0).UTC()
	reader := &siteReadFake{detail: &repository.SiteDetail{SiteListItem: repository.SiteListItem{Site: model.Site{ID: siteID, Revision: 8, FirstCollectedAt: now, LastCollectedAt: now}}}}
	writer := &siteProfileWriterFake{updated: true, deletedEntry: repository.SiteEntryDeleteResult{DeletedSite: true}}
	service := NewSiteManagementService(reader, writer)
	name, purpose := "  Docs ", "  Read API docs  "
	if _, err := service.UpdateEntry(context.Background(), siteID.String(), entryID.String(), `"7"`, dto.SiteEntryUpdateRequest{Name: &name, Purpose: &purpose}); err != nil {
		t.Fatalf("UpdateEntry() error = %v", err)
	}
	if writer.entryParams.SiteID != siteID || writer.entryParams.EntryID != entryID || writer.entryParams.Revision != 7 || writer.entryParams.Name == nil || *writer.entryParams.Name != "Docs" || writer.entryParams.Purpose == nil || *writer.entryParams.Purpose != "Read API docs" {
		t.Fatalf("entry params = %#v", writer.entryParams)
	}
	if _, err := service.SetPrimaryEntry(context.Background(), siteID.String(), entryID.String(), "8"); err != nil {
		t.Fatalf("SetPrimaryEntry() error = %v", err)
	}
	if writer.primaryParams.SiteID != siteID || writer.primaryParams.EntryID != entryID || writer.primaryParams.Revision != 8 {
		t.Fatalf("primary params = %#v", writer.primaryParams)
	}
	deleted, err := service.DeleteEntry(context.Background(), siteID.String(), entryID.String(), "9")
	if err != nil || !deleted.DeletedSite || writer.deleteEntryParams.Revision != 9 {
		t.Fatalf("DeleteEntry() = %#v, %v; params=%#v", deleted, err, writer.deleteEntryParams)
	}
	if err := service.Delete(context.Background(), siteID.String(), "10", "2"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if writer.deleteParams.ID != siteID || writer.deleteParams.Revision != 10 || writer.deleteParams.ConfirmEntryCount != 2 {
		t.Fatalf("delete params = %#v", writer.deleteParams)
	}
}

func TestSiteManagementServiceEntryErrorsHaveStableContracts(t *testing.T) {
	t.Parallel()
	siteID, entryID := uuid.New(), uuid.New()
	service := NewSiteManagementService(&siteReadFake{}, &siteProfileWriterFake{err: repository.ErrSiteEntryNotFound})
	_, err := service.SetPrimaryEntry(context.Background(), siteID.String(), entryID.String(), "1")
	var statusErr *httperr.Error
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusNotFound || statusErr.HTTPErrorCode() != httperr.CodeSiteEntryNotFound {
		t.Fatalf("entry not found error = %v", err)
	}
	service = NewSiteManagementService(&siteReadFake{}, &siteProfileWriterFake{err: repository.ErrRevisionConflict})
	_, err = service.UpdateEntry(context.Background(), siteID.String(), entryID.String(), "1", dto.SiteEntryUpdateRequest{})
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusUnprocessableEntity || statusErr.HTTPErrorCode() != httperr.CodeSiteEntryUpdateEmpty {
		t.Fatalf("empty entry update error = %v", err)
	}
	err = service.Delete(context.Background(), siteID.String(), "1", "0")
	if !errors.As(err, &statusErr) || statusErr.HTTPStatus() != http.StatusUnprocessableEntity || statusErr.HTTPErrorCode() != httperr.CodeSiteDeleteConfirm {
		t.Fatalf("delete confirmation error = %v", err)
	}
}
