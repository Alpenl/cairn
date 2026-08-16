package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/observability"
	"webtag/internal/repository"
)

// 两腿检索的每腿返回上限。提成具名常量而非内联字面量，是为了让
// TestServicePageLimitsFitPreallocBound 能读到**本体**——它要断言
// 「service 的上界 ≤ alloc.MaxPrealloc」，而读不到本体的断言是死断言
// （实测：内联时把这里改成 100000，那条断言照样绿）。
const (
	defaultLibrarySearchLimit = 10
	defaultThoughtSearchLimit = 20
	maxLibrarySearchLimit     = 50
)

type LibrarySearchService struct {
	links            librarySearchLinkReader
	sites            repository.SiteSearchStore
	reader           readerSearchStore
	queryEmbedder    RetrievalEmbedder
	metrics          *observability.Metrics
	thoughtCursorKey []byte
}

// LibrarySearchServiceOptions configures process-stable cursor verification.
type LibrarySearchServiceOptions struct {
	CursorSigningKey string
}

type librarySearchLinkReader interface {
	ListDone(context.Context, repository.ListLinksFilter) ([]model.Link, int, error)
}

type readerSearchStore interface {
	SearchThoughts(context.Context, string, string, int) ([]model.ReaderThoughtSearch, int, string, error)
	SearchPublishedNotes(context.Context, string, int) ([]model.ReaderNoteSearch, int, error)
}

// NewLibrarySearchServiceWithMetrics keeps the historic constructor stable
// while allowing production to record a bounded mode-labelled site search
// latency histogram.
func NewLibrarySearchServiceWithMetrics(links librarySearchLinkReader, sites repository.SiteSearchStore, embedder RetrievalEmbedder, metrics *observability.Metrics, readerStores ...readerSearchStore) *LibrarySearchService {
	return newLibrarySearchService(links, sites, embedder, metrics, LibrarySearchServiceOptions{}, readerStores...)
}

// NewLibrarySearchServiceWithMetricsAndOptions constructs the production
// search service with metrics, Reader groups, and explicit cursor settings.
func NewLibrarySearchServiceWithMetricsAndOptions(links librarySearchLinkReader, sites repository.SiteSearchStore, embedder RetrievalEmbedder, metrics *observability.Metrics, options LibrarySearchServiceOptions, readerStores ...readerSearchStore) *LibrarySearchService {
	return newLibrarySearchService(links, sites, embedder, metrics, options, readerStores...)
}

func newLibrarySearchService(links librarySearchLinkReader, sites repository.SiteSearchStore, embedder RetrievalEmbedder, metrics *observability.Metrics, options LibrarySearchServiceOptions, readerStores ...readerSearchStore) *LibrarySearchService {
	var reader readerSearchStore
	if len(readerStores) > 0 {
		reader = readerStores[0]
	}
	cursorKey := processReaderCursorKey
	if options.CursorSigningKey != "" {
		cursorKey = []byte(options.CursorSigningKey)
	}
	return &LibrarySearchService{
		links: links, sites: sites, reader: reader, queryEmbedder: embedder, metrics: metrics,
		thoughtCursorKey: append([]byte(nil), cursorKey...),
	}
}

