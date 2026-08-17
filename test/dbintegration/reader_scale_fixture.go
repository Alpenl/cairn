// Reader vNext scale fixture — the reproducible data set behind the Reader
// hot-path baseline (issue #40 phase 0).
//
// Everything in here is a pure function of readerScaleSeed: identifiers are
// UUIDv5 derived from the seed, text bodies are SHA-256 keystreams, and every
// timestamp is an offset from a frozen base instant. There is no clock, no
// rand.Source without an explicit seed, and no database default in any insert
// path. Running SeedReaderScaleFixture against an empty database therefore
// reproduces byte-identical rows on any machine, which is what makes the
// per-table digests in test/fixtures/reader-vnext-performance/manifest.json a
// meaningful contract rather than a snapshot of one developer's laptop.
//
// The fixture is deliberately built with pgx CopyFrom rather than through the
// production repositories. Two reasons:
//
//   - Speed. ~37k rows land in a couple of seconds. Driving them through
//     Create/Update service calls would take minutes and make the fixture
//     itself the slowest part of a measurement run.
//   - Isolation. The point of phase 0 is to freeze what the *read* paths cost.
//     Seeding through the write paths would bake the write paths' current
//     behaviour into the fixture, so a later write-path change would silently
//     move the baseline.
//
// The shapes below are chosen to expose the specific costs issue #40 names, not
// to be pretty: heavy inbox bodies sit at the top of the first screen, TODO
// checkboxes are spread across both thought and note hosts so Home's
// reconcile has real work, and link tags/domains fan out wide enough that the
// Activity rebuild's unnest is not a rounding error.
package dbintegration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// ReaderScaleFixtureID and ReaderScaleSeed are the fixture's identity. Any
	// change to either is a new fixture: bump the id, regenerate the manifest
	// with WEBTAG_READER_PERF_WRITE_MANIFEST=1, and treat every recorded
	// baseline as invalidated. Never edit the generator while keeping the id.
	ReaderScaleFixtureID = "reader-vnext-scale-i40"
	ReaderScaleSeed      = "reader-vnext-scale-40-2026-08-17"

	// ReaderScaleSearchToken is embedded in a fixed subset of thought bodies
	// and tombstone snapshots so the Thought search path has a deterministic,
	// non-trivial match count. It is intentionally a single lowercase word with
	// no regex metacharacters: the production query wraps it in %…% and feeds
	// it to ILIKE, so anything else would make the fixture's match count depend
	// on collation or escaping rules.
	ReaderScaleSearchToken = "cairnbaseline"
)

// readerScaleBase is the frozen "now" of the fixture. It is in the past
// relative to any plausible run date because the Thought search path filters
// on created_at <= statement_timestamp(); a base in the future would silently
// empty the result set and make the search baseline meaningless.
var readerScaleBase = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// readerScaleNamespace anchors every derived UUID to the seed, so two fixtures
// with different seeds can never collide on a primary key.
var readerScaleNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("webtag:reader-scale:"+ReaderScaleSeed))

// ReaderScaleCounts is the declared size of the fixture. It is asserted against
// live row counts after seeding, so a generator bug that drops rows fails
// loudly instead of quietly shrinking the baseline.
type ReaderScaleCounts struct {
	Links              int `json:"link_count"`
	Thoughts           int `json:"thought_count"`
	ThoughtTombstones  int `json:"thought_tombstone_count"`
	Notes              int `json:"note_count"`
	InboxItems         int `json:"inbox_item_count"`
	FeedItems          int `json:"feed_item_count"`
	FeedSubscriptions  int `json:"feed_subscription_count"`
	FeedFolders        int `json:"feed_folder_count"`
	Sites              int `json:"site_count"`
	Categories         int `json:"category_count"`
	Categorizables     int `json:"categorizable_count"`
	Engagements        int `json:"engagement_count"`
	StandaloneTodos    int `json:"standalone_todo_count"`
	TodoProjectionRows int `json:"todo_projection_source_count"`
}

