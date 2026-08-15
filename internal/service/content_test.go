package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/contentdoc"
	"webtag/internal/dto"
	"webtag/internal/fetcher"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type fakeContentStore struct {
	link         *model.Link
	content      *model.SavedContent
	saved        string
	savedContent model.SavedContent
	savedID      uuid.UUID
	revision     int64
	editMiss     bool
	editCalls    int
}

func (f *fakeContentStore) GetParseInputByID(_ context.Context, _ uuid.UUID) (*repository.LinkParseInput, error) {
	if f.link == nil {
		return nil, nil
	}
	projection := contentParseInput(f.link)
	return &projection, nil
}

func contentParseInput(link *model.Link) repository.LinkParseInput {
	return repository.LinkParseInput{
		ID: link.ID, URL: link.URL, SourceKind: link.SourceKind, SourceKey: link.SourceKey,
		InputTitle: link.InputTitle, InputText: link.InputText, InputHTML: link.InputHTML,
		InputImages: link.InputImages, SourceMetadata: link.SourceMetadata,
		Description: link.Description, Status: link.Status, LibraryKind: link.LibraryKind,
		LibraryKindLocked: link.LibraryKindLocked, ContentRevision: link.ContentRevision,
		UpdatedAt: link.UpdatedAt,
	}
}
func (f *fakeContentStore) UpdateContentIfCurrent(_ context.Context, id uuid.UUID, expectedUpdatedAt time.Time, content model.SavedContent) (int64, bool, error) {
	if f.link == nil || f.link.Status != model.LinkStatusDone || !f.link.UpdatedAt.Equal(expectedUpdatedAt) || f.content != nil {
		return 0, false, nil
	}
	return f.record(id, content), true, nil
}
func (f *fakeContentStore) ReplaceContentIfCurrent(_ context.Context, id uuid.UUID, expectedUpdatedAt time.Time, content model.SavedContent) (int64, bool, error) {
	if f.link == nil || f.link.Status != model.LinkStatusDone || !f.link.UpdatedAt.Equal(expectedUpdatedAt) {
		return 0, false, nil
	}
	return f.record(id, content), true, nil
}

func (f *fakeContentStore) EditContentIfRevision(_ context.Context, id uuid.UUID, expectedRevision int64, content model.SavedContent) (int64, bool, error) {
	f.editCalls++
	if f.editMiss || f.link == nil || f.link.Status != model.LinkStatusDone || f.content == nil {
		return 0, false, nil
	}
	currentRevision := f.content.Revision
	if currentRevision <= 0 {
		currentRevision = f.link.ContentRevision
	}
	if currentRevision != expectedRevision {
		return 0, false, nil
	}
	return f.record(id, content), true, nil
}

// record 复刻真实仓储的两条不变量：写入自增 content_revision，且后续 GetContent
// 读到的正文带着同一个代次。fake 不模拟自增，服务层「响应必须带新代次」这条
// 断言就只能测到 0，等于没测。
func (f *fakeContentStore) record(id uuid.UUID, content model.SavedContent) int64 {
	baseRevision := f.revision
	if f.content != nil && f.content.Revision > baseRevision {
		baseRevision = f.content.Revision
	}
	if f.link != nil && f.link.ContentRevision > baseRevision {
		baseRevision = f.link.ContentRevision
	}
	f.revision = baseRevision + 1
	f.savedID = id
	f.saved = content.Text
	content.Revision = f.revision
	f.savedContent = content
	f.content = &f.savedContent
	if f.link != nil {
		f.link.ContentRevision = f.revision
	}
	return f.revision
}
func (f *fakeContentStore) GetContent(_ context.Context, _ uuid.UUID) (*model.SavedContent, error) {
	return f.content, nil
}

type fakeContentFetcher struct {
	content fetcher.Content
	err     error
	onFetch func()
}

func (f fakeContentFetcher) Fetch(_ context.Context, _ string) (fetcher.Content, error) {
	if f.onFetch != nil {
		f.onFetch()
	}
	return f.content, f.err
}

func contentDoneLink() *model.Link {
	return &model.Link{ID: uuid.New(), URL: "https://example.com/a", Status: model.LinkStatusDone}
}

func stringPointerForContentTest(value string) *string {
	return &value
}

