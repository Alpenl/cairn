package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestContentRepositoryWritesStructuredSnapshotAtomically(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	id := uuid.New()
	revision := time.Now().UTC()
	document := "# Heading\n\nBody"
	// 两项阅读计数与正文写在同一条 UPDATE 里（PF6）。
	content := model.SavedContent{Text: "Heading\n\nBody", Document: &document, Format: model.ContentFormatMarkdown, CJKChars: 0, Words: 3}

	// 写入自增后的 content_revision 必须由同一条语句 RETURNING 回来——补一次
	// SELECT 就有窗口读到别人的代次。
	mock.ExpectQuery(regexp.QuoteMeta("SET content = $1, content_document = $2, content_format = $3")).
		WithArgs(content.Text, content.Document, content.Format, content.CJKChars, content.Words, id, revision).
		WillReturnRows(mock.NewRows([]string{"content_revision"}).AddRow(int64(8)))

	newRevision, written, err := repo.UpdateContentIfCurrent(context.Background(), id, revision, content)
	if err != nil || !written {
		t.Fatalf("UpdateContentIfCurrent() = %v, %v, %v; want 8, true, nil", newRevision, written, err)
	}
	if newRevision != 8 {
		t.Fatalf("UpdateContentIfCurrent() revision = %d, want 8 (the post-write generation, straight from RETURNING)", newRevision)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestContentRepositoryReplacesAndReadsStructuredSnapshot(t *testing.T) {
	t.Parallel()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool() error = %v", err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	id := uuid.New()
	revision := time.Now().UTC()
	document := "## Fresh"
	content := model.SavedContent{Text: "Fresh", Document: &document, Format: model.ContentFormatMarkdown}

	mock.ExpectQuery(regexp.QuoteMeta("SET content = $1, content_document = $2, content_format = $3")).
		WithArgs(content.Text, content.Document, content.Format, content.CJKChars, content.Words, id, revision).
		WillReturnRows(mock.NewRows([]string{"content_revision"}).AddRow(int64(3)))
	newRevision, replaced, err := repo.ReplaceContentIfCurrent(context.Background(), id, revision, content)
	if err != nil || !replaced || newRevision != 3 {
		t.Fatalf("ReplaceContentIfCurrent() = %v, %v, %v; want 3, true, nil", newRevision, replaced, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT content, content_document, content_format, content_source, content_revision FROM links WHERE id = $1 AND deleted_at IS NULL")).
		WithArgs(id).
		WillReturnRows(mock.NewRows([]string{"content", "content_document", "content_format", "content_source", "content_revision"}).
			AddRow(content.Text, document, string(content.Format), string(model.ContentSourceFetched), int64(3)))
	got, err := repo.GetContent(context.Background(), id)
	if err != nil || got == nil {
		t.Fatalf("GetContent() = %#v, %v", got, err)
	}
	if got.Text != content.Text || got.Document == nil || *got.Document != document || got.Format != model.ContentFormatMarkdown {
		t.Fatalf("GetContent() = %#v, want %#v", got, content)
	}
	// 读回来的正文必须带着自己的代次：Reader 的正文缓存键和划线 envelope 都
	// 靠它，读路径丢掉代次等于「保存后幂等返回」那条分支永远回 0。
	if got.Revision != 3 {
		t.Fatalf("GetContent() revision = %d, want 3", got.Revision)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
