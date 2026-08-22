package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWriteQueriesSetUpdatedAtExplicitly(t *testing.T) {
	t.Parallel()

	for name, query := range map[string]string{
		"insertLinkSQL":         insertLinkSQL,
		"updateLinkStateSQL":    updateLinkStateSQL,
		"updateLinkAnalysisSQL": updateLinkAnalysisSQL,
	} {
		if !strings.Contains(query, "updated_at = NOW()") && !strings.Contains(query, "NOW(), NOW()") {
			t.Fatalf("%s = %q, want explicit updated_at handling", name, query)
		}
	}
}

func TestReadQueriesStayOnExistingSchema(t *testing.T) {
	t.Parallel()

	// The done-links COUNT(*) SQL is no longer a static const — it is
	// assembled inline by buildListDoneSQL alongside the status predicate,
	// and its schema-drift coverage moved to TestBuildListDoneSQLStaysOnSchema
	// (which now asserts on both the list and count query strings).
	if !strings.Contains(listTagCountsSQL, "links") {
		t.Fatalf("listTagCountsSQL = %q, want links table reference", listTagCountsSQL)
	}
}

// TestBuildListDoneSQLStaysOnSchema replaces the schema-drift coverage
// the static `listDoneLinksSQL` constant used to provide before Wave
// 12.2 H4-C moved that query into the dynamically-built buildListDoneSQL
// helper. This test exercises the build helper directly with every filter
// knob set so column and table drift cannot hide behind string assembly.
func TestBuildListDoneSQLStaysOnSchema(t *testing.T) {
	t.Parallel()

	domain := "example.com"
	contentType := "article"
	listSQL, countSQL, _, _ := buildListDoneSQL(ListLinksFilter{
		Tags:        []string{"go"},
		Domain:      &domain,
		ContentType: &contentType,
		Limit:       20,
		Offset:      0,
	})

	for name, query := range map[string]string{
		"buildListDoneSQL.list":  listSQL,
		"buildListDoneSQL.count": countSQL,
	} {
		if !strings.Contains(query, "links") {
			t.Fatalf("%s = %q, want links table reference", name, query)
		}
	}
}

// TestBuildListDoneSQLStatusFilter pins the ?status= filter contract at
// the SQL-assembly layer: an empty Statuses set keeps the historical
// literal `status = 'done'` (no positional arg, so the partial index
// idx_links_done_created still matches), while a non-empty set switches
// to `status = ANY($1::text[])` with the slice as the leading arg and
// the remaining filters re-numbered after it.
func TestBuildListDoneSQLStatusFilter(t *testing.T) {
	t.Parallel()

	t.Run("default empty set keeps status = 'done'", func(t *testing.T) {
		t.Parallel()
		listSQL, countSQL, listArgs, countArgs := buildListDoneSQL(ListLinksFilter{Limit: 20})
		if !strings.Contains(listSQL, "WHERE deleted_at IS NULL AND status = 'done'") {
			t.Fatalf("list SQL = %q, want live rows with status = 'done'", listSQL)
		}
		if !strings.Contains(countSQL, "WHERE deleted_at IS NULL AND status = 'done'") {
			t.Fatalf("count SQL = %q, want live rows with status = 'done'", countSQL)
		}
		if strings.Contains(listSQL, "ANY(") {
			t.Fatalf("default path must not use ANY(): %q", listSQL)
		}
		if len(listArgs) != 2 || listArgs[0] != 20 || listArgs[1] != 0 {
			t.Fatalf("list args = %#v, want limit and offset", listArgs)
		}
		if len(countArgs) != 0 {
			t.Fatalf("count args = %d, want none", len(countArgs))
		}
	})

	t.Run("status set uses ANY with leading arg and renumbers filters", func(t *testing.T) {
		t.Parallel()
		domain := "example.com"
		statuses := []string{"pending", "processing", "failed"}
		listSQL, countSQL, listArgs, countArgs := buildListDoneSQL(ListLinksFilter{
			Statuses: statuses,
			Tags:     []string{"go"},
			Domain:   &domain,
			Limit:    20,
		})
		if !strings.Contains(listSQL, "WHERE deleted_at IS NULL AND status = ANY($1::text[])") {
			t.Fatalf("list SQL = %q, want live rows with status = ANY($1::text[])", listSQL)
		}
		if !strings.Contains(countSQL, "WHERE deleted_at IS NULL AND status = ANY($1::text[])") {
			t.Fatalf("count SQL = %q, want live rows with status = ANY($1::text[])", countSQL)
		}
		if !strings.Contains(listSQL, "tags @> $2::text[]") {
			t.Fatalf("list SQL = %q, want tags at $2", listSQL)
		}
		if !strings.Contains(listSQL, "domain = $3") {
			t.Fatalf("list SQL = %q, want domain at $3", listSQL)
		}
		if len(listArgs) != 5 {
			t.Fatalf("list args = %d, want 5", len(listArgs))
		}
		gotStatuses, ok := listArgs[0].([]string)
		if !ok || !equalStringSliceRepo(gotStatuses, statuses) {
			t.Fatalf("list args[0] = %#v, want %#v", listArgs[0], statuses)
		}
		if len(countArgs) != 3 {
			t.Fatalf("count args = %d, want 3", len(countArgs))
		}
	})

	t.Run("cursor mode carries status set as first argument", func(t *testing.T) {
		t.Parallel()
		listSQL, args := buildListDoneCursorSQL(ListLinksFilter{
			Statuses: []string{"failed"},
			Limit:    10,
			Cursor:   true,
		})
		if !strings.Contains(listSQL, "WHERE deleted_at IS NULL AND status = ANY($1::text[])") {
			t.Fatalf("cursor SQL = %q, want live rows with status = ANY($1::text[])", listSQL)
		}
		if len(args) != 2 {
			t.Fatalf("cursor args = %d, want 2", len(args))
		}
	})
}

