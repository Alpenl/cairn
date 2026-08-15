package dbintegration

import (
	"fmt"
	"testing"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// TestCursorModeSkipsWindowedCount 锁定 PF8 的核心收益。
//
// 偏移分页每翻一页都要在过滤后的全集上算一次 COUNT(*) OVER()，并用 OFFSET
// 扫描并丢弃前面所有行。游标模式两者都没有——仓储用 total=0 这个哨兵如实
// 表达「本次没有计算总数」，因此它就是「窗口计数没跑」的可观测证据。
func TestCursorModeSkipsWindowedCount(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)

	const total = 12
	for i := 0; i < total; i++ {
		mustCreateDoneLink(t, links, ctx,
			fmt.Sprintf("https://cursor.example.com/%d", i), "cursor", "cursor.example.com")
	}

	offsetItems, offsetTotal, err := links.ListDone(ctx, repository.ListLinksFilter{
		Statuses: []string{string(model.LinkStatusDone)}, Limit: 5, Offset: 0,
	})
	if err != nil {
		t.Fatalf("偏移模式 ListDone: %v", err)
	}
	if len(offsetItems) != 5 {
		t.Fatalf("偏移模式返回 %d 条, want 5", len(offsetItems))
	}
	if offsetTotal != total {
		t.Fatalf("偏移模式 total = %d, want %d（窗口计数应当生效）", offsetTotal, total)
	}

	cursorItems, cursorTotal, err := links.ListDone(ctx, repository.ListLinksFilter{
		Statuses: []string{string(model.LinkStatusDone)}, Limit: 5, Cursor: true,
	})
	if err != nil {
		t.Fatalf("游标模式 ListDone: %v", err)
	}
	if len(cursorItems) != 5 {
		t.Fatalf("游标模式返回 %d 条, want 5", len(cursorItems))
	}
	if cursorTotal != 0 {
		t.Fatalf("游标模式 total = %d, want 0 —— 非零说明仍在算 COUNT(*) OVER()", cursorTotal)
	}
}

// TestCursorPaginationCoversEveryRowExactlyOnce 锁定游标翻页不重不漏。
//
// created_at 相同的行会让「只按时间戳续读」的朴素实现卡住或跳行，因此后端在
// ORDER BY 里加了 id 作为 tiebreaker（link_repo_list.go）。这里用一批时间戳
// 高度重复的数据把那个 tiebreaker 压出来：全部链接在同一秒内创建。
func TestCursorPaginationCoversEveryRowExactlyOnce(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)

	const total = 25
	for i := 0; i < total; i++ {
		mustCreateDoneLink(t, links, ctx,
			fmt.Sprintf("https://tie.example.com/%d", i), "tie", "tie.example.com")
	}

	seen := map[string]int{}
	var after *repository.ListLinksCursor
	for pageIndex := 0; pageIndex < 20; pageIndex++ {
		filter := repository.ListLinksFilter{
			Statuses: []string{string(model.LinkStatusDone)}, Limit: 7, Cursor: true, After: after,
		}
		items, _, err := links.ListDone(ctx, filter)
		if err != nil {
			t.Fatalf("第 %d 页 ListDone: %v", pageIndex, err)
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			seen[item.ID.String()]++
		}
		last := items[len(items)-1]
		after = &repository.ListLinksCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		if len(items) < 7 {
			break
		}
	}

	if len(seen) != total {
		t.Fatalf("游标翻页覆盖 %d 条, want %d（有遗漏）", len(seen), total)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("链接 %s 被返回了 %d 次（有重复）", id, count)
		}
	}
}
