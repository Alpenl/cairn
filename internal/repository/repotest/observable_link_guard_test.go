package repotest_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

// TestTerminalCompleterFakesRejectWhatProductionRejects 锁住一条 fake 设计原则：
// fake 可以比生产省事，不可以比生产宽松。
//
// 两个终态 completer 未设 hook 时默认成功，这是刻意的便利。但「默认成功」若变成
// 「无条件成功」，生产会拒绝的参数在测试里就静默通过——一次归属为空的回归正是
// 这样对整套测试隐形的。本测试确保守卫不会在后续改动中被悄悄拿掉。
//
// 期望的错误条件逐条对应生产会拒绝的参数：
//   - CompleteReadingParse → parse_state_repository.go 的 Kind/ID 校验，
//     以及同事务内 updateLibraryClassificationOn 的 Source 白名单
//   - CompleteSiteParse    → site_repository.go 的 Kind+Source / ID 校验、
//     aggregateSiteOn 的 name/entry_name 非空，以及 DB 约束
//     chk_links_library_kind_source 的 Source 白名单（site 路径不走
//     updateLibraryClassificationOn，故 Go 层无对应守卫）
func TestTerminalCompleterFakesRejectWhatProductionRejects(t *testing.T) {
	t.Parallel()

	linkID, otherID, jobID := uuid.New(), uuid.New(), uuid.New()

	t.Run("reading: 归属非 reading 必须报错", func(t *testing.T) {
		t.Parallel()
		store := &repotest.ObservableLinkStore{}
		_, err := store.CompleteReadingParse(context.Background(), repository.CompleteReadingParseParams{
			Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID},
			Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: ""},
		}, jobID)
		if err == nil {
			t.Fatal("空归属应被拒绝——生产的 CompleteReadingParse 要求 Kind 必须是 reading")
		}
	})

	t.Run("reading: link id 不一致必须报错", func(t *testing.T) {
		t.Parallel()
		store := &repotest.ObservableLinkStore{}
		_, err := store.CompleteReadingParse(context.Background(), repository.CompleteReadingParseParams{
			Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID},
			Classification: repository.UpdateLibraryClassificationParams{ID: otherID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceAuto},
		}, jobID)
		if err == nil {
			t.Fatal("分析与分类指向不同 link 应被拒绝")
		}
	})

	t.Run("reading: 合法参数放行", func(t *testing.T) {
		t.Parallel()
		store := &repotest.ObservableLinkStore{}
		got, err := store.CompleteReadingParse(context.Background(), repository.CompleteReadingParseParams{
			Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID, ExpectedMetadataRevision: 7},
			Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceAuto},
		}, jobID)
		if err != nil {
			t.Fatalf("合法参数不应报错：%v", err)
		}
		if !got.MetadataApplied || got.MetadataRevision != 7 {
			t.Fatalf("completion result = %#v, want applied revision 7", got)
		}
	})

	t.Run("reading: 零元数据租约不授权元数据写入", func(t *testing.T) {
		t.Parallel()
		const storedRevision int64 = 12
		store := &repotest.ObservableLinkStore{
			ByID: map[uuid.UUID]*model.Link{
				linkID: {ID: linkID, MetadataRevision: storedRevision},
			},
		}
		got, err := store.CompleteReadingParse(context.Background(), repository.CompleteReadingParseParams{
			Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID},
			Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindReading, Source: model.LibraryKindSourceAuto},
		}, jobID)
		if err != nil {
			t.Fatalf("legacy completion returned error: %v", err)
		}
		if got.MetadataApplied || got.MetadataRevision != storedRevision {
			t.Fatalf("completion result = %#v, want non-applying stored revision %d", got, storedRevision)
		}
		if len(store.CompleteReadingParseCalls) != 1 {
			t.Fatalf("completion calls = %d, want lifecycle completion recorded", len(store.CompleteReadingParseCalls))
		}
	})

	t.Run("reading: Source 不在白名单必须报错", func(t *testing.T) {
		t.Parallel()
		// 生产的 CompleteReadingParse 在同一事务内调 updateLibraryClassificationOn，
		// 那里有 Source 白名单（link_repo_library.go）。该校验在被调用的下游函数里，
		// 不在 CompleteReadingParse 的函数体内，最容易漏掉。
		for _, source := range []model.LibraryKindSource{"", "garbage"} {
			store := &repotest.ObservableLinkStore{}
			_, err := store.CompleteReadingParse(context.Background(), repository.CompleteReadingParseParams{
				Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID},
				Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindReading, Source: source},
			}, jobID)
			if err == nil {
				t.Errorf("Source=%q 应被拒绝——生产的白名单只收 auto/user/migration", source)
			}
			if len(store.CompleteReadingParseCalls) != 0 {
				t.Errorf("Source=%q 被拒绝后不应留下调用记录：生产会回滚整个事务", source)
			}
		}
	})

	t.Run("site: 缺 Source 必须报错", func(t *testing.T) {
		t.Parallel()
		store := &repotest.ObservableLinkStore{}
		_, err := store.CompleteSiteParse(context.Background(), repository.CompleteSiteParseParams{
			Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID},
			Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindSite},
			Site:           repository.AggregateSiteParams{LinkID: linkID, IdentityKey: "example.com", NormalizedURL: "https://example.com/", Name: "站点", EntryName: "首页"},
		}, jobID)
		if err == nil {
			t.Fatal("Source 为空应被拒绝——生产的 CompleteSiteParse 要求它非空")
		}
	})

	t.Run("site: link id 不一致必须报错", func(t *testing.T) {
		t.Parallel()
		store := &repotest.ObservableLinkStore{}
		_, err := store.CompleteSiteParse(context.Background(), repository.CompleteSiteParseParams{
			Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID},
			Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindSite, Source: model.LibraryKindSourceAuto},
			Site:           repository.AggregateSiteParams{LinkID: otherID, IdentityKey: "example.com", NormalizedURL: "https://example.com/", Name: "站点", EntryName: "首页"},
		}, jobID)
		if err == nil {
			t.Fatal("site 与分析指向不同 link 应被拒绝")
		}
		if len(store.CompleteSiteParseCalls) != 0 {
			t.Fatal("被拒绝的调用不应留下记录：生产在守卫失败时不写任何行")
		}
	})

	t.Run("site: Source 不在白名单必须报错", func(t *testing.T) {
		t.Parallel()
		// site 路径的 Source 白名单只由 DB 的 chk_links_library_kind_source 强制，
		// Go 层没有对应守卫——fake 在此代 DB 拒绝。
		store := &repotest.ObservableLinkStore{}
		_, err := store.CompleteSiteParse(context.Background(), repository.CompleteSiteParseParams{
			Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID},
			Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindSite, Source: "garbage"},
			Site:           repository.AggregateSiteParams{LinkID: linkID, IdentityKey: "example.com", NormalizedURL: "https://example.com/", Name: "站点", EntryName: "首页"},
		}, jobID)
		if err == nil {
			t.Fatal("Source=garbage 应被拒绝——DB 约束只收 auto/user/migration")
		}
		if len(store.CompleteSiteParseCalls) != 0 {
			t.Fatal("被拒绝的调用不应留下记录")
		}
	})

	t.Run("site: name / entry_name 为空必须报错", func(t *testing.T) {
		t.Parallel()
		// aggregateSiteOn 的守卫，对应 DB 的 chk_sites_lengths /
		// chk_site_entries_lengths。
		for _, site := range []repository.AggregateSiteParams{
			{LinkID: linkID, IdentityKey: "example.com", NormalizedURL: "https://example.com/", Name: "", EntryName: "首页"},
			{LinkID: linkID, IdentityKey: "example.com", NormalizedURL: "https://example.com/", Name: "站点", EntryName: "  "},
		} {
			store := &repotest.ObservableLinkStore{}
			_, err := store.CompleteSiteParse(context.Background(), repository.CompleteSiteParseParams{
				Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID},
				Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindSite, Source: model.LibraryKindSourceAuto},
				Site:           site,
			}, jobID)
			if err == nil {
				t.Errorf("name=%q entry=%q 应被拒绝", site.Name, site.EntryName)
			}
		}
	})

	t.Run("site: 合法参数放行", func(t *testing.T) {
		t.Parallel()
		store := &repotest.ObservableLinkStore{}
		if _, err := store.CompleteSiteParse(context.Background(), repository.CompleteSiteParseParams{
			Analysis:       repository.UpdateLinkAnalysisParams{ID: linkID, ExpectedMetadataRevision: 11},
			Classification: repository.UpdateLibraryClassificationParams{ID: linkID, Kind: model.LibraryKindSite, Source: model.LibraryKindSourceAuto},
			Site:           repository.AggregateSiteParams{LinkID: linkID, IdentityKey: "example.com", NormalizedURL: "https://example.com/", Name: "站点", EntryName: "首页"},
		}, jobID); err != nil {
			t.Fatalf("合法参数不应报错：%v", err)
		}
	})
}