func TestContentGetReturnsSavedSnapshotWithoutFetching(t *testing.T) {
	t.Parallel()
	existing := "already saved"
	existingContent := contentdoc.Plain(existing)
	store := &fakeContentStore{link: contentDoneLink(), content: &existingContent}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("Get 只读已保存原文，绝不能触发抓取")
	}}, nil)

	resp, err := svc.Get(context.Background(), store.link.ID.String())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if resp.Content != existing || resp.FetcherType != "stored" {
		t.Fatalf("Get() = %#v，want 已保存正文 + stored", resp)
	}
	if store.saved != "" {
		t.Fatalf("Get() 写了库：%q，它必须是纯读", store.saved)
	}
}

func TestContentGetReturns404WhenNothingSaved(t *testing.T) {
	t.Parallel()
	store := &fakeContentStore{link: contentDoneLink()}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("没有已保存原文时也不该抓取——那是 Save 的职责，不是 Get 的")
	}}, nil)

	_, err := svc.Get(context.Background(), store.link.ID.String())

	assertHTTPCode(t, err, httperr.CodeLinkNotFound)
}

func TestContentSaveReturnsExistingContentWithoutFetching(t *testing.T) {
	t.Parallel()
	existing := "already saved"
	existingContent := contentdoc.Plain(existing)
	link := contentDoneLink()
	link.Status = model.LinkStatusPending
	store := &fakeContentStore{link: link, content: &existingContent}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("fetcher must not run when content is already saved")
	}}, nil)

	resp, err := svc.Save(context.Background(), store.link.ID.String())
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if resp.Content != existing {
		t.Fatalf("content = %q, want existing content", resp.Content)
	}
	if resp.FetcherType != "stored" {
		t.Fatalf("fetcher_type = %q, want stored", resp.FetcherType)
	}
	if resp.ContentFormat != "plain" || resp.ContentDocument != nil {
		t.Fatalf("stored response format/document = %q/%#v, want plain/nil", resp.ContentFormat, resp.ContentDocument)
	}
	if store.saved != "" {
		t.Fatalf("UpdateContent() stored %q, want no write", store.saved)
	}
}

func TestContentSavePromotesBrowserCaptureTextWithoutFetching(t *testing.T) {
	t.Parallel()
	captured := "  captured readable text  "
	link := contentDoneLink()
	link.SourceKind = "browser_capture"
	link.InputText = &captured
	store := &fakeContentStore{link: link}
	svc := NewContentService(store, nil, nil)

	resp, err := svc.Save(context.Background(), link.ID.String())
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if resp.Content != "captured readable text" || store.saved != "captured readable text" {
		t.Fatalf("response/stored content = %q/%q, want captured readable text", resp.Content, store.saved)
	}
	if resp.FetcherType != "browser_capture" {
		t.Fatalf("fetcher_type = %q, want browser_capture", resp.FetcherType)
	}
}

func TestContentSavePreservesStructuredBrowserCaptureHTML(t *testing.T) {
	t.Parallel()
	plain := "Guide First paragraph One Two fmt.Println(\"ok\") Docs"
	html := `<article>
		<h1>Guide</h1>
		<p>First paragraph</p>
		<ul><li>One</li><li>Two</li></ul>
		<pre><code>fmt.Println(&quot;ok&quot;)</code></pre>
		<p><a href="https://example.com/docs">Docs</a></p>
		<script>alert("must not survive")</script>
		<a href="javascript:alert('x')">unsafe link</a>
	</article>`
	link := contentDoneLink()
	link.SourceKind = "browser_capture"
	link.InputText = &plain
	link.InputHTML = &html
	store := &fakeContentStore{link: link}
	svc := NewContentService(store, nil, nil)

	resp, err := svc.Save(context.Background(), link.ID.String())
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if !strings.Contains(resp.Content, "Guide\n") || !strings.Contains(resp.Content, "\nOne\n") {
		t.Fatalf("content = %q, want paragraph/list boundaries from captured HTML", resp.Content)
	}
	if resp.ContentFormat != "markdown" || resp.ContentDocument == nil {
		t.Fatalf("response format/document = %q/%#v, want markdown document", resp.ContentFormat, resp.ContentDocument)
	}
	for _, want := range []string{"# Guide", "- One", "- Two", "```", "[Docs](https://example.com/docs)"} {
		if !strings.Contains(*resp.ContentDocument, want) {
			t.Errorf("content_document = %q, want %q", *resp.ContentDocument, want)
		}
	}
	for _, unsafe := range []string{"alert(\"must not survive\")", "javascript:"} {
		if strings.Contains(resp.Content, unsafe) || strings.Contains(*resp.ContentDocument, unsafe) {
			t.Fatalf("saved content = %#v, must not contain %q", resp, unsafe)
		}
	}
}

