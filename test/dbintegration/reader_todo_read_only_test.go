package dbintegration

import (
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"webtag/internal/model"
	"webtag/internal/repository"
)

// readReaderTodoWriteFingerprint captures the volatile columns a projection
// write touches. Home and Todos used to bump updated_at on every GET, so this
// is the cheapest evidence that they no longer write at all.
func readReaderTodoWriteFingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var fingerprint string
	if err := pool.QueryRow(t.Context(), `
		SELECT COALESCE(md5(string_agg(
			id::text||':'||text||':'||done::text||':'||host_revision::text||':'||
			COALESCE(deleted_at::text,'-')||':'||updated_at::text||':'||origin_ref::text,
			'|' ORDER BY id)),'empty')
		FROM reader_todos`).Scan(&fingerprint); err != nil {
		t.Fatalf("read TODO write fingerprint: %v", err)
	}
	return fingerprint
}

// TestReaderHomeAndTodosAreReadOnlyUnderConcurrency is the acceptance check
// for the read-path move: many Home and Todos reads running at once must all
// succeed, must not report a serialization failure, and must leave every
// projection row byte-identical — including updated_at, which the old
// read-time reconcile bumped on each request.
func TestReaderHomeAndTodosAreReadOnlyUnderConcurrency(t *testing.T) {
	pool := StartPostgres(t)
	ctx := t.Context()
	reader := repository.NewPGXReaderVNextRepository(pool)
	service := postgresReaderApplications(reader).Todos

	if _, err := reader.CreateNote(ctx, model.ReaderNote{
		Title:            "Concurrent read host",
		PublishedContent: "- [ ] first\ncontext\n- [x] second\ncontext\n- [ ] third",
	}); err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	linkID := seedReaderVNextSavedLink(t, pool, "https://todo.example/"+uuid.NewString(), "Concurrent link", "body", "summary")
	appendThoughtWithBody(t, ctx, reader, "thought-todo-"+uuid.NewString(), linkID, 1, "- [ ] fourth")

	before := readReaderTodoWriteFingerprint(t, pool)
	if before == "empty" {
		t.Fatal("write-time maintenance produced no projections to read")
	}

	const readers = 8
	const rounds = 4
	failures := make(chan error, readers*rounds*2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for worker := range readers {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			<-start
			for range rounds {
				if worker%2 == 0 {
					aggregate, err := reader.LoadHomeAggregate(ctx)
					if err != nil {
						failures <- err
						continue
					}
					if len(aggregate.Todos) != 4 {
						failures <- errReaderTodoCount(len(aggregate.Todos))
					}
					continue
				}
				page, err := service.ListTodos(ctx, "", 200)
				if err != nil {
					failures <- err
					continue
				}
				if len(page.Items) != 4 {
					failures <- errReaderTodoCount(len(page.Items))
				}
			}
		}(worker)
	}
	close(start)
	wait.Wait()
	close(failures)

	for err := range failures {
		if strings.Contains(err.Error(), "40001") || strings.Contains(strings.ToLower(err.Error()), "serializ") {
			t.Fatalf("concurrent read hit a serialization failure: %v", err)
		}
		t.Fatalf("concurrent read failed: %v", err)
	}

	if after := readReaderTodoWriteFingerprint(t, pool); after != before {
		t.Fatalf("concurrent Home/Todos reads wrote to reader_todos: %s -> %s", before, after)
	}
}

type readerTodoCountError int

func (e readerTodoCountError) Error() string {
	return "concurrent read returned an unexpected TODO count"
}

func errReaderTodoCount(count int) error { return readerTodoCountError(count) }
