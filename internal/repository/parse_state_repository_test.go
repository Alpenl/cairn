package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestMarkParseProcessingCommitsLinkAndExactJobTogether(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, jobID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectExec(regexp.QuoteMeta(updateLinkStateSQL)).
		WithArgs(linkID, model.LinkStatusProcessing, (*string)(nil)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(updateRunnableParseJobStateSQL)).
		WithArgs(jobID, model.JobStatusProcessing, (*string)(nil), linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	if err := repo.MarkParseProcessing(context.Background(), linkID, jobID); err != nil {
		t.Fatalf("MarkParseProcessing() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestMarkParseFailedRollsBackLinkWhenExactJobUpdateFails(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, jobID := uuid.New(), uuid.New()
	message := "network: fetch failed"
	wantErr := errors.New("job update failed")

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	mock.ExpectExec(regexp.QuoteMeta(updateFailedSiteCandidateSQL)).
		WithArgs(linkID, model.LinkStatusFailed, &message).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(updateRunnableParseJobStateSQL)).
		WithArgs(jobID, model.JobStatusFailed, &message, linkID).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	err = repo.MarkParseFailed(context.Background(), linkID, jobID, message)
	if !errors.Is(err, wantErr) {
		t.Fatalf("MarkParseFailed() error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

// TestSiteCandidatePredicateGuardsEveryPayloadColumn 断言解析失败路径上，每一列
// 抓取素材都受 site 候选谓词保护——reading 链接必须留着正文供重试与手动保存。
//
// 断言分两层：谓词内容本身，以及它是否覆盖了全部六列。谓词只在一处定义
// （SQLSiteCandidatePredicate），因此这里既不用重复字面量，也不会因为实现改了
// 措辞而变成一个永远通过的空测试。
func TestSiteCandidatePredicateGuardsEveryPayloadColumn(t *testing.T) {
	t.Parallel()

	// 谓词内容——改坏它会让 reading 链接的正文被误清，属高危变更。
	const wantPredicate = "(library_kind = 'site' OR predicted_library_kind = 'site')"
	if SQLSiteCandidatePredicate != wantPredicate {
		t.Fatalf("SQLSiteCandidatePredicate = %q, want %q", SQLSiteCandidatePredicate, wantPredicate)
	}
	if SQLNotReadingPredicate != "COALESCE(library_kind, '') <> 'reading'" {
		t.Fatalf("SQLNotReadingPredicate = %q", SQLNotReadingPredicate)
	}

	// 六列的完整形状，逐列字面量。
	//
	// 不用 purgeSitePayloadColumn 生成期望值：被测 SQL 出自同一个函数，那样对
	// 形状而言是同义反复。实测两种变异都能骗过生成式断言——删掉 ELSE 子句
	// （reading 链接的这些列被一并置 NULL），或把 ELSE 分支写死成某一列
	// （其余列被整列覆写成该列的值）。两者都是静默的数据损坏，正是本不变量
	// 要防的事，因此这里逐列钉死。
	for _, tc := range []struct{ column, want string }{
		{"input_text", "input_text = CASE WHEN (library_kind = 'site' OR predicted_library_kind = 'site') THEN NULL ELSE input_text END"},
		{"input_html", "input_html = CASE WHEN (library_kind = 'site' OR predicted_library_kind = 'site') THEN NULL ELSE input_html END"},
		{"input_images", "input_images = CASE WHEN (library_kind = 'site' OR predicted_library_kind = 'site') THEN NULL ELSE input_images END"},
		{"source_metadata", "source_metadata = CASE WHEN (library_kind = 'site' OR predicted_library_kind = 'site') THEN NULL ELSE source_metadata END"},
		{"payload_purge_due_at", "payload_purge_due_at = CASE WHEN (library_kind = 'site' OR predicted_library_kind = 'site') THEN NULL ELSE payload_purge_due_at END"},
		{"payload_purged_at", "payload_purged_at = CASE WHEN (library_kind = 'site' OR predicted_library_kind = 'site') THEN NOW() ELSE payload_purged_at END"},
	} {
		if !strings.Contains(updateFailedSiteCandidateSQL, tc.want) {
			t.Errorf("updateFailedSiteCandidateSQL 未按预期保护 %s 列。\n缺少片段：%s\n实际 SQL：%s",
				tc.column, tc.want, updateFailedSiteCandidateSQL)
		}
	}
}

func TestCompleteParseRollsBackAnalysisWhenExactJobCompletionFails(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, jobID := uuid.New(), uuid.New()
	wantErr := errors.New("job completion failed")

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	expectAnyLinkAnalysisForParseUpdate(mock)
	mock.ExpectExec(regexp.QuoteMeta(completeParseJobStateSQL)).
		WithArgs(jobID, linkID, int64(1)).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	err = repo.CompleteParse(context.Background(), UpdateLinkAnalysisParams{
		ID: linkID, Status: model.LinkStatusDone, ExpectedMetadataRevision: 1,
	}, jobID)
	if !errors.Is(err, wantErr) {
		t.Fatalf("CompleteParse() error = %v, want %v", err, wantErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestCompleteParseRollsBackWhenPersistedJobMetadataRevisionDoesNotMatch(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, jobID := uuid.New(), uuid.New()
	const expectedMetadataRevision int64 = 7

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	expectAnyLinkAnalysisForParseUpdate(mock)
	// A zero-row terminal UPDATE is how PostgreSQL reports either a stale job
	// state or a stored metadata lease that differs from the caller's value.
	// The transaction must roll back the preceding link analysis in either case.
	mock.ExpectExec(regexp.QuoteMeta(completeParseJobStateSQL)).
		WithArgs(jobID, linkID, expectedMetadataRevision).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	err = repo.CompleteParse(context.Background(), UpdateLinkAnalysisParams{
		ID: linkID, Status: model.LinkStatusDone, ExpectedMetadataRevision: expectedMetadataRevision,
	}, jobID)
	if !errors.Is(err, ErrParseJobNotRunnable) {
		t.Fatalf("CompleteParse() error = %v, want ErrParseJobNotRunnable", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestCompleteReadingParseRollsBackAnalysisWhenRequestedIntentChanged(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, jobID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	expectAnyLinkAnalysisForParseUpdate(mock)
	mock.ExpectExec(regexp.QuoteMeta(updateLibraryClassificationForCompletionSQL)).
		WithArgs(
			linkID, model.LibraryKindReading, model.LibraryKindSourceAuto, false,
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			model.RequestedLibraryKindAuto, model.RequestedLibraryKindSourceAuto,
		).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectQuery(regexp.QuoteMeta(selectLibraryIntentForCompletionSQL)).
		WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"requested_library_kind", "requested_library_kind_source", "library_kind_locked"}).
			AddRow(model.RequestedLibraryKindSite, model.RequestedLibraryKindSourceUser, false))
	mock.ExpectRollback()

	_, err = repo.CompleteReadingParse(context.Background(), CompleteReadingParseParams{
		Analysis:       UpdateLinkAnalysisParams{ID: linkID, Status: model.LinkStatusDone, ExpectedMetadataRevision: 1},
		Classification: UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceAuto},
	}, jobID)
	if !errors.Is(err, ErrLibraryIntentChanged) {
		t.Fatalf("CompleteReadingParse() error = %v, want ErrLibraryIntentChanged", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func TestCompleteReadingParseDetachesExistingSiteEntryAtomically(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	linkID, jobID, siteID, entryID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	const siteRevision int64 = 7

	mock.ExpectBegin()
	expectRepresentationWriteGateShared(mock)
	expectAnyLinkAnalysisForParseUpdate(mock)
	mock.ExpectExec(regexp.QuoteMeta(updateLibraryClassificationForCompletionSQL)).
		WithArgs(
			linkID, model.LibraryKindReading, model.LibraryKindSourceUser, true,
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			model.RequestedLibraryKindReading, model.RequestedLibraryKindSourceUser,
		).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta(lockEntryByLinkSQL)).WithArgs(linkID).
		WillReturnRows(mock.NewRows([]string{"site_id", "id"}).AddRow(siteID, entryID))
	mock.ExpectQuery(regexp.QuoteMeta(lockSiteForManagementSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"revision", "primary_entry_id"}).AddRow(siteRevision, entryID.String()))
	mock.ExpectQuery(regexp.QuoteMeta(countSiteEntriesSQL)).WithArgs(siteID).
		WillReturnRows(mock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(clearManagedPrimaryEntrySQL)).WithArgs(nil, siteID, siteRevision).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteEntryByLinkSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta(deleteManagedSiteSQL)).WithArgs(siteID).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta(clearReadingPayloadPurgeDeadlineSQL)).WithArgs(linkID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(regexp.QuoteMeta(completeParseJobStateSQL)).WithArgs(jobID, linkID, int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	_, err = repo.CompleteReadingParse(context.Background(), CompleteReadingParseParams{
		Analysis:                           UpdateLinkAnalysisParams{ID: linkID, Status: model.LinkStatusDone, ExpectedMetadataRevision: 1},
		Classification:                     UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceUser, Locked: true},
		ExpectedRequestedLibraryKind:       model.RequestedLibraryKindReading,
		ExpectedRequestedLibraryKindSource: model.RequestedLibraryKindSourceUser,
		DetachSiteEntry:                    true,
	}, jobID)
	if err != nil {
		t.Fatalf("CompleteReadingParse() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet mock expectations: %v", err)
	}
}

func expectAnyLinkAnalysisForParseUpdate(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery(regexp.QuoteMeta(updateLinkAnalysisForParseSQL)).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnRows(mock.NewRows([]string{"metadata_revision", "metadata_applied"}).AddRow(int64(1), true))
}
