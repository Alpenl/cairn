package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

const readerMetadataUpdateQueryPattern = `(?s)WITH target AS \(.*FROM links.*deleted_at IS NULL.*status='done'.*library_kind='reading'.*metadata_revision=\$5.*FOR UPDATE.*\), updated AS \(.*UPDATE links AS link.*metadata_revision=link.metadata_revision\+1.*target.metadata_revision < \$6.*\).*SELECT.*EXISTS \(SELECT 1 FROM target\)`

func TestUpdateLinkMetadataRejectsStaleRevisionWithoutProjectionWrites(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	patch := readerMetadataPatchForTest()
	mock.ExpectBegin()
	expectReaderActivityFence(mock)
	mock.ExpectQuery(readerMetadataUpdateQueryPattern).
		WithArgs(patch.Title, patch.Summary, patch.Tags, patch.LinkID, patch.ExpectedRevision, model.LinkMetadataMaxRevision).
		WillReturnRows(mock.NewRows([]string{"found", "metadata_revision", "tags_changed", "changed", "tuple_changed"}).
			AddRow(false, int64(0), false, false, false))
	mock.ExpectRollback()

	_, err = NewPGXReaderVNextRepository(mock).UpdateLinkMetadata(context.Background(), patch)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("UpdateLinkMetadata() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLinkMetadataNoopAndTitleOnlyChangesSkipTagProjectionWrites(t *testing.T) {
	tests := []struct {
		name        string
		result      model.ReaderLinkMetadataUpdate
		changed     bool
		wantRev     int64
		wantTagsSet bool
	}{
		{
			name:    "identical replacement",
			result:  model.ReaderLinkMetadataUpdate{MetadataRevision: 7, TagsChanged: false},
			changed: false,
			wantRev: 7,
		},
		{
			name:    "title only replacement",
			result:  model.ReaderLinkMetadataUpdate{MetadataRevision: 8, TagsChanged: false},
			changed: true,
			wantRev: 8,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer mock.Close()

			patch := readerMetadataPatchForTest()
			mock.ExpectBegin()
			expectReaderActivityFence(mock)
			mock.ExpectQuery(readerMetadataUpdateQueryPattern).
				WithArgs(patch.Title, patch.Summary, patch.Tags, patch.LinkID, patch.ExpectedRevision, model.LinkMetadataMaxRevision).
				WillReturnRows(mock.NewRows([]string{"found", "metadata_revision", "tags_changed", "changed", "tuple_changed"}).
					AddRow(true, tc.result.MetadataRevision, tc.result.TagsChanged, tc.changed, tc.changed))
			mock.ExpectCommit()

			result, err := NewPGXReaderVNextRepository(mock).UpdateLinkMetadata(context.Background(), patch)
			if err != nil {
				t.Fatalf("UpdateLinkMetadata() error = %v", err)
			}
			if result.MetadataRevision != tc.wantRev || result.TagsChanged != tc.wantTagsSet {
				t.Fatalf("result = %#v, want revision=%d tags_changed=%v", result, tc.wantRev, tc.wantTagsSet)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpdateLinkMetadataRebuildsTagProjectionsInsideCASCommit(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	patch := readerMetadataPatchForTest()
	firstConcept := uuid.New()
	secondConcept := uuid.New()
	removedConcepts := []uuid.UUID{firstConcept, secondConcept}
	mock.ExpectBegin()
	expectReaderActivityFence(mock)
	mock.ExpectQuery(readerMetadataUpdateQueryPattern).
		WithArgs(patch.Title, patch.Summary, patch.Tags, patch.LinkID, patch.ExpectedRevision, model.LinkMetadataMaxRevision).
		WillReturnRows(mock.NewRows([]string{"found", "metadata_revision", "tags_changed", "changed", "tuple_changed"}).
			AddRow(true, int64(8), true, true, true))
	mock.ExpectQuery(regexp.QuoteMeta(`DELETE FROM link_concept WHERE link_id=$1 RETURNING concept_id`)).
		WithArgs(patch.LinkID).
		WillReturnRows(mock.NewRows([]string{"concept_id"}).
			AddRow(firstConcept).
			AddRow(secondConcept).
			AddRow(firstConcept))
	mock.ExpectExec("(?s)UPDATE concept c.*WHERE concept_id = ANY\\(\\$1\\).*WHERE c.id = winners.concept_id").
		WithArgs(removedConcepts).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	expectMetadataActivityRefresh(mock)
	mock.ExpectCommit()

	result, err := NewPGXReaderVNextRepository(mock).UpdateLinkMetadata(context.Background(), patch)
	if err != nil {
		t.Fatalf("UpdateLinkMetadata() error = %v", err)
	}
	if result.MetadataRevision != 8 || !result.TagsChanged {
		t.Fatalf("result = %#v, want changed tag tuple at revision 8", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLinkMetadataRollsBackWhenTagProjectionCleanupFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	patch := readerMetadataPatchForTest()
	projectionErr := errors.New("link_concept unavailable")
	mock.ExpectBegin()
	expectReaderActivityFence(mock)
	mock.ExpectQuery(readerMetadataUpdateQueryPattern).
		WithArgs(patch.Title, patch.Summary, patch.Tags, patch.LinkID, patch.ExpectedRevision, model.LinkMetadataMaxRevision).
		WillReturnRows(mock.NewRows([]string{"found", "metadata_revision", "tags_changed", "changed", "tuple_changed"}).
			AddRow(true, int64(8), true, true, true))
	mock.ExpectQuery(regexp.QuoteMeta(`DELETE FROM link_concept WHERE link_id=$1 RETURNING concept_id`)).
		WithArgs(patch.LinkID).
		WillReturnError(projectionErr)
	mock.ExpectRollback()

	_, err = NewPGXReaderVNextRepository(mock).UpdateLinkMetadata(context.Background(), patch)
	if !errors.Is(err, projectionErr) {
		t.Fatalf("UpdateLinkMetadata() error = %v, want projection error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLinkMetadataRejectsChangedTupleAtSafeCeilingWithoutProjectionWrites(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	patch := readerMetadataPatchForTest()
	patch.ExpectedRevision = model.LinkMetadataMaxRevision
	mock.ExpectBegin()
	expectReaderActivityFence(mock)
	mock.ExpectQuery(readerMetadataUpdateQueryPattern).
		WithArgs(patch.Title, patch.Summary, patch.Tags, patch.LinkID, patch.ExpectedRevision, model.LinkMetadataMaxRevision).
		WillReturnRows(mock.NewRows([]string{"found", "metadata_revision", "tags_changed", "changed", "tuple_changed"}).
			AddRow(true, model.LinkMetadataMaxRevision, true, false, true))
	mock.ExpectRollback()

	_, err = NewPGXReaderVNextRepository(mock).UpdateLinkMetadata(context.Background(), patch)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("UpdateLinkMetadata() error = %v, want ErrRevisionConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLinkMetadataAllowsIdenticalTupleAtSafeCeiling(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	patch := readerMetadataPatchForTest()
	patch.ExpectedRevision = model.LinkMetadataMaxRevision
	mock.ExpectBegin()
	expectReaderActivityFence(mock)
	mock.ExpectQuery(readerMetadataUpdateQueryPattern).
		WithArgs(patch.Title, patch.Summary, patch.Tags, patch.LinkID, patch.ExpectedRevision, model.LinkMetadataMaxRevision).
		WillReturnRows(mock.NewRows([]string{"found", "metadata_revision", "tags_changed", "changed", "tuple_changed"}).
			AddRow(true, model.LinkMetadataMaxRevision, false, false, false))
	mock.ExpectCommit()

	result, err := NewPGXReaderVNextRepository(mock).UpdateLinkMetadata(context.Background(), patch)
	if err != nil {
		t.Fatalf("UpdateLinkMetadata() error = %v", err)
	}
	if result.MetadataRevision != model.LinkMetadataMaxRevision || result.TagsChanged {
		t.Fatalf("result = %#v, want an unchanged safe-ceiling revision", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func readerMetadataPatchForTest() model.ReaderLinkMetadataPatch {
	title := "Replacement title"
	summary := "Replacement summary"
	return model.ReaderLinkMetadataPatch{
		LinkID:           uuid.New(),
		Title:            &title,
		Summary:          &summary,
		Tags:             []string{"replacement"},
		ExpectedRevision: 7,
	}
}

func expectMetadataActivityRefresh(mock pgxmock.PgxPoolIface) {
	mock.ExpectExec("(?s)WITH current.*GREATEST\\(l.created_at,l.first_collected_at,l.last_recollected_at\\).*status='done'.*reader_tag_activity.*ON CONFLICT.*DELETE FROM reader_tag_activity.*NOT EXISTS").
		WillReturnResult(pgxmock.NewResult("WITH", 1))
	mock.ExpectExec("(?s)WITH current.*GREATEST\\(l.created_at,l.first_collected_at,l.last_recollected_at\\).*status='done'.*reader_domain_activity.*ON CONFLICT.*DELETE FROM reader_domain_activity.*NOT EXISTS").
		WillReturnResult(pgxmock.NewResult("WITH", 1))
}
