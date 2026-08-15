package dbintegration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/repository"
	"webtag/internal/representation"
	"webtag/internal/service"
)

func TestReaderThoughtSearchPostgresSnapshotAndCursorContract(t *testing.T) {
	pool := StartPostgres(t)
	repo := repository.NewPGXReaderVNextRepository(pool)
	ctx := t.Context()

	updatedAt := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	snapshot := func(body, source, quote string) map[string]any {
		return map[string]any{
			"body":       body,
			"source":     source,
			"quote":      map[string]string{"exact": quote},
			"updated_at": updatedAt.Format(time.RFC3339Nano),
		}
	}

	seedReaderThoughtSearchFixture(t, pool, "active-body", "active-body-token", "active-source", "active quote", false, updatedAt, nil)
	seedReaderThoughtSearchFixture(t, pool, "tombstone-body", "mutable body", "mutable-source", "mutable quote", false, updatedAt, snapshot("snapshot-body-token", "snapshot-body-source", "snapshot body quote"))
	seedReaderThoughtSearchFixture(t, pool, "tombstone-quote", "mutable body", "mutable-source", "mutable quote", false, updatedAt, snapshot("snapshot quote body", "snapshot-quote-source", "snapshot-quote-token"))
	seedReaderThoughtSearchFixture(t, pool, "tombstone-source", "mutable body", "mutable-source", "mutable quote", false, updatedAt, snapshot("snapshot source body", "snapshot-source-token", "snapshot source quote"))
	seedReaderThoughtSearchFixture(t, pool, "snapshot-authoritative", "mutable-authority-token", "mutable-source", "mutable quote", false, updatedAt, snapshot("snapshot-authority-token", "snapshot-authority-source", "snapshot authority quote"))
	seedReaderThoughtSearchFixture(t, pool, "user-deleted", "deleted-body-token", "deleted-source", "deleted quote", true, updatedAt, snapshot("deleted-body-token", "deleted-source", "deleted quote"))

	assertSingle := func(ctx context.Context, query, wantID string, wantTombstone bool) {
		t.Helper()
		items, total, next, err := repo.SearchThoughts(ctx, query, "", 20)
		if err != nil {
			t.Fatalf("SearchThoughts(%q): %v", query, err)
		}
		if total != 1 || len(items) != 1 || items[0].ID != wantID || next != "" {
			t.Fatalf("SearchThoughts(%q) = items=%#v total=%d next=%q, want only %q", query, items, total, next, wantID)
		}
		if got := items[0].LifecycleStatus == "tombstone"; got != wantTombstone {
			t.Fatalf("SearchThoughts(%q) lifecycle status = %q, want tombstone=%v", query, items[0].LifecycleStatus, wantTombstone)
		}
	}

	assertSingle(ctx, "active-body-token", "active-body", false)
	assertSingle(ctx, "snapshot-body-token", "tombstone-body", true)
	assertSingle(ctx, "snapshot-quote-token", "tombstone-quote", true)
	assertSingle(ctx, "snapshot-source-token", "tombstone-source", true)
	assertSingle(ctx, "snapshot-authority-token", "snapshot-authoritative", true)

	for _, query := range []string{"mutable-authority-token", "deleted-body-token"} {
		items, total, next, err := repo.SearchThoughts(ctx, query, "", 20)
		if err != nil {
			t.Fatalf("SearchThoughts(%q): %v", query, err)
		}
		if total != 0 || len(items) != 0 || next != "" {
			t.Fatalf("SearchThoughts(%q) = items=%#v total=%d next=%q, want no mutable or user-deleted projection", query, items, total, next)
		}
	}

	const malformedThoughtID = "malformed-search-snapshot"
	const mutableSentinel = "mutable-sentinel-must-not-search"
	const malformedSnapshotSentinel = "malformed-snapshot-must-not-search"
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO reader_thoughts (id,host_kind,host_id,target,quote,body,source,deleted,last_sequence,created_at,updated_at)
		VALUES ($1,'note','malformed-host','{"kind":"note"}'::jsonb,'{"exact":"mutable quote"}'::jsonb,$2,$3,false,1,$4,$4)`,
		malformedThoughtID, mutableSentinel, mutableSentinel, updatedAt,
	); err != nil {
		t.Fatalf("seed malformed search thought: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot,created_at)
		VALUES ($1,'note','malformed-host','note_deleted',$2::jsonb,$3)`,
		malformedThoughtID,
		readerVNextJSON(t, map[string]any{
			"id": malformedThoughtID, "body": malformedSnapshotSentinel,
			"updated_at": updatedAt.Format(time.RFC3339Nano),
		}), updatedAt,
	); err != nil {
		t.Fatalf("seed malformed search tombstone: %v", err)
	}
	for _, query := range []string{mutableSentinel, malformedSnapshotSentinel} {
		items, total, next, err := repo.SearchThoughts(ctx, query, "", 20)
		if err != nil {
			t.Fatalf("SearchThoughts(%q) malformed tombstone: %v", query, err)
		}
		if total != 0 || len(items) != 0 || next != "" {
			t.Fatalf("SearchThoughts(%q) = items=%#v total=%d next=%q, want malformed tombstone fail-closed", query, items, total, next)
		}
	}

	for index, invalidVersion := range []any{"1", json.RawMessage(`1.0`), nil, 2} {
		thoughtID := fmt.Sprintf("invalid-version-search-%d", index)
		mutableToken := fmt.Sprintf("invalid-version-mutable-%d", index)
		snapshotToken := fmt.Sprintf("invalid-version-snapshot-%d", index)
		invalidSnapshot := snapshot(snapshotToken, "invalid-version-source", "invalid version quote")
		invalidSnapshot["snapshot_version"] = invalidVersion
		seedReaderThoughtSearchFixture(t, pool, thoughtID, mutableToken, mutableToken, "mutable quote", false, updatedAt, invalidSnapshot)
		for _, query := range []string{mutableToken, snapshotToken} {
			items, total, next, err := repo.SearchThoughts(ctx, query, "", 20)
			if err != nil {
				t.Fatalf("SearchThoughts(%q) invalid-version tombstone: %v", query, err)
			}
			if total != 0 || len(items) != 0 || next != "" {
				t.Fatalf("SearchThoughts(%q) = items=%#v total=%d next=%q, want invalid-version tombstone fail-closed", query, items, total, next)
			}
		}
	}

	for number := 1; number <= 21; number++ {
		seedReaderThoughtSearchFixture(t, pool, fmt.Sprintf("page-%02d", number), "cursor-page-token", "page-source", "page quote", false, updatedAt, nil)
	}
	searchService := service.NewLibrarySearchServiceWithMetricsAndOptions(
		repository.NewPGXLinkRepository(pool), repository.NewPGXSiteRepository(pool), nil, nil,
		service.LibrarySearchServiceOptions{CursorSigningKey: "dbintegration-thought-search-key"},
		repo,
	)
	installationA := thoughtSearchIdentityContext(t, "40000000-0000-0000-0000-000000000004")
	installationB := thoughtSearchIdentityContext(t, "50000000-0000-0000-0000-000000000005")
	firstResponse, err := searchService.Search(installationA, "cursor-page-token", 20, 20, 20, "")
	if err != nil {
		t.Fatalf("first cursor page: %v", err)
	}
	if firstResponse.Thoughts == nil {
		t.Fatal("first cursor page omitted thought group")
	}
	first, firstTotal, cursor := firstResponse.Thoughts.Items, firstResponse.Thoughts.TotalHint, firstResponse.Thoughts.NextCursor
	if firstTotal != 21 || len(first) != 20 || cursor == "" {
		t.Fatalf("first cursor page = items=%d total=%d cursor=%q, want 20/21/nonempty", len(first), firstTotal, cursor)
	}
	for index, item := range first {
		wantID := fmt.Sprintf("page-%02d", 21-index)
		if item.ID != wantID {
			t.Fatalf("first cursor page item %d = %q, want timestamp tie-break ID %q", index, item.ID, wantID)
		}
	}
	// Both writes commit after the first page's authority. A backdated new row
	// must not enter the stream, and a later lifecycle snapshot must not replace
	// the active projection until a fresh search begins.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reader_thoughts (id,host_kind,host_id,target,quote,body,source,deleted,last_sequence,created_at,updated_at)
		VALUES ('page-00-late','note','host-page-00-late','{"kind":"note"}'::jsonb,'{}'::jsonb,'cursor-page-token','page-source',false,2,NOW(),$1)`,
		updatedAt,
	); err != nil {
		t.Fatalf("seed post-snapshot thought: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot)
		VALUES ('page-01','note','host-page-01','note_deleted','{"lifecycle_sequence":2}'::jsonb)`,
	); err != nil {
		t.Fatalf("seed post-snapshot tombstone: %v", err)
	}
	secondResponse, err := searchService.Search(installationA, "cursor-page-token", 20, 20, 20, cursor)
	if err != nil {
		t.Fatalf("second cursor page: %v", err)
	}
	if secondResponse.Thoughts == nil {
		t.Fatal("second cursor page omitted thought group")
	}
	second, secondTotal, secondCursor := secondResponse.Thoughts.Items, secondResponse.Thoughts.TotalHint, secondResponse.Thoughts.NextCursor
	if secondTotal != 21 || len(second) != 1 || second[0].ID != "page-01" || secondCursor != "" {
		t.Fatalf("second cursor page = items=%#v total=%d cursor=%q, want page-01/21/empty", second, secondTotal, secondCursor)
	}
	if second[0].LifecycleStatus != "active" {
		t.Fatalf("post-snapshot lifecycle status = %q, want active authority", second[0].LifecycleStatus)
	}
	seen := make(map[string]struct{}, 21)
	for _, page := range [][]dto.ReaderThoughtSearchResponse{first, second} {
		for _, item := range page {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("cursor pagination returned duplicate thought %q", item.ID)
			}
			seen[item.ID] = struct{}{}
		}
	}
	if len(seen) != 21 {
		t.Fatalf("cursor pagination returned %d unique thoughts, want 21", len(seen))
	}

	assertThoughtSearchInvalidCursor(t, searchService, installationA, "different cursor query", cursor)
	assertThoughtSearchInvalidCursor(t, searchService, installationB, "cursor-page-token", cursor)
	assertThoughtSearchInvalidCursor(t, searchService, installationA, "cursor-page-token", tamperThoughtSearchCursorTotal(t, cursor))
	assertThoughtSearchInvalidCursor(t, searchService, installationA, "cursor-page-token", extractRepositoryThoughtCursor(t, cursor))
}

