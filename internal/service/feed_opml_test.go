package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
)

func TestImportOPMLPreservesRootUngroupedAndNestedFolder(t *testing.T) {
	t.Parallel()
	type imported struct {
		url       string
		folderID  *uuid.UUID
		setFolder bool
	}
	imports := make([]imported, 0, 2)
	folderID := uuid.New()
	store := &feedStoreStub{
		createFolderFn: func(_ context.Context, name string) (model.FeedFolder, error) {
			if name != "Engineering" {
				t.Fatalf("folder name = %q", name)
			}
			return model.FeedFolder{ID: folderID, Name: name}, nil
		},
		createSubscriptionFn: func(_ context.Context, rawURL string, gotFolderID *uuid.UUID, setFolder bool, _ string) (model.FeedSubscription, error) {
			imports = append(imports, imported{url: rawURL, folderID: gotFolderID, setFolder: setFolder})
			return model.FeedSubscription{URL: rawURL}, nil
		},
	}
	locker := &recordingFeedLocker{}
	service := NewFeedService(FeedServiceOptions{Store: store, Locker: locker})
	payload := []byte(`<?xml version="1.0"?><opml version="2.0"><body>
		<outline text="Root" type="rss" xmlUrl="https://example.com/root.xml"/>
		<outline text="Engineering"><outline text="Nested" type="rss" xmlUrl="https://example.com/nested.xml"/></outline>
	</body></opml>`)
	response, err := service.ImportOPML(context.Background(), payload)
	if err != nil {
		t.Fatalf("ImportOPML() error = %v", err)
	}
	if response.Imported != 2 || len(imports) != 2 {
		t.Fatalf("response=%#v imports=%#v", response, imports)
	}
	if !imports[0].setFolder || imports[0].folderID != nil {
		t.Fatalf("root subscription did not explicitly become ungrouped: %#v", imports[0])
	}
	if !imports[1].setFolder || imports[1].folderID == nil || *imports[1].folderID != folderID {
		t.Fatalf("nested subscription folder = %#v", imports[1])
	}
	if locker.batchCalls != 1 || len(locker.keys) != 3 {
		t.Fatalf("OPML mutations did not all use lifecycle locker: %#v", locker.keys)
	}
}

func TestImportOPMLRejectsExcessiveOutlineDepth(t *testing.T) {
	t.Parallel()
	var payload strings.Builder
	payload.WriteString(`<opml version="2.0"><body>`)
	for index := 0; index < maxOPMLDepth+1; index++ {
		payload.WriteString(`<outline text="folder">`)
	}
	payload.WriteString(`<outline text="feed" xmlUrl="https://example.com/feed.xml"/>`)
	for index := 0; index < maxOPMLDepth+1; index++ {
		payload.WriteString(`</outline>`)
	}
	payload.WriteString(`</body></opml>`)
	service := NewFeedService(FeedServiceOptions{Store: &feedStoreStub{}})
	if _, err := service.ImportOPML(context.Background(), []byte(payload.String())); err == nil {
		t.Fatal("ImportOPML() error = nil")
	}
}

func TestImportOPMLDeduplicatesCanonicalURLVariants(t *testing.T) {
	t.Parallel()
	created := make([]string, 0, 1)
	store := &feedStoreStub{
		createSubscriptionFn: func(_ context.Context, rawURL string, _ *uuid.UUID, _ bool, _ string) (model.FeedSubscription, error) {
			created = append(created, rawURL)
			return model.FeedSubscription{URL: rawURL}, nil
		},
	}
	service := NewFeedService(FeedServiceOptions{Store: store})
	payload := []byte(`<?xml version="1.0"?><opml version="2.0"><body>
		<outline text="Primary" type="rss" xmlUrl="https://EXAMPLE.com:443/feed.xml#latest"/>
		<outline text="Duplicate" type="rss" xmlUrl="https://example.com/feed.xml"/>
	</body></opml>`)

	response, err := service.ImportOPML(context.Background(), payload)
	if err != nil {
		t.Fatalf("ImportOPML() error = %v", err)
	}
	if response.Imported != 1 || response.Skipped != 1 {
		t.Fatalf("response = %#v, want one import and one skipped duplicate", response)
	}
	if len(created) != 1 || created[0] != "https://example.com/feed.xml" {
		t.Fatalf("created URLs = %#v", created)
	}
}
