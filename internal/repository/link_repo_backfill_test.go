package repository

import (
	"context"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"
)

func TestListLinksNeedingEmbeddingUsesInstallationScope(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	afterID := uuid.New()
	id := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, title, summary, input_text, metadata_revision
		 FROM links
		 WHERE deleted_at IS NULL
		   AND id > $1
		   AND status = 'done'
		   AND (embedding IS NULL OR embedding_model IS DISTINCT FROM $2)
		 ORDER BY id
		 LIMIT $3`)).
		WithArgs(afterID, "embed-model", 2).
		WillReturnRows(mock.NewRows([]string{"id", "title", "summary", "input_text", "metadata_revision"}).
			AddRow(id, "title", "summary", "body", int64(7)))

	items, err := NewPGXLinkRepository(mock).ListLinksNeedingEmbedding(context.Background(), "embed-model", afterID, 2)
	if err != nil {
		t.Fatalf("ListLinksNeedingEmbedding() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != id || items[0].MetadataRevision != 7 {
		t.Fatalf("items = %#v, want one candidate %s", items, id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLinkEmbeddingUsesInstallationScope(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	id := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE links
		 SET embedding = $2, embedding_model = $3
		 WHERE id = $1 AND deleted_at IS NULL
		   AND metadata_revision = $4`)).
		WithArgs(id, pgxmock.AnyArg(), "embed-model", int64(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	applied, err := NewPGXLinkRepository(mock).UpdateLinkEmbedding(context.Background(), id, 7, []float32{1, 0}, "embed-model")
	if err != nil {
		t.Fatalf("UpdateLinkEmbedding() error = %v", err)
	}
	if !applied {
		t.Fatal("UpdateLinkEmbedding() applied = false, want true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLinkEmbeddingReportsStaleCASMiss(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	id := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE links
		 SET embedding = $2, embedding_model = $3
		 WHERE id = $1 AND deleted_at IS NULL
		   AND metadata_revision = $4`)).
		WithArgs(id, pgxmock.AnyArg(), "embed-model", int64(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	applied, err := NewPGXLinkRepository(mock).UpdateLinkEmbedding(context.Background(), id, 7, []float32{1, 0}, "embed-model")
	if err != nil {
		t.Fatalf("UpdateLinkEmbedding() error = %v", err)
	}
	if applied {
		t.Fatal("UpdateLinkEmbedding() applied = true, want false after stale metadata CAS miss")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