func (s *LibrarySearchService) Search(ctx context.Context, raw string, readingLimit, siteLimit, thoughtLimit int, thoughtAfter string) (dto.GroupedSearchResponse, error) { //nolint:gocyclo // 混合检索：语义腿 + 关键词腿 + 分组装配，各腿有独立降级路径
	query := strings.TrimSpace(raw)
	if query == "" {
		if strings.TrimSpace(thoughtAfter) != "" {
			return dto.GroupedSearchResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidCursor, "invalid thought cursor")
		}
		return dto.GroupedSearchResponse{Reading: dto.LibrarySearchGroup{Items: []dto.LinkResponse{}}, Sites: dto.SiteSearchGroup{Items: []dto.SiteSearchResultResponse{}}}, nil
	}
	if len([]rune(query)) > maxListQueryLen {
		return dto.GroupedSearchResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeQueryTooLong, "search query is too long")
	}
	if readingLimit < 1 {
		readingLimit = defaultLibrarySearchLimit
	}
	if readingLimit > maxLibrarySearchLimit {
		readingLimit = maxLibrarySearchLimit
	}
	if siteLimit < 1 {
		siteLimit = defaultLibrarySearchLimit
	}
	if siteLimit > maxLibrarySearchLimit {
		siteLimit = maxLibrarySearchLimit
	}
	if thoughtLimit < 1 {
		thoughtLimit = defaultThoughtSearchLimit
	}
	if thoughtLimit > maxLibrarySearchLimit {
		thoughtLimit = maxLibrarySearchLimit
	}
	repositoryThoughtAfter, err := s.decodeThoughtSearchCursor(ctx, query, thoughtAfter)
	if err != nil {
		return dto.GroupedSearchResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidCursor, "invalid thought cursor")
	}
	kind := model.LibraryKindReading
	filter := repository.ListLinksFilter{Query: &query, LibraryKind: &kind, Limit: readingLimit}
	var vector []float32
	var modelName string
	if s.queryEmbedder != nil && s.queryEmbedder.Enabled() {
		if vectors, embedErr := s.queryEmbedder.Embed(ctx, []string{query}); embedErr == nil && len(vectors) == 1 && len(vectors[0]) > 0 {
			vector, modelName = vectors[0], s.queryEmbedder.Model()
			filter.QueryEmbedding, filter.EmbeddingModel = vector, modelName
		}
	}
	links, linkTotal, err := s.links.ListDone(ctx, filter)
	if err != nil {
		return dto.GroupedSearchResponse{}, err
	}
	var siteMatches []repository.SiteSearchMatch
	var siteTotal int
	searchMode := "keyword"
	started := time.Now()
	if semantic, ok := s.sites.(repository.SiteSemanticSearchStore); ok && len(vector) > 0 && strings.TrimSpace(modelName) != "" {
		searchMode = "semantic"
		siteMatches, siteTotal, err = semantic.SearchSitesSemantic(ctx, query, vector, modelName, siteLimit)
	} else {
		siteMatches, siteTotal, err = s.sites.SearchSites(ctx, query, siteLimit)
	}
	if s.metrics != nil && s.metrics.SiteSearchDuration != nil {
		s.metrics.SiteSearchDuration.WithLabelValues(searchMode).Observe(time.Since(started).Seconds())
	}
	if err != nil {
		return dto.GroupedSearchResponse{}, err
	}
	out := dto.GroupedSearchResponse{Reading: dto.LibrarySearchGroup{Items: make([]dto.LinkResponse, 0, len(links)), TotalHint: linkTotal}, Sites: dto.SiteSearchGroup{Items: make([]dto.SiteSearchResultResponse, 0, len(siteMatches)), TotalHint: siteTotal}}
	for _, link := range links {
		out.Reading.Items = append(out.Reading.Items, linkToResponse(link))
	}
	for _, match := range siteMatches {
		item := dto.SiteSearchResultResponse{ID: match.SiteID.String(), Name: match.SiteName, MatchedEntries: make([]dto.SiteSearchEntryResponse, 0, len(match.MatchedEntries))}
		for _, entry := range match.MatchedEntries {
			item.MatchedEntries = append(item.MatchedEntries, dto.SiteSearchEntryResponse{ID: entry.ID.String(), Name: entry.Name, URL: entry.URL})
		}
		out.Sites.Items = append(out.Sites.Items, item)
	}
	if s.reader != nil {
		thoughts, thoughtTotal, thoughtNextCursor, err := s.reader.SearchThoughts(ctx, query, repositoryThoughtAfter, thoughtLimit)
		if err != nil {
			if errors.Is(err, repository.ErrInvalidReaderCursor) {
				return dto.GroupedSearchResponse{}, httperr.NewWithCode(http.StatusUnprocessableEntity, httperr.CodeInvalidCursor, "invalid thought cursor")
			}
			return dto.GroupedSearchResponse{}, err
		}
		notes, noteTotal, err := s.reader.SearchPublishedNotes(ctx, query, readingLimit)
		if err != nil {
			return dto.GroupedSearchResponse{}, err
		}
		thoughtNextCursor, err = s.encodeThoughtSearchCursor(ctx, query, thoughtNextCursor)
		if err != nil {
			return dto.GroupedSearchResponse{}, err
		}
		out.Thoughts = &dto.ReaderThoughtSearchGroup{Items: make([]dto.ReaderThoughtSearchResponse, 0, len(thoughts)), TotalHint: thoughtTotal, NextCursor: thoughtNextCursor}
		for _, thought := range thoughts {
			item := dto.ReaderThoughtSearchResponse{ID: thought.ID, HostKind: thought.HostKind, HostID: thought.HostID, Snippet: thought.Snippet, UpdatedAt: thought.UpdatedAt, LifecycleStatus: thought.LifecycleStatus, LifecycleReason: thought.LifecycleReason, HistoryDeepLink: thought.HistoryDeepLink}
			if thought.LinkID != nil {
				linkID := thought.LinkID.String()
				item.LinkID = &linkID
			}
			out.Thoughts.Items = append(out.Thoughts.Items, item)
		}
		out.Notes = &dto.ReaderNoteSearchGroup{Items: make([]dto.ReaderNoteSearchResponse, 0, len(notes)), TotalHint: noteTotal}
		for _, note := range notes {
			out.Notes.Items = append(out.Notes.Items, dto.ReaderNoteSearchResponse{ID: note.ID.String(), Title: note.Title, Snippet: note.Snippet, PublishedRevision: note.PublishedRevision, UpdatedAt: note.UpdatedAt})
		}
	}
	return out, nil
}
