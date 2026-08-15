package dbintegration

import (
	"fmt"
	"testing"
	"time"

	"webtag/internal/model"
	"webtag/internal/repository"
)

func TestListDonePagesEveryLinkInHalfOpenCreatedRangeExactlyOnce(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	links := repository.NewPGXLinkRepository(pool)

	from := time.Date(2026, 8, 10, 16, 0, 0, 0, time.UTC)
	before := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	const total = 101
	for index := 0; index < total; index++ {
		id := mustCreateDoneLink(t, links, ctx,
			fmt.Sprintf("https://today.example.com/in-range/%03d", index),
			"today", "today.example.com")
		createdAt := from.Add(time.Duration(index) * time.Minute)
		if _, err := pool.Exec(ctx, `UPDATE links SET created_at = $1, library_kind = 'reading', library_kind_source = 'user' WHERE id = $2`, createdAt, id); err != nil {
			t.Fatalf("set in-range created_at %d: %v", index, err)
		}
	}

	for name, createdAt := range map[string]time.Time{
		"before-lower-bound": from.Add(-time.Nanosecond),
		"at-upper-bound":     before,
	} {
		id := mustCreateDoneLink(t, links, ctx,
			"https://today.example.com/"+name, "today", "today.example.com")
		if _, err := pool.Exec(ctx, `UPDATE links SET created_at = $1, library_kind = 'reading', library_kind_source = 'user' WHERE id = $2`, createdAt, id); err != nil {
			t.Fatalf("set %s created_at: %v", name, err)
		}
	}

	wrongTag := mustCreateDoneLink(t, links, ctx,
		"https://today.example.com/wrong-tag", "other", "today.example.com")
	wrongDomain := mustCreateDoneLink(t, links, ctx,
		"https://other.example.com/wrong-domain", "today", "other.example.com")
	for _, id := range []any{wrongTag, wrongDomain} {
		if _, err := pool.Exec(ctx, `UPDATE links SET created_at = $1, library_kind = 'reading', library_kind_source = 'user' WHERE id = $2`, from.Add(time.Hour), id); err != nil {
			t.Fatalf("set orthogonal fixture created_at: %v", err)
		}
	}

	domain := "today.example.com"
	kind := model.LibraryKindReading
	seen := make(map[string]int, total)
	var after *repository.ListLinksCursor
	for pageIndex := 0; pageIndex < 10; pageIndex++ {
		items, _, err := links.ListDone(ctx, repository.ListLinksFilter{
			Statuses:      []string{string(model.LinkStatusDone)},
			Tags:          []string{"today"},
			Domain:        &domain,
			LibraryKind:   &kind,
			CreatedFrom:   &from,
			CreatedBefore: &before,
			Limit:         30,
			Cursor:        true,
			After:         after,
		})
		if err != nil {
			t.Fatalf("page %d ListDone: %v", pageIndex, err)
		}
		for _, item := range items {
			seen[item.ID.String()]++
			if item.CreatedAt.Before(from) || !item.CreatedAt.Before(before) {
				t.Fatalf("item %s created_at %s outside [%s, %s)", item.ID, item.CreatedAt, from, before)
			}
		}
		if len(items) < 30 {
			break
		}
		last := items[len(items)-1]
		after = &repository.ListLinksCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if len(seen) != total {
		t.Fatalf("range pagination reached %d links, want %d", len(seen), total)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("link %s returned %d times, want exactly once", id, count)
		}
	}
}