// ReaderScaleSpec is the full generator input. Callers do not construct it;
// ReaderScaleFixtureSpec returns the one frozen instance.
type ReaderScaleSpec struct {
	Counts ReaderScaleCounts

	// tagPool / domainPool size the Activity rebuild's output cardinality.
	tagPool    int
	domainPool int

	// siteEvery marks every Nth link as library_kind='site'. Site links carry a
	// sites + site_entries row and, per chk_links_site_has_no_content, no
	// summary or content at all.
	siteEvery int

	// checkboxThoughtEvery / checkboxNoteEvery control how many TODO projection
	// sources exist. Home and Todos both reconcile every projection with two
	// round trips each, so this number is the single biggest lever on their
	// measured cost. It is tuned to ~1000 projections: large enough that the
	// write amplification is unmistakable, small enough that 100 warm samples
	// finish in minutes rather than hours.
	checkboxThoughtEvery int
	checkboxNoteEvery    int

	// heavyInboxItems inbox rows carry a 1 MiB body and are the newest rows, so
	// they land on the first screen of the default (status, updated_at DESC)
	// ordering. This is the payload pathology issue #40 phase 1.1 must remove.
	heavyInboxItems int
	heavyInboxBody  int
	inboxBody       int
	inboxNote       int
}

// ReaderScaleFixtureSpec returns the frozen fixture definition.
func ReaderScaleFixtureSpec() ReaderScaleSpec {
	spec := ReaderScaleSpec{
		Counts: ReaderScaleCounts{
			Links:             10000,
			Thoughts:          10000,
			ThoughtTombstones: 500,
			Notes:             2000,
			InboxItems:        1000,
			FeedItems:         10000,
			FeedSubscriptions: 50,
			FeedFolders:       5,
			Categories:        24,
			Engagements:       3000,
			StandaloneTodos:   200,
		},
		tagPool:              200,
		domainPool:           250,
		siteEvery:            20,
		checkboxThoughtEvery: 50,
		checkboxNoteEvery:    10,
		heavyInboxItems:      10,
		heavyInboxBody:       1 << 20,
		inboxBody:            8 << 10,
		inboxNote:            2 << 10,
	}
	spec.Counts.Sites = spec.Counts.Links / spec.siteEvery
	// One categorizable per 5th link and per 2nd inbox item.
	spec.Counts.Categorizables = spec.Counts.Links/5 + spec.Counts.InboxItems/2
	spec.Counts.TodoProjectionRows = readerScaleProjectionCount(spec)
	return spec
}

// readerScaleProjectionCount mirrors the checkbox generation below. It is
// declared rather than measured so a generator change that silently alters the
// TODO reconcile load fails the fixture contract instead of moving the
// baseline underneath a later phase.
func readerScaleProjectionCount(spec ReaderScaleSpec) int {
	total := 0
	for i := range spec.Counts.Thoughts {
		if readerScaleThoughtIsTombstoned(spec, i) {
			continue
		}
		if i%spec.checkboxThoughtEvery == 0 {
			total += readerScaleThoughtCheckboxes
		}
	}
	for i := range spec.Counts.Notes {
		if i%spec.checkboxNoteEvery == 0 {
			total += readerScaleNoteCheckboxes
		}
	}
	return total
}

const (
	readerScaleThoughtCheckboxes = 2
	readerScaleNoteCheckboxes    = 3
	// readerScaleSearchEvery seeds the search token into every Nth thought body,
	// tombstoned or not. The last 500 thoughts also carry a tombstone, so 5 of
	// the 100 token-bearing bodies are matched through the snapshot branch of
	// the search predicate rather than the active branch — which is deliberate:
	// the production query has two branches and a fixture that only exercised
	// one would hide half of it.
	readerScaleSearchEvery = 100
	// readerScaleTombSearch additionally seeds the token into every Nth
	// tombstone snapshot, giving the snapshot branch matches of its own.
	readerScaleTombSearch = 5
)

// readerScaleThoughtIsTombstoned reports whether thought i also gets a
// tombstone row. Tombstoned thoughts stay deleted=false — the production search
// requires that — but they drop out of Home's recent list and counts.
func readerScaleThoughtIsTombstoned(spec ReaderScaleSpec, i int) bool {
	return i >= spec.Counts.Thoughts-spec.Counts.ThoughtTombstones
}

func readerScaleUUID(key string) uuid.UUID {
	return uuid.NewSHA1(readerScaleNamespace, []byte(key))
}

// readerScaleText produces `size` bytes of deterministic ASCII from a label.
// Hex output keeps it single-byte-per-rune, so a byte budget is also a
// character budget and the resulting column widths are predictable.
func readerScaleText(size int, label string) string {
	if size <= 0 {
		return ""
	}
	var out strings.Builder
	out.Grow(size + 64)
	for block := 0; out.Len() < size; block++ {
		sum := sha256.Sum256([]byte(ReaderScaleSeed + "|" + label + "|" + strconv.Itoa(block)))
		out.WriteString(hex.EncodeToString(sum[:]))
	}
	return out.String()[:size]
}

