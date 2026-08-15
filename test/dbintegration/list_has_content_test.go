package dbintegration

import (
	"testing"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestListReportsHasContentTruthfully 锁定 PF6 的核心：**列表说真话**。
//
// 此前 linkListColumns 不含正文列，列表因此恒报 has_content=false，而详情端
// 会如实返回 true。Reader 为此专门打了一段补丁，在合并列表刷新时把详情端的
// 真值保住——否则原文折叠头每 30 秒翻转一次，等于谎报「原文没保存」。
//
// has_content 现在是生成列（由 content IS NULL 唯一决定），因此「列与正文
// 不同步」在物理上不可能；这条测试守的是它确实进了列表投影。
func TestListReportsHasContentTruthfully(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	ctx := t.Context()

	withContent := mustCreateDoneLink(t, links, ctx, "https://hc.example.com/saved", "hc", "hc.example.com")
	withoutContent := mustCreateDoneLink(t, links, ctx, "https://hc.example.com/bare", "hc", "hc.example.com")

	// 保存原文：计数与正文写在同一条 UPDATE 里。
	saved, err := links.GetByID(ctx, withContent)
	if err != nil || saved == nil {
		t.Fatalf("GetByID: %v", err)
	}
	// 计数由 service 层的 countReadingUnits 算；仓储只负责原样写入。这里直连
	// 仓储，因此手工给出与该公式一致的值：CJK 4 字 + 5 个西文词。
	// （此前这里写的是 4/5 但文本只有 4 个词，断言了一个没有任何公式会产生的
	// 数字——它没能发现「service 层根本没算过这两个数」正是因为它绕开了那一层。）
	_, written, err := links.UpdateContentIfCurrent(ctx, withContent, saved.UpdatedAt, model.SavedContent{
		Text:     "正文内容 with some english words here",
		Format:   model.ContentFormatPlain,
		CJKChars: 4,
		Words:    5,
	})
	if err != nil || !written {
		t.Fatalf("UpdateContentIfCurrent() = %v, %v", written, err)
	}

	items, _, err := links.ListDone(ctx, repository.ListLinksFilter{
		Statuses: []string{string(model.LinkStatusDone)}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListDone: %v", err)
	}

	seen := map[string]model.Link{}
	for _, item := range items {
		seen[item.ID.String()] = item
	}

	savedRow, ok := seen[withContent.String()]
	if !ok {
		t.Fatal("已保存原文的链接不在列表里")
	}
	if !savedRow.HasContent {
		t.Fatal("列表仍在谎报 has_content=false —— PF6 的核心断言失败")
	}
	if savedRow.ContentCJKChars != 4 || savedRow.ContentWords != 5 {
		t.Fatalf("列表里的阅读计数 = %d/%d, want 4/5", savedRow.ContentCJKChars, savedRow.ContentWords)
	}

	bareRow, ok := seen[withoutContent.String()]
	if !ok {
		t.Fatal("无原文的链接不在列表里")
	}
	if bareRow.HasContent {
		t.Fatal("没有原文的链接被报成 has_content=true")
	}

	// 详情与列表必须一致 —— 这正是那段被删掉的补丁当初在掩盖的东西。
	detail, err := links.GetByID(ctx, withContent)
	if err != nil || detail == nil {
		t.Fatalf("GetByID(detail): %v", err)
	}
	if detail.HasContent != savedRow.HasContent ||
		detail.ContentCJKChars != savedRow.ContentCJKChars ||
		detail.ContentWords != savedRow.ContentWords {
		t.Fatalf("详情与列表不一致：detail=%v/%d/%d list=%v/%d/%d",
			detail.HasContent, detail.ContentCJKChars, detail.ContentWords,
			savedRow.HasContent, savedRow.ContentCJKChars, savedRow.ContentWords)
	}
}

// TestHasContentIsGeneratedAndCannotDesync 锁定 has_content 是生成列。
//
// 直接 UPDATE 它必须失败——这正是「不需要任何一致性测试来守」的那个保证：
// 列与正文不同步在物理上不可能发生。
func TestHasContentIsGeneratedAndCannotDesync(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)

	id := mustCreateDoneLink(t, links, ctx, "https://gen.example.com/a", "gen", "gen.example.com")

	_, err := pool.Exec(ctx, `UPDATE links SET has_content = true WHERE id = $1`, id)
	if err == nil {
		t.Fatal("has_content 可以被直接写入 —— 它不是生成列，列与正文可能不同步")
	}

	// 写入正文之后它自动变 true，无需任何应用层维护。
	saved, err := links.GetByID(ctx, id)
	if err != nil || saved == nil {
		t.Fatalf("GetByID: %v", err)
	}
	if _, _, err := links.UpdateContentIfCurrent(ctx, id, saved.UpdatedAt, model.SavedContent{
		Text: "body", Format: model.ContentFormatPlain, CJKChars: 0, Words: 1,
	}); err != nil {
		t.Fatalf("UpdateContentIfCurrent: %v", err)
	}
	after, err := links.GetByID(ctx, id)
	if err != nil || after == nil {
		t.Fatalf("GetByID 二次: %v", err)
	}
	if !after.HasContent {
		t.Fatal("写入正文后 has_content 仍为 false")
	}
}
