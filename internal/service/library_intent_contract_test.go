package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestCaptureEntryPointsUseTheSameRequestedLibraryIntentContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		invoke     func(context.Context, *SubmitService, *IngestService) error
		wantKind   model.RequestedLibraryKind
		wantSource model.RequestedLibraryKindSource
	}{
		{
			name: "single explicit reading",
			invoke: func(ctx context.Context, submit *SubmitService, _ *IngestService) error {
				_, err := submit.Submit(ctx, dto.LinkCreateRequest{
					URL: "https://intent.example/single", RequestedLibraryKind: "reading",
				})
				return err
			},
			wantKind: model.RequestedLibraryKindReading, wantSource: model.RequestedLibraryKindSourceUser,
		},
		{
			name: "batch explicit site",
			invoke: func(ctx context.Context, submit *SubmitService, _ *IngestService) error {
				_, err := submit.Batch(ctx, dto.BatchCreateRequest{Items: []dto.LinkCreateRequest{{
					URL: "https://intent.example/batch", RequestedLibraryKind: "site",
				}}})
				return err
			},
			wantKind: model.RequestedLibraryKindSite, wantSource: model.RequestedLibraryKindSourceUser,
		},
		{
			name: "extension browser capture explicit reading",
			invoke: func(ctx context.Context, _ *SubmitService, ingest *IngestService) error {
				_, err := ingest.Ingest(ctx, dto.IngestRequest{
					RequestedLibraryKind: "reading",
					Sources: []dto.IngestSource{{
						Kind: "browser_capture", URL: "https://intent.example/extension",
						Title: "Captured title", Text: "Captured body",
					}},
				})
				return err
			},
			wantKind: model.RequestedLibraryKindReading, wantSource: model.RequestedLibraryKindSourceUser,
		},
		{
			name: "RSS analysis automatic reading",
			invoke: func(ctx context.Context, _ *SubmitService, ingest *IngestService) error {
				_, err := ingest.AnalyzeRSS(ctx, RSSIngestRequest{
					URL: "https://intent.example/rss", FeedURL: "https://intent.example/feed.xml",
					ExternalID: "item-1", SubscriptionID: uuid.New(), ItemID: uuid.New(),
				})
				return err
			},
			wantKind: model.RequestedLibraryKindReading, wantSource: model.RequestedLibraryKindSourceAuto,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			linkID, jobID := uuid.New(), uuid.New()
			links := &repotest.ObservableLinkStore{
				CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
					return &model.Link{ID: linkID, URL: params.URL, SourceKey: params.SourceKey, Status: params.Status}, nil
				},
			}
			jobs := &repotest.ObservableJobStore{
				CreateFunc: func(_ context.Context, createdLinkID uuid.UUID) (*model.ParseJob, error) {
					return &model.ParseJob{ID: jobID, LinkID: createdLinkID, Status: model.JobStatusPending}, nil
				},
			}
			commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
			submit, ingest := NewLinkServices(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{})

			if err := test.invoke(context.Background(), submit, ingest); err != nil {
				t.Fatalf("entry point error = %v", err)
			}
			if len(links.CreateCalls) != 1 {
				t.Fatalf("Create calls = %d, want 1", len(links.CreateCalls))
			}
			created := links.CreateCalls[0]
			if created.RequestedLibraryKind != test.wantKind || created.RequestedLibraryKindSource != test.wantSource {
				t.Fatalf("requested intent = %s/%s, want %s/%s",
					created.RequestedLibraryKind, created.RequestedLibraryKindSource, test.wantKind, test.wantSource)
			}
		})
	}
}

func TestExplicitIntentOnActiveLinkReusesCurrentParseAttempt(t *testing.T) {
	t.Parallel()

	for _, status := range []model.LinkStatus{model.LinkStatusPending, model.LinkStatusProcessing} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			linkID, jobID := uuid.New(), uuid.New()
			rawURL := "https://active-intent.example/" + string(status)
			link := &model.Link{ID: linkID, URL: rawURL, SourceKey: rawURL, Status: status}
			links := &repotest.ObservableLinkStore{
				ByID:  map[uuid.UUID]*model.Link{linkID: link},
				ByURL: map[string]*model.Link{rawURL: link},
			}
			job := &model.ParseJob{ID: jobID, LinkID: linkID, Status: model.JobStatusProcessing, UpdatedAt: time.Now().UTC()}
			jobs := &repotest.ObservableJobStore{LatestByLinkID: map[uuid.UUID]*model.ParseJob{linkID: job}}
			commands := (&submitFakeSubmitter{links: links, jobs: jobs}).withQueue(&submitFakeQueue{})
			submit := NewSubmitService(links, jobs, commands, &submitFakeLocker{}, SubmitServiceOptions{})

			response, err := submit.Submit(context.Background(), dto.LinkCreateRequest{
				URL: rawURL, RequestedLibraryKind: "site",
			})
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if len(commands.intentUpdates) != 1 {
				t.Fatalf("intent updates = %d, want 1", len(commands.intentUpdates))
			}
			update := commands.intentUpdates[0]
			if update.Kind != model.RequestedLibraryKindSite || update.Source != model.RequestedLibraryKindSourceUser {
				t.Fatalf("intent update = %s/%s, want site/user", update.Kind, update.Source)
			}
			if response.JobID == nil || *response.JobID != jobID.String() {
				t.Fatalf("job id = %v, want reused %s", response.JobID, jobID)
			}
			if len(commands.requeueCaptures) != 0 || len(jobs.CreateCalls) != 0 {
				t.Fatalf("active intent created replacement work: requeues=%d jobs=%d", len(commands.requeueCaptures), len(jobs.CreateCalls))
			}
		})
	}
}