func readerScaleAt(minutes int) time.Time {
	return readerScaleBase.Add(-time.Duration(minutes) * time.Minute)
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("reader scale fixture: marshal: " + err.Error())
	}
	return string(encoded)
}

// SeedReaderScaleFixture builds the entire fixture in one pass and leaves the
// database ANALYZEd. The caller must hand it an empty (freshly truncated)
// database; StartPostgres already guarantees that.
//
// VACUUM (ANALYZE) at the end is load-bearing for the plan-shape assertions,
// and plain ANALYZE is not enough. ANALYZE refreshes column statistics but
// leaves pg_class.relpages/reltuples for the *indexes* at whatever they were
// when the index was built — which, for the trigram indexes, is an empty
// reader_thoughts. The planner then costs a GIN scan as nearly free and picks
// it; once autovacuum gets around to the table the real index size appears and
// it switches to a sequential scan. Whichever side of that race the run lands
// on decides the recorded plan, so the assertion was intermittently red on a
// tree nobody had touched. VACUUM updates the index relation statistics too,
// which removes the race instead of hiding it behind a retry.
func SeedReaderScaleFixture(t testing.TB, pool *pgxpool.Pool) ReaderScaleSpec {
	t.Helper()
	spec := ReaderScaleFixtureSpec()
	ctx := context.Background()
	started := time.Now()

	linkIDs, siteLink := seedReaderScaleLinks(t, ctx, pool, spec)
	seedReaderScaleSites(t, ctx, pool, spec, linkIDs, siteLink)
	seedReaderScaleEngagement(t, ctx, pool, spec, linkIDs)
	seedReaderScaleThoughts(t, ctx, pool, spec, linkIDs)
	seedReaderScaleNotes(t, ctx, pool, spec)
	seedReaderScaleInbox(t, ctx, pool, spec)
	seedReaderScaleFeed(t, ctx, pool, spec, linkIDs)
	seedReaderScaleCategories(t, ctx, pool, spec, linkIDs)
	seedReaderScaleStandaloneTodos(t, ctx, pool, spec)

	if _, err := pool.Exec(ctx, `VACUUM (ANALYZE)`); err != nil {
		t.Fatalf("reader scale fixture: vacuum analyze: %v", err)
	}
	t.Logf("reader scale fixture: seeded id=%s seed=%s in %s", ReaderScaleFixtureID, ReaderScaleSeed, time.Since(started).Round(time.Millisecond))
	return spec
}

func copyReaderScale(t testing.TB, ctx context.Context, pool *pgxpool.Pool, table string, columns []string, rows [][]any) {
	t.Helper()
	copied, err := pool.CopyFrom(ctx, pgx.Identifier{table}, columns, pgx.CopyFromRows(rows))
	if err != nil {
		t.Fatalf("reader scale fixture: copy %s: %v", table, err)
	}
	if int(copied) != len(rows) {
		t.Fatalf("reader scale fixture: copy %s wrote %d rows, want %d", table, copied, len(rows))
	}
}

// ---------------------------------------------------------------- links

func seedReaderScaleLinks(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec) ([]uuid.UUID, []bool) {
	columns := []string{
		"id", "url", "title", "summary", "tags", "fetcher_type", "status",
		"created_at", "updated_at", "domain", "content_type", "path_depth",
		"parent_path", "description", "is_low_confidence", "source_kind",
		"source_key", "content", "content_format", "library_kind",
		"library_kind_source", "library_kind_locked", "content_revision",
		"first_collected_at", "last_recollected_at", "content_cjk_chars",
		"content_words", "content_source", "metadata_revision", "feed_managed",
		"requested_library_kind", "requested_library_kind_source",
	}
	rows := make([][]any, 0, spec.Counts.Links)
	ids := make([]uuid.UUID, spec.Counts.Links)
	siteLink := make([]bool, spec.Counts.Links)

	for i := range spec.Counts.Links {
		id := readerScaleUUID(fmt.Sprintf("link:%05d", i))
		ids[i] = id
		isSite := i%spec.siteEvery == 0
		siteLink[i] = isSite

		domain := fmt.Sprintf("host-%03d.example", i%spec.domainPool)
		tags := []string{
			fmt.Sprintf("tag-%03d", i%spec.tagPool),
			fmt.Sprintf("tag-%03d", (i*7)%spec.tagPool),
			fmt.Sprintf("tag-%03d", (i*13+5)%spec.tagPool),
		}
		collected := readerScaleAt(i)

		var summary, content any
		libraryKind := "reading"
		if isSite {
			libraryKind = "site"
		} else {
			summary = "Summary " + readerScaleText(180, fmt.Sprintf("link-summary-%05d", i))
			content = "Article " + readerScaleText(2048, fmt.Sprintf("link-content-%05d", i))
		}

		rows = append(rows, []any{
			id,
			fmt.Sprintf("https://%s/article/%05d", domain, i),
			fmt.Sprintf("Link %05d %s", i, readerScaleText(48, fmt.Sprintf("link-title-%05d", i))),
			summary,
			tags,
			"reader-scale-fixture",
			"done",
			collected,
			collected,
			domain,
			"article",
			2,
			"/article",
			"Description " + readerScaleText(120, fmt.Sprintf("link-desc-%05d", i)),
			false,
			"url",
			fmt.Sprintf("reader-scale:link:%05d", i),
			content,
			"plain",
			libraryKind,
			"auto",
			false,
			int64(1),
			collected,
			collected,
			320,
			640,
			"fetched",
			int64(1),
			false,
			"auto",
			"auto",
		})
	}
	copyReaderScale(t, ctx, pool, "links", columns, rows)
	return ids, siteLink
}

