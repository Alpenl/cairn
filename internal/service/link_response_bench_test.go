package service

// Benchmark 覆盖 linkToResponse——读路径 (/api/links, /api/links/:id) 上每条记录
// 都要走一次的纯 DTO 映射。即使单次只 100ns 级，列表页一次返回 50 行就放大 50 倍，
// 是值得盯防的微优化点。
//
// 这里只测纯 Go 内存映射；真实 PG 行扫描和 JSONB 反序列化在 link_scan 路径，
// 那一块需要真实数据库，所以放到 dbintegration 那边而不是这个 benchmark 里。

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/model"
)

// makeBenchLink 造一条字段填得"刚好"够代表生产数据的 link：URL、标题、摘要、
// 5 个 tag、错误诊断字段全部 non-nil。这是 LinkResponse 最常见的形态——已解析
// 完成、有完整 metadata。
func makeBenchLink() model.Link {
	title := "Project announcement: open-sourcing FrameworkX"
	summary := "这篇文章讨论了检索增强生成（RAG）在企业知识库中的落地经验。"
	description := "用户备注：值得长期关注的项目"
	fetcherType := "basic"
	domain := "example.com"
	contentType := "article"
	depth := 2
	parentPath := "/foo/bar"
	parentID := uuid.New()
	errMsg := "thin_content"
	lowConfReason := "thin_content"

	return model.Link{
		ID:                  uuid.New(),
		URL:                 "https://example.com/foo/bar/announcement",
		SourceKind:          "url",
		SourceKey:           "https://example.com/foo/bar/announcement",
		Title:               &title,
		Summary:             &summary,
		Description:         &description,
		Tags:                []string{"RAG", "LLM", "知识库", "Embedding", "架构"},
		FetcherType:         &fetcherType,
		IsLowConfidence:     false,
		LowConfidenceReason: &lowConfReason,
		Status:              model.LinkStatusDone,
		ErrorMsg:            &errMsg,
		Domain:              &domain,
		ContentType:         &contentType,
		PathDepth:           &depth,
		ParentPath:          &parentPath,
		ParentID:            &parentID,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
}

// BenchmarkLinkResponseMapping 衡量"一条 link → 一个 LinkResponse"的微开销。
// 在 list 接口里这个函数会按页大小（默认 50）被调用一次循环，单次 ns/op 直接
// 影响 GET /api/links 的总响应时间。
func BenchmarkLinkResponseMapping(b *testing.B) {
	link := makeBenchLink()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := linkToResponse(link)
		// 防止编译器把整个 linkToResponse 视为 dead code 优化掉——读一下结果。
		if resp.ID == "" {
			b.Fatal("empty ID")
		}
	}
}