func thoughtSearchIdentityContext(t *testing.T, installationID string) context.Context {
	t.Helper()
	identity, err := representation.NewClientIdentity(representation.VersionBase{RepresentationNamespace: uuid.MustParse(installationID)})
	if err != nil {
		t.Fatalf("NewClientIdentity(%q): %v", installationID, err)
	}
	return representation.WithClientIdentity(t.Context(), identity)
}

func assertThoughtSearchInvalidCursor(t *testing.T, search *service.LibrarySearchService, ctx context.Context, query, cursor string) {
	t.Helper()
	_, err := search.Search(ctx, query, 20, 20, 20, cursor)
	var coder interface{ HTTPErrorCode() string }
	if !errors.As(err, &coder) || coder.HTTPErrorCode() != httperr.CodeInvalidCursor {
		t.Fatalf("Search(cursor=%q) error = %v, want %q", cursor, err, httperr.CodeInvalidCursor)
	}
}

func extractRepositoryThoughtCursor(t *testing.T, cursor string) string {
	t.Helper()
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 {
		t.Fatalf("signed thought cursor = %q, want payload.signature", cursor)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode signed thought cursor: %v", err)
	}
	var payload struct {
		RepositoryCursor string `json:"repository_cursor"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil || payload.RepositoryCursor == "" {
		t.Fatalf("decode signed thought cursor payload: payload=%#v err=%v", payload, err)
	}
	return payload.RepositoryCursor
}

func tamperThoughtSearchCursorTotal(t *testing.T, cursor string) string {
	t.Helper()
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 {
		t.Fatalf("signed thought cursor = %q, want payload.signature", cursor)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode signed thought cursor: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("decode signed thought cursor payload: %v", err)
	}
	repositoryCursor, ok := payload["repository_cursor"].(string)
	if !ok {
		t.Fatalf("signed thought cursor repository_cursor = %#v", payload["repository_cursor"])
	}
	repositoryJSON, err := base64.RawURLEncoding.DecodeString(repositoryCursor)
	if err != nil {
		t.Fatalf("decode repository thought cursor: %v", err)
	}
	var repositoryPayload map[string]any
	if err := json.Unmarshal(repositoryJSON, &repositoryPayload); err != nil {
		t.Fatalf("decode repository thought cursor payload: %v", err)
	}
	repositoryPayload["total"] = 999
	repositoryJSON, err = json.Marshal(repositoryPayload)
	if err != nil {
		t.Fatalf("encode tampered repository thought cursor: %v", err)
	}
	payload["repository_cursor"] = base64.RawURLEncoding.EncodeToString(repositoryJSON)
	payloadJSON, err = json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode tampered signed thought cursor: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(payloadJSON) + "." + parts[1]
}

func seedReaderThoughtSearchFixture(
	t *testing.T,
	pool *pgxpool.Pool,
	id, body, source, quote string,
	deleted bool,
	updatedAt time.Time,
	snapshot map[string]any,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO reader_thoughts (id,host_kind,host_id,target,quote,body,source,deleted,last_sequence,created_at,updated_at)
		VALUES ($1,'note',$2,'{"kind":"note"}'::jsonb,$3::jsonb,$4,$5,$6,1,$7,$7)`,
		id, "host-"+id, readerVNextJSON(t, map[string]string{"exact": quote}), body, source, deleted, updatedAt,
	); err != nil {
		t.Fatalf("seed thought %q: %v", id, err)
	}
	if snapshot == nil {
		return
	}
	// Search must accept exactly the same complete immutable snapshot shape as
	// replay. Keep fixture tombstones realistic so malformed rows can have their
	// own fail-closed assertion below.
	for name, value := range map[string]any{
		"snapshot_version":       1,
		"id":                     id,
		"host_kind":              "note",
		"host_id":                "host-" + id,
		"link_id":                nil,
		"type":                   "thought",
		"target":                 map[string]any{"kind": "note", "host_id": "host-" + id, "version": map[string]any{"note_revision": 1}},
		"created_at":             updatedAt.Format(time.RFC3339Nano),
		"updated_at":             updatedAt.Format(time.RFC3339Nano),
		"original_host_snapshot": map[string]any{"content": "frozen host"},
		"original_host_identity": map[string]any{"kind": "note", "id": "host-" + id},
		"frozen_at":              updatedAt.Format(time.RFC3339Nano),
	} {
		if _, exists := snapshot[name]; !exists {
			snapshot[name] = value
		}
	}
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO reader_thought_tombstones (thought_id,host_kind,host_id,reason,snapshot,created_at)
		VALUES ($1,'note',$2,'note_deleted',$3::jsonb,$4)`,
		id, "host-"+id, readerVNextJSON(t, snapshot), updatedAt,
	); err != nil {
		t.Fatalf("seed thought tombstone %q: %v", id, err)
	}
}