// ---------------------------------------------------------------- sites

func seedReaderScaleSites(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec, linkIDs []uuid.UUID, siteLink []bool) {
	siteColumns := []string{
		"id", "site_key", "name", "name_source", "intro", "intro_source",
		"homepage_url", "homepage_source", "icon_url", "icon_source",
		"user_note", "pinned", "primary_source", "grouping_locked",
		"needs_review", "revision", "first_collected_at", "last_collected_at",
		"created_at", "updated_at",
	}
	entryColumns := []string{
		"id", "site_id", "link_id", "entry_name", "entry_name_source",
		"purpose", "purpose_source", "normalized_url", "first_collected_at",
		"last_recollected_at", "created_at", "updated_at",
	}
	siteRows := make([][]any, 0, spec.Counts.Sites)
	entryRows := make([][]any, 0, spec.Counts.Sites)

	ordinal := 0
	for i := range linkIDs {
		if !siteLink[i] {
			continue
		}
		siteID := readerScaleUUID(fmt.Sprintf("site:%05d", ordinal))
		entryID := readerScaleUUID(fmt.Sprintf("site-entry:%05d", ordinal))
		domain := fmt.Sprintf("host-%03d.example", i%spec.domainPool)
		at := readerScaleAt(i)

		siteRows = append(siteRows, []any{
			siteID,
			fmt.Sprintf("reader-scale:site:%05d", ordinal),
			fmt.Sprintf("Site %04d", ordinal),
			"auto",
			"Intro " + readerScaleText(160, fmt.Sprintf("site-intro-%05d", ordinal)),
			"auto",
			fmt.Sprintf("https://%s/", domain),
			"auto",
			fmt.Sprintf("https://%s/favicon.ico", domain),
			"auto",
			"",
			false,
			"auto",
			false,
			false,
			int64(1),
			at, at, at, at,
		})
		entryRows = append(entryRows, []any{
			entryID,
			siteID,
			linkIDs[i],
			fmt.Sprintf("Entry %04d", ordinal),
			"auto",
			"Purpose " + readerScaleText(96, fmt.Sprintf("site-purpose-%05d", ordinal)),
			"auto",
			fmt.Sprintf("https://%s/article/%05d", domain, i),
			at, at, at, at,
		})
		ordinal++
	}
	copyReaderScale(t, ctx, pool, "sites", siteColumns, siteRows)
	copyReaderScale(t, ctx, pool, "site_entries", entryColumns, entryRows)
}

// ------------------------------------------------------------ engagement

func seedReaderScaleEngagement(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec, linkIDs []uuid.UUID) {
	columns := []string{"link_id", "read", "progress", "read_later", "last_opened", "updated_at"}
	rows := make([][]any, 0, spec.Counts.Engagements)
	seen := 0
	for i := range linkIDs {
		if i%spec.siteEvery == 0 {
			// Site-kind links are excluded from the reading library, so an
			// engagement row on one would never surface in Continue Reading.
			continue
		}
		if seen >= spec.Counts.Engagements {
			break
		}
		at := readerScaleAt(seen)
		// Strictly inside (0,1): Continue Reading filters progress > 0 AND < 1.
		progress := float32(0.05 + float64(seen%18)*0.05)
		rows = append(rows, []any{linkIDs[i], false, progress, seen%11 == 0, at, at})
		seen++
	}
	if len(rows) != spec.Counts.Engagements {
		t.Fatalf("reader scale fixture: built %d engagement rows, want %d", len(rows), spec.Counts.Engagements)
	}
	copyReaderScale(t, ctx, pool, "reader_engagement", columns, rows)
}

