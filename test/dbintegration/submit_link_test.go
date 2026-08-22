package dbintegration

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
)

func TestLinkSubmitCreatesOneLinkAndRiverAttempt(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	result, err := dbLinkCommands(pool, links, queue).SubmitLink(t.Context(), service.SubmitLinkCommand{
		Capture: service.LinkCapture{
			URL: "https://submit.example.com/one", SourceKind: "url",
			SourceKey: "submit-one", Status: model.LinkStatusPending,
		},
	})
	if err != nil || result.Link == nil || !result.Enqueued {
		t.Fatalf("SubmitLink() = %+v, %v, want enqueued Link", result, err)
	}
	assertActiveRiverAttempt(t, pool, parseAttemptForLink(result.Link))
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links = %d, want 1", got)
	}
	if got := rawCountParseRiverJobs(t, pool); got != 1 {
		t.Fatalf("River parse jobs = %d, want 1", got)
	}
}

func TestLinkSubmitReusesConflictWithoutNewRiverAttempt(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, links, queue)
	capture := service.LinkCapture{
		URL: "https://submit.example.com/duplicate", SourceKind: "url",
		SourceKey: "submit-duplicate", Status: model.LinkStatusPending,
	}
	first, err := commands.SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: capture})
	if err != nil || first.Link == nil || !first.Enqueued {
		t.Fatalf("initial SubmitLink() = %+v, %v", first, err)
	}
	second, err := commands.SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: capture})
	if err != nil {
		t.Fatalf("conflicting SubmitLink(): %v", err)
	}
	if second.Enqueued || second.Link == nil || second.Link.ID != first.Link.ID {
		t.Fatalf("conflicting SubmitLink() = %+v, want existing Link without enqueue", second)
	}
	if got := rawCountLinks(t, pool); got != 1 {
		t.Fatalf("links after conflict = %d, want 1", got)
	}
	if got := rawCountParseRiverJobs(t, pool); got != 1 {
		t.Fatalf("River parse jobs after conflict = %d, want 1", got)
	}
}

func TestLinkSubmitRestoresTerminalTrashWithoutImplicitReparse(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	reader := repository.NewPGXReaderVNextRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	capture := service.LinkCapture{
		URL: "https://submit.example.com/trashed-terminal", SourceKind: "url",
		SourceKey: "submit-trashed-terminal", Status: model.LinkStatusDone,
	}
	seeded, err := links.Create(t.Context(), repository.CreateLinkParams{
		URL: capture.URL, SourceKind: capture.SourceKind, SourceKey: capture.SourceKey, Status: capture.Status,
	})
	if err != nil {
		t.Fatalf("seed terminal Link: %v", err)
	}
	if _, err := reader.SoftDeleteHost(t.Context(), model.ReaderHostLink, seeded.ID); err != nil {
		t.Fatalf("trash terminal Link: %v", err)
	}

	result, err := dbLinkCommands(pool, links, queue).SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: capture})
	if err != nil || result.Link == nil || result.Link.ID != seeded.ID || result.Enqueued {
		t.Fatalf("SubmitLink(restored terminal) = %+v, %v", result, err)
	}
	assertReaderFeedLinkLive(t, pool, seeded.ID, true)
	if got := rawCountParseRiverJobs(t, pool); got != 0 {
		t.Fatalf("River parse jobs = %d, want 0", got)
	}
}

func TestLinkSubmitRestartsInflightTrashWithReplacementAttempt(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	commands := dbLinkCommands(pool, links, queue)
	capture := service.LinkCapture{
		URL: "https://submit.example.com/trashed-pending", SourceKind: "url",
		SourceKey: "submit-trashed-pending", Status: model.LinkStatusPending,
	}
	seeded, err := commands.SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: capture})
	if err != nil || seeded.Link == nil || !seeded.Enqueued {
		t.Fatalf("seed SubmitLink() = %+v, %v", seeded, err)
	}
	oldAttempt := parseAttemptForLink(seeded.Link)
	if err := commands.DeleteLink(t.Context(), service.DeleteLinkCommand{LinkID: seeded.Link.ID}); err != nil {
		t.Fatalf("trash pending Link: %v", err)
	}

	restored, err := commands.SubmitLink(t.Context(), service.SubmitLinkCommand{Capture: capture})
	if err != nil || restored.Link == nil || restored.Link.ID != seeded.Link.ID || !restored.Enqueued {
		t.Fatalf("SubmitLink(restored pending) = %+v, %v", restored, err)
	}
	current, err := links.GetByID(t.Context(), seeded.Link.ID)
	if err != nil || current == nil {
		t.Fatalf("read restored Link: %#v, %v", current, err)
	}
	newAttempt := parseAttemptForLink(current)
	if newAttempt.Generation != oldAttempt.Generation+1 {
		t.Fatalf("replacement generation = %d, want %d", newAttempt.Generation, oldAttempt.Generation+1)
	}
	if got := countRiverParseJobs(t, pool, newAttempt); got != 1 {
		t.Fatalf("replacement River rows = %d, want 1", got)
	}
	assertReaderFeedLinkLive(t, pool, seeded.Link.ID, true)
}

func TestLinkSubmitRollsBackOnEncodingFailure(t *testing.T) {
	pool := StartPostgres(t)
	links := repository.NewPGXLinkRepository(pool)
	queue := newRiverQueue(t, pool, newRecordingProcessor(pool))
	_, err := dbLinkCommands(pool, links, queue).SubmitLink(t.Context(), service.SubmitLinkCommand{
		Capture: service.LinkCapture{
			URL: "https://submit.example.com/invalid", SourceKind: "url",
			SourceKey: "submit-invalid", Status: model.LinkStatusPending,
			SourceMetadata: map[string]any{"invalid": make(chan struct{})},
		},
	})
	if err == nil {
		t.Fatal("SubmitLink() with non-JSON metadata succeeded")
	}
	if got := rawCountLinks(t, pool); got != 0 {
		t.Fatalf("links after failed submit = %d, want 0", got)
	}
	if got := rawCountParseRiverJobs(t, pool); got != 0 {
		t.Fatalf("River parse rows after failed submit = %d, want 0", got)
	}
}

func rawCountParseRiverJobs(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM river_job WHERE kind='parse_link'`).Scan(&count); err != nil {
		t.Fatalf("count River parse jobs: %v", err)
	}
	return count
}