func TestContentSavePromotesMultimodalTextWithoutRefetching(t *testing.T) {
	t.Parallel()
	captured := "captured text plus user context"
	link := contentDoneLink()
	link.SourceKind = "multimodal"
	link.InputText = &captured
	store := &fakeContentStore{link: link}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("fetcher must not run for an ingest source with saved text")
	}}, nil)

	resp, err := svc.Save(context.Background(), link.ID.String())
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if resp.Content != captured || store.saved != captured {
		t.Fatalf("response/stored content = %q/%q, want %q", resp.Content, store.saved, captured)
	}
	if resp.FetcherType != "multimodal" {
		t.Fatalf("fetcher_type = %q, want multimodal", resp.FetcherType)
	}
}

func TestContentSaveDoesNotRefetchIngestWithoutText(t *testing.T) {
	t.Parallel()
	link := contentDoneLink()
	link.SourceKind = "image"
	store := &fakeContentStore{link: link}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("fetcher must not run for an ingest source without text")
	}}, nil)

	_, err := svc.Save(context.Background(), link.ID.String())
	assertHTTPCode(t, err, httperr.CodeLinkContentUnavailable)
	if store.saved != "" {
		t.Fatalf("UpdateContent() stored %q, want no write", store.saved)
	}
}

func TestContentSaveStoresFetchedBody(t *testing.T) {
	t.Parallel()
	store := &fakeContentStore{link: contentDoneLink()}
	svc := NewContentService(store, fakeContentFetcher{content: fetcher.Content{Body: "  正文原文  ", FetcherType: "jina"}}, nil)

	resp, err := svc.Save(context.Background(), store.link.ID.String())
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if resp.Content != "正文原文" {
		t.Errorf("content = %q, want trimmed 正文原文", resp.Content)
	}
	if resp.FetcherType != "jina" {
		t.Errorf("fetcher_type = %q, want jina", resp.FetcherType)
	}
	if store.saved != "正文原文" {
		t.Errorf("stored body = %q, want 正文原文", store.saved)
	}
}

func TestContentSaveDoesNotWriteAcrossCaptureRevision(t *testing.T) {
	t.Parallel()

	link := contentDoneLink()
	link.UpdatedAt = time.Now().UTC().Add(-time.Minute)
	store := &fakeContentStore{link: link}
	svc := NewContentService(store, fakeContentFetcher{
		content: fetcher.Content{Body: "old revision body", FetcherType: "jina"},
		onFetch: func() {
			store.link.Status = model.LinkStatusPending
			store.link.UpdatedAt = time.Now().UTC()
		},
	}, nil)

	_, err := svc.Save(context.Background(), link.ID.String())
	assertHTTPCode(t, err, httperr.CodeLinkNotReady)
	if store.saved != "" {
		t.Fatalf("stored body = %q, want no stale write", store.saved)
	}
}

func TestContentSaveReturnsNotFoundWhenLinkIsDeletedDuringFetch(t *testing.T) {
	t.Parallel()

	link := contentDoneLink()
	link.UpdatedAt = time.Now().UTC().Add(-time.Minute)
	store := &fakeContentStore{link: link}
	svc := NewContentService(store, fakeContentFetcher{
		content: fetcher.Content{Body: "orphaned body", FetcherType: "jina"},
		onFetch: func() {
			store.link = nil
		},
	}, nil)

	_, err := svc.Save(context.Background(), link.ID.String())
	assertHTTPCode(t, err, httperr.CodeLinkNotFound)
	if store.saved != "" {
		t.Fatalf("stored body = %q, want no write for a deleted link", store.saved)
	}
}