// -------------------------------------------------------------- thoughts

func readerScaleThoughtBody(spec ReaderScaleSpec, i int) string {
	var body strings.Builder
	body.WriteString(fmt.Sprintf("Thought %05d — ", i))
	body.WriteString(readerScaleText(180, fmt.Sprintf("thought-body-%05d", i)))
	if i%readerScaleSearchEvery == 0 {
		body.WriteString(" " + ReaderScaleSearchToken)
	}
	if !readerScaleThoughtIsTombstoned(spec, i) && i%spec.checkboxThoughtEvery == 0 {
		for box := range readerScaleThoughtCheckboxes {
			marker := " "
			if box%2 == 1 {
				marker = "x"
			}
			body.WriteString(fmt.Sprintf("\n- [%s] thought task %05d/%d", marker, i, box))
		}
	}
	return body.String()
}

func seedReaderScaleThoughts(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec, linkIDs []uuid.UUID) {
	thoughtColumns := []string{
		"id", "host_kind", "host_id", "link_id", "target", "quote", "body",
		"source", "deleted", "last_sequence", "created_at", "updated_at",
		"winner_logical_clock", "winner_device_id", "winner_op_id", "user_deleted",
	}
	tombColumns := []string{"thought_id", "host_kind", "host_id", "reason", "snapshot", "created_at"}

	thoughtRows := make([][]any, 0, spec.Counts.Thoughts)
	tombRows := make([][]any, 0, spec.Counts.ThoughtTombstones)

	for i := range spec.Counts.Thoughts {
		id := fmt.Sprintf("thought-%05d", i)
		linkID := linkIDs[i%len(linkIDs)]
		hostID := linkID.String()
		at := readerScaleAt(i)
		sequence := int64(i + 1)
		body := readerScaleThoughtBody(spec, i)
		target := mustJSON(map[string]any{
			"type":   "text-quote",
			"start":  i % 400,
			"end":    i%400 + 40,
			"prefix": readerScaleText(24, fmt.Sprintf("thought-prefix-%05d", i)),
		})
		var quote any
		if i%3 == 0 {
			quote = mustJSON(map[string]any{"exact": readerScaleText(120, fmt.Sprintf("thought-quote-%05d", i))})
		}

		thoughtRows = append(thoughtRows, []any{
			id, "link", hostID, linkID, target, quote, body, "user",
			false, sequence, at, at, sequence, "device-baseline",
			fmt.Sprintf("op-%05d", i), false,
		})

		if !readerScaleThoughtIsTombstoned(spec, i) {
			continue
		}
		tombIndex := i - (spec.Counts.Thoughts - spec.Counts.ThoughtTombstones)
		snapshotBody := fmt.Sprintf("Tombstoned thought %05d — %s", i, readerScaleText(160, fmt.Sprintf("tomb-body-%05d", i)))
		if tombIndex%readerScaleTombSearch == 0 {
			snapshotBody += " " + ReaderScaleSearchToken
		}
		// Every key below is required by the production search predicate's
		// `snapshot ?& ARRAY[...]` guard; drop one and the tombstone silently
		// stops matching, which would look like a search regression later.
		snapshot := mustJSON(map[string]any{
			"snapshot_version":       1,
			"id":                     id,
			"host_kind":              "link",
			"host_id":                hostID,
			"link_id":                hostID,
			"type":                   "thought",
			"body":                   snapshotBody,
			"target":                 map[string]any{"type": "text-quote", "start": 0, "end": 40},
			"quote":                  map[string]any{"exact": readerScaleText(80, fmt.Sprintf("tomb-quote-%05d", i))},
			"source":                 "user",
			"created_at":             at.Format(time.RFC3339Nano),
			"updated_at":             at.Format(time.RFC3339Nano),
			"original_host_snapshot": map[string]any{"title": fmt.Sprintf("Link %05d", i%len(linkIDs))},
			"original_host_identity": map[string]any{"host_kind": "link", "host_id": hostID},
			"frozen_at":              at.Format(time.RFC3339Nano),
			"lifecycle_sequence":     strconv.FormatInt(sequence, 10),
		})
		tombRows = append(tombRows, []any{id, "link", hostID, "superseded", snapshot, at})
	}

	if len(tombRows) != spec.Counts.ThoughtTombstones {
		t.Fatalf("reader scale fixture: built %d tombstones, want %d", len(tombRows), spec.Counts.ThoughtTombstones)
	}
	copyReaderScale(t, ctx, pool, "reader_thoughts", thoughtColumns, thoughtRows)
	copyReaderScale(t, ctx, pool, "reader_thought_tombstones", tombColumns, tombRows)
}

