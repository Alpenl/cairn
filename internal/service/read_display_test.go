package service

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
)

type stubContentReader struct {
	content *model.SavedContent
}

func (s stubContentReader) GetContent(context.Context, uuid.UUID) (*model.SavedContent, error) {
	return s.content, nil
}

func TestGetIncludesStructuredSavedContentOnlyOnDetail(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	// PF6 起 has_content 与两项计数是 links 上的列（has_content 是生成列，
	// 由 content IS NULL 唯一决定），不再由详情端读一遍正文推导出来。
	// fake 必须照这个来，否则测试会在一个现实中不存在的行状态上通过。
	link := model.Link{
		ID: id, URL: "https://example.com/article", Status: model.LinkStatusDone,
		HasContent: true, ContentCJKChars: 0, ContentWords: 2,
	}
	store := &readFakeLinkStore{byID: map[uuid.UUID]*model.Link{id: &link}}
	document := "# Heading\n\nBody"
	content := &model.SavedContent{
		Text:     "Heading\n\nBody",
		Document: &document,
		Format:   model.ContentFormatMarkdown,
	}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store, ContentReader: stubContentReader{content: content}})

	resp, err := svc.Get(context.Background(), id.String())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if resp.Content == nil || *resp.Content != content.Text || resp.ContentDocument == nil || *resp.ContentDocument != document {
		t.Fatalf("Get() content fields = %#v", resp)
	}
	if resp.ContentFormat == nil || *resp.ContentFormat != "markdown" {
		t.Fatalf("Get() content_format = %#v, want markdown", resp.ContentFormat)
	}
	if !resp.HasContent {
		t.Fatal("Get() has_content = false，已保存原文必须报 true")
	}
}

// TestGetWithoutContentReportsHasContentButOmitsBody 钉住「原文按需取」这条契约的
// 读半边：include_content=false 时详情只说明「有原文」，不把整篇正文塞进响应——
// Reader 的原文是折叠的，展开时才走 GET /api/links/{id}/content。
func TestGetWithoutContentReportsHasContentButOmitsBody(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	// PF6 起 has_content 与两项计数是 links 上的列（has_content 是生成列，
	// 由 content IS NULL 唯一决定），不再由详情端读一遍正文推导出来。
	// fake 必须照这个来，否则测试会在一个现实中不存在的行状态上通过。
	link := model.Link{
		ID: id, URL: "https://example.com/article", Status: model.LinkStatusDone,
		HasContent: true, ContentCJKChars: 0, ContentWords: 2,
	}
	store := &readFakeLinkStore{byID: map[uuid.UUID]*model.Link{id: &link}}
	document := "# Heading\n\nBody"
	content := &model.SavedContent{Text: "Heading\n\nBody", Document: &document, Format: model.ContentFormatMarkdown}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store, ContentReader: stubContentReader{content: content}})

	resp, err := svc.GetWithContent(context.Background(), id.String(), false)
	if err != nil {
		t.Fatalf("GetWithContent() error = %v", err)
	}
	if !resp.HasContent {
		t.Fatal("has_content = false，即便不带正文也必须告诉客户端「有原文」")
	}
	if resp.Content != nil || resp.ContentDocument != nil || resp.ContentFormat != nil {
		t.Fatalf("include_content=false 仍带回了正文：%#v", resp)
	}
}

// TestGetWithoutSavedContentReportsHasContentFalse 补另一半：没有已保存原文时
// has_content 必须是 false，否则 Reader 会显示一个点开来是空的折叠区。
func TestGetWithoutSavedContentReportsHasContentFalse(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	// PF6 起 has_content 与两项计数是 links 上的列（has_content 是生成列，
	// 由 content IS NULL 唯一决定），不再由详情端读一遍正文推导出来。
	// fake 必须照这个来，否则测试会在一个现实中不存在的行状态上通过。
	// 这条链接没有已保存原文：生成列 has_content 因此为 false。
	link := model.Link{ID: id, URL: "https://example.com/article", Status: model.LinkStatusDone}
	store := &readFakeLinkStore{byID: map[uuid.UUID]*model.Link{id: &link}}
	svc := NewLinkReadService(LinkReadServiceOptions{Links: store, ContentReader: stubContentReader{content: nil}})

	resp, err := svc.GetWithContent(context.Background(), id.String(), false)
	if err != nil {
		t.Fatalf("GetWithContent() error = %v", err)
	}
	if resp.HasContent {
		t.Fatal("has_content = true，但这条链接根本没有已保存原文")
	}
}

// TestCountReadingUnitsMatchesReaderTokenization 钉住跨语言同源：这段样本与
// reader/src/components/DetailPane.test.tsx 里那条「阅读时长展开前后一致」的
// 用例用的是同一套口径——CJK 逐字、西文按词（撇号 / 连字符不断词）。任何一边
// 改了分词规则而另一边没跟上，同一篇文章在折叠与展开状态下就会显示两个数字。
func TestCountReadingUnitsMatchesReaderTokenization(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		text     string
		wantCJK  int
		wantWord int
	}{
		{"纯中文", strings.Repeat("正", 800), 800, 0},
		{"纯英文", strings.Repeat("word ", 500), 0, 500},
		{"中英混排", strings.Repeat("word ", 500) + strings.Repeat("中文", 100), 200, 500},
		{"撇号与连字符不断词", "it's state-of-the-art", 0, 2},
		{"标点与空白不计数", "…… ，。！\n\t", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cjk, words := countReadingUnits(tt.text)
			if cjk != tt.wantCJK || words != tt.wantWord {
				t.Fatalf("countReadingUnits() = (%d, %d), want (%d, %d)", cjk, words, tt.wantCJK, tt.wantWord)
			}
		})
	}
}