func TestContentSaveReusesConcurrentWinner(t *testing.T) {
	t.Parallel()

	link := contentDoneLink()
	link.UpdatedAt = time.Now().UTC()
	winner := "content saved by the concurrent request"
	winnerContent := contentdoc.Plain(winner)
	store := &fakeContentStore{link: link}
	svc := NewContentService(store, fakeContentFetcher{
		content: fetcher.Content{Body: "duplicate fetched body", FetcherType: "jina"},
		onFetch: func() {
			store.content = &winnerContent
		},
	}, nil)

	response, err := svc.Save(context.Background(), link.ID.String())
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if response.Content != winner || response.FetcherType != "stored" {
		t.Fatalf("Save() = %#v, want concurrent stored winner", response)
	}
	if store.saved != "" {
		t.Fatalf("stored body = %q, want CAS loser to skip its fetched body", store.saved)
	}
}

func TestContentReplaceRefetchesAndOverwritesExistingContent(t *testing.T) {
	t.Parallel()
	existing := contentdoc.Plain("old flattened body")
	store := &fakeContentStore{link: contentDoneLink(), content: &existing}
	svc := NewContentService(store, fakeContentFetcher{content: fetcher.Content{
		Body:        "Heading Fresh body",
		HTML:        `<article><h1>Heading</h1><p>Fresh body</p></article>`,
		FetcherType: "basic",
	}}, nil)

	resp, err := svc.Replace(context.Background(), store.link.ID.String())
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if resp.Content == "old flattened body" || !strings.Contains(resp.Content, "Heading\n\nFresh body") {
		t.Fatalf("Replace() content = %q, want freshly fetched structured projection", resp.Content)
	}
	if resp.ContentFormat != "markdown" || resp.ContentDocument == nil {
		t.Fatalf("Replace() = %#v, want markdown document", resp)
	}
}

// 写路径必须把自增后的 content_revision 交回给客户端。少了它，Reader 要等
// useLinks 那 30 秒的静默刷新才知道正文换了代次，而这段窗口里用户在新正文上
// 建的划线带着旧代次，刷新后被 annotations.ts 判定失配、连同 localStorage 一起
// 抹掉——用户看到的是「刚划的线自己没了」。
func TestContentWritesReturnPostWriteRevision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		call func(*ContentService, string) (dto.LinkContentResponse, error)
		want int64
	}{
		{"save", func(s *ContentService, id string) (dto.LinkContentResponse, error) {
			return s.Save(context.Background(), id)
		}, 1},
		{"replace", func(s *ContentService, id string) (dto.LinkContentResponse, error) {
			return s.Replace(context.Background(), id)
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeContentStore{link: contentDoneLink()}
			svc := NewContentService(store, fakeContentFetcher{content: fetcher.Content{Body: "fresh body", FetcherType: "basic"}}, nil)

			resp, err := tc.call(svc, store.link.ID.String())
			if err != nil {
				t.Fatalf("%s() error = %v", tc.name, err)
			}
			if resp.ContentRevision != tc.want {
				t.Fatalf("%s() content_revision = %d, want %d — 客户端拿不到新代次就会丢划线", tc.name, resp.ContentRevision, tc.want)
			}
		})
	}
}

// 幂等分支（已保存过，直接返回旧正文）同样要带代次：它返回的是库里那份正文，
// 报 0 会让客户端以为自己缓存的是第 0 代。
func TestContentSaveReturnsStoredRevisionOnIdempotentHit(t *testing.T) {
	t.Parallel()
	existing := contentdoc.Plain("already saved")
	existing.Revision = 7
	store := &fakeContentStore{link: contentDoneLink(), content: &existing}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("已保存原文时不该抓取")
	}}, nil)

	resp, err := svc.Save(context.Background(), store.link.ID.String())
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if resp.ContentRevision != 7 {
		t.Fatalf("Save() content_revision = %d, want 7", resp.ContentRevision)
	}
}

func TestContentSaveRejectsNotDone(t *testing.T) {
	t.Parallel()
	link := contentDoneLink()
	link.Status = model.LinkStatusPending
	store := &fakeContentStore{link: link}
	svc := NewContentService(store, fakeContentFetcher{content: fetcher.Content{Body: "x"}}, nil)

	_, err := svc.Save(context.Background(), link.ID.String())
	assertHTTPCode(t, err, httperr.CodeLinkNotReady)
	if store.saved != "" {
		t.Error("must not store content for non-done link")
	}
}

