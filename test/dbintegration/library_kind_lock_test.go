// library_kind_lock_test.go 覆盖 links.library_kind_locked 的持久层守卫。
//
// 为什么必须在真实 PG 上测：守卫写在 updateLibraryClassificationSQL 的 WHERE
// 里（`library_kind_locked = false OR $3 <> 'auto'`）。pgxmock 只比对 SQL 字符串
// 与参数顺序，无法回答「这条 WHERE 在真实行上到底放行还是拦截」——而这正是本
// 守卫存在的全部意义。RowsAffected 的取值只有真实计划器能给出。
package dbintegration

import (
	"errors"
	"testing"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestUpdateLibraryClassification_LockedRowRejectsAutomaticRewrite 是 P0 冲突的
// 持久层回归。
//
// 背景：站点转阅读会写入 library_kind_locked=true 并把 status 置为 pending，
// 从而自己触发重解析；重解析时 analyzer 若仍判 site，旧逻辑会把归属改回 site
// 并清掉锁——用户刚做的选择在几秒内被系统撤销。
//
// 决策层（decideFinalLibrary）已让路，这里是第二道防线：任何绕过决策层的自动
// 写入也必须被拦下。两层防的是不同的东西，缺一不可。
func TestUpdateLibraryClassification_LockedRowRejectsAutomaticRewrite(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	link, _, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/locked",
		SourceKind: "url",
		SourceKey:  "src-locked",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	reading := model.LibraryKindReading
	site := model.LibraryKindSite
	confidence := float32(0.9)
	reason := "user_locked"
	version := "test-v1"

	// 用户裁决：锁定为 reading。source=user 因此不受守卫限制。
	if err := repo.UpdateLibraryClassification(ctx, repository.UpdateLibraryClassificationParams{
		ID:                link.ID,
		Kind:              model.LibraryKindReading,
		Source:            model.LibraryKindSourceUser,
		Locked:            true,
		PredictedKind:     &reading,
		Confidence:        &confidence,
		Reason:            &reason,
		ClassifierVersion: &version,
	}); err != nil {
		t.Fatalf("用户裁决写入失败: %v", err)
	}

	// 自动分类试图改回 site —— 必须被守卫拦下，且错误可与 ErrNotFound 区分。
	autoReason := "ai_site"
	err = repo.UpdateLibraryClassification(ctx, repository.UpdateLibraryClassificationParams{
		ID:                link.ID,
		Kind:              model.LibraryKindSite,
		Source:            model.LibraryKindSourceAuto,
		Locked:            false,
		PredictedKind:     &site,
		Confidence:        &confidence,
		Reason:            &autoReason,
		ClassifierVersion: &version,
	})
	if !errors.Is(err, repository.ErrLibraryKindLocked) {
		t.Fatalf("自动分类改写已锁定的链接 err = %v, want ErrLibraryKindLocked", err)
	}
	if errors.Is(err, repository.ErrNotFound) {
		t.Fatal("被锁定不应报 ErrNotFound——那会把排查引向「数据丢了」")
	}

	// 归属确实没被改动。
	got, err := repo.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LibraryKind == nil || *got.LibraryKind != model.LibraryKindReading {
		t.Fatalf("library_kind = %v, want reading——用户裁决被覆盖", got.LibraryKind)
	}
	if !got.LibraryKindLocked {
		t.Fatal("library_kind_locked 被清除")
	}

	// 用户自己改主意：source=user 应当放行，锁不对用户生效。
	if err := repo.UpdateLibraryClassification(ctx, repository.UpdateLibraryClassificationParams{
		ID:                link.ID,
		Kind:              model.LibraryKindSite,
		Source:            model.LibraryKindSourceUser,
		Locked:            true,
		PredictedKind:     &site,
		Confidence:        &confidence,
		Reason:            &reason,
		ClassifierVersion: &version,
	}); err != nil {
		t.Fatalf("用户改判被拦下了: %v——锁只应对自动分类生效", err)
	}

	got, err = repo.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID after user re-decision: %v", err)
	}
	if got.LibraryKind == nil || *got.LibraryKind != model.LibraryKindSite {
		t.Fatalf("用户改判未生效，library_kind = %v", got.LibraryKind)
	}
}

// TestUpdateLibraryClassification_UnlockedRowAcceptsAutomaticRewrite 确认守卫
// 没有误伤常规路径——未锁定的链接，自动分类照常改写。
func TestUpdateLibraryClassification_UnlockedRowAcceptsAutomaticRewrite(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	link, _, err := repo.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/unlocked",
		SourceKind: "url",
		SourceKey:  "src-unlocked",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	site := model.LibraryKindSite
	confidence := float32(0.88)
	reason := "ai_site"
	version := "test-v1"

	if err := repo.UpdateLibraryClassification(ctx, repository.UpdateLibraryClassificationParams{
		ID:                link.ID,
		Kind:              model.LibraryKindSite,
		Source:            model.LibraryKindSourceAuto,
		Locked:            false,
		PredictedKind:     &site,
		Confidence:        &confidence,
		Reason:            &reason,
		ClassifierVersion: &version,
	}); err != nil {
		t.Fatalf("未锁定链接的自动分类被拦下: %v", err)
	}

	got, err := repo.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.LibraryKind == nil || *got.LibraryKind != model.LibraryKindSite {
		t.Fatalf("library_kind = %v, want site", got.LibraryKind)
	}
}
