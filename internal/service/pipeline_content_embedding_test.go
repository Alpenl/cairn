package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/errsafe"
	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/observability"
	analyzerpkg "webtag/internal/service/analyzer"
)

// recordingEmbedder captures the embed inputs so the content-vector input
// shape (title + summary + body) can be asserted. enabled/err drive the
// on/failure branches.
type recordingEmbedder struct {
	enabled bool
	err     error
	inputs  []string
	vec     []float32
}

func (e *recordingEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	e.inputs = append(e.inputs, inputs...)
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = e.vec
	}
	return out, nil
}
func (e *recordingEmbedder) Model() string { return "content-model" }
func (e *recordingEmbedder) Enabled() bool { return e.enabled }

// recordingLinkEmbeddingWriter captures UpdateLinkEmbedding calls.
type recordingLinkEmbeddingWriter struct {
	calls []linkEmbeddingCall
	err   error
}

type linkEmbeddingCall struct {
	id              uuid.UUID
	parseJobID      uuid.UUID
	expectedTitle   *string
	expectedSummary *string
	vec             []float32
	model           string
}

func (w *recordingLinkEmbeddingWriter) UpdateLinkEmbeddingForParse(_ context.Context, id, parseJobID uuid.UUID, expectedTitle, expectedSummary *string, vec []float32, model string) error {
	w.calls = append(w.calls, linkEmbeddingCall{id: id, parseJobID: parseJobID, expectedTitle: expectedTitle, expectedSummary: expectedSummary, vec: vec, model: model})
	return w.err
}

// newContentEmbeddingPipeline wires a minimal success-path pipeline with the
// supplied embedder + writer, returning the link id to Run.
func newContentEmbeddingPipeline(t *testing.T, embedder RetrievalEmbedder, writer LinkEmbeddingWriter) (*ParsePipeline, uuid.UUID, uuid.UUID) {
	t.Helper()
	linkID := uuid.New()
	jobID := uuid.New()
	now := time.Now().UTC()

	linkStore := newPipelineLinkStore(map[uuid.UUID]*model.Link{
		linkID: {ID: linkID, URL: "https://example.com/articles/p9", Status: model.LinkStatusPending, CreatedAt: now, UpdatedAt: now},
	})
	jobStore := newPipelineJobStore(map[uuid.UUID]*model.ParseJob{
		linkID: {ID: jobID, LinkID: linkID, Status: model.JobStatusPending, ExpectedMetadataRevision: 1, CreatedAt: now, UpdatedAt: now},
	})
	fetch := pipelineFetcherFunc(func(context.Context, string) (fetcher.Content, error) {
		return fetcher.Content{URL: "https://example.com/articles/p9", Title: "Vector DBs", Body: "pgvector body text", FetcherType: "basic"}, nil
	})
	analyze := pipelineAnalyzerFunc(func(context.Context, analyzerpkg.AnalyzeRequest) (analyzerpkg.AnalysisResult, error) {
		return analyzerpkg.AnalysisResult{Summary: "a summary about vectors", Tags: []string{"db"}}, nil
	})
	// The link sits under example.com so ensureParent looks for an existing
	// real ancestor; supply a homepage lookup so parent resolution succeeds.
	treeStore := newPipelineTreeStore(
		map[string]*model.Link{
			"https://example.com/": {ID: uuid.New(), URL: "https://example.com/", Status: model.LinkStatusDone, CreatedAt: now, UpdatedAt: now},
		},
	)

	pipeline := NewParsePipeline(ParsePipelineOptions{
		Links:               linkStore,
		ReadingCompleter:    linkStore,
		SiteCompleter:       linkStore,
		Jobs:                jobStore,
		Tags:                &pipelineFakeTagStore{},
		Tree:                treeStore,
		Fetcher:             fetch,
		Analyzer:            analyze,
		Metrics:             observability.NewMetrics(),
		Embedder:            embedder,
		LinkEmbeddingWriter: writer,
	})
	return pipeline, linkID, jobID
}