func TestContentSaveNotFound(t *testing.T) {
	t.Parallel()
	svc := NewContentService(&fakeContentStore{link: nil}, fakeContentFetcher{}, nil)
	_, err := svc.Save(context.Background(), uuid.New().String())
	assertHTTPCode(t, err, httperr.CodeLinkNotFound)
}

func TestContentSaveFetchErrorIsBadGateway(t *testing.T) {
	t.Parallel()
	store := &fakeContentStore{link: contentDoneLink()}
	svc := NewContentService(store, fakeContentFetcher{err: errors.New("blocked")}, nil)
	_, err := svc.Save(context.Background(), store.link.ID.String())
	var he *httperr.Error
	if !errors.As(err, &he) || he.HTTPStatus() != 502 {
		t.Fatalf("err = %v, want 502", err)
	}
	if store.saved != "" {
		t.Error("must not store on fetch error")
	}
}

func TestContentSaveWithoutConfiguredFetcherIsServiceUnavailable(t *testing.T) {
	t.Parallel()
	store := &fakeContentStore{link: contentDoneLink()}
	svc := NewContentService(store, nil, nil)

	_, err := svc.Save(context.Background(), store.link.ID.String())
	var httpErr *httperr.Error
	if !errors.As(err, &httpErr) || httpErr.HTTPStatus() != 503 {
		t.Fatalf("err = %v, want 503", err)
	}
	if store.saved != "" {
		t.Error("must not store content without a configured fetcher")
	}
}

func TestContentSaveRejectsSearchSummaryAsOriginalContent(t *testing.T) {
	t.Parallel()
	store := &fakeContentStore{link: contentDoneLink()}
	svc := NewContentService(store, fakeContentFetcher{
		content: fetcher.Content{Body: "search result excerpt", FetcherType: "basic+search"},
	}, nil)

	_, err := svc.Save(context.Background(), store.link.ID.String())
	var httpErr *httperr.Error
	if !errors.As(err, &httpErr) || httpErr.HTTPStatus() != 502 {
		t.Fatalf("err = %v, want 502", err)
	}
	if store.saved != "" {
		t.Error("must not persist a search summary as canonical content")
	}
}

func TestContentSaveEmptyBodyIsBadGateway(t *testing.T) {
	t.Parallel()
	store := &fakeContentStore{link: contentDoneLink()}
	svc := NewContentService(store, fakeContentFetcher{content: fetcher.Content{Body: "   "}}, nil)
	_, err := svc.Save(context.Background(), store.link.ID.String())
	var he *httperr.Error
	if !errors.As(err, &he) || he.HTTPStatus() != 502 {
		t.Fatalf("err = %v, want 502 on empty body", err)
	}
}

func TestContentEditPlainUsesRevisionCASAndDoesNotFetch(t *testing.T) {
	t.Parallel()
	link := contentDoneLink()
	link.ContentRevision = 7
	existing := contentdoc.Plain("old plain body")
	existing.Revision = 7
	existing.Source = model.ContentSourceFetched
	store := &fakeContentStore{link: link, content: &existing}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("Edit must never fetch the remote page")
	}}, nil)

	resp, err := svc.Edit(context.Background(), link.ID.String(), dto.ContentEditRequest{
		Content:                 "  new plain body  ",
		ExpectedContentRevision: 7,
	})
	if err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if resp.Content != "new plain body" || resp.ContentFormat != "plain" || resp.ContentDocument != nil {
		t.Fatalf("Edit() response = %#v, want canonical plain content", resp)
	}
	if resp.ContentSource != string(model.ContentSourceUser) {
		t.Fatalf("content_source = %q, want user", resp.ContentSource)
	}
	if resp.ContentRevision != 8 || store.savedContent.Revision != 8 {
		t.Fatalf("revision response/store = %d/%d, want 8/8", resp.ContentRevision, store.savedContent.Revision)
	}
	if store.savedContent.Source != model.ContentSourceUser || store.editCalls != 1 {
		t.Fatalf("stored content/calls = %#v/%d, want user source and one CAS write", store.savedContent, store.editCalls)
	}
}

