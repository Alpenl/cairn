package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"

	"webtag/internal/model"
)

func TestUpdateLibraryClassificationWritesFields(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	id := uuid.New()
	predicted := model.LibraryKindReading
	confidence := float32(.91)
	reason := "jsonld_article"
	explanation := "long-form article"
	version := "library-v1"
	mock.ExpectExec(regexp.QuoteMeta(updateLibraryClassificationSQL)).WithArgs(id, model.LibraryKindReading, model.LibraryKindSourceAuto, false, &predicted, &confidence, &reason, &explanation, &version).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := repo.UpdateLibraryClassification(context.Background(), UpdateLibraryClassificationParams{ID: id, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceAuto, PredictedKind: &predicted, Confidence: &confidence, Reason: &reason, Explanation: &explanation, ClassifierVersion: &version}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLibraryClassificationReturnsNotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()
	repo := NewPGXLinkRepository(mock)
	id := uuid.New()
	var (
		nilKind  *model.LibraryKind
		nilFloat *float32
		nilText  *string
	)
	mock.ExpectExec(regexp.QuoteMeta(updateLibraryClassificationSQL)).WithArgs(id, model.LibraryKindSite, model.LibraryKindSourceUser, true, nilKind, nilFloat, nilText, nilText, nilText).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	// 零行后会探测锁状态以区分「行不存在」与「被锁定拒绝」。这里模拟行确实不存在。
	mock.ExpectQuery(regexp.QuoteMeta(selectLibraryKindLockedSQL)).WithArgs(id).WillReturnError(pgx.ErrNoRows)
	err = repo.UpdateLibraryClassification(context.Background(), UpdateLibraryClassificationParams{ID: id, Kind: model.LibraryKindSite, Source: model.LibraryKindSourceUser, Locked: true})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

// TestUpdateLibraryClassificationDistinguishesLockFromMissing 锁住零行后的三种
// 归因：行不存在 → ErrNotFound；被锁定拒绝 → ErrLibraryKindLocked；探测本身
// 失败 → 原样上抛，不得伪装成前两者。
//
// 最后一条是重点：整段逻辑的立意就是「别把排查引向数据丢了」，若把连接断开、
// 超时、权限失败也吞成 ErrNotFound，正好犯同一个错。
func TestUpdateLibraryClassificationDistinguishesLockFromMissing(t *testing.T) {
	var (
		nilKind  *model.LibraryKind
		nilFloat *float32
		nilText  *string
	)
	params := func(id uuid.UUID) UpdateLibraryClassificationParams {
		return UpdateLibraryClassificationParams{ID: id, Kind: model.LibraryKindSite, Source: model.LibraryKindSourceAuto}
	}

	t.Run("被锁定拒绝", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer mock.Close()
		repo := NewPGXLinkRepository(mock)
		id := uuid.New()
		mock.ExpectExec(regexp.QuoteMeta(updateLibraryClassificationSQL)).
			WithArgs(id, model.LibraryKindSite, model.LibraryKindSourceAuto, false, nilKind, nilFloat, nilText, nilText, nilText).
			WillReturnResult(pgxmock.NewResult("UPDATE", 0))
		mock.ExpectQuery(regexp.QuoteMeta(selectLibraryKindLockedSQL)).WithArgs(id).
			WillReturnRows(pgxmock.NewRows([]string{"library_kind_locked"}).AddRow(true))

		err = repo.UpdateLibraryClassification(context.Background(), params(id))
		if !errors.Is(err, ErrLibraryKindLocked) {
			t.Fatalf("error = %v, want ErrLibraryKindLocked", err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatal("被锁定不应报 ErrNotFound——那会把排查引向「数据丢了」")
		}
	})

	t.Run("探测失败不得伪装成 NotFound", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer mock.Close()
		repo := NewPGXLinkRepository(mock)
		id := uuid.New()
		mock.ExpectExec(regexp.QuoteMeta(updateLibraryClassificationSQL)).
			WithArgs(id, model.LibraryKindSite, model.LibraryKindSourceAuto, false, nilKind, nilFloat, nilText, nilText, nilText).
			WillReturnResult(pgxmock.NewResult("UPDATE", 0))
		mock.ExpectQuery(regexp.QuoteMeta(selectLibraryKindLockedSQL)).WithArgs(id).
			WillReturnError(errors.New("connection reset by peer"))

		err = repo.UpdateLibraryClassification(context.Background(), params(id))
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrLibraryKindLocked) {
			t.Fatalf("DB 故障被伪装成 %v——真实故障必须原样上抛", err)
		}
		if err == nil {
			t.Fatal("探测失败却返回 nil")
		}
	})
}

func TestRequestedLibraryClassification(t *testing.T) {
	tests := []struct {
		requested       model.RequestedLibraryKind
		requestedSource model.RequestedLibraryKindSource
		kind            model.LibraryKind
		source          model.LibraryKindSource
		locked          bool
	}{
		{requested: model.RequestedLibraryKindAuto},
		{requested: model.RequestedLibraryKindReading, requestedSource: model.RequestedLibraryKindSourceUser, kind: model.LibraryKindReading, source: model.LibraryKindSourceUser, locked: true},
		{requested: model.RequestedLibraryKindSite, requestedSource: model.RequestedLibraryKindSourceUser, kind: model.LibraryKindSite, source: model.LibraryKindSourceUser, locked: true},
		{requested: model.RequestedLibraryKindReading, requestedSource: model.RequestedLibraryKindSourceAuto, kind: model.LibraryKindReading, source: model.LibraryKindSourceAuto},
	}
	for _, tt := range tests {
		kind, source, locked := requestedLibraryClassification(tt.requested, tt.requestedSource)
		if tt.kind == "" {
			if kind != nil || source != nil || locked {
				t.Fatalf("auto classification = (%v, %v, %t), want unresolved", kind, source, locked)
			}
			continue
		}
		if kind == nil || *kind != tt.kind || source == nil || *source != tt.source || locked != tt.locked {
			t.Fatalf("classification = (%v, %v, %t), want (%q, %q, %t)", kind, source, locked, tt.kind, tt.source, tt.locked)
		}
	}
}