// ----------------------------------------------------------------- notes

func readerScaleNoteContent(spec ReaderScaleSpec, i int) string {
	var body strings.Builder
	body.WriteString(fmt.Sprintf("# Note %04d\n\n", i))
	body.WriteString(readerScaleText(1400, fmt.Sprintf("note-body-%04d", i)))
	if i%spec.checkboxNoteEvery == 0 {
		body.WriteString("\n\n## Tasks\n")
		for box := range readerScaleNoteCheckboxes {
			marker := " "
			if box == 1 {
				marker = "x"
			}
			body.WriteString(fmt.Sprintf("\n- [%s] note task %04d/%d", marker, i, box))
		}
	}
	return body.String()
}

func seedReaderScaleNotes(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec) {
	columns := []string{
		"id", "title", "published_content", "published_revision", "draft_content",
		"draft_revision", "draft_updated_at", "deleted_at", "created_at", "updated_at",
	}
	rows := make([][]any, 0, spec.Counts.Notes)
	for i := range spec.Counts.Notes {
		at := readerScaleAt(i)
		rows = append(rows, []any{
			readerScaleUUID(fmt.Sprintf("note:%04d", i)),
			fmt.Sprintf("Note %04d", i),
			readerScaleNoteContent(spec, i),
			int64(3),
			nil,
			int64(0),
			nil,
			nil,
			at,
			at,
		})
	}
	copyReaderScale(t, ctx, pool, "reader_notes", columns, rows)
}

// ----------------------------------------------------------------- inbox

func seedReaderScaleInbox(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec) {
	columns := []string{
		"id", "url", "identity_key", "source_kind", "title", "body", "summary",
		"suggested_tags", "tags", "status", "metadata_revision", "job_id",
		"created_at", "updated_at", "expires_at", "expired_at", "deleted_at",
		"note", "proposal_signals", "proposal_status",
	}
	rows := make([][]any, 0, spec.Counts.InboxItems)
	for i := range spec.Counts.InboxItems {
		// i < heavyInboxItems are the newest rows, so the 1 MiB bodies sit on
		// the first screen of (status, updated_at DESC, id DESC).
		at := readerScaleAt(i)
		bodySize := spec.inboxBody
		if i < spec.heavyInboxItems {
			bodySize = spec.heavyInboxBody
		}
		domain := fmt.Sprintf("inbox-%03d.example", i%spec.domainPool)
		rows = append(rows, []any{
			readerScaleUUID(fmt.Sprintf("inbox:%04d", i)),
			fmt.Sprintf("https://%s/capture/%04d", domain, i),
			fmt.Sprintf("reader-scale:inbox:%04d", i),
			"browser_capture",
			fmt.Sprintf("Inbox %04d", i),
			readerScaleText(bodySize, fmt.Sprintf("inbox-body-%04d", i)),
			"Summary " + readerScaleText(200, fmt.Sprintf("inbox-summary-%04d", i)),
			[]string{fmt.Sprintf("tag-%03d", i%spec.tagPool), "suggested"},
			[]string{fmt.Sprintf("tag-%03d", (i*3)%spec.tagPool)},
			"pending",
			int64(1),
			nil,
			at,
			at,
			at.Add(30 * 24 * time.Hour),
			nil,
			nil,
			readerScaleText(spec.inboxNote, fmt.Sprintf("inbox-note-%04d", i)),
			mustJSON(map[string]any{
				"confidence": 0.5 + float64(i%40)/100,
				"reason":     readerScaleText(64, fmt.Sprintf("inbox-signal-%04d", i)),
			}),
			"completed",
		})
	}
	copyReaderScale(t, ctx, pool, "reader_inbox", columns, rows)
}

// ------------------------------------------------------------------ feed