func TestContentEditMarkdownSanitizesAndResolvesRelativeLinks(t *testing.T) {
	t.Parallel()
	link := contentDoneLink()
	link.URL = "https://example.com/articles/start"
	existing := model.SavedContent{
		Text:     "old markdown",
		Document: stringPointerForContentTest("# Old\n\nold markdown"),
		Format:   model.ContentFormatMarkdown,
		Source:   model.ContentSourceFetched,
		Revision: 4,
	}
	link.ContentRevision = 4
	store := &fakeContentStore{link: link, content: &existing}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("Markdown Edit must not fetch")
	}}, nil)

	resp, err := svc.Edit(context.Background(), link.ID.String(), dto.ContentEditRequest{
		Content:                 "# New\n\n[Docs](../docs)\n\n[Unsafe](javascript:alert(1))\n\n<script>alert(2)</script>\n\n<iframe>frame</iframe>\n\n<form>form</form>",
		ExpectedContentRevision: 4,
	})
	if err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if resp.ContentFormat != "markdown" || resp.ContentDocument == nil {
		t.Fatalf("response format/document = %q/%#v, want markdown document", resp.ContentFormat, resp.ContentDocument)
	}
	for _, want := range []string{"# New", "[Docs](https://example.com/docs)"} {
		if !strings.Contains(*resp.ContentDocument, want) {
			t.Errorf("content_document = %q, want %q", *resp.ContentDocument, want)
		}
	}
	for _, unsafe := range []string{"javascript:", "alert(1)", "alert(2)", "iframe", "form"} {
		if strings.Contains(resp.Content, unsafe) || strings.Contains(*resp.ContentDocument, unsafe) {
			t.Errorf("normalized edit contains unsafe %q: %#v", unsafe, resp)
		}
	}
	if resp.ContentSource != string(model.ContentSourceUser) || resp.ContentRevision != 5 {
		t.Fatalf("source/revision = %q/%d, want user/5", resp.ContentSource, resp.ContentRevision)
	}
}

func TestContentEditRejectsWhitespaceAndSanitizedEmpty(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		format  model.ContentFormat
		doc     *string
		request string
	}{
		{name: "plain whitespace", format: model.ContentFormatPlain, request: " \n\t "},
		{name: "markdown sanitized empty", format: model.ContentFormatMarkdown, doc: stringPointerForContentTest("# Existing"), request: "<script>alert(1)</script>\n<iframe>blocked</iframe>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			link := contentDoneLink()
			link.ContentRevision = 3
			existing := model.SavedContent{Text: "existing", Document: tc.doc, Format: tc.format, Source: model.ContentSourceFetched, Revision: 3}
			store := &fakeContentStore{link: link, content: &existing}
			svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
				t.Fatal("Edit must not fetch while rejecting empty content")
			}}, nil)

			_, err := svc.Edit(context.Background(), link.ID.String(), dto.ContentEditRequest{
				Content:                 tc.request,
				ExpectedContentRevision: 3,
			})
			assertHTTPCode(t, err, httperr.CodeContentEmpty)
			if store.editCalls != 0 || store.saved != "" {
				t.Fatalf("empty edit wrote/called CAS = %q/%d, want no write", store.saved, store.editCalls)
			}
		})
	}
}

func TestContentEditEnforcesDecodedUTF8ByteLimit(t *testing.T) {
	t.Parallel()
	newStore := func() *fakeContentStore {
		link := contentDoneLink()
		link.ContentRevision = 1
		existing := contentdoc.Plain("existing")
		existing.Revision = 1
		return &fakeContentStore{link: link, content: &existing}
	}

	store := newStore()
	svc := NewContentService(store, nil, nil)
	resp, err := svc.Edit(context.Background(), store.link.ID.String(), dto.ContentEditRequest{
		Content:                 strings.Repeat("a", 2<<20),
		ExpectedContentRevision: 1,
	})
	if err != nil {
		t.Fatalf("2 MiB Edit() error = %v", err)
	}
	if resp.ContentRevision != 2 || len([]byte(resp.Content)) != 2<<20 {
		t.Fatalf("2 MiB response revision/bytes = %d/%d, want 2/%d", resp.ContentRevision, len([]byte(resp.Content)), 2<<20)
	}

	store = newStore()
	_, err = svc.Edit(context.Background(), store.link.ID.String(), dto.ContentEditRequest{
		Content:                 strings.Repeat("a", (2<<20)+1),
		ExpectedContentRevision: 1,
	})
	assertHTTPCode(t, err, httperr.CodeContentTooLarge)
	if store.editCalls != 0 {
		t.Fatalf("oversized edit invoked CAS %d times, want 0", store.editCalls)
	}
}