func equalStringSliceRepo(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestBuildListDoneCursorSQLSkipsCount pins the cursor-mode SQL: it
// must include the (created_at, id) tuple comparison and must NOT
// include COUNT(*) OVER() (the whole point of cursor mode is to skip
// the windowed count).
func TestBuildListDoneCursorSQLSkipsCount(t *testing.T) {
	t.Parallel()

	domain := "example.com"
	listSQL, args := buildListDoneCursorSQL(ListLinksFilter{
		Tags:   []string{"go"},
		Domain: &domain,
		Limit:  20,
		Cursor: true,
		After:  &ListLinksCursor{},
	})
	if strings.Contains(listSQL, "COUNT(*) OVER") {
		t.Fatalf("cursor SQL = %q, must skip windowed count", listSQL)
	}
	if !strings.Contains(listSQL, "(created_at, id) <") {
		t.Fatalf("cursor SQL = %q, want tuple comparison", listSQL)
	}
	if !strings.Contains(listSQL, "ORDER BY created_at DESC, id DESC") {
		t.Fatalf("cursor SQL = %q, want stable two-key ordering", listSQL)
	}
	if len(args) != 5 {
		t.Fatalf("cursor args count = %d, want 5", len(args))
	}
}

func TestBuildListDoneSQLCreatedRangeIsHalfOpenAndComposable(t *testing.T) {
	t.Parallel()

	domain := "example.com"
	from := time.Date(2026, 8, 10, 16, 0, 0, 0, time.UTC)
	before := time.Date(2026, 8, 11, 16, 0, 0, 0, time.UTC)
	cursorTime := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)
	cursorID := uuid.MustParse("00000000-0000-0000-0000-000000000063")
	filter := ListLinksFilter{
		Statuses:      []string{"done"},
		Tags:          []string{"reader"},
		Domain:        &domain,
		CreatedFrom:   &from,
		CreatedBefore: &before,
		Limit:         30,
	}

	listSQL, countSQL, listArgs, countArgs := buildListDoneSQL(filter)
	for name, query := range map[string]string{"list": listSQL, "count": countSQL} {
		if !strings.Contains(query, "tags @> $2::text[] AND domain = $3 AND created_at >= $4 AND created_at < $5") {
			t.Fatalf("%s SQL = %q, want filters followed by half-open created range", name, query)
		}
	}
	if len(countArgs) != 5 || countArgs[3] != from || countArgs[4] != before {
		t.Fatalf("count args = %#v, want status/tags/domain/from/before", countArgs)
	}
	if len(listArgs) != 7 || listArgs[3] != from || listArgs[4] != before || listArgs[5] != 30 || listArgs[6] != 0 {
		t.Fatalf("list args = %#v, want range before limit/offset", listArgs)
	}

	filter.Cursor = true
	filter.After = &ListLinksCursor{CreatedAt: cursorTime, ID: cursorID}
	cursorSQL, cursorArgs := buildListDoneCursorSQL(filter)
	if !strings.Contains(cursorSQL, "created_at >= $4 AND created_at < $5 AND (created_at, id) < ($6, $7)") {
		t.Fatalf("cursor SQL = %q, want range before cursor predicate", cursorSQL)
	}
	if len(cursorArgs) != 8 || cursorArgs[3] != from || cursorArgs[4] != before || cursorArgs[5] != cursorTime || cursorArgs[6] != cursorID || cursorArgs[7] != 30 {
		t.Fatalf("cursor args = %#v, want range/cursor/limit positional order", cursorArgs)
	}
}