func seedReaderScaleFeed(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec, linkIDs []uuid.UUID) {
	folderColumns := []string{"id", "name", "created_at", "updated_at"}
	folderRows := make([][]any, 0, spec.Counts.FeedFolders)
	folderIDs := make([]uuid.UUID, spec.Counts.FeedFolders)
	for i := range spec.Counts.FeedFolders {
		id := readerScaleUUID(fmt.Sprintf("feed-folder:%02d", i))
		folderIDs[i] = id
		at := readerScaleAt(i)
		folderRows = append(folderRows, []any{id, fmt.Sprintf("Folder %02d", i), at, at})
	}
	copyReaderScale(t, ctx, pool, "feed_folders", folderColumns, folderRows)

	subColumns := []string{
		"id", "folder_id", "url", "site_url", "title", "description", "active",
		"next_fetch_at", "failure_count", "created_at", "updated_at", "canonical_url",
	}
	subRows := make([][]any, 0, spec.Counts.FeedSubscriptions)
	subIDs := make([]uuid.UUID, spec.Counts.FeedSubscriptions)
	for i := range spec.Counts.FeedSubscriptions {
		id := readerScaleUUID(fmt.Sprintf("feed-sub:%03d", i))
		subIDs[i] = id
		at := readerScaleAt(i)
		host := fmt.Sprintf("feed-%03d.example", i)
		subRows = append(subRows, []any{
			id,
			folderIDs[i%len(folderIDs)],
			fmt.Sprintf("https://%s/rss.xml", host),
			fmt.Sprintf("https://%s/", host),
			fmt.Sprintf("Feed %03d", i),
			"Feed description " + readerScaleText(120, fmt.Sprintf("feed-desc-%03d", i)),
			true,
			at,
			0,
			at,
			at,
			fmt.Sprintf("https://%s/rss.xml", host),
		})
	}
	copyReaderScale(t, ctx, pool, "feed_subscriptions", subColumns, subRows)

	itemColumns := []string{
		"id", "subscription_id", "external_id", "url", "title", "author",
		"summary", "content_text", "content_html", "published_at", "read_at",
		"starred", "read_later", "link_id", "created_at", "updated_at",
	}
	itemRows := make([][]any, 0, spec.Counts.FeedItems)
	for i := range spec.Counts.FeedItems {
		at := readerScaleAt(i)
		var readAt any
		if i%3 == 0 {
			readAt = at.Add(time.Hour)
		}
		var linkID any
		if i%10 == 0 {
			linkID = linkIDs[i%len(linkIDs)]
		}
		host := fmt.Sprintf("feed-%03d.example", i%spec.Counts.FeedSubscriptions)
		itemRows = append(itemRows, []any{
			readerScaleUUID(fmt.Sprintf("feed-item:%05d", i)),
			subIDs[i%len(subIDs)],
			fmt.Sprintf("reader-scale:feed-item:%05d", i),
			fmt.Sprintf("https://%s/entry/%05d", host, i),
			fmt.Sprintf("Feed item %05d", i),
			fmt.Sprintf("Author %02d", i%37),
			"Summary " + readerScaleText(200, fmt.Sprintf("feed-summary-%05d", i)),
			readerScaleText(1024, fmt.Sprintf("feed-text-%05d", i)),
			"<p>" + readerScaleText(1024, fmt.Sprintf("feed-html-%05d", i)) + "</p>",
			at,
			readAt,
			i%50 == 0,
			i%37 == 0,
			linkID,
			at,
			at,
		})
	}
	copyReaderScale(t, ctx, pool, "feed_items", itemColumns, itemRows)
}

// ------------------------------------------------------------ categories

func seedReaderScaleCategories(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec, linkIDs []uuid.UUID) {
	categoryColumns := []string{"id", "name", "created_at"}
	categoryRows := make([][]any, 0, spec.Counts.Categories)
	categoryIDs := make([]uuid.UUID, spec.Counts.Categories)
	for i := range spec.Counts.Categories {
		id := readerScaleUUID(fmt.Sprintf("category:%02d", i))
		categoryIDs[i] = id
		categoryRows = append(categoryRows, []any{id, fmt.Sprintf("Category %02d", i), readerScaleAt(i)})
	}
	copyReaderScale(t, ctx, pool, "reader_categories", categoryColumns, categoryRows)

	linkColumns := []string{"category_id", "host_kind", "host_id"}
	rows := make([][]any, 0, spec.Counts.Categorizables)
	for i := range linkIDs {
		if i%5 != 0 {
			continue
		}
		rows = append(rows, []any{categoryIDs[i%len(categoryIDs)], "link", linkIDs[i].String()})
	}
	for i := range spec.Counts.InboxItems {
		if i%2 != 0 {
			continue
		}
		id := readerScaleUUID(fmt.Sprintf("inbox:%04d", i))
		rows = append(rows, []any{categoryIDs[i%len(categoryIDs)], "inbox", id.String()})
	}
	if len(rows) != spec.Counts.Categorizables {
		t.Fatalf("reader scale fixture: built %d categorizables, want %d", len(rows), spec.Counts.Categorizables)
	}
	copyReaderScale(t, ctx, pool, "reader_categorizables", linkColumns, rows)
}

