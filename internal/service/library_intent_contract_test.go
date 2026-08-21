package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/repository/repotest"
)

func TestCaptureEntryPointsUseTheSameRequestedLibraryIntentContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		invoke           func(context.Context, *SubmitService, *IngestService) error
		wantKind         model.RequestedLibraryKind
		wantUserSelected bool
	}{
		{
			name: "single explicit site",
			invoke: func(ctx context.Context, submit *SubmitService, _ *IngestService) error {
				_, err := submit.Submit(ctx, dto.LinkCreateRequest{
					URL: "https://intent.example/single", Destination: captureDestinationLibrary, RequestedLibraryKind: "site",
				})
				return err
			},
			wantKind: model.RequestedLibraryKindSite, wantUserSelected: true,
		},
		{
			name: "extension browser capture explicit reading",
			invoke: func(ctx context.Context, _ *SubmitService, ingest *IngestService) error {
				_, err := ingest.Ingest(ctx, dto.IngestRequest{
					Destination:          captureDestinationLibrary,
					RequestedLibraryKind: "reading",
					Sources: []dto.IngestSource{{
						Kind: "browser_capture", URL: "https://intent.example/extension",
						Title: "Captured title", Text: "Captured body",
					}},
				})
				return err
			},
			wantKind: model.RequestedLibraryKindReading, wantUserSelected: true,
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
			wantKind: model.RequestedLibraryKindReading, wantUserSelected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			linkID := uuid.New()
			links := &repotest.ObservableLinkStore{
				CreateFunc: func(_ context.Context, params repository.CreateLinkParams) (*model.Link, error) {
					return &model.Link{ID: linkID, URL: params.URL, SourceKey: params.SourceKey, Status: params.Status}, nil
				},
			}
			commands := (&submitFakeSubmitter{links: links}).withQueue(&submitFakeQueue{})
			submit, ingest := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{})

			if err := test.invoke(context.Background(), submit, ingest); err != nil {
				t.Fatalf("entry point error = %v", err)
			}
			if len(links.CreateCalls) != 1 {
				t.Fatalf("Create calls = %d, want 1", len(links.CreateCalls))
			}
			created := links.CreateCalls[0]
			if created.RequestedLibraryKind != test.wantKind || created.UserSelectedLibraryKind != test.wantUserSelected {
				t.Fatalf("library request = %s user-selected=%v, want %s/%v",
					created.RequestedLibraryKind, created.UserSelectedLibraryKind, test.wantKind, test.wantUserSelected)
			}
		})
	}
}

func TestExplicitIntentOnActiveLinkKeepsCurrentWork(t *testing.T) {
	t.Parallel()

	for _, status := range []model.LinkStatus{model.LinkStatusPending, model.LinkStatusProcessing} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			linkID := uuid.New()
			rawURL := "https://active-intent.example/" + string(status)
			link := &model.Link{ID: linkID, URL: rawURL, SourceKey: rawURL, Status: status}
			links := &repotest.ObservableLinkStore{
				ByID:  map[uuid.UUID]*model.Link{linkID: link},
				ByURL: map[string]*model.Link{rawURL: link},
			}
			queue := &submitFakeQueue{}
			commands := (&submitFakeSubmitter{links: links}).withQueue(queue)
			submit, _ := NewLinkServices(links, commands, &submitFakeLocker{}, SubmitServiceOptions{})

			response, err := submit.Submit(context.Background(), dto.LinkCreateRequest{
				URL: rawURL, Destination: captureDestinationLibrary, RequestedLibraryKind: "site",
			})
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if len(commands.libraryKindUpdates) != 1 {
				t.Fatalf("library kind updates = %d, want 1", len(commands.libraryKindUpdates))
			}
			update := commands.libraryKindUpdates[0]
			if update.Kind != model.LibraryKindSite || !update.Override {
				t.Fatalf("library kind update = %s override=%v, want site/true", update.Kind, update.Override)
			}
			if response.LinkID != linkID.String() || response.Status != string(status) {
				t.Fatalf("response = %#v, want active link %s in %s", response, linkID, status)
			}
			if len(commands.requeueCaptures) != 0 || len(queue.ids) != 0 {
				t.Fatalf("active intent created replacement work: requeues=%d queued=%d", len(commands.requeueCaptures), len(queue.ids))
			}
		})
	}
}
