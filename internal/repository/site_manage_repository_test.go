package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestSiteEntryMutationsTakeSharedGateBeforeBusinessRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call func(context.Context, *PGXSiteRepository) error
	}{
		{
			name: "update entry",
			call: func(ctx context.Context, repo *PGXSiteRepository) error {
				_, err := repo.UpdateSiteEntry(ctx, UpdateSiteEntryParams{SiteID: uuid.New(), EntryID: uuid.New(), Revision: 1})
				return err
			},
		},
		{
			name: "set primary entry",
			call: func(ctx context.Context, repo *PGXSiteRepository) error {
				_, err := repo.SetSitePrimaryEntry(ctx, SetSitePrimaryEntryParams{SiteID: uuid.New(), EntryID: uuid.New(), Revision: 1})
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool() error = %v", err)
			}
			defer mock.Close()
			gateErr := errors.New("shared representation gate rejected")
			mock.ExpectBegin()
			mock.ExpectExec(regexp.QuoteMeta(lockRepresentationWriteGateSharedSQL)).WillReturnError(gateErr)
			mock.ExpectRollback()

			if err := tc.call(t.Context(), NewPGXSiteRepository(mock)); !errors.Is(err, gateErr) {
				t.Fatalf("site mutation error = %v, want gate error", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

func TestDeleteSiteEntryReplacesPrimaryBeforeDeletingItsLink(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	siteID, entryID, fallbackID, linkID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(4), entryID.String()))
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteEntrySQL)).WithArgs(siteID, entryID).
		WillReturnRows(mock.NewRows([]string{"link_id"}).AddRow(linkID))
	mock.ExpectQuery(regexp.QuoteMeta(countSiteEntriesSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(findFallbackPrimaryEntrySQL)).WithArgs(siteID, entryID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(fallbackID))
	mock.ExpectExec(regexp.QuoteMeta(clearManagedPrimaryEntrySQL)).WithArgs(fallbackID, siteID, int64(4)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectCommit()

	result, err := repo.DeleteSiteEntry(context.Background(), DeleteSiteEntryParams{SiteID: siteID, EntryID: entryID, Revision: 4})
	if err != nil || result.DeletedSite {
		t.Fatalf("DeleteSiteEntry() = %#v, %v", result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestDeleteFinalSiteEntryDeletesLinkAndEmptySite(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	siteID, entryID, linkID := uuid.New(), uuid.New(), uuid.New()
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(7), entryID.String()))
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteEntrySQL)).WithArgs(siteID, entryID).
		WillReturnRows(mock.NewRows([]string{"link_id"}).AddRow(linkID))
	mock.ExpectQuery(regexp.QuoteMeta(countSiteEntriesSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(clearManagedPrimaryEntrySQL)).WithArgs(nil, siteID, int64(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedSiteSQL)).WithArgs(siteID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectCommit()

	result, err := repo.DeleteSiteEntry(context.Background(), DeleteSiteEntryParams{SiteID: siteID, EntryID: entryID, Revision: 7})
	if err != nil || !result.DeletedSite {
		t.Fatalf("DeleteSiteEntry() = %#v, %v", result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestDeleteSiteRequiresConfirmedCurrentEntryCount(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	siteID := uuid.New()
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(3), nil))
	mock.ExpectQuery(regexp.QuoteMeta(countSiteEntriesSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectRollback()

	_, err = repo.DeleteSite(context.Background(), DeleteSiteParams{ID: siteID, Revision: 3, ConfirmEntryCount: 1})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("DeleteSite() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestDeleteSiteDeletesAllEntryLinksBeforeAggregate(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	siteID := uuid.New()
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(3), nil))
	mock.ExpectQuery(regexp.QuoteMeta(countSiteEntriesSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedSiteLinksSQL)).WithArgs(siteID).
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedSiteSQL)).WithArgs(siteID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectCommit()

	deleted, err := repo.DeleteSite(context.Background(), DeleteSiteParams{ID: siteID, Revision: 3, ConfirmEntryCount: 2})
	if err != nil || !deleted {
		t.Fatalf("DeleteSite() = %v, %v", deleted, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdateSiteProfileAndTagsUsesOneRevisionGuardedTransaction(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	siteID := uuid.New()
	name := "Example"
	params := UpdateSiteProfileParams{
		ID:          siteID,
		Revision:    5,
		Name:        &name,
		TagAdds:     []SiteTagMutation{{Tag: "Go", NormalizedTag: "go"}, {Tag: "Tools", NormalizedTag: "tools"}},
		TagRemovals: []string{"legacy"},
	}
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectExec(regexp.QuoteMeta(updateSiteProfileSQL)).WithArgs(params.Name, params.Intro, params.HomepageURL, params.IconURL, params.UserNote, params.Pinned, siteID, int64(5)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteUserSiteTagsSQL)).WithArgs(siteID, params.TagRemovals).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	for _, tag := range params.TagAdds {
		mock.ExpectExec(regexp.QuoteMeta(upsertUserSiteTagSQL)).WithArgs(siteID, tag.Tag, tag.NormalizedTag).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	mock.ExpectCommit()

	updated, err := repo.UpdateSiteProfileAndTags(context.Background(), params)
	if err != nil || !updated {
		t.Fatalf("UpdateSiteProfileAndTags() = %v, %v", updated, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestExecuteSiteMergeMovesUniqueEntriesAndDeletesDuplicatesAtomically(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	targetID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	sourceID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	duplicateEntry, duplicateLink := uuid.New(), uuid.New()
	movedEntry, movedLink := uuid.New(), uuid.New()

	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(targetID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(4), nil))
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(sourceID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(7), nil))
	mock.ExpectQuery(regexp.QuoteMeta(mergeSourceEntriesSQL)).WithArgs(sourceID).
		WillReturnRows(mock.NewRows([]string{"id", "link_id", "normalized_url"}).
			AddRow(duplicateEntry, duplicateLink, "https://example.com/").
			AddRow(movedEntry, movedLink, "https://example.com/docs"))
	mock.ExpectQuery(regexp.QuoteMeta(mergeTargetURLExistsSQL)).WithArgs(targetID, "https://example.com/").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedLinkSQL)).WithArgs(duplicateLink).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(mergeTargetURLExistsSQL)).WithArgs(targetID, "https://example.com/docs").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectExec(regexp.QuoteMeta(mergeMoveEntrySQL)).WithArgs(targetID, movedEntry).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(mergeUserTagsSQL)).WithArgs(targetID, sourceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 2))
	mock.ExpectExec(regexp.QuoteMeta(mergeIdentitiesSQL)).WithArgs(targetID, sourceID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(mergeTargetSQL)).WithArgs((*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil), targetID, int64(4)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedSiteSQL)).WithArgs(sourceID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectCommit()

	result, err := repo.ExecuteSiteMerge(context.Background(), ExecuteSiteMergeParams{TargetID: targetID, TargetRevision: 4, Sources: []SiteMergeSource{{ID: sourceID, Revision: 7}}})
	if err != nil {
		t.Fatalf("ExecuteSiteMerge() error = %v", err)
	}
	if result.SiteID != targetID || result.Revision != 5 || result.MovedEntries != 1 || result.DeletedLinks != 1 {
		t.Fatalf("result = %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestExecuteSiteMergeRollsBackBeforeAnyEntryMutationOnRevisionMismatch(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	targetID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	sourceID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	mock.ExpectBegin()
	expectLibraryFeedRevisionPrelock(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(targetID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(4), nil))
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(sourceID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(8), nil))
	mock.ExpectRollback()

	_, err = repo.ExecuteSiteMerge(context.Background(), ExecuteSiteMergeParams{TargetID: targetID, TargetRevision: 4, Sources: []SiteMergeSource{{ID: sourceID, Revision: 7}}})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("ExecuteSiteMerge() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestExecuteSiteSplitMovesEntriesAndKeepsAllWritesInOneTransaction(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXSiteRepository(mock)
	sourceID, entryID, newID := uuid.New(), uuid.New(), uuid.New()
	identity := "v1:host:example.com"
	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(sourceID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(int64(4), nil))
	mock.ExpectQuery(regexp.QuoteMeta(splitCountEntriesSQL)).WithArgs(sourceID).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(splitLockSelectedEntriesSQL)).WithArgs(sourceID, []uuid.UUID{entryID}).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(entryID))
	mock.ExpectQuery(regexp.QuoteMeta(splitInsertSiteSQL)).WithArgs(pgxmock.AnyArg(), "Separated", (*string)(nil), (*string)(nil), (*string)(nil), (*string)(nil)).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(newID))
	mock.ExpectExec(regexp.QuoteMeta(splitMoveEntriesSQL)).WithArgs(newID, sourceID, []uuid.UUID{entryID}).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(splitSetPrimarySQL)).WithArgs(entryID, newID).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(splitUpdateSourceSQL)).WithArgs([]uuid.UUID{entryID}, sourceID, int64(4)).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(splitCopyUserTagsSQL)).WithArgs(newID, sourceID).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(regexp.QuoteMeta(splitMoveIdentitySQL)).WithArgs(newID, sourceID, identity).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()
	result, err := repo.ExecuteSiteSplit(context.Background(), ExecuteSiteSplitParams{SourceID: sourceID, SourceRevision: 4, EntryIDs: []uuid.UUID{entryID}, Name: "Separated", PrimaryEntryID: entryID, IdentityKeyForNewSite: &identity})
	if err != nil {
		t.Fatalf("ExecuteSiteSplit() error = %v", err)
	}
	if result.NewSiteID != newID || result.SourceRevision != 5 || result.MovedEntries != 1 {
		t.Fatalf("result=%#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