func TestContentEditRejectsInvalidOrUnavailableTargets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		link *model.Link
		body *model.SavedContent
		id   string
		want string
	}{
		{name: "invalid uuid", link: contentDoneLink(), id: "bad-id", want: httperr.CodeInvalidLinkID},
		{name: "missing link", id: uuid.NewString(), want: httperr.CodeLinkNotFound},
		{name: "no saved content", link: contentDoneLink(), id: "link", want: httperr.CodeLinkNotFound},
		{name: "site", link: contentDoneLink(), id: "link", want: httperr.CodeSiteOriginalContentForbidden},
		{name: "not done", link: contentDoneLink(), id: "link", want: httperr.CodeLinkNotReady},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			link := tc.link
			if link == nil && tc.name == "missing link" {
				link = nil
			} else if link == nil {
				link = contentDoneLink()
			}
			if tc.name == "site" {
				kind := model.LibraryKindSite
				link.LibraryKind = &kind
			}
			if tc.name == "not done" {
				link.Status = model.LinkStatusPending
			}
			if tc.id == "link" {
				tc.id = link.ID.String()
			}
			store := &fakeContentStore{link: link, content: tc.body}
			svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
				t.Fatal("Edit target validation must not fetch")
			}}, nil)
			_, err := svc.Edit(context.Background(), tc.id, dto.ContentEditRequest{
				Content:                 "new body",
				ExpectedContentRevision: 1,
			})
			assertHTTPCode(t, err, tc.want)
		})
	}
}

func TestContentEditRejectsStaleRevisionAndClassifiesCASMiss(t *testing.T) {
	t.Parallel()
	newStore := func() *fakeContentStore {
		link := contentDoneLink()
		link.ContentRevision = 7
		existing := contentdoc.Plain("existing")
		existing.Revision = 7
		return &fakeContentStore{link: link, content: &existing}
	}

	store := newStore()
	svc := NewContentService(store, nil, nil)
	_, err := svc.Edit(context.Background(), store.link.ID.String(), dto.ContentEditRequest{
		Content:                 "new",
		ExpectedContentRevision: 6,
	})
	assertHTTPCode(t, err, httperr.CodeContentRevisionConflict)
	if store.editCalls != 0 {
		t.Fatalf("stale preflight invoked CAS %d times, want 0", store.editCalls)
	}

	store = newStore()
	store.editMiss = true
	svc = NewContentService(store, nil, nil)
	_, err = svc.Edit(context.Background(), store.link.ID.String(), dto.ContentEditRequest{
		Content:                 "new",
		ExpectedContentRevision: 7,
	})
	assertHTTPCode(t, err, httperr.CodeLinkNotReady)
	if store.editCalls != 1 || store.saved != "" {
		t.Fatalf("CAS miss calls/write = %d/%q, want one call and no write", store.editCalls, store.saved)
	}
}

func TestContentEditNormalizesLegacyHTMLBeforeEditing(t *testing.T) {
	t.Parallel()
	link := contentDoneLink()
	link.ContentRevision = 9
	existing := model.SavedContent{
		Text:     "Legacy body",
		Document: stringPointerForContentTest(`<article><h1>Legacy</h1><p>Body</p><script>bad()</script></article>`),
		Format:   model.ContentFormatHTML,
		Source:   model.ContentSourceFetched,
		Revision: 9,
	}
	store := &fakeContentStore{link: link, content: &existing}
	svc := NewContentService(store, fakeContentFetcher{onFetch: func() {
		t.Fatal("legacy HTML normalization must not fetch")
	}}, nil)

	resp, err := svc.Edit(context.Background(), link.ID.String(), dto.ContentEditRequest{
		Content:                 "# Fixed\n\nSafe body",
		ExpectedContentRevision: 9,
	})
	if err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	if resp.ContentFormat != "markdown" || resp.ContentDocument == nil || strings.Contains(*resp.ContentDocument, "bad()") {
		t.Fatalf("legacy HTML edit response = %#v, want sanitized Markdown", resp)
	}
}

func assertHTTPCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	var he *httperr.Error
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *httperr.Error", err)
	}
	if he.HTTPErrorCode() != wantCode {
		t.Fatalf("error code = %q, want %q", he.HTTPErrorCode(), wantCode)
	}
}