// ----------------------------------------------------------------- todos

// seedReaderScaleStandaloneTodos seeds only standalone rows. Projected rows are
// deliberately absent: Home and Todos create them on first read, and measuring
// that first-read cost separately from the steady state is exactly what phase 2
// needs.
func seedReaderScaleStandaloneTodos(t testing.TB, ctx context.Context, pool *pgxpool.Pool, spec ReaderScaleSpec) {
	columns := []string{
		"id", "text", "due_at", "done", "origin_kind", "origin_host_kind",
		"origin_host_id", "origin_ref", "host_revision", "completed_at",
		"deleted_at", "created_at", "updated_at",
	}
	rows := make([][]any, 0, spec.Counts.StandaloneTodos)
	for i := range spec.Counts.StandaloneTodos {
		at := readerScaleAt(i)
		done := i%4 == 0
		var completed any
		if done {
			completed = at.Add(time.Hour)
		}
		var due any
		if i%3 == 0 {
			due = at.Add(72 * time.Hour)
		}
		rows = append(rows, []any{
			readerScaleUUID(fmt.Sprintf("todo:%04d", i)),
			fmt.Sprintf("Standalone todo %04d %s", i, readerScaleText(40, fmt.Sprintf("todo-%04d", i))),
			due,
			done,
			"standalone",
			nil,
			nil,
			nil,
			int64(0),
			completed,
			nil,
			at,
			at,
		})
	}
	copyReaderScale(t, ctx, pool, "reader_todos", columns, rows)
}

// ------------------------------------------------------------- verification

// readerScaleDigestTables are the tables covered by the reproducibility digest.
// reader_todos is included because the fixture seeds only standalone rows;
// digesting it therefore also proves no projection leaked into the seed.
var readerScaleDigestTables = []string{
	"links",
	"sites",
	"site_entries",
	"reader_engagement",
	"reader_thoughts",
	"reader_thought_tombstones",
	"reader_notes",
	"reader_inbox",
	"reader_categories",
	"reader_categorizables",
	"reader_todos",
	"feed_folders",
	"feed_subscriptions",
	"feed_items",
}

// ReaderScaleDigests hashes the full text rendering of every seeded row, per
// table. `(t.*)::text` renders the whole tuple including defaults the generator
// did not set, so the digest catches a changed column default or a trigger
// rewrite just as reliably as a changed generator constant.
//
// Three things are pinned so the digest describes the data and nothing else:
//
//   - TimeZone, DateStyle and extra_float_digits, because timestamptz and real
//     render through them. They are set with SET LOCAL inside a transaction so
//     they cannot outlive this function on a pooled connection.
//   - The aggregation order, with COLLATE "C". string_agg's ORDER BY otherwise
//     uses the database collation, which would make the digest depend on the
//     image's locale even though every byte of the data is identical.
func ReaderScaleDigests(t testing.TB, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("reader scale digest: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL TimeZone='UTC'; SET LOCAL DateStyle='ISO, MDY'; SET LOCAL extra_float_digits=1`); err != nil {
		t.Fatalf("reader scale digest: pin rendering: %v", err)
	}

	digests := make(map[string]string, len(readerScaleDigestTables))
	for _, table := range readerScaleDigestTables {
		var digest string
		query := fmt.Sprintf(
			`SELECT COALESCE(md5(string_agg(line, E'\n' ORDER BY line COLLATE "C")),'empty')
			 FROM (SELECT (x.*)::text AS line FROM %s x) s`,
			pgx.Identifier{table}.Sanitize(),
		)
		if err := tx.QueryRow(ctx, query).Scan(&digest); err != nil {
			t.Fatalf("reader scale digest: %s: %v", table, err)
		}
		digests[table] = digest
	}
	return digests
}

// ReaderScaleRowCounts reads the live row count of every digested table.
func ReaderScaleRowCounts(t testing.TB, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	ctx := context.Background()
	counts := make(map[string]int, len(readerScaleDigestTables))
	for _, table := range readerScaleDigestTables {
		var count int
		query := fmt.Sprintf(`SELECT count(*)::int FROM %s`, pgx.Identifier{table}.Sanitize())
		if err := pool.QueryRow(ctx, query).Scan(&count); err != nil {
			t.Fatalf("reader scale count: %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}