// TestPipelineWritesContentVector: an enabled embedder + wired writer persists
// the content vector with the current model, and the embed input folds title +
// summary + body.
func TestPipelineWritesContentVector(t *testing.T) {
	t.Parallel()
	embedder := &recordingEmbedder{enabled: true, vec: []float32{0.5, 0.5}}
	writer := &recordingLinkEmbeddingWriter{}
	pipeline, linkID, jobID := newContentEmbeddingPipeline(t, embedder, writer)

	runPipelineExpectPersisted(t, pipeline, linkID, jobID)

	if len(writer.calls) != 1 {
		t.Fatalf("UpdateLinkEmbedding calls = %d, want 1", len(writer.calls))
	}
	call := writer.calls[0]
	if call.id != linkID || call.parseJobID != jobID || call.model != "content-model" || len(call.vec) == 0 {
		t.Fatalf("unexpected embedding write: %+v", call)
	}
	if call.expectedTitle == nil || *call.expectedTitle != "Vector DBs" {
		t.Fatalf("embedding expected title = %v, want non-nil %q", call.expectedTitle, "Vector DBs")
	}
	if call.expectedSummary == nil || *call.expectedSummary != "a summary about vectors" {
		t.Fatalf("embedding expected summary = %v, want non-nil %q", call.expectedSummary, "a summary about vectors")
	}
	if len(embedder.inputs) != 1 {
		t.Fatalf("embed inputs = %d, want 1", len(embedder.inputs))
	}
	in := embedder.inputs[0]
	for _, want := range []string{"Vector DBs", "a summary about vectors", "pgvector body text"} {
		if !strings.Contains(in, want) {
			t.Fatalf("content embed input missing %q; got %q", want, in)
		}
	}
}

// TestPipelineContentVectorFailSoftOnEmbedError: an embedding error does NOT
// fail the parse (link still reaches done) and writes no vector.
func TestPipelineContentVectorFailSoftOnEmbedError(t *testing.T) {
	t.Parallel()
	embedder := &recordingEmbedder{enabled: true, err: errors.New("embed upstream down")}
	writer := &recordingLinkEmbeddingWriter{}
	pipeline, linkID, jobID := newContentEmbeddingPipeline(t, embedder, writer)

	runPipelineExpectPersisted(t, pipeline, linkID, jobID)

	if len(writer.calls) != 0 {
		t.Fatalf("embedding write should be skipped on embed failure; got %d calls", len(writer.calls))
	}
}

// TestPipelineContentVectorFailSoftOnWriteError: a write error is swallowed —
// the parse still succeeds (no error surfaced beyond ErrAlreadyPersisted).
func TestPipelineContentVectorFailSoftOnWriteError(t *testing.T) {
	t.Parallel()
	embedder := &recordingEmbedder{enabled: true, vec: []float32{1}}
	writer := &recordingLinkEmbeddingWriter{err: errors.New("write failed")}
	pipeline, linkID, jobID := newContentEmbeddingPipeline(t, embedder, writer)

	runPipelineExpectPersisted(t, pipeline, linkID, jobID)

	if len(writer.calls) != 1 {
		t.Fatalf("write should be attempted once even though it errors; got %d", len(writer.calls))
	}
}

// TestPipelineSkipsContentVectorWhenEmbedderDisabled: a disabled embedder makes
// the content-vector write a no-op (no embed call, no write).
func TestPipelineSkipsContentVectorWhenEmbedderDisabled(t *testing.T) {
	t.Parallel()
	embedder := &recordingEmbedder{enabled: false}
	writer := &recordingLinkEmbeddingWriter{}
	pipeline, linkID, jobID := newContentEmbeddingPipeline(t, embedder, writer)

	runPipelineExpectPersisted(t, pipeline, linkID, jobID)

	if len(embedder.inputs) != 0 || len(writer.calls) != 0 {
		t.Fatalf("disabled embedder must skip content vector: inputs=%d writes=%d", len(embedder.inputs), len(writer.calls))
	}
}

// runPipelineExpectPersisted runs the pipeline and asserts the parse
// completed: the success path returns nil, while a failure that the pipeline
// already persisted returns ErrAlreadyPersisted. Any other error fails the
// test — the content-vector write must never turn a success into a failure.
func runPipelineExpectPersisted(t *testing.T, pipeline *ParsePipeline, linkID, jobID uuid.UUID) {
	t.Helper()
	err := pipeline.Run(context.Background(), linkID, jobID)
	if err != nil && !errors.Is(err, errsafe.ErrAlreadyPersisted) {
		t.Fatalf("Run() error = %v, want nil or ErrAlreadyPersisted", err)
	}
}