func TestTagCountQueryMatchesRawUnnestSemantics(t *testing.T) {
	t.Parallel()

	if strings.Contains(listTagCountsSQL, "SELECT DISTINCT") {
		t.Fatalf("listTagCountsSQL = %q, want raw unnest counting without DISTINCT", listTagCountsSQL)
	}
	if !strings.Contains(listTagCountsSQL, "unnest(tags)") {
		t.Fatalf("listTagCountsSQL = %q, want unnest(tags) counting", listTagCountsSQL)
	}
	if !strings.Contains(listTagCountsSQL, "LIMIT 1000") {
		t.Fatalf("listTagCountsSQL = %q, want LIMIT 1000 cap (M-3)", listTagCountsSQL)
	}
	if !strings.Contains(listDistinctTagsSQL, "LIMIT 1000") {
		t.Fatalf("listDistinctTagsSQL = %q, want LIMIT 1000 cap", listDistinctTagsSQL)
	}
}

// TestBuildListDoneSQLLowConfidenceFilter 锁住 low_confidence 筛选进两条分页
// 路径的 SQL，并且**只在显式给值时**出现。
//
// 为什么值得单独一条：这个筛选存在的理由是让「低置信」视图能翻到底。它此前是
// 前端在最新 100 条里过滤，越老的低置信链接越看不见——而老的恰恰是最该复核的。
// 如果谓词只加进了 offset 路径而漏了 cursor 路径，症状是「翻页翻着翻着混进了
// 正常链接」，很难联想回这里。
func TestBuildListDoneSQLLowConfidenceFilter(t *testing.T) {
	t.Parallel()

	yes, no := true, false

	t.Run("offset 路径带谓词与参数", func(t *testing.T) {
		t.Parallel()
		listSQL, countSQL, listArgs, countArgs := buildListDoneSQL(
			ListLinksFilter{LowConfidence: &yes, Limit: 30})

		if !strings.Contains(listSQL, "is_low_confidence = $") {
			t.Fatalf("list SQL 缺少 is_low_confidence 谓词: %s", listSQL)
		}
		if !strings.Contains(countSQL, "is_low_confidence = $") {
			t.Fatalf("count SQL 缺少 is_low_confidence 谓词: %s", countSQL)
		}
		// 参数必须真的传下去；谓词在但值没进 args 会直接报参数个数不符。
		if !containsBool(countArgs, true) {
			t.Fatalf("countArgs 未包含筛选值: %#v", countArgs)
		}
		if !containsBool(listArgs, true) {
			t.Fatalf("listArgs 未包含筛选值: %#v", listArgs)
		}
	})

	t.Run("cursor 路径带谓词与参数", func(t *testing.T) {
		t.Parallel()
		listSQL, listArgs := buildListDoneCursorSQL(
			ListLinksFilter{LowConfidence: &yes, Limit: 30, Cursor: true})

		if !strings.Contains(listSQL, "is_low_confidence = $") {
			t.Fatalf("cursor SQL 缺少 is_low_confidence 谓词: %s", listSQL)
		}
		if !containsBool(listArgs, true) {
			t.Fatalf("cursor listArgs 未包含筛选值: %#v", listArgs)
		}
	})

	// false 是有意义的筛选值（「只看高置信」），不能被当成「未提供」丢掉。
	t.Run("显式 false 同样生成谓词", func(t *testing.T) {
		t.Parallel()
		_, _, _, countArgs := buildListDoneSQL(
			ListLinksFilter{LowConfidence: &no, Limit: 30})
		if !containsBool(countArgs, false) {
			t.Fatalf("显式 false 未进入参数: %#v", countArgs)
		}
	})

	// nil = 不筛选。多出一条 `is_low_confidence = $n` 会让默认列表少掉一半数据。
	// 注意匹配的是谓词而不是列名——is_low_confidence 本来就在 SELECT 列表里。
	t.Run("未提供时不生成谓词", func(t *testing.T) {
		t.Parallel()
		listSQL, countSQL, _, _ := buildListDoneSQL(ListLinksFilter{Limit: 30})
		if strings.Contains(listSQL, "is_low_confidence = $") || strings.Contains(countSQL, "is_low_confidence = $") {
			t.Fatalf("未提供筛选时不该出现谓词:\nlist=%s\ncount=%s", listSQL, countSQL)
		}
	})
}

func containsBool(args []any, want bool) bool {
	for _, a := range args {
		if v, ok := a.(bool); ok && v == want {
			return true
		}
	}
	return false
}
