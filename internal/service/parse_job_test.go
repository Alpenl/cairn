package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"webtag/internal/errsafe"
)

// stubParseProcessor records the link it was asked to run and returns a
// preconfigured error (nil = success).
type stubParseProcessor struct {
	ranLinkID uuid.UUID
	ranJobID  uuid.UUID
	err       error
}

func (s *stubParseProcessor) Run(_ context.Context, linkID, jobID uuid.UUID) error {
	s.ranLinkID = linkID
	s.ranJobID = jobID
	return s.err
}

func (s *stubParseProcessor) RecordDiscard(context.Context, uuid.UUID, uuid.UUID, error) error {
	return nil
}

// TestParseLinkArgs_KindAndInsertOpts locks the job contract: the kind string
// (changing it orphans historical jobs) and the unique-by-args + bounded
// MaxAttempts insert options that replace the old in-flight dedupe and bound
// retries while still leaving room for crash rescue.
func TestParseLinkArgs_KindAndInsertOpts(t *testing.T) {
	var args ParseLinkArgs
	if args.Kind() != "parse_link" {
		t.Fatalf("Kind() = %q, want parse_link", args.Kind())
	}
	opts := args.InsertOpts()
	if !opts.UniqueOpts.ByArgs {
		t.Fatal("InsertOpts.UniqueOpts.ByArgs = false, want true (dedupe by link_id args)")
	}
	// ByState must be set explicitly and must NOT include completed: River's
	// default ByState includes completed, which would let a finished parse_link
	// job block re-enqueue (refresh) of the same link until the job cleaner
	// runs — the link stays stuck pending. Guard the in-flight-only contract.
	if len(opts.UniqueOpts.ByState) == 0 {
		t.Fatal("InsertOpts.UniqueOpts.ByState is empty; must be set explicitly to exclude completed")
	}
	for _, s := range opts.UniqueOpts.ByState {
		if s == rivertype.JobStateCompleted {
			t.Fatal("ByState includes completed; a finished job would block refresh re-enqueue")
		}
	}
	// The states River requires to be present must all be there.
	for _, required := range []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	} {
		found := false
		for _, s := range opts.UniqueOpts.ByState {
			if s == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ByState missing required state %q", required)
		}
	}
	if opts.MaxAttempts != parseLinkMaxAttempts {
		t.Fatalf("InsertOpts.MaxAttempts = %d, want %d", opts.MaxAttempts, parseLinkMaxAttempts)
	}
	if parseLinkMaxAttempts < 2 {
		t.Fatalf("parseLinkMaxAttempts = %d, must be >= 2 so the rescuer retries crashed jobs", parseLinkMaxAttempts)
	}
}

// TestParseLinkWorker_WorkSuccess delegates to the processor and returns nil.
func TestParseLinkWorker_WorkSuccess(t *testing.T) {
	linkID := uuid.New()
	jobID := uuid.New()
	proc := &stubParseProcessor{}
	w := NewParseLinkWorker(proc, 0)
	if err := w.Work(context.Background(), &river.Job[ParseLinkArgs]{Args: ParseLinkArgs{
		LinkID:     linkID,
		ParseJobID: jobID,
	}}); err != nil {
		t.Fatalf("Work() = %v, want nil", err)
	}
	if proc.ranLinkID != linkID {
		t.Fatalf("processor link id = %s, want %s", proc.ranLinkID, linkID)
	}
	if proc.ranJobID != jobID {
		t.Fatalf("processor job id = %s, want %s", proc.ranJobID, jobID)
	}
}

// TestParseLinkWorker_WorkAlreadyPersistedReturnsNil is the key retry-decision
// assertion: when the pipeline already persisted a terminal failure (returns a
// *PipelineRunError ≡ errsafe.ErrAlreadyPersisted), Work swallows it (returns
// nil) so River marks the job completed and does NOT retry a deterministically
// failing parse — matching the old in-memory queue's one-shot semantics.
func TestParseLinkWorker_WorkAlreadyPersistedReturnsNil(t *testing.T) {
	proc := &stubParseProcessor{err: &PipelineRunError{Cause: errors.New("fetch boom")}}
	w := NewParseLinkWorker(proc, 0)
	if err := w.Work(context.Background(), &river.Job[ParseLinkArgs]{Args: ParseLinkArgs{
		LinkID: uuid.New(), ParseJobID: uuid.New(),
	}}); err != nil {
		t.Fatalf("Work() = %v, want nil (already-persisted failure ⇒ job done, no retry)", err)
	}
	// Sanity: the stubbed error really is ErrAlreadyPersisted-shaped.
	if !errors.Is(proc.err, errsafe.ErrAlreadyPersisted) {
		t.Fatal("test fixture error is not ErrAlreadyPersisted; assertion would be vacuous")
	}
}

// TestParseLinkWorker_WorkUnexpectedErrorBubbles an unexpected (not
// already-persisted) error bubbles to River so it retries / rescues.
func TestParseLinkWorker_WorkUnexpectedErrorBubbles(t *testing.T) {
	want := errors.New("db connection reset")
	proc := &stubParseProcessor{err: want}
	w := NewParseLinkWorker(proc, 0)
	err := w.Work(context.Background(), &river.Job[ParseLinkArgs]{Args: ParseLinkArgs{
		LinkID: uuid.New(), ParseJobID: uuid.New(),
	}})
	if !errors.Is(err, want) {
		t.Fatalf("Work() = %v, want it to bubble %v (River should retry unexpected errors)", err, want)
	}
}
